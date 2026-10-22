import copy
import contextlib
import fcntl
import io
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import campaign as c


def node(uid="node"):
    return dict(metadata=dict(name=uid, uid=uid), spec={}, status=dict(conditions=[dict(type="Ready", status="True")]))


def pod(uid="pod", restarts=0, oom=False):
    return dict(metadata=dict(name=uid, uid=uid, labels=dict(app="etcd")), status=dict(
        phase="Running", conditions=[dict(type="Ready", status="True")], containerStatuses=[dict(
            name="main", restartCount=restarts, lastState=dict(terminated=dict(reason="OOMKilled", finishedAt=uid)) if oom else {})]))


class CampaignTests(unittest.TestCase):
    def path_config(self):
        return dict(guestKubeconfig="private/guest", managementKubeconfig="private/management", lockFile="state/workload.lock",
                    seed=dict(binary=dict(path="bin/burner")), record="inputs/record.json", retention="inputs/retention.json", adversaries={},
                    capture=dict(endpoint="http://127.0.0.1:19107", readyFile="observer/ready.json", streamManifests=["observer/stream/manifest.json"],
                                 guestStats=[dict(output="observer/guest")], expectedTargets="inputs/targets.json", finalQueries="inputs/queries.json"))

    def test_paths_resolve_against_config_parent_not_caller(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            (base / "config").mkdir()
            config = self.path_config()
            c.save(base / "config/inputs/record.json", dict(id="record"))
            query = base / "config/pressure.promql"
            query.write_text('sum(rate(cpu[1m]))')
            config["pressureCPUQueryFile"] = "pressure.promql"
            config["requireLimits"] = dict(inventory="inputs/limits.json")
            config["cleanupPrevious"] = dict(run="old", inventory="old/owned.json")
            result = c.resolve_config_paths(config, base / "config/campaign.json", base / "run")
            self.assertEqual(result["guestKubeconfig"], str(base / "config/private/guest"))
            self.assertEqual(result["record"], str(base / "config/inputs/record.json"))
            self.assertEqual(result["capture"]["guestStats"][0]["output"], str(base / "config/observer/guest"))
            self.assertEqual(result["cleanupPrevious"]["inventory"], str(base / "config/old/owned.json"))
            self.assertEqual(result["pressureCPUQuery"], 'sum(rate(cpu[1m]))')
            self.assertEqual(result["capture"]["endpoint"], config["capture"]["endpoint"])
            self.assertEqual(config["guestKubeconfig"], "private/guest", "must not mutate caller input")
            for invalid in (None, 42, "", "https://example/config", "bad\npath"):
                with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                    c.resolve_config_paths(dict(config, guestKubeconfig=invalid), base / "config/campaign.json", base / "run")

    def test_durable_policy_rejects_volatile_and_symlink_dependencies(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            c.resolve_config_paths(self.path_config(), root / "config.json", root / "run")
            config = dict(self.path_config(), durableStateRoot="/home/durable-state", guestKubeconfig="/tmp/guest")
            with self.assertRaisesRegex(RuntimeError, "volatile"):
                c.resolve_config_paths(config, Path("/home/config.json"), Path("/home/durable-state/run"))
            link = root / "link"
            link.symlink_to("/tmp")
            config["guestKubeconfig"] = str(link / "guest")
            with self.assertRaisesRegex(RuntimeError, "volatile"):
                c.resolve_config_paths(config, Path("/home/config.json"), Path("/home/durable-state/run"))
            with self.assertRaisesRegex(RuntimeError, "outside"):
                c.resolve_config_paths(config, Path("/home/config.json"), Path("/home/other/run"))

    def test_saved_unit_quotes_and_never_enables_restart(self):
        text = c.unit_definition(dict(unit="hcp-perf-safe", runtimeSeconds=7200), Path('/home/state/a % $ "quote"'))
        self.assertIn('WorkingDirectory="/home/state/a %% $ \\"quote\\""', text)
        self.assertIn('a %% $$ \\"quote\\"/bundle/campaign.py', text)
        self.assertIn("Restart=no", text)
        self.assertNotIn("[Install]", text)
        self.assertNotIn("WantedBy", text)
        with self.assertRaises(RuntimeError):
            c.unit_definition(dict(unit="evil\nunit", runtimeSeconds=1), Path("/home/run"))

    def test_offline_inspection_missing_outcome_never_trusts_pid_or_writes(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            config = c.resolve_config_paths(self.path_config(), root / "config.json", root)
            c.save(root / "bundle/config.json", config)
            c.save(root / "bundle/sha256.json", {"gone": "hash", "config.json": "wrong"})
            c.save(root / "started.json", dict(pid=os.getpid(), at=c.stamp()))
            before = sorted(str(p) for p in root.rglob("*"))
            result = c.recover_inspect(root)
            self.assertEqual(result["state"], "started-without-outcome-interrupted-or-unknown")
            self.assertFalse(result["automaticResumeAllowed"])
            self.assertEqual(result["liveState"], "unknown-not-queried")
            self.assertEqual(result["bundleIntegrity"], dict(missing=["gone"], changed=["config.json"]))
            with contextlib.redirect_stdout(io.StringIO()) as output:
                c.review(root)
            self.assertIn("interrupted-or-unknown", output.getvalue())
            self.assertEqual(sorted(str(p) for p in root.rglob("*")), before)
            c.save(root / "outcome.json", dict(execution="aborted"))
            self.assertEqual(c.recover_inspect(root)["state"], "terminal-recorded")

    def test_launch_working_directory_and_started_guard_without_systemd(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            c.save(root / "bundle/config.json", dict(unit="test-unit", runtimeSeconds=60))
            with patch.object(sys, "argv", ["campaign.py", "launch", "--output", str(root)]), patch.object(c.subprocess, "run") as run:
                self.assertEqual(c.main(), 0)
                command = run.call_args.args[0]
                self.assertIn("--working-directory=" + str(root), command)
                self.assertIn("--property=Restart=no", command)
                c.save(root / "started.json", dict(pid=123))
                run.reset_mock()
                with self.assertRaisesRegex(RuntimeError, "no automatic resume"):
                    c.main()
                run.assert_not_called()

    def test_freeze_embeds_inputs_and_binary_independent_of_source_files(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            config = self.path_config()
            config.update(self.hour_config())
            config["record"] = "inputs/record.json"
            for path, value in (("inputs/record.json", self.hour_config()["record"]),
                                ("inputs/retention.json", dict(version="v1", resourceGroups=["approved"])),
                                ("inputs/targets.json", dict(data=dict(activeTargets=[self.capture_target()]))),
                                ("inputs/queries.json", [dict(name="up", expr="up")]),
                                ("observer/ready.json", dict(captureReady=True))):
                c.save(root / path, value)
            for name in ("private/guest", "private/management", "bin/burner"):
                file = root / name
                file.parent.mkdir(exist_ok=True)
                file.write_text("private fixture")
            config["seed"]["binary"]["expectedSHA256"] = c.digest(root / "bin/burner")
            config["unit"] = "test-freeze"
            config["capture"].update(pollSeconds=5, lossSeconds=60, gateSeconds=120)
            c.save(root / "config.json", config)
            def build(args, **kwargs):
                Path(args[3]).write_text("built binary fixture")
            with patch.object(c.shutil, "copytree"), patch.object(c.subprocess, "run", side_effect=build):
                c.freeze(root / "config.json", root / "run")
            frozen = c.read(root / "run/bundle/config.json")
            self.assertEqual(frozen["record"]["id"], "record")
            self.assertEqual(frozen["capture"]["finalQueries"][0]["expr"], "up")
            self.assertEqual(frozen["seed"]["binary"]["path"], str(root / "run/bundle/kube-burner"))
            self.assertEqual(frozen["guestKubeconfig"], str(root / "private/guest"))
            for file in (root / "inputs").iterdir():
                file.unlink()
            (root / "bin/burner").unlink()
            self.assertEqual(c.recover_inspect(root / "run")["bundleIntegrity"], dict(missing=[], changed=[]))
            self.assertIn("campaign.service", c.read(root / "run/bundle/sha256.json"))

    def hour_config(self):
        return dict(version="v1", runtimeSeconds=7200, setupBudgetSeconds=1200, recoverySeconds=60, canaryCadenceSeconds=30,
                    record=dict(durationSeconds=7200, namespace="managed", id="record"),
                    adversaries=dict(durationSeconds=7200, namespace="managed", id="cpu"),
                    externalLifetimes=[dict(name=n, validUntil="2099-01-01T00:00:00Z") for n in ("guest-access", "management-access", "capture-forward")],
                    arms=[dict(name="hour", applications=120, durationSeconds=3600, offeredQPS=50,
                               contended=True, turnoverPeriodSeconds=3600, canaryCadenceSeconds=60)])

    def test_hour_config_limits_and_offer_budget(self):
        config = self.hour_config()
        for rate in (30, 50):
            config["arms"][0]["offeredQPS"] = rate
            c.validate_campaign(config)
            self.assertLessEqual(rate*3600, 1000000)
        for field, value in (("durationSeconds", 3601), ("durationSeconds", True), ("offeredQPS", 278), ("canaryCadenceSeconds", 0)):
            bad = copy.deepcopy(config)
            bad["arms"][0][field] = value
            with self.assertRaises(RuntimeError):
                c.validate_campaign(bad)
        for change in (dict(runtimeSeconds=4000), dict(setupBudgetSeconds=4000), dict(externalLifetimes=[]), dict(record=dict(durationSeconds=3600))):
            with self.assertRaises(RuntimeError):
                c.validate_campaign(dict(config, **change))
        short = dict(config, arms=[dict(config["arms"][0], durationSeconds=180)], externalLifetimes=[])
        short.pop("setupBudgetSeconds")
        c.validate_campaign(short)

    def test_required_limits_cover_all_containers_and_frozen_values(self):
        container = dict(name="main", image="pinned", resources=dict(requests=dict(cpu="100m", memory="100Mi"),
                         limits=dict(cpu="0.1", memory="110Mi")), env=[dict(name="GOMEMLIMIT", value="80MiB")])
        profile = dict(name="component", selector=dict(app="test"), containers=[container],
                       initContainers=[dict(container, name="init")])
        policy = dict(inventory=[profile], memoryLimitPercent=110)
        obj = dict(kind="Pod", metadata=dict(name="downstream", labels=dict(app="test")), spec=copy.deepcopy(profile))
        c.limits_inventory([obj], policy)
        for mutation in (lambda o: o["spec"]["containers"][0]["resources"]["limits"].pop("cpu"),
                         lambda o: o["spec"]["initContainers"][0]["resources"]["limits"].update(memory="111Mi"),
                         lambda o: o["metadata"].update(labels=dict(unmanaged="true")),
                         lambda o: o["spec"]["containers"][0]["env"][0].update(value="90MiB"),
                         lambda o: o["spec"].update(ephemeralContainers=[container])):
            bad = copy.deepcopy(obj)
            mutation(bad)
            with self.assertRaises(RuntimeError):
                c.limits_inventory([bad], policy)
        template = dict(kind="CronJob", metadata=dict(name="job"), spec=dict(jobTemplate=dict(spec=dict(template=dict(metadata=obj["metadata"], spec=obj["spec"])))))
        c.limits_inventory([template], policy)
        with self.assertRaises(RuntimeError):
            c.limits_inventory([obj], dict(policy, inventory=[profile, profile]))

    def test_downstream_exemption_requires_exact_owner_chain_and_profile(self):
        container = dict(name="downstream", image="pinned", resources=dict(requests=dict(cpu="10m", memory="64Mi")),
                         env=[dict(name="GOMEMLIMIT", value="unchanged")])
        owner = dict(kind="Deployment", metadata=dict(name="downstream", namespace="hcp", uid="owner"),
                     spec=dict(template=dict(metadata=dict(labels=dict(app="downstream")), spec=dict(containers=[container]))))
        replica = dict(kind="ReplicaSet", metadata=dict(name="replica", namespace="hcp", uid="replica", ownerReferences=[
            dict(kind="Deployment", name="downstream", uid="owner", controller=True)]), spec=copy.deepcopy(owner["spec"]))
        actual = dict(kind="Pod", metadata=dict(name="pod", namespace="hcp", uid="pod", labels=dict(app="downstream"), ownerReferences=[
            dict(kind="ReplicaSet", name="replica", uid="replica", controller=True)]), spec=dict(containers=[container]))
        policy = dict(memoryLimitPercent=110, inventory=[dict(name="controlled", selector=dict(app="controlled"), containers=[])],
                      ignoredOwners=[dict(owner=dict(kind="Deployment", name="downstream", namespace="hcp", uid="owner"),
                                          reason="fixed downstream charge, not projected", containers=[container])])
        rows = c.limits_inventory([owner, replica, actual], policy)
        self.assertTrue(all(r["scope"] == "explicit-owner-exemption" for r in rows))
        for mutate in (lambda objects: objects[0]["metadata"].update(uid="replacement"),
                       lambda objects: objects[2]["spec"]["containers"][0]["resources"]["requests"].update(cpu="20m"),
                       lambda objects: objects[2]["spec"].update(initContainers=[container]),
                       lambda objects: objects[2]["metadata"].update(ownerReferences=[])):
            objects = copy.deepcopy([owner, replica, actual])
            mutate(objects)
            with self.assertRaises(RuntimeError):
                c.limits_inventory(objects, policy)
        with self.assertRaisesRegex(RuntimeError, "overlap"):
            c.limits_inventory([owner, replica, actual], dict(policy, inventory=[dict(name="bad", selector=dict(app="downstream"))]))
        with self.assertRaises(RuntimeError):
            c.limits_inventory([actual], policy)

    def test_planning_inputs_refuse_validation_and_freeze(self):
        with self.assertRaisesRegex(RuntimeError, "planning-only"):
            c.validate_campaign(dict(planningOnly=True))
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            c.save(root / "plan.json", dict(planningOnly=True))
            with self.assertRaisesRegex(RuntimeError, "cannot freeze"):
                c.freeze(root / "plan.json", root / "attempt")
            self.assertFalse((root / "attempt/bundle/hcp-perf").exists())

    def test_progression_hold_records_decision_without_starting_work(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            runner = self.runner(root)
            self.assertFalse(runner.progression_hold(dict(name="historical")))
            self.assertFalse((root / "progression-hold.json").exists())
            with patch.object(runner, "start") as start:
                self.assertTrue(runner.progression_hold(dict(name="small-useful-point", holdAfterArm=True)))
                start.assert_not_called()
            self.assertEqual(c.read(root / "progression-hold.json")["completedArm"], "small-useful-point")

    def test_job_and_external_lifetime_bounds(self):
        job = dict(metadata=dict(name="recorder"), spec=dict(activeDeadlineSeconds=7200),
                   status=dict(startTime="1970-01-01T00:00:00Z"))
        c.job_lifetime_gate([job], 100, 4020)
        for jobs in ([], [dict(job, spec=dict(activeDeadlineSeconds=4000))],
                     [dict(job, spec=dict(activeDeadlineSeconds=7200, suspend=True))],
                     [dict(job, status=dict(startTime="1970-01-01T00:00:00Z", conditions=[dict(type="Failed", status="True")]))]):
            with self.assertRaises(RuntimeError):
                c.job_lifetime_gate(jobs, 100, 4020)
        with self.assertRaises(RuntimeError):
            c.lifetime_gate(dict(forward=4120), 100, 4020)
        c.lifetime_gate(dict(forward=4121), 100, 4020)

    def test_hour_measurement_all_apps_and_cadence_offline(self):
        for cadence in (30, 60):
            with self.subTest(cadence=cadence), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                runner = self.runner(root)
                runner.config.update(self.hour_config(), guestNodes=3, maxOutstanding=16)
                runner.deadline = 7300
                runner.session_expirations = dict(record=10000, adversaries=10000)
                runner.canary_namespace = "canary"
                arm = dict(runner.config["arms"][0], canaryCadenceSeconds=cadence)
                seed = dict(namespacePrefix="apps", applications=120)
                c.save(root / "credential-provenance.json", dict(benchmark=dict(files=[dict(validUntil="2099-01-01T00:00:00Z")]),
                                                               canary=dict(validUntil="2099-01-01T00:00:00Z")))
                clock, cycles, started = [100.0], [], []
                def start(args, label):
                    started.append((args, label))
                    return label
                def wait(child, bound):
                    if child == "turnover":
                        clock[0] = 3700
                        return dict(scheduled=120, completed=120, failed=0, backlog=0, outstanding=0, notStarted=0)
                    return {}
                job = dict(metadata=dict(name="job"), spec=dict(activeDeadlineSeconds=10000), status=dict(startTime="1970-01-01T00:00:00Z"))
                with patch.object(c.time, "monotonic", side_effect=lambda: clock[0]), patch.object(c.time, "time", side_effect=lambda: clock[0]), \
                     patch.object(runner, "start", side_effect=start), patch.object(runner, "wait", side_effect=wait), \
                     patch.object(runner, "get", return_value=dict(items=[job])), \
                     patch.object(runner, "pause", side_effect=lambda n: clock.__setitem__(0, clock[0]+n)), \
                     patch.object(runner, "canary", side_effect=lambda *args: cycles.append(args)), patch.object(c, "summarize_api"):
                    runner.measure(arm, root, root, seed, {})
                self.assertEqual(len(cycles), 3600//cadence)
                self.assertEqual(c.read(root / "api.json")["durationSeconds"], 3600)
                self.assertEqual(c.read(root / "turnover.json")["turnoverPeriodSeconds"], 3600)
                self.assertEqual(c.read(root / "window.json")["expectedRollouts"], 120)
                self.assertEqual(c.read(root / "outcome.json")["turnover"]["completed"], 120)
                self.assertEqual(clock[0], 3760, "hold recovery after full hour")
                for apps in (1, 7, 120, 240):
                    self.assertEqual(c.turnover_expected(dict(arm, applications=apps)), apps)
                    offsets = [i*3600*10**9//apps for i in range(apps)]
                    self.assertTrue(all(t < 3600*10**9 for t in offsets))

    def test_hour_admission_refuses_runtime_exhaustion_before_any_child(self):
        with tempfile.TemporaryDirectory() as temp:
            runner = self.runner(Path(temp))
            runner.config.update(self.hour_config())
            with patch.object(runner, "start") as start:
                with self.assertRaisesRegex(RuntimeError, "hard deadline"):
                    runner.measure(runner.config["arms"][0], Path(temp), Path(temp), {}, {})
                start.assert_not_called()

    def test_hour_signal_preserves_aborted_outcome_and_final_export(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            runner = self.runner(root)
            runner.config.update(self.hour_config())
            c.save(root / "bundle/sha256.json", {})
            c.save(root / "started.json", dict(at=c.stamp()))
            def stop_during_capture():
                runner.halt.set()
                runner.reason = "signal_SIGTERM"
                runner.check()
            calls = []
            with patch.object(runner, "observations_ready", side_effect=stop_during_capture), \
                 patch.object(runner, "get", return_value=dict(items=[])), \
                 patch.object(runner, "perf", side_effect=lambda *args: calls.append(args)):
                self.assertEqual(runner.execute(), 1)
            outcome = c.read(root / "outcome.json")
            self.assertEqual(outcome["stopReason"], "signal_SIGTERM")
            self.assertEqual(outcome["execution"], "aborted")
            self.assertEqual(outcome["activeLocalChildren"], [])
            self.assertEqual(calls[0][0][:2], ["metrics", "export"])

    def test_long_capture_lifetimes_fail_inside_outcome(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            runner = self.runner(root)
            runner.config.update(self.hour_config())
            runner.deadline = time.monotonic()+7200
            expires = c.datetime.fromtimestamp(time.time()+4000, c.timezone.utc).isoformat()
            c.save(root / "ready.json", dict(captureReady=True, endpoint="http://127.0.0.1:1", tokenExpiresAt="2099-01-01T00:00:00Z"))
            c.save(root / "stream.json", dict(status="running", completedChunks=1, updated=c.stamp(), end=expires))
            runner.config["capture"].update(readyFile=str(root / "ready.json"), streamManifests=[str(root / "stream.json")])
            with self.assertRaisesRegex(RuntimeError, "insufficient lifetime"):
                runner.observations_ready("hour")
            self.assertEqual(c.read(root / "capture-gates/hour/outcome.json")["status"], "blocked")

    def test_retained_adoption_only_adds_nodes(self):
        obj = dict(metadata=dict(uid="fixture"), spec=dict(template=dict(cpu="20m")), status=dict(conditions=[dict(type="Ready", status="True")]))
        old_node = node()
        old_node["status"]["allocatable"] = dict(cpu="8")
        new_node = copy.deepcopy(old_node)
        new_node["metadata"] = dict(name="new", uid="new")
        previous = dict(outcome=dict(execution="completed", activeLocalChildren=[], finalizationErrors=[]),
                        pods=[obj], deployments=[obj], apf=[obj], nodes=[old_node], resourceLock=[], managementNodeUIDs=["node"], cscSpec=dict(cpu=2))
        live = dict(pods=[obj], deployments=[obj], apf=[obj], nodes=[old_node, new_node], resourceLock=[], managementNodes=[old_node], cscSpec=dict(cpu=2))
        c.retained_gate(previous, live)
        for field in ("pods", "deployments", "apf"):
            altered = copy.deepcopy(live)
            altered[field][0]["spec"]["template"]["cpu"] = "40m"
            with self.assertRaisesRegex(RuntimeError, "UID/spec/template"):
                c.retained_gate(previous, altered)
            altered[field][0]["metadata"]["uid"] = "replacement"
            with self.assertRaises(RuntimeError):
                c.retained_gate(previous, altered)
        for field, value in (("nodes", [new_node]), ("managementNodes", [new_node]), ("cscSpec", dict(cpu=4))):
            with self.assertRaises(RuntimeError):
                c.retained_gate(previous, dict(live, **{field: value}))
        with self.assertRaisesRegex(RuntimeError, "predecessor"):
            c.retained_gate(dict(previous, outcome=dict(execution="aborted")), live)

    def test_fixed_api_identity_subset(self):
        seed = dict(namespacePrefix="population", applications=240)
        self.assertEqual(c.api_namespaces(seed, dict(apiIdentities=6)), ["population-"+str(i) for i in range(6)])
        self.assertEqual(len(c.api_namespaces(seed, dict(apiIdentities=12))), 12)
        with self.assertRaises(RuntimeError):
            c.api_namespaces(seed, dict(apiIdentities=13))

    def test_review_does_not_count_adopted_credential_receipt_as_canary(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            c.save(root / "bundle/config.json", dict(arms=[dict(name="retained")]))
            c.save(root / "bundle/sha256.json", {})
            c.save(root / "outcome.json", dict(execution="completed"))
            c.save(root / "arms/retained/canary-credentials.json", dict(validUntil="2099-01-01T00:00:00Z"))
            c.review(root)
            self.assertEqual(c.read(root / "review.json")["arms"][0]["canaries"]["completed"], 0)

    def test_exact_retention(self):
        spec = dict(resourceGroups=["/RG/A", "/RG/B"])
        result = dict(version="v1", status="verified", dryRun=False, failure=None, resourceGroups=[
            dict(id=x, status="verified", attempted=True, updated=True, verified=True, possiblyUpdated=False, failure=None)
            for x in ("/rg/a", "/rg/b")])
        c.retention_gate(spec, result)
        for mutation in (lambda v: v["resourceGroups"].pop(), lambda v: v["resourceGroups"].append(v["resourceGroups"][0]),
                         lambda v: v.update(dryRun=True), lambda v: v["resourceGroups"][0].update(possiblyUpdated=True)):
            value = copy.deepcopy(result)
            mutation(value)
            with self.assertRaises(RuntimeError):
                c.retention_gate(spec, value)

    def test_oom_and_replacement_restart_storm(self):
        safety = c.Safety([pod()], [node()], {})
        self.assertTrue(safety.observe([pod(oom=True)], [node()], 1)[1])
        safety = c.Safety([pod()], [node()], {})
        self.assertFalse(safety.observe([pod("new1", 2)], [node()], 1)[1])
        self.assertIn("restart storm in 120s", safety.observe([pod("new2", 1)], [node()], 2)[1])

    def test_quorum_grace_and_recovery(self):
        safety = c.Safety([pod()], [node()], {"etcd": dict(labels=dict(app="etcd"), minimumReady=1)})
        self.assertFalse(safety.observe([], [node()], 1)[1])
        self.assertTrue(safety.observe([], [node()], 46)[1])
        self.assertFalse(safety.observe([pod()], [node()], 47)[1])
        self.assertFalse(safety.observe([], [node()], 48)[1])
        self.assertTrue(safety.observe([pod()], [node("replacement")], 49)[1])

    def runner(self, root):
        c.save(root / "bundle/config.json", dict(runtimeSeconds=60, capture=dict(endpoint="http://127.0.0.1:1"),
                                                 managementKubeconfig="unused", guestKubeconfig="unused"))
        return c.Runner(root)

    def test_timeout_reaps_and_preserves_exit(self):
        with tempfile.TemporaryDirectory() as temp:
            runner = self.runner(Path(temp))
            child = runner.start([sys.executable, "-c", "import time; time.sleep(60)"], "sleep")
            with self.assertRaises(RuntimeError):
                runner.wait(child, .1)
            self.assertIsNotNone(child[0].poll())
            self.assertTrue(c.read(str(child[1]) + ".exit.json")["interrupted"])

    def test_signal_stop_and_finalization_still_runs(self):
        with tempfile.TemporaryDirectory() as temp:
            runner = self.runner(Path(temp))
            child = runner.start([sys.executable, "-c", "import time; time.sleep(60)"], "sleep")
            runner.halt.set()
            with self.assertRaises(RuntimeError):
                runner.wait(child, 10)
            runner.finalizing = True
            result = runner.run([sys.executable, "-c", 'print("{\\"exported\\":true}")'], "export")
            self.assertEqual(result, dict(exported=True))

    def test_failed_preflight_finally_collects_and_exports(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            runner = self.runner(root)
            c.save(root / "bundle/sha256.json", {})
            c.save(root / "started.json", dict(at=c.stamp()))
            calls = []
            with patch.object(runner, "observations_ready", side_effect=RuntimeError("capture not ready")), \
                 patch.object(runner, "get", return_value=dict(items=[])), \
                 patch.object(runner, "perf", side_effect=lambda *args: calls.append(args)):
                self.assertEqual(runner.execute(), 1)
            outcome = c.read(root / "outcome.json")
            self.assertEqual(outcome["stopReason"], "capture not ready")
            self.assertEqual(outcome["activeLocalChildren"], [])
            self.assertEqual(len(calls), 1)
            self.assertEqual(calls[0][0][:2], ["metrics", "export"])

    def test_no_overlap_lock(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "lock"
            with path.open("a") as first, path.open("a") as second:
                fcntl.flock(first, fcntl.LOCK_EX | fcntl.LOCK_NB)
                with self.assertRaises(BlockingIOError):
                    fcntl.flock(second, fcntl.LOCK_EX | fcntl.LOCK_NB)

    def test_cleanup_uid_precondition_and_pvc_refusal(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            runner = self.runner(root)
            c.save(root / "seed.json", dict(id="owned"))
            c.save(root / "owned.json", [dict(kind="Namespace", metadata=dict(name="owned", uid="exact-uid", labels={c.LABEL: "owned"}))])
            calls = []
            with patch.object(runner, "get", return_value=dict(items=[])), patch.object(runner, "kube", side_effect=lambda *args: calls.append(args)):
                runner.cleanup_population(root)
            self.assertEqual(c.read(root / "delete-exact-uid.json")["preconditions"], dict(uid="exact-uid"))
            self.assertIn("--raw", calls[0][1])
            with patch.object(runner, "get", return_value=dict(items=[dict(kind="PersistentVolumeClaim")])):
                with self.assertRaisesRegex(RuntimeError, "PVCs"):
                    runner.cleanup_population(root)

    def test_body_complete_summary_excludes_fast_shedding_and_body_timeouts(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp) / "api"
            directory.mkdir()
            c.save(directory / "result.json", dict(started="2026-01-01T00:00:00Z", finished="2026-01-01T00:00:12Z",
                   spec=dict(durationSeconds=10, targets=[dict(resource="deployments"), dict(resource="pods"), dict(resource="replicasets")]),
                   generatorLimited=2))
            rows = [dict(target=0, statusCode=200, latencyNanoseconds=2_000_000_000),
                    dict(target=1, statusCode=429, latencyNanoseconds=1_000_000),
                    dict(target=1, statusCode=200, errClass="timeout", latencyNanoseconds=10_000_000_000),
                    dict(target=2, statusCode=200, latencyNanoseconds=4_000_000_000)]
            with (directory / "requests.ndjson").open("w") as stream:
                for row in rows:
                    stream.write(c.json.dumps(row) + "\n")
            c.summarize_api(directory)
            value = c.read(directory.parent / "api-summary.json")
            self.assertEqual(value["successfulBodyComplete"], 2)
            self.assertAlmostEqual(value["successQPSIncludingDrain"], 2 / 12)
            self.assertEqual(value["successPerOfferedSecond"], .2)
            self.assertIsNone(value["successfulLatency"]["pods"]["p95Seconds"])
            self.assertEqual(value["successfulLatency"]["deployments"]["p95Seconds"], 2)
            self.assertIsNone(value["enforcedErrorRateSLO"])
            self.assertIsNone(value["goalSuccessQPS"], "a successful-QPS floor requires explicit user approval")

    def test_ready_requires_matching_external_handoff(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            runner = self.runner(root)
            path = root / "ready.json"
            runner.config["capture"]["readyFile"] = str(path)
            c.save(path, dict(captureReady=False, endpoint="http://127.0.0.1:1"))
            with self.assertRaisesRegex(RuntimeError, "matching readiness"):
                runner.observations_ready()

    def capture_target(self, source=100, health="up", instance="one"):
        return dict(labels=dict(instance=instance), scrapeUrl="http://"+instance+"/metrics", scrapePool="nodes",
                    scrapeInterval="5s", scrapeTimeout="5s", health=health, lastError="",
                    lastScrape=c.datetime.fromtimestamp(source, c.timezone.utc).isoformat())

    def test_transient_recovery_needs_distinct_fresh_scrapes(self):
        target = self.capture_target()
        health = c.CaptureHealth([target], 60)
        self.assertFalse(health.observe([target], 0, 100)["admissible"])
        self.assertFalse(health.observe([target], 5, 105)["admissible"])
        self.assertTrue(health.observe([self.capture_target(105)], 6, 106)["admissible"])
        self.assertTrue(health.observe([self.capture_target(110, "down")], 10, 110)["gaps"])
        self.assertFalse(health.observe([self.capture_target(115)], 15, 115)["admissible"])
        result = health.observe([self.capture_target(120)], 20, 120)
        self.assertTrue(result["admissible"])
        self.assertEqual(health.success[c.target_key(target)], 120)

    def test_persistent_down_stale_missing_identity_change_block(self):
        for rows in ([self.capture_target(100, "down")], [self.capture_target(1)], [], [self.capture_target(instance="replacement")]):
            with self.subTest(rows=rows):
                health = c.CaptureHealth([self.capture_target()], 60)
                self.assertFalse(health.observe(rows, 0, 100)["admissible"])
                self.assertFalse(health.observe(rows, 59, 159)["sustained"])
                result = health.observe(rows, 60, 160)
                self.assertTrue(result["sustained"])
                self.assertTrue(all(g["durationSeconds"] == 60 for g in result["gaps"]))

    def test_capture_gate_retry_interruption_and_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            runner = self.runner(root)
            ready_path, manifest = root / "ready.json", root / "manifest.json"
            future = "2099-01-01T00:00:00Z"
            c.save(ready_path, dict(captureReady=True, endpoint="http://127.0.0.1:1", tokenExpiresAt=future))
            c.save(manifest, dict(status="running", completedChunks=1, updated=c.stamp(), end=future))
            runner.config["capture"].update(readyFile=str(ready_path), streamManifests=[str(manifest)],
                expectedTargets=[self.capture_target()], lossSeconds=60, gateSeconds=120, pollSeconds=5)
            rows = [self.capture_target(time.time(), "down")]
            with patch.object(runner, "capture_sample", return_value=dict(targets=rows, history=[], timestamps=[], error=None)), patch.object(runner, "pause", side_effect=lambda _: runner.halt.set()):
                with self.assertRaisesRegex(RuntimeError, "stop requested"):
                    runner.observations_ready("next-arm")
            self.assertEqual(c.read(root / "capture-gates/next-arm/0001.json")["targets"], rows)
            self.assertEqual(c.read(root / "capture-gates/next-arm/outcome.json")["status"], "interrupted")
            c.save(ready_path, dict(captureReady=True, endpoint="http://127.0.0.1:2"))
            with self.assertRaisesRegex(RuntimeError, "matching readiness"):
                runner.observations_ready()

    def test_gate_deterministic_recovery_and_persistent_deadline(self):
        for recover in (True, False):
            with self.subTest(recover=recover), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                runner = self.runner(root)
                clock = [100.0]
                ready_path, manifest = root / "ready.json", root / "manifest.json"
                c.save(ready_path, dict(captureReady=True, endpoint="http://127.0.0.1:1", tokenExpiresAt="2099-01-01T00:00:00Z"))
                c.save(manifest, dict(status="running", completedChunks=1, updated=self.capture_target()["lastScrape"], end="2099-01-01T00:00:00Z"))
                runner.config["capture"].update(readyFile=str(ready_path), streamManifests=[str(manifest)],
                    expectedTargets=[self.capture_target()], lossSeconds=60, gateSeconds=120, pollSeconds=5)
                def targets():
                    return dict(targets=[self.capture_target(clock[0], "up" if recover and clock[0] >= 105 else "down")], history=[], timestamps=[], error=None)
                with patch.object(c.time, "monotonic", side_effect=lambda: clock[0]), \
                     patch.object(c.time, "time", side_effect=lambda: clock[0]), \
                     patch.object(runner, "capture_sample", side_effect=targets), \
                     patch.object(runner, "pause", side_effect=lambda seconds: clock.__setitem__(0, clock[0]+seconds)):
                    if recover:
                        runner.observations_ready("test")
                    else:
                        with self.assertRaisesRegex(RuntimeError, "sustained capture loss"):
                            runner.observations_ready("test")
                outcome = c.read(root / "capture-gates/test/outcome.json")
                self.assertEqual(outcome["attempts"], 3 if recover else 13)
                self.assertEqual(outcome["elapsedSeconds"], 10 if recover else 60)
                self.assertEqual(outcome["status"], "recovered-ready" if recover else "blocked")
                self.assertTrue(c.read(root / "capture-gates/test/0001.json")["gaps"])

    def test_source_loss_separate_from_confirmation_and_late_recovery(self):
        health = c.CaptureHealth([self.capture_target()], 60)
        health.observe([self.capture_target(100, "down")], 0, 100)
        self.assertFalse(health.observe([self.capture_target(155)], 55, 155)["admissible"])
        result = health.observe([self.capture_target(165)], 65, 165)
        self.assertFalse(result["sustained"])
        self.assertTrue(result["admissible"])
        health = c.CaptureHealth([self.capture_target()], 60)
        health.observe([self.capture_target(100)], 0, 100)
        self.assertTrue(health.observe([self.capture_target(165)], 65, 165)["sustained"])

    def test_sparse_polls_use_source_history_not_lookback_replay(self):
        health = c.CaptureHealth([self.capture_target()], 60)
        health.observe([self.capture_target(100)], 0, 100)
        history = [dict(metric=dict(instance="one"), values=[[110, "1"], [130, "1"], [150, "1"], [165, "0"]])]
        timestamps = [dict(metric=dict(instance="one"), values=[[110, "110"], [130, "130"], [150, "150"], [165, "165"]])]
        self.assertFalse(health.observe([self.capture_target(165, "down")], 65, 165, history, timestamps)["sustained"])
        self.assertEqual(health.success[c.target_key(self.capture_target())], 150)
        stale = [dict(metric=dict(instance="one"), values=[[220, "1"]])]
        replay = [dict(metric=dict(instance="one"), values=[[220, "150"]])]
        self.assertTrue(health.observe([self.capture_target(220, "down")], 120, 220, stale, replay)["sustained"])

    def test_historical_failure_resets_confirmation_not_success_clock(self):
        target = self.capture_target(105)
        key = c.target_key(target)
        health = c.CaptureHealth([target], 60)
        health.observe([target], 0, 105)
        history = [dict(metric=target["labels"], values=[[105, "1"], [110, "0"], [115, "1"]])]
        times = [dict(metric=target["labels"], values=[[105, "105"], [110, "110"], [115, "115"]])]
        result = health.observe([self.capture_target(115)], 10, 115, history, times)
        self.assertFalse(result["admissible"], "one post-failure success must not admit")
        self.assertEqual(health.confirmed[key], {115})
        self.assertEqual(health.success[key], 115)
        self.assertEqual(result["sourceFailures"], [dict(identity=key, sourceTime=110, value=0)])
        result = health.observe([self.capture_target(120)], 15, 120, history, times)
        self.assertTrue(result["admissible"], "two post-failure successes must admit despite replayed failure")
        failure = [dict(metric=target["labels"], values=[[125, "0"]])]
        failure_time = [dict(metric=target["labels"], values=[[125, "125"]])]
        health.observe([self.capture_target(125, "down")], 20, 125, failure, failure_time)
        self.assertEqual(health.success[key], 120, "failure must not refresh last-success clock")

    def test_fresh_history_witness_survives_later_stale_replay(self):
        for evaluations in ([120, 171], [171, 120]):
            with self.subTest(evaluations=evaluations):
                target = self.capture_target(100)
                health = c.CaptureHealth([target], 60)
                health.observe([target], 0, 100)
                history = [dict(metric=target["labels"], values=[[at, "1"] for at in evaluations])]
                times = [dict(metric=target["labels"], values=[[at, "120"] for at in evaluations])]
                result = health.observe([self.capture_target(172)], 72, 172, history, times)
                self.assertFalse(result["sustained"], "success100/120/172 has maximum gap52, not72")
                self.assertEqual(health.success[c.target_key(target)], 172)

    def test_invalid_or_future_history_is_not_a_success_witness(self):
        for evaluation, source, value in ((120, "121", "1"), (173, "120", "1"),
                (float("nan"), "120", "1"), (float("inf"), "120", "1"),
                (120, "nan", "1"), (120, "inf", "1"), (120, "invalid", "1"),
                (120, "120", "nan"), (120, "120", "inf"), (120, "120", "2"),
                (120, "120", "-1"), (120, "120", "invalid")):
            with self.subTest(evaluation=evaluation, source=source, value=value):
                target = self.capture_target(100)
                health = c.CaptureHealth([target], 60)
                health.observe([target], 0, 100)
                history = [dict(metric=target["labels"], values=[[evaluation, value]])]
                times = [dict(metric=target["labels"], values=[[evaluation, source]])]
                result = health.observe([self.capture_target(172)], 72, 172, history, times)
                self.assertTrue(result["sustained"], "invalid history must not bridge a real72s gap")

    def test_duplicates_unexpected_and_aliases_do_not_confirm(self):
        target = self.capture_target()
        health = c.CaptureHealth([target], 60)
        health.observe([target], 0, 100)
        self.assertFalse(health.observe([self.capture_target(105)]*2, 5, 105)["admissible"])
        self.assertFalse(health.observe([self.capture_target(110)], 10, 110)["admissible"])
        self.assertFalse(health.observe([self.capture_target(115), self.capture_target(115, instance="extra")], 15, 115)["admissible"])
        self.assertFalse(health.observe([self.capture_target(120)], 20, 120)["admissible"])
        alias = dict(target, scrapeUrl="http://one/other")
        health = c.CaptureHealth([target, alias], 60)
        health.observe([target, alias], 0, 100)
        rows = [dict(metric=dict(instance="one"), values=[[165, "1"]])]
        times = [dict(metric=dict(instance="one"), values=[[165, "165"]])]
        result = health.observe([self.capture_target(165), dict(alias, health="down", lastScrape=self.capture_target(165)["lastScrape"])], 65, 165, rows, times)
        self.assertIn(c.target_key(alias), result["sustained"])
        self.assertEqual(len(result["aliasHistoryNotAttributed"]), 2)

    def test_gate_precheck_and_postfetch_stop_have_outcomes(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            runner = self.runner(root)
            runner.config["capture"]["readyFile"] = str(root / "missing.json")
            with self.assertRaises(RuntimeError):
                runner.observations_ready("missing")
            self.assertEqual(c.read(root / "capture-gates/missing/outcome.json")["status"], "blocked")
            c.save(root / "ready.json", dict(captureReady=True, endpoint="http://127.0.0.1:1", tokenExpiresAt="2099-01-01T00:00:00Z"))
            c.save(root / "manifest.json", dict(status="running", completedChunks=1, updated=c.stamp(), end="2099-01-01T00:00:00Z"))
            runner.config["capture"].update(readyFile=str(root / "ready.json"), streamManifests=[str(root / "manifest.json")],
                expectedTargets=[self.capture_target()], lossSeconds=60, gateSeconds=120, pollSeconds=5)
            def fetch():
                runner.halt.set()
                return dict(targets=[self.capture_target(time.time())], history=[], timestamps=[], error=None)
            with patch.object(runner, "capture_sample", side_effect=fetch), patch.object(c.CaptureHealth, "observe", return_value=dict(admissible=True, sustained=[])):
                with self.assertRaisesRegex(RuntimeError, "stop requested"):
                    runner.observations_ready("fetch-stop")
            self.assertEqual(c.read(root / "capture-gates/fetch-stop/outcome.json")["status"], "interrupted")
            runner.halt.clear()
            with patch.object(runner, "capture_sample", side_effect=lambda: (setattr(runner, "deadline", 0) or dict(targets=[], history=[], timestamps=[], error=None))):
                with self.assertRaises(RuntimeError):
                    runner.observations_ready("fetch-deadline")
            self.assertNotEqual(c.read(root / "capture-gates/fetch-deadline/outcome.json")["status"], "recovered-ready")


if __name__ == "__main__":
    unittest.main()

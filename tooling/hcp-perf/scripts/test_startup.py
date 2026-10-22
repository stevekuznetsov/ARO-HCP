import copy
import contextlib
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import startup as s


IMAGE = "registry.invalid/test@sha256:" + "a" * 64


def container(name="main"):
    return dict(name=name, image=IMAGE, resources=dict(requests=dict(cpu="10m", memory="100Mi"),
                limits=dict(cpu="10m", memory="110Mi")), env=[dict(name="GOMEMLIMIT", value="83886080B")])


def obj(kind, name, uid=None, **kwargs):
    return dict(kind=kind, metadata=dict(name=name, uid=uid or name, namespace="hcp", generation=1), **kwargs)


def config():
    profiles = [dict(name=name, owner=dict(kind=kind, name=name, namespace="$hcpNamespace"), scope="controlled",
                     expectedReplicas=replicas, containers=[container()]) for name, kind, replicas in s.CORE]
    return s.validate(dict(version="v1", managementKubeconfig="/persistent/management", kubectl="/usr/bin/kubectl",
                          durableStateRoot="/persistent", identity=dict(csID="cs-exact", hcpNamespace="hcp"),
                          candidate=dict(id="test", profiles=profiles, resolvedPolicy=dict(spec={}), ledger=dict(totalUSD=.125)),
                          requirements=[dict(profile=name, readyReplicas=n, endpoints=name, readyEndpoints=n) for name, _, n in s.CORE]))


def healthy():
    objects = []
    for name, kind, replicas in s.CORE:
        owner = obj(kind, name, spec=dict(replicas=replicas, template=dict(metadata={}, spec=dict(containers=[container()]))),
                    status=dict(observedGeneration=1, replicas=replicas, readyReplicas=replicas, updatedReplicas=replicas,
                                availableReplicas=replicas, currentReplicas=replicas, currentRevision="revision", updateRevision="revision"))
        objects.append(owner)
        controller = owner
        if kind == "Deployment":
            controller = obj("ReplicaSet", name + "-rs", spec=copy.deepcopy(owner["spec"]))
            controller["metadata"]["ownerReferences"] = [dict(kind=kind, name=name, uid=name, controller=True)]
            objects.append(controller)
        addresses = []
        for i in range(replicas):
            pod = obj("Pod", name + "-" + str(i), spec=dict(containers=[container()], nodeName="node"),
                      status=dict(phase="Running", conditions=[dict(type="Ready", status="True")],
                                  containerStatuses=[dict(name="main", ready=True, restartCount=0, state=dict(running={}))]))
            pod["metadata"]["ownerReferences"] = [dict(kind=controller["kind"], name=controller["metadata"]["name"],
                                                        uid=controller["metadata"]["uid"], controller=True)]
            objects.append(pod)
            addresses.append(dict(ip="10.0.0." + str(i), targetRef=dict(kind="Pod", uid=pod["metadata"]["uid"])))
        objects.append(obj("Endpoints", name + "-endpoint", subsets=[dict(addresses=addresses)]))
        objects[-1]["metadata"]["name"] = name
    for kind in ("HostedCluster", "HostedControlPlane"):
        item = obj(kind, kind.lower(), status=dict(conditions=[dict(type="Available", status="True")]))
        item["metadata"]["labels"] = {s.CS_LABEL: "cs-exact"}
        objects.append(item)
    objects.append(obj("Namespace", "hcp", "namespace-uid"))
    return dict(objects=objects, errors=[], namespace="hcp", identity=dict(namespaceUID="namespace-uid"))


class Clock:
    def __init__(self):
        self.now = 0

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


class FakeKubectl:
    """API-free command stub with an allowlist; RP/exec/mutation calls fail tests."""
    def __init__(self, snapshots):
        self.snapshots, self.calls, self.index = snapshots, [], -1

    def command(self, args, deadline, max_bytes=16777216):
        assert args[0] in ("get", "logs"), args
        self.calls.append(args)
        if args[0] == "logs":
            assert any(a.startswith("--tail=") for a in args)
            assert any(a.startswith("--limit-bytes=") for a in args)
            return dict(stdout="the server could not find the requested resource", exitCode=0)
        if args[1] == "--raw":
            return dict(stdout='container_cpu_usage_seconds_total{namespace="hcp",pod="x"} 12\n'
                        'container_memory_rss{namespace="other"} 500\n', exitCode=0)
        if args[1] == "hostedclusters,hostedcontrolplanes":
            self.index = min(self.index + 1, len(self.snapshots) - 1)
            kinds = {"HostedCluster", "HostedControlPlane"}
        elif args[1] == "namespace":
            kinds = {"Namespace"}
        else:
            assert args[1] == s.RESOURCES, args
            kinds = {"Pod", "Deployment", "StatefulSet", "ReplicaSet", "Job", "CronJob", "DaemonSet", "Endpoints", "Service", "Event"}
        return dict(stdout=json.dumps(dict(kind="List", items=[o for o in self.snapshots[self.index]["objects"] if o["kind"] in kinds])), exitCode=0)


class StartupTests(unittest.TestCase):
    def test_ready_requires_helpers_all_core_endpoints_and_not_global_hash(self):
        snapshot, cfg = healthy(), config()
        for obj_ in snapshot["objects"]:
            obj_["metadata"]["annotations"] = {s.POLICY_HASH: "old-global-hash"}
        self.assertEqual(s.analyze(snapshot, cfg)["phase"], "ready")
        pod = next(o for o in snapshot["objects"] if o["kind"] == "Pod")
        pod["status"]["conditions"][0]["status"] = "False"
        pod["status"]["containerStatuses"].append(dict(name="helper", ready=False))
        result = s.analyze(snapshot, cfg)
        self.assertEqual(result["phase"], "progressing", "healthy main must not hide unready Pod helper")
        self.assertEqual(result["checks"][0]["ready"], 2)
        self.assertFalse(result["etcdQuorumProxy"]["proofOfQuorum"])
        for endpoint in snapshot["objects"]:
            if endpoint["kind"] == "Endpoints":
                endpoint["subsets"][0]["addresses"] *= 2
        self.assertEqual(s.analyze(snapshot, cfg)["checks"][0]["endpoints"], 2, "deduplicate Endpoint Pod UIDs")

    def test_gated_rollout_and_current_vs_desired(self):
        snapshot, cfg = healthy(), config()
        pod = next(o for o in snapshot["objects"] if o["kind"] == "Pod")
        pod["spec"]["containers"][0]["resources"]["requests"]["cpu"] = "9m"
        result = s.analyze(snapshot, cfg)
        self.assertEqual(result["phase"], "applying")
        self.assertTrue(result["mixedRollout"])
        self.assertFalse(result["failures"])
        self.assertTrue(next(r for r in result["profiles"] if r["role"] == "desired-template")["matchesExpected"])
        self.assertFalse(next(r for r in result["profiles"] if r["role"] == "current-pod")["matchesExpected"])

    def test_all_groups_oom_probe_completed_and_scheduling(self):
        snapshot = healthy()
        pod = next(o for o in snapshot["objects"] if o["kind"] == "Pod")
        for group, key in s.GROUPS.items():
            if group != "containers":
                pod["spec"][group] = [container(group)]
            pod["status"][key] = [dict(name=pod["spec"][group][0]["name"], restartCount=1,
                state=dict(waiting=dict(reason="CrashLoopBackOff")), lastState=dict(terminated=dict(reason="OOMKilled", exitCode=137)))]
        pod["status"]["conditions"] = [dict(type="PodScheduled", status="False", reason="Unschedulable", message="Insufficient cpu")]
        other = [o for o in snapshot["objects"] if o["kind"] == "Pod"][1]
        other["status"]["containerStatuses"][0]["state"] = dict(terminated=dict(reason="Completed", exitCode=0))
        other["status"]["conditions"][0]["status"] = "False"
        snapshot["objects"].append(obj("Event", "kill", reason="Killing", message="Container main failed liveness probe, will be restarted",
            involvedObject=dict(uid=other["metadata"]["uid"], name=other["metadata"]["name"], fieldPath="containers{main}")))
        result = s.analyze(snapshot, config())
        self.assertEqual(result["phase"], "failing")
        self.assertEqual({f["group"] for f in result["failures"] if f["reason"] == "OOMKilled"}, set(s.GROUPS))
        self.assertTrue(any(f["reason"] == "ProbeKill" for f in result["failures"]))
        self.assertTrue(any(f["reason"] == "PendingScheduling" for f in result["failures"]))
        self.assertFalse(any(f["reason"] == "OOMKilled" and f["pod"] == other["metadata"]["name"] for f in result["failures"]))

    def test_exact_identity_ambiguity_and_future_uids(self):
        objects = healthy()["objects"]
        bound = s.identity(objects, dict(csID="cs-exact"), {})
        self.assertEqual(bound["hcpNamespace"], "hcp")
        self.assertEqual(s.identity([], dict(csID="cs-exact"), {}), {})
        wrong = copy.deepcopy(objects)
        hcp = next(o for o in wrong if o["kind"] == "HostedControlPlane")
        hcp["metadata"]["uid"] = "future-new-uid"
        with self.assertRaisesRegex(ValueError, "replaced"):
            s.identity(wrong, dict(csID="cs-exact"), bound)
        self.assertEqual(s.identity(wrong, dict(csID="cs-exact"), {})["hcpUID"], "future-new-uid")
        with self.assertRaisesRegex(ValueError, "ambiguous"):
            s.identity(objects + [hcp], dict(csID="cs-exact"), {})
        hcp["metadata"]["labels"] = {"unrelated": "cs-exact"}
        self.assertEqual(s.identity([hcp], dict(csID="cs-exact"), {}), {})

    def test_unknown_owner_not_heuristic_or_silent_ignore(self):
        snapshot = healthy()
        snapshot["objects"] = [o for o in snapshot["objects"] if o["kind"] != "ReplicaSet"]
        result = s.analyze(snapshot, config())
        self.assertEqual(result["phase"], "unknown")
        self.assertTrue(any("missing controller UID" in e for e in result["unknown"]))

    def test_ignored_scope_stays_profile_locked_and_visible(self):
        snapshot, cfg = healthy(), config()
        ignored = cfg["candidate"]["profiles"][0]
        ignored.update(scope="ignored", reason="parent accounting exemption")
        result = s.analyze(snapshot, s.validate(cfg))
        self.assertEqual(result["phase"], "ready")
        self.assertTrue(any(r.get("scope") == "ignored" for r in result["profiles"]))
        pod = next(o for o in snapshot["objects"] if o["kind"] == "Pod")
        pod["spec"]["containers"][0]["image"] = IMAGE.replace("a" * 64, "b" * 64)
        self.assertEqual(s.analyze(snapshot, cfg)["phase"], "applying")

    def test_all_expected_profiles_must_be_present_in_both_scopes(self):
        for scope in ("controlled", "ignored"):
            with self.subTest(scope=scope):
                snapshot, cfg = healthy(), config()
                dns = dict(name="dns", owner=dict(kind="Deployment", name="dns", namespace="$hcpNamespace"),
                           scope=scope, reason="known downstream layout", expectedReplicas=1, containers=[container()])
                cfg["candidate"]["profiles"].append(dns)
                result = s.analyze(snapshot, s.validate(cfg))
                self.assertEqual(result["phase"], "progressing")
                check = next(c for c in result["profileChecks"] if c["profile"] == "dns")
                self.assertEqual(check["ownerCount"], 0)
                self.assertFalse(check["satisfied"])
                dns["lifecycle"] = dict(mode="disabled", reason="parent explicitly disabled DNS for this layout")
                dns["expectedReplicas"] = 0
                self.assertEqual(s.analyze(snapshot, s.validate(cfg))["phase"], "ready")

    def test_exact_replica_layout_and_controller_generation(self):
        for mutation in ("desired-four", "expected-four", "stale-generation", "missing-generation", "old-replicas",
                         "extra-pod", "stateful-revision", "absent-owner"):
            with self.subTest(mutation=mutation):
                snapshot, cfg = healthy(), config()
                owner = next(o for o in snapshot["objects"] if o["kind"] == "Deployment")
                if mutation == "desired-four":
                    owner["spec"]["replicas"] = 4
                elif mutation == "expected-four":
                    cfg["candidate"]["profiles"][0]["expectedReplicas"] = 4
                    owner["spec"]["replicas"] = 4
                elif mutation == "stale-generation":
                    owner["metadata"]["generation"] = 2
                elif mutation == "missing-generation":
                    del owner["status"]["observedGeneration"]
                elif mutation == "old-replicas":
                    owner["status"]["updatedReplicas"] = 2
                elif mutation == "extra-pod":
                    pod = copy.deepcopy(next(o for o in snapshot["objects"] if o["kind"] == "Pod"))
                    pod["metadata"].update(name="surge", uid="surge")
                    snapshot["objects"].append(pod)
                elif mutation == "stateful-revision":
                    next(o for o in snapshot["objects"] if o["kind"] == "StatefulSet")["status"]["updateRevision"] = "new"
                else:
                    snapshot["objects"].remove(owner)
                self.assertNotEqual(s.analyze(snapshot, s.validate(cfg))["phase"], "ready")
        cfg = config()
        del cfg["candidate"]["profiles"][0]["expectedReplicas"]
        with self.assertRaisesRegex(ValueError, "expectedReplicas required"):
            s.validate(cfg)

    def test_ignored_explicit_absent_limits_and_controlled_strictness(self):
        cfg, snapshot = config(), healthy()
        p = cfg["candidate"]["profiles"][0]
        p.update(scope="ignored", reason="downstream unlimited policy")
        del p["containers"][0]["resources"]["limits"]
        for item in snapshot["objects"]:
            if item["metadata"]["name"].startswith("kube-apiserver") and item["kind"] in ("Pod", "ReplicaSet", "Deployment"):
                spec = item["spec"] if item["kind"] == "Pod" else item["spec"]["template"]["spec"]
                spec["containers"][0]["resources"]["limits"] = {}
        self.assertEqual(s.analyze(snapshot, s.validate(cfg))["phase"], "ready")
        pod = next(o for o in snapshot["objects"] if o["kind"] == "Pod")
        pod["spec"]["containers"][0]["resources"]["limits"] = dict(cpu="0")
        self.assertEqual(s.analyze(snapshot, cfg)["phase"], "applying", "absent is not zero")
        p["scope"] = "controlled"
        with self.assertRaisesRegex(ValueError, "controlled expected requests and limits"):
            s.validate(cfg)

    def test_init_order_including_restartable_sidecars_is_significant(self):
        for restartable in (False, True):
            with self.subTest(restartable=restartable):
                snapshot, cfg = healthy(), config()
                initializers = [container("z-first"), container("a-second")]
                if restartable:
                    initializers[0]["restartPolicy"] = "Always"
                cfg["candidate"]["profiles"][0]["initContainers"] = copy.deepcopy(initializers)
                for item in snapshot["objects"]:
                    if item["metadata"]["name"].startswith("kube-apiserver") and item["kind"] in ("Pod", "ReplicaSet", "Deployment"):
                        spec = item["spec"] if item["kind"] == "Pod" else item["spec"]["template"]["spec"]
                        spec["initContainers"] = copy.deepcopy(initializers)
                self.assertEqual(s.analyze(snapshot, s.validate(cfg))["phase"], "ready")
                pod = next(o for o in snapshot["objects"] if o["kind"] == "Pod")
                pod["spec"]["initContainers"].reverse()
                self.assertEqual(s.analyze(snapshot, cfg)["phase"], "applying")
        self.assertEqual(s.profile(dict(containers=initializers)), s.profile(dict(containers=list(reversed(initializers)))))

    def test_job_cronjob_lifecycle_is_explicit(self):
        for kind in ("Job", "CronJob"):
            for mode in ("present", "completed", "optional", "disabled"):
                with self.subTest(kind=kind, mode=mode):
                    cfg, snapshot = config(), healthy()
                    p = dict(name="bootstrap", owner=dict(kind=kind, name="bootstrap", namespace="$hcpNamespace"),
                             scope="controlled", containers=[container()], lifecycle=dict(mode=mode, reason="reviewed lifecycle"))
                    cfg["candidate"]["profiles"].append(p)
                    s.validate(cfg)
                    self.assertEqual(s.analyze(snapshot, cfg)["phase"], "ready" if mode in ("optional", "disabled") else "progressing")
                    job = obj("Job", "bootstrap" if kind == "Job" else "bootstrap-run", spec=dict(template=dict(metadata={}, spec=dict(containers=[container()]))))
                    if kind == "CronJob":
                        cron = obj(kind, "bootstrap", spec=dict(jobTemplate=dict(spec=copy.deepcopy(job["spec"]))))
                        job["metadata"]["ownerReferences"] = [dict(kind=kind, name="bootstrap", uid="bootstrap", controller=True)]
                        snapshot["objects"].append(cron)
                    snapshot["objects"].append(job)
                    self.assertEqual(s.analyze(snapshot, cfg)["phase"], "ready" if mode in ("present", "optional") else "progressing")
                    job["status"] = dict(conditions=[dict(type="Complete", status="True")])
                    self.assertEqual(s.analyze(snapshot, cfg)["phase"], "progressing" if mode == "disabled" else "ready")
                    job["status"] = dict(conditions=[dict(type="Failed", status="True")])
                    self.assertEqual(s.analyze(snapshot, cfg)["phase"], "failing" if mode != "disabled" else "progressing")
                    del p["lifecycle"]
                    with self.assertRaisesRegex(ValueError, "explicit lifecycle"):
                        s.validate(cfg)

    def test_missing_namespace_response_blocks_ready_and_empty_coverage(self):
        for previous_binding in (False, True):
            with self.subTest(previous_binding=previous_binding), tempfile.TemporaryDirectory() as temp:
                cfg, snapshot, clock = config(), healthy(), Clock()
                cfg["maxDurationSeconds"] = 15
                snapshot["objects"] = [o for o in snapshot["objects"] if o["kind"] != "Namespace"]
                client = FakeKubectl([snapshot])
                original_command = client.command
                def command(args, deadline, max_bytes=16777216):
                    receipt = original_command(args, deadline, max_bytes)
                    if args[1] == "namespace":
                        receipt["stdout"] = ""  # kubectl --ignore-not-found succeeds with no body.
                    return receipt
                client.command = command
                monitor = s.Monitor(cfg, temp, client, clock.monotonic, clock.sleep)
                if previous_binding:
                    monitor.bound = dict(hcpNamespace="hcp", namespaceUID="namespace-uid")
                result = monitor.run()
                self.assertEqual(result["last"]["phase"], "unknown")
                self.assertFalse(result["coverage"]["emptyNamespaceObserved"])
                self.assertTrue(result["coverage"]["gaps"])
                self.assertIn("namespace absent or UID unverified", result["last"]["unknown"])
        snapshot = healthy()
        del snapshot["identity"]["namespaceUID"]
        self.assertEqual(s.analyze(snapshot, config())["phase"], "unknown")

    def test_daemonset_exact_counts_and_completed_job_pods(self):
        cfg, snapshot = config(), healthy()
        p = dict(name="dns", owner=dict(kind="DaemonSet", name="dns", namespace="$hcpNamespace"),
                 scope="controlled", expectedReplicas=1, containers=[container()])
        cfg["candidate"]["profiles"].append(p)
        daemon = obj("DaemonSet", "dns", spec=dict(template=dict(metadata={}, spec=dict(containers=[container()]))),
                     status=dict(observedGeneration=1, desiredNumberScheduled=1, currentNumberScheduled=1,
                                 numberReady=1, numberAvailable=1, updatedNumberScheduled=1, numberMisscheduled=0))
        pod = copy.deepcopy(next(o for o in snapshot["objects"] if o["kind"] == "Pod"))
        pod["metadata"].update(name="dns-pod", uid="dns-pod", ownerReferences=[dict(kind="DaemonSet", name="dns", uid="dns", controller=True)])
        snapshot["objects"].extend([daemon, pod])
        self.assertEqual(s.analyze(snapshot, s.validate(cfg))["phase"], "ready")
        daemon["status"]["desiredNumberScheduled"] = 2
        self.assertEqual(s.analyze(snapshot, cfg)["phase"], "progressing")
        cfg, snapshot = config(), healthy()
        cfg["candidate"]["profiles"].append(dict(name="bootstrap", owner=dict(kind="Job", name="bootstrap", namespace="$hcpNamespace"),
            scope="controlled", containers=[container()], lifecycle=dict(mode="completed", reason="must finish bootstrap")))
        job = obj("Job", "bootstrap", spec=dict(template=dict(metadata={}, spec=dict(containers=[container()]))), status={})
        pod["metadata"]["ownerReferences"] = [dict(kind="Job", name="bootstrap", uid="bootstrap", controller=True)]
        pod["status"] = dict(phase="Succeeded")
        snapshot["objects"].extend([job, pod])
        self.assertEqual(s.analyze(snapshot, s.validate(cfg))["phase"], "progressing")
        job["status"]["conditions"] = [dict(type="Complete", status="True")]
        self.assertEqual(s.analyze(snapshot, cfg)["phase"], "ready")

    def test_no_namespace_and_api_failure_keep_observing_unknown(self):
        cfg, clock = config(), Clock()
        cfg["identity"].pop("hcpNamespace")
        cfg["maxDurationSeconds"] = 45
        client = FakeKubectl([dict(objects=[])])
        with tempfile.TemporaryDirectory() as temp:
            result = s.Monitor(cfg, temp, client, clock.monotonic, clock.sleep).run()
            self.assertEqual(result["outcome"], "deadline")
            self.assertEqual(result["last"]["phase"], "unknown")
            self.assertEqual(result["snapshots"], 3)
            self.assertFalse(result["coverage"]["emptyNamespaceObserved"])
        snapshot = healthy()
        snapshot["errors"] = ["pods: forbidden"]
        self.assertEqual(s.analyze(snapshot, config())["phase"], "unknown")

    def test_completed_init_is_meaningful_progress_not_timestamp(self):
        snapshot, cfg = healthy(), config()
        pod = next(o for o in snapshot["objects"] if o["kind"] == "Pod")
        pod["spec"]["initContainers"] = [container("bootstrap")]
        before = s.analyze(snapshot, cfg)["milestones"]
        pod["status"]["initContainerStatuses"] = [dict(name="bootstrap", state=dict(terminated=dict(exitCode=0)))]
        after = s.analyze(snapshot, cfg)["milestones"]
        self.assertGreater(len(after), len(before))
        pod["metadata"]["resourceVersion"] = "new"
        self.assertEqual(after, s.analyze(snapshot, cfg)["milestones"])

    def test_empty_before_first_pod_no_rp_and_unchanged_stall(self):
        clock, cfg = Clock(), config()
        cfg.update(maxDurationSeconds=240, stallSeconds=180)
        empty = dict(objects=[obj("Namespace", "hcp", "namespace-uid")])
        snapshot = healthy()
        for pod in snapshot["objects"]:
            if pod["kind"] == "Pod":
                pod["status"]["conditions"][0]["status"] = "False"
        client = FakeKubectl([empty, snapshot])
        with tempfile.TemporaryDirectory() as temp:
            monitor = s.Monitor(cfg, temp, client, clock.monotonic, clock.sleep)
            result = monitor.run()
            self.assertEqual(result["outcome"], "deadline")
            self.assertTrue(result["last"]["stalled"])
            self.assertTrue(result["coverage"]["firstPod"]["emptyNamespaceObservedBefore"])
            first = s.read(Path(temp) / "snapshot-000001.json")
            self.assertEqual(first["analysis"]["phase"], "not-applied")
            self.assertFalse(result["rpQueried"])
            self.assertEqual(len(client.calls), result["snapshots"] * 3)
            self.assertEqual(len((Path(temp) / "progress.jsonl").read_text().splitlines()), result["snapshots"])

    def test_failure_window_aggregation_diagnostics_and_metrics(self):
        clock, cfg, snapshot = Clock(), config(), healthy()
        cfg.update(maxDurationSeconds=300)
        cfg["nodeMetrics"]["enabled"] = True
        pod = next(o for o in snapshot["objects"] if o["kind"] == "Pod")
        pod["status"]["containerStatuses"][0].update(restartCount=1, lastState=dict(terminated=dict(reason="OOMKilled")))
        client = FakeKubectl([snapshot])
        with tempfile.TemporaryDirectory() as temp:
            result = s.Monitor(cfg, temp, client, clock.monotonic, clock.sleep).run()
            self.assertEqual(result["outcome"], "failure-window-complete")
            self.assertEqual(result["elapsedSeconds"], 120)
            self.assertEqual(len([c for c in client.calls if c[0] == "logs"]), 4, "bounded current/previous logs, no heavy all-log polling")
            snap = s.read(Path(temp) / "snapshot-000001.json")
            self.assertTrue(snap["diagnostics"]["logFindings"])
            metrics = next(r for r in snap["diagnostics"]["records"] if r["source"].get("source") == "kubelet-cadvisor")
            self.assertIn(" 12", metrics["receipt"]["stdout"])
            self.assertNotIn("other", metrics["receipt"]["stdout"])

    def test_signal_and_clipped_failure_metadata(self):
        cfg, snapshot, clock = config(), healthy(), Clock()
        cfg["maxDurationSeconds"] = 30
        pod = next(o for o in snapshot["objects"] if o["kind"] == "Pod")
        pod["status"]["containerStatuses"][0]["restartCount"] = 1
        with tempfile.TemporaryDirectory() as temp:
            monitor = s.Monitor(cfg, temp, FakeKubectl([snapshot]), clock.monotonic, clock.sleep)
            def stop(seconds):
                clock.sleep(seconds)
                monitor.stop(signal.SIGTERM, None)
            monitor.sleep = stop
            result = monitor.run()
            self.assertEqual(result["outcome"], "interrupted")
            self.assertEqual(result["signal"], "SIGTERM")
            self.assertTrue(result["last"]["failureWindowClipped"])
            self.assertFalse(result["loadStarted"])

    def test_freeze_relative_inputs_permissions_and_offline_report(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cfg = config()
            cfg.update(durableStateRoot=".", managementKubeconfig="management", kubectl=sys.executable)
            (root / "management").write_text("fake credential, never embedded")
            for field in ("profiles", "resolvedPolicy", "ledger"):
                data = s.encoded(cfg["candidate"][field])
                (root / (field + ".json")).write_bytes(data)
                cfg["candidate"][field] = dict(path=field + ".json", sha256=s.digest(data))
            s.save(root / "input.json", cfg)
            with patch.object(s, "VOLATILE", ()):
                result = s.freeze(root / "input.json", root / "run")
            output = root / "run"
            self.assertEqual(output.stat().st_mode & 0o777, 0o700)
            self.assertEqual((output / "bundle/config.json").stat().st_mode & 0o777, 0o600)
            self.assertIn(str(output / "bundle/startup.py"), result["entrypoint"])
            self.assertEqual(s.read(output / "bundle/config.json")["managementKubeconfig"], str(root / "management"))
            (root / "profiles.json").unlink()
            s.save(output / "snapshot-000001.json", healthy())
            s.save(output / "started.json", dict(pid=123))
            before = {str(p): p.stat().st_mtime_ns for p in output.rglob("*")}
            with patch.object(subprocess, "Popen", side_effect=AssertionError("offline only")):
                report = s.diagnose(output)
            self.assertEqual(report["state"], "started-without-outcome-interrupted-or-unknown")
            self.assertEqual(report["latest"]["phase"], "ready")
            self.assertEqual(before, {str(p): p.stat().st_mtime_ns for p in output.rglob("*")})
            self.assertFalse(report["integrityErrors"])
            # An interrupted bundle cannot be launched, even with a frozen entrypoint.
            with patch.object(s, "VOLATILE", ()), patch.object(s, "__file__", str(output / "bundle/startup.py")), \
                    patch.object(sys, "argv", ["startup.py", "observe", "--output", str(output)]), \
                    patch.object(s, "Kubectl", side_effect=AssertionError("must not contact API")), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(s.main(), 1)
            (output / "bundle/startup.py").write_text("changed")
            self.assertTrue(s.diagnose(output)["integrityErrors"])

    def test_volatile_and_symlink_paths(self):
        with self.assertRaisesRegex(ValueError, "volatile"):
            s.persistent("/tmp/kubeconfig")
        with tempfile.TemporaryDirectory() as temp:
            link = Path(temp) / "link"
            link.symlink_to("/run")
            with self.assertRaisesRegex(ValueError, "volatile"):
                s.persistent(link / "credentials")

    def test_input_bounds_and_ambiguity(self):
        for field, value in (("version", "v2"), ("pollSeconds", 1), ("maxDurationSeconds", 8000), ("rpEndpoint", "anything")):
            cfg = config()
            cfg[field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                s.validate(cfg)
        cfg = config()
        cfg["candidate"]["profiles"].append(copy.deepcopy(cfg["candidate"]["profiles"][0]))
        with self.assertRaisesRegex(ValueError, "duplicate"):
            s.validate(cfg)
        cfg = config()
        cfg["requirements"][0]["readyReplicas"] = 2
        with self.assertRaisesRegex(ValueError, "required core"):
            s.validate(cfg)

    def test_subprocess_stub_bounds_and_no_cluster(self):
        # Execute Python rather than kubectl; the stub observes the actual argv contract.
        with tempfile.TemporaryDirectory() as temp:
            stub = Path(temp) / "stub.py"
            stub.write_text("import sys\nassert '--request-timeout=10s' in sys.argv\nprint('x' * 10000)\n")
            cfg = config()
            cfg["kubectl"] = sys.executable
            client = s.Kubectl(cfg)
            real_popen = subprocess.Popen
            def fake_popen(argv, **kwargs):
                return real_popen([sys.executable, str(stub), *argv[1:]], **kwargs)
            with patch.object(s.subprocess, "Popen", side_effect=fake_popen):
                receipt = client.command(["get", "pods"], time.monotonic() + 3, max_bytes=200)
            self.assertEqual(receipt["error"], "response size bound")
            self.assertEqual(len(receipt["stdout"]), 200)


if __name__ == "__main__":
    unittest.main()

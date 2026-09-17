"""Synthetic range-query fixtures for the offline observed processor."""

import copy
import json
import unittest
from unittest.mock import patch

import observed.process as processor
from observed.process import SIZE_LABEL, _quantity, process_bundle


def bundle():
    return {"schema_version": 1, "at": 7200, "start": 3600, "step_seconds": 300,
            "window_seconds": 3600, "search_start": 0, "settle_seconds": 900,
            "sources": [{"environment": "prod", "url": "https://metrics.example"}],
            "queries": [], "snapshots": [], "errors": [], "warnings": []}


def query(raw, metric, labels, values=1, times=None, cluster="mc", environment="prod", required=False):
    times = list(range(3600, 7201, 300)) if times is None else list(times)
    if not isinstance(values, list):
        values = [values] * len(times)
    raw["queries"].append({"environment": environment, "region": "eastus", "cluster": cluster,
                           "metric": metric, "required": required, "start": raw["start"],
                           "end": raw["at"], "step": raw["step_seconds"],
                           "series": [{"labels": {"cluster": cluster, **labels},
                                       "samples": list(zip(times, values))}]})


def snapshot(raw, size="small", timestamp=0, uid="hcp", cluster="mc", event="Update", name="customer.one"):
    raw["snapshots"].append({"environment": "prod", "region": "eastus", "cluster": cluster,
                             "timestamp": timestamp, "event": event, "uid": uid,
                             "namespace": "ocm-test", "name": name,
                             "object": {"metadata": {"namespace": "ocm-test", "name": name,
                                                       "uid": uid, "labels": {SIZE_LABEL: size}}}})


def inventory(raw, times=None, node="node-a", uid="pod-a", cluster="mc", environment="prod"):
    labels = {"namespace": "ocm-test-customer-one", "pod": "api-0", "uid": uid, "node": node}
    query(raw, "pod_info", labels, times=times, cluster=cluster, environment=environment)
    query(raw, "pod_phase", {**labels, "phase": "Running"}, times=times, cluster=cluster, environment=environment)
    query(raw, "node_info", {"node": node}, times=times, cluster=cluster, environment=environment)
    return labels


def node_snapshot(raw, timestamp=3600, uid="node-uid", name="node-a", cluster="mc", environment="prod",
                  event="Update", sku="Standard_D16", created="1970-01-01T00:00:00Z"):
    row = {"environment": environment, "region": "eastus", "cluster": cluster,
           "timestamp": timestamp, "event": event, "uid": uid, "name": name,
           "object": {"apiVersion": "v1", "kind": "Node",
                      "metadata": {"name": name, "uid": uid, "creationTimestamp": created,
                                   "labels": {"node.kubernetes.io/instance-type": sku,
                                              "kubernetes.azure.com/agentpool": "workers",
                                              "topology.kubernetes.io/zone": "eastus-1"}},
                      "spec": {"providerID": "azure:///node", "unschedulable": True,
                               "taints": [{"key": "test", "effect": "NoSchedule"}]},
                      "status": {"capacity": {"cpu": "16", "memory": "64Gi", "pods": "110",
                                               "aro.openshift.io/swift-nic": "16"},
                                 "allocatable": {"cpu": "15500m", "memory": "60Gi", "pods": "100",
                                                 "aro.openshift.io/swift-nic": "15"},
                                 "conditions": [{"type": "Ready", "status": "False"}]}}}
    raw.setdefault("node_snapshots", []).append(row)
    return row


def usage(raw, labels, times=None, cpu=2, memory=2**20, **kwargs):
    query(raw, "cpu", {**labels, "container": "main"}, cpu, times, **kwargs)
    query(raw, "memory", {**labels, "container": "main"}, memory, times, **kwargs)


def first_pod(view):
    return view["management_clusters"][0]["nodes"][0]["pods"][0]


class ProcessTests(unittest.TestCase):
    def test_first_observed_rate_warmup_is_bounded_and_leading_only(self):
        for first_seen, missing, warmup in (
                (3900, {3900}, True),
                (3900, {3900, 4200}, False),
                (3900, {3900, 4500}, False),
                (3900, {4500}, False),
                (3600, {3600}, False),
                (3900, set(range(3900, 7200, 300)), False)):
            with self.subTest(first_seen=first_seen, missing=missing):
                raw = bundle()
                snapshot(raw)
                times = list(range(first_seen, 7201, 300))
                labels = inventory(raw, times=times)
                query(raw, "memory", {**labels, "container": "main"}, 2**20, times)
                query(raw, "cpu", {**labels, "container": "main"}, 2,
                      [t for t in times if t not in missing])
                original = copy.deepcopy(raw)
                view = process_bundle(raw)
                self.assertEqual(raw, original)
                pod = first_pod(view)
                self.assertTrue(pod["current"])
                self.assertEqual(pod["phase"], "running")
                self.assertIsNone(pod["usage"]["cpu_mc"])
                self.assertAlmostEqual(pod["coverage"]["cpu"], 1 - len(missing) / (len(times) - 1))
                self.assertEqual(pod["coverage"]["memory"], 1)
                self.assertEqual(pod["usage"]["mem_mib"], (7200 - first_seen) / 3600)
                self.assertEqual(bool(view["errors"]), not warmup)
                self.assertEqual(pod["usage_issues"], ["startup" if warmup else "sampling-gap"])
                self.assertEqual(any("startup/first-observed sampling gap" in w
                                     for w in view["management_clusters"][0]["warnings"]), warmup)

    def test_first_observed_warmup_does_not_hide_query_error_or_current_omission(self):
        for failure in ("query", "current"):
            with self.subTest(failure=failure):
                raw = bundle()
                snapshot(raw)
                labels = inventory(raw, times=range(3900, 7201, 300))
                usage(raw, labels, times=range(4200, 7201, 300))
                if failure == "query":
                    raw["queries"][-2]["error"] = "unavailable"
                else:
                    raw["queries"][0]["series"][0]["samples"].pop()
                view = process_bundle(raw)
                pod = first_pod(view)
                self.assertEqual(pod["current"], failure == "query")
                self.assertIsNone(pod["usage"]["cpu_mc"])
                self.assertAlmostEqual(pod["coverage"]["cpu"], 10 / 11)
                self.assertEqual(any("unavailable" in e for e in view["errors"]), failure == "query")
                if failure == "current":
                    self.assertNotIn("startup", pod["usage_issues"])

    def test_size_initialization_before_first_contribution_is_not_uncertainty(self):
        for unknown_at in (3899.877, 3900, 4500):
            with self.subTest(unknown_at=unknown_at):
                raw = bundle()
                snapshot(raw)
                snapshot(raw, size=None, timestamp=unknown_at)
                snapshot(raw, timestamp=unknown_at + .123)
                labels = inventory(raw, times=range(3900, 7201, 300))
                usage(raw, labels, times=range(3900, 7201, 300))
                view = process_bundle(raw)
                self.assertEqual(any("missing assigned size" in e for e in view["errors"]), unknown_at >= 3900)
                self.assertEqual(view["transitions"], [])
                self.assertIsNone(view["suggestion"])
                self.assertEqual(first_pod(view)["hcp_size"], "small")
                if unknown_at < 3900:
                    self.assertEqual(view["errors"], [])

    def test_size_baseline_uses_earliest_namespace_telemetry_not_only_inventory(self):
        for namespace in ("ocm-test", "ocm-test-customer-one"):
            with self.subTest(namespace=namespace):
                raw = bundle()
                snapshot(raw, size=None)
                snapshot(raw, timestamp=4200)
                labels = inventory(raw, times=range(4500, 7201, 300))
                usage(raw, labels, times=range(4500, 7201, 300))
                query(raw, "memory", {**labels, "namespace": namespace, "container": "main"}, 2**20, [3900])
                view = process_bundle(raw)
                self.assertTrue(any("missing assigned size" in e for e in view["errors"]))

    def test_known_resize_before_first_contribution_still_blocks_full_window(self):
        raw = bundle()
        snapshot(raw)
        snapshot(raw, size="large", timestamp=3900)
        labels = inventory(raw, times=range(5100, 7201, 300))
        usage(raw, labels, times=range(5100, 7201, 300))
        view = process_bundle(raw)
        self.assertTrue(any("transition" in e for e in view["errors"]))
        self.assertEqual([(t["from_size"], t["to_size"]) for t in view["transitions"]], [("small", "large")])

    def test_startup_gaps_warn_but_running_gaps_block(self):
        for phase, missing_at, blocking in (("pending", 3600, False), ("unknown", 3600, False),
                                           ("pending", 4200, True), ("running", 3600, True)):
            with self.subTest(phase=phase, missing_at=missing_at):
                raw = bundle()
                snapshot(raw)
                labels = inventory(raw)
                if phase != "running":
                    raw["queries"][1]["series"][0]["samples"][0] = (3600, 0)
                    query(raw, "pod_phase", {**labels, "phase": phase}, times=[3600])
                usage(raw, labels, times=[t for t in range(3600, 7201, 300) if t != missing_at])
                view = process_bundle(raw)
                pod = first_pod(view)
                self.assertTrue(pod["current"])
                self.assertEqual(pod["phase"], "running")
                self.assertEqual(pod["usage"], {"cpu_mc": None, "mem_mib": None})
                self.assertEqual(pod["coverage"], {"cpu": 11 / 12, "memory": 11 / 12})
                self.assertEqual(bool(view["errors"]), blocking)
                self.assertIn("sampling-gap" if blocking else "startup", pod["usage_issues"])
                diagnostics = view["errors"] if blocking else view["management_clusters"][0]["warnings"]
                self.assertTrue(any("incomplete cpu coverage" in d for d in diagnostics))

    def test_query_failures_block_even_for_pending_or_historical_pods(self):
        for phase in ("pending", "running"):
            for required in (False, True):
                with self.subTest(phase=phase, required=required):
                    raw = bundle()
                    snapshot(raw)
                    labels = inventory(raw, times=None if phase == "pending" else [3600, 3900])
                    raw["queries"][1]["series"][0]["labels"]["phase"] = phase
                    query(raw, "cpu", {**labels, "container": "main"}, times=[], required=required)
                    raw["queries"][-1]["error"] = "query unavailable"
                    view = process_bundle(raw)
                    self.assertTrue(any("query unavailable" in e for e in view["errors"]))
                    self.assertIsNone(first_pod(view)["usage"]["cpu_mc"])

    def test_historical_incomplete_usage_and_trailing_metrics_are_nonblocking(self):
        raw = bundle()
        snapshot(raw)
        labels = inventory(raw, times=[3600, 3900, 4200])
        usage(raw, labels, times=[3600, 4200, 4500])
        view = process_bundle(raw)
        pod = first_pod(view)
        self.assertFalse(pod["current"])
        self.assertEqual(pod["usage"], {"cpu_mc": None, "mem_mib": None})
        self.assertEqual(pod["coverage"], {"cpu": 2 / 3, "memory": 2 / 3})
        self.assertEqual(view["errors"], [])
        warnings = view["management_clusters"][0]["warnings"]
        self.assertTrue(any("no active pod_info" in w for w in warnings))
        self.assertTrue(any("incomplete cpu coverage" in w for w in warnings))

    def test_instant_only_pod_has_unknown_lifetime_not_zero_usage(self):
        raw = bundle()
        snapshot(raw)
        labels = inventory(raw, times=[7200])
        usage(raw, labels, times=[7200])
        view = process_bundle(raw)
        pod = first_pod(view)
        self.assertTrue(pod["current"])
        self.assertEqual(pod["usage"], {"cpu_mc": None, "mem_mib": None})
        self.assertEqual(pod["coverage"], {"cpu": 0, "memory": 0})
        self.assertEqual(pod["usage_issues"], ["unknown-lifetime"])
        warnings = view["management_clusters"][0]["warnings"]
        self.assertTrue(any("unknown lifetime" in w for w in warnings))
        self.assertFalse(any("0/0" in w for w in warnings))
        self.assertEqual(view["errors"], [])

    def test_unmatched_stale_history_and_resize_do_not_block(self):
        for matched in (False, True):
            for metric in ("pod_info", "cpu"):
                with self.subTest(matched=matched, metric=metric):
                    raw = bundle()
                    snapshot(raw)
                    snapshot(raw, uid="stale", name="unused", timestamp=-86401)
                    snapshot(raw, uid="stale", name="unused", timestamp=6000, size="large")
                    labels = inventory(raw)
                    usage(raw, labels)
                    # Out-of-window and null records do not establish contribution.
                    extra = {**labels, "namespace": "ocm-test-unused", "container": "main"}
                    query(raw, metric, extra, times=[3300])
                    query(raw, metric, extra, values=None)
                    if matched:
                        query(raw, metric, extra)
                    view = process_bundle(raw)
                    self.assertEqual(any("stale baseline" in e for e in view["errors"]), matched)
                    self.assertEqual(any("transition" in e for e in view["errors"]), matched)
                    self.assertEqual([t["hcp_id"] for t in view["transitions"]], ["stale"] if matched else [])
                    self.assertTrue(any("stale baseline" in w for w in view["management_clusters"][0]["warnings"]))
                    if not matched:
                        self.assertEqual(view["errors"], [])

    def test_initial_size_assignment_is_not_resize_or_backfilled(self):
        for assigned_at in (3300, 4500):
            with self.subTest(assigned_at=assigned_at):
                raw = bundle()
                snapshot(raw, size=None)
                snapshot(raw, timestamp=assigned_at)
                labels = inventory(raw)
                usage(raw, labels)
                early = inventory(raw, uid="early", times=[3600, 3900])
                usage(raw, early, times=[3600, 3900])
                view = process_bundle(raw)
                self.assertEqual(view["transitions"], [])
                self.assertIsNone(view["suggestion"])
                self.assertFalse(any("transition" in e for e in view["errors"]))
                self.assertEqual(any("missing assigned size" in e for e in view["errors"]), assigned_at > 3600)
                pods = view["management_clusters"][0]["nodes"][0]["pods"]
                self.assertEqual([p["hcp_size"] for p in pods],
                                 [None if assigned_at > 3600 else "small", "small"])
                snapshot(raw, timestamp=6000, size="large")
                resized = process_bundle(raw)
                self.assertEqual([(t["from_size"], t["to_size"]) for t in resized["transitions"]],
                                 [("small", "large")])
                self.assertTrue(any("transition" in e for e in resized["errors"]))

    def test_snapshot_conflicts_block_only_contributing_hcps(self):
        for matched in (False, True):
            with self.subTest(matched=matched):
                raw = bundle()
                snapshot(raw)
                snapshot(raw, size="large")
                if matched:
                    labels = inventory(raw)
                    usage(raw, labels)
                view = process_bundle(raw)
                self.assertEqual(any("conflicting snapshots" in e for e in view["errors"]), matched)
                if not matched:
                    self.assertTrue(any("conflicting snapshots" in w
                                        for w in view["management_clusters"][0]["warnings"]))
                    self.assertEqual(view["errors"], [])

    def test_pending_usage_has_explicit_presentation_reason(self):
        raw = bundle()
        labels = inventory(raw)
        for q in raw["queries"]:
            if q["metric"] == "pod_phase":
                q["series"][0]["labels"]["phase"] = "Pending"
        pod = first_pod(process_bundle(raw))
        self.assertEqual(pod["phase"], "pending")
        self.assertEqual(pod["usage_issues"], ["pending"])
        self.assertIsNone(pod["usage"]["cpu_mc"])

    def test_historical_gap_is_not_reported_as_pending(self):
        raw = bundle()
        inventory(raw, times=range(3600, 4500, 300))
        pod = first_pod(process_bundle(raw))
        self.assertFalse(pod["current"])
        self.assertEqual(pod["phase"], "running")
        self.assertEqual(pod["usage_issues"], ["sampling-gap"])

    def test_conflicting_phase_is_retained_as_lifecycle_issue(self):
        raw = bundle()
        labels = inventory(raw)
        query(raw, "pod_phase", {**labels, "phase": "Succeeded"}, times=[3600])
        usage(raw, labels)
        pod = first_pod(process_bundle(raw))
        self.assertIn("lifecycle-conflict", pod["usage_issues"])
        self.assertEqual(pod["phase"], "running")

    def test_sparse_nic_requests_are_zero_after_successful_namespace_query(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels)
        query(raw, "requests", {**labels, "container": "main", "resource": "cpu"}, 1)
        self.assertEqual(first_pod(process_bundle(raw))["requests"]["nic"], 0)
        query(raw, "requests", {**labels, "container": "router", "resource": "aro_openshift_io_swift_nic"}, 1)
        self.assertEqual(first_pod(process_bundle(raw))["requests"]["nic"], 1)

    def test_failed_or_other_namespace_request_query_does_not_imply_zero(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels)
        query(raw, "requests", {**labels, "container": "main", "resource": "cpu"}, 1)
        raw["queries"][-1]["error"] = "unavailable"
        self.assertIsNone(first_pod(process_bundle(raw))["requests"]["nic"])
        raw["queries"][-1].pop("error")
        raw["queries"][-1]["expression"] = 'kube_pod_container_resource_requests{namespace="another"}'
        self.assertIsNone(first_pod(process_bundle(raw))["requests"]["nic"])

    def test_empty_successful_namespace_request_query_means_zero_nics(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels)
        query(raw, "requests", {})
        raw["queries"][-1].update(series=[], expression='kube_pod_container_resource_requests{namespace="ocm-test-customer-one"}')
        self.assertEqual(first_pod(process_bundle(raw))["requests"]["nic"], 0)

    def test_request_availability_index_preserves_scope_and_inclusive_times(self):
        for changes, expected in (({}, None), ({"start": 7201}, 0), ({"end": 7199}, 0),
                                  ({"start": 7200}, None), ({"end": 7200}, None),
                                  ({"environment": "stage"}, 0), ({"region": "westus"}, 0),
                                  ({"cluster": "other"}, 0), ({"cluster": None}, None),
                                  ({"expression": 'requests{namespace="other"}'}, 0)):
            with self.subTest(changes=changes):
                raw = bundle()
                labels = inventory(raw)
                query(raw, "requests", {**labels, "container": "main", "resource": "cpu"})
                failed = copy.deepcopy(raw["queries"][-1])
                failed.update(error="unavailable", series=[],
                              expression='requests{namespace="ocm-test-customer-one"}')
                failed.update(changes)
                raw["queries"].append(failed)
                mc = next(mc for mc in process_bundle(raw)["management_clusters"] if mc["id"] == "prod/mc")
                self.assertEqual(mc["nodes"][0]["pods"][0]["requests"]["nic"], expected)

    def test_namespace_selector_parsed_once_for_many_pods(self):
        raw = bundle()
        for index in range(20):
            labels = {"namespace": "system", "pod": f"pod-{index}", "uid": str(index), "node": "node-a"}
            query(raw, "pod_info", labels)
            query(raw, "pod_phase", {**labels, "phase": "running"})
        query(raw, "requests", {})
        raw["queries"][-1].update(series=[], expression=r'requests{namespace="sys\u0074em"}')
        with patch.object(processor.re, "search", wraps=processor.re.search) as search:
            view = process_bundle(raw)
        self.assertEqual(search.call_count, 1)
        pods = view["management_clusters"][0]["nodes"][0]["pods"]
        self.assertEqual(len(pods), 20)
        self.assertTrue(all(pod["requests"]["nic"] == 0 for pod in pods))

    def test_request_availability_uses_each_pods_last_active_time(self):
        raw = bundle()
        inventory(raw, uid="old", times=[3600, 3900])
        inventory(raw, uid="new", times=[6900, 7200])
        query(raw, "requests", {})
        raw["queries"][-1].update(series=[], end=3900,
                                  expression='requests{namespace="ocm-test-customer-one"}')
        pods = process_bundle(raw)["management_clusters"][0]["nodes"][0]["pods"]
        self.assertEqual({p["id"].split("/")[-1]: p["requests"]["nic"] for p in pods},
                         {"old": 0, "new": None})

    def test_init_request_errors_remain_cluster_scoped_not_namespace_or_time_scoped(self):
        for cluster, expected in (("mc", None), (None, 0), ("other", 0)):
            with self.subTest(cluster=cluster):
                raw = bundle()
                labels = inventory(raw)
                query(raw, "requests", {**labels, "resource": "cpu", "container": "main"})
                query(raw, "init_requests", {}, cluster=cluster, times=[])
                raw["queries"][-1].update(error="unavailable", start=0, end=300, region="westus",
                                          expression='requests{namespace="other"}')
                mc = next(mc for mc in process_bundle(raw)["management_clusters"] if mc["id"] == "prod/mc")
                self.assertEqual(mc["nodes"][0]["pods"][0]["requests"]["nic"], expected)

    def test_terminal_index_preserves_optional_uid_matching(self):
        for terminal_uid, usage_uid, matches in (("old", "old", True), ("old", "", True),
                                                 ("", "new", True), ("old", "new", False)):
            with self.subTest(terminal_uid=terminal_uid, usage_uid=usage_uid):
                raw = bundle()
                labels = inventory(raw, uid=terminal_uid)
                raw["queries"][1]["series"][0]["labels"]["phase"] = "succeeded"
                usage(raw, {**labels, "uid": usage_uid})
                mc = process_bundle(raw)["management_clusters"][0]
                self.assertEqual(len(mc["nodes"][0]["pods"]), 0 if matches else 1)

    def test_hcp_index_joins_asof_namespace_and_prefers_live_identity(self):
        raw = bundle()
        snapshot(raw, uid="old")
        snapshot(raw, uid="old", timestamp=4500, event="Deleted")
        snapshot(raw, uid="new", timestamp=4500, event="Added", size="large")
        snapshot(raw, uid="unrelated", cluster="other", size="medium")
        inventory(raw, uid="early", times=[3600, 3900])
        inventory(raw, uid="late", times=[6900, 7200])
        pods = process_bundle(raw)["management_clusters"][0]["nodes"][0]["pods"]
        self.assertEqual([(p["hcp_id"], p["hcp_size"]) for p in pods],
                         [("old", "small"), ("new", "large")])

    def test_node_index_keeps_raw_metadata_distinct_from_expiring_gauges(self):
        raw = bundle()
        query(raw, "node_info", {"node": "retired"}, times=[3600, 3900])
        query(raw, "node_info", {"node": "current"})
        for node in ("retired", "current"):
            query(raw, "capacity", {"node": node, "resource": "cpu"}, [4, 8], [3300, 3600])
            query(raw, "capacity", {"node": node, "resource": "memory"}, 2**20, [3300])
            query(raw, "node_cpu", {"node": node}, 0, [3600])
            query(raw, "node_memory", {"node": node}, 2**20, [3600, 3900])
        mc = process_bundle(raw)["management_clusters"][0]
        current, retired = mc["nodes"]
        self.assertIsNone(current["capacity"]["cpu_mc"])
        self.assertEqual(retired["capacity"]["cpu_mc"], 8000)
        self.assertEqual(retired["capacity"]["mem_mib"], 1)
        self.assertIsNone(retired["node_usage"]["cpu_mc"])
        self.assertAlmostEqual(retired["node_usage"]["mem_mib"], 1 / 6)
        self.assertFalse(any("capacity cpu changed" in warning for warning in mc["warnings"]))

    def test_time_weighted_churn_and_instant_not_averaged(self):
        raw = bundle()
        snapshot(raw)
        labels = inventory(raw, times=range(3600, 4500, 300))
        usage(raw, labels, times=range(3600, 4500, 300))
        labels = inventory(raw, times=range(4500, 7201, 300), uid="pod-b", node="node-b")
        usage(raw, labels, times=range(4500, 7201, 300), cpu=0)
        raw["queries"][-2]["series"][0]["samples"][-1] = (7200, 100)
        view = process_bundle(raw)
        old, current = view["management_clusters"][0]["nodes"]
        self.assertFalse(old["current"])
        self.assertFalse(old["pods"][0]["current"])
        self.assertEqual(old["pods"][0]["usage"]["cpu_mc"], 500)
        self.assertEqual(old["pods"][0]["usage"]["mem_mib"], 0.25)
        self.assertEqual(old["pods"][0]["coverage"], {"cpu": 1, "memory": 1})
        self.assertTrue(current["current"])
        self.assertEqual(current["pods"][0]["usage"]["cpu_mc"], 0)
        self.assertEqual(old["pods"][0]["hcp_size"], "small")
        self.assertEqual(view["errors"], [])

    def test_gap_is_null_not_free_and_duplicates_not_summed(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels)
        raw["queries"].append(copy.deepcopy(raw["queries"][-2]))
        for q in raw["queries"]:
            if q["metric"] == "cpu":
                q["series"][0]["samples"][2] = (4200, None)
        view = process_bundle(raw)
        pod = first_pod(view)
        self.assertIsNone(pod["usage"]["cpu_mc"])
        self.assertAlmostEqual(pod["coverage"]["cpu"], 11 / 12)
        self.assertEqual(pod["usage"]["mem_mib"], 1)
        self.assertTrue(any("incomplete cpu coverage" in e for e in view["errors"]))

    def test_same_uid_scoped_by_environment_and_mc(self):
        raw = bundle()
        for env, mc, cpu in (("prod", "a", 1), ("prod", "b", 2), ("stage", "a", 3)):
            labels = inventory(raw, cluster=mc, environment=env)
            usage(raw, labels, cpu=cpu, cluster=mc, environment=env)
        view = process_bundle(raw)
        self.assertEqual([mc["id"] for mc in view["management_clusters"]], ["prod/a", "prod/b", "stage/a"])
        self.assertEqual([mc["nodes"][0]["pods"][0]["usage"]["cpu_mc"] for mc in view["management_clusters"]], [1000, 2000, 3000])

    def test_reused_pod_name_joins_cgroup_uid_and_node(self):
        raw = bundle()
        old = inventory(raw, uid="old", times=range(3600, 5401, 300))
        new = inventory(raw, uid="new", node="node-b", times=range(5400, 7201, 300))
        for labels, times, cpu in ((old, range(3600, 5401, 300), 1), (new, range(5400, 7201, 300), 2)):
            measured = {k: v for k, v in labels.items() if k != "uid"}
            measured["id"] = f"/kubepods/pod{labels['uid']}/container"
            usage(raw, measured, times=times, cpu=cpu)
        view = process_bundle(raw)
        pods = [n["pods"][0] for n in view["management_clusters"][0]["nodes"]]
        self.assertEqual(pods[0]["usage"]["cpu_mc"], 1000 * 7 / 12)
        self.assertEqual(pods[1]["usage"]["cpu_mc"], 1000)
        self.assertFalse(any("ambiguous pod UID" in e for e in view["errors"]))

    def test_ambiguous_reuse_is_not_double_counted(self):
        raw = bundle()
        labels = inventory(raw, uid="old")
        inventory(raw, uid="new")
        usage(raw, {k: v for k, v in labels.items() if k != "uid"})
        view = process_bundle(raw)
        self.assertTrue(any("ambiguous pod UID" in e for e in view["errors"]))
        self.assertTrue(all(p["usage"]["cpu_mc"] is None for p in view["management_clusters"][0]["nodes"][0]["pods"]))

    def test_transitions_baseline_repeats_and_common_suggestion(self):
        raw = bundle()
        raw.update(at=14400, start=10800)
        inventory(raw, times=range(10800, 14401, 300))
        inventory(raw, cluster="other", times=range(10800, 14401, 300))
        snapshot(raw, timestamp=0)
        snapshot(raw, timestamp=3000)
        snapshot(raw, size="medium", timestamp=12000)
        snapshot(raw, cluster="other", timestamp=0)
        snapshot(raw, cluster="other", size="large", timestamp=9000)
        snapshot(raw, cluster="other", size="large", timestamp=13500)
        view = process_bundle(raw)
        self.assertEqual(len(view["transitions"]), 2)
        self.assertTrue(any("transition" in e for e in view["errors"]))
        self.assertEqual(view["suggestion"]["at"], "1970-01-01T02:25:00Z")
        self.assertEqual(view["suggestion"]["start"], "1970-01-01T01:25:00Z")
        self.assertIn("--at", view["suggestion"]["command"])
        self.assertIn("--window 3600s --step 300s", view["suggestion"]["command"])

    def test_settling_before_start_excludes_window(self):
        raw = bundle()
        inventory(raw)
        snapshot(raw, timestamp=0)
        snapshot(raw, size="medium", timestamp=3300)
        view = process_bundle(raw)
        self.assertTrue(any("settling" in e for e in view["errors"]))
        self.assertIsNone(view["suggestion"])

    def test_missing_baseline_does_not_fabricate_stability(self):
        raw = bundle()
        inventory(raw)
        snapshot(raw, timestamp="1970-01-01T01:15:00Z")
        snapshot(raw, timestamp=4800, size="medium")
        view = process_bundle(raw)
        self.assertTrue(any("missing baseline" in e for e in view["errors"]))
        self.assertIsNone(view["suggestion"])

    def test_requests_only_observed_no_pause_or_synthetic_overhead(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels)
        query(raw, "cpu", {**labels, "container": "sidecar"}, 0.5)
        query(raw, "memory", {**labels, "container": "sidecar"}, 0)
        query(raw, "cpu", {**labels, "container": "POD"}, 999)
        for container, cpu in (("main", 0.1), ("sidecar", 0.2)):
            query(raw, "requests", {**labels, "container": container, "resource": "cpu"}, cpu)
        query(raw, "requests", {**labels, "container": "main", "resource": "memory"}, 4 * 2**20)
        query(raw, "requests", {**labels, "container": "sidecar", "resource": "memory"}, 0)
        query(raw, "requests", {**labels, "container": "main", "resource": "aro_openshift_io_swift_nic"}, 1)
        query(raw, "requests", {**labels, "container": "sidecar", "resource": "aro_openshift_io_swift_nic"}, 0)
        query(raw, "requests", {**labels, "container": "POD", "resource": "cpu"}, 500)
        view = process_bundle(raw)
        requests = first_pod(view)["requests"]
        self.assertEqual(first_pod(view)["usage"]["cpu_mc"], 2500)
        self.assertAlmostEqual(requests["cpu_mc"], 300)
        self.assertEqual(requests["mem_mib"], 4)
        self.assertEqual(requests["nic"], 1)
        query(raw, "init_requests", {**labels, "container": "init", "resource": "cpu"}, 0.8)
        query(raw, "pod_overhead", {**labels, "resource": "cpu"}, 0.05)
        self.assertAlmostEqual(first_pod(process_bundle(raw))["requests"]["cpu_mc"], 850)

    def test_node_gauges_independent_and_retired_capacity(self):
        raw = bundle()
        labels = inventory(raw, times=range(3600, 4500, 300))
        usage(raw, labels, times=range(3600, 4500, 300))
        query(raw, "node_cpu", {"node": "node-a"}, 8, range(3600, 4500, 300))
        query(raw, "node_memory", {"node": "node-a"}, 8 * 2**20, range(3600, 4500, 300))
        query(raw, "capacity", {"node": "node-a", "resource": "cpu"}, 16, range(3600, 4500, 300))
        query(raw, "allocatable", {"node": "node-a", "resource": "cpu"}, 12, range(3600, 4500, 300))
        query(raw, "node_labels", {"node": "node-a", "label_node_kubernetes_io_instance_type": "Standard_D16",
                                   "label_kubernetes_azure_com_agentpool": "workers", "label_topology_kubernetes_io_zone": "1"},
              times=range(3600, 4500, 300))
        node = process_bundle(raw)["management_clusters"][0]["nodes"][0]
        self.assertFalse(node["current"])
        self.assertEqual(node["capacity"]["cpu_mc"], 16000)
        self.assertEqual(node["allocatable"]["cpu_mc"], 12000)
        self.assertEqual(node["node_usage"], {"cpu_mc": 2000, "mem_mib": 2})
        self.assertEqual((node["sku"], node["pool"], node["zone"]), ("Standard_D16", "workers", "1"))

    def test_terminal_pod_and_deleted_hcp_retained(self):
        raw = bundle()
        snapshot(raw)
        snapshot(raw, timestamp=6900, event="Delete")
        labels = inventory(raw)
        usage(raw, labels, times=range(3600, 6900, 300))
        raw["queries"][1]["series"][0]["samples"] = [(t, 1 if t < 6900 else 0) for t in range(3600, 7201, 300)]
        query(raw, "pod_phase", {**labels, "phase": "Succeeded"}, times=[6900, 7200])
        view = process_bundle(raw)
        self.assertFalse(first_pod(view)["current"])
        self.assertEqual(first_pod(view)["hcp_id"], "hcp")
        self.assertEqual(view["management_clusters"][0]["hcps"][0]["state"], "deleted")

    def test_no_current_extrapolation_and_unplaced_pod(self):
        raw = bundle()
        labels = inventory(raw, node="", times=range(3600, 7200, 300))
        usage(raw, labels, times=range(3600, 7200, 300))
        view = process_bundle(raw)
        mc = view["management_clusters"][0]
        self.assertEqual(mc["nodes"], [])
        self.assertFalse(mc["unplaced_pods"][0]["current"])
        self.assertEqual(mc["unplaced_pods"][0]["usage"]["cpu_mc"], 2000)

    def test_errors_propagated_required_empty_and_json_safe(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels, cpu=float("nan"))
        query(raw, "capacity", {"node": "node-a"}, times=[], required=True)
        raw["queries"][-1]["error"] = "timeout"
        raw["errors"] = ["collector failure"]
        raw["warnings"] = ["collector warning"]
        view = process_bundle(raw)
        self.assertIn("collector failure", view["errors"])
        self.assertIn("collector warning", view["warnings"])
        self.assertTrue(any("empty required query capacity" in e for e in view["errors"]))
        self.assertTrue(any("timeout" in e for e in view["errors"]))
        json.dumps(view, allow_nan=False)

    def test_source_duplicates_and_optional_identity_not_double_counted(self):
        raw = bundle()
        snapshot(raw)
        labels = inventory(raw)
        usage(raw, labels)
        duplicate = copy.deepcopy(raw["queries"][-2])
        duplicate["series"][0]["labels"]["prometheus_replica"] = "replica-b"
        raw["queries"].append(duplicate)
        usage(raw, {k: v for k, v in labels.items() if k not in ("node", "uid")})
        view = process_bundle(raw)
        self.assertEqual(first_pod(view)["usage"]["cpu_mc"], 2000)
        self.assertEqual(view["errors"], [])

    def test_partial_container_gap_is_not_partial_pod_usage(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels)
        query(raw, "memory", {**labels, "container": "sidecar"}, 2**20)
        view = process_bundle(raw)
        self.assertIsNone(first_pod(view)["usage"]["cpu_mc"])
        self.assertEqual(first_pod(view)["coverage"]["cpu"], 0)
        self.assertEqual(first_pod(view)["usage"]["mem_mib"], 2)

    def test_node_without_inventory_and_usage_without_pod_metadata_visible(self):
        raw = bundle()
        query(raw, "capacity", {"node": "capacity-only", "resource": "cpu"}, 16)
        query(raw, "node_cpu", {"node": "gauge-only"}, 4)
        query(raw, "node_memory", {"node": "gauge-only"}, 2**20)
        query(raw, "cpu", {"namespace": "system", "pod": "not-a-node-name", "container": "main"}, 1)
        view = process_bundle(raw)
        mc = view["management_clusters"][0]
        self.assertEqual([n["name"] for n in mc["nodes"]], ["capacity-only", "gauge-only"])
        self.assertEqual(mc["nodes"][1]["node_usage"]["cpu_mc"], 4000)
        self.assertEqual(len(mc["unplaced_pods"]), 1)
        self.assertIsNone(mc["unplaced_pods"][0]["usage"]["cpu_mc"])
        self.assertTrue(any("no active pod_info" in w for w in mc["warnings"]))
        self.assertEqual(view["errors"], [])

    def test_required_inventory_empty_is_not_empty_success(self):
        raw = bundle()
        query(raw, "node_info", {}, times=[], required=True)
        self.assertTrue(any("empty required query node_info" in e for e in process_bundle(raw)["errors"]))

    def test_partial_step_and_samples_before_start_excluded(self):
        raw = bundle()
        raw.update(at=7250, window_seconds=3650)
        labels = inventory(raw, times=range(3600, 7201, 300))
        usage(raw, labels, times=range(3600, 7201, 300))
        query(raw, "cpu", {**labels, "container": "main"}, 999, [3300])
        view = process_bundle(raw)
        self.assertEqual(first_pod(view)["usage"]["cpu_mc"], 2000)
        self.assertFalse(first_pod(view)["current"])

    def test_owner_chain_and_terminal_requests(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels, times=range(3600, 6900, 300))
        raw["queries"][1]["series"][0]["samples"] = [(t, 1 if t < 6900 else 0) for t in range(3600, 7201, 300)]
        query(raw, "pod_phase", {**labels, "phase": "Failed"}, times=[6900, 7200])
        query(raw, "pod_owner", {**labels, "owner_kind": "replicaset", "owner_name": "api-rs"})
        query(raw, "replicaset_owner", {"namespace": labels["namespace"], "replicaset": "api-rs",
                                        "owner_kind": "Deployment", "owner_name": "api"})
        query(raw, "requests", {**labels, "container": "main", "resource": "cpu"}, 0.2, [3600])
        pod = first_pod(process_bundle(raw))
        self.assertEqual(pod["component"], "api")
        self.assertEqual(pod["requests"]["cpu_mc"], 200)

    def test_fixed_schema(self):
        raw = bundle()
        snapshot(raw)
        labels = inventory(raw)
        usage(raw, labels)
        view = process_bundle(raw)
        self.assertEqual(set(view), {"schema_version", "mode", "at", "start", "window_seconds", "step_seconds",
                                     "generated_at", "sources", "errors", "warnings", "transitions", "suggestion", "management_clusters"})
        mc = view["management_clusters"][0]
        self.assertEqual(set(mc), {"id", "name", "environment", "region", "hcps", "nodes", "unplaced_pods", "warnings"})
        self.assertEqual(set(mc["hcps"][0]), {"id", "name", "namespace", "control_plane_namespace", "size", "metadata_at", "state"})
        self.assertEqual(set(mc["nodes"][0]), {"id", "name", "sku", "pool", "zone", "current", "capacity", "allocatable", "node_usage", "pods",
                                              "metadata_at", "metadata_source", "metadata_uid"})
        self.assertEqual(set(first_pod(view)), {"id", "name", "namespace", "component", "hcp_id", "hcp_size", "current", "usage", "requests", "coverage", "phase", "usage_issues"})

    def test_retired_node_uses_last_resource_sample_and_reports_changes(self):
        raw = bundle()
        labels = inventory(raw, times=range(3600, 4500, 300))
        usage(raw, labels, times=range(3600, 4500, 300))
        query(raw, "capacity", {"node": "node-a", "resource": "cpu"}, [8, 16], [3600, 3900])
        mc = process_bundle(raw)["management_clusters"][0]
        self.assertEqual(mc["nodes"][0]["capacity"]["cpu_mc"], 16000)
        self.assertTrue(any("capacity cpu changed" in w for w in mc["warnings"]))

    def test_inventory_gap_is_reported_even_if_usage_is_also_missing(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels)
        for q in raw["queries"]:
            if q["metric"] != "node_info":
                q["series"][0]["samples"] = [(t, v) for t, v in q["series"][0]["samples"] if t != 4200]
        view = process_bundle(raw)
        self.assertTrue(any("pod_info inventory gap" in e for e in view["errors"]))

    def test_required_unscoped_empty_query(self):
        raw = bundle()
        query(raw, "node_info", {}, times=[], required=True)
        del raw["queries"][0]["cluster"]
        raw["queries"][0]["series"] = []
        self.assertTrue(process_bundle(raw)["errors"])

    def test_request_only_pod_is_retained_without_claiming_liveness(self):
        raw = bundle()
        query(raw, "requests", {"namespace": "system", "pod": "orphan", "container": "main", "resource": "cpu"}, 0.1)
        view = process_bundle(raw)
        pod = view["management_clusters"][0]["unplaced_pods"][0]
        self.assertFalse(pod["current"])
        self.assertIsNone(pod["usage"]["cpu_mc"])
        self.assertEqual(pod["requests"]["cpu_mc"], 100)
        self.assertEqual(pod["coverage"], {"cpu": 0, "memory": 0})
        self.assertIn("unknown-lifetime", pod["usage_issues"])
        self.assertTrue(any("unknown lifetime" in w for w in view["management_clusters"][0]["warnings"]))
        self.assertFalse(any("0/0" in w for w in view["management_clusters"][0]["warnings"]))
        self.assertEqual(view["errors"], [])

    def test_no_suggestion_outside_24_hour_search(self):
        raw = bundle()
        raw.update(at=100000, start=96400, search_start=0)
        inventory(raw, times=[96400, 100000])
        snapshot(raw, timestamp=0)
        snapshot(raw, timestamp=14000, size="medium")
        snapshot(raw, timestamp=97000, size="large")
        raw["settle_seconds"] = 86400
        self.assertIsNone(process_bundle(raw)["suggestion"])

    def test_lowercase_completed_jobs_are_dropped(self):
        for phase in ("succeeded", "failed", "Succeeded", "Failed"):
            with self.subTest(phase=phase):
                raw = bundle()
                labels = inventory(raw)
                raw["queries"] = [q for q in raw["queries"] if q["metric"] != "pod_phase"]
                query(raw, "pod_phase", {**labels, "phase": phase})
                query(raw, "pod_phase", {**labels, "phase": "running"}, 0)
                query(raw, "requests", {**labels, "resource": "cpu", "container": "main"}, 1)
                usage(raw, labels, cpu=None, memory=None)
                view = process_bundle(raw)
                self.assertEqual(view["management_clusters"][0]["nodes"][0]["pods"], [])
                self.assertEqual(view["errors"], [])

    def test_missing_phase_never_establishes_current_or_drops_pod(self):
        for phase in (None, "unknown"):
            with self.subTest(phase=phase):
                raw = bundle()
                snapshot(raw)
                labels = inventory(raw)
                raw["queries"] = [q for q in raw["queries"] if q["metric"] != "pod_phase"]
                query(raw, "pod_phase", {**labels, "phase": phase or "running"},
                      times=[] if phase is None else None, required=True)
                usage(raw, labels)
                view = process_bundle(raw)
                self.assertFalse(first_pod(view)["current"])
                self.assertEqual(first_pod(view)["usage"]["cpu_mc"], 2000)
                self.assertTrue(any("pod_phase coverage missing or unknown" in w
                                    for w in view["management_clusters"][0]["warnings"]))
                self.assertEqual(bool(view["errors"]), phase is None)

    def test_terminal_end_does_not_hide_unknown_or_running_history(self):
        raw = bundle()
        snapshot(raw)
        labels = inventory(raw)
        raw["queries"] = [q for q in raw["queries"] if q["metric"] != "pod_phase"]
        query(raw, "pod_phase", {**labels, "phase": "succeeded"}, times=range(5400, 7201, 300))
        view = process_bundle(raw)
        self.assertFalse(first_pod(view)["current"])
        self.assertIsNone(first_pod(view)["usage"]["cpu_mc"])
        self.assertTrue(any("0/1800 active seconds" in w for w in view["management_clusters"][0]["warnings"]))
        self.assertEqual(view["errors"], [])

    def test_rectangular_restart_series_nulls_are_absence(self):
        raw = bundle()
        snapshot(raw)
        uid = "0e77189a-fa8c-4857-89fc-6e190480f57a"
        labels = inventory(raw, uid=uid)
        measured = {k: v for k, v in labels.items() if k not in ("uid", "node")}
        for suffix, cpu in (("old", [1] * 6 + [None] * 7), ("new", [None] * 6 + [2] * 7)):
            cgroup = f"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod{uid.replace('-', '_')}.slice/cri-containerd-{suffix}.scope"
            usage(raw, {**measured, "id": cgroup}, cpu=cpu, memory=[None if v is None else 2**20 for v in cpu])
        view = process_bundle(raw)
        self.assertEqual(first_pod(view)["usage"], {"cpu_mc": 1500, "mem_mib": 1})
        self.assertEqual(view["errors"], [])

    def test_cgroup_uid_prevents_orphan_join_to_reused_name(self):
        raw = bundle()
        labels = inventory(raw, uid="new")
        measured = {k: v for k, v in labels.items() if k != "uid"}
        usage(raw, {**measured, "id": "/kubepods/podold/container"})
        view = process_bundle(raw)
        pods = view["management_clusters"][0]["nodes"][0]["pods"]
        self.assertEqual(len(pods), 2)
        self.assertTrue(all(p["usage"]["cpu_mc"] is None for p in pods))
        self.assertTrue(any("/old: cpu has no active pod_info" in w for w in view["management_clusters"][0]["warnings"]))
        self.assertTrue(any("/new: incomplete cpu coverage" in e for e in view["errors"]))

    def test_node_instances_resolve_by_name_and_ip_without_collapsing(self):
        raw = bundle()
        query(raw, "node_info", {"node": "a", "internal_ip": "10.0.0.1"})
        query(raw, "node_info", {"node": "b", "internal_ip": "10.0.0.2"})
        for metric in ("node_cpu", "node_memory"):
            query(raw, metric, {"instance": "a"}, 1)
            query(raw, metric, {"instance": "10.0.0.2:9100"}, 2)
        view = process_bundle(raw)
        nodes = view["management_clusters"][0]["nodes"]
        self.assertEqual([n["node_usage"]["cpu_mc"] for n in nodes], [1000, 2000])
        self.assertFalse(any("conflicting" in e for e in view["errors"]))

    def test_unmapped_node_instance_is_error_not_guessed_node(self):
        raw = bundle()
        query(raw, "node_info", {"node": "a"})
        query(raw, "node_cpu", {"instance": "10.0.0.1:9100"}, 2)
        view = process_bundle(raw)
        self.assertIsNone(view["management_clusters"][0]["nodes"][0]["node_usage"]["cpu_mc"])
        self.assertTrue(any("no unique node mapping" in e for e in view["errors"]))

    def test_sparse_requests_are_unknown_not_zero_or_partial_total(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels)
        query(raw, "cpu", {**labels, "container": "sidecar"}, 0)
        query(raw, "memory", {**labels, "container": "sidecar"}, 0)
        query(raw, "requests", {**labels, "container": "main", "resource": "cpu"}, 0.2)
        query(raw, "init_requests", {**labels, "container": "init", "resource": "memory"}, 2**20)
        pod = first_pod(process_bundle(raw))
        self.assertEqual(pod["requests"], {"cpu_mc": None, "mem_mib": None, "nic": 0})
        query(raw, "requests", {**labels, "container": "sidecar", "resource": "cpu"}, 0)
        self.assertEqual(first_pod(process_bundle(raw))["requests"]["cpu_mc"], 200)

    def test_no_hcp_pods_does_not_claim_missing_stability(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels)
        for q in raw["queries"]:
            for s in q["series"]:
                if "namespace" in s["labels"]:
                    s["labels"]["namespace"] = "kube-system"
        view = process_bundle(raw)
        self.assertFalse(any("stability" in w for w in view["management_clusters"][0]["warnings"]))
        self.assertEqual(view["errors"], [])

    def test_unknown_hcp_history_prevents_common_stable_suggestion(self):
        raw = bundle()
        snapshot(raw, cluster="other")
        snapshot(raw, cluster="other", timestamp=6000, size="large")
        labels = inventory(raw)
        usage(raw, labels)
        view = process_bundle(raw)
        self.assertIsNone(view["suggestion"])
        self.assertTrue(any("size stability unknown" in e for e in view["errors"]))

    def test_pending_is_not_proof_of_zero_usage(self):
        raw = bundle()
        snapshot(raw)
        inventory(raw)
        raw["queries"][1]["series"][0]["labels"]["phase"] = "pending"
        view = process_bundle(raw)
        self.assertTrue(first_pod(view)["current"])
        self.assertIsNone(first_pod(view)["usage"]["cpu_mc"])
        self.assertTrue(any("0/3600 active seconds" in w for w in view["management_clusters"][0]["warnings"]))
        self.assertEqual(view["errors"], [])

    def test_live_container_null_sample_remains_gap_with_restart_padding(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, {**labels, "id": "/kubepods/podpod-a/old"}, cpu=None, memory=None)
        usage(raw, {**labels, "id": "/kubepods/podpod-a/new"}, cpu=[1, None] + [1] * 11)
        view = process_bundle(raw)
        self.assertIsNone(first_pod(view)["usage"]["cpu_mc"])
        self.assertAlmostEqual(first_pod(view)["coverage"]["cpu"], 11 / 12)

    def test_node_ip_mapping_is_timestamp_scoped(self):
        raw = bundle()
        query(raw, "node_info", {"node": "old", "internal_ip": "10.0.0.1"}, times=range(3600, 5400, 300))
        query(raw, "node_info", {"node": "new", "internal_ip": "10.0.0.1"}, times=range(5400, 7201, 300))
        query(raw, "node_cpu", {"instance": "10.0.0.1:9100"}, 2)
        nodes = process_bundle(raw)["management_clusters"][0]["nodes"]
        self.assertEqual([n["node_usage"]["cpu_mc"] for n in nodes], [1000, 1000])

    def test_terminal_info_does_not_hide_unmatched_usage_elsewhere_in_window(self):
        raw = bundle()
        snapshot(raw)
        labels = inventory(raw, times=[7200])
        raw["queries"] = [q for q in raw["queries"] if q["metric"] != "pod_phase"]
        query(raw, "pod_phase", {**labels, "phase": "succeeded"}, times=[7200])
        usage(raw, labels, times=[3600])
        view = process_bundle(raw)
        self.assertFalse(first_pod(view)["current"])
        self.assertIsNone(first_pod(view)["usage"]["cpu_mc"])
        self.assertTrue(any("no active pod_info" in w for w in view["management_clusters"][0]["warnings"]))
        self.assertEqual(view["errors"], [])

    def test_stale_baseline_and_latest_metadata_are_errors(self):
        for timestamp, expected in ((-86401, "stale baseline"), (3600 - 86400, "stale latest")):
            with self.subTest(timestamp=timestamp):
                raw = bundle()
                snapshot(raw, timestamp=timestamp)
                inventory(raw)
                view = process_bundle(raw)
                self.assertTrue(any(expected in e for e in view["errors"]))
                self.assertEqual(view["management_clusters"][0]["hcps"][0]["size"], "small")
                self.assertIsNone(view["suggestion"])

    def test_fresh_latest_does_not_repair_stale_baseline(self):
        raw = bundle()
        inventory(raw)
        snapshot(raw, timestamp=-86401)
        snapshot(raw, timestamp=6000)
        self.assertTrue(any("stale baseline" in e for e in process_bundle(raw)["errors"]))

    def test_metadata_age_override_and_exact_boundary(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels)
        snapshot(raw, timestamp=0)
        raw["metadata_max_age_seconds"] = 7200
        self.assertEqual(process_bundle(raw)["errors"], [])
        raw["metadata_max_age_seconds"] = 7199
        self.assertTrue(any("stale latest" in e for e in process_bundle(raw)["errors"]))
        for invalid in (0, -1, float("nan"), float("inf")):
            raw["metadata_max_age_seconds"] = invalid
            with self.assertRaises(ValueError):
                process_bundle(raw)

    def test_stale_metadata_blocks_suggested_window(self):
        raw = bundle()
        raw.update(at=100000, start=96400, search_start=13600)
        inventory(raw, times=range(96400, 100001, 300))
        snapshot(raw, timestamp=-86400)
        snapshot(raw, timestamp=98000, size="large")
        view = process_bundle(raw)
        self.assertTrue(any("stale baseline" in e for e in view["errors"]))
        self.assertIsNone(view["suggestion"])

    def test_deleted_history_does_not_expire_or_require_precreation_baseline(self):
        raw = bundle()
        snapshot(raw, timestamp=-100000, event="Deleted")
        snapshot(raw, uid="temporary", timestamp=4200, event="Added")
        snapshot(raw, uid="temporary", timestamp=4800, event="Deleted")
        view = process_bundle(raw)
        self.assertEqual(view["errors"], [])
        self.assertTrue(all(h["state"] == "deleted" for h in view["management_clusters"][0]["hcps"]))

    def test_deleted_in_window_retains_historical_pod_metadata(self):
        raw = bundle()
        snapshot(raw, timestamp=3600, event="Added")
        snapshot(raw, timestamp=4500, event="Deleted")
        raw["snapshots"][-1]["object"] = {}
        labels = inventory(raw, times=range(3600, 4500, 300))
        usage(raw, labels, times=range(3600, 4500, 300))
        view = process_bundle(raw)
        self.assertEqual(view["errors"], [])
        self.assertEqual(first_pod(view)["hcp_id"], "hcp")
        self.assertEqual(first_pod(view)["usage"]["cpu_mc"], 500)
        self.assertEqual(view["management_clusters"][0]["hcps"][0]["state"], "deleted")

    def test_phase_conflict_warns_when_activity_is_unambiguous(self):
        raw = bundle()
        snapshot(raw)
        labels = inventory(raw)
        usage(raw, labels)
        query(raw, "pod_phase", {**labels, "phase": "Running", "prometheus_replica": "other"}, 0)
        query(raw, "pod_phase", {**labels, "phase": "Pending", "prometheus_replica": "other"}, 1)
        view = process_bundle(raw)
        self.assertEqual(view["errors"], [])
        self.assertTrue(first_pod(view)["current"])
        self.assertTrue(any("same activity classification" in w for w in view["management_clusters"][0]["warnings"]))

    def test_conflicting_terminal_and_running_never_drops_pod(self):
        raw = bundle()
        snapshot(raw)
        labels = inventory(raw)
        usage(raw, labels)
        query(raw, "pod_phase", {**labels, "phase": "succeeded", "prometheus_replica": "other"})
        view = process_bundle(raw)
        self.assertFalse(first_pod(view)["current"])
        self.assertEqual(first_pod(view)["phase"], "unknown")
        self.assertEqual(first_pod(view)["usage"]["cpu_mc"], 2000)
        self.assertTrue(any("activity uncertain" in w for w in view["management_clusters"][0]["warnings"]))
        self.assertEqual(view["errors"], [])

    def test_deleted_before_candidate_does_not_block_suggestion(self):
        raw = bundle()
        inventory(raw)
        snapshot(raw, timestamp=-100000, event="Deleted", uid="old")
        snapshot(raw, timestamp=0)
        snapshot(raw, timestamp=6000, size="large")
        view = process_bundle(raw)
        self.assertIsNotNone(view["suggestion"])

    def test_fresh_endpoints_do_not_hide_midwindow_metadata_expiry(self):
        raw = bundle()
        inventory(raw)
        raw["metadata_max_age_seconds"] = 900
        snapshot(raw, timestamp=3600)
        snapshot(raw, timestamp=6900)
        view = process_bundle(raw)
        self.assertTrue(any("stale metadata within window" in e for e in view["errors"]))

    def test_deleted_in_window_still_needs_fresh_active_baseline(self):
        raw = bundle()
        inventory(raw, times=[3600, 3900])
        snapshot(raw, timestamp=-86401)
        snapshot(raw, timestamp=4500, event="Deleted")
        view = process_bundle(raw)
        self.assertTrue(any("stale baseline" in e for e in view["errors"]))

    def test_phase_zero_one_without_other_phase_is_not_safe_terminal(self):
        raw = bundle()
        snapshot(raw)
        labels = inventory(raw)
        raw["queries"] = [q for q in raw["queries"] if q["metric"] != "pod_phase"]
        query(raw, "pod_phase", {**labels, "phase": "succeeded"})
        query(raw, "pod_phase", {**labels, "phase": "succeeded", "prometheus_replica": "other"}, 0)
        view = process_bundle(raw)
        self.assertFalse(first_pod(view)["current"])
        self.assertIsNone(first_pod(view)["usage"]["cpu_mc"])
        self.assertTrue(any("activity uncertain" in w for w in view["management_clusters"][0]["warnings"]))
        self.assertEqual(view["errors"], [])

    def test_long_deleted_histories_do_not_swell_view_or_block_stability(self):
        raw = bundle()
        inventory(raw)
        raw["sources"][0]["discovery"] = [{"large": "endpoint metadata"}]
        snapshot(raw, timestamp=0, uid="live")
        snapshot(raw, timestamp=6000, uid="live", size="large")
        for uid in ("old-a", "old-b"):
            snapshot(raw, timestamp=-100000, uid=uid, size=None)
            snapshot(raw, timestamp=-90000, uid=uid, event="Deleted", size=None)
            snapshot(raw, timestamp=1000, uid=uid, event="Deleted", size="large")
        raw["snapshots"].reverse()
        original = copy.deepcopy(raw)
        view = process_bundle(raw)
        self.assertEqual(raw, original)
        self.assertEqual([h["id"] for h in view["management_clusters"][0]["hcps"]], ["live"])
        self.assertEqual([t["hcp_id"] for t in view["transitions"]], ["live"])
        self.assertIsNotNone(view["suggestion"])
        self.assertFalse(any("old-" in e for e in view["errors"]))
        self.assertNotIn("discovery", view["sources"][0])

    def test_ended_search_history_not_in_view_or_requested_settling(self):
        raw = bundle()
        labels = inventory(raw)
        usage(raw, labels)
        snapshot(raw, timestamp=0, uid="live")
        snapshot(raw, timestamp=0, uid="ended")
        snapshot(raw, timestamp=3300, uid="ended", size="large")
        snapshot(raw, timestamp=3500, uid="ended", size="large", event="Deleted")
        view = process_bundle(raw)
        self.assertEqual(view["errors"], [])
        self.assertEqual([h["id"] for h in view["management_clusters"][0]["hcps"]], ["live"])
        self.assertEqual(view["transitions"], [])
        snapshot(raw, timestamp=6000, uid="live", size="large")
        # An HCP that contributes no pods in this window cannot block candidates.
        self.assertIsNotNone(process_bundle(raw)["suggestion"])

    def test_post_search_baseline_nondeleted_observation_keeps_identity(self):
        raw = bundle()
        snapshot(raw, timestamp=-100000, event="Deleted")
        snapshot(raw, timestamp=3000, event="Added")
        view = process_bundle(raw)
        self.assertEqual(len(view["management_clusters"][0]["hcps"]), 1)
        self.assertEqual(view["management_clusters"][0]["hcps"][0]["state"], "active")
        self.assertEqual(view["errors"], [])

    def test_node_snapshot_enrichment_scoped_without_discovery_or_state(self):
        raw = bundle()
        node_snapshot(raw, cluster="undiscovered")
        node_snapshot(raw, name="undiscovered")
        self.assertEqual(process_bundle(raw)["management_clusters"], [])
        for env, mc, sku in (("prod", "a", "A"), ("prod", "b", "B"), ("stage", "a", "C")):
            query(raw, "node_info", {"node": "node-a"}, cluster=mc, environment=env)
            node_snapshot(raw, cluster=mc, environment=env, sku=sku)
        wrong_region = node_snapshot(raw, cluster="a", timestamp=7100, sku="wrong-region")
        wrong_region["region"] = "westus"
        original = copy.deepcopy(raw)
        view = process_bundle(raw)
        self.assertEqual(raw, original)
        self.assertEqual([mc["id"] for mc in view["management_clusters"]], ["prod/a", "prod/b", "stage/a"])
        self.assertEqual([mc["nodes"][0]["sku"] for mc in view["management_clusters"]], ["A", "B", "C"])
        for mc in view["management_clusters"]:
            self.assertEqual(len(mc["nodes"]), 1)
            node = mc["nodes"][0]
            self.assertTrue(node["current"])
            self.assertEqual(node["node_usage"], {"cpu_mc": None, "mem_mib": None})
            self.assertEqual((node["pool"], node["zone"]), ("workers", "eastus-1"))
            self.assertEqual((node["metadata_source"], node["metadata_uid"]), ("kusto", "node-uid"))
            self.assertFalse(any("metadata missing" in w for w in mc["warnings"]))

    def test_node_snapshots_asof_and_retired_last_live_not_late_gauges(self):
        for current, expected, timestamp in ((True, "new", "1970-01-01T02:00:00Z"),
                                              (False, "old", "1970-01-01T01:00:00Z")):
            with self.subTest(current=current):
                raw = bundle()
                query(raw, "node_info", {"node": "node-a"}, times=None if current else [3600, 3900, 4200])
                query(raw, "node_cpu", {"node": "node-a"}, 1, times=[6900])
                node_snapshot(raw, timestamp=7201, sku="future")
                node_snapshot(raw, timestamp=7200, sku="new")
                node_snapshot(raw, timestamp=4500, sku="after-retirement")
                node_snapshot(raw, timestamp=3600, sku="old")
                node_snapshot(raw, timestamp=7300 if current else 4500, event="Delete")
                node = process_bundle(raw)["management_clusters"][0]["nodes"][0]
                self.assertEqual(node["sku"], expected)
                self.assertEqual(node["metadata_at"], timestamp)
                self.assertEqual(node["current"], current)

    def test_node_snapshot_delete_suppresses_same_uid_before_latest(self):
        for deleted_at, expected in ((7201, "delayed"), (7200, None), (4000, None)):
            with self.subTest(deleted_at=deleted_at):
                raw = bundle()
                query(raw, "node_info", {"node": "node-a"})
                node_snapshot(raw, timestamp=3600)
                deleted = node_snapshot(raw, timestamp=deleted_at, event="Deleted")
                deleted["object"] = {}
                node_snapshot(raw, timestamp=7100, sku="delayed")
                raw["node_snapshots"].reverse()
                node = process_bundle(raw)["management_clusters"][0]["nodes"][0]
                self.assertTrue(node["current"])
                self.assertEqual(node["sku"], expected)
                if expected is None:
                    self.assertIsNone(node["metadata_source"])

    def test_node_snapshot_deleted_uid_does_not_hide_replacement(self):
        raw = bundle()
        query(raw, "node_info", {"node": "node-a", "provider_id": "azure:///new"}, times=[6900, 7200])
        node_snapshot(raw, timestamp=6000, event="Delete")
        node_snapshot(raw, timestamp=7100, sku="delayed-old")
        replacement = node_snapshot(raw, timestamp=6900, uid="new", sku="replacement",
                                    created="1970-01-01T01:45:00Z")
        replacement["object"]["spec"]["providerID"] = "azure:///new"
        node = process_bundle(raw)["management_clusters"][0]["nodes"][0]
        self.assertEqual((node["sku"], node["metadata_uid"]), ("replacement", "new"))

    def test_node_snapshot_provider_and_creation_guard_incarnation(self):
        for mismatch in ("provider", "creation", "invalid-creation"):
            with self.subTest(mismatch=mismatch):
                raw = bundle()
                query(raw, "node_info", {"node": "node-a", "provider_id": "azure:///node"})
                node_snapshot(raw, timestamp=3600, sku="matching")
                row = node_snapshot(raw, timestamp=7100, uid="other", sku="wrong")
                if mismatch == "provider":
                    row["object"]["spec"]["providerID"] = "azure:///other"
                else:
                    row["object"]["metadata"]["creationTimestamp"] = (
                        "1970-01-01T01:30:00Z" if mismatch == "creation" else "invalid")
                mc = process_bundle(raw)["management_clusters"][0]
                self.assertEqual(mc["nodes"][0]["sku"], "matching")
                self.assertTrue(any("ignored" in w for w in mc["warnings"]))

    def test_node_snapshot_azure_provider_id_case_matches(self):
        provider = ("azure:///subscriptions/00000000-0000-0000-0000-000000000001/"
                    "resourceGroups/example-management-cluster/"
                    "providers/Microsoft.Compute/virtualMachineScaleSets/"
                    "aks-example-12345678-vmss/virtualMachines/2")
        for snapshot_provider, matches in ((provider, True), (provider[:-1] + "3", False),
                                            (None, False), ("aws:///Zone/Instance", False)):
            with self.subTest(provider=snapshot_provider):
                raw = bundle()
                query(raw, "node_info", {"node": "node-a", "provider_id": provider.lower()})
                row = node_snapshot(raw, timestamp=7100, sku="Standard_E32ds_v5")
                row["object"]["spec"]["providerID"] = snapshot_provider
                original = copy.deepcopy(raw)
                mc = process_bundle(raw)["management_clusters"][0]
                node = mc["nodes"][0]
                self.assertEqual(raw, original)
                self.assertEqual(node["sku"], "Standard_E32ds_v5" if matches else None)
                self.assertEqual(node["metadata_uid"], "node-uid" if matches else None)
                self.assertEqual(any("provider_id disagreement" in w for w in mc["warnings"]), not matches)

    def test_provider_case_does_not_change_azure_lifetime_or_other_provider_identity(self):
        for provider, matches in (("azure:///subscriptions/sub/resourceGroups/Group", True),
                                  ("aws:///Zone/Instance", False)):
            with self.subTest(provider=provider):
                raw = bundle()
                query(raw, "node_info", {"node": "node-a", "provider_id": provider}, times=[3600, 3900])
                query(raw, "node_info", {"node": "node-a", "provider_id": provider.lower()},
                      times=range(4200, 7201, 300))
                row = node_snapshot(raw)
                row["object"]["spec"]["providerID"] = provider
                node = process_bundle(raw)["management_clusters"][0]["nodes"][0]
                self.assertEqual(node["metadata_uid"], "node-uid" if matches else None)
                if matches:
                    # A casing change must not permit a creation after first observation.
                    row["object"]["metadata"]["creationTimestamp"] = "1970-01-01T01:10:00Z"
                    row["timestamp"] = 7100
                    mc = process_bundle(raw)["management_clusters"][0]
                    self.assertIsNone(mc["nodes"][0]["metadata_uid"])
                    self.assertTrue(any("creationTimestamp disagrees" in w for w in mc["warnings"]))

    def test_node_snapshot_ambiguous_incarnation_or_conflicting_latest_unknown(self):
        for uid in ("other", "node-uid"):
            with self.subTest(uid=uid):
                raw = bundle()
                query(raw, "node_info", {"node": "node-a"})
                node_snapshot(raw)
                node_snapshot(raw, uid=uid, sku="conflict")
                mc = process_bundle(raw)["management_clusters"][0]
                self.assertIsNone(mc["nodes"][0]["sku"])
                self.assertTrue(any("ignored" in w for w in mc["warnings"]))

    def test_node_snapshot_stale_boundary_and_retired_reference(self):
        for current, age, stale in ((True, 86400, False), (True, 86401, True), (False, 86400, False)):
            with self.subTest(current=current, age=age):
                raw = bundle()
                query(raw, "node_info", {"node": "node-a"}, times=[7200] if current else [4200])
                node_snapshot(raw, timestamp=(7200 if current else 4200) - age,
                              created="1969-12-01T00:00:00Z")
                mc = process_bundle(raw)["management_clusters"][0]
                node = mc["nodes"][0]
                self.assertEqual(node["sku"], None if stale else "Standard_D16")
                self.assertEqual(node["metadata_source"], None if stale else "kusto")
                self.assertEqual(any("stale Node snapshot" in w for w in mc["warnings"]), stale)
                if stale:
                    self.assertIsNone(node["capacity"]["cpu_mc"])
                    self.assertTrue(any("sku metadata missing" in w for w in mc["warnings"]))

    def test_node_snapshot_quantities_and_metric_precedence(self):
        raw = bundle()
        query(raw, "node_info", {"node": "node-a"})
        query(raw, "node_labels", {"node": "node-a", "label_node_kubernetes_io_instance_type": "metric-sku"})
        query(raw, "capacity", {"node": "node-a", "resource": "cpu"}, 8)
        query(raw, "allocatable", {"node": "node-a", "resource": "aro_openshift_io_swift_nic"}, 0)
        node_snapshot(raw)
        mc = process_bundle(raw)["management_clusters"][0]
        node = mc["nodes"][0]
        self.assertEqual(node["sku"], "metric-sku")
        self.assertEqual(node["capacity"], {"cpu_mc": 8000, "mem_mib": 65536, "pods": 110, "nic": 16})
        self.assertEqual(node["allocatable"], {"cpu_mc": 15500, "mem_mib": 61440, "pods": 100, "nic": 0})
        self.assertEqual(sum("keeping Grafana" in w for w in mc["warnings"]), 3)
        self.assertFalse(any("metadata missing" in w for w in mc["warnings"]))
        self.assertEqual(node["node_usage"], {"cpu_mc": None, "mem_mib": None})

    def test_node_snapshot_incarnation_after_inventory_gap_or_provider_change(self):
        for gap in (True, False):
            with self.subTest(gap=gap):
                raw = bundle()
                query(raw, "node_info", {"node": "node-a", "provider_id": "azure:///old"}, times=[3600, 3900])
                query(raw, "node_info", {"node": "node-a", "provider_id": "azure:///node"},
                      times=[6900, 7200] if gap else range(4200, 7201, 300))
                old = node_snapshot(raw, uid="old")
                old["object"]["spec"]["providerID"] = "azure:///old"
                node_snapshot(raw, timestamp=6900, created="1970-01-01T01:10:00Z")
                node = process_bundle(raw)["management_clusters"][0]["nodes"][0]
                self.assertEqual(node["metadata_uid"], "node-uid")

    def test_stale_snapshot_does_not_remove_metrics_or_current_state(self):
        raw = bundle()
        query(raw, "node_info", {"node": "node-a", "label_node_kubernetes_io_instance_type": "metric-sku"})
        query(raw, "capacity", {"node": "node-a", "resource": "cpu"}, 8)
        node_snapshot(raw, timestamp=-86400, created="1969-12-01T00:00:00Z")
        node = process_bundle(raw)["management_clusters"][0]["nodes"][0]
        self.assertEqual(node["sku"], "metric-sku")
        self.assertEqual(node["capacity"]["cpu_mc"], 8000)
        self.assertTrue(node["current"])
        self.assertIsNone(node["metadata_source"])

    def test_kubernetes_quantity_parser(self):
        for quantity, expected in {"4": 4, "250m": .25, "250000u": .25, "250000000n": .25,
                                   "1Ki": 1024, "1Mi": 2**20, "1.5Gi": 1.5 * 2**30,
                                   "1Ti": 2**40, "1Pi": 2**50, "1Ei": 2**60,
                                   "1k": 1000, "1M": 1e6, "1G": 1e9, "1T": 1e12,
                                   "1P": 1e15, "1E": 1e18, "129e6": 129e6, "1E+3": 1000,
                                   "1e-3": .001, ".5": .5, "+1.": 1, "0": 0}.items():
            with self.subTest(quantity=quantity):
                self.assertAlmostEqual(_quantity(quantity), expected)
        for quantity in (None, "", "NaN", "inf", "1e9999", "1GiB", "1K", "1e3Mi", "-1", " 1", True):
            with self.subTest(quantity=quantity):
                self.assertIsNone(_quantity(quantity))

    def test_node_snapshot_beta_labels_and_invalid_quantities(self):
        raw = bundle()
        query(raw, "node_info", {"node": "node-a"})
        row = node_snapshot(raw)
        row["object"]["metadata"]["labels"] = {"beta.kubernetes.io/instance-type": "beta",
                                               "agentpool": "legacy", "failure-domain.beta.kubernetes.io/zone": "zone"}
        row["object"]["status"]["capacity"]["cpu"] = "bogus"
        row["object"]["status"]["allocatable"]["memory"] = "129e6"
        mc = process_bundle(raw)["management_clusters"][0]
        node = mc["nodes"][0]
        self.assertEqual((node["sku"], node["pool"], node["zone"]), ("beta", "legacy", "zone"))
        self.assertIsNone(node["capacity"]["cpu_mc"])
        self.assertEqual(node["allocatable"]["mem_mib"], 129e6 / 2**20)
        self.assertTrue(any("invalid Node snapshot capacity cpu quantity" in w for w in mc["warnings"]))
        json.dumps(process_bundle(raw), allow_nan=False)


if __name__ == "__main__":
    unittest.main()

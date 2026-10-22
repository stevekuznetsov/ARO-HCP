"""Deterministic offline evidence fixtures; no network or cluster dependencies."""

import copy
from decimal import Decimal
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

import offline_budgets as b


def fixture():
    container = {"name": "worker", "image": "image@sha256:final",
                 "resources": {"requests": {"cpu": "10m", "memory": "64Mi"}},
                 "env": [{"name": "GOMEMLIMIT", "value": "53687091"}]}
    workload = {"kind": "Deployment", "metadata": {"name": "exact", "namespace": "ns", "uid": "deployment"},
                "spec": {"replicas": 2, "selector": {"matchLabels": {"app": "exact"}},
                         "template": {"metadata": {"annotations": {b.POLICY_HASH: "hash"}},
                                      "spec": {"containers": [container], "initContainers": [
                                          {"name": "init", "image": "init-image", "resources": {}}]}}}}
    rs = {"kind": "ReplicaSet", "metadata": {"name": "exact-rs", "namespace": "ns", "uid": "rs",
           "ownerReferences": [{"kind": "Deployment", "name": "exact", "uid": "deployment", "controller": True}]}}
    pods, normalized = [], []
    for index in range(2):
        uid = f"uid-{index}"
        pod = {"kind": "Pod", "metadata": {"name": f"pod-{index}", "namespace": "ns", "uid": uid,
               "ownerReferences": [{"kind": "ReplicaSet", "name": "exact-rs", "uid": "rs", "controller": True}]},
               "spec": {**copy.deepcopy(workload["spec"]["template"]["spec"]), "nodeName": "node"},
               "status": {"phase": "Running", "containerStatuses": [{"name": "worker", "containerID": f"containerd://cid-{index}",
                          "imageID": "sha256:final", "restartCount": 0,
                          "state": {"running": {"startedAt": "2026-09-10T00:00:00Z"}}}]}}
        pods.append(pod)
        normalized.append({"metadata": pod["metadata"], "containers": [dict(c, group=group)
                           for group in ("containers", "initContainers") for c in pod["spec"][group]]})
    return pods, [workload, rs], normalized


def series(item, points, cpu=False):
    labels = {"namespace": item["namespace"], "pod": item["pod"], "container": item["container"],
              "name": item["containerID"].split("://")[-1], "id": item["podUID"].replace("-", "_"), "node": item["node"]}
    if cpu:
        labels["cpu"] = "total"
    return {b.labels_key(labels): {"labels": labels, "points": points}}


class BudgetsTest(unittest.TestCase):
    def test_missing_is_not_zero_and_monotonic_rounding(self):
        self.assertIsNone(b.quantity(None))
        self.assertIsNone(b.proposed(None, None, "1.2", ".01"))
        self.assertEqual(b.stats([])["max"], None)
        self.assertEqual(b.proposed(Decimal(".55"), .01, "1.2", ".01"), Decimal(".55"))
        self.assertEqual(b.proposed(Decimal(".01"), .02001, "1.2", ".01"), Decimal(".03"))
        self.assertEqual(b.proposed(b.quantity("64Mi"), 65 * b.MIB, "1.1", str(16 * b.MIB)), 80 * b.MIB)
        self.assertEqual(b.proposed(None, 80 * b.MIB, "1.1", str(b.MIB)), 88 * b.MIB)
        self.assertEqual(b.quantity("1Gi"), b.quantity("1024Mi"))
        self.assertEqual(b.quantity("10m"), Decimal(".01"))

    def test_distinct_counter_samples_resets_and_gaps(self):
        points = {100: 2, 105: 2, 110: 4, 120: 1, 130: 3, 180: 5}
        times = {100: 100, 105: 100, 110: 110, 120: 120, 130: 130, 180: 180}
        intervals, audit = b.counter_intervals(points, times, 100, 200, 30)
        self.assertEqual([p["cores"] for p in intervals], [.2, .2])
        self.assertEqual(audit["distinctSourceSamples"], 5)
        self.assertEqual(audit["resets"], 1)
        self.assertEqual(audit["longGaps"], 1)
        self.assertEqual(audit["coveredSeconds"], 20)
        self.assertEqual(b.counter_intervals(points, {}, 100, 200, 30)[0], [])
        with self.assertRaisesRegex(ValueError, "one source timestamp"):
            b.counter_intervals({100: 2, 105: 3}, {100: 100, 105: 100}, 100, 200, 30)

    def test_exact_owner_replicas_init_and_versioning(self):
        pods, workloads, normalized = fixture()
        rows, instances = b.inventory(pods, workloads, normalized)
        self.assertEqual(len(rows), 2)
        self.assertEqual(len(instances), 4)
        active = b.stable_active([(rows, instances), (rows, copy.deepcopy(instances))])
        self.assertEqual(len(active), 2)
        self.assertTrue(all(i["row"][2] == "exact" for i in active.values()))
        changed = copy.deepcopy(instances)
        next(i for i in changed.values() if i["running"])["imageID"] = "sha256:old"
        with self.assertRaisesRegex(ValueError, "version changed"):
            b.stable_active([(rows, instances), (rows, changed)])
        pods[0]["metadata"]["ownerReferences"][0]["uid"] = "missing"
        with self.assertRaisesRegex(ValueError, "unresolved exact owner"):
            b.inventory(pods, workloads, normalized)

    def test_same_name_wrong_uid_and_template_drift_rejected(self):
        pods, workloads, normalized = fixture()
        workloads[1]["metadata"]["ownerReferences"][0]["name"] = "ambiguous"
        with self.assertRaisesRegex(ValueError, "mismatch"):
            b.inventory(pods, workloads, normalized)
        pods, workloads, normalized = fixture()
        pods[0]["spec"]["containers"][0]["resources"]["requests"]["cpu"] = "20m"
        with self.assertRaisesRegex(ValueError, "actual/template config mismatch"):
            b.inventory(pods, workloads, normalized)
        pods, workloads, normalized = fixture()
        normalized = copy.deepcopy(normalized)
        normalized[0]["containers"][0]["resources"]["requests"]["memory"] = "80Mi"
        with self.assertRaisesRegex(ValueError, "full/normalized inventory mismatch"):
            b.inventory(pods, workloads, normalized)

    def test_replica_peak_not_average_or_sum(self):
        replicas = [[.01, .02], [.01, .21]]
        summary = b.stats([v for replica in replicas for v in replica], .01)
        self.assertEqual(summary["max"], .21)
        self.assertEqual(summary["fractionAboveRequest"], .5)
        per_replica = b.proposed(Decimal(".01"), summary["max"], "1.2", ".01")
        self.assertEqual(per_replica, Decimal(".26"))
        self.assertEqual(per_replica * len(replicas), Decimal(".52"))

    def test_freshness_boundary_missing_and_runtime_identity(self):
        rows, instances = b.inventory(*fixture())
        item = next(i for i in instances.values() if i["running"])
        node = {
            "container-memory": series(item, {100: 10, 105: 20, 110: 30, 115: 40}),
            "container-memory-time": series(item, {100: 99, 105: 101, 110: 70}),
            "container-cpu-rate": series(item, {100: .5, 160: .2}, cpu=True),
            "container-cpu-time": series(item, {100: 99, 160: 158}, cpu=True),
        }
        grids = [{0: {100, 105, 110, 115, 160}}, {0: {100, 105, 110, 115, 160}}]
        windows, values = b.measure(item, [node, {}], grids, [(100, 170)], 30)
        charge = windows[0]["metrics"]["chargeBytes"]
        self.assertEqual(charge["coveredSlots"], 1)
        self.assertEqual(charge["preWindowSourceSlotsExcluded"], 1)
        self.assertEqual(charge["staleOrMissingTimestampSlots"], 2)
        self.assertEqual(values["chargeBytes"], [20])
        self.assertEqual(values["cpuRateCores"], [.2])
        self.assertIsNone(windows[0]["metrics"]["processRSSBytes"]["max"])
        labels = next(iter(node["container-memory"].values()))["labels"]
        self.assertTrue(b.match_series(labels, item, True))
        self.assertFalse(b.match_series(dict(labels, name="other-runtime"), item, True))
        self.assertFalse(b.match_series(dict(labels, id="other-pod-uid"), item, True))

    def test_hashes_and_manifest_version(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            chunk = root / "chunk-000000"
            chunk.mkdir()
            raw = {"status": "success", "data": {"resultType": "matrix", "result": []}}
            content = json.dumps(raw).encode()
            path = chunk / "container-memory.json"
            path.write_bytes(content)
            expected = hashlib.sha256(content).hexdigest()
            evidence = b.Evidence(root)
            self.assertEqual(evidence.read(path, expected), raw)
            with self.assertRaisesRegex(ValueError, "SHA256 mismatch"):
                evidence.read(path, "bad")
            manifest = {"version": 2, "stepNanoseconds": 5_000_000_000,
                        "start": b.WINDOWS[0]["start"], "end": b.WINDOWS[0]["end"], "queries": []}
            (chunk / "manifest.json").write_text(json.dumps(manifest))
            bounds = [(b.timestamp(b.WINDOWS[0]["start"]), b.timestamp(b.WINDOWS[0]["end"]))]
            with self.assertRaisesRegex(ValueError, "unsupported stream manifest version"):
                b.load_stream(evidence, root, bounds, "ns")
            manifest["version"] = 1
            manifest["queries"] = [{"name": "container-memory", "expr": "container_memory_usage_bytes", "status": "success",
                                    "file": path.name, "sha256": expected}]
            (chunk / "manifest.json").write_text(json.dumps(manifest))
            loaded = b.load_stream(b.Evidence(root), root, bounds, "ns")
            self.assertEqual(loaded[0], {})
            self.assertEqual(len(loaded[1][0]), 96)


if __name__ == "__main__":
    unittest.main()

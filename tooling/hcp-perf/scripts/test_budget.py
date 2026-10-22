#!/usr/bin/env python3
import copy
from fractions import Fraction as F
import json
import os
from pathlib import Path
import tempfile
import unittest

import budget as b


EXAMPLE = Path(__file__).resolve().parents[1] / "examples/budget-small.json"


class BudgetTests(unittest.TestCase):
    def setUp(self):
        self.spec = json.loads(EXAMPLE.read_bytes())

    def tiny(self):
        spec = copy.deepcopy(self.spec)
        row = next(r for r in spec["rows"] if r["identity"] ==
                   {"kind": "Deployment", "workload": "kube-apiserver", "container": "kube-apiserver"})
        row.update(replicas=1, weights=dict(cpuMilli=1, memoryMiB=1), floors=dict(cpuMilli=1, memoryMiB=1))
        spec["rows"] = [row]
        spec["pods"] = [dict(workload="kube-apiserver", replicas=1, regular=[0], init=[], overhead=dict(cpuMilli=0, memoryMiB=0))]
        spec["downstream"].update(rows=[], cpuMilli=0, memoryMiB=0)
        spec["node"].update(priceUSDPerHour="1", cpuMilli=100, memoryMiB=100)
        spec["budgetUSDPerHour"] = "1"
        return spec

    def test_quota_minimum_quantum(self):
        config = self.spec["quota"]
        for cpu, want in ((1, 10), (3, 10), (10, 10), (23, 30), (183, 190)):
            self.assertEqual(b.quota(cpu, config), want)
        self.assertEqual(b.quota(1, dict(periodUS=50000, minimumUS=1500, quantumUS=1000)), 30)
        self.assertEqual(b.quota(23, dict(periodUS=100000, minimumUS=1, quantumUS=1)), 23)
        for field in ("periodUS", "minimumUS", "quantumUS"):
            with self.assertRaises(ValueError):
                b.quota(1, {**config, field: 0})
        with self.assertRaises(ValueError):
            b.quota(0, config)

    def test_shared_parent_and_replicas(self):
        pod = dict(regular=[0, 1, 2], init=[], overhead=dict(cpuMilli=0, memoryMiB=0))
        charge = b.pod_charge(pod, [3, 3, 3], "cpuMilli", self.spec["quota"])
        self.assertEqual(charge["uncappedStageMaximumMilli"], 30)
        self.assertEqual(charge["parentQuotaMilli"], 10)
        self.assertEqual(charge["charge"], 10)
        spec = self.tiny()
        spec["rows"][0]["replicas"] = spec["pods"][0]["replicas"] = 3
        _, ledger = b.allocate(spec)
        self.assertEqual(ledger["resources"]["cpuMilli"]["offeringCharge"], "90")
        self.assertEqual(ledger["resources"]["memoryMiB"]["offeringCharge"], "99")

    def test_sequential_restartable_and_overhead(self):
        pod = dict(regular=[0], init=[dict(row=1, restartable=False), dict(row=2, restartable=False)],
                   overhead=dict(cpuMilli=3, memoryMiB=7))
        self.assertEqual(b.stages(pod), [[1], [2], [0]])
        self.assertEqual(b.pod_charge(pod, [20, 50, 40], "memoryMiB", self.spec["quota"]), dict(request=57, charge=62))
        pod["init"][0]["restartable"] = True
        self.assertEqual(b.stages(pod), [[1], [1, 2], [0, 1]])
        charge = b.pod_charge(pod, [20, 50, 40], "cpuMilli", self.spec["quota"])
        self.assertEqual(charge["request"], 93)
        self.assertEqual(charge["parentQuotaMilli"], 100)
        self.assertEqual(charge["charge"], 93)
        self.assertEqual(b.pod_charge(pod, [1, 1, 1], "memoryMiB", self.spec["quota"])["charge"], 11)

    def test_floors_are_not_fixed_and_exact_exhaustion(self):
        spec = self.tiny()
        spec["rows"][0]["floors"] = dict(cpuMilli=20, memoryMiB=64)
        profile, ledger = b.allocate(spec)
        self.assertGreater(profile["anchor"]["cpuMilli"], 20)
        self.assertGreater(profile["anchor"]["memoryMiB"], 64)
        spec["node"].update(cpuMilli=20, memoryMiB=71)
        profile, ledger = b.allocate(spec)
        self.assertEqual(profile["anchor"], dict(cpuMilli=20, memoryMiB=64))
        self.assertEqual(ledger["costUSDPerHour"], "1")
        spec["node"]["memoryMiB"] = 70
        with self.assertRaisesRegex(ValueError, "protected allocations exceed budget"):
            b.allocate(spec)
        spec["node"].update(cpuMilli=19, memoryMiB=71)
        with self.assertRaisesRegex(ValueError, "protected allocations exceed budget"):
            b.allocate(spec)

    def test_bad_data_rejected(self):
        changes = [lambda s: s["rows"][0]["weights"].update(cpuMilli=0),
                   lambda s: s["rows"][0]["weights"].update(memoryMiB="NaN"),
                   lambda s: s["rows"][0]["weights"].update(cpuMilli=1.5),
                   lambda s: s["rows"][0]["floors"].update(cpuMilli=True),
                   lambda s: s["rows"][0]["floors"].update(memoryMiB=0),
                   lambda s: s["rows"][0].update(replicas=2),
                   lambda s: s["rows"][0].update(quietP95CPUMilli=None),
                   lambda s: s["rows"][0]["source"].update(method="annotation"),
                   lambda s: s["rows"][0]["source"].update(sha256=""),
                   lambda s: s["rows"].append(copy.deepcopy(s["rows"][0])),
                   lambda s: s["pods"][0]["regular"].append(999),
                   lambda s: s["pods"][0].update(resources={}),
                   lambda s: s["node"].update(currency="EUR"),
                   lambda s: s["node"].update(cpuMilli=0),
                   lambda s: s["node"].update(priceUSDPerHour="Infinity"),
                   lambda s: s["downstream"].update(cpuMilli=471),
                   lambda s: s["rows"][0]["floors"].update(cpuMilil=500),
                   lambda s: s.update(fixedCPU=100),
                   lambda s: s["excludedInstrumentation"][0].update(workload="kube-apiserver")]
        for i, change in enumerate(changes):
            with self.subTest(case=i):
                spec = copy.deepcopy(self.spec)
                change(spec)
                with self.assertRaises(ValueError):
                    b.allocate(spec)
        del self.spec["rows"][0]["weights"]["cpuMilli"]
        with self.assertRaises(ValueError):
            b.allocate(self.spec)

    def test_portable_combined_candidate_and_runtime(self):
        before = b.encode(self.spec)
        profile, ledger = b.allocate(self.spec)
        self.assertEqual(before, b.encode(self.spec))
        self.assertEqual(len(profile["containers"]), 92)
        self.assertEqual(sum(p["replicas"] for p in ledger["pods"]), 52)
        self.assertEqual(sum(r["replicas"] for r in ledger["rows"]), 97)
        self.assertEqual(ledger["costUSDPerHour"], "12493/100000")
        self.assertEqual(ledger["resources"]["memoryMiB"]["offeringCharge"], "10101")
        for resource, result in ledger["resources"].items():
            self.assertLessEqual(F(result["offeringCharge"]), F(result["capacity"]))
            self.assertGreater(F(result["nextGridCharge"]), F(result["capacity"]))
            for row in ledger["rows"]:
                self.assertGreaterEqual(row["requests"][resource], row["floors"][resource])
        rows = {(r["identity"]["workload"], r["identity"]["container"]): r for r in ledger["rows"]}
        self.assertEqual(rows["kube-apiserver", "bootstrap"]["requests"], dict(cpuMilli=50, memoryMiB=64))
        self.assertEqual(rows["cluster-network-operator", "cluster-network-operator"]["floors"]["memoryMiB"], 80)
        self.assertEqual(rows["ignition-server-proxy", "haproxy"]["env"], {})
        self.assertEqual(rows["router", "router"]["env"], {})
        self.assertEqual(rows["cluster-version-operator", "cluster-version-operator"]["env"], {"GOMEMLIMIT": "53687091B"})
        self.assertTrue(ledger["missingLayoutRows"])
        self.assertFalse(ledger["qualified"])
        self.assertEqual((profile, ledger), b.allocate(self.spec))

    def test_immutable_outputs_and_check(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "candidate"
            b.run(EXAMPLE, output)
            initial = {p.name: p.read_bytes() for p in output.iterdir()}
            b.run(EXAMPLE, output, check=True)
            with self.assertRaisesRegex(ValueError, "never overwrites"):
                b.run(EXAMPLE, output)
            self.assertEqual(initial, {p.name: p.read_bytes() for p in output.iterdir()})
            (output / "candidate.json").write_bytes(b"{}")
            with self.assertRaisesRegex(ValueError, "output differs"):
                b.run(EXAMPLE, output, check=True)

    @unittest.skipUnless(os.environ.get("HCP_PERF_BUDGET_BINARY"), "set HCP_PERF_BUDGET_BINARY for existing CLI integration")
    def test_every_row_resolves_and_renders(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "candidate"
            binary = Path(os.environ["HCP_PERF_BUDGET_BINARY"])
            result = b.run(EXAMPLE, output, binary=binary)
            self.assertTrue(result["resolverVerified"])
            b.run(EXAMPLE, output, check=True, binary=binary)


if __name__ == "__main__":
    unittest.main()

"""Fast benchmark regressions, without running the production fleet or CP search."""

from contextlib import redirect_stdout, redirect_stderr
from copy import deepcopy
from dataclasses import asdict
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

from demand.model import ClusterDemand, Component
from optimize import benchmark, engine, exact
from skus import SKU


class TinyDemand:
    sizes = ["12", "30", "60", "120", "250"]

    def cluster_demand(self, size, policy, *args, **kwargs):
        return ClusterDemand(size, policy, [
            Component("etcd", "zonal_etcd", True, 3, 100, 100, 1),
            Component("other", "overflow", False, 1, 100, 100, 0)])


class BenchmarkTests(unittest.TestCase):
    def setUp(self):
        self.model = TinyDemand()
        self.skus = [SKU("small", 4, 16, 4), SKU("large", 8, 32, 4)]
        self.cfg = engine.RunConfig(mode="exact", reserve_slots=1, az_failure_reserve=0,
                                    concurrent_rolling_hcps=0, overflow_az_count=3)
        self.dist = {"30": 2}
        self.result = engine.simulate(self.dist, self.cfg, self.model, self.skus)
        self.prices = {"hourly": {"small": 1, "large": 2}}

    def summarize(self, result=None, approach="greedy", mode="full"):
        with patch.object(benchmark, "PRICES", self.prices):
            return benchmark.summarize(self.result if result is None else result, approach,
                                       self.dist, self.cfg, self.model, self.skus, 0.25, mode=mode)

    def test_reserve_role_mismatch_full_and_pinned(self):
        self.cfg.concurrent_rolling_hcps = 1
        demand = ClusterDemand("30", "minimal", [
            Component("api", "overflow", True, 2, 100, 100, 0)])
        with patch.object(self.model, "cluster_demand", return_value=demand):
            self.result = engine.simulate(self.dist, self.cfg, self.model, self.skus)
            self.result["management_clusters"][0]["count"] = 2
            self.result["n_mcs"] = 2
            self.dist = {"30": 4}
            full = self.summarize()
            pinned = self.summarize(mode="pinned")
            self.result["solver"] = {"status": "FEASIBLE", "validated": True}
            pinned_cp = self.summarize(approach="cpsat", mode="pinned")
        self.assertTrue(full["verified"])
        self.assertTrue(full["policy_violation"])
        self.assertFalse(full["comparable"])
        self.assertEqual(full["status"], "NONCOMPLIANT")
        self.assertEqual(full["reserve_role_difference"], [{
            "hcp_size": "30", "component": "api", "reserve_kind": "rollout",
            "expected_pool": "zonal", "actual_pool": "overflow", "pods": 2,
            "cpu_cores": 0.2, "memory_gib": 200 / 1024}])
        for row in (pinned, pinned_cp):
            self.assertTrue(row["policy_violation"])
            self.assertTrue(row["comparable"])
            self.assertTrue(any("replay legacy builder" in warning for warning in row["warnings"]))
        for mode, row, basis in (("full", full, "not_same_policy"),
                                 ("pinned", pinned, "replay_legacy_builder")):
            with patch.object(benchmark.engine, "simulate", return_value={}), \
                    patch.object(benchmark.cpsat, "solve_region", return_value={}), \
                    patch.object(benchmark, "summarize", return_value=row), \
                    redirect_stdout(io.StringIO()) as stdout:
                result = benchmark.run_case("small", self.dist, self.cfg, self.model, self.skus, {"mode": mode})
                benchmark.print_report({"cases": [result], "notes": []})
            comparison = result["policies"]["minimal"]
            self.assertEqual(comparison["comparison_basis"], basis)
            self.assertIsNotNone(comparison["difference_cpsat_minus_greedy"])
            self.assertIn(f"[{basis}]", stdout.getvalue())

    def test_inventory_demand_and_physical_overflow_bill(self):
        row = self.summarize()
        self.assertTrue(row["verified"])
        self.assertTrue(row["comparable"])
        self.assertFalse(row["policy_violation"])
        self.assertEqual(row["reserve_role_difference"], [])
        metrics = row["metrics"]
        total = metrics["total"]
        self.assertEqual(total["real_demand"]["pods"], 8)
        self.assertEqual(total["reserved_demand"]["pods"], 4)
        self.assertAlmostEqual(total["total_demand"]["cpu_cores"], 1.2)
        self.assertAlmostEqual(total["total_demand"]["memory_gib"], 1200 / 1024)
        self.assertEqual(total["total_demand"]["nic"], 9)
        expected_bill = sum(count * self.prices["hourly"][sku]
                            for role in ("zonal", "overflow")
                            for sku, count in metrics[role]["sku_mix"].items())
        self.assertEqual(total["hourly"], expected_bill)
        self.assertEqual(metrics["overflow"]["nodes"], self.result["region"]["overflow_nodes"])
        self.assertNotIn("packing", json.dumps(row))

    def test_grouped_mc_multiplicity(self):
        baseline = self.summarize()["metrics"]["total"]
        self.result["management_clusters"][0]["count"] = 2
        self.result["n_mcs"] = 2
        self.dist = {"30": 4}
        row = self.summarize()
        self.assertTrue(row["verified"])
        for key in ("cores", "nodes", "memory_gib", "hourly", "pod_capacity", "nic_capacity"):
            self.assertEqual(row["metrics"]["total"][key], 2 * baseline[key])

    def test_regional_overlap_even_when_each_mc_is_disjoint(self):
        other = deepcopy(self.result["management_clusters"][0])
        swap = {"small": self.skus[1], "large": self.skus[0]}
        for group in [*other["packing"]["zonal"], other["packing"]["overflow"]]:
            for node in group:
                replacement = exact.node_to_json(exact.new_node(self.cfg, swap[node["sku"]]))
                node.update({k: replacement[k] for k in ("sku", "full", "cap", "infra")})
        self.result["management_clusters"].append(other)
        self.result["n_mcs"] = 2
        self.dist = {"30": 4}
        row = self.summarize()
        self.assertTrue(row["verified"])
        self.assertFalse(row["comparable"])
        self.assertEqual(row["status"], "NONCOMPLIANT")
        self.assertEqual(row["regional_sku_overlap"], ["large", "small"])

    def test_corrupt_packing_cannot_be_verified(self):
        self.result["management_clusters"][0]["packing"]["overflow"][0]["pods"].pop()
        row = self.summarize()
        self.assertEqual(row["status"], "INVALID")
        self.assertIsNone(row["metrics"])

    def test_missing_pool_is_not_zero_cost(self):
        self.result["management_clusters"][0]["overflow_pool"] = None
        row = self.summarize()
        self.assertEqual(row["status"], "NOT_SOLVED")
        self.assertIsNone(row["metrics"])

    def test_missing_price_is_not_free(self):
        self.prices = {"hourly": {}}
        row = self.summarize()
        self.assertIsNone(row["metrics"]["total"]["hourly"])
        self.assertTrue(row["warnings"])

    def test_no_incumbent_and_guards_preserve_status_and_bound(self):
        for status in ("UNKNOWN", "INFEASIBLE", "MODEL_TOO_LARGE", "DEPENDENCY_MISSING"):
            with self.subTest(status=status):
                result = {"region": None, "management_clusters": [], "error": "not solved",
                          "solver": {"status": status, "objective": None,
                                     "best_objective_bound": 123, "build_time_seconds": 0.1,
                                     "solve_time_seconds": 0.2}}
                row = self.summarize(result, "cpsat")
                self.assertEqual(row["status"], status)
                self.assertEqual(row["solver"]["best_objective_bound"], 123)
                self.assertEqual(row["build_seconds"], 0.1)
                self.assertEqual(row["search_seconds"], 0.2)
                self.assertFalse(row["feasible"])
                self.assertIsNone(row["metrics"])

    def test_cpsat_requires_validation(self):
        self.result["solver"] = {"status": "FEASIBLE", "validated": False}
        self.assertEqual(self.summarize(approach="cpsat")["status"], "INVALID")

    def test_vns_requires_validation_and_independent_packing_check(self):
        for status, validated in (("FEASIBLE", False), ("UNKNOWN", True)):
            self.result["solver"] = {"status": status, "validated": validated}
            row = self.summarize(approach="vns", mode="fixed")
            self.assertEqual(row["status"], "INVALID")
            self.assertIn("VNS incumbent", row["reason"])
        self.result["solver"] = {"status": "FEASIBLE", "validated": True,
                                 "seed_time_seconds": 0.1, "search_time_seconds": 0.2}
        row = self.summarize(approach="vns", mode="fixed")
        self.assertTrue(row["verified"])
        self.assertEqual((row["build_seconds"], row["search_seconds"]), (0.1, 0.2))
        self.assertEqual(row["metrics"], self.summarize(mode="fixed")["metrics"])
        self.result["management_clusters"][0]["packing"]["overflow"][0]["pods"].pop()
        row = self.summarize(approach="vns", mode="fixed")
        self.assertEqual(row["status"], "INVALID")
        self.assertIsNone(row["metrics"])

    def test_vns_fixed_scope_allows_regional_overlap_not_global_claim(self):
        self.test_regional_overlap_even_when_each_mc_is_disjoint()
        self.result["solver"] = {"status": "FEASIBLE", "validated": True}
        search = {"time_limit": 7, "seed": 13, "max_iterations": 29}
        with patch.object(benchmark.engine, "simulate", return_value=self.result) as greedy, \
                patch.object(benchmark.vns, "solve_region", return_value=self.result) as vns, \
                patch.object(benchmark.cpsat, "solve_region", side_effect=AssertionError("CP-SAT called")), \
                patch.object(benchmark, "PRICES", self.prices), \
                redirect_stdout(io.StringIO()) as stdout:
            result = benchmark.run_case("small", self.dist, self.cfg, self.model, self.skus,
                                        search, solver="vns")
            benchmark.print_report({"cases": [result], "notes": benchmark.VNS_NOTES})
        for i, policy in enumerate(("legacy", "minimal")):
            self.assertEqual(greedy.call_args_list[i].args, vns.call_args_list[i].args)
            self.assertEqual(vns.call_args_list[i].kwargs, search)
            self.assertEqual(vns.call_args_list[i].args[1].policy, policy)
            comparison = result["policies"][policy]
            self.assertEqual(comparison["comparison_basis"], "fixed_build_mc_per_mc")
            self.assertEqual(comparison["scope"], benchmark.vns.SCOPE)
            self.assertNotIn("difference_cpsat_minus_greedy", comparison)
            delta = comparison["difference_vns_minus_greedy"]
            self.assertTrue(all(value == 0 for value in delta.values()))
            self.assertEqual(set(delta), {"zonal_cores", "overflow_cores", "cores", "nodes",
                                           "memory_gib", "hourly", "pod_capacity", "nic_capacity"})
            for row in comparison["runs"]:
                self.assertTrue(row["verified"] and row["comparable"])
                self.assertEqual(row["status"], "FEASIBLE")
                self.assertEqual(row["regional_sku_overlap"], ["large", "small"])
        self.assertIn("VNS minus greedy: [fixed_build_mc_per_mc]", stdout.getvalue())
        self.assertIn("not globally SKU-disjoint", stdout.getvalue())

    def test_vns_fixed_scope_validates_balanced_replica_spread_and_reserve_routing(self):
        self.cfg.concurrent_rolling_hcps = 1
        demand = ClusterDemand("30", "minimal", [
            Component("api", "zonal_pair", True, 4, 100, 100, 0),
            Component("other", "overflow", True, 2, 100, 100, 0)])
        with patch.object(self.model, "cluster_demand", return_value=demand):
            self.result = engine.simulate(self.dist, self.cfg, self.model, self.skus)
            self.result["solver"] = {"status": "FEASIBLE", "validated": True}
            for approach in ("greedy", "vns"):
                row = self.summarize(approach=approach, mode="fixed")
                self.assertTrue(row["verified"], row["reason"])
                self.assertTrue(row["comparable"])
                self.assertTrue(row["policy_violation"])
                self.assertEqual(row["status"], "FEASIBLE")

    def test_vns_cli_counters_options_and_checkpoints_without_ortools(self):
        for options, seed, iterations, interrupted in (
                ([], 1, 1000, False),
                (["--seed", "19", "--max-iterations", "0"], 19, 0, False),
                ([], 1, 1000, True)):
            with self.subTest(options=options, interrupted=interrupted), tempfile.TemporaryDirectory() as tmp:
                output = Path(tmp) / "vns.json"
                original = engine.simulate

                def solve(dist, cfg, model, skus, **search):
                    saved = json.loads(output.read_text())
                    self.assertEqual(saved["status"], "running")
                    comparison = saved["cases"][0]["policies"][cfg.policy]
                    self.assertEqual(len(comparison["runs"]), 1)
                    self.assertEqual(comparison["runs"][0]["approach"], "greedy")
                    self.assertEqual(comparison["scope"], benchmark.vns.SCOPE)
                    self.assertIsNone(comparison["difference_vns_minus_greedy"])
                    self.assertEqual(search, {"time_limit": 9, "seed": seed, "max_iterations": iterations})
                    if interrupted:
                        raise KeyboardInterrupt
                    result = original(dist, cfg, model, skus)
                    result["solver"] = {"status": "FEASIBLE", "validated": True,
                                        "seed_time_seconds": 0.125, "search_time_seconds": 0.25,
                                        "neighborhoods": {"repack_2": {
                                            "attempted": 10, "feasible": 6, "improved": 2,
                                            "shakes_accepted": 3}}}
                    return result

                with patch.dict(sys.modules, {"ortools": None}), \
                        patch.object(benchmark, "DemandModel", return_value=self.model), \
                        patch.object(benchmark, "load_catalog", return_value=self.skus), \
                        patch.object(benchmark, "PRICES", self.prices), \
                        patch.object(benchmark.vns, "solve_region", side_effect=solve) as vns, \
                        patch.object(benchmark.cpsat, "solve_region", side_effect=AssertionError("CP-SAT called")), \
                        redirect_stdout(io.StringIO()) as stdout:
                    code = benchmark.main(["--solver", "vns", "--fleet", '{"30": 2}',
                                           "--time-limit", "9", "--az-reserve", "0",
                                           "--reserve-slots", "0", "--output", str(output), *options])
                report = json.loads(output.read_text())
                self.assertEqual(report["provenance"]["solver"], "vns")
                self.assertEqual(report["provenance"]["search"], {
                    "time_limit": 9, "seed": seed, "max_iterations": iterations})
                self.assertEqual(code, 130 if interrupted else 0)
                self.assertEqual(report["status"], "interrupted" if interrupted else "complete")
                self.assertEqual(vns.call_count, 1 if interrupted else 2)
                self.assertIn("Starting custom/legacy/vns (mode=fixed)", stdout.getvalue())
                if not interrupted:
                    self.assertIn("repack_2: attempted=10 feasible=6 improving=2 shaken=3", stdout.getvalue())
                    self.assertIn("0.125 0.250", stdout.getvalue())
                    for comparison in report["cases"][0]["policies"].values():
                        self.assertEqual(comparison["difference_vns_minus_greedy"]["hourly"], 0)
                        self.assertEqual(comparison["runs"][1]["solver"]["neighborhoods"]["repack_2"]["attempted"], 10)

    def test_same_config_both_policies_and_rejected_exception(self):
        seen = []
        original = engine.simulate

        def greedy(dist, cfg, model, skus):
            seen.append(("greedy", dist, asdict(cfg)))
            return original(dist, cfg, model, skus)

        def cp(dist, cfg, model, skus, **search):
            seen.append(("cpsat", dist, asdict(cfg)))
            raise ValueError("model guard")

        with patch.object(benchmark.engine, "simulate", side_effect=greedy), \
                patch.object(benchmark.cpsat, "solve_region", side_effect=cp), \
                redirect_stdout(io.StringIO()):
            result = benchmark.run_case("small", self.dist, self.cfg, self.model, self.skus, {})
        self.assertEqual(len(seen), 4)
        for i, policy in ((0, "legacy"), (2, "minimal")):
            self.assertEqual(seen[i][1:], seen[i + 1][1:])
            self.assertEqual(seen[i][2]["policy"], policy)
            self.assertEqual(seen[i][2]["mode"], "exact")
            comparison = result["policies"][policy]
            self.assertEqual(comparison["runs"][1]["status"], "REJECTED")
            self.assertEqual(comparison["runs"][1]["reason"], "model guard")
            self.assertIsNone(comparison["difference_cpsat_minus_greedy"])

    def test_difference_only_for_verified_comparable_results(self):
        for comparable in (True, False):
            row = self.summarize()
            row["comparable"] = comparable
            with patch.object(benchmark.engine, "simulate", return_value={}), \
                    patch.object(benchmark.cpsat, "solve_region", return_value={}), \
                    patch.object(benchmark, "summarize", return_value=row), \
                    redirect_stdout(io.StringIO()):
                result = benchmark.run_case("small", self.dist, self.cfg, self.model, self.skus, {})
            delta = result["policies"]["minimal"]["difference_cpsat_minus_greedy"]
            self.assertEqual(delta is not None, comparable)
            if comparable:
                self.assertEqual(delta["hourly"], 0)

    def test_table_reports_unavailable_not_zero(self):
        result = {"region": None, "solver": {"status": "MODEL_TOO_LARGE",
                  "objective": None, "best_objective_bound": None}, "error": "max_pods guard"}
        row = self.summarize(result, "cpsat")
        report = {"cases": [{"case": "default", "policies": {"minimal": {
            "runs": [row], "difference_cpsat_minus_greedy": None}}}], "notes": []}
        with redirect_stdout(io.StringIO()) as stdout:
            benchmark.print_report(report)
        text = stdout.getvalue()
        self.assertIn("MODEL_TOO_LARGE", text)
        self.assertIn("NA NA NA NA NA", text)
        self.assertIn("objective=NA bound=NA", text)
        self.assertIn("max_pods guard", text)
        self.assertIn("requires verified, comparable incumbents", text)

    def test_cli_all_defaults_and_json(self):
        with tempfile.TemporaryDirectory() as tmp, \
                patch.object(benchmark, "DemandModel", return_value=self.model), \
                patch.object(benchmark, "load_catalog", return_value=self.skus), \
                patch.object(benchmark, "run_case", side_effect=lambda name, dist, *args, **kwargs: {
                    "case": name, "distribution": dist, "policies": {}}) as run, \
                redirect_stdout(io.StringIO()):
            output = Path(tmp) / "summary.json"
            self.assertEqual(benchmark.main(["--case", "all", "--output", str(output)]), 0)
            report = json.loads(output.read_text())
        self.assertEqual(run.call_count, 4)
        self.assertEqual({c["case"]: c["distribution"] for c in report["cases"]}, benchmark.CASES)
        search = report["provenance"]["search"]
        self.assertEqual(report["provenance"]["solver"], "cpsat")
        self.assertEqual(search["seed"], 1)
        self.assertNotIn("max_iterations", search)
        self.assertEqual((search["time_limit"], search["workers"], search["mode"], search["max_pods"]),
                         (30, 8, "full", 10000))
        cfg = run.call_args.args[2]
        self.assertEqual((cfg.reserve_slots, cfg.reserve_size, cfg.reservation_mode), (5, "30", "scaled"))
        self.assertIn("ortools", report["provenance"])
        self.assertEqual(report["status"], "complete")

    def test_progress_and_checkpoint_survive_interruption(self):
        class FlushedOutput(io.StringIO):
            def __init__(self):
                super().__init__()
                self.flushed = ""

            def flush(self):
                self.flushed = self.getvalue()

        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / "partial.json"
            stdout = FlushedOutput()

            def interrupt(*args, **kwargs):
                partial = json.loads(output.read_text())
                self.assertEqual(partial["status"], "running")
                rows = partial["cases"][0]["policies"]["legacy"]["runs"]
                self.assertEqual(len(rows), 1)
                self.assertEqual(rows[0]["approach"], "greedy")
                self.assertIn("Starting small/legacy/cpsat", stdout.flushed)
                raise KeyboardInterrupt

            with patch.object(benchmark, "DemandModel", return_value=self.model), \
                    patch.object(benchmark, "load_catalog", return_value=self.skus), \
                    patch.object(benchmark.cpsat, "solve_region", side_effect=interrupt), \
                    redirect_stdout(stdout):
                code = benchmark.main(["--case", "small", "--output", str(output)])
            saved = json.loads(output.read_text())
        self.assertEqual(code, 130)
        self.assertEqual(saved["status"], "interrupted")
        comparison = saved["cases"][0]["policies"]["legacy"]
        self.assertEqual(len(comparison["runs"]), 1)
        self.assertIsNone(comparison["difference_cpsat_minus_greedy"])

    def test_cli_custom_fleet_and_options(self):
        with patch.object(benchmark, "DemandModel", return_value=self.model), \
                patch.object(benchmark, "load_catalog", return_value=self.skus), \
                patch.object(benchmark, "run_case", return_value={"case": "custom", "policies": {}}) as run, \
                redirect_stdout(io.StringIO()):
            benchmark.main(["--fleet", '{"30": 1}', "--mode", "pinned", "--no-lns",
                            "--reserve-slots", "0", "--az-reserve", "0.25", "--max-pods", "20",
                            "--workers", "1", "--time-limit", "0", "--seed", "23"])
        name, dist, cfg, _, _, search = run.call_args.args
        self.assertEqual((name, dist), ("custom", {"30": 1}))
        self.assertEqual((cfg.reserve_slots, cfg.az_failure_reserve), (0, 0.25))
        self.assertEqual((search["mode"], search["use_lns"], search["max_pods"]), ("pinned", False, 20))
        self.assertEqual(search["seed"], 23)

    def test_invalid_cli_inputs(self):
        for args in (["--workers", "0"], ["--max-pods", "0"], ["--az-reserve", "nan"],
                     ["--solver", "other"], ["--max-iterations", "-1"], ["--seed", "1.5"],
                     ["--reserve-slots", "100"], ["--time-limit", "-1"],
                     ["--fleet", "{"], ["--fleet", "[]"], ["--fleet", '{"30": true}'],
                     ["--fleet", '{"30": 0}'], ["--case", "all", "--fleet", '{"30": 1}']):
            with self.subTest(args=args), redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as exc:
                benchmark.main(args)
            self.assertEqual(exc.exception.code, 2)


if __name__ == "__main__":
    unittest.main()

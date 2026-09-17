"""Offline peak ranking, provenance and resumability contracts."""
import argparse
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from observed import cli, peak


class RankWindowTests(unittest.TestCase):
    def rank(self, *values, window=120):
        return peak.rank_window({f"int/mc-{i}": dict(enumerate(v, 0))
                                 for i, v in enumerate(values)}, window // 60, 1)

    def test_sustained_load_beats_spike(self):
        result = self.rank([15, 0, 0, 10, 10, 0])
        self.assertEqual((result["selected_at"], result["score"]), (5, 10))

    def test_common_fleet_not_individual_maxima(self):
        result = self.rank([10, 10, 0, 0, 8, 8, 0], [0, 0, 10, 10, 8, 8, 0])
        self.assertEqual((result["selected_at"], result["score"]), (6, 16))
        self.assertEqual(result["clusters"], ["int/mc-0", "int/mc-1"])

    def test_missing_and_nonfinite_ticks_are_not_zero(self):
        for missing in (None, float("nan"), float("inf"), -1):
            with self.subTest(missing=missing):
                result = self.rank([50, missing, 1, 1, 1], [1, 1, 1, 1, 1])
                self.assertEqual((result["selected_at"], result["score"]), (4, 2))

    def test_absent_tick_and_end_presence_required(self):
        with self.assertRaisesRegex(ValueError, "full management-cluster coverage"):
            peak.rank_window({"int/a": {0: 100, 60: 100}, "int/b": {0: 1, 120: 1}},
                             120, 60, 0, 120)
        result = self.rank([1, 1, 100])
        self.assertEqual(result["score"], 1)

    def test_latest_tie_including_fractional_load(self):
        for values in ([2, 2, 2, 2], [0.1, 0.2, 0.1, 0.2, 0.1]):
            self.assertEqual(self.rank(values)["selected_at"], len(values) - 1)

    def test_zero_usage_is_valid(self):
        self.assertEqual(self.rank([0, 0, 0])["score"], 0)

    def test_invalid_windows_and_bounds(self):
        for window, step in ((0, 60), (30, 60), (61, 60), (120, 0), (120, -1), (1.5, 1)):
            with self.subTest(window=window, step=step), self.assertRaises(ValueError):
                peak.rank_window({"a": {0: 1, 60: 1}}, window, step)
        for start, end in ((1, 180), (0, 61), (120, 0), (0, 60)):
            with self.subTest(start=start, end=end), self.assertRaises(ValueError):
                peak.rank_window({"a": {0: 1}}, 120, 60, start, end)

    def test_no_data_and_week_union(self):
        for series in ({}, {"a": {}}, {"a": {0: 1, 60: 1, 120: 1}, "b": {}}):
            with self.subTest(series=series), self.assertRaises(ValueError):
                peak.rank_window(series, 120, 60)


class ForwardWindowTests(unittest.TestCase):
    def test_unchanged_without_overlap(self):
        for transitions in ([], [119.5], [120], [301]):
            with self.subTest(transitions=transitions):
                self.assertEqual(peak.forward_window(300, 120, 60, 60, transitions, 600), 300)

    def test_shift_forward_and_round_up(self):
        self.assertEqual(peak.forward_window(300, 120, 60, 60, [210], 600), 420)

    def test_repeat_for_newly_overlapping_transitions(self):
        transitions = [421, 210, 390]
        self.assertEqual(peak.forward_window(300, 120, 60, 60, transitions, 660), 660)
        self.assertEqual(transitions, [421, 210, 390])

    def test_smaller_new_class_still_requires_settling(self):
        # Direction and class size do not affect whether a transition overlaps.
        for old_class, new_class in ((4, 16), (16, 4)):
            with self.subTest(old_class=old_class, new_class=new_class):
                self.assertEqual(peak.forward_window(300, 120, 60, 60, [240], 600), 420)

    def test_fractional_transition_and_settling(self):
        for transition, settle, expected in ((240.001, 60, 480), (180, 0.001, 360),
                                             (179.5, 0.5, 300), (179.5, 0.501, 360)):
            with self.subTest(transition=transition, settle=settle):
                self.assertEqual(peak.forward_window(300, 120, 60, settle,
                                                     [transition], 600), expected)

    def test_inclusive_endpoints_even_without_settling(self):
        for transition, expected in ((180, 360), (300, 480), (180.5, 360)):
            with self.subTest(transition=transition):
                self.assertEqual(peak.forward_window(300, 120, 60, 0,
                                                     [transition], 600), expected)

    def test_settling_before_start_and_no_backwards_movement(self):
        for transition, settle, expected in ((0, 180, 300), (0, 181, 360),
                                             (179, 0, 300), (179, 2, 360)):
            with self.subTest(transition=transition, settle=settle):
                adjusted = peak.forward_window(300, 120, 60, settle, [transition], 600)
                self.assertEqual(adjusted, expected)
                self.assertGreaterEqual(adjusted, 300)
                self.assertLess(transition, adjusted - 120)
                self.assertLessEqual(transition + settle, adjusted - 120)

    def test_search_end_is_inclusive_and_never_exceeded(self):
        self.assertEqual(peak.forward_window(300, 120, 60, 60, [240], 420), 420)
        for transitions, horizon in (([240], 419.999), ([], 299), ([210, 390], 540)):
            with self.subTest(transitions=transitions, horizon=horizon):
                with self.assertRaisesRegex(ValueError, "exceeds search_end"):
                    peak.forward_window(300, 120, 60, 60, transitions, horizon)

    def test_invalid_inputs(self):
        for kwargs in ({"window": 0}, {"step": 0}, {"window": 121}, {"settle": -1},
                       {"selected_at": float("nan")}, {"search_end": float("inf")},
                       {"transitions": [float("nan")]}, {"settle": float("inf")}):
            args = dict(selected_at=300, window=120, step=60, settle=60,
                        transitions=[], search_end=600)
            args.update(kwargs)
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                peak.forward_window(**args)


class SearchTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.enterContext(patch("socket.socket", side_effect=AssertionError("network forbidden")))
        self.enterContext(patch.object(cli.subprocess, "run", side_effect=AssertionError("Azure CLI forbidden")))
        self.request = self.enterContext(patch.object(cli.Client, "request", side_effect=self.respond))
        self.args = argparse.Namespace(peak="memory", peak_lookback=240, peak_end=600,
                                       window=120, step=60, environment=["int"], cluster=".*",
                                       grafana=[], cache_dir=self.root / "cache", refresh=False,
                                       output=self.root / "output", allow_partial=True)
        self.uids = ["services-uksouth"]
        self.empty = set()
        self.fail = set()
        self.inventory_only = False
        self.series = {}

    def respond(self, url, resource=None, body=None):
        self.assertEqual(resource, cli.GRAFANA_RESOURCE)
        if url.endswith("/api/datasources"):
            return [{"uid": uid, "type": "prometheus", "secureJsonData": {"password": "secret"}}
                    for uid in self.uids]
        q = body["queries"][0]
        uid = q["datasource"]["uid"]
        if uid in self.fail:
            return {"results": {"A": {"error": "backend unavailable"}}}
        if uid in self.empty or (self.inventory_only and "kube_node_info" not in q["expr"]):
            return {"results": {"A": {"frames": []}}}
        ticks = list(range(int(body["from"]) // 1000, int(body["to"]) // 1000 + 1,
                           q["intervalMs"] // 1000))
        env = next(env for env, base in cli.GRAFANAS.items() if url.startswith(base))
        series = self.series.get((env, uid), {f"{env}-uksouth-mgmt-1": dict.fromkeys(ticks, 10)})
        return {"results": {"A": {"frames": [{
            "schema": {"fields": [{"type": "time"}, {"type": "number",
                       "labels": {"cluster": name}}]},
            "data": {"values": [[t * 1000 for t in ticks], [samples.get(t) for t in ticks]]},
        } for name, samples in series.items()]}}}

    def raw(self):
        return json.loads((self.args.output / "peak-search.json").read_text())

    def test_selection_schema_and_snapshot_resume_without_network(self):
        result = peak.search(self.args)
        self.assertEqual(set(result), {"scope", "regions", "metric", "window_seconds", "step_seconds",
                                       "search_start", "search_end", "config", "config_key",
                                       "queries", "sources", "warnings"})
        self.assertEqual(result["scope"], "regional")
        self.assertEqual(result["regions"], [{
            "environment": "int", "region": "uksouth", "selected_at": 600,
            "original_selected_at": 600, "score": 10, "clusters": ["int/int-uksouth-mgmt-1"],
            "window_seconds": 120, "step_seconds": 60, "metric": "memory", "units": "bytes",
            "search_start": 360, "search_end": 600,
        }])
        self.assertEqual(result["metric"], "memory")
        self.assertEqual((result["search_start"], result["search_end"]), (360, 600))
        self.assertEqual(len(result["config_key"]), 64)
        self.assertEqual(len(result["queries"]), 2)
        self.assertIn("response", result["queries"][0])
        self.assertNotIn("secret", json.dumps(self.raw()))
        self.args.cache_dir = None
        self.args.peak_end = None
        self.request.reset_mock(side_effect=True)
        self.request.side_effect = AssertionError("resume must not request")
        with patch.object(peak.time, "time", return_value=99999):
            self.assertEqual(peak.search(self.args), result)
        self.request.assert_not_called()

    def test_regions_rank_independently_and_rerank_legacy_global_cache(self):
        self.uids.append("services-westus3")
        ticks = range(360, 601, 60)
        # Deliberately misleading MC names: the query metadata defines region.
        self.series = {
            ("int", "services-uksouth"): {"int-westus3-mgmt-1": dict(zip(ticks, [20, 20, 1, 1, 1]))},
            ("int", "services-westus3"): {"int-uksouth-mgmt-1": dict(zip(ticks, [1, 1, 30, 30, 1]))},
        }
        result = peak.search(self.args)
        self.assertEqual([(r["region"], r["selected_at"], r["score"]) for r in result["regions"]],
                         [("uksouth", 480, 20), ("westus3", 600, 30)])
        global_selection = peak.rank_window({name: samples for series in self.series.values()
                                             for name, samples in series.items()}, 120, 60, 360, 600)
        self.assertEqual((global_selection["selected_at"], global_selection["score"]), (600, 31))
        raw = self.raw()
        self.assertEqual(raw["config"], {
            "metric": "memory", "window_seconds": 120, "step_seconds": 60,
            "lookback_seconds": 240, "search_start": 360, "search_end": 600,
            "environments": ["int"], "cluster": ".*", "grafana": {"int": cli.GRAFANAS["int"]},
        })
        raw["selection"] = global_selection
        cli.write_json(self.args.output / "peak-search.json", raw)
        self.args.cache_dir = None
        self.request.reset_mock()
        self.request.side_effect = AssertionError("legacy snapshot must not request")
        self.assertEqual(peak.search(self.args), result)
        self.assertEqual(self.raw()["config_key"], raw["config_key"])
        self.assertEqual(self.raw()["selection"]["regions"], result["regions"])
        self.request.assert_not_called()

    def test_region_requires_common_windows_but_not_other_regions_ticks(self):
        self.uids.append("services-westus3")
        self.series = {
            ("int", "services-uksouth"): {
                "int-other-mgmt-1": {360: 100, 420: 100, 480: 1, 540: 1},
                "int-other-mgmt-2": {420: 1, 480: 1, 540: 1},
            },
            ("int", "services-westus3"): {"int-other-mgmt-3": {480: 10, 540: 10, 600: 10}},
        }
        result = peak.search(self.args)
        self.assertEqual([(r["region"], r["selected_at"], r["score"]) for r in result["regions"]],
                         [("uksouth", 540, 51.5), ("westus3", 600, 10)])
        self.assertEqual(result["regions"][0]["clusters"], ["int/int-other-mgmt-1", "int/int-other-mgmt-2"])

    def test_same_region_in_different_environments_is_independent(self):
        self.args.environment = ["int", "stg"]
        ticks = range(360, 601, 60)
        self.series = {
            ("int", "services-uksouth"): {"int-other-mgmt-1": dict(zip(ticks, [20, 20, 1, 1, 1]))},
            ("stg", "services-uksouth"): {"stg-other-mgmt-1": dict(zip(ticks, [1, 1, 30, 30, 1]))},
        }
        result = peak.search(self.args)
        self.assertEqual([(r["environment"], r["region"], r["selected_at"]) for r in result["regions"]],
                         [("int", "uksouth", 480), ("stg", "uksouth", 600)])

    def test_interrupted_search_freezes_end_and_reuses_finished_query(self):
        self.args.peak_end = None
        with patch.object(peak.time, "time", return_value=935):
            with patch.object(cli, "query", side_effect=KeyboardInterrupt), self.assertRaises(KeyboardInterrupt):
                peak.search(self.args)
        self.assertEqual(self.raw()["config"]["search_end"], 600)
        self.assertIsNone(self.raw()["selection"])
        self.assertTrue(self.raw()["errors"])
        self.request.reset_mock()
        with patch.object(peak.time, "time", return_value=99999):
            result = peak.search(self.args)
        self.assertEqual(result["search_end"], 600)
        self.assertEqual(self.request.call_count, 2)  # discovery came from Client cache

    def test_client_disk_cache_reused_in_new_output(self):
        result = peak.search(self.args)
        self.args.output = self.root / "other-output"
        self.request.reset_mock(side_effect=True)
        self.request.side_effect = AssertionError("cache must not request")
        self.assertEqual(peak.search(self.args), result)
        self.request.assert_not_called()

    def test_interrupted_search_reuses_checkpointed_queries_without_cache(self):
        self.args.cache_dir = None
        real_query = cli.query
        calls = 0

        def interrupt(*args, **kwargs):
            nonlocal calls
            calls += 1
            if calls == 2:
                raise KeyboardInterrupt
            return real_query(*args, **kwargs)

        with patch.object(cli, "query", side_effect=interrupt), self.assertRaises(KeyboardInterrupt):
            peak.search(self.args)
        self.assertIn("response", self.raw()["queries"][0])
        self.request.reset_mock()
        self.assertEqual(peak.search(self.args)["regions"][0]["selected_at"], 600)
        self.assertEqual(self.request.call_count, 1)
        self.assertNotIn("kube_node_info", self.request.call_args.args[2]["queries"][0]["expr"])

    def test_refresh_uses_frozen_end_and_failed_refresh_removes_selection(self):
        peak.search(self.args)
        self.args.peak_end = None
        self.args.refresh = True
        self.request.reset_mock()
        with patch.object(peak.time, "time", return_value=99999):
            self.assertEqual(peak.search(self.args)["search_end"], 600)
        self.assertEqual(self.request.call_count, 3)
        self.fail.add("services-uksouth")
        with self.assertRaisesRegex(ValueError, "every datasource must succeed"):
            peak.search(self.args)
        self.assertIsNone(self.raw()["selection"])
        self.assertTrue(self.raw()["errors"])

    def test_changed_settings_rejected_before_requests(self):
        peak.search(self.args)
        for key, value in (("peak", "cpu"), ("window", 180), ("step", 30),
                           ("peak_lookback", 360), ("environment", ["stg"]),
                           ("cluster", "mgmt-2"), ("grafana", ["int=https://other.example"]),
                           ("peak_end", 660)):
            with self.subTest(key=key):
                args = argparse.Namespace(**vars(self.args))
                setattr(args, key, value)
                self.request.reset_mock()
                with self.assertRaisesRegex(ValueError, "cannot change"):
                    peak.search(args)
                self.request.assert_not_called()

    def test_default_horizon_window_and_daily_chunks(self):
        self.args.peak_end = 1209600
        del self.args.peak_lookback
        del self.args.window
        del self.args.peak
        result = peak.search(self.args)
        self.assertEqual(result["window_seconds"], 900)
        self.assertEqual(result["metric"], "memory")
        self.assertEqual(result["search_end"] - result["search_start"], 604800)
        rows = [q for q in result["queries"] if q["metric"] == "memory"]
        self.assertEqual(sum((q["end"] - q["start"]) // 60 + 1 for q in rows), 10081)
        for a, b in zip(rows, rows[1:]):
            self.assertEqual(a["end"] + 60, b["start"])
        self.assertTrue(all(q["end"] - q["start"] < 86400 for q in rows))

    def test_regional_filter_empty_region_and_source_coverage(self):
        self.uids += ["services-uk", "services-stg-westus3", "hcps-uksouth", "services-int-westus3"]
        self.empty.add("services-int-westus3")
        result = peak.search(self.args)
        source = result["sources"][0]
        self.assertEqual(source["datasources"], ["services-int-westus3", "services-uksouth"])
        self.assertEqual(source["coverage"][0]["clusters"], [])
        self.assertEqual(source["coverage"][0]["successful_queries"], 2)
        self.assertEqual(source["coverage"][1]["clusters"], result["regions"][0]["clusters"])
        self.assertEqual(len(result["regions"]), 1)

    def test_any_failed_datasource_blocks_even_allow_partial(self):
        self.uids.append("services-westus3")
        self.fail.add("services-westus3")
        with self.assertRaisesRegex(ValueError, "every datasource must succeed"):
            peak.search(self.args)
        raw = self.raw()
        self.assertIsNone(raw["selection"])
        self.assertEqual(raw["sources"][0]["coverage"][1]["failed_queries"], 2)
        self.assertIn("response", raw["queries"][-1])

    def test_inventory_without_usage_cannot_be_ignored(self):
        self.inventory_only = True
        with self.assertRaises(ValueError):
            peak.search(self.args)
        self.assertIsNone(self.raw()["selection"])

    def test_inventory_required_at_selected_end(self):
        def missing_inventory_end(url, resource=None, body=None):
            response = self.respond(url, resource, body)
            if body and "kube_node_info" in body["queries"][0]["expr"]:
                response["results"]["A"]["frames"][0]["data"]["values"][1][-1] = None
            return response

        self.request.side_effect = missing_inventory_end
        self.assertEqual(peak.search(self.args)["regions"][0]["selected_at"], 540)

    def test_query_helper_rejects_coarser_backend_grid(self):
        def coarser_grid(url, resource=None, body=None):
            response = self.respond(url, resource, body)
            if body:
                response["results"]["A"]["frames"][0]["schema"]["meta"] = {
                    "executedQueryString": "Step: 5m"}
            return response

        self.request.side_effect = coarser_grid
        with self.assertRaisesRegex(ValueError, "every datasource"):
            peak.search(self.args)
        self.assertIn("different sampling step", self.raw()["queries"][0]["error"])

    def test_all_environments_expands_to_configured_sources(self):
        self.args.environment = ["all"]
        result = peak.search(self.args)
        self.assertEqual(result["config"]["environments"], sorted(cli.GRAFANAS))
        self.assertEqual(len(result["sources"]), len(cli.GRAFANAS))

    def test_no_scope_clusters(self):
        self.args.cluster = "does-not-exist"
        with self.assertRaisesRegex(ValueError, "No management clusters"):
            peak.search(self.args)
        self.assertTrue(self.raw()["errors"])

    def test_no_regional_sources_and_discovery_failure(self):
        self.uids = ["services-uk", "services-stg-westus3"]
        with self.assertRaisesRegex(ValueError, "every datasource"):
            peak.search(self.args)
        self.args.refresh = True
        self.request.side_effect = RuntimeError("discovery unavailable")
        with self.assertRaisesRegex(ValueError, "every datasource"):
            peak.search(self.args)
        self.assertIn("discovery unavailable", self.raw()["sources"][0]["error"])

    def test_cpu_and_memory_queries_deduplicate_before_sum(self):
        memory = peak.search(self.args)
        expr = next(q["expression"] for q in memory["queries"] if q["metric"] == "memory")
        self.assertEqual(expr.count("max by (cluster,instance)"), 2)
        self.assertIn("node_memory_MemTotal_bytes", expr)
        self.assertIn("node_memory_MemAvailable_bytes", expr)
        self.args.peak = "cpu"
        self.args.output = self.root / "cpu"
        cpu = peak.search(self.args)
        self.assertEqual(cpu["regions"][0]["units"], "cores")
        expr = next(q["expression"] for q in cpu["queries"] if q["metric"] == "cpu")
        self.assertTrue(expr.startswith("sum by (cluster) (max by (cluster,instance,cpu,mode) (rate("))
        for term in ('mode!="idle"', 'mode!="guest"', 'mode!="guest_nice"', 'job=~"node|node-exporter"'):
            self.assertIn(term, expr)
        self.assertNotIn("container_cpu", expr)

    def test_invalid_settings_and_credential_urls(self):
        for key, value in (("window", 0), ("window", 90), ("step", 0), ("peak_lookback", 60),
                           ("peak_end", 601), ("peak", "disk"),
                           ("grafana", ["int=https://user:password@grafana.example"]),
                           ("grafana", ["int=https://grafana.example?token=secret"])):
            with self.subTest(key=key, value=value):
                args = argparse.Namespace(**vars(self.args))
                setattr(args, key, value)
                with self.assertRaises(ValueError):
                    peak.search(args)
        self.request.assert_not_called()


if __name__ == "__main__":
    unittest.main()

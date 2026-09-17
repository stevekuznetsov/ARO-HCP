"""Offline collector and publication contract tests; no Azure identity is needed."""

import argparse
from copy import deepcopy
from email.message import Message
import io
import json
from pathlib import Path
import re
import tempfile
import unittest
from unittest.mock import Mock, patch
import urllib.error
import urllib.response

from observed import cli


def response_frame(fields, values):
    return {"results": {"A": {"frames": [
        {"schema": {"fields": fields}, "data": {"values": values}},
    ]}}}


def view_fixture(errors=()):
    return {"schema_version": 1, "mode": "observed",
            "at": "2026-09-08T12:00:00Z", "start": "2026-09-08T11:00:00Z",
            "generated_at": "2026-09-08T12:05:00Z", "window_seconds": 3600,
            "step_seconds": 60, "sources": [], "errors": list(errors),
            "warnings": [], "transitions": [], "suggestion": None,
            "management_clusters": [{"id": "int/int-uksouth-mgmt-1"}]}


class CliTests(unittest.TestCase):
    def setUp(self):
        self.enterContext(patch("socket.socket", side_effect=AssertionError("network forbidden")))
        self.enterContext(patch.object(
            cli.urllib.request, "urlopen", side_effect=AssertionError("network forbidden")))
        self.real_build_opener = cli.urllib.request.build_opener
        self.opener = Mock(spec=cli.urllib.request.OpenerDirector)
        self.opener.open.side_effect = AssertionError("network forbidden")
        self.build_opener = self.enterContext(patch.object(
            cli.urllib.request, "build_opener", return_value=self.opener))
        self.run = self.enterContext(patch.object(
            cli.subprocess, "run", side_effect=AssertionError("Azure CLI forbidden")))
        self.enterContext(patch.object(cli.time, "sleep"))
        self.stdout = self.enterContext(patch("sys.stdout", new_callable=io.StringIO))
        self.stderr = self.enterContext(patch("sys.stderr", new_callable=io.StringIO))

    def output_dir(self):
        return Path(self.enterContext(tempfile.TemporaryDirectory()))

    def main(self, *args):
        with patch("sys.argv", ["observed.cli", *map(str, args)]):
            return cli.main()

    def client(self, cache_dir=None, **kwargs):
        client = cli.Client(cache_dir, **kwargs)
        client.request = Mock(side_effect=AssertionError("unexpected request"))
        return client

    def test_duration_units(self):
        for text, seconds in (("30s", 30), ("1m", 60), ("2h", 7200), ("1d", 86400)):
            with self.subTest(text=text):
                self.assertEqual(cli.duration(text), seconds)

    def test_duration_rejects_nonpositive_or_unsupported_formats(self):
        for text in ("0s", "-1m", "1.5h", "1h30m", "1", "1H", " 1m", ""):
            with self.subTest(text=text), self.assertRaises(argparse.ArgumentTypeError):
                cli.duration(text)

    def test_timestamp_normalizes_timezone_and_truncates_subseconds(self):
        expected = 1788868800
        for text in ("2026-09-08T12:00:00Z", "2026-09-08T14:00:00+02:00",
                     "2026-09-08T12:00:00.999Z"):
            with self.subTest(text=text):
                self.assertEqual(cli.timestamp(text), expected)
        self.assertEqual(cli.iso(expected), "2026-09-08T12:00:00Z")

    def test_timestamp_requires_valid_timezone_aware_input(self):
        for text in ("2026-09-08", "2026-09-08T12:00:00", "2026-02-30T00:00:00Z", "bad"):
            with self.subTest(text=text), self.assertRaises(argparse.ArgumentTypeError):
                cli.timestamp(text)

    def test_frames_select_numbers_by_type_not_name_or_position(self):
        response = response_frame([
            {"name": "Value", "type": "string"},
            {"name": "cpu", "type": "number", "labels": {"pod": "api"}},
            {"name": "Time", "type": "time"},
            {"name": "memory", "type": "number"},
        ], [["ignored", "ignored"], [0, None], [1788868800123, 1788868860000], [2.5, 3]])
        response["results"]["A"]["frames"].append({
            "schema": {"fields": [{"type": "time"}, {"type": "number", "labels": {"node": "a"}}]},
            "data": {"values": [[1788868800000], [7]]},
        })
        self.assertEqual(cli.frames(response), [
            {"labels": {"pod": "api"}, "samples": [[1788868800.123, 0], [1788868860, None]]},
            {"labels": {}, "samples": [[1788868800.123, 2.5], [1788868860, 3]]},
            {"labels": {"node": "a"}, "samples": [[1788868800, 7]]},
        ])

    def test_frames_empty_success_and_status_zero(self):
        for result in ({}, {"frames": []}, {"status": 0, "frames": []}):
            with self.subTest(result=result):
                self.assertEqual(cli.frames({"results": {"A": result}}), [])

    def test_genuine_empty_frame_is_valid_query_response(self):
        response = response_frame([], [])
        client = self.client()
        client.request.side_effect = None
        client.request.return_value = response
        original, series = cli.query(client, "https://grafana.example", "services-uksouth",
                                     "kube_node_info", 3600, 7200, 60)
        self.assertIs(original, response)
        self.assertEqual(series, [])
        cli.validate_grid(series, 3600, 7200, 60)

    def test_grid_accepts_exact_timestamps_with_nulls_and_empty_series(self):
        cli.validate_grid([
            {"samples": [[3600, 0], [3660, None], [3720, 1]]},
            {"samples": []},
        ], 3600, 3720, 60)

    def test_grid_accepts_contiguous_short_lived_subsets(self):
        for times in ([3600, 3660], [3660, 3720], [3600], [3660], [3720]):
            with self.subTest(times=times):
                cli.validate_grid([{"samples": [[t, 1] for t in times]}], 3600, 3720, 60)

    def test_grid_checks_executed_step_metadata(self):
        for reported, valid in (("60s", True), ("1m", True), ("0.5m30s", True), ("5m", False)):
            with self.subTest(reported=reported):
                response = response_frame([{"type": "time"}, {"type": "number"}], [[3660000], [1]])
                response["results"]["A"]["frames"][0]["schema"]["meta"] = {
                    "executedQueryString": f"Expr: up\nStep: {reported}",
                }
                series = cli.frames(response)
                if valid:
                    cli.validate_grid(series, 3600, 3720, 60, response)
                else:
                    with self.assertRaisesRegex(ValueError, "executed a different sampling step"):
                        cli.validate_grid(series, 3600, 3720, 60, response)

    def test_grid_rejects_shifted_sparse_reordered_and_extra_timestamps(self):
        for times in ([3601, 3661, 3721], [3600, 3720],
                      [3660, 3600, 3720], [3600, 3660, 3660, 3720],
                      [3540, 3600, 3660, 3720], [3720, 3780], [3600, 3660.001, 3720]):
            with self.subTest(times=times), self.assertRaisesRegex(ValueError, "different sampling grid"):
                cli.validate_grid([{"samples": [[t, None] for t in times]}], 3600, 3720, 60)

    def test_frames_reject_query_errors_and_missing_result(self):
        for response in ({}, {"results": {"B": {}}},
                         {"results": {"A": {"error": "query timeout"}}},
                         {"results": {"A": {"status": 500, "frames": []}}}):
            with self.subTest(response=response), self.assertRaisesRegex(RuntimeError, "Grafana query failed"):
                cli.frames(response)

    def test_query_accepts_sparse_ticks_only_with_confirmed_step_for_all_metrics(self):
        expressions = (
            "kube_node_info{}", "kube_node_labels{}", "kube_node_status_capacity{}",
            "kube_node_status_allocatable{}", "sum(rate(node_cpu_seconds_total{}[5m]))",
            "node_memory_MemTotal_bytes{} - node_memory_MemAvailable_bytes{}",
            "kube_pod_info{}", "kube_pod_status_phase{}", "kube_pod_owner{}",
            "kube_replicaset_owner{}", "kube_pod_container_resource_requests{}",
            "kube_pod_init_container_resource_requests{}",
            "rate(container_cpu_usage_seconds_total{}[5m])", "container_memory_working_set_bytes{}",
        )
        for expr in expressions:
            for reported in (None, "5m", "1m0s"):
                with self.subTest(expr=expr, reported=reported):
                    response = response_frame([{"type": "time"}, {"type": "number"}],
                                              [[3600000, 3720000, 3780000], [0, None, 2]])
                    if reported:
                        response["results"]["A"]["frames"][0]["schema"]["meta"] = {
                            "executedQueryString": f"Expr: {expr}\nStep: {reported}",
                        }
                    client = self.client()
                    client.request.side_effect = None
                    client.request.return_value = response
                    if reported == "1m0s":
                        original, series = cli.query(client, "https://grafana", "uid", expr, 3600, 3780, 60)
                        self.assertIs(original, response)
                        self.assertEqual(series[0]["samples"], [[3600, 0], [3720, None], [3780, 2]])
                        # Direct validation stays strict, even with matching metadata.
                        with self.assertRaisesRegex(ValueError, "different sampling grid"):
                            cli.validate_grid(series, 3600, 3780, 60, response)
                    else:
                        with self.assertRaisesRegex(ValueError, "different sampling"):
                            cli.query(client, "https://grafana", "uid", expr, 3600, 3780, 60)

    def test_sparse_query_still_rejects_offgrid_unordered_and_conflicting_steps(self):
        for times, second_step in (([3601, 3720], None), ([3600, 3780], None),
                                   ([3720, 3600], None), ([3600, 3600], None),
                                   ([3600, 3720], "5m")):
            with self.subTest(times=times, second_step=second_step):
                response = response_frame([{"type": "time"}, {"type": "number"}],
                                          [[t * 1000 for t in times], [1] * len(times)])
                response["results"]["A"]["frames"][0]["schema"]["meta"] = {
                    "executedQueryString": "Expr: up\nStep: 1m0s",
                }
                if second_step:
                    response["results"]["A"]["frames"].append({
                        "schema": {"fields": [], "meta": {"executedQueryString": f"Step: {second_step}"}},
                        "data": {"values": []},
                    })
                client = self.client()
                client.request.side_effect = None
                client.request.return_value = response
                with self.assertRaisesRegex(ValueError, "different sampling"):
                    cli.query(client, "https://grafana", "uid", "up", 3600, 3720, 60)

    def test_sparse_query_keeps_processor_gaps_unknown_and_peak_windows_strict(self):
        from observed.peak import rank_window
        from observed.test_process import bundle, first_pod, inventory, usage

        for missing in (True, False):
            with self.subTest(missing=missing):
                raw = bundle()
                labels = inventory(raw)
                usage(raw, labels)
                row = raw["queries"][-1]
                samples = row["series"][0]["samples"]
                if missing:
                    samples.pop(1)
                else:
                    samples[1] = (3900, None)
                response = response_frame([
                    {"type": "time"}, {"type": "number", "labels": row["series"][0]["labels"]},
                ], [[t * 1000 for t, _ in samples], [v for _, v in samples]])
                response["results"]["A"]["frames"][0]["schema"]["meta"] = {"executedQueryString": "Step: 5m"}
                client = self.client()
                _, row["series"] = cli.query(client, "https://grafana", "uid", "memory", 3600, 7200, 300,
                                             snapshot=response)
                view = cli.process_bundle(raw)
                self.assertIsNone(first_pod(view)["usage"]["mem_mib"])
                self.assertTrue(any("incomplete memory coverage" in error for error in view["errors"]))
                with self.assertRaisesRegex(ValueError, "No common window with full"):
                    rank_window({"prod/mc": dict(row["series"][0]["samples"])}, 3600, 300, 3600, 7200)
                client.request.assert_not_called()

    def test_frames_require_time_field(self):
        with self.assertRaisesRegex(ValueError, "no time field"):
            cli.frames(response_frame([{"name": "Time", "type": "number"}], [[123]]))

    def test_query_uses_absolute_milliseconds_for_range_and_instant(self):
        response = response_frame([{"type": "time"}, {"type": "number"}], [[7200000], [1]])
        client = self.client()
        client.request.side_effect = None
        client.request.return_value = response
        for instant in (False, True):
            with self.subTest(instant=instant):
                original, series = cli.query(client, "https://grafana.example", "services-uksouth",
                                             "kube_node_info", 3600, 7200, 60, instant=instant)
                self.assertIs(original, response)
                self.assertEqual(series, [{"labels": {}, "samples": [[7200, 1]]}])
                client.request.assert_called_with("https://grafana.example/api/ds/query", cli.GRAFANA_RESOURCE, {
                    "from": "3600000", "to": "7200000", "queries": [{
                        "refId": "A", "datasource": {"type": "prometheus", "uid": "services-uksouth"},
                        "expr": "kube_node_info", "instant": instant, "range": not instant,
                        "intervalMs": 60000, "maxDataPoints": 61,
                    }],
                })

    def test_query_errors_retain_original_response(self):
        for response, error in (({"results": {"A": {"error": "query failed"}}}, RuntimeError),
                                (response_frame([{"type": "number"}], [[1]]), ValueError)):
            with self.subTest(response=response):
                client = self.client()
                client.request.side_effect = None
                client.request.return_value = response
                with self.assertRaises(error) as raised:
                    cli.query(client, "https://grafana.example", "services-uksouth", "up", 3600, 7200, 60)
                self.assertIs(raised.exception.response, response)

    def test_kql_keeps_baseline_history_and_deletes_with_time_and_environment_scope(self):
        kql = cli.kusto_query("int", ["int-uksouth-mgmt-1", 'mc"quoted'], 3600, 7200)
        self.assertIn("let T=datetime(1970-01-01T02:00:00Z);", kql)
        self.assertIn("let S=datetime(1970-01-01T01:00:00Z);", kql)
        self.assertIn('| where environment == "int" and cluster in ("int-uksouth-mgmt-1","mc\\"quoted")', kql)
        self.assertIn("| where timestamp <= T", kql)
        self.assertIn("apiVersion == 'hypershift.openshift.io/v1beta1' and objectKind == 'HostedCluster'", kql)
        self.assertIn("or (apiVersion == 'v1' and objectKind == 'Node')", kql)
        self.assertIn("union (observations | where timestamp <= S | summarize arg_max(timestamp, *) by environment,region,cluster,objectKind,uid)", kql)
        self.assertIn("(observations | where timestamp > S)", kql)
        self.assertIn("| project environment,region,cluster,timestamp,event,uid,namespace,name,objectKind,object", kql)
        self.assertIn("| order by timestamp asc", kql)
        self.assertNotRegex(kql, r"(?i)where[^\n]*(event|delete)|\btake\b|\bnow\(|\bago\(")

    def test_kql_projects_compact_node_and_hosted_cluster_metadata(self):
        kql = cli.kusto_query("int", ["int-uksouth-mgmt-1"], 3600, 7200)
        for label in ("hypershift.openshift.io/hosted-cluster-size", "node.kubernetes.io/instance-type",
                      "kubernetes.azure.com/agentpool", "agentpool", "topology.kubernetes.io/zone"):
            self.assertIn(f"'{label}',object.metadata.labels['{label}']", kql)
        self.assertIn("'labels',bag_pack(", kql)
        self.assertNotIn("'labels',object.metadata.labels", kql)
        for field in ("spec.providerID", "status.capacity", "status.allocatable",
                      "metadata.creationTimestamp", "metadata.deletionTimestamp", "metadata.resourceVersion"):
            self.assertIn("object." + field, kql)
        for field in ("spec.unschedulable", "spec.taints", "status.conditions", "metadata.annotations"):
            self.assertNotIn("object." + field, kql)

    def test_grafana_cache_identity_and_hits_without_auth(self):
        cache = self.output_dir()
        args = ["https://grafana.example", "services-uksouth", "up", 3600, 7200, 60, False]
        response = {"results": {"A": {"frames": []}}}
        client = self.client(cache, at=7200)
        client.request.side_effect = None
        client.request.return_value = response
        cli.query(client, *args)
        cached = cli.Client(cache, at=7200)
        self.assertEqual(cli.query(cached, *args), (response, []))
        self.run.assert_not_called()
        self.opener.open.assert_not_called()
        for index, value in enumerate(("https://other.example", "other-uid", "down", 3660, 7260, 120, True)):
            changed = args.copy()
            changed[index] = value
            with self.subTest(index=index):
                cli.query(client, *changed)
        self.assertEqual(client.request.call_count, 8)
        client.at = 7260
        cli.query(client, *args)
        with patch.object(cli, "CACHE_VERSION", 2):
            cli.query(client, *args)
        self.assertEqual(client.request.call_count, 10)

    def test_cache_resource_body_and_snapshot_partition(self):
        client = self.client(self.output_dir(), at=7200)
        client.request.side_effect = None
        client.request.return_value = {"ok": True}
        for url, resource, body, at in (
                ("https://kusto/query", "audience", {"db": "one", "csl": "KQL"}, 7200),
                ("https://other/query", "audience", {"db": "one", "csl": "KQL"}, 7200),
                ("https://kusto/query", "other", {"db": "one", "csl": "KQL"}, 7200),
                ("https://kusto/query", "audience", {"db": "two", "csl": "KQL"}, 7200),
                ("https://kusto/query", "audience", {"db": "one", "csl": "Node KQL"}, 7200),
                ("https://kusto/query", "audience", {"db": "one", "csl": "KQL"}, 7260)):
            client.at = at
            client.cached(url, resource, body, validate=lambda r: r["ok"])
            client.cached(url, resource, body, validate=lambda r: r["ok"])
        self.assertEqual(client.request.call_count, 6)

    def test_failed_grafana_http200_responses_are_never_cached(self):
        for response in (
                {"results": {"A": {"error": "backend timeout"}}},
                {"error": "backend failure", "results": {"A": {}}},
                response_frame([{"type": "number"}], [[1]]),
                response_frame([{"type": "time"}, {"type": "number"}], [[3601000], [1]]),
                response_frame([{"type": "time"}, {"type": "number"}], [[3600000], []])):
            with self.subTest(response=response):
                cache = self.output_dir()
                client = cli.Client(cache)
                self.opener.open.side_effect = None
                self.opener.open.return_value.__enter__ = Mock(side_effect=lambda: io.StringIO(json.dumps(response)))
                self.opener.open.return_value.__exit__ = Mock(return_value=False)
                with patch.object(client, "token", return_value="not-persisted"):
                    for _ in range(2):
                        with self.assertRaises((ValueError, RuntimeError)):
                            cli.query(client, "https://grafana", "uid", "up", 3600, 7200, 60)
                self.assertEqual(list(cache.iterdir()), [])

    def test_kusto_partial_diagnostics_and_invalid_objects_are_not_cached(self):
        primary = {"Columns": [{"ColumnName": n} for n in ("object", "timestamp", "uid")],
                   "Rows": [[{}, "2026-09-08T12:00:00Z", "node"]]}
        diagnostic = {"Columns": [{"ColumnName": n} for n in ("Severity", "StatusCode")], "Rows": [[2, 0]]}
        for response in ({"Tables": [primary, diagnostic]}, {"error": "failed", "Tables": [primary]},
                         {"Tables": [{**primary, "Rows": [["not json", "T", "uid"]]}]}):
            with self.subTest(response=response):
                cache = self.output_dir()
                client = self.client(cache)
                client.request.side_effect = None
                client.request.return_value = response
                for _ in range(2):
                    with self.assertRaises((ValueError, RuntimeError)):
                        client.cached("https://kusto/query", "audience", {"db": "ServiceLogs", "csl": "KQL"},
                                      validate=cli.kusto_rows)
                self.assertEqual(client.request.call_count, 2)
                self.assertEqual(list(cache.iterdir()), [])

    def test_kusto_ignores_irrelevant_property_tables(self):
        row = {"object": {"kind": "Node"}, "timestamp": "2026-09-08T12:00:00Z", "uid": "node"}
        primary = {"Columns": [{"ColumnName": name} for name in row], "Rows": [list(row.values())]}
        properties = {"TableName": "@ExtendedProperties",
                      "Columns": [{"ColumnName": "Key"}, {"ColumnName": "Value"}],
                      "Rows": [["Visualization", "{}", "extra property cell"]]}
        for tables in ([properties, primary], [primary, properties]):
            with self.subTest(tables=tables):
                self.assertEqual(cli.kusto_rows({"Tables": tables}), ([row], []))
        with self.assertRaisesRegex(RuntimeError, "no HostedCluster result table"):
            cli.kusto_rows({"Tables": [properties]})

    def test_kusto_embedded_truncation_retries_without_caching_partial_rows(self):
        primary = {"Columns": [{"ColumnName": n} for n in ("object", "timestamp", "uid")],
                   "Rows": [[{}, "2026-09-08T12:00:00Z", "node"]]}
        exception = {"Exceptions": [{"code": "E_QUERY_RESULT_SET_TOO_LARGE",
                                     "message": "Query results exceeded the 64 MB limit"}]}
        for tables in ([{**primary, "Rows": [*primary["Rows"], exception]}],
                       [primary, {"Columns": [{"ColumnName": "Severity"}], "Rows": [exception]}]):
            with self.subTest(tables=tables):
                cache = self.output_dir()
                client = self.client(cache)
                response = {"Tables": tables}
                client.request.side_effect = None
                client.request.return_value = response
                for _ in range(2):
                    with self.assertRaisesRegex(RuntimeError, "Kusto returned partial results.*E_QUERY_RESULT_SET_TOO_LARGE") as raised:
                        client.cached("https://kusto/query", "audience", {"db": "ServiceLogs", "csl": "KQL"},
                                      validate=cli.kusto_rows)
                    self.assertIs(raised.exception.response, response)
                    self.assertEqual(list(cache.iterdir()), [])
                self.assertEqual(client.request.call_count, 2)

    def test_discovery_cache_validates_and_partitions_by_timestamp(self):
        for url, validate, good, bad in (
                ("https://grafana/api/datasources", cli.datasources,
                 [{"type": "prometheus", "uid": "services-uksouth"}], {"error": "failed"}),
                ("https://kusto/v1/rest/auth/metadata", cli.kusto_audience,
                 {"AzureAD": {"KustoServiceResourceId": "https://test.kusto.example", "LoginMfaRequired": True}},
                 {"error": "failed"})):
            with self.subTest(url=url):
                cache = self.output_dir()
                client = self.client(cache, at=7200)
                client.request.side_effect = None
                client.request.return_value = bad
                with self.assertRaises((ValueError, KeyError)):
                    client.cached(url, validate=validate)
                self.assertEqual(list(cache.iterdir()), [])
                client.request.return_value = good
                client.cached(url, validate=validate)
                cached = cli.Client(cache, at=7200)
                self.assertEqual(cached.cached(url, validate=validate), (good, validate(good)))
                client.at = 7260
                client.cached(url, validate=validate)
                self.assertEqual(client.request.call_count, 3)
        self.assertEqual(cli.kusto_audience(good), "https://test.kustomfa.example")
        self.run.assert_not_called()
        self.opener.open.assert_not_called()

    def test_corrupt_cache_retries_and_refresh_invalidates_old_success(self):
        cache = self.output_dir()
        client = self.client(cache)
        good = {"results": {"A": {"frames": []}}}
        client.request.side_effect = None
        client.request.return_value = good
        args = (client, "https://grafana", "uid", "up", 3600, 7200, 60)
        cli.query(*args)
        path, = cache.iterdir()
        entry = json.loads(path.read_text())
        for content in ("{broken", "null", json.dumps({**entry, "response": {"results": {"A": {"error": "failed"}}}})):
            path.write_text(content)
            cli.query(*args)
            self.assertEqual(json.loads(path.read_text())["response"], good)
        self.assertEqual(client.request.call_count, 4)
        client.refresh = True
        client.request.side_effect = RuntimeError("offline")
        with self.assertRaisesRegex(RuntimeError, "offline"):
            cli.query(*args)
        self.assertFalse(path.exists())
        client.refresh = False
        with self.assertRaisesRegex(RuntimeError, "offline"):
            cli.query(*args)

    def test_atomic_json_write_preserves_previous_file_on_failure(self):
        output = self.output_dir()
        target = output / "raw.json"
        cli.write_json(target, {"old": True})
        with self.assertRaises(ValueError):
            cli.write_json(target, {"invalid": float("nan")})
        self.assertEqual(json.loads(target.read_text()), {"old": True})
        self.assertEqual(list(output.iterdir()), [target])
        replace = Path.replace

        def check_replace(temp, destination):
            self.assertNotEqual(temp, target)
            self.assertEqual(temp.parent, target.parent)
            self.assertEqual(json.loads(target.read_text()), {"old": True})
            self.assertEqual(json.loads(temp.read_text()), {"new": True})
            return replace(temp, destination)

        with patch.object(Path, "replace", check_replace):
            cli.write_json(target, {"new": True})

    def test_mappings_override_without_mutating_defaults(self):
        defaults = {"int": "https://old.example", "prod": "https://prod.example"}
        self.assertEqual(cli.mappings(defaults, ["int=https://new.example/dashboards/"]),
                         {"int": "https://new.example", "prod": "https://prod.example"})
        self.assertEqual(defaults["int"], "https://old.example")
        for mapping in ("int", "int=http://unsafe.example", "int="):
            with self.subTest(mapping=mapping), self.assertRaisesRegex(ValueError, "mapping must be"):
                cli.mappings(defaults, [mapping])

    def test_credentials_use_existing_identity_and_cache_token(self):
        self.run.side_effect = None
        self.run.return_value = Mock(returncode=0, stdout=json.dumps({"accessToken": "fake-token"}))
        client = cli.Client()
        self.assertEqual(client.token(cli.GRAFANA_RESOURCE), "fake-token")
        self.assertEqual(client.token(cli.GRAFANA_RESOURCE), "fake-token")
        self.run.assert_called_once_with(
            ["az", "account", "get-access-token", "--resource", cli.GRAFANA_RESOURCE, "--output", "json"],
            capture_output=True, text=True)

    def test_auth_failure_does_not_login_or_expose_cli_output(self):
        self.run.side_effect = None
        self.run.return_value = Mock(returncode=1, stdout="private stdout", stderr="private stderr")
        with self.assertRaisesRegex(RuntimeError, "Azure CLI authentication failed") as raised:
            cli.Client().token(cli.GRAFANA_RESOURCE)
        self.assertNotIn("private", str(raised.exception))
        self.assertEqual(self.run.call_count, 1)

    def test_http_errors_and_retry_exhaustion(self):
        for status, attempts in ((403, 1), (429, 3), (503, 3)):
            with self.subTest(status=status):
                self.opener.open.reset_mock()
                self.opener.open.side_effect = [urllib.error.HTTPError(
                    "https://grafana.example/api/ds/query", status, "error", {}, io.BytesIO(b"query failed"))
                    for _ in range(attempts)]
                with self.assertRaisesRegex(RuntimeError, f"HTTP {status} from https://grafana.example: query failed"):
                    cli.Client().request("https://grafana.example/api/ds/query")
                self.assertEqual(self.opener.open.call_count, attempts)

    def test_connection_errors_retry_then_report_failure(self):
        for error in (urllib.error.URLError("offline"), TimeoutError()):
            with self.subTest(error=error):
                self.opener.open.reset_mock()
                self.opener.open.side_effect = error
                with self.assertRaisesRegex(RuntimeError, "Connection failed for https://grafana.example"):
                    cli.Client().request("https://grafana.example/api/ds/query")
                self.assertEqual(self.opener.open.call_count, 3)

    def test_redirect_does_not_forward_authorization(self):
        requests = []

        class RedirectTransport(cli.urllib.request.HTTPSHandler):
            def https_open(self, req):
                requests.append(req)
                headers = Message()
                headers["Location"] = "https://untrusted.example/stolen"
                response = urllib.response.addinfourl(io.BytesIO(b""), headers, req.full_url, code=302)
                response.msg = "Found"
                return response

        self.build_opener.side_effect = lambda handler: self.real_build_opener(handler, RedirectTransport())
        with patch.object(cli.Client, "token", return_value="fake-token"):
            with self.assertRaisesRegex(RuntimeError, "Refusing HTTP redirect"):
                cli.Client().request("https://grafana.example/api/ds/query", cli.GRAFANA_RESOURCE,
                                     {"queries": []})
        self.assertEqual(len(requests), 1)
        self.assertEqual(requests[0].full_url, "https://grafana.example/api/ds/query")
        self.assertEqual(requests[0].get_header("Authorization"), "Bearer fake-token")
        self.build_opener.assert_called_once()

    def test_peak_and_collect_keep_independent_window_defaults(self):
        selection = {"regions": []}
        for command, window in (("collect", 3600), ("peak", 900), ("collect", 3600)):
            with self.subTest(command=command), \
                    patch("observed.peak.search", return_value=selection) as search, \
                    patch.object(cli, "collect_regional_peak" if command == "peak" else "collect",
                                 return_value=0) as collect:
                output = self.output_dir()
                self.assertEqual(self.main(command, "--output", output), 0)
                collect.assert_called_once()
                args = collect.call_args.args[0]
                self.assertEqual(args.command, command)
                self.assertEqual(args.window, window)
                self.assertEqual(args.step, 60)
                self.assertEqual(args.output, output)
                if command == "peak":
                    search.assert_called_once_with(args)
                    self.assertEqual(args.peak, "memory")
                    self.assertEqual(args.peak_lookback, 604800)
                    self.assertIsNone(args.peak_end)
                    self.assertIsNone(args.at)
                    self.assertIs(collect.call_args.args[1], selection)
                else:
                    search.assert_not_called()
                    self.assertIsNone(args.at)
                    self.assertFalse(hasattr(args, "peak_selection"))

    def test_peak_explicit_options_select_collection_time_and_return_status(self):
        output = self.output_dir()
        selection = {"regions": []}

        def search(args):
            self.assertIsNone(args.at)
            self.assertEqual(args.window, 300)
            self.assertEqual(args.peak, "cpu")
            self.assertEqual(args.peak_end, 1788872400)
            self.assertEqual(args.output, output)
            return selection

        with patch("observed.peak.search", side_effect=search) as peak_search, \
                patch.object(cli, "collect_regional_peak", return_value=2) as collect:
            self.assertEqual(self.main("peak", "--output", output, "--window", "5m",
                                       "--peak", "cpu", "--peak-end", "2026-09-08T13:00:00Z"), 2)
            collect.assert_called_once()
            args = collect.call_args.args[0]
            peak_search.assert_called_once_with(args)
            self.assertEqual(args.window, 300)
            self.assertIsNone(args.at)
            self.assertIs(collect.call_args.args[1], selection)

    def test_peak_rejects_at_before_search_or_collection(self):
        with patch("observed.peak.search") as search, patch.object(cli, "collect_regional_peak") as collect:
            self.assertEqual(self.main("peak", "--output", self.output_dir(),
                                       "--at", "2026-09-08T12:00:00Z"), 1)
            search.assert_not_called()
            collect.assert_not_called()
        self.assertIn("peak chooses --at; use --peak-end", self.stderr.getvalue())

    def test_failed_peak_search_never_collects_even_with_allow_partial(self):
        for error in (ValueError("No common window with full management-cluster coverage"),
                      RuntimeError("Peak search incomplete")):
            with self.subTest(error=error), patch("observed.peak.search", side_effect=error) as search, \
                    patch.object(cli, "collect_regional_peak") as collect:
                output = self.output_dir()
                self.assertEqual(self.main("peak", "--output", output, "--allow-partial"), 1)
                search.assert_called_once()
                collect.assert_not_called()
                self.assertEqual(list(output.iterdir()), [])
                self.assertIn(str(error), self.stderr.getvalue())

    def test_legacy_peak_collect_preserves_metadata_through_process_cache(self):
        output, cache = self.output_dir(), self.output_dir()
        selection = {"selected_at": 1788868800, "metric": "memory", "score": 2 * 2**30,
                     "units": "bytes", "window_seconds": 900, "step_seconds": 60,
                     "search_start": 1788267600, "search_end": 1788872400,
                     "clusters": ["int/int-uksouth-mgmt-1"], "config_key": "fixture-key",
                     "config": {"metric": "memory", "search_end": 1788872400},
                     "warnings": ["telemetry-observed coverage"],
                     "queries": [{"response": {"fixture": "search response"}}],
                     "sources": [{"environment": "int", "coverage": []}]}
        metadata = {k: v for k, v in selection.items() if k not in ("queries", "sources")}
        checkpoints = {".peak-selection.json": {"schema_version": 1, "config": selection["config"]},
                       "peak-search.json": {"schema_version": 1, "selection": metadata}}

        view = view_fixture()
        view.update(start="2026-09-08T11:45:00Z", window_seconds=900)
        collect_original = cli.collect
        with patch.object(cli, "collect", wraps=collect_original) as collect, \
                patch.object(cli.Client, "request", side_effect=self.collect_fixture) as request, \
                patch.object(cli, "process_bundle", return_value=view) as processor:
            # Existing global peak raw bundles remain resumable with collect/process.
            def collect_with_metadata(args):
                for name, content in checkpoints.items():
                    cli.write_json(args.output / name, content)
                args.peak_selection = selection
                return collect(args)
            with patch.object(cli, "collect", side_effect=collect_with_metadata):
                self.assertEqual(self.main("collect", "--environment", "int", "--output", output,
                                           "--cache-dir", cache, "--window", "15m",
                                           "--at", "2026-09-08T12:00:00Z"), 0)
            collect.assert_called_once()
            self.assertGreater(request.call_count, 5)
            raw = json.loads((output / "raw.json").read_text())
            self.assertEqual((raw["at"], raw["start"], raw["window_seconds"]),
                             (selection["selected_at"], selection["selected_at"] - 900, 900))
            self.assertEqual(raw["peak_selection"], metadata)
            self.assertEqual(raw["errors"], [])
            processor.assert_called_once_with(raw)
            for name, content in checkpoints.items():
                self.assertEqual(json.loads((output / name).read_text()), content)
            for name in ("view.json", "manifest.json"):
                self.assertEqual(json.loads((output / name).read_text())["peak_selection"], metadata)

            request.reset_mock()
            request.side_effect = AssertionError("offline processing must not request")
            processor.side_effect = AssertionError("processed cache must be reused")
            for _ in range(2):
                self.assertEqual(self.main("process", output / "raw.json", "--output", output), 0)
                for name in ("view.json", "manifest.json"):
                    self.assertEqual(json.loads((output / name).read_text())["peak_selection"], metadata)
            processor.assert_called_once_with(raw)
            collect.assert_called_once()
            request.assert_not_called()
        self.run.assert_not_called()
        self.opener.open.assert_not_called()

    def regional_fixture(self):
        args = argparse.Namespace(output=self.output_dir(), cache_dir=self.output_dir(), refresh=False,
                                  environment=["int", "stg"], region=None, cluster=".*", at=None,
                                  window=900, step=60, size_settle=900, search_back=86400,
                                  kusto=[], grafana=[], workers=1, allow_partial=False)
        regions = [{"environment": env, "region": region, "selected_at": at,
                    "original_selected_at": at, "metric": "memory", "score": 123,
                    "units": "bytes", "clusters": [f"{env}/{env}-{region}-mgmt-1"],
                    "window_seconds": 900, "step_seconds": 60, "search_start": 0, "search_end": 14400}
                   for env, region, at in (("int", "uksouth", 3600), ("stg", "westus3", 7200))]
        return args, {"scope": "regional", "regions": regions, "search_end": 14400,
                      "sources": [], "warnings": []}

    def regional_collect_fixture(self, args):
        args.output.mkdir(parents=True, exist_ok=True)
        view = view_fixture()
        view.update(at=cli.iso(args.at), start=cli.iso(args.at - args.window),
                    window_seconds=args.window,
                    management_clusters=[{"id": name} for name in args.peak_selection["clusters"]])
        cli.write_json(args.output / "view.json", view)
        cli.write_json(args.output / "manifest.json", view)
        cli.write_json(args.output / "raw.json", {"peak_selection": args.peak_selection})
        return 0

    def test_regional_peak_isolated_windows_forward_once_and_mixed_time_merge(self):
        args, selection = self.regional_fixture()
        selection["regions"][0]["clusters"].append("int/int-uksouth-mgmt-2.test")
        original = deepcopy(selection)
        old_raw = {"legacy": "global raw must survive"}
        cli.write_json(args.output / "raw.json", old_raw)

        def preflight(child_args, region):
            self.assertIs(child_args, args)
            return {"warnings": [], "transitions": [{"at": t} for t in (3000, 4500)]
                    if region["environment"] == "int" else []}

        def collect(child):
            checkpoint = json.loads((args.output / "regional-manifest.json").read_text())
            self.assertIn("collecting", [r["status"] for r in checkpoint["regions"]])
            self.assertIsNot(child, args)
            self.assertIsNot(child.grafana, args.grafana)
            env, = child.environment
            region, = child.region
            self.assertEqual(child.at, 6300 if env == "int" else 7200)
            self.assertEqual(child.output, args.output / "regions" / f"{env}-{region}" / str(child.at))
            self.assertEqual(child.allow_partial, args.allow_partial)
            self.assertEqual(child.search_back, args.search_back)
            for name in child.peak_selection["clusters"]:
                name = name.removeprefix(env + "/")
                self.assertIsNotNone(re.search(child.cluster, name))
                self.assertIsNone(re.search(child.cluster, "extra" + name))
                self.assertIsNone(re.search(child.cluster, name + "extra"))
            if env == "int":
                self.assertIsNone(re.search(child.cluster, "int-uksouth-mgmt-2Xtest"))
                self.assertEqual(child.peak_selection["selected_at"], 3600)
                self.assertEqual(child.peak_selection["adjustment_seconds"], 2700)
            self.assertEqual(child.peak_selection["score"], 123)
            return self.regional_collect_fixture(child)

        with patch.object(cli, "regional_preflight", side_effect=preflight), \
                patch.object(cli, "collect", side_effect=collect) as collector:
            self.assertEqual(cli.collect_regional_peak(args, selection), 0)
        self.assertEqual(collector.call_count, 2)
        self.assertEqual(selection, original)
        self.assertIsNone(args.at)
        self.assertEqual(args.environment, ["int", "stg"])
        self.assertEqual(json.loads((args.output / "raw.json").read_text()), old_raw)
        view = json.loads((args.output / "view.json").read_text())
        self.assertIsNone(view["at"])
        self.assertIsNone(view["start"])
        self.assertIsNone(view["suggestion"])
        self.assertEqual(len(view["regional_windows"]), 2)
        self.assertEqual({mc["at"] for mc in view["management_clusters"]}, {cli.iso(6300), cli.iso(7200)})
        self.assertTrue(any("not simultaneous" in w for w in view["warnings"]))
        for mc in view["management_clusters"]:
            self.assertEqual(mc["peak_selection"]["score"], 123)

    def test_regional_failures_checkpoint_continue_and_enforce_partial_policy(self):
        for failure in ("kusto", "horizon", "collect", "metadata", "partial-view"):
            for allow_partial in (False, True):
                with self.subTest(failure=failure, allow_partial=allow_partial):
                    args, selection = self.regional_fixture()
                    args.allow_partial = allow_partial
                    cli.write_json(args.output / "view.json", {"stale": True})

                    def preflight(_, regional):
                        if regional["environment"] == "int":
                            if failure == "kusto":
                                raise RuntimeError("Kusto unavailable")
                            if failure == "horizon":
                                return {"warnings": [], "transitions": [{"at": t} for t in range(3000, 14401, 600)]}
                        return {"warnings": [], "transitions": []}

                    def collect(child):
                        if child.environment == ["int"]:
                            if failure == "collect":
                                raise RuntimeError("collection failed")
                            if failure in ("metadata", "partial-view"):
                                child.output.mkdir(parents=True, exist_ok=True)
                                view = view_fixture(["missing/stale metadata"])
                                cli.write_json(child.output / "manifest.json", view)
                                if failure == "partial-view":
                                    cli.write_json(child.output / "view.json", view)
                                return 2
                        return self.regional_collect_fixture(child)

                    with patch.object(cli, "regional_preflight", side_effect=preflight), \
                            patch.object(cli, "collect", side_effect=collect):
                        self.assertEqual(cli.collect_regional_peak(args, selection), 2)
                    manifest = json.loads((args.output / "regional-manifest.json").read_text())
                    failed, success = manifest["regions"]
                    self.assertEqual(failed["status"], "partial" if failure == "partial-view" and allow_partial else "blocked")
                    self.assertTrue(failed["errors"])
                    self.assertEqual(success["status"], "success")
                    self.assertTrue(Path(success["view"]).exists())
                    self.assertTrue(manifest["errors"])
                    self.assertEqual((args.output / "view.json").exists(), allow_partial)
                    if allow_partial:
                        view = json.loads((args.output / "view.json").read_text())
                        self.assertTrue(view["errors"])
                        self.assertEqual(len(view["management_clusters"]), 2 if failure == "partial-view" else 1)

    def test_preflight_compact_full_horizon_uid_transitions_and_offline_resume(self):
        args, selection = self.regional_fixture()
        regional = selection["regions"][0]
        label = "hypershift.openshift.io/hosted-cluster-size"

        def snapshot(tick, size, uid="hc", **overrides):
            return {"environment": "int", "region": "uksouth", "cluster": "int-uksouth-mgmt-1",
                    "timestamp": cli.iso(tick), "uid": uid, "event": "Update", "objectKind": "HostedCluster",
                    "object": {"kind": "HostedCluster", "metadata": {"labels": {label: size}}}, **overrides}

        rows = [snapshot(1200, "small"), snapshot(2400, "medium"), snapshot(2400, "medium"),
                snapshot(2460, "medium"), snapshot(4500, "large"),
                snapshot(1000, None, "unknown"), snapshot(2520, "small", "unknown"),
                snapshot(2700, "small", "new-uid"), snapshot(2760, None, "new-uid"),
                snapshot(2820, "medium", "new-uid"),
                snapshot(3000, "large", event="Deleted"), snapshot(3300, "extra-large"),
                snapshot(15000, "extra-large", "future"),
                snapshot(2900, "small", "wrong-env", environment="stg"),
                snapshot(2900, "small", "wrong-region", region="westus3"),
                snapshot(2900, "small", "wrong-mc", cluster="int-uksouth-mgmt-2")]
        # A different UID's future change must be seen before collecting the shifted window.
        rows.extend([snapshot(1200, "small", "other"), snapshot(4500, "medium", "other")])
        responses = [{"Tables": [{"Columns": [{"ColumnName": name} for name in rows[0]],
                                 "Rows": [list(row.values()) for row in reversed(rows)
                                          if (cli.timestamp(row["timestamp"]) < 1800) == baseline]}]}
                     for baseline in (True, False)]
        auth = {"AzureAD": {"KustoServiceResourceId": "https://kusto.example"}}
        with patch.object(cli.Client, "request", side_effect=[auth, *responses]) as request:
            record = cli.regional_preflight(args, regional)
        self.assertEqual(request.call_count, 3)
        self.assertEqual([(t["at"], t["uid"], t["from"], t["to"]) for t in record["transitions"]],
                         [(2400, "hc", "small", "medium"),
                          (2820, "new-uid", "small", "medium"),
                          (4500, "other", "small", "medium")])
        self.assertEqual([q["response"] for q in record["queries"]], responses)
        self.assertEqual(record["response"], {"Tables": [r["Tables"][0] for r in responses]})
        config = record["config"]
        self.assertEqual((config["start"], config["end"]), (1800, 14400))
        self.assertEqual(config["clusters"], ["int-uksouth-mgmt-1"])
        baseline, changes = [q["query"] for q in record["queries"]]
        self.assertIn("timestamp < S", baseline)
        self.assertIn("arg_max(timestamp, *)", baseline)
        self.assertIn("timestamp >= S", changes)
        for kql in (baseline, changes):
            self.assertNotIn("Node", kql)
            self.assertNotIn("resourceVersion", kql)
            self.assertNotIn("object.spec", kql)
        saved = args.output / "regions/int-uksouth/preflight.json"
        self.assertEqual(json.loads(saved.read_text()), record)
        # Snapshot provenance alone can seed a fresh cache without auth or requests.
        args.cache_dir = self.output_dir()
        with patch.object(cli.Client, "request", side_effect=AssertionError("offline")) as request:
            self.assertEqual(cli.regional_preflight(args, regional), record)
            request.assert_not_called()
        args.refresh = True
        with patch.object(cli.Client, "request", side_effect=RuntimeError("offline")):
            with self.assertRaisesRegex(RuntimeError, "offline"):
                cli.regional_preflight(args, regional)
        self.assertEqual(json.loads(saved.read_text())["error"], "offline")
        self.assertNotIn("response", json.loads(saved.read_text()))

    def test_preflight_kql_compacts_consecutive_states_before_packing_objects(self):
        args, selection = self.regional_fixture()
        response = {"Tables": [{"Columns": [{"ColumnName": name} for name in ("object", "timestamp", "uid")],
                                "Rows": []}]}
        with patch.object(cli.Client, "request", side_effect=[
                {"AzureAD": {"KustoServiceResourceId": "https://kusto.example"}}, response, response]):
            record = cli.regional_preflight(args, selection["regions"][0])
        self.assertEqual(record["transitions"], [])
        baseline, kql = [q["query"] for q in record["queries"]]
        self.assertIn('environment == "int" and region == "uksouth"', kql)
        self.assertIn('cluster in ("int-uksouth-mgmt-1")', kql)
        self.assertIn("timestamp >= S and timestamp < T", kql)
        self.assertIn("timestamp <= datetime(1970-01-01T04:00:00Z)", kql)
        self.assertIn("event=tostring(column_ifexists('event', dynamic(null)))", kql)
        self.assertIn("isDeleted=tolower(event) in ('delete', 'deleted')", kql)
        self.assertIn("| where timestamp < S", baseline)
        self.assertNotIn("timestamp >=", baseline)
        self.assertIn("| summarize arg_max(timestamp, *) by environment,region,cluster,uid", baseline)
        self.assertNotIn("serialize", baseline)
        self.assertNotIn("union", kql)
        self.assertIn("| sort by environment asc,region asc,cluster asc,uid asc,timestamp asc\n| serialize", kql)
        self.assertIn("firstUID=row_number() == 1", kql)
        for field in ("environment", "region", "cluster", "uid"):
            self.assertIn(f"{field} != prev({field})", kql)
        self.assertIn("previousSize=prev(size), previousDeleted=prev(isDeleted)", kql)
        reduction = "| where firstUID or size != previousSize or isDeleted != previousDeleted"
        self.assertIn(reduction, kql)
        self.assertLess(kql.index(reduction), kql.index("object=bag_pack("))
        self.assertEqual(kql.count("summarize"), 0)
        self.assertNotRegex(kql, r"\b(take|ago|now)\b|object\.(spec|status)|'metadata',object.metadata|'labels',object.metadata.labels")
        self.assertIn("'labels',bag_pack('hypershift.openshift.io/hosted-cluster-size',size)", kql)

    def test_preflight_only_known_resizes_across_repeated_and_unknown_samples(self):
        cases = (
            ([(2400, None, "Add"), (2460, "e2e", "Update"), (2520, "e2e", "Update")], []),
            ([(2400, "e2e", "Add"), (2460, "e2e", None), (2520, "e2e", "Update")], []),
            ([(1200, "small", "Add"), (1800, "medium", "Update"),
              (1860, "medium", "Update"), (2400, "small", "Update"), (2460, "small", "Update")],
             [(1800, "small", "medium"), (2400, "medium", "small")]),
            ([(1200, "small", "Add"), (2400, None, "Update"), (2460, "small", "Update"),
              (2520, "", "Update"), (2580, "medium", "Update")], [(2580, "small", "medium")]),
            ([(1200, "small", "Add"), (2400, "small", "Delete"), (2460, "large", "Update")], []),
            ([(1200, "small", "Deleted"), (2400, "medium", "Add")], []),
        )
        for samples, expected in cases:
            with self.subTest(samples=samples):
                args, selection = self.regional_fixture()
                columns = ("environment", "region", "cluster", "timestamp", "uid", "event", "object")
                rows = [["int", "uksouth", "int-uksouth-mgmt-1", cli.iso(tick), "hc", event,
                         {"kind": "HostedCluster", "metadata": {"labels": {
                             "hypershift.openshift.io/hosted-cluster-size": size}}}]
                        for tick, size, event in samples]
                responses = [{"Tables": [{"Columns": [{"ColumnName": name} for name in columns],
                                           "Rows": [row for row in rows if (cli.timestamp(row[3]) < 1800) == baseline]}]}
                             for baseline in (True, False)]
                with patch.object(cli.Client, "request", side_effect=[
                        {"AzureAD": {"KustoServiceResourceId": "https://kusto.example"}}, *responses]):
                    record = cli.regional_preflight(args, selection["regions"][0])
                self.assertEqual([(t["at"], t["from"], t["to"]) for t in record["transitions"]], expected)

    def test_preflight_missing_endpoint_and_rejected_response_are_retained(self):
        args, selection = self.regional_fixture()
        regional = selection["regions"][0]
        path = args.output / "regions/int-uksouth/preflight.json"
        with patch.object(cli, "KUSTOS", {}), patch.object(cli, "Client") as client:
            with self.assertRaisesRegex(ValueError, "configure --kusto"):
                cli.regional_preflight(args, regional)
            client.assert_not_called()
        self.assertIn("configure --kusto", json.loads(path.read_text())["error"])
        response = {"error": "truncated"}
        with patch.object(cli.Client, "request", side_effect=[
                {"AzureAD": {"KustoServiceResourceId": "https://kusto.example"}}, response]):
            with self.assertRaises(RuntimeError):
                cli.regional_preflight(args, regional)
        self.assertEqual(json.loads(path.read_text())["response"], response)

    def test_preflight_daily_boundaries_keep_known_resizes_and_retry_only_failed_chunk(self):
        args, selection = self.regional_fixture()
        regional = selection["regions"][0]
        start = regional["original_selected_at"] - args.window - args.size_settle
        boundary = start + 86400
        end = start + 2 * 86400 + 3600
        regional["search_end"] = end
        columns = ("environment", "region", "cluster", "timestamp", "uid", "event", "object")
        # Server-compacted runs: every chunk preserves its first row for each UID.
        samples = [
            [(start - 365 * 86400, "hc", "small")],
            [(start, "hc", "small"), (start + 60, "new", "small")],
            [(boundary, "hc", "medium"), (boundary + 60, "new", "small")],
            [(boundary + 86400, "hc", "medium"), (end, "hc", "small")],
        ]
        responses = [{"Tables": [{"Columns": [{"ColumnName": name} for name in columns], "Rows": [
            ["int", "uksouth", "int-uksouth-mgmt-1", cli.iso(tick), uid, "Update",
             {"kind": "HostedCluster", "metadata": {"labels": {
                 "hypershift.openshift.io/hosted-cluster-size": size}}}]
            for tick, uid, size in chunk]}]} for chunk in samples]
        auth = {"AzureAD": {"KustoServiceResourceId": "https://kusto.example"}}
        failure = {"error": "daily query timeout"}
        with patch.object(cli.Client, "request", side_effect=[auth, *responses[:3], failure]) as request:
            with self.assertRaises(RuntimeError):
                cli.regional_preflight(args, regional)
        self.assertEqual(request.call_count, 5)
        path = args.output / "regions/int-uksouth/preflight.json"
        failed = json.loads(path.read_text())
        self.assertEqual([q["response"] for q in failed["queries"]], [*responses[:3], failure])
        self.assertIn("error", failed["queries"][-1])
        # A fresh disk cache proves successful responses resume from preflight.json.
        args.cache_dir = self.output_dir()
        with patch.object(cli.Client, "request", return_value=responses[-1]) as request:
            record = cli.regional_preflight(args, regional)
        request.assert_called_once_with(cli.KUSTOS["int/uksouth"] + "/v1/rest/query",
                                        "https://kusto.example",
                                        {"db": "ServiceLogs", "csl": failed["queries"][-1]["query"]})
        self.assertNotIn("error", record)
        self.assertTrue(all("error" not in q for q in record["queries"]))
        self.assertEqual([(t["at"], t["uid"], t["from"], t["to"]) for t in record["transitions"]],
                         [(boundary, "hc", "small", "medium"), (end, "hc", "medium", "small")])
        baseline, *changes = [q["query"] for q in record["queries"]]
        self.assertIn(f"let S=datetime({cli.iso(start)});", baseline)
        self.assertIn("| where timestamp < S", baseline)
        self.assertNotRegex(baseline, r"timestamp >=|\b(ago|now|serialize)\b")
        self.assertEqual(len(changes), 3)
        for kql, lower, upper in zip(changes, (start, boundary, boundary + 86400),
                                     (boundary, boundary + 86400, end + 1)):
            self.assertIn(f"let S=datetime({cli.iso(lower)});", kql)
            self.assertIn(f"let T=datetime({cli.iso(upper)});", kql)
            self.assertIn("| where timestamp >= S and timestamp < T", kql)
            self.assertIn("| where firstUID or size != previousSize or isDeleted != previousDeleted", kql)
            self.assertNotIn("arg_max", kql)
            self.assertLessEqual(upper - lower, 86400)
        with patch.object(cli.Client, "request", side_effect=AssertionError("offline")) as request:
            self.assertEqual(cli.regional_preflight(args, regional), record)
            request.assert_not_called()

    def test_preflight_baseline_timeout_blocks_and_retries_without_history_floor(self):
        args, selection = self.regional_fixture()
        regional = selection["regions"][0]
        args.cache_dir = None
        auth = {"AzureAD": {"KustoServiceResourceId": "https://kusto.example"}}
        response = {"Tables": [{"Columns": [{"ColumnName": name} for name in ("object", "timestamp", "uid")],
                                 "Rows": []}]}
        with patch.object(cli.Client, "request", side_effect=[auth, TimeoutError("baseline timeout")]) as request:
            with self.assertRaisesRegex(TimeoutError, "baseline timeout"):
                cli.regional_preflight(args, regional)
        self.assertEqual(request.call_count, 2)
        path = args.output / "regions/int-uksouth/preflight.json"
        failed = json.loads(path.read_text())
        self.assertEqual(failed["queries"][0]["error"], "baseline timeout")
        self.assertNotIn("response", failed["queries"][0])
        self.assertEqual(failed["transitions"], [])
        with patch.object(cli.Client, "request", side_effect=[response, response]) as request:
            record = cli.regional_preflight(args, regional)
        self.assertEqual(request.call_count, 2)
        self.assertEqual(request.call_args_list[0].args[2]["csl"], failed["queries"][0]["query"])
        self.assertNotIn("error", record)

    def test_regional_peak_main_reuses_preflight_and_detailed_cache_at_frozen_end(self):
        args, selection = self.regional_fixture()
        selection["regions"] = selection["regions"][:1]
        regional = selection["regions"][0]
        at = regional["search_end"]
        regional.update(selected_at=at, original_selected_at=at)
        options = ("peak", "--environment", "int", "--output", args.output, "--cache-dir", args.cache_dir)
        with patch("observed.peak.search", return_value=selection) as search, \
                patch.object(cli.Client, "request", side_effect=self.collect_fixture) as request, \
                patch.object(cli, "process_bundle", return_value=view_fixture()) as processor:
            self.assertEqual(self.main(*options), 0)
            calls = [c for c in request.call_args_list if c.args[0].endswith("/v1/rest/query")]
            self.assertEqual(len(calls), 3)
            self.assertNotIn("Node", calls[0].args[2]["csl"])
            self.assertNotIn("Node", calls[1].args[2]["csl"])
            self.assertIn("Node", calls[2].args[2]["csl"])
            output = args.output / "regions/int-uksouth" / str(at)
            raw = json.loads((output / "raw.json").read_text())
            self.assertEqual(raw["at"], at)
            self.assertEqual(raw["peak_selection"]["adjustment_seconds"], 0)
            self.assertEqual(raw["collection_config"]["regions"], ["uksouth"])
            self.assertEqual(len(raw["node_snapshots"]), 1)
            request.reset_mock()
            request.side_effect = AssertionError("cached resume must not request")
            processor.side_effect = AssertionError("processed view must be cached")
            with patch.object(cli.time, "time", return_value=999999):
                self.assertEqual(self.main(*options), 0)
            request.assert_not_called()
            processor.assert_called_once()
            self.assertEqual(search.call_count, 2)
        self.assertFalse((args.output / "raw.json").exists())
        manifest = json.loads((args.output / "regional-manifest.json").read_text())
        self.assertEqual(manifest["regions"][0]["at"], cli.iso(at))
        self.assertEqual(manifest["regions"][0]["status"], "success")

    def test_collect_region_filter_skips_unrelated_datasources_and_pins_resume_scope(self):
        output, cache = self.output_dir(), self.output_dir()
        options = ("collect", "--environment", "int", "--output", output, "--cache-dir", cache)

        def request(url, resource=None, body=None):
            if url.endswith("/api/datasources"):
                return [{"type": "prometheus", "uid": uid} for uid in (
                    "services-int-uksouth", "hcps-int-uksouth", "services-westus3", "hcps-westus3")]
            if url.endswith("/api/ds/query"):
                self.assertIn("uksouth", body["queries"][0]["datasource"]["uid"])
            return self.collect_fixture(url, resource, body)

        with patch.object(cli.Client, "request", side_effect=request), \
                patch.object(cli, "process_bundle", return_value=view_fixture()):
            self.assertEqual(self.main(*options, "--at", "2026-09-08T12:00:00Z",
                                       "--region", "uksouth", "--region", "uksouth"), 0)
        raw = json.loads((output / "raw.json").read_text())
        self.assertEqual(raw["collection_config"]["regions"], ["uksouth"])
        self.assertEqual({q["region"] for q in raw["queries"]}, {"uksouth"})
        self.assertEqual(len(raw["kusto_queries"]), 1)
        with patch.object(cli.Client, "request", side_effect=AssertionError("cached")) as request:
            self.assertEqual(self.main(*options, "--region", "uksouth"), 0)
            self.assertEqual(self.main(*options), 1)
            self.assertEqual(self.main(*options, "--region", "westus3"), 1)
            request.assert_not_called()

    def test_collect_rejects_invalid_time_constraints_before_client_use(self):
        for options, message in ((["--at", "2026-09-08T12:00:01Z"], "must align"),
                                 (["--window", "61s"], "multiple of step"),
                                 (["--window", "30s"], "multiple of step"),
                                 (["--search-back", "30m"], "at least window")):
            with self.subTest(options=options), patch.object(cli, "Client") as client:
                self.assertEqual(self.main("collect", "--output", self.output_dir(), *options), 1)
                self.assertIn(message, self.stderr.getvalue())
                client.assert_not_called()

    def test_collect_rejects_nonempty_output_without_reusing_view(self):
        output = self.output_dir()
        (output / "view.json").write_text("old view")
        with patch.object(cli, "Client") as client:
            self.assertEqual(self.main("collect", "--output", output), 1)
            client.assert_not_called()
        self.assertEqual((output / "view.json").read_text(), "old view")
        self.assertIn("output directory must be empty", self.stderr.getvalue())

    def collect_fixture(self, url, resource=None, body=None):
        if url.endswith("/api/datasources"):
            return [{"type": "prometheus", "uid": uid} for uid in ("services-uksouth", "hcps-uksouth")]
        if url.endswith("/api/ds/query"):
            if body["queries"][0]["expr"].startswith("kube_node_info"):
                return response_frame([
                    {"type": "time"}, {"type": "number", "labels": {"cluster": "int-uksouth-mgmt-1"}},
                ], [[int(body["to"])], [1]])
            return {"results": {"A": {"frames": []}}}
        if url.endswith("/v1/rest/auth/metadata"):
            return {"AzureAD": {"KustoServiceResourceId": "https://kusto.example"}}
        if url.endswith("/v1/rest/query"):
            return {"Tables": [{"Columns": [{"ColumnName": name} for name in ("object", "timestamp", "uid")],
                                "Rows": [[{"kind": "Node"}, "2026-09-08T12:00:00Z", "node"]]}]}
        self.fail(f"unexpected request {url}")

    def test_resume_and_cross_output_cache_need_no_credentials(self):
        output, cache = self.output_dir(), self.output_dir()
        options = ("collect", "--environment", "int", "--cache-dir", cache)
        with patch.object(cli.Client, "request", side_effect=self.collect_fixture) as request, \
                patch.object(cli, "process_bundle", return_value=view_fixture()) as processor:
            self.assertEqual(self.main(*options, "--output", output, "--at", "2026-09-08T12:00:00Z"), 0)
            raw = json.loads((output / "raw.json").read_text())
            self.assertGreater(request.call_count, 5)
            request.side_effect = AssertionError("cached run must not request")
            request.reset_mock()
            with patch.object(cli.time, "time", return_value=1789999999):
                self.assertEqual(self.main(*options, "--output", output), 0)
            resumed = json.loads((output / "raw.json").read_text())
            self.assertEqual(resumed["at"], raw["at"])
            self.assertEqual(resumed["collection_config"], raw["collection_config"])
            self.assertEqual(resumed["node_snapshots"], raw["node_snapshots"])
            self.assertEqual(resumed["errors"], [])
            processor.assert_called_once()
            other = self.output_dir()
            self.assertEqual(self.main(*options, "--output", other, "--at", "2026-09-08T12:00:00Z"), 0)
            self.assertEqual(processor.call_count, 2)
            request.assert_not_called()
        self.run.assert_not_called()
        self.opener.open.assert_not_called()
        manifest = json.loads((output / "manifest.json").read_text())
        self.assertEqual(manifest["collection_config"], raw["collection_config"])

    def test_legacy_raw_reuses_metrics_and_fetches_new_node_kql(self):
        output, cache = self.output_dir(), self.output_dir()
        options = ("collect", "--environment", "int", "--output", output)
        with patch.object(cli.Client, "request", side_effect=self.collect_fixture), \
                patch.object(cli, "process_bundle", return_value=view_fixture()):
            self.assertEqual(self.main(*options, "--cache-dir", cache, "--at", "2026-09-08T12:00:00Z"), 0)
        raw = json.loads((output / "raw.json").read_text())
        raw.pop("collection_config")
        raw.pop("node_snapshots")
        raw["errors"] = ["Collection did not finish; raw checkpoint is incomplete", "old error"]
        raw["kusto_queries"][0]["query"] = "legacy HostedCluster-only KQL"
        for row in raw["queries"]:
            row.pop("url")
            row.pop("instant")
        (output / "raw.json").write_text(json.dumps(raw))
        (output / "manifest.json").unlink()
        with patch.object(cli.Client, "request", side_effect=self.collect_fixture) as request, \
                patch.object(cli, "process_bundle", return_value=view_fixture()):
            self.assertEqual(self.main(*options, "--cache-dir", self.output_dir()), 0)
        self.assertEqual([call.args[0] for call in request.call_args_list], [
            cli.KUSTOS["int/uksouth"] + "/v1/rest/auth/metadata", cli.KUSTOS["int/uksouth"] + "/v1/rest/query"])
        resumed = json.loads((output / "raw.json").read_text())
        self.assertEqual(resumed["errors"], [])
        self.assertEqual(len(resumed["node_snapshots"]), 1)
        self.assertEqual(len(resumed["queries"]), len(raw["queries"]))

    def test_resume_revalidates_formerly_rejected_sparse_responses_without_requests(self):
        output, cache = self.output_dir(), self.output_dir()
        options = ("collect", "--environment", "int", "--output", output, "--cache-dir", cache)
        with patch.object(cli.Client, "request", side_effect=self.collect_fixture), \
                patch.object(cli, "process_bundle", return_value=view_fixture()):
            self.assertEqual(self.main(*options, "--at", "2026-09-08T12:00:00Z"), 0)
        raw = json.loads((output / "raw.json").read_text())
        for row in raw["queries"]:
            response = response_frame([
                {"type": "time"}, {"type": "number", "labels": {"cluster": "int-uksouth-mgmt-1"}},
            ], [[raw["start"] * 1000, (raw["start"] + 120) * 1000], [1, 1]])
            response["results"]["A"]["frames"][0]["schema"]["meta"] = {"executedQueryString": "Step: 1m0s"}
            row.update(response=response, series=[], error="Grafana returned a different sampling grid")
        cli.write_json(output / "raw.json", raw)
        with patch.object(cli.Client, "request", side_effect=AssertionError("must reuse original responses")) as request, \
                patch.object(cli, "publish", return_value=2):
            for _ in range(2):
                self.assertEqual(self.main(*options), 2)
                resumed = json.loads((output / "raw.json").read_text())
                self.assertEqual(len(resumed["queries"]), len(raw["queries"]))
                for row in resumed["queries"]:
                    self.assertIn("interior sampling gaps", row["error"])
                    self.assertEqual(row["series"], cli.frames(row["response"]))
                    self.assertTrue(row["series"])
                view = cli.process_bundle({**resumed, "node_snapshots": []})
                self.assertTrue(any("node_cpu: Query has interior sampling gaps" in e for e in view["errors"]))
                diagnostics = [*view["errors"], *view["warnings"],
                               *(w for mc in view["management_clusters"] for w in mc["warnings"])]
                self.assertTrue(any("node_labels: Query has interior sampling gaps" in e for e in diagnostics))
            request.assert_not_called()

    def test_collect_brazil_ksm_gaps_use_replica_union_per_identity(self):
        metrics = {"node_info", "node_labels", "capacity", "allocatable", "pod_info",
                   "pod_phase", "pod_owner", "replicaset_owner", "requests", "init_requests"}
        cases = (
            ("sparse covered", [(0, 1), (120, 1)], [(0, 1), (60, 0), (120, 1)], {}, set()),
            ("null covered", [(0, 1), (60, None), (120, 1)], [(60, 0)], {}, set()),
            ("complementary", [(0, 1), (120, 1)], [(60, 1), (180, 1)], {}, set()),
            ("uncovered", [(0, 1), (120, 1)], [(0, 1), (120, 1)], {}, metrics),
            ("null hole", [(0, 1), (60, None), (120, 1)], [(60, None)], {}, metrics),
            ("nonfinite hole", [(0, 1), (60, "NaN"), (120, 1)], [(60, "Inf")], {}, metrics),
            ("short lifetime", [(0, None), (60, 0), (120, None)], [], {}, set()),
            ("other cluster", [(0, 1), (120, 1)], [(60, 1)], {"cluster": "prod-brazilsouth-mgmt-2"}, metrics),
            ("other node", [(0, 1), (120, 1)], [(60, 1)], {"node": "other"},
             {"node_info", "node_labels", "capacity", "allocatable", "pod_info", "requests", "init_requests"}),
            ("other resource", [(0, 1), (120, 1)], [(60, 1)], {"resource": "memory"},
             {"capacity", "allocatable", "requests", "init_requests"}),
            ("other uid", [(0, 1), (120, 1)], [(60, 1)], {"uid": "other"},
             {"pod_info", "pod_phase", "pod_owner", "requests", "init_requests"}),
        )
        for name, first, second, overrides, gaps in cases:
            with self.subTest(case=name):
                output, cache = self.output_dir(), self.output_dir()

                def request(url, resource=None, body=None):
                    if url.endswith("/api/datasources"):
                        return [{"type": "prometheus", "uid": prefix + "brazilsouth"}
                                for prefix in ("services-", "hcps-")]
                    if url.endswith("/api/ds/query") and body["queries"][0]["expr"].startswith("kube_"):
                        labels = {"cluster": "prod-brazilsouth-mgmt-1", "node": "worker",
                                  "namespace": "kube-system", "pod": "controller", "uid": "pod-uid",
                                  "container": "main", "resource": "cpu", "phase": "Running",
                                  "replicaset": "controller-rs", "owner_kind": "Deployment", "owner_name": "controller"}
                        response = {"results": {"A": {"frames": []}}}
                        for replica, samples in enumerate((first, second)):
                            scrape = {key: str(replica) for key in (
                                "job", "instance", "prometheus", "prometheus_replica", "endpoint", "service", "__name__")}
                            frame = response_frame([
                                {"type": "time"}, {"type": "number", "labels": {
                                    **labels, **scrape, **(overrides if replica else {})}},
                            ], [[int(body["from"]) + t * 1000 for t, _ in samples], [v for _, v in samples]])
                            frame["results"]["A"]["frames"][0]["schema"]["meta"] = {"executedQueryString": "Step: 1m"}
                            response["results"]["A"]["frames"].extend(frame["results"]["A"]["frames"])
                        return response
                    return self.collect_fixture(url, resource, body)

                with patch.object(cli.Client, "request", side_effect=request), \
                        patch.object(cli, "publish", return_value=0):
                    self.assertEqual(self.main("collect", "--environment", "prod", "--region", "brazilsouth",
                                               "--output", output, "--cache-dir", cache,
                                               "--at", "2026-09-08T12:00:00Z"), 0)
                raw = json.loads((output / "raw.json").read_text())
                self.assertEqual(raw["errors"], [])
                self.assertEqual({q["metric"] for q in raw["queries"]} & metrics, metrics)
                for row in raw["queries"]:
                    if row["metric"] in gaps:
                        self.assertIn("interior sampling gaps", row["error"])
                    else:
                        self.assertNotIn("error", row)
                    self.assertEqual(row["series"], cli.frames(row["response"]))

    def test_collect_keeps_http_failures_as_row_errors(self):
        output = self.output_dir()
        original_request = cli.Client.request

        def request(url, resource=None, body=None):
            if url.endswith("/api/ds/query") and body["queries"][0]["expr"].startswith("kube_node_status_capacity"):
                return original_request(cli.Client(), url)
            return self.collect_fixture(url, resource, body)

        self.opener.open.side_effect = urllib.error.HTTPError(
            "https://grafana.example/api/ds/query", 403, "error", {}, io.BytesIO(b"forbidden"))
        # Mock cached transport dispatch, but exercise real HTTP error handling for capacity.
        with patch.object(cli.Client, "request", side_effect=request), \
                patch.object(cli, "publish", return_value=2):
            self.assertEqual(self.main("collect", "--environment", "int", "--output", output,
                                       "--cache-dir", self.output_dir(), "--at", "2026-09-08T12:00:00Z"), 2)
        raw = json.loads((output / "raw.json").read_text())
        row = next(q for q in raw["queries"] if q["metric"] == "capacity")
        self.assertIn("HTTP 403", row["error"])
        self.assertEqual(row["series"], [])

    def test_resume_raw_metric_match_includes_full_scope_and_query(self):
        output, cache = self.output_dir(), self.output_dir()
        options = ("collect", "--environment", "int", "--output", output)
        with patch.object(cli.Client, "request", side_effect=self.collect_fixture), \
                patch.object(cli, "process_bundle", return_value=view_fixture()):
            self.assertEqual(self.main(*options, "--cache-dir", cache, "--at", "2026-09-08T12:00:00Z"), 0)
        original = json.loads((output / "raw.json").read_text())
        for field, value in (("environment", "prod"), ("region", "westus3"), ("cluster", "other"),
                             ("datasource", "other"), ("expression", "other"), ("start", 0),
                             ("end", 0), ("step", 120), ("url", "https://other"), ("instant", True),
                             ("response", {"results": {"A": {"error": "invalid old result"}}})):
            with self.subTest(field=field):
                raw = json.loads(json.dumps(original))
                next(q for q in raw["queries"] if q["metric"] == "capacity")[field] = value
                (output / "raw.json").write_text(json.dumps(raw))
                with patch.object(cli.Client, "request", side_effect=self.collect_fixture) as request, \
                        patch.object(cli, "process_bundle", return_value=view_fixture()):
                    self.assertEqual(self.main(*options, "--cache-dir", self.output_dir()), 0)
                metric_calls = [c for c in request.call_args_list if c.args[0].endswith("/api/ds/query")]
                self.assertEqual(len(metric_calls), 1)
                self.assertIn("kube_node_status_capacity", metric_calls[0].args[2]["queries"][0]["expr"])

    def test_prior_raw_takes_precedence_over_shared_cache_and_seeds_it(self):
        cache = self.output_dir()
        client = self.client(cache)
        older = response_frame([{"type": "time"}, {"type": "number"}], [[7200000], [1]])
        newer = response_frame([{"type": "time"}, {"type": "number"}], [[7200000], [2]])
        client.request.side_effect = None
        client.request.return_value = newer
        args = ("https://grafana", "uid", "up", 3600, 7200, 60)
        cli.query(client, *args)
        self.assertEqual(cli.query(client, *args, snapshot=older)[0], older)
        self.assertEqual(cli.query(cli.Client(cache), *args)[0], older)
        client.request.assert_called_once()

    def test_resume_rejects_changed_scope_or_time_before_overwriting(self):
        output, cache = self.output_dir(), self.output_dir()
        options = ("collect", "--environment", "int", "--output", output, "--cache-dir", cache)
        with patch.object(cli.Client, "request", side_effect=self.collect_fixture), \
                patch.object(cli, "process_bundle", return_value=view_fixture()):
            self.assertEqual(self.main(*options, "--at", "2026-09-08T12:00:00Z"), 0)
        original = {p.name: p.read_text() for p in output.iterdir()}
        for extra in (("--at", "2026-09-08T12:01:00Z"), ("--window", "2h"), ("--step", "2m"),
                      ("--search-back", "2d"), ("--size-settle", "30m"), ("--environment", "prod"),
                      ("--cluster", "other"), ("--grafana", "int=https://other.example")):
            with self.subTest(extra=extra), patch.object(cli, "Client") as client:
                self.assertEqual(self.main(*options, *extra), 1)
                client.assert_not_called()
                self.assertEqual({p.name: p.read_text() for p in output.iterdir()}, original)

    def test_resume_changed_kusto_map_and_kql_rebuild_snapshots_only(self):
        output, cache = self.output_dir(), self.output_dir()
        options = ("collect", "--environment", "int", "--output", output, "--cache-dir", cache)
        with patch.object(cli.Client, "request", side_effect=self.collect_fixture) as request, \
                patch.object(cli, "process_bundle", return_value=view_fixture()):
            self.assertEqual(self.main(*options, "--at", "2026-09-08T12:00:00Z"), 0)
            request.reset_mock()
            override = ("--kusto", "int/uksouth=https://other.kusto.example")
            self.assertEqual(self.main(*options, *override), 0)
            self.assertEqual([c.args[0] for c in request.call_args_list], [
                "https://other.kusto.example/v1/rest/auth/metadata", "https://other.kusto.example/v1/rest/query"])
            request.reset_mock()
            with patch.object(cli, "kusto_query", return_value="new Node query"):
                self.assertEqual(self.main(*options, *override), 0)
            request.assert_called_once_with("https://other.kusto.example/v1/rest/query", "https://kusto.example",
                                            {"db": "ServiceLogs", "csl": "new Node query"})
        raw = json.loads((output / "raw.json").read_text())
        self.assertEqual(len(raw["node_snapshots"]), 1)
        self.assertEqual(len(raw["kusto_queries"]), 1)

    def test_failed_raw_metrics_retry_and_refresh_bypasses_all_reuse(self):
        output, cache = self.output_dir(), self.output_dir()
        options = ("collect", "--environment", "int", "--output", output, "--cache-dir", cache)

        def fail_capacity(url, resource=None, body=None):
            if body and "queries" in body and body["queries"][0]["expr"].startswith("kube_node_status_capacity"):
                return {"results": {"A": {"error": "capacity failed"}}}
            return self.collect_fixture(url, resource, body)

        with patch.object(cli.Client, "request", side_effect=fail_capacity) as request, \
                patch.object(cli, "process_bundle", return_value=view_fixture()):
            self.assertEqual(self.main(*options, "--at", "2026-09-08T12:00:00Z"), 0)
            total = request.call_count
            raw = json.loads((output / "raw.json").read_text())
            self.assertEqual(sum("error" in q for q in raw["queries"]), 1)
            request.reset_mock()
            request.side_effect = self.collect_fixture
            self.assertEqual(self.main(*options), 0)
            self.assertEqual(request.call_count, 1)
            self.assertIn("kube_node_status_capacity", request.call_args.args[2]["queries"][0]["expr"])
            raw = json.loads((output / "raw.json").read_text())
            self.assertFalse(any("error" in q for q in raw["queries"]))
            request.reset_mock()
            self.assertEqual(self.main(*options, "--refresh"), 0)
            self.assertEqual(request.call_count, total)

    def test_interrupted_resume_removes_stale_publication_and_keeps_checkpoint(self):
        output, cache = self.output_dir(), self.output_dir()
        options = ("collect", "--environment", "int", "--output", output, "--cache-dir", cache)
        with patch.object(cli.Client, "request", side_effect=self.collect_fixture), \
                patch.object(cli, "process_bundle", return_value=view_fixture()):
            self.assertEqual(self.main(*options, "--at", "2026-09-08T12:00:00Z"), 0)
        with patch.object(cli.Client, "request", side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                self.main(*options, "--refresh")
        self.assertFalse((output / "view.json").exists())
        self.assertFalse((output / "manifest.json").exists())
        raw = json.loads((output / "raw.json").read_text())
        self.assertEqual(raw["errors"], ["Collection did not finish; raw checkpoint is incomplete"])
        with patch.object(cli.Client, "request", side_effect=self.collect_fixture), \
                patch.object(cli, "process_bundle", return_value=view_fixture()):
            self.assertEqual(self.main(*options), 0)
        self.assertEqual(json.loads((output / "raw.json").read_text())["errors"], [])

    def test_kusto_checkpoints_each_region_including_failures(self):
        for failed in (False, True):
            with self.subTest(failed=failed):
                output, cache = self.output_dir(), self.output_dir()

                def request(url, resource=None, body=None):
                    if url.endswith("/api/datasources"):
                        return [{"type": "prometheus", "uid": prefix + region}
                                for region in ("uksouth", "westus3") for prefix in ("services-", "hcps-")]
                    if url == "https://westus3.kusto.example/v1/rest/auth/metadata":
                        checkpoint = json.loads((output / "raw.json").read_text())
                        record, = checkpoint["kusto_queries"]
                        self.assertEqual(record["region"], "uksouth")
                        self.assertEqual("error" in record, failed)
                        self.assertEqual(len(checkpoint["node_snapshots"]), 0 if failed else 1)
                        raise KeyboardInterrupt
                    if failed and url.endswith("/v1/rest/query"):
                        raise RuntimeError("HTTP 403 fixture")
                    return self.collect_fixture(url, resource, body)

                with patch.object(cli.Client, "request", side_effect=request):
                    with self.assertRaises(KeyboardInterrupt):
                        self.main("collect", "--environment", "int", "--output", output, "--cache-dir", cache,
                                  "--at", "2026-09-08T12:00:00Z",
                                  "--kusto", "int/westus3=https://westus3.kusto.example")

    def test_collect_default_endpoints_and_aligned_default_time_retain_errors(self):
        output = self.output_dir()
        client = self.client()
        client.request.side_effect = RuntimeError("fixture discovery failure")
        with patch.object(cli, "Client", return_value=client), patch.object(cli.time, "time", return_value=1788869137):
            self.assertEqual(self.main("collect", "--output", output), 2)
        self.assertEqual({call.args[0] for call in client.request.call_args_list},
                         {base + "/api/datasources" for base in cli.GRAFANAS.values()})
        raw = json.loads((output / "raw.json").read_text())
        self.assertEqual((raw["at"], raw["start"], raw["step_seconds"]), (1788868800, 1788865200, 60))
        self.assertEqual(raw["search_start"], raw["at"] - 86400)
        self.assertEqual(raw["settle_seconds"], 900)
        self.assertEqual(len(raw["errors"]), 3)
        self.assertTrue(all("fixture discovery failure" in error for error in raw["errors"]))
        self.assertTrue(json.loads((output / "manifest.json").read_text())["partial"])
        self.assertFalse((output / "view.json").exists())

    def test_successful_collect_fixture_persists_raw_manifest_and_view(self):
        output = self.output_dir()
        node_response = response_frame([
            {"type": "time"}, {"type": "number", "labels": {"cluster": "int-uksouth-mgmt-1", "node": "a"}},
        ], [list(range(1788865200000, 1788868800001, 60000)), [1] * 61])

        def request(url, resource=None, body=None):
            if url == cli.GRAFANAS["int"] + "/api/datasources":
                return [{"type": "prometheus", "uid": uid} for uid in ("services-uksouth", "hcps-uksouth")]
            if url == cli.GRAFANAS["int"] + "/api/ds/query":
                self.assertEqual((body["from"], body["to"]), ("1788865200000", "1788868800000"))
                return node_response if body["queries"][0]["expr"].startswith("kube_node_info") else {"results": {"A": {"frames": []}}}
            if url == cli.KUSTOS["int/uksouth"] + "/v1/rest/auth/metadata":
                return {"AzureAD": {"KustoServiceResourceId": "https://kusto.example"}}
            if url == cli.KUSTOS["int/uksouth"] + "/v1/rest/query":
                self.assertEqual(resource, "https://kusto.example")
                self.assertEqual(body["db"], "ServiceLogs")
                self.assertEqual(body["csl"], cli.kusto_query("int", ["int-uksouth-mgmt-1"], 1788782400, 1788868800))
                return {"Tables": [{"Columns": [{"ColumnName": name} for name in ("object", "timestamp", "uid", "event")],
                                    "Rows": [[json.dumps({"metadata": {"name": "fixture"}}), "2026-09-08T11:00:00Z", "hcp", "Delete"]]}]}
            self.fail(f"unexpected request: {url}")

        client = self.client()
        client.request.side_effect = request
        view = view_fixture()
        with patch.object(cli, "Client", return_value=client), patch.object(cli, "process_bundle", return_value=view) as processor:
            self.assertEqual(self.main("collect", "--environment", "int", "--cluster", "^int-uksouth-mgmt-1$",
                                       "--at", view["at"], "--window", "1h", "--step", "1m", "--output", output), 0)
        raw = json.loads((output / "raw.json").read_text())
        processor.assert_called_once_with(raw)
        self.assertEqual(raw["errors"], [])
        self.assertTrue(all("error" not in row for row in raw["queries"]))
        self.assertEqual(raw["queries"][0]["response"], node_response)
        self.assertEqual(raw["snapshots"][0]["object"], {"metadata": {"name": "fixture"}})
        self.assertEqual(raw["snapshots"][0]["event"], "Delete")
        self.assertEqual(json.loads((output / "view.json").read_text()), view)
        self.assertFalse(json.loads((output / "manifest.json").read_text())["partial"])

    def test_successful_collect_separates_node_and_hosted_cluster_snapshots(self):
        output = self.output_dir()
        snapshots = []
        node_snapshots = []
        tables = []
        for kind_field in ("objectKind", "kind"):
            rows = []
            for kind, destination in (("Node", node_snapshots), ("HostedCluster", snapshots)):
                row = {"object": {"metadata": {"name": kind.lower()}},
                       "timestamp": "2026-09-08T11:00:00Z", "uid": "shared-uid", "event": "Delete"}
                if kind_field == "objectKind":
                    row["objectKind"] = kind
                else:
                    row["object"]["kind"] = kind
                destination.append(row)
                rows.append({**row, "object": json.dumps(row["object"])})
            tables.append({"Columns": [{"ColumnName": name} for name in rows[0]],
                           "Rows": [list(row.values()) for row in rows]})
        response = {"Tables": tables}
        client = self.client()
        client.request.side_effect = [
            [{"type": "prometheus", "uid": uid} for uid in ("services-uksouth", "hcps-uksouth")],
            {"AzureAD": {"KustoServiceResourceId": "https://kusto.example"}}, response,
        ]
        series = [{"labels": {"cluster": "int-uksouth-mgmt-1"}, "samples": [[1788868800, 1]]}]
        with patch.object(cli, "Client", return_value=client), \
                patch.object(cli, "query", return_value=({}, series)), \
                patch.object(cli, "process_bundle", return_value=view_fixture()) as processor:
            self.assertEqual(self.main("collect", "--environment", "int", "--at",
                                       "2026-09-08T12:00:00Z", "--output", output), 0)
        raw = json.loads((output / "raw.json").read_text())
        processor.assert_called_once_with(raw)
        self.assertEqual(raw["errors"], [])
        self.assertEqual(raw["snapshots"], snapshots)
        self.assertEqual(raw["node_snapshots"], node_snapshots)
        self.assertEqual(raw["kusto_queries"][0]["response"], response)

    def test_capture_keeps_rejected_response_and_blocks_publication(self):
        coarser_response = response_frame([
            {"type": "time"}, {"type": "number", "labels": {"cluster": "int-westus3-mgmt-1"}},
        ], [[1788868800000], [1]])
        coarser_response["results"]["A"]["frames"][0]["schema"]["meta"] = {
            "executedQueryString": "Expr: kube_node_info\nStep: 5m",
        }
        for response, message in (
            ({"results": {"A": {"error": "fixture query failure"}}}, "fixture query failure"),
            (response_frame([{"type": "number"}], [[1]]), "no time field"),
            (response_frame([
                {"type": "time"}, {"type": "number", "labels": {"cluster": "int-westus3-mgmt-1"}},
            ], [[1788865200000, 1788865320000], [1, 1]]), "different sampling grid"),
            (coarser_response, "executed a different sampling step"),
        ):
            with self.subTest(message=message):
                output = self.output_dir()
                client = self.client()
                client.request.side_effect = lambda url, *args: (
                    [{"type": "prometheus", "uid": "services-westus3"}]
                    if url.endswith("/api/datasources") else response)
                with patch.object(cli, "Client", return_value=client):
                    self.assertEqual(self.main("collect", "--environment", "int", "--at",
                                               "2026-09-08T12:00:00Z", "--output", output), 2)
                raw = json.loads((output / "raw.json").read_text())
                self.assertEqual(raw["queries"][0]["response"], response)
                self.assertIn(message, raw["queries"][0]["error"])
                manifest = json.loads((output / "manifest.json").read_text())
                self.assertTrue(any(message in error for error in manifest["errors"]))
                self.assertFalse((output / "view.json").exists())

    def test_kusto_primary_table_and_diagnostic_severity(self):
        primary = {"Columns": [{"ColumnName": name} for name in ("object", "timestamp", "uid")], "Rows": []}
        for tables, error, warning in (
            ([primary], None, False),
            ([{"Columns": [{"ColumnName": "unrelated"}], "Rows": []}], "no HostedCluster result table", False),
            ([primary, {"Columns": [{"ColumnName": name} for name in ("Severity", "StatusCode", "StatusDescription")],
                        "Rows": [[3, 0, "fixture warning"]]}], None, True),
            ([primary, {"Columns": [{"ColumnName": name} for name in ("Severity", "StatusCode", "StatusDescription")],
                        "Rows": [[2, 0, "fixture error"]]}], "Kusto query incomplete", False),
            ([primary, {"Columns": [{"ColumnName": name} for name in ("Severity", "StatusCode", "StatusDescription")],
                        "Rows": [[3, 1, "fixture error"]]}], "Kusto query incomplete", False),
        ):
            with self.subTest(tables=tables):
                output = self.output_dir()
                response = {"Tables": tables}
                client = self.client()
                client.request.side_effect = [
                    [{"type": "prometheus", "uid": uid} for uid in ("services-uksouth", "hcps-uksouth")],
                    {"AzureAD": {"KustoServiceResourceId": "https://kusto.example"}}, response,
                ]

                def query(client, base, uid, expr, start, at, step, instant=False):
                    series = [{"labels": {"cluster": "int-uksouth-mgmt-1"},
                               "samples": [[t, 1] for t in range(start, at + 1, step)]}]
                    return {}, series if expr.startswith("kube_node_info") else []

                with patch.object(cli, "Client", return_value=client), patch.object(cli, "query", side_effect=query), \
                        patch.object(cli, "publish", return_value=0) as publish:
                    self.assertEqual(self.main("collect", "--environment", "int", "--at",
                                               "2026-09-08T12:00:00Z", "--output", output), 0)
                raw = json.loads((output / "raw.json").read_text())
                publish.assert_called_once_with(raw, output, False)
                self.assertEqual(raw["kusto_queries"][0]["response"], response)
                if error:
                    self.assertIn(error, raw["kusto_queries"][0]["error"])
                    self.assertTrue(any(error in message for message in raw["errors"]))
                else:
                    self.assertNotIn("error", raw["kusto_queries"][0])
                    self.assertEqual(raw["errors"], [])
                self.assertEqual(any("Kusto warning: fixture warning" in message for message in raw["warnings"]), warning)

    def test_publish_rejects_partial_and_removes_stale_view(self):
        output = self.output_dir()
        view = view_fixture(["required metric missing"])
        view["suggestion"] = {"command": "--at 2026-09-08T10:00:00Z"}
        (output / "view.json").write_text("stale view")
        (output / "raw.json").write_text("retained raw")
        with patch.object(cli, "process_bundle", return_value=view):
            self.assertEqual(cli.publish({}, output, False), 2)
        self.assertFalse((output / "view.json").exists())
        self.assertEqual((output / "raw.json").read_text(), "retained raw")
        manifest = json.loads((output / "manifest.json").read_text())
        self.assertTrue(manifest["partial"])
        self.assertEqual(manifest["errors"], view["errors"])
        self.assertEqual(manifest["suggestion"], view["suggestion"])
        self.assertIn("Suggested rerun:", self.stderr.getvalue())

    def test_processing_cache_hits_for_identical_inputs_offline(self):
        output = self.output_dir()
        raw = {"schema_version": 1, "queries": [], "snapshots": [], "node_snapshots": []}
        (output / "raw.json").write_text(json.dumps(raw))
        view = view_fixture()
        with patch.object(cli, "Client") as client, patch.object(cli, "process_bundle", return_value=view) as processor:
            for _ in range(2):
                self.assertEqual(self.main("process", output / "raw.json", "--output", output), 0)
                self.assertEqual(json.loads((output / "view.json").read_text()), view)
            processor.assert_called_once_with(raw)
            client.assert_not_called()
        cached = json.loads((output / ".processed.json").read_text())
        self.assertEqual(cached["view"], view)
        self.assertEqual(json.loads((output / "manifest.json").read_text())["generated_at"], view["generated_at"])

    def test_processing_cache_ignores_responses_and_query_completion_order(self):
        output = self.output_dir()
        raw = {"schema_version": 1, "queries": [
            {"metric": "node_cpu", "expression": "cpu", "series": [], "response": {"original": 1}},
            {"metric": "node_memory", "expression": "memory", "series": [], "response": {"original": 2}},
        ], "kusto_queries": [{"query": "old KQL", "response": {"Tables": []}}]}
        with patch.object(cli, "process_bundle", return_value=view_fixture()) as processor:
            self.assertEqual(cli.publish(raw, output, False), 0)
            digest = json.loads((output / ".processed.json").read_text())["digest"]
            raw["queries"].reverse()
            for query in raw["queries"]:
                query["response"] = {"different envelope": True}
            raw["kusto_queries"] = [{"query": "new KQL", "response": {"ignored": True}}]
            self.assertEqual(cli.publish(raw, output, False), 0)
            self.assertEqual(json.loads((output / ".processed.json").read_text())["digest"], digest)
            processor.assert_called_once()

    def test_processing_cache_invalidates_on_changed_inputs(self):
        for field, value in (
                ("queries", [{"metric": "node_cpu", "series": [{"labels": {}, "samples": [[7200, 2]]}]}]),
                ("snapshots", [{"uid": "hcp", "object": {"kind": "HostedCluster"}}]),
                ("node_snapshots", [{"uid": "node", "object": {"kind": "Node"}}]),
                ("errors", ["new collection failure"]), ("warnings", ["new warning"]),
                ("at", 7260), ("step_seconds", 120), ("settle_seconds", 1800),
                ("sources", [{"environment": "int", "url": "https://other"}])):
            with self.subTest(field=field):
                output = self.output_dir()
                raw = {"schema_version": 1, "at": 7200, "step_seconds": 60, "settle_seconds": 900,
                       "queries": [{"metric": "node_cpu", "series": [{"labels": {}, "samples": [[7200, 1]]}]}],
                       "snapshots": [], "node_snapshots": [], "errors": [], "warnings": [], "sources": []}
                first, updated = view_fixture(), view_fixture()
                updated["generated_at"] = "2026-09-08T12:06:00Z"
                with patch.object(cli, "process_bundle", side_effect=[first, updated]) as processor:
                    self.assertEqual(cli.publish(raw, output, False), 0)
                    digest = json.loads((output / ".processed.json").read_text())["digest"]
                    raw[field] = value
                    self.assertEqual(cli.publish(raw, output, False), 0)
                    self.assertNotEqual(json.loads((output / ".processed.json").read_text())["digest"], digest)
                    self.assertEqual(cli.publish(raw, output, False), 0)
                    self.assertEqual(processor.call_count, 2)
                    self.assertEqual(json.loads((output / "view.json").read_text()), updated)

    def test_processing_cache_invalidates_on_processor_source_change(self):
        output = self.output_dir()
        source = Path(cli.__file__).with_name("process.py")
        read_bytes = Path.read_bytes
        source_reads = []

        def changed_source(path):
            content = read_bytes(path)
            if path == source:
                source_reads.append(path)
                return content + b"\n# processor revision fixture\n"
            return content

        with patch.object(cli, "process_bundle", return_value=view_fixture()) as processor:
            self.assertEqual(cli.publish({}, output, False), 0)
            digest = json.loads((output / ".processed.json").read_text())["digest"]
            with patch.object(Path, "read_bytes", changed_source):
                for _ in range(2):
                    self.assertEqual(cli.publish({}, output, False), 0)
                self.assertEqual((output / "view.json").read_bytes(), read_bytes(output / "view.json"))
            self.assertEqual(source_reads, [source, source])
            self.assertEqual(processor.call_count, 2)
            self.assertNotEqual(json.loads((output / ".processed.json").read_text())["digest"], digest)

    def test_processing_cache_never_bypasses_partial_publication_policy(self):
        for empty_inventory in (False, True):
            with self.subTest(empty_inventory=empty_inventory):
                output = self.output_dir()
                view = view_fixture([] if empty_inventory else ["required metric missing"])
                if empty_inventory:
                    view["management_clusters"] = []
                with patch.object(cli, "process_bundle", return_value=view) as processor:
                    for allow_partial in (True, False, True, False):
                        self.assertEqual(cli.publish({}, output, allow_partial), 2)
                        self.assertEqual((output / "view.json").exists(), allow_partial)
                        manifest = json.loads((output / "manifest.json").read_text())
                        self.assertTrue(manifest["partial"])
                        self.assertEqual(len(manifest["errors"]), 1)
                    processor.assert_called_once()

    def test_processing_cache_corruption_reprocesses(self):
        output = self.output_dir()
        view = view_fixture()
        with patch.object(cli, "process_bundle", return_value=view):
            self.assertEqual(cli.publish({}, output, False), 0)
        path = output / ".processed.json"
        valid = json.loads(path.read_text())
        for content in ("{broken", "null", "[]", "{}", json.dumps({"digest": valid["digest"]}),
                        json.dumps({**valid, "view": None}),
                        json.dumps({**valid, "view": {**view, "schema_version": 99}})):
            with self.subTest(content=content):
                path.write_text(content)
                with patch.object(cli, "process_bundle", return_value=view) as processor:
                    self.assertEqual(cli.publish({}, output, False), 0)
                    self.assertEqual(cli.publish({}, output, False), 0)
                    processor.assert_called_once_with({})
                self.assertEqual(json.loads(path.read_text()), valid)

    def test_publish_allow_partial_writes_view_but_returns_two(self):
        output = self.output_dir()
        view = view_fixture(["required metric missing"])
        with patch.object(cli, "process_bundle", return_value=view):
            self.assertEqual(cli.publish({}, output, True), 2)
        self.assertEqual(json.loads((output / "view.json").read_text()), view)
        self.assertTrue(json.loads((output / "manifest.json").read_text())["partial"])

    def test_publish_removes_stale_view_before_processing_exception(self):
        output = self.output_dir()

        def fail(raw):
            self.assertFalse((output / "view.json").exists())
            raise ValueError("fixture processing failure")

        for allow_partial in (False, True):
            with self.subTest(allow_partial=allow_partial):
                (output / "view.json").write_text("stale view")
                with patch.object(cli, "process_bundle", side_effect=fail):
                    with self.assertRaisesRegex(ValueError, "fixture processing failure"):
                        cli.publish({}, output, allow_partial)
                self.assertFalse((output / "view.json").exists())
                self.assertFalse((output / "manifest.json").exists())

    def test_publish_empty_inventory_is_not_success(self):
        output = self.output_dir()
        view = view_fixture()
        view["management_clusters"] = []
        with patch.object(cli, "process_bundle", return_value=view):
            self.assertEqual(cli.publish({}, output, False), 2)
        self.assertIn("No management clusters discovered", view["errors"][0])
        self.assertFalse((output / "view.json").exists())

    def test_process_offline_success_allows_warnings(self):
        output = self.output_dir()
        raw = {"schema_version": 1, "fixture": True}
        (output / "raw.json").write_text(json.dumps(raw))
        view = view_fixture()
        view["warnings"] = ["unknown optional metadata"]
        with patch.object(cli, "Client") as client, patch.object(cli, "process_bundle", return_value=view) as processor:
            self.assertEqual(self.main("process", output / "raw.json", "--output", output), 0)
            client.assert_not_called()
            processor.assert_called_once_with(raw)
        self.assertEqual(json.loads((output / "view.json").read_text()), view)
        manifest = json.loads((output / "manifest.json").read_text())
        self.assertFalse(manifest["partial"])
        self.assertEqual(manifest["warnings"], view["warnings"])

    def test_process_offline_partial_requires_explicit_flag(self):
        output = self.output_dir()
        raw = {"schema_version": 1, "at": 7200, "start": 3600, "step_seconds": 60,
               "errors": ["fixture collection failure"]}
        (output / "raw.json").write_text(json.dumps(raw))
        self.assertEqual(self.main("process", output / "raw.json", "--output", output), 2)
        self.assertFalse((output / "view.json").exists())
        self.assertEqual(self.main("process", output / "raw.json", "--output", output, "--allow-partial"), 2)
        self.assertIn("fixture collection failure", json.loads((output / "view.json").read_text())["errors"])

    def test_process_invalid_raw_reports_failure(self):
        output = self.output_dir()
        for content in ("not json", '{"schema_version": 99}'):
            with self.subTest(content=content):
                (output / "raw.json").write_text(content)
                (output / "view.json").write_text("stale view")
                self.assertEqual(self.main("process", output / "raw.json", "--output", output), 1)
                self.assertFalse((output / "view.json").exists())
                self.assertEqual((output / "raw.json").read_text(), content)


if __name__ == "__main__":
    unittest.main()

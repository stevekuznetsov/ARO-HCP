// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
)

func TestUpgradeSignalQueries(t *testing.T) {
	var queries []metrics.Query
	if err := decodeFile("examples/upgrade-signal-queries.json", &queries); err != nil {
		t.Fatal(err)
	}
	// Export validates the public query contract before observing cancellation.
	// This transport makes even an accidental attempted network call fail locally.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &http.Client{Transport: upgradeSignalNoNetwork{t}}
	start := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	manifest, err := metrics.Export(ctx, client, "https://unused.invalid", queries, start, start.Add(time.Minute), 5*time.Second, filepath.Join(t.TempDir(), "export"))
	if !errors.Is(err, context.Canceled) || len(manifest.Queries) != len(queries) {
		t.Fatalf("query contract validation failed: %v (%d queries)", err, len(manifest.Queries))
	}
	values, timestamps := map[string]string{}, map[string]string{}
	metricName := regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	for _, q := range queries {
		expr := q.Expr
		isTime := strings.HasPrefix(expr, "timestamp(") && strings.HasSuffix(expr, ")")
		if isTime {
			expr = strings.TrimSuffix(strings.TrimPrefix(expr, "timestamp("), ")")
		}
		selector, labels, _ := strings.Cut(expr, "{")
		if selector == "" {
			var ok bool
			selector, ok = strings.CutPrefix(labels, `__name__=~"`)
			if !ok {
				t.Fatalf("%s: expected an exact metric selection", q.Name)
			}
			selector, labels, ok = strings.Cut(selector, `"`)
			if !ok || isTime {
				t.Fatalf("%s: timestamp must identify one metric", q.Name)
			}
			labels = strings.TrimPrefix(labels, ",")
			if labels == "}" {
				labels = ""
			}
		}
		if strings.Contains(labels, "namespace") || strings.Contains(labels, "pod=") || strings.Contains(labels, "node=") {
			t.Fatalf("%s: inventory scope must not be hardcoded", q.Name)
		}
		for _, name := range strings.Split(selector, "|") {
			if !metricName.MatchString(name) {
				t.Fatalf("%s: wildcard or invalid metric name %q", q.Name, name)
			}
			if strings.HasPrefix(name, "container_") && labels != `container!="",container!="POD"}` {
				t.Fatalf("%s: container scope changed: %s", q.Name, labels)
			}
			if isTime {
				if _, exists := timestamps[name]; exists {
					t.Fatalf("duplicate timestamp selection: %s", name)
				}
				// Histogram timestamps deliberately use just the +Inf bucket.
				labels = strings.ReplaceAll(labels, `,le="+Inf"`, "")
				labels = strings.ReplaceAll(labels, `le="+Inf"}`, "")
				timestamps[name] = labels
			} else {
				if _, exists := values[name]; exists {
					t.Fatalf("overlapping value selection: %s", name)
				}
				values[name] = labels
			}
		}
	}
	for name, labels := range values {
		if got, ok := timestamps[name]; !ok || got != labels {
			t.Errorf("%s: missing or mismatched per-family timestamp: %q != %q", name, got, labels)
		}
	}
	for name := range timestamps {
		if _, ok := values[name]; !ok {
			t.Errorf("timestamp without raw metric: %s", name)
		}
	}
	for _, name := range []string{
		"up", "apiserver_request_total", "apiserver_request_duration_seconds_bucket",
		"apiserver_flowcontrol_rejected_requests_total", "apiserver_flowcontrol_request_wait_duration_seconds_bucket",
		"etcd_server_quota_backend_bytes", "etcd_mvcc_db_total_size_in_use_in_bytes", "etcd_mvcc_db_total_size_in_bytes",
		"etcd_disk_wal_fsync_duration_seconds_bucket", "etcd_disk_backend_commit_duration_seconds_bucket",
		"etcd_server_proposals_pending", "etcd_server_proposals_failed_total", "etcd_server_leader_changes_seen_total",
		"container_cpu_usage_seconds_total", "container_pressure_cpu_waiting_seconds_total", "container_spec_cpu_quota",
		"container_memory_usage_bytes", "container_spec_memory_limit_bytes", "container_oom_events_total",
		"container_pressure_memory_waiting_seconds_total", "container_pressure_io_waiting_seconds_total",
		"kubelet_volume_stats_available_bytes", "go_gc_gomemlimit_bytes", "go_gc_limiter_last_enabled_gc_cycle",
		"go_memstats_heap_alloc_bytes", "go_memstats_heap_inuse_bytes", "go_memstats_heap_sys_bytes",
		"go_memstats_sys_bytes", "go_memstats_heap_released_bytes", "go_gc_heap_live_bytes", "go_gc_heap_goal_bytes",
		"go_memstats_alloc_bytes_total", "go_gc_cycles_total_gc_cycles_total",
		"go_cpu_classes_gc_total_cpu_seconds_total", "go_cpu_classes_gc_mark_assist_cpu_seconds_total",
		"go_cpu_classes_total_cpu_seconds_total", "go_sched_gomaxprocs_threads",
		"go_gc_duration_seconds_sum", "go_gc_duration_seconds_count",
		"process_resident_memory_bytes", "process_start_time_seconds",
	} {
		if _, ok := values[name]; !ok {
			t.Errorf("missing upgrade evidence family: %s", name)
		}
	}
}

type upgradeSignalNoNetwork struct{ t *testing.T }

func (n upgradeSignalNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	n.t.Error("query contract test attempted a network request")
	return nil, errors.New("network disabled")
}

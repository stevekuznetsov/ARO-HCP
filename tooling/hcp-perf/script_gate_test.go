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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
)

// Keep real Bash waits, traps, timeout process groups and finalization dispatch.
// Only the CLI is replaced; no Kubernetes clients or credentials are used.
func TestCaseFinalizationSignals(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "timeout", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required: %v", tool, err)
		}
	}
	body, err := os.ReadFile("scripts/run-case.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	start, end := strings.Index(script, "active_pid="), strings.Index(script, "\n# Validate only after finalization")
	if start < 0 || end <= start {
		t.Fatal("missing finalization functions and traps")
	}
	for _, tc := range []struct {
		name          string
		workloadExit  int
		initialSignal syscall.Signal
		exportExit    int
		signals       bool
		softMetrics   bool
		gate          string
	}{
		{name: "normal"},
		{name: "term-during-export", signals: true},
		{name: "preserve-workload-failure", workloadExit: 23, signals: true},
		{name: "safety-term-during-workload", initialSignal: syscall.SIGTERM, workloadExit: 143, signals: true},
		{name: "int-during-workload", initialSignal: syscall.SIGINT, workloadExit: 130, signals: true},
		{name: "hup-during-workload", initialSignal: syscall.SIGHUP, workloadExit: 129, signals: true},
		{name: "export-failure", exportExit: 17, signals: true},
		{name: "export-timeout", exportExit: 124, signals: true},
		{name: "soft-violation-metrics-503", softMetrics: true},
		{name: "completed-turnover-violation", gate: "false", workloadExit: 1},
		{name: "malformed-turnover-gate", gate: "error(\"bad counters\")", workloadExit: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mock := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1 $2" >>"$TEST_DIR/calls"
case "$1 $2" in
    'workload run'|'turnover run')
        label=$1
        trap 'printf stopped >"$TEST_DIR/$label.stopped"; exit 143' TERM
        printf ready >"$TEST_DIR/$label.ready"
        sleep 3 & wait ;;
    'adversaries stop'|'record stop')
        test -s "$TEST_DIR/finalization-started.json"
        printf '{}\n' ;;
    'adversaries status'|'record status')
        printf '%s\n' '{"phase":"stopped","nonterminalPods":0,"submissionIncomplete":false}' ;;
    'record export')
        trap 'printf signaled >"$TEST_DIR/export.signaled"; exit 143' TERM INT
        printf ready >"$TEST_DIR/export.ready"
        sleep 1 & wait
        printf '%s\n' '{"complete":true}' >"$TEST_DIR/manifest.json"
        printf '%s\n' '{"exported":true}'
        exit "$EXPORT_EXIT" ;;
    'scrape check') printf '{}\n' ;;
    'metrics export') printf '%s\n' '{"status":"error","error":"HTTP 503"}'; exit 1 ;;
    'analyze --input') printf '{}\n'; exit 1 ;;
    *) exit 91 ;;
esac
`
			perf := filepath.Join(dir, "mock-perf")
			if err := os.WriteFile(perf, []byte(mock), 0700); err != nil {
				t.Fatal(err)
			}
			harness := `
set -Eeuo pipefail
exec 4>&2
commands=$1 evidence=$1 artifacts_dir=$1 inputs=$1
export TEST_DIR=$1 EXPORT_EXIT=$2
HCP_PERF=$1/mock-perf MGMT_KUBECONFIG=unused mode=validation
export_bound=4s
if [[ $EXPORT_EXIT == 124 ]]; then export_bound=0.5s; fi
printf '{}\n' >"$inputs/input.json"
` + script[start:end] + `
recorder_attempted=true adversaries_attempted=true
phase=workload
if [[ -n $6 ]]; then
    workload_completed=true
    run turnover-gate 5s jq -en "$6"
fi
if [[ $5 == true ]]; then
    observation_started=true steady_started=true
    duration=1 cadence=1 steady_epoch=1 steady_seconds=0 slot=1 expected_canaries=1 cycle=1
    HCP_NAMESPACE=unused GUEST_ADMIN_KUBECONFIG=unused CAPTURE_ENDPOINT=unused NODE_METRICS_ENDPOINT=unused
    kubectl() { printf '{}\n'; }
    export -f kubectl
    printf '%s\n' '{"performance":"degraded","degraded":true}' >"$commands/canary-steady-1.json"
    printf '0\n' >"$commands/canary-steady-1.exit"
fi
if [[ $3 != 0 ]]; then
    timeout --kill-after=1s 4s "$HCP_PERF" turnover run &
    turnover_pid=$!
    while [[ ! -f $evidence/turnover.ready ]]; do sleep 0.01; done
    run workload 4s "$HCP_PERF" workload run
fi
exit "$4"
`
			// Shorten bounds only; keep the real timeout process as active_pid.
			harness = strings.ReplaceAll(harness, "--kill-after=150s", "--kill-after=1s")
			harness = strings.ReplaceAll(harness, "run record-export 12m", "run record-export \"$export_bound\"")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			// timeout cannot execute exported shell functions directly.
			harness = strings.ReplaceAll(harness, "40s kubectl", "40s bash -c 'printf \"{}\\n\"' --")
			cmd := exec.CommandContext(ctx, "bash", "-c", harness, "test", dir, fmt.Sprint(tc.exportExit), fmt.Sprint(tc.initialSignal), fmt.Sprint(tc.workloadExit), fmt.Sprint(tc.softMetrics), tc.gate)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var waitErr error
			go func() {
				waitErr = cmd.Wait()
				close(done)
			}()
			defer func() {
				cancel()
				<-done
			}()
			waitFor := func(name string, count int) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					data, err := os.ReadFile(filepath.Join(dir, name))
					if err == nil && (count == 0 || bytes.Count(data, []byte("\n")) >= count) {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
				t.Fatalf("timed out waiting for %s (%d events)", name, count)
			}
			if tc.initialSignal != 0 {
				waitFor("workload.ready", 0)
				if err := cmd.Process.Signal(tc.initialSignal); err != nil {
					t.Fatal(err)
				}
			}
			waitFor("export.ready", 0)
			marker, err := os.ReadFile(filepath.Join(dir, "finalization-started.json"))
			var phase struct {
				Phase, StartedAt string
				WorkloadExitCode int
			}
			if err != nil || json.Unmarshal(marker, &phase) != nil || phase.Phase != "finalization" || phase.WorkloadExitCode != tc.workloadExit {
				t.Fatalf("invalid finalization marker: %s (%v)", marker, err)
			}
			if _, err := time.Parse(time.RFC3339, phase.StartedAt); err != nil {
				t.Fatal(err)
			}
			if tc.signals {
				for i, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGTERM, syscall.SIGINT} {
					if err := cmd.Process.Signal(signal); err != nil {
						t.Fatal(err)
					}
					waitFor("shutdown-events.jsonl", i+1)
				}
			}
			<-done
			wantExit := tc.workloadExit
			if wantExit == 0 && (tc.exportExit != 0 || tc.softMetrics) {
				wantExit = 1
			}
			if cmd.ProcessState.ExitCode() != wantExit {
				t.Fatalf("exit: %v, want %d; output: %s", waitErr, wantExit, output.String())
			}
			data, err := os.ReadFile(filepath.Join(dir, "outcome.json"))
			var outcome struct {
				ExitCode, WorkloadExitCode                       int
				ShutdownOrExportFailed                           bool
				Interrupted                                      bool `json:"finalization_interrupted"`
				ExecutionStatus, ExperimentStatus, CaptureStatus string
				ExpectationViolations                            struct{ DegradedCanaries *int }
			}
			if err != nil || json.Unmarshal(data, &outcome) != nil || outcome.ExitCode != wantExit || outcome.WorkloadExitCode != tc.workloadExit || outcome.ShutdownOrExportFailed != (tc.exportExit != 0 || tc.softMetrics) || outcome.Interrupted != tc.signals {
				t.Fatalf("unexpected outcome: %s (%v)", data, err)
			}
			wantExecution := "completed"
			if tc.initialSignal != 0 {
				wantExecution = "aborted"
			} else if tc.workloadExit != 0 {
				wantExecution = "harness_error"
			}
			if tc.gate == "false" {
				wantExecution = "completed"
			}
			if outcome.ExecutionStatus != wantExecution {
				t.Fatalf("wrong execution status: %s", data)
			}
			if tc.gate == "false" && outcome.ExperimentStatus != "violations" {
				t.Fatalf("failed gate not classified: %s", data)
			}
			if tc.softMetrics && (outcome.ExperimentStatus != "degraded" || outcome.CaptureStatus != "incomplete" || outcome.ExpectationViolations.DegradedCanaries == nil || *outcome.ExpectationViolations.DegradedCanaries != 1) {
				t.Fatalf("soft violation/capture axes conflated: %s", data)
			}
			if !tc.softMetrics && outcome.ExpectationViolations.DegradedCanaries != nil {
				t.Fatalf("missing canaries must not imply zero violations: %s", data)
			}
			if _, err := os.Stat(filepath.Join(dir, "outcome.json.tmp")); !os.IsNotExist(err) {
				t.Fatalf("unpublished outcome temporary: %v", err)
			}
			data, err = os.ReadFile(filepath.Join(dir, "record-export.exit"))
			if err != nil || strings.TrimSpace(string(data)) != fmt.Sprint(tc.exportExit) {
				t.Fatalf("lost child exit status: %s (%v)", data, err)
			}
			if tc.exportExit != 124 {
				data, err = os.ReadFile(filepath.Join(dir, "manifest.json"))
				if err != nil || !json.Valid(data) {
					t.Fatalf("export child not awaited: %s (%v)", data, err)
				}
				if _, err := os.Stat(filepath.Join(dir, "export.signaled")); !os.IsNotExist(err) {
					t.Fatalf("finalization signal reached export child: %v", err)
				}
				data, err = os.ReadFile(filepath.Join(dir, "record-export.json"))
				if err != nil || !json.Valid(data) {
					t.Fatalf("corrupt command JSON: %s (%v)", data, err)
				}
			}
			if tc.initialSignal != 0 {
				for _, label := range []string{"workload", "turnover"} {
					if _, err := os.Stat(filepath.Join(dir, label+".stopped")); err != nil {
						t.Fatalf("%s was not stopped: %v", label, err)
					}
				}
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil || !strings.Contains(string(calls), "adversaries stop\nadversaries status\nrecord stop\nrecord status\nrecord export\n") {
				t.Fatalf("shutdown/export sequence incomplete: %s (%v)", calls, err)
			}
			if tc.signals {
				data, err = os.ReadFile(filepath.Join(dir, "shutdown-events.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					var event struct{ Type, Signal, ObservedAt string }
					want := []string{"TERM", "INT", "TERM", "INT"}[i]
					if json.Unmarshal([]byte(line), &event) != nil || event.Type != "finalization-stop-requested" || event.Signal != want {
						t.Fatalf("invalid shutdown event: %s", line)
					}
					if !strings.Contains(output.String(), "WARNING: "+want+" requested during finalization") {
						t.Fatalf("missing signal warning: %s", output.String())
					}
				}
			}
		})
	}
}

func TestCaseRecorderStartupGate(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required: %v", tool, err)
		}
	}
	body, err := os.ReadFile("scripts/run-case.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	start := strings.Index(script, "poll_session() {")
	end := strings.Index(script, "\n# Capture health")
	gate := strings.Index(script, "record_ready='")
	seed := strings.Index(script, "\nrun seed ")
	if start < 0 || end <= start || gate < 0 || seed <= gate {
		t.Fatal("missing recorder startup gate")
	}
	for _, mode := range []string{"ready", "delayed", "pending", "unready", "deleting", "no-start", "old-cli", "failed", "status-error"} {
		t.Run(mode, func(t *testing.T) {
			harness := `
set -euo pipefail
commands=$1 mode=$2
record_namespace=record record_id=record MGMT_KUBECONFIG=unused HCP_PERF=mock_perf
SECONDS=0 calls=0
sleep() { SECONDS=$((SECONDS+$1)); }
run() { local label=$1; shift 2; "$@" >"$commands/$label.json"; }
mock_perf() {
    calls=$((calls+1))
    [[ $mode != status-error ]] || return 1
    jq -n --arg mode "$mode" --argjson calls "$calls" '
      {phase:"running",nonterminalPods:1,pods:[{phase:"Running",ready:true,terminating:false,startTime:"2026-01-01T00:00:00Z"}]} |
      if $mode=="pending" or ($mode=="delayed" and $calls<3) then .phase="pending" | .pods[0].phase="Pending"
      elif $mode=="unready" then .pods[0].ready=false
      elif $mode=="deleting" then .pods[0].terminating=true
      elif $mode=="no-start" then del(.pods[0].startTime)
      elif $mode=="old-cli" then del(.pods[0].ready, .pods[0].startTime)
      elif $mode=="failed" then .phase="failed"
      else . end'
}
`
			harness += script[start:end] + "\n" + script[gate:seed] + `
[[ $mode != delayed || $calls == 3 ]]
printf seeded >"$commands/seeded"
`
			harness = strings.ReplaceAll(harness, "SECONDS", "TEST_CLOCK")
			dir := t.TempDir()
			output, err := exec.Command("bash", "-c", harness, "test", dir, mode).CombinedOutput()
			ready := mode == "ready" || mode == "delayed"
			_, seedErr := os.Stat(filepath.Join(dir, "seeded"))
			if (err == nil) != ready || (seedErr == nil) != ready {
				t.Fatalf("gate/seed result: %v, %v; output: %s", err, seedErr, output)
			}
		})
	}
}

func TestCaseCaptureHealthGate(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required: %v", tool, err)
		}
	}
	script, err := os.ReadFile("scripts/run-case.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(script), "require_metrics() {")
	end := strings.Index(string(script), "\nfinish() {")
	if start < 0 || end <= start {
		t.Fatal("capture health function not found")
	}
	for _, tc := range []struct {
		name, query, values, extra string
		omit, mismatch, fail       bool
	}{
		{name: "healthy"},
		{name: "new-container", query: "container-memory", values: `[[1010,"12"]]`},
		{name: "family-warning", query: "container-memory", extra: `,"warnings":["partial data"]`},
		{name: "down", query: "up", values: `[[1000,"1"],[1005,"0"],[1010,"1"]]`, fail: true},
		{name: "up-gap", query: "up", values: `[[1000,"1"],[1010,"1"]]`, fail: true},
		{name: "time-gap", query: "scrape-time", values: `[[1000,"999"],[1010,"1009"]]`, fail: true},
		{name: "health-warning", query: "up", extra: `,"warnings":["partial data"]`, fail: true},
		{name: "lookback-stale", query: "scrape-time", values: `[[1000,"960"],[1005,"960"],[1010,"960"]]`, fail: true},
		{name: "freshness-boundary", query: "scrape-time", values: `[[1000,"955"],[1005,"960"],[1010,"965"]]`, fail: true},
		{name: "future-source", query: "scrape-time", values: `[[1000,"1010"],[1005,"1010"],[1010,"1010"]]`, fail: true},
		{name: "missing-up", query: "up", omit: true, fail: true},
		{name: "missing-time", query: "scrape-time", omit: true, fail: true},
		{name: "different-target", mismatch: true, fail: true},
		{name: "empty-family", query: "container-memory", values: `[]`, fail: true},
		{name: "nonfinite-family", query: "container-memory", values: `[[1010,"NaN"]]`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query().Get("query")
				values := `[[1000,"1"],[1005,"1"],[1010,"1"]]`
				if query == "scrape-time" {
					values = `[[1000,"999"],[1005,"1004"],[1010,"1009"]]`
				}
				extra := ""
				if query == tc.query {
					if tc.values != "" {
						values = tc.values
					}
					extra = tc.extra
				}
				instance := "replica"
				if tc.mismatch && query == "scrape-time" {
					instance = "other-replica"
				}
				_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"instance":"` + instance + `"},"values":` + values + `}]}` + extra + `}`))
			}))
			defer server.Close()
			dir := t.TempDir()
			capture := filepath.Join(dir, "capture")
			queries := []metrics.Query{{Name: "up", Expr: "up"}, {Name: "scrape-time", Expr: "scrape-time"}, {Name: "container-memory", Expr: "container-memory"}}
			if tc.omit {
				for i, query := range queries {
					if query.Name == tc.query {
						queries = append(queries[:i], queries[i+1:]...)
						break
					}
				}
			}
			_, exportErr := metrics.Export(t.Context(), server.Client(), server.URL, queries, time.Unix(1000, 0), time.Unix(1010, 0), 5*time.Second, capture)
			analysis, analyzeErr := metrics.Analyze(capture)
			if !tc.fail && (exportErr != nil || analyzeErr != nil) {
				t.Fatalf("export: %v; analyze: %v", exportErr, analyzeErr)
			}
			if tc.name == "new-container" || tc.name == "family-warning" {
				if analysis.Queries[2].Status != metrics.StatusIncomplete || analysis.Manifest.Queries[2].Status != metrics.StatusIncomplete {
					t.Fatal("dynamic-family incomplete flags must be preserved")
				}
			}
			body, err := json.Marshal(analysis)
			if err != nil {
				t.Fatal(err)
			}
			analysisFile := filepath.Join(dir, "analysis.json")
			if err := os.WriteFile(analysisFile, body, 0600); err != nil {
				t.Fatal(err)
			}
			// Mock only command dispatch; use the real exporter, analyzer and jq gates.
			cmd := exec.Command("bash", "-c", string(script[start:end])+`
set -euo pipefail
commands=$1
SCRAPE_MAX_AGE_SECONDS=45
HCP_PERF=mock_perf
mock_perf() { jq . "$commands/analysis.json"; }
run() { local label=$1; shift 2; "$@" >"$commands/$label.json"; }
require_metrics test "$2"
`, "test", dir, capture)
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("gate error: %v, want failure %v; output: %s", err, tc.fail, output)
			}
		})
	}
}

// Exercise the actual script blocks with local dispatch and a virtual shell clock.
// No Kubernetes, credentials, long-lived child processes or real sleeps are used.
func TestCaseScriptWorkloadGates(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required: %v", tool, err)
		}
	}
	body, err := os.ReadFile("scripts/run-case.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	block := func(start, end string) string {
		t.Helper()
		a := strings.Index(script, start)
		if a < 0 {
			t.Fatalf("missing block %q", start)
		}
		b := len(script)
		if end != "" {
			i := strings.Index(script[a:], end)
			if i < 0 {
				t.Fatalf("missing block end %q", end)
			}
			b = a + i
		}
		return script[a:b]
	}
	for _, tc := range []struct {
		name, mode string
		apps       int
		fail       bool
		validation bool
		duration   int
	}{
		{name: "unseeded", apps: 0},
		{name: "validation-unseeded", validation: true},
		{name: "validation-hourly-churn", validation: true, apps: 30},
		{name: "validation-minimum", validation: true, duration: 30},
		{name: "validation-fractional-cadence-window", validation: true, duration: 301},
		{name: "validation-short-window", validation: true, mode: "short-window", fail: true},
		{name: "validation-canary-failure", validation: true, mode: "canary-failure", fail: true},
		{name: "validation-missed-cadence", validation: true, mode: "slow-canary"},
		{name: "validation-multiple-missed-slots", validation: true, mode: "long-canary"},
		{name: "validation-final-late-cycle", validation: true, mode: "final-late-canary"},
		{name: "validation-degraded", validation: true, mode: "degraded"},
		{name: "validation-hard-timeout", validation: true, mode: "hard-timeout", fail: true},
		{name: "validation-expired-canary", validation: true, mode: "expired-canary", fail: true},
		{name: "validation-sufficient-token", validation: true, mode: "short-token"},
		{name: "soak-insufficient-token", mode: "short-token", fail: true},
		{name: "validation-bad-turnover-count", validation: true, mode: "bad-turnover-count", fail: true},
		{name: "partial-namespace", apps: 12},
		{name: "two-namespaces", apps: 30},
		{name: "full-load", apps: 120},
		{name: "seeded-node-full", apps: 120, mode: "node-full", fail: true},
		{name: "seeded-aggregate-full", apps: 120, mode: "aggregate-full", fail: true},
		{name: "missing-list", mode: "missing-list", fail: true},
		{name: "unexpected-seed", mode: "unexpected-seed", fail: true},
		{name: "missing-credentials", mode: "missing-credentials", fail: true},
		{name: "expired-canary", mode: "expired-canary", fail: true},
		{name: "negative-offset-canary", mode: "negative-offset-canary"},
		{name: "positive-offset-canary", mode: "positive-offset-canary"},
		{name: "expired-offset-canary", mode: "expired-offset-canary", fail: true},
		{name: "invalid-expiry-canary", mode: "invalid-expiry-canary", fail: true},
		{name: "short-window", mode: "short-window", fail: true},
		{name: "canary-failure", mode: "canary-failure", fail: true},
		{name: "bad-turnover-count", mode: "bad-turnover-count", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			harness := `
set -euo pipefail
commands=$1 evidence=$1 artifacts_dir=$1 inputs=$1
applications=$2 mode=$3
duration=$4 cadence=$5 recovery=$6
expected_canaries=$(((duration+cadence-1)/cadence))
scheduled=$(((applications*duration+3599)/3600))
namespace_count=$(((applications+19)/20))
prefix=apps seed_id=seed seed_timeout=60
max_outstanding=16
jq -n --arg mode "$mode" '{items:[range(3)|{metadata:{name:("node-"+tostring),uid:tostring},status:{allocatable:{pods:(if $mode=="node-full" and .==0 then "201" elif $mode=="aggregate-full" then "205" else "250" end)}}}]}' >"$commands/guest-nodes.json"
record_namespace=record record_id=record canary_namespace=canary
MGMT_KUBECONFIG=unused GUEST_ADMIN_KUBECONFIG=unused
HCP_PERF=mock_perf guest=(mock_guest)
contended=false
warmups=0 canaries=0 setup_calls=0 elapsed=0 SECONDS=0
missed_canaries=0 completed_after_window=0 last_finished=-1
die() { printf '%s\n' "$*" >&2; exit 1; }
poll_session() { :; }
sleep() {
    local seconds=$1
    if ((seconds==1 && SECONDS<duration)); then
        seconds=$((cadence-SECONDS%cadence))
        ((SECONDS+seconds<=duration)) || seconds=$((duration-SECONDS))
    fi
    SECONDS=$((SECONDS+seconds)); elapsed=$((elapsed+seconds))
}
run() { local label=$1; shift 2; "$@" >"$commands/$label.json"; }
mock_guest() {
    if [[ $* == *' wait '* ]]; then warmups=$((warmups+1)); return; fi
    if [[ $mode == missing-list ]]; then jq -n '{}'; return; fi
    local count=$applications
    [[ $mode != unexpected-seed ]] || count=1
    if [[ $1 == get && $2 == deployments ]]; then
        jq -n --argjson count "$count" '{items:[range($count)|{metadata:{generation:1},spec:{replicas:5},status:{observedGeneration:1,updatedReplicas:5,readyReplicas:5,availableReplicas:5}}]}'
    else
        jq -n --argjson count "$((count*5))" '{items:[range($count)|{spec:{nodeName:("node-"+(.%3|tostring))},status:{phase:"Running",conditions:[{type:"Ready",status:"True"}]}}]}'
    fi
}

mock_perf() {
    case "$1 $2" in
    'seed credentials')
        if [[ $mode == missing-credentials ]]; then jq -n '{}'; return; fi
        jq -n --argjson count "$namespace_count" '{files:[range($count)|{validUntil:"2099-01-01T00:00:00Z"}]}' ;;
    'workload setup') setup_calls=$((setup_calls+1)) ;;
    'workload credentials')
        local expiry=2099-01-01T00:00:00Z
        [[ $mode != expired-canary ]] || expiry=2000-01-01T00:00:00Z
        [[ $mode != negative-offset-canary ]] || expiry=2099-01-01T00:00:00.123-06:00
        [[ $mode != positive-offset-canary ]] || expiry=2099-01-01T00:00:00+05:30
        [[ $mode != expired-offset-canary ]] || expiry=2000-01-01T00:00:00-06:00
        [[ $mode != invalid-expiry-canary ]] || expiry=invalid
        [[ $mode != short-token ]] || expiry=$(date -u -d '1000 seconds' +%Y-%m-%dT%H:%M:%SZ)
        jq -n --arg expiry "$expiry" '{validUntil:$expiry}' ;;
    'canary run')
        canaries=$((canaries+1))
        local log_count=0 previous= arg
        for arg in "$@"; do
            if [[ $previous == --request-log ]]; then
                [[ $arg == "$commands/$label.requests.jsonl" && ! -e $arg ]] || return 95
                : >"$arg"
            fi
            [[ $arg != --request-log ]] || log_count=$((log_count+1))
            previous=$arg
        done
        ((log_count==1)) || return 96
        [[ $* == *'--timeout 5m'* && $* == *'--ack-budget 30s --ready-budget 2m --cleanup-budget 2m'* ]] || return 92
        if ((canaries>=7)); then
            [[ $* == *'--ack-expected 3ns --ready-expected 6ns --cleanup-expected 9ns'* ]] || return 93
            ((SECONDS > last_finished)) || return 94
        else
            [[ $* != *--ack-expected* ]]
        fi
        if [[ $mode == slow-canary && $canaries -ge 7 ]]; then sleep "$cadence"; fi
        if [[ $mode == long-canary && $canaries -ge 7 ]]; then sleep "$((cadence*2+1))"; fi
        if [[ $mode == final-late-canary && $canaries == "$((expected_canaries+6))" ]]; then sleep "$((cadence+1))"; fi
        if ((canaries>=7)); then last_finished=$SECONDS; fi
        [[ $mode != canary-failure || $canaries -ne 7 ]] || return 1
        [[ $mode != hard-timeout || $canaries -ne 7 ]] || return 124
        jq -n --arg mode "$mode" '{performance:(if $mode=="degraded" then "degraded" else "within_expectations" end),degraded:($mode=="degraded"),ack:{durationNanoseconds:1,completed:"2026-01-01T00:00:00Z"},ready:{durationNanoseconds:2,completed:"2026-01-01T00:00:00Z"},cleanup:{durationNanoseconds:3,completed:"2026-01-01T00:00:00Z"}}' ;;
    esac
}
`
			harness += block("recorder_attempted=true\n", "\nif $contended; then")
			harness += block("run token-lifetime ", "\n# The parent supervises")
			// The child is a no-op. kill/wait model its lifetime in virtual time;
			// the final result is supplied independently of the successful canaries.
			harness += `
timeout() {
    [[ $2 == "$((duration+recovery+120))s" ]] || return 97
    local log_count=0 previous= arg
    for arg in "$@"; do
        if [[ $previous == --request-log ]]; then
            [[ $arg == "$commands/turnover.requests.jsonl" && ! -e $arg ]] || return 98
            : >"$arg"
        fi
        [[ $arg != --request-log ]] || log_count=$((log_count+1))
        previous=$arg
    done
    ((log_count==1))
}
kill() {
    local window=$duration
    [[ $mode != short-window ]] || window=$((duration/2))
    ((SECONDS < steady_seconds+window))
}
wait() { builtin wait "$@"; }
stop_local() { :; }
jq -n --argjson count "$scheduled" --arg mode "$mode" \
    '{scheduled:$count,completed:(if $mode=="bad-turnover-count" then $count+1 else $count end),failed:0,notStarted:0,backlog:0,outstanding:0}' >"$commands/turnover-result.json"
# Child redirection can race with the mock; move the fixture at gate dispatch.
run() {
    local label=$1 rc=0; shift 2
    if [[ $label == turnover-gate ]]; then cp "$commands/turnover-result.json" "$commands/turnover.json"; fi
    "$@" >"$commands/$label.json" || rc=$?
    printf '%s\n' "$rc" >"$commands/$label.exit"
    return "$rc"
}
`
			harness += block("record_missed_slots() {", "\nfinish() {")
			harness += `
trap 'rc=$?; if [[ ${steady_started:-false} == true ]]; then record_missed_slots "$duration" run-ended; fi; exit "$rc"' EXIT
`
			harness += block("steady_seconds=$SECONDS\n", "")
			harness += `
expected_elapsed=$((duration+recovery))
((last_finished+1<=expected_elapsed)) || expected_elapsed=$((last_finished+1))
[[ $warmups == "$namespace_count" && $setup_calls == 1 && $canaries == "$((cycle+6))" && $((cycle+missed_canaries)) == "$expected_canaries" && $elapsed == "$expected_elapsed" ]]
logs=("$commands/"*.requests.jsonl)
[[ ${#logs[@]} == "$((canaries+1))" ]]
if [[ $mode != slow-canary && $mode != long-canary ]]; then [[ $cycle == "$expected_canaries" ]]; fi
rc=0 workload_rc=0 cleanup_failed=0 finalization_interrupted=false
execution=completed reason=completed workload_failed_label= capture_failed=0 shutdown_failed=0 observation_started=true
`
			harness += "\nwrite_test_outcome() {\n" + block("    # Count only real steady", "\n    printf 'Run exit") + "\n}\nwrite_test_outcome\n"
			harness = strings.ReplaceAll(harness, "SECONDS", "TEST_CLOCK")
			duration, cadence, recovery := 7200, 300, 600
			if tc.validation {
				duration, cadence, recovery = 300, 30, 30
				if tc.duration != 0 {
					duration = tc.duration
					cadence = min(30, max(5, duration/5))
				}
			}
			cmd := exec.Command("bash", "-c", harness, "test", dir, fmt.Sprint(tc.apps), tc.mode, fmt.Sprint(duration), fmt.Sprint(cadence), fmt.Sprint(recovery))
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("script error: %v, want failure %v; output: %s", err, tc.fail, output)
			}
			if tc.fail {
				if tc.mode == "canary-failure" || tc.mode == "hard-timeout" {
					if _, err := os.Stat(filepath.Join(dir, "failure-recovery.json")); err != nil {
						t.Fatalf("hard failure must observe recovery: %v", err)
					}
					check := exec.Command("jq", "-es", fmt.Sprintf(`([.[]|select(.type=="cycle-started")]|length)==1 and ([.[]|select(.type=="missed-slot")]|length)==%d and ([.[]|select(.type!="cycle-completed")|.slotIndex]|unique|length)==%d`, (duration+cadence-1)/cadence-1, (duration+cadence-1)/cadence), filepath.Join(dir, "canary-cadence.jsonl"))
					if out, err := check.CombinedOutput(); err != nil {
						t.Fatalf("hard failure must account for all remaining slots once: %s (%v)", out, err)
					}
				}
				return
			}
			data, err := os.ReadFile(filepath.Join(dir, "canary-cadence.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			var starts, misses, late int
			slots := map[int]bool{}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var event struct {
					Type, ScheduledAt, Command string
					SlotIndex                  int
					AfterWindow, Scored        bool
				}
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatal(err)
				}
				if event.Type == "cycle-completed" {
					if event.AfterWindow {
						late++
						if event.Scored {
							t.Fatal("after-window completion must be unscored")
						}
					}
					continue
				}
				if slots[event.SlotIndex] {
					t.Fatalf("duplicate slot %d: %s", event.SlotIndex, data)
				}
				slots[event.SlotIndex] = true
				scheduledAt, err := time.Parse(time.RFC3339, event.ScheduledAt)
				if err != nil {
					t.Fatal(err)
				}
				start, err := os.ReadFile(filepath.Join(dir, "steady-start.txt"))
				if err != nil {
					t.Fatal(err)
				}
				steadyStart, err := time.Parse(time.RFC3339, strings.TrimSpace(string(start)))
				if err != nil || !scheduledAt.Equal(steadyStart.Add(time.Duration(event.SlotIndex*cadence)*time.Second)) {
					t.Fatalf("wrong scheduled time: %s", line)
				}
				if event.Type == "missed-slot" {
					misses++
				} else if event.Type == "cycle-started" {
					starts++
					if event.Command != fmt.Sprintf("canary-steady-%d", event.SlotIndex+1) {
						t.Fatalf("command must use slot index: %s", line)
					}
				} else {
					t.Fatalf("unexpected cadence event: %s", line)
				}
			}
			if len(slots) != (duration+cadence-1)/cadence {
				t.Fatalf("unaccounted slots: %s", data)
			}
			if (tc.mode == "slow-canary" || tc.mode == "long-canary") && misses == 0 {
				t.Fatal("slow cycles must skip slots")
			}
			if tc.mode == "slow-canary" && (starts != 5 || misses != 5) || tc.mode == "long-canary" && (starts != 4 || misses != 6) {
				t.Fatalf("wrong skipped-slot counts: attempted=%d missed=%d", starts, misses)
			}
			if (tc.mode == "final-late-canary" || tc.mode == "long-canary") && late != 1 {
				t.Fatalf("expected final late satisfaction, got %d", late)
			}
			var outcome struct {
				WorkloadExitCode, AttemptedSteadyCanaries, MissedSteadyCanaries, CompletedAfterWindow int
				Qualified                                                                             bool
				ObservationCoverage                                                                   string
			}
			data, err = os.ReadFile(filepath.Join(dir, "outcome.json"))
			if err != nil || json.Unmarshal(data, &outcome) != nil || outcome.WorkloadExitCode != 0 || outcome.Qualified || outcome.AttemptedSteadyCanaries != starts || outcome.MissedSteadyCanaries != misses || outcome.CompletedAfterWindow != late {
				t.Fatalf("unexpected outcome: %s (%v)", data, err)
			}
			if (misses > 0 || late > 0) && outcome.ObservationCoverage != "incomplete" {
				t.Fatalf("coverage must remain incomplete: %s", data)
			}
		})
	}
}

func TestCaseScriptPreflightGates(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required: %v", tool, err)
		}
	}
	body, err := os.ReadFile("scripts/run-case.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, tc := range []struct {
		name, start, end, mutate, message string
		fail                              bool
	}{
		{name: "explicit-zero", start: "run validate-inputs ", end: "nodes=$("},
		{name: "zero-period", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.turnoverPeriodSeconds=0'`},
		{name: "fast-period", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.turnoverPeriodSeconds=1'`},
		{name: "long-period", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.turnoverPeriodSeconds=86400'`},
		{name: "null-period", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.turnoverPeriodSeconds=null'`, fail: true},
		{name: "string-period", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.turnoverPeriodSeconds="300"'`, fail: true},
		{name: "fractional-period", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.turnoverPeriodSeconds=300.5'`, fail: true},
		{name: "negative-period", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.turnoverPeriodSeconds=-1'`, fail: true},
		{name: "large-period", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.turnoverPeriodSeconds=86401'`, fail: true},
		{name: "overflow-period", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.turnoverPeriodSeconds=9223372036854775807'`, fail: true},
		{name: "period-does-not-relax-population", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.turnoverPeriodSeconds=300 | .applications=12'`, fail: true},
		{name: "period-does-not-relax-prefix", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.turnoverPeriodSeconds=300 | .namespacePrefix="other"'`, fail: true},
		{name: "validation", start: "run validate-inputs ", end: "nodes=$(", mutate: `mode=validation; settle=30; update turnover '.durationSeconds=300 | .recoverySeconds=30'; update record '.durationSeconds=2220'`},
		{name: "short-without-opt-in", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.durationSeconds=300'`, fail: true},
		{name: "validation-too-short", start: "run validate-inputs ", end: "nodes=$(", mutate: `mode=validation; update turnover '.durationSeconds=29 | .recoverySeconds=30'`, fail: true},
		{name: "validation-too-long", start: "run validate-inputs ", end: "nodes=$(", mutate: `mode=validation; update turnover '.durationSeconds=1801 | .recoverySeconds=30'`, fail: true},
		{name: "validation-recovery-too-long", start: "run validate-inputs ", end: "nodes=$(", mutate: `mode=validation; update turnover '.durationSeconds=300 | .recoverySeconds=121'`, fail: true},
		{name: "validation-recorder-too-short", start: "run validate-inputs ", end: "nodes=$(", mutate: `mode=validation; settle=30; update turnover '.durationSeconds=300 | .recoverySeconds=30'; update record '.durationSeconds=2219'`, fail: true},
		{name: "fractional-duration", start: "run validate-inputs ", end: "nodes=$(", mutate: `mode=validation; update turnover '.durationSeconds=300.5 | .recoverySeconds=30'`, fail: true},
		{name: "fractional-recovery", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.recoverySeconds=30.5'`, fail: true},
		{name: "validation-default-cadence", start: "recovery=$(jq", end: "canary_namespace=", mutate: `mode=validation; cadence=; update turnover '.durationSeconds=30 | .recoverySeconds=30'`},
		{name: "validation-explicit-cadence", start: "recovery=$(jq", end: "canary_namespace=", mutate: `mode=validation; cadence=10; update turnover '.durationSeconds=300 | .recoverySeconds=30'`},
		{name: "cadence-too-small", start: "recovery=$(jq", end: "canary_namespace=", mutate: `cadence=4`, fail: true},
		{name: "cadence-too-large", start: "recovery=$(jq", end: "canary_namespace=", mutate: `cadence=7201`, fail: true},
		{name: "validation-adversary", start: "adversary_minimum=", end: "# Presence of CPU", mutate: `mode=validation; duration=300; recovery=30; settle=30`},
		{name: "validation-adversary-too-short", start: "adversary_minimum=", end: "# Presence of CPU", mutate: `mode=validation; duration=300; recovery=30; settle=31`, fail: true},
		{name: "soak-adversary-too-short", start: "adversary_minimum=", end: "# Presence of CPU", fail: true},
		{name: "missing-apps", start: "run validate-inputs ", end: "nodes=$(", mutate: `update seed 'del(.applications)'`, fail: true},
		{name: "missing-setup-apps", start: "run validate-inputs ", end: "nodes=$(", mutate: `update setup 'del(.applications)'`, fail: true},
		{name: "missing-turnover-apps", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover 'del(.applications)'`, fail: true},
		{name: "different-apps", start: "run validate-inputs ", end: "nodes=$(", mutate: `update turnover '.applications=12'`, fail: true},
		{name: "different-nodes", start: "run validate-inputs ", end: "nodes=$(", mutate: `update setup '.guestNodes=1'`, fail: true},
		{name: "fractional-apps", start: "run validate-inputs ", end: "nodes=$(", mutate: `for file in seed setup turnover; do update "$file" '.applications=1.5'; done`, fail: true},
		{name: "too-many-app-pods", start: "run validate-inputs ", end: "nodes=$(", mutate: `for file in seed setup turnover; do update "$file" '.applications=121'; done`, fail: true},
		{name: "three-ready-nodes", start: "run guest-node-gate ", end: "run guest-baseline "},
		{name: "wrong-node-count", start: "run guest-node-gate ", end: "run guest-baseline ", mutate: `update guest-nodes '.items=.items[:1]'`, fail: true},
		{name: "headroom", start: "run guest-capacity ", end: "run hcp-inventory "},
		{name: "headroom-exhausted", start: "run guest-capacity ", end: "run hcp-inventory ", mutate: `update guest-nodes '.items[].status.allocatable.pods="210"'`, fail: true, message: "insufficient aggregate guest Pod slots"},
		{name: "health-queries", start: "for file in queries node-queries; do", end: "jq -n --argjson maxAge"},
		{name: "missing-up", start: "for file in queries node-queries; do", end: "jq -n --argjson maxAge", mutate: `update queries 'map(select(.name!="up"))'`, fail: true, message: "queries.json requires named up and scrape-time"},
		{name: "missing-node-up", start: "for file in queries node-queries; do", end: "jq -n --argjson maxAge", mutate: `update node-queries 'map(select(.name!="up"))'`, fail: true, message: "node-queries.json requires named up and scrape-time"},
		{name: "missing-time", start: "for file in queries node-queries; do", end: "jq -n --argjson maxAge", mutate: `update queries 'map(select(.name!="scrape-time"))'`, fail: true, message: "requires named up and scrape-time"},
		{name: "wrong-selector", start: "for file in queries node-queries; do", end: "jq -n --argjson maxAge", mutate: `update queries '.[1].expr="timestamp(up{job=\"other\"})"'`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := strings.Index(script, tc.start)
			if a < 0 {
				t.Fatalf("missing block %q", tc.start)
			}
			b := strings.Index(script[a:], tc.end)
			if b < 0 {
				t.Fatalf("missing block end %q", tc.end)
			}
			harness := `
set -euo pipefail
inputs=$1 commands=$1 evidence=$1 HCP_NAMESPACE=hcp nodes=3 applications=120 max_outstanding=20
mode=soak settle=300 contended=false
die() { printf '%s\n' "$*" >&2; exit 1; }
run() { local label=$1; shift 2; "$@" >"$commands/$label.json"; }
update() { jq "$2" "$inputs/$1.json" >"$inputs/temp.json"; mv "$inputs/temp.json" "$inputs/$1.json"; }
for file in seed setup turnover; do
    jq -n '{guestNodes:3,applications:0,id:"seed",namespacePrefix:"apps",image:"image",timeoutSeconds:60,durationSeconds:7200,recoverySeconds:600,maxOutstanding:20}' >"$inputs/$file.json"
done
jq -n '{namespaces:["hcp"],enableWatch:true,durationSeconds:21600}' >"$inputs/record.json"
jq -n '{targetNamespace:"hcp",durationSeconds:480}' >"$inputs/adversaries.json"
for file in queries node-queries; do
    jq -n '[{name:"up",expr:"up"},{name:"scrape-time",expr:"timestamp(up)"}]' >"$inputs/$file.json"
done
jq -n '{items:[range(3)|{metadata:{name:("node-"+tostring)},status:{allocatable:{pods:"250"},conditions:[{type:"Ready",status:"True"}]}}]}' >"$commands/guest-nodes.json"
jq -n '{items:[range(30)|{spec:{nodeName:"node-0"},status:{phase:"Running"}}]}' >"$commands/guest-baseline.json"
`
			cmd := exec.Command("bash", "-c", harness+tc.mutate+"\n"+script[a:a+b], "test", t.TempDir())
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail || !strings.Contains(string(output), tc.message) {
				t.Fatalf("gate error: %v, want failure %v with %q; output: %s", err, tc.fail, tc.message, output)
			}
		})
	}
}

func TestCaseScriptPeriodMetadata(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(err)
		}
	}
	data, err := os.ReadFile("scripts/run-case.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start, end := strings.Index(script, "duration=$(jq"), strings.Index(script, "chmod 0400 \"$inputs/session.json\"")
	if start < 0 || end <= start {
		t.Fatal("missing session block")
	}
	for _, tc := range []struct {
		period     string
		apps, want int
	}{
		{"", 120, 10}, {"0", 120, 10}, {"3600", 120, 10}, {"1800", 120, 20},
		{"900", 120, 40}, {"600", 120, 60}, {"300", 120, 120}, {"86400", 120, 1},
		{"1", 0, 0}, {"150", 40000, 80000}, {"149", 40000, -1},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.period, tc.apps), func(t *testing.T) {
			dir := t.TempDir()
			input := `{"durationSeconds":300`
			if tc.period != "" {
				input += `,"turnoverPeriodSeconds":` + tc.period
			}
			if err := os.WriteFile(filepath.Join(dir, "turnover.json"), []byte(input+"}"), 0600); err != nil {
				t.Fatal(err)
			}
			harness := `set -euo pipefail
inputs=$1 applications=$2
mode=validation cadence= recovery=30 settle=30 nodes=3 contended=false max_outstanding=8
die() { exit 1; }
` + script[start:end]
			cmd := exec.CommandContext(t.Context(), "bash", "-c", harness, "test", dir, fmt.Sprint(tc.apps))
			output, err := cmd.CombinedOutput()
			if tc.want < 0 {
				if err == nil {
					t.Fatal("schedule cap ignored")
				}
				return
			}
			if err != nil {
				t.Fatalf("%v: %s", err, output)
			}
			data, err := os.ReadFile(filepath.Join(dir, "session.json"))
			var got struct {
				TurnoverPeriodSeconds, ScheduledRollouts, BaselineCycles, ExpectedSteadyCanaries, MaxOutstanding int
				NominalRolloutsPerHour                                                                           float64
				WorkloadRecipe                                                                                   string
			}
			if err != nil || json.Unmarshal(data, &got) != nil {
				t.Fatalf("session: %s, %v", data, err)
			}
			if got.TurnoverPeriodSeconds < 1 || got.ScheduledRollouts != tc.want || got.BaselineCycles != 5 || got.ExpectedSteadyCanaries != 10 || got.MaxOutstanding != 8 || got.NominalRolloutsPerHour != float64(tc.apps*3600)/float64(got.TurnoverPeriodSeconds) {
				t.Fatalf("wrong period metadata: %s", data)
			}
			if strings.Contains(got.WorkloadRecipe, "per hour") != (got.TurnoverPeriodSeconds == 3600) {
				t.Fatalf("wrong recipe: %s", got.WorkloadRecipe)
			}
		})
	}
}

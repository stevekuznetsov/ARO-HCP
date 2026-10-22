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

package assessment

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const failedCanary = `{
  "preflight":{"started":"2026-09-09T10:00:00Z","completed":"2026-09-09T10:00:01Z","durationNanoseconds":1000000000},
  "ack":{"started":"2026-09-09T10:00:01Z","completed":"0001-01-01T00:00:00Z","durationNanoseconds":181354000,"attempts":1},
  "ready":{"started":"2026-09-09T10:00:01Z","completed":"0001-01-01T00:00:00Z","durationNanoseconds":999999999},
  "cleanup":{"started":"2026-09-09T10:00:02Z","completed":"2026-09-09T10:00:07.632Z","durationNanoseconds":5632000000,"attempts":1},
  "deploymentUID":"deployment-1","generation":12,
  "last":{"deployment":{"uid":"deployment-1","generation":11}},
  "cleanupLast":{"observedAt":"2026-09-09T10:00:07.632Z","deployment":{"uid":"deployment-1","generation":13,"observedGeneration":13,"desired":0,"replicas":0,"updated":0,"ready":0,"available":0},"pods":null}
}`

func failureFixture(t *testing.T) Options {
	t.Helper()
	opts := fixture(t)
	writeArtifact(t, opts.Input, "commands/canary-steady-1.exit", "1")
	writeArtifact(t, opts.Input, "commands/canary-steady-1.json", failedCanary)
	writeArtifact(t, opts.Input, "commands/canary-steady-1.stderr", `{"version":"v1","error":"ack: Put https://SECRET:credential@guest.invalid/path?token=PRIVATE: context deadline exceeded"}`)
	writeArtifact(t, opts.Input, "commands/baseline-budgets.json", `{"ack":180820000,"ready":2000000000,"cleanup":9000000000}`)
	return opts
}

func TestFailureACKTimeoutAndRecovery(t *testing.T) {
	opts := failureFixture(t)
	r, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status.Workload != "fail" || r.Status.Qualification != "not_assessed" || len(r.Failures) != 1 {
		t.Fatalf("missing failed workload gate: %+v", r)
	}
	f := r.Failures[0]
	if f.Stage != "ack" || f.Operation != "Scale Deployment from 0 to 1 using PUT deployments/scale" || f.Status != "client_timeout" || f.ElapsedKind != "time_to_timeout" || f.ElapsedSeconds != .181354 || f.DeadlineSeconds == nil || *f.DeadlineSeconds != .180820 || f.CompletionObserved || f.ApplicationOutcome != "unknown" {
		t.Fatalf("incorrect failure semantics: %+v", f)
	}
	if f.Recovery.Status != "observed" || f.Recovery.ElapsedSeconds != 5.632 || !f.Recovery.ZeroReplicasObserved || !f.Recovery.NoOwnedPodsObserved || len(f.EvidenceRefs) != 4 {
		t.Fatalf("missing recovery/provenance: %+v", f)
	}
	for _, p := range r.DataPoints {
		if strings.HasPrefix(p.Name, "canary.steady.") {
			t.Fatalf("failed cycle entered successful latency population: %+v", p)
		}
	}
	statement := ""
	for _, finding := range r.Findings {
		if strings.HasPrefix(finding.Statement, "canary-steady-1:") {
			statement = finding.Statement
		}
	}
	for _, text := range []string{"ACK not observed before 180.820ms; client timed out after 181.354ms.", "Cleanup completed in 5.632s, zero replicas/no owned pods.", "Ready not reached", "cause unknown"} {
		if !strings.Contains(statement, text) {
			t.Fatalf("missing %q in %q", text, statement)
		}
	}
	found := false
	for _, c := range r.CarryForward {
		found = found || strings.Contains(c.Recommendation, "record overrun; wait to an explicit hard deadline; compare completed latency")
	}
	if !found {
		t.Fatal("missing diagnose rather than retune recommendation")
	}
	assertReferences(t, opts, r)
	data, _ := json.Marshal(r)
	for _, secret := range []string{"SECRET", "credential@", "guest.invalid", "PRIVATE", "Put https"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("stderr leaked %q", secret)
		}
	}
	decoded, err := Decode(data)
	if err != nil || !reflect.DeepEqual(decoded.Failures, r.Failures) {
		t.Fatalf("failure round trip: %v %+v", err, decoded.Failures)
	}
}

func TestHardTimeoutUsesRecordedDeadlineNotExpectation(t *testing.T) {
	for _, stderr := range []string{`{"error":"ack: context deadline exceeded"}`, `{"error":"ack: transport error"}`} {
		opts := failureFixture(t)
		body := strings.Replace(failedCanary, `"durationNanoseconds":181354000`, `"durationNanoseconds":30000000000,"expectedDurationNanoseconds":180000000,"deadlineNanoseconds":30000000000,"expectationExceeded":true`, 1)
		writeArtifact(t, opts.Input, "commands/canary-steady-1.json", body)
		writeArtifact(t, opts.Input, "commands/canary-steady-1.stderr", stderr)
		r, err := Build(context.Background(), opts)
		if err != nil || r.Status.Workload != "fail" || r.Status.Performance != "unknown" || len(r.Failures) != 1 {
			t.Fatalf("censored latency classified: %+v %v", r, err)
		}
		f := r.Failures[0]
		if f.DeadlineSeconds == nil || *f.DeadlineSeconds != 30 || f.ExpectedSeconds == nil || *f.ExpectedSeconds != .18 || f.CompletionObserved {
			t.Fatalf("hard/soft limits conflated: %+v", f)
		}
		for _, p := range r.DataPoints {
			if strings.HasPrefix(p.Name, "canary.steady.") {
				t.Fatalf("censored latency included: %+v", p)
			}
		}
		assertReferences(t, opts, r)
	}
}

func TestFailureEarliestStage(t *testing.T) {
	for _, tc := range []struct {
		name, want, elapsed string
		edit                func(map[string]any)
	}{
		{"preflight", "preflight", "1", func(d map[string]any) { d["preflight"].(map[string]any)["completed"] = nil }},
		{"ack", "ack", "0.181354", func(map[string]any) {}},
		{"ready", "ready", "0.999999999", func(d map[string]any) { d["ack"].(map[string]any)["completed"] = "2026-09-09T10:00:01.1Z" }},
		{"cleanup", "cleanup", "5.632", func(d map[string]any) {
			d["ack"].(map[string]any)["completed"] = "2026-09-09T10:00:01.1Z"
			d["ready"].(map[string]any)["completed"] = "2026-09-09T10:00:01.2Z"
			d["cleanup"].(map[string]any)["completed"] = nil
		}},
		{"no ACK attempt", "unknown", "0", func(d map[string]any) { d["ack"].(map[string]any)["attempts"] = 0 }},
		{"missing preflight", "unknown", "0", func(d map[string]any) { delete(d, "preflight") }},
		{"all completed", "unknown", "0", func(d map[string]any) {
			d["ack"].(map[string]any)["completed"] = "2026-09-09T10:00:01.1Z"
			d["ready"].(map[string]any)["completed"] = "2026-09-09T10:00:01.2Z"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := failureFixture(t)
			var d map[string]any
			_ = json.Unmarshal([]byte(failedCanary), &d)
			tc.edit(d)
			data, _ := json.Marshal(d)
			writeArtifact(t, opts.Input, "commands/canary-steady-1.json", string(data))
			writeArtifact(t, opts.Input, "commands/canary-steady-1.stderr", `{"error":"client error"}`)
			r, err := Build(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			f := r.Failures[0]
			elapsed, _ := json.Marshal(f.ElapsedSeconds)
			if f.Stage != tc.want || string(elapsed) != tc.elapsed || f.ElapsedKind != "time_until_error" || f.CompletionObserved {
				t.Fatalf("wrong stage/elapsed: %+v", f)
			}
			if tc.want == "preflight" && (f.DeadlineSeconds != nil || !strings.Contains(f.Operation, "without mutation")) {
				t.Fatalf("preflight used fitted mutation budget: %+v", f)
			}
		})
	}
}

func TestFailureRecoveryRequiresExplicitEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		from string
		to   string
		want string
	}{
		{"null pods", `"pods":null`, `"pods":null`, "observed"},
		{"empty pods", `"pods":null`, `"pods":[]`, "observed"},
		{"write not applied", `"generation":13,"observedGeneration":13`, `"generation":11,"observedGeneration":11`, "observed"},
		{"missing pods", `,"pods":null`, ``, "unknown"},
		{"object pods", `"pods":null`, `"pods":{}`, "unknown"},
		{"remaining pod", `"pods":null`, `"pods":[{}]`, "unknown"},
		{"missing count", `,"replicas":0`, ``, "unknown"},
		{"null count", `"desired":0`, `"desired":null`, "unknown"},
		{"wrong count type", `"available":0`, `"available":"0"`, "unknown"},
		{"nonzero count", `"updated":0`, `"updated":1`, "unknown"},
		{"stale observation", `"observedGeneration":13`, `"observedGeneration":12`, "unknown"},
		{"missing generation", `"generation":13,`, ``, "unknown"},
		{"replaced UID", `"uid":"deployment-1","generation":13`, `"uid":"replacement","generation":13`, "unknown"},
		{"no identity", `"deploymentUID":"deployment-1"`, `"deploymentUID":""`, "unknown"},
		{"no prior snapshot", `"last":{"deployment":{"uid":"deployment-1","generation":11}}`, `"last":{}`, "unknown"},
		{"empty cleanup snapshot", `"cleanupLast":{"observedAt":"2026-09-09T10:00:07.632Z","deployment":{"uid":"deployment-1","generation":13,"observedGeneration":13,"desired":0,"replicas":0,"updated":0,"ready":0,"available":0},"pods":null}`, `"cleanupLast":{}`, "unknown"},
		{"incomplete cleanup", `"completed":"2026-09-09T10:00:07.632Z"`, `"completed":null`, "unknown"},
		{"zero duration", `"durationNanoseconds":5632000000`, `"durationNanoseconds":0`, "unknown"},
		{"no observation time", `"observedAt":"2026-09-09T10:00:07.632Z"`, `"observedAt":null`, "unknown"},
		{"future completion", `"completed":"2026-09-09T10:00:07.632Z"`, `"completed":"2099-01-01T00:00:00Z"`, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := failureFixture(t)
			writeArtifact(t, opts.Input, "commands/canary-steady-1.json", strings.Replace(failedCanary, tc.from, tc.to, 1))
			r, err := Build(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			f := r.Failures[0]
			if f.Recovery.Status != tc.want || (tc.want == "unknown" && (f.Recovery.ZeroReplicasObserved || f.Recovery.NoOwnedPodsObserved || f.Recovery.ElapsedSeconds != 0)) {
				t.Fatalf("recovery guessed from incomplete evidence: %+v", f)
			}
			assertReferences(t, opts, r)
		})
	}
}

func TestFailureInvalidBudgetsAndStderr(t *testing.T) {
	for _, value := range []string{`null`, `"180820000"`, `true`, `0`, `-1`, `1.5`, `9223372036854775808`} {
		t.Run("budget="+value, func(t *testing.T) {
			opts := failureFixture(t)
			writeArtifact(t, opts.Input, "commands/baseline-budgets.json", `{"ack":`+value+`,"ready":2000000000,"cleanup":9000000000}`)
			r, err := Build(context.Background(), opts)
			if err != nil || r.Failures[0].DeadlineSeconds != nil {
				t.Fatalf("invalid budget used: %v %+v", err, r.Failures)
			}
			assertReferences(t, opts, r)
		})
	}
	for _, body := range []string{`{"error":null}`, `{"error":3}`, `{"other":"context deadline exceeded"}`, `{"error":"rejected\ncleanup: context deadline exceeded"}`, `context deadline exceeded SECRET`, `{"error":"SECRET 403 Forbidden"}`, `{}`} {
		t.Run(body, func(t *testing.T) {
			opts := failureFixture(t)
			writeArtifact(t, opts.Input, "commands/canary-steady-1.stderr", body)
			r, err := Build(context.Background(), opts)
			if err != nil || r.Failures[0].Status != "error" || r.Failures[0].ApplicationOutcome != "unknown" {
				t.Fatalf("error classification guessed: %v %+v", err, r.Failures)
			}
			data, _ := json.Marshal(r)
			if strings.Contains(string(data), "SECRET") {
				t.Fatal("raw error leaked")
			}
		})
	}
}

func TestFailureMissingArtifactsAndBaselineDeadline(t *testing.T) {
	for _, command := range []string{"canary-steady-2", "canary-baseline-2", "canary-warmup", "turnover-gate"} {
		t.Run(command, func(t *testing.T) {
			opts := fixture(t)
			writeArtifact(t, opts.Input, "commands/"+command+".exit", "1")
			r, err := Build(context.Background(), opts)
			if err != nil || len(r.Failures) != 1 || r.Failures[0].Command != command || r.Failures[0].Stage != "unknown" || r.Status.Workload != "fail" {
				t.Fatalf("failed marker omitted without details: %v %+v", err, r)
			}
			assertReferences(t, opts, r)
		})
	}
	opts := fixture(t)
	writeArtifact(t, opts.Input, "commands/canary-baseline-1.exit", "1")
	writeArtifact(t, opts.Input, "commands/canary-baseline-1.json", failedCanary)
	writeArtifact(t, opts.Input, "commands/baseline-budgets.json", `{"ack":180820000,"ready":2000000000,"cleanup":9000000000}`)
	r, err := Build(context.Background(), opts)
	if err != nil || r.Failures[0].Stage != "ack" || r.Failures[0].DeadlineSeconds != nil {
		t.Fatalf("post-baseline fitted budget used for baseline: %v %+v", err, r.Failures)
	}
}

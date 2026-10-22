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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/assessment"
)

func TestAssessmentCLIExclusiveOfflineOutput(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "assessment.json")
	args := []string{"assessment", "build", "--input", dir, "--metrics", filepath.Join(dir, "absent-metrics"), "--metrics", filepath.Join(dir, "other-metrics"), "--output", output}
	var out bytes.Buffer
	if err := runAssessment(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	r, err := assessment.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status.Workload != "incomplete" || r.Status.Qualification != "not_assessed" {
		t.Fatalf("unexpected assessment %+v", r.Status)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
	if err := runAssessment(context.Background(), args, &out); err == nil {
		t.Fatal("overwrote existing output")
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(data, after) {
		t.Fatal("existing assessment changed")
	}
}

func TestAssessmentCLIValidation(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "assessment.json")
	for _, args := range [][]string{
		{}, {"assessment", "render"}, {"assessment", "build", "--input", dir},
		{"assessment", "build", "--output", output},
		{"assessment", "build", "--input", dir, "--output", output, "extra"},
		{"assessment", "build", "--input", dir, "--output", output, "--metrics", ""},
		{"assessment", "build", "--input", dir, "--output", output, "--notes", "unsupported"},
	} {
		if err := runAssessment(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted invalid args: %v", args)
		}
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("invalid invocation wrote output: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runAssessment(ctx, []string{"assessment", "build", "--input", dir, "--output", output}, &bytes.Buffer{}); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestAssessmentCLIOutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name, execution, experiment, reason, capture string
		workloadExit, exit                           int
		invalid                                      bool
	}{
		{"soft-metrics-failure", "completed", "degraded", "completed", "incomplete", 0, 1, false},
		{"terminal-gate", "completed", "violations", "expectation_violation", "complete", 1, 1, false},
		{"hard-canary", "aborted", "violations", "workload_aborted", "complete", 1, 1, false},
		{"safety-signal", "aborted", "unknown", "signal_TERM", "complete", 143, 143, false},
		{"input", "harness_error", "unknown", "input_error", "unknown", 1, 1, false},
		{"unknown-class", "success", "violations", "completed", "complete", 0, 0, true},
		{"contradictory-exit", "completed", "within_expectations", "completed", "complete", 1, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "evidence"), 0700); err != nil {
				t.Fatal(err)
			}
			doc := map[string]any{"exitCode": tc.exit, "workloadExitCode": tc.workloadExit, "shutdownOrExportFailed": tc.capture == "incomplete",
				"executionStatus": tc.execution, "experimentStatus": tc.experiment, "stopReason": tc.reason, "captureStatus": tc.capture, "shutdownStatus": "complete",
				"expectationViolations": map[string]any{"degradedCanaries": nil, "missedCanarySlots": nil, "completedAfterWindow": nil, "safety": nil}}
			body, _ := json.Marshal(doc)
			if err := os.WriteFile(filepath.Join(dir, "evidence/outcome.json"), body, 0600); err != nil {
				t.Fatal(err)
			}
			ledger := `{"label":"metrics-capture","exitCode":1,"classification":"capture_error","phase":"finalization","stderr":"PRIVATE-CONTENT"}`
			if err := os.WriteFile(filepath.Join(dir, "evidence/failures.jsonl"), []byte(ledger), 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "assessment.json")
			var out bytes.Buffer
			if err := runAssessment(t.Context(), []string{"assessment", "build", "--input", dir, "--output", output}, &out); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			r, err := assessment.Decode(body)
			if err != nil {
				t.Fatal(err)
			}
			if tc.invalid {
				if r.Outcome != nil || r.Status.Execution != "unknown" {
					t.Fatalf("invalid outcome trusted: %s", body)
				}
			} else if r.Outcome == nil || r.Status.Execution != tc.execution || r.Status.Experiment != tc.experiment || r.Status.Qualification != "not_assessed" {
				t.Fatalf("terminal result lost: %s", body)
			}
			if len(r.CommandProblems) != 1 || strings.Contains(string(body), "PRIVATE-CONTENT") {
				t.Fatalf("unsafe/lost command classification: %s", body)
			}
			if !strings.Contains(out.String(), `"execution"`) {
				t.Fatalf("CLI status omits execution axis: %s", &out)
			}
		})
	}
}

func requestEvidenceCLIFixture(t *testing.T) (string, string) {
	t.Helper()
	input, review := t.TempDir(), t.TempDir()
	put := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(input, "commands/canary-steady-7.exit"), "1")
	put(filepath.Join(input, "commands/canary-steady-7.json"), `{"deploymentUID":"9fd9e977-a86b-42fc-9b5a-00265fd8ab95","preflight":{"started":"2023-11-14T22:13:20Z","completed":"2023-11-14T22:13:21Z","durationNanoseconds":1000000000},"ack":{"started":"2023-11-14T22:13:21Z","durationNanoseconds":181354000,"attempts":1}}`)
	put(filepath.Join(input, "commands/canary-steady-7.stderr"), `{"error":"ack: context deadline exceeded"}`)
	put(filepath.Join(input, "commands/baseline-budgets.json"), `{"ack":180820000,"ready":1000000000,"cleanup":1000000000}`)
	const source = "PRIVATE opaque audit bytes"
	put(filepath.Join(review, "record/logs.ndjson"), source)
	hash := sha256.Sum256([]byte(source))
	doc := map[string]any{"schemaVersion": assessment.RequestEvidenceVersion, "observations": []any{map[string]any{
		"command": "canary-steady-7", "auditID": "705d557d-8564-4104-b940-865da7ab45a3", "verb": "update", "path": "/apis/apps/v1/namespaces/hcp-perf-canary-test/deployments/canary/scale",
		"objectUID": "9fd9e977-a86b-42fc-9b5a-00265fd8ab95", "responseCode": 200, "stage": "ResponseComplete",
		"receivedAt": "2023-11-14T22:13:21.678180Z", "completedAt": "2023-11-14T22:13:21.786989Z",
		"sourcePath": "record/logs.ndjson", "sourceSHA256": hex.EncodeToString(hash[:]), "correlation": "path-uid-time-serial",
	}}}
	body, _ := json.Marshal(doc)
	path := filepath.Join(review, "request-evidence-v1.json")
	put(path, string(body))
	return input, path
}

func TestAssessmentCLIRequestEvidence(t *testing.T) {
	input, request := requestEvidenceCLIFixture(t)
	output := filepath.Join(t.TempDir(), "assessment.json")
	args := []string{"assessment", "build", "--input", input, "--request-evidence", request, "--output", output}
	if err := runAssessment(context.Background(), args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	r, err := assessment.Decode(body)
	if err != nil || r.Failures[0].ServerObservation == nil || r.Failures[0].ServerObservation.ResponseCode != 200 || r.Status.Workload != "fail" || strings.Contains(string(body), "PRIVATE opaque") {
		t.Fatalf("audit evidence not safely passed through CLI: %v %+v", err, r)
	}
	badOutput := filepath.Join(t.TempDir(), "invalid.json")
	args = []string{"assessment", "build", "--input", input, "--request-evidence", request + ".missing", "--output", badOutput}
	if err := runAssessment(context.Background(), args, &bytes.Buffer{}); err == nil {
		t.Fatal("missing explicit request evidence accepted")
	}
	if _, err := os.Stat(badOutput); !os.IsNotExist(err) {
		t.Fatal("invalid request evidence wrote output")
	}
}

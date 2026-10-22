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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func requestEvidenceFixture(t *testing.T) (Options, map[string]any, map[string]any) {
	t.Helper()
	opts := failureFixture(t)
	dir := t.TempDir()
	// Opaque source intentionally is not parsed or copied into the assessment.
	const source = "PRIVATE LOG CONTENT\n"
	hash := sha256.Sum256([]byte(source))
	writeArtifact(t, dir, "record/logs.ndjson", source)
	o := map[string]any{
		"command": "canary-steady-1", "auditID": "705d557d-8564-4104-b940-865da7ab45a3", "verb": "update",
		"path": "/apis/apps/v1/namespaces/hcp-perf-canary-test/deployments/canary/scale", "objectUID": "deployment-1",
		"responseCode": 200, "stage": "ResponseComplete", "receivedAt": "2026-09-09T10:00:01.678180Z", "completedAt": "2026-09-09T10:00:01.786989Z",
		"sourcePath": "record/logs.ndjson", "sourceSHA256": hex.EncodeToString(hash[:]), "sourceLine": 101551, "correlation": "path-uid-time-serial",
	}
	doc := map[string]any{"schemaVersion": RequestEvidenceVersion, "observations": []any{o}, "artifactReferences": []any{map[string]any{"role": "cli-artifact", "path": "../PRIVATE-DO-NOT-READ", "sha256": strings.Repeat("0", 64)}}}
	opts.RequestEvidence = filepath.Join(dir, "request-evidence-v1.json")
	return opts, doc, o
}

func writeRequestEvidence(t *testing.T, opts Options, doc map[string]any) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	writeArtifact(t, filepath.Dir(opts.RequestEvidence), filepath.Base(opts.RequestEvidence), string(data))
}

func TestRequestEvidenceSuccessIsNotClientReceipt(t *testing.T) {
	opts, doc, _ := requestEvidenceFixture(t)
	writeRequestEvidence(t, opts, doc)
	r, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	f := r.Failures[0]
	s := f.ServerObservation
	if s == nil || s.ResponseCode != 200 || s.DurationSeconds != .108809 || s.Provenance != "reviewer-correlated source-hash-verified" || s.Correlation != "path-uid-time-serial" || s.SourceLine == nil || *s.SourceLine != 101551 {
		t.Fatalf("lost server observation: %+v", s)
	}
	if f.ApplicationOutcome != "server_success_observed" || f.Status != "client_timeout" || f.CompletionObserved || f.ElapsedSeconds != .181354 || f.ElapsedKind != "time_to_timeout" || r.Status.Workload != "fail" || r.Status.Qualification != "not_assessed" {
		t.Fatalf("server report changed client gate: %+v %+v", f, r.Status)
	}
	data, _ := json.Marshal(r)
	text := string(data)
	for _, want := range []string{"HTTP 200 in 108.809ms; client ACK not observed before 180.820ms", "not shared client request-ID correlation", "not independently extracted", "no cross-clock latency subtraction"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing qualification %q", want)
		}
	}
	for _, unwanted := range []string{"PRIVATE LOG CONTENT", "PRIVATE-DO-NOT-READ", "Application outcome and cause unknown", "linked server evidence unavailable"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("leaked source or retained stale conclusion %q", unwanted)
		}
	}
	assertReferences(t, opts, r)
	decoded, err := Decode(data)
	if err != nil || !reflect.DeepEqual(decoded.Failures, r.Failures) {
		t.Fatalf("roundtrip changed server evidence: %v", err)
	}
	second, err := Build(context.Background(), opts)
	if err != nil || !reflect.DeepEqual(r, second) {
		t.Fatalf("request evidence build not deterministic: %v", err)
	}
}

func TestRequestEvidenceRejectsInvalidObservations(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any, map[string]any)
	}{
		{"wrong version", func(d, o map[string]any) { d["schemaVersion"] = "hcp-perf.request-evidence/v2" }},
		{"no observations", func(d, o map[string]any) { delete(d, "observations") }},
		{"empty observations", func(d, o map[string]any) { d["observations"] = []any{} }},
		{"unknown command", func(d, o map[string]any) { o["command"] = "canary-steady-99" }},
		{"missing command", func(d, o map[string]any) { delete(o, "command") }},
		{"wrong UID", func(d, o map[string]any) { o["objectUID"] = "replacement" }},
		{"missing UID", func(d, o map[string]any) { delete(o, "objectUID") }},
		{"wrong verb", func(d, o map[string]any) { o["verb"] = "get" }},
		{"wrong stage", func(d, o map[string]any) { o["stage"] = "RequestReceived" }},
		{"wrong path", func(d, o map[string]any) { o["path"] = "/apis/apps/v1/namespaces/test/deployments/other/scale" }},
		{"path traversal", func(d, o map[string]any) { o["path"] = "/apis/apps/v1/namespaces/../deployments/canary/scale" }},
		{"full URL", func(d, o map[string]any) { o["path"] = "https://SECRET@host" + o["path"].(string) }},
		{"query credentials", func(d, o map[string]any) { o["path"] = o["path"].(string) + "?token=SECRET" }},
		{"invalid audit ID", func(d, o map[string]any) { o["auditID"] = "SECRET" }},
		{"guessed correlation", func(d, o map[string]any) { o["correlation"] = "exact-request-id" }},
		{"negative time", func(d, o map[string]any) { o["completedAt"] = "2026-09-09T09:00:00Z" }},
		{"missing time", func(d, o map[string]any) { delete(o, "receivedAt") }},
		{"future time", func(d, o map[string]any) { o["completedAt"] = "2099-01-01T00:00:00Z" }},
		{"low code", func(d, o map[string]any) { o["responseCode"] = 99 }},
		{"high code", func(d, o map[string]any) { o["responseCode"] = 600 }},
		{"string code", func(d, o map[string]any) { o["responseCode"] = "200" }},
		{"fractional code", func(d, o map[string]any) { o["responseCode"] = 200.5 }},
		{"null code", func(d, o map[string]any) { o["responseCode"] = nil }},
		{"bad source hash", func(d, o map[string]any) { o["sourceSHA256"] = "invalid" }},
		{"mismatched source hash", func(d, o map[string]any) { o["sourceSHA256"] = strings.Repeat("0", 64) }},
		{"source escape", func(d, o map[string]any) { o["sourcePath"] = "../logs.ndjson" }},
		{"absolute source", func(d, o map[string]any) { o["sourcePath"] = "/tmp/logs.ndjson" }},
		{"missing source", func(d, o map[string]any) { o["sourcePath"] = "absent.ndjson" }},
		{"noncanonical source", func(d, o map[string]any) { o["sourcePath"] = "record/../record/logs.ndjson" }},
		{"negative line", func(d, o map[string]any) { o["sourceLine"] = -1 }},
		{"duplicate observation", func(d, o map[string]any) { d["observations"] = []any{o, o} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, doc, observation := requestEvidenceFixture(t)
			tc.edit(doc, observation)
			writeRequestEvidence(t, opts, doc)
			_, err := Build(context.Background(), opts)
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("invalid observation accepted or error leaked private fields: %v", err)
			}
		})
	}
}

func TestRequestEvidenceSourceSafetyAndNonSuccess(t *testing.T) {
	for _, code := range []int{100, 200, 201, 299, 300, 403, 500, 599} {
		opts, doc, observation := requestEvidenceFixture(t)
		observation["responseCode"] = code
		delete(observation, "sourceLine")
		writeRequestEvidence(t, opts, doc)
		r, err := Build(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := "unknown"
		if code >= 200 && code < 300 {
			want = "server_success_observed"
		}
		if r.Failures[0].ApplicationOutcome != want || r.Failures[0].ServerObservation.ResponseCode != code || r.Status.Workload != "fail" {
			t.Fatalf("code %d changed client gate or guessed application: %+v", code, r.Failures[0])
		}
	}
	for _, parent := range []bool{false, true} {
		opts, doc, observation := requestEvidenceFixture(t)
		dir := filepath.Dir(opts.RequestEvidence)
		old, alias := "record/logs.ndjson", "alias.ndjson"
		if parent {
			old, alias = "record", "alias"
			observation["sourcePath"] = "alias/logs.ndjson"
		} else {
			observation["sourcePath"] = alias
		}
		if err := os.Symlink(old, filepath.Join(dir, alias)); err != nil {
			t.Fatal(err)
		}
		writeRequestEvidence(t, opts, doc)
		if _, err := Build(context.Background(), opts); err == nil {
			t.Fatal("source symlink accepted")
		}
	}
	opts, _, _ := requestEvidenceFixture(t)
	if _, err := Build(context.Background(), opts); err == nil {
		t.Fatal("explicit missing request evidence silently ignored")
	}
}

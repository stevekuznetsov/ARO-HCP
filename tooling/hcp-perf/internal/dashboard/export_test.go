// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func TestSourceAllowlist(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("{}")
	if err := os.WriteFile(filepath.Join(dir, "source.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	r := &sourceReader{ctx: context.Background(), root: root, sources: map[string]Source{}}
	ref := InputRef{Path: "source.json", SHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), ID: "source", Label: "Reviewed source"}
	if _, err := r.read("", ref); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../source.json", "/etc/passwd", "https://example.invalid/source"} {
		bad := ref
		bad.Path = path
		if _, err := r.read("", bad); err == nil {
			t.Fatalf("unsafe/unavailable path accepted: %s", path)
		}
	}
	bad := ref
	bad.SHA256 = "00"
	if _, err := r.read("", bad); err == nil {
		t.Fatal("malformed hash accepted")
	}
	bad.SHA256 = fmt.Sprintf("%064d", 0)
	if _, err := r.read("", bad); err == nil {
		t.Fatal("wrong hash accepted")
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	bad = ref
	bad.Path = "escape"
	if _, err := r.read("", bad); err == nil {
		t.Fatal("symlink escaped private root")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.ctx = ctx
	if _, err := r.read("", ref); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestRequestUnionAndCorruption(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	api := &apiload.Result{RequestAccountingVersion: "v1", Started: start, SchedulingStopped: start.Add(time.Second), Finished: start.Add(2 * time.Second), Spec: apiload.Spec{Targets: []apiload.Target{{Verb: "get", Resource: "deployments"}}}, Counts: apiload.Counts{Offered: 2, Dispatched: 2, Completed: 2, Successful: 1, Rejected429: 1, Timeouts: 1, DegradedRequests: 1, HTTPAttempts: 2}}
	raw := []byte("{\"sequence\":1,\"target\":0,\"statusCode\":200,\"httpAttempts\":1,\"latencyNanoseconds\":1000000000}\n{\"sequence\":2,\"target\":0,\"statusCode\":429,\"errClass\":\"timeout\",\"httpAttempts\":1}\n")
	var run Run
	if err := exportRequests(&run, api, raw); err != nil {
		t.Fatal(err)
	}
	if run.Requests.DegradedUnion != 1 || run.Requests.SuccessfulQPS != .5 || run.Latencies[0].Distribution.Count != 1 {
		t.Fatalf("incorrect request cohort: %+v", run)
	}
	api.Successful++
	if err := exportRequests(&run, api, raw); err == nil {
		t.Fatal("corrupt summary accepted")
	}
}

func TestCanaryCensoring(t *testing.T) {
	raw := []byte(`{"kind":"baseline-canary","value":{}}
{"kind":"measured-canary","value":{"ack":{"started":"2026-01-01T00:00:00Z","completed":"2026-01-01T00:00:01Z","durationNanoseconds":1000000000},"ready":{"started":"2026-01-01T00:00:00Z","durationNanoseconds":120000000000},"cleanup":{}}}`)
	var run Run
	if err := exportCanaries(&run, raw); err != nil {
		t.Fatal(err)
	}
	if run.CanaryCycles != 1 || run.Latencies[0].Distribution.Median != 1 || run.Latencies[1].Distribution != nil || run.Latencies[1].Censored != 1 || run.Latencies[2].Censored != 1 {
		t.Fatalf("censored stages became measurements: %+v", run)
	}
}

func TestMeasuredCanaryFailures(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stage := workload.Stage{Started: start, Completed: start.Add(time.Second), Duration: time.Second}
	passed := workload.Result{Preflight: stage, Ack: stage, Ready: stage, Cleanup: stage}
	failed := passed
	failed.Cleanup.Completed = time.Time{}
	failed.Cleanup.Duration = 120 * time.Second
	failed.Degraded = true
	allFailed := failed
	allFailed.Ack.Completed, allFailed.Ready.Completed = time.Time{}, time.Time{}
	event := func(kind string, value any) []byte {
		b, err := json.Marshal(map[string]any{"kind": kind, "value": value})
		if err != nil {
			t.Fatal(err)
		}
		return append(b, '\n')
	}
	zero, one := 0, 1
	for _, tc := range []struct {
		name         string
		raw          []byte
		want         *int
		cycles, soft int
	}{
		{"strict cleanup failure", event("measured-canary", failed), &one, 1, 1},
		{"multiple failed stages count once", event("measured-canary", allFailed), &one, 1, 1},
		{"recovery-only failure excluded", append(event("measured-canary", passed), event("recovery-probe", failed)...), &zero, 1, 0},
		{"no measured evidence", event("recovery-probe", failed), nil, 0, 0},
		{"absent result", event("measured-canary", nil), nil, 1, 0},
		{"missing stages", event("measured-canary", map[string]any{"ack": stage}), nil, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale := 99
			run := Run{CanaryHardFailed: &stale, CanaryCycles: 99, CanarySoftDegraded: 99}
			if err := exportCanaries(&run, tc.raw); err != nil {
				t.Fatal(err)
			}
			if (run.CanaryHardFailed == nil) != (tc.want == nil) || (tc.want != nil && *run.CanaryHardFailed != *tc.want) || run.CanaryCycles != tc.cycles || run.CanarySoftDegraded != tc.soft {
				t.Fatalf("unexpected measured counts: %+v", run)
			}
		})
	}
}

func TestNullableRequestPolicy(t *testing.T) {
	stale := .05
	run := Run{Requests: Requests{MaxDegradedFraction: &stale}}
	if err := exportRequestPolicy(&run, nil); err != nil {
		t.Fatal(err)
	}
	if run.Requests.MaxDegradedFraction != nil || len(run.Notes) != 1 {
		t.Fatal("legacy strict policy retained a fabricated union threshold")
	}
	for _, bps := range []int{0, 500} {
		if err := exportRequestPolicy(&run, &requestQualityPolicy{MaxDegradedBasisPoints: bps}); err != nil {
			t.Fatal(err)
		}
		if run.Requests.MaxDegradedFraction == nil || *run.Requests.MaxDegradedFraction != float64(bps)/10000 {
			t.Fatal("explicit policy lost, including explicit zero")
		}
	}
	run.Requests.MaxDegradedFraction = nil
	b, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"maxDegradedFraction":null`)) || !bytes.Contains(b, []byte(`"canaryHardFailed":null`)) {
		t.Fatal("unknown fields must serialize as null")
	}
}

// Optional read-only replay of an explicitly selected private manifest. Normal
// tests never discover artifacts or depend on a developer's workspace.
func TestExportSavedManifest(t *testing.T) {
	path := os.Getenv("HCP_DASHBOARD_TEST_MANIFEST")
	if path == "" {
		t.Skip("no explicit private manifest")
	}
	spec, err := ReadExportSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	dataset, err := Export(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(dataset)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range dataset.Sizes {
		for _, run := range size.Runs {
			switch run.ID {
			case "envelope-loaded5-hour-001":
				if run.CanaryHardFailed == nil || *run.CanaryHardFailed != 1 {
					t.Fatal("strict cleanup failure missing")
				}
			case "envelope-observe5-hour-004":
				if run.CanaryHardFailed == nil || *run.CanaryHardFailed != 0 || run.CanaryCycles != 48 {
					t.Fatal("latest measured canary accounting changed")
				}
			case "cpo144-hour1-001", "cpo144-pinned-hour3-001":
				if run.Requests.MaxDegradedFraction != nil {
					t.Fatal("historical strict policy has fabricated union threshold")
				}
			}
			for _, usage := range run.Resources {
				if usage.CPU == nil || usage.Memory == nil {
					t.Logf("%s missing %s cpu=%v memory=%v", run.ID, usage.ComponentID, usage.CPU != nil, usage.Memory != nil)
				}
			}
			t.Logf("%s: success=%d/%d resources=%d whole=%+v", run.ID, run.Requests.Successful, run.Requests.Dispatched, len(run.Resources), run.WholeHCP)
		}
	}
	t.Logf("sanitized dataset bytes=%d sources=%d", len(b), len(dataset.Sources))
	for _, secret := range []string{"/home/", "/subscriptions/", "ocm-arohcp", "Bearer ", "https://"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("private identifier/address leaked: %s", secret)
		}
	}
	if len(b) >= MaxInputBytes {
		t.Fatal("dataset exceeds the portable input bound")
	}
	if path := os.Getenv("HCP_DASHBOARD_TEST_DATASET"); path != "" {
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.TrimSpace(saved), b) {
			t.Fatal("saved dataset differs from deterministic manifest replay")
		}
	}
}

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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
)

func writeArtifact(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) Options {
	t.Helper()
	opts := Options{Input: t.TempDir(), Now: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
	writeArtifact(t, opts.Input, "inputs/seed.json", `{"version":"v1","id":"trial-1","guestNodes":3,"applications":3}`)
	writeArtifact(t, opts.Input, "inputs/session.json", `{"mode":"validation","steadySeconds":60,"baselineCycles":1,"expectedSteadyCanaries":1,"scheduledRollouts":1}`)
	writeArtifact(t, opts.Input, "evidence/run.json", `{"mode":"validation","applicationPods":15,"contendedRequested":false}`)
	writeArtifact(t, opts.Input, "commands/turnover.json", `{"version":"v1","startedAt":"2026-09-09T10:00:00Z","admissionEnd":"2026-09-09T10:01:00Z","finishedAt":"2026-09-09T10:01:00Z","scheduled":1,"completed":1,"failed":0,"notStarted":0,"backlog":0,"outstanding":0}`)
	writeArtifact(t, opts.Input, "evidence/outcome.json", `{"exitCode":0,"shutdownOrExportFailed":false}`)
	for _, phase := range []string{"baseline", "steady"} {
		s := stage{Started: opts.Now.Add(-2 * time.Hour), Completed: opts.Now.Add(-2*time.Hour + time.Second), Duration: int64(time.Second)}
		data, _ := json.Marshal(map[string]any{"ack": s, "ready": s, "cleanup": s})
		writeArtifact(t, opts.Input, "commands/canary-"+phase+"-1.json", string(data))
		writeArtifact(t, opts.Input, "commands/canary-"+phase+"-1.exit", "0\n")
	}
	return opts
}

func TestAssessmentWorkloadAndCaptureIndependent(t *testing.T) {
	opts := fixture(t)
	writeArtifact(t, opts.Input, "evidence/outcome.json", `{"exitCode":1,"workloadExitCode":0,"shutdownOrExportFailed":true}`)
	writeArtifact(t, opts.Input, "commands/turnover-gate.exit", "0\n")
	opts.RecorderSummary = filepath.Join(t.TempDir(), "stream-summary.json")
	writeArtifact(t, filepath.Dir(opts.RecorderSummary), "stream-summary.json", `{"enableWatch":true,"enableLogs":true,"incomplete":true,"watch":{"required":4,"initialized":4,"errors":0,"gaps":0},"logs":{"errors":642,"gaps":216}}`)
	r, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != (Status{Execution: "unknown", Experiment: "unknown", Workload: "pass", Performance: "unknown", Capture: "incomplete", Qualification: "not_assessed"}) {
		t.Fatalf("separate axes: %+v", r.Status)
	}
	if r.Experiment.MeasurementMode != "unspecified" || r.Experiment.Mode != "validation" || r.Experiment.CPUTreatment != "quiet" || r.Experiment.Pods == nil || *r.Experiment.Pods != 15 {
		t.Fatalf("experiment: %+v", r.Experiment)
	}
	found := map[string]float64{}
	for _, p := range r.DataPoints {
		found[p.Name+"."+p.Statistic] = p.Value
	}
	for name, want := range map[string]float64{"recorder.logs.errors.count": 642, "recorder.logs.gaps.count": 216, "turnover.completed.count": 1, "canary.baseline.ready.median": 1, "canary.steady.ready.max": 1} {
		if found[name] != want {
			t.Fatalf("%s=%v, want %v", name, found[name], want)
		}
	}
	assertReferences(t, opts, r)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Status, r.Status) {
		t.Fatal("round trip changed status")
	}
	r2, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	data2, _ := json.Marshal(r2)
	if string(data) != string(data2) {
		t.Fatal("fixed clock build must be reproducible")
	}
}

func assertReferences(t *testing.T, opts Options, r Result) {
	t.Helper()
	evidence := map[string]bool{}
	points := map[string]bool{}
	for _, e := range r.Evidence {
		if filepath.IsAbs(e.Path) || evidence[e.ID] {
			t.Fatalf("invalid evidence identity/path: %+v", e)
		}
		data, err := os.ReadFile(filepath.Join(opts.Input, filepath.FromSlash(e.Path)))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if e.SHA256 != hex.EncodeToString(hash[:]) {
			t.Fatalf("incorrect content hash for %s", e.Path)
		}
		evidence[e.ID] = true
	}
	check := func(refs []string) {
		for _, ref := range refs {
			if !evidence[ref] {
				t.Fatalf("dangling evidence ref %s", ref)
			}
		}
	}
	for _, p := range r.DataPoints {
		check(p.Provenance.EvidenceRefs)
		if points[p.ID] {
			t.Fatalf("duplicate point ID %s", p.ID)
		}
		points[p.ID] = true
	}
	for _, f := range r.Findings {
		check(f.Evidence)
	}
	for _, f := range r.Failures {
		check(f.EvidenceRefs)
		if f.ServerObservation != nil {
			check(f.ServerObservation.EvidenceRefs)
		}
	}
	for _, c := range r.CarryForward {
		check(c.EvidenceRefs)
		for _, id := range c.DataPointRefs {
			if !points[id] {
				t.Fatalf("dangling point ref %s", id)
			}
		}
	}
	for _, c := range r.MetricsCoverage {
		check(c.EvidenceRefs)
	}
}

func TestAssessmentMissingFutureAndMalformedNeverPass(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"empty", "", ""},
		{"empty outcome", "evidence/outcome.json", `{}`},
		{"future version", "inputs/session.json", `{"version":"v2","baselineCycles":1,"expectedSteadyCanaries":1,"steadySeconds":60}`},
		{"future canary", "commands/canary-steady-1.json", `{"ack":{"started":"2099-01-01T00:00:00Z","completed":"2099-01-01T00:00:01Z","durationNanoseconds":1}}`},
		{"empty turnover", "commands/turnover.json", `{}`},
		{"future turnover", "commands/turnover.json", `{"version":"v2","scheduled":0,"completed":0}`},
		{"null outcome", "evidence/outcome.json", `null`},
		{"bad exit", "commands/canary-steady-1.exit", "garbage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := fixture(t)
			if tc.name == "empty" {
				opts.Input = t.TempDir()
			} else {
				writeArtifact(t, opts.Input, tc.path, tc.body)
			}
			r, err := Build(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status.Workload != "incomplete" || r.Status.Capture != "unknown" || r.Status.Qualification != "not_assessed" {
				t.Fatalf("missing evidence became success/failure: %+v", r.Status)
			}
		})
	}
}

func TestAssessmentFailureAndUnknownMetadata(t *testing.T) {
	opts := fixture(t)
	writeArtifact(t, opts.Input, "commands/canary-steady-1.exit", "1")
	writeArtifact(t, opts.Input, "inputs/session.json", `{"steadySeconds":60,"baselineCycles":1,"expectedSteadyCanaries":1}`)
	writeArtifact(t, opts.Input, "evidence/run.json", `{"guestNodes":4}`)
	r, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status.Workload != "fail" || r.Experiment.Mode != "unknown" || r.Experiment.CPUTreatment != "unknown" || r.Experiment.GuestNodes != nil {
		t.Fatalf("inferred/conflicting metadata: %+v %+v", r.Status, r.Experiment)
	}
}

func TestAssessmentArchiveIntegrityNotCaptureHealth(t *testing.T) {
	opts := fixture(t)
	archive := []byte("opaque archive bytes, never parsed")
	hash := sha256.Sum256(archive)
	m, _ := json.Marshal(map[string]any{"version": "v1", "archive": "archive.tar", "bytes": len(archive), "sha256": hex.EncodeToString(hash[:])})
	writeArtifact(t, opts.Input, "evidence/record/archive.tar", string(archive))
	writeArtifact(t, opts.Input, "evidence/record/manifest.json", string(m))
	opts.RecorderSummary = filepath.Join(opts.Input, "stream-summary.json")
	writeArtifact(t, opts.Input, "stream-summary.json", `{"enableWatch":true,"enableLogs":true,"incomplete":false,"watch":{"required":1,"initialized":1,"errors":0,"gaps":0},"logs":{"errors":0,"gaps":0}}`)
	r, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status.Capture != "unknown" {
		t.Fatalf("archive and detached clean summary qualified capture: %+v", r.Status)
	}
	assertReferences(t, opts, r)
	writeArtifact(t, opts.Input, "evidence/record/archive.tar", "modified")
	r, err = Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status.Capture != "incomplete" || r.Status.Workload != "pass" {
		t.Fatalf("archive integrity axes: %+v", r.Status)
	}
}

func TestAssessmentMetricsAllSeriesAndHashes(t *testing.T) {
	opts := fixture(t)
	dir := t.TempDir()
	opts.Metrics = []string{dir}
	start := opts.Now.Add(-time.Hour)
	series := []any{}
	for i := 0; i < 61; i++ {
		series = append(series, map[string]any{"metric": map[string]string{"pod": "preserve-in-source"}, "values": [][]any{{start.Unix(), "1"}, {start.Add(2 * time.Second).Unix(), "7"}}})
	}
	raw, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": series}})
	hash := sha256.Sum256(raw)
	m := metrics.Manifest{Version: 1, Start: start, End: start.Add(2 * time.Second), Step: time.Second, Queries: []metrics.QueryResult{{Query: metrics.Query{Name: "cpu", Expr: "rate(cpu[1m])"}, Status: metrics.StatusSuccess, HTTPStatus: 200, File: "cpu.json", SHA256: hex.EncodeToString(hash[:])}}}
	data, _ := json.Marshal(m)
	writeArtifact(t, dir, "manifest.json", string(data))
	writeArtifact(t, dir, "cpu.json", string(raw))
	r, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.MetricsCoverage) != 1 {
		t.Fatalf("coverage: %+v", r.MetricsCoverage)
	}
	c := r.MetricsCoverage[0]
	if c.Series != 61 || c.ValidSamples != 122 || c.Expected != 183 || c.MissingApprox != 61 || c.IncompleteSeries != 61 || c.SampleWindow.Start == nil || !c.SampleWindow.Start.Equal(start) {
		t.Fatalf("all-series coverage: %+v", c)
	}
	assertReferences(t, opts, r)
	writeArtifact(t, dir, "cpu.json", `{"modified":true}`)
	r, err = Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status.Capture != "incomplete" || r.MetricsCoverage[0].Status != "error" || r.MetricsCoverage[0].Series != 0 {
		t.Fatalf("hash mismatch yielded usable measurements: %+v", r)
	}
	found := false
	for _, f := range r.Findings {
		found = found || strings.Contains(f.Statement, "SHA256 mismatch")
	}
	if !found {
		t.Fatal("hash failure not surfaced")
	}
}

func TestAssessmentAllowlistBoundsAndCancellation(t *testing.T) {
	opts := Options{Input: t.TempDir()}
	writeArtifact(t, opts.Input, "private/secret.json", `{"credential":"DO-NOT-READ"}`)
	writeArtifact(t, opts.Input, "commands/seed-credentials.json", `{"credential":"DO-NOT-READ"}`)
	writeArtifact(t, opts.Input, "commands/arbitrary.json", `{"credential":"DO-NOT-READ"}`)
	if err := os.Symlink("private", filepath.Join(opts.Input, "inputs")); err != nil {
		t.Fatal(err)
	}
	writeArtifact(t, opts.Input, "private/seed.json", `{"id":"DO-NOT-READ"}`)
	writeArtifact(t, opts.Input, "evidence/run.json", strings.Repeat(" ", maxMetadataBytes+1))
	r, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), "DO-NOT-READ") || len(r.Evidence) != 0 {
		t.Fatalf("allowlist or size limit escaped: %s", data)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, opts); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestAssessmentDecodeVersioning(t *testing.T) {
	r, err := Build(context.Background(), Options{Input: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r)
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	doc["futureAdditiveField"] = true
	data, _ = json.Marshal(doc)
	if _, err := Decode(data); err != nil {
		t.Fatal(err)
	}
	if doc["analysisVersion"] != "v2" {
		t.Fatal("new analysis semantics must be versioned")
	}
	doc["analysisVersion"] = "v1"
	delete(doc["status"].(map[string]any), "performance")
	data, _ = json.Marshal(doc)
	if old, err := Decode(data); err != nil || old.Status.Performance != "" {
		t.Fatalf("legacy import changed: %+v %v", old.Status, err)
	}
	doc["status"].(map[string]any)["performance"] = "failed"
	data, _ = json.Marshal(doc)
	if _, err := Decode(data); err == nil {
		t.Fatal("invalid performance accepted")
	}
	delete(doc["status"].(map[string]any), "performance")
	doc["schemaVersion"] = "hcp-perf.assessment/v2"
	data, _ = json.Marshal(doc)
	if _, err := Decode(data); err == nil {
		t.Fatal("future major accepted")
	}
	if _, err := Decode([]byte(`{}`)); err == nil {
		t.Fatal("empty document accepted")
	}
}

func TestCompletedLatencyPerformance(t *testing.T) {
	for _, tc := range []struct {
		name        string
		expected    *int64
		duration    int64
		performance string
	}{
		{"late", ptrDuration(180 * time.Millisecond), int64(500 * time.Millisecond), "degraded"},
		{"at expectation", ptrDuration(180 * time.Millisecond), int64(180 * time.Millisecond), "within_expectations"},
		{"legacy", nil, int64(500 * time.Millisecond), "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := fixture(t)
			writeArtifact(t, opts.Input, "inputs/session.json", `{"mode":"validation","steadySeconds":60,"baselineCycles":1,"expectedSteadyCanaries":2,"scheduledRollouts":1,"canaryPolicy":"observe-degradation"}`)
			// Neither old hard budgets nor a changed summary can rewrite stored expectations.
			writeArtifact(t, opts.Input, "commands/baseline-budgets.json", `{"ack":1,"ready":1,"cleanup":1}`)
			writeArtifact(t, opts.Input, "commands/baseline-expectations.json", `{"ack":9999999999,"ready":9999999999,"cleanup":9999999999}`)
			for i := 1; i <= 2; i++ {
				s := stage{Started: opts.Now.Add(-time.Hour), Completed: opts.Now.Add(-time.Hour + time.Duration(tc.duration)), Duration: tc.duration, Expected: tc.expected}
				if tc.expected != nil {
					s.Deadline = ptrDuration(30 * time.Second)
				}
				data, _ := json.Marshal(map[string]any{"ack": s, "ready": s, "cleanup": s, "performance": "within_expectations"})
				writeArtifact(t, opts.Input, fmt.Sprintf("commands/canary-steady-%d.json", i), string(data))
				writeArtifact(t, opts.Input, fmt.Sprintf("commands/canary-steady-%d.exit", i), "0")
			}
			r, err := Build(context.Background(), opts)
			if err != nil || r.Status.Workload != "pass" || r.Status.Performance != tc.performance || len(r.Failures) != 0 {
				t.Fatalf("completion/performance conflated: %+v %v", r, err)
			}
			points := map[string]float64{}
			for _, p := range r.DataPoints {
				points[p.Name+"."+p.Statistic] = p.Value
			}
			if points["canary.steady.ack.max"] != float64(tc.duration)/1e9 || points["canary.steady.ack.count"] != 2 {
				t.Fatalf("actual timings or next cycle lost: %+v", points)
			}
			if tc.performance == "degraded" {
				if points["canary.steady.degraded.count"] != 2 {
					t.Fatal("missing separate degraded cycle count")
				}
				found := false
				for _, f := range r.Findings {
					if f.Category == "performance" {
						found = strings.Contains(f.Statement, "500.000ms") && strings.Contains(f.Statement, "180.000ms") && strings.Contains(f.Statement, "30.000s") && strings.Contains(f.Statement, "cleanup completed") && len(f.Evidence) == 2
						if !found {
							t.Fatalf("missing completion facts: %+v", f)
						}
					}
				}
				if !found {
					t.Fatal("missing performance finding")
				}
			}
			assertReferences(t, opts, r)
		})
	}
}

func ptrDuration(d time.Duration) *int64 { n := int64(d); return &n }

func TestSkippedCadenceIsObservationGap(t *testing.T) {
	opts := fixture(t)
	writeArtifact(t, opts.Input, "inputs/session.json", `{"mode":"validation","steadySeconds":60,"baselineCycles":1,"expectedSteadyCanaries":2,"scheduledRollouts":1}`)
	writeArtifact(t, opts.Input, "evidence/canary-cadence.jsonl", "{\"type\":\"cycle-started\",\"slotIndex\":0,\"canaryNumber\":1}\n{\"type\":\"cycle-completed\",\"slotIndex\":0,\"canaryNumber\":1}\n{\"type\":\"missed-slot\",\"slotIndex\":1,\"canaryNumber\":2}\n")
	r, err := Build(context.Background(), opts)
	if err != nil || r.Status.Workload != "incomplete" || len(r.Failures) != 0 {
		t.Fatalf("observation gap became failure: %+v %v", r, err)
	}
	points := map[string]float64{}
	for _, p := range r.DataPoints {
		points[p.Name+"."+p.Statistic] = p.Value
	}
	if points["canary.steady.skipped.count"] != 1 || points["canary.steady.ack.count"] != 1 {
		t.Fatalf("invented observations: %+v", points)
	}
	assertReferences(t, opts, r)
}

func TestAssessmentExplicitMeasurementAndSizingPolicy(t *testing.T) {
	opts := fixture(t)
	writeArtifact(t, opts.Input, "inputs/profile.json", `{"version":"v1","anchor":{"cpuMilli":250,"memoryMiB":1024}}`)
	for _, phase := range []string{"baseline", "steady"} {
		path := filepath.Join(opts.Input, "commands/canary-"+phase+"-1.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		doc["measurementMode"] = "watch"
		data, _ = json.Marshal(doc)
		writeArtifact(t, opts.Input, "commands/canary-"+phase+"-1.json", string(data))
	}
	r, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Experiment.MeasurementMode != "watch" {
		t.Fatalf("recorded method lost: %+v", r.Experiment)
	}
	anchors := 0
	for _, p := range r.DataPoints {
		if strings.HasPrefix(p.Name, "canary.") && p.MeasurementMethod != "watch" {
			t.Fatalf("incorrect sample method: %+v", p)
		}
		if strings.HasPrefix(p.Name, "sizingAnchor.") {
			anchors++
			if p.Statistic != "configured" || (p.Unit != "millicores" && p.Unit != "MiB") {
				t.Fatalf("anchor became measurement: %+v", p)
			}
		}
	}
	if anchors != 2 {
		t.Fatalf("wanted two explicit anchor values, got %d", anchors)
	}
	assertReferences(t, opts, r)
	writeArtifact(t, opts.Input, "inputs/session.json", `{"mode":"validation","steadySeconds":120,"baselineCycles":1,"expectedSteadyCanaries":1}`)
	r, err = Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status.Workload != "incomplete" {
		t.Fatalf("short steady window passed: %+v", r.Status)
	}
}

func TestAssessmentOutcomeStrictDecoding(t *testing.T) {
	for _, body := range []string{
		`{"exitCode":"0","shutdownOrExportFailed":false}`,
		`{"exitCode":false,"shutdownOrExportFailed":false}`,
		`{"exitCode":0.5,"shutdownOrExportFailed":false}`,
		`{"exitCode":null,"shutdownOrExportFailed":false}`,
		`{"exitCode":-1,"shutdownOrExportFailed":false}`,
		`{"exitCode":256,"shutdownOrExportFailed":false}`,
		`{"exitCode":0,"shutdownOrExportFailed":"false"}`,
		`{"exitCode":0,"shutdownOrExportFailed":null}`,
		`{"exitCode":0,"shutdownOrExportFailed":false,"workloadExitCode":"0"}`,
		`{"exitCode":0,"shutdownOrExportFailed":false,"workloadExitCode":null}`,
		`{"exitCode":0,"shutdownOrExportFailed":false,"workloadExitCode":0.5}`,
		`{"exitCode":0,"shutdownOrExportFailed":false,"workloadExitCode":-1}`,
		`{"exitCode":0,"shutdownOrExportFailed":false,"workloadExitCode":256}`,
	} {
		t.Run(body, func(t *testing.T) {
			opts := fixture(t)
			writeArtifact(t, opts.Input, "evidence/outcome.json", body)
			r, err := Build(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status.Workload != "incomplete" {
				t.Fatalf("invalid typed outcome changed status: %+v", r.Status)
			}
			found := false
			for _, f := range r.Findings {
				if f.Status == "inconclusive" && strings.Contains(f.Statement, "Invalid typed outcome") {
					if len(f.Evidence) != 1 {
						t.Fatalf("missing outcome provenance: %+v", f)
					}
					for _, e := range r.Evidence {
						if e.ID == f.Evidence[0] && e.Path == "evidence/outcome.json" {
							found = true
						}
					}
				}
			}
			if !found {
				t.Fatal("invalid typed outcome must have a sourced inconclusive finding")
			}
			assertReferences(t, opts, r)
		})
	}
}

func TestAssessmentLegacyCombinedExitRemainsAmbiguous(t *testing.T) {
	opts := fixture(t)
	writeArtifact(t, opts.Input, "evidence/outcome.json", `{"exitCode":1,"shutdownOrExportFailed":true}`)
	writeArtifact(t, opts.Input, "commands/turnover-gate.exit", "0")
	writeArtifact(t, opts.Input, "commands/recovery-window.exit", "0")
	writeArtifact(t, opts.Input, "inputs/session.json", `{"mode":"validation","steadySeconds":60,"baselineCycles":1,"expectedSteadyCanaries":24,"scheduledRollouts":1}`)
	data, err := os.ReadFile(filepath.Join(opts.Input, "commands/canary-steady-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 24; i++ {
		writeArtifact(t, opts.Input, fmt.Sprintf("commands/canary-steady-%d.json", i), string(data))
		writeArtifact(t, opts.Input, fmt.Sprintf("commands/canary-steady-%d.exit", i), "0")
	}
	r, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status.Workload != "incomplete" || r.Status.Capture != "incomplete" {
		t.Fatalf("legacy combined exit passed: %+v", r.Status)
	}
	observed, ambiguous, count := false, false, false
	for _, f := range r.Findings {
		observed = observed || (f.Status == "observed" && strings.Contains(f.Statement, "24 successful steady canaries"))
		ambiguous = ambiguous || (f.Status == "inconclusive" && strings.Contains(f.Statement, "Legacy combined"))
	}
	for _, p := range r.DataPoints {
		count = count || (p.Name == "canary.steady.ready" && p.Statistic == "count" && p.Value == 24)
	}
	if !observed || !ambiguous || !count {
		t.Fatalf("completed observations or ambiguity lost: observed=%v ambiguous=%v count=%v", observed, ambiguous, count)
	}
	assertReferences(t, opts, r)
}

func TestAssessmentWorkloadCommandsOverrideOutcome(t *testing.T) {
	for _, command := range []string{"recovery-window", "failure-recovery", "turnover-gate", "turnover", "canary-steady-2", "canary-warmup", "seed-warmup-0", "seed", "baseline-budgets"} {
		for _, outcome := range []string{
			`{"exitCode":1,"shutdownOrExportFailed":true}`,
			`{"exitCode":1,"workloadExitCode":0,"shutdownOrExportFailed":true}`,
			`{"exitCode":0,"workloadExitCode":0,"shutdownOrExportFailed":false}`,
		} {
			t.Run(command+outcome, func(t *testing.T) {
				opts := fixture(t)
				writeArtifact(t, opts.Input, "evidence/outcome.json", outcome)
				writeArtifact(t, opts.Input, "commands/"+command+".exit", "1")
				r, err := Build(context.Background(), opts)
				if err != nil {
					t.Fatal(err)
				}
				if r.Status.Workload != "fail" {
					t.Fatalf("known failed command masked: %+v", r.Status)
				}
				assertReferences(t, opts, r)
			})
		}
	}
	for _, marker := range []struct{ path, body string }{
		{"commands/recovery-window.started", "2026-09-09T10:01:00Z"},
		{"commands/recovery-window.exit", "bad exit"},
		{"commands/turnover-gate.exit", "256"},
	} {
		opts := fixture(t)
		writeArtifact(t, opts.Input, marker.path, marker.body)
		r, err := Build(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status.Workload != "incomplete" {
			t.Fatalf("unfinished/invalid command passed: %s %+v", marker.path, r.Status)
		}
	}
	for _, command := range []string{"record-export", "metrics-capture", "workload-dispatch", "arbitrary"} {
		opts := fixture(t)
		writeArtifact(t, opts.Input, "commands/"+command+".exit", "1")
		r, err := Build(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status.Workload != "pass" {
			t.Fatalf("non-workload command changed workload status: %s %+v", command, r.Status)
		}
	}
}

func TestAssessmentExplicitWorkloadExit(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"exitCode":1,"workloadExitCode":0,"shutdownOrExportFailed":true}`, "pass"},
		{`{"exitCode":0,"workloadExitCode":0,"shutdownOrExportFailed":false}`, "pass"},
		{`{"exitCode":1,"workloadExitCode":1,"shutdownOrExportFailed":true}`, "fail"},
		{`{"exitCode":0,"workloadExitCode":1,"shutdownOrExportFailed":false}`, "fail"},
		{`{"exitCode":1,"workloadExitCode":0,"shutdownOrExportFailed":false}`, "incomplete"},
		{`{"exitCode":0,"workloadExitCode":0,"shutdownOrExportFailed":true}`, "incomplete"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			opts := fixture(t)
			writeArtifact(t, opts.Input, "evidence/outcome.json", tc.body)
			r, err := Build(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status.Workload != tc.want {
				t.Fatalf("want %s: %+v", tc.want, r.Status)
			}
		})
	}
}

func TestAssessmentIntendedSchedule(t *testing.T) {
	for _, tc := range []struct {
		name         string
		apps, actual int
		schedule     string
		want         string
	}{
		{"explicit match", 3, 1, `,"scheduledRollouts":1`, "pass"},
		{"explicit mismatch", 3, 1, `,"scheduledRollouts":2`, "incomplete"},
		{"calculated match", 3, 1, "", "pass"},
		{"calculated mismatch", 3, 3, "", "incomplete"},
		{"spurious empty result", 3, 0, "", "incomplete"},
		{"explicit zero applications", 0, 0, `,"scheduledRollouts":0`, "pass"},
		{"calculated zero applications", 0, 0, "", "pass"},
		{"invalid schedule string", 3, 1, `,"scheduledRollouts":"1"`, "incomplete"},
		{"invalid schedule null", 3, 1, `,"scheduledRollouts":null`, "incomplete"},
		{"invalid schedule negative", 3, 1, `,"scheduledRollouts":-1`, "incomplete"},
		{"invalid schedule overflow", 3, 1, `,"scheduledRollouts":9223372036854775808`, "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := fixture(t)
			writeArtifact(t, opts.Input, "inputs/seed.json", fmt.Sprintf(`{"version":"v1","guestNodes":3,"applications":%d}`, tc.apps))
			writeArtifact(t, opts.Input, "inputs/session.json", `{"mode":"validation","steadySeconds":60,"baselineCycles":1,"expectedSteadyCanaries":1`+tc.schedule+`}`)
			writeArtifact(t, opts.Input, "commands/turnover.json", fmt.Sprintf(`{"version":"v1","startedAt":"2026-09-09T10:00:00Z","admissionEnd":"2026-09-09T10:01:00Z","finishedAt":"2026-09-09T10:01:00Z","scheduled":%d,"completed":%d,"failed":0,"notStarted":0,"backlog":0,"outstanding":0}`, tc.actual, tc.actual))
			writeArtifact(t, opts.Input, "commands/turnover-gate.exit", "0")
			r, err := Build(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status.Workload != tc.want {
				t.Fatalf("want %s: %+v", tc.want, r.Status)
			}
			assertReferences(t, opts, r)
			if tc.want == "pass" {
				found := false
				for _, p := range r.DataPoints {
					if p.Name == "turnover.intended" {
						found = p.Value == float64(tc.actual) && len(p.Provenance.EvidenceRefs) > 0
						if tc.schedule == "" && p.Provenance.Formula != "ceil(applications * steadySeconds / 3600)" {
							t.Fatalf("missing calculation provenance: %+v", p)
						}
					}
				}
				if !found {
					t.Fatal("missing intended schedule point")
				}
			}
		})
	}
}

func TestAssessmentRecordedPeriod(t *testing.T) {
	for _, tc := range []struct {
		name, path, fields string
		actual             int
		want               string
	}{
		{"hourly", "inputs/turnover.json", `"turnoverPeriodSeconds":3600`, 10, "pass"},
		{"half-hour", "inputs/session.json", `"turnoverPeriodSeconds":1800`, 20, "pass"},
		{"quarter-hour", "inputs/case.json", `"turnoverPeriodSeconds":900`, 40, "pass"},
		{"ten-minute", "evidence/run.json", `"turnoverPeriodSeconds":600`, 60, "pass"},
		{"five-minute", "commands/turnover.json", `"spec":{"turnoverPeriodSeconds":300,"durationSeconds":300,"applications":120}`, 120, "pass"},
		{"result-spec", "evidence/turnover/result.json", `"spec":{"turnoverPeriodSeconds":300}`, 120, "pass"},
		{"zero", "inputs/turnover.json", `"turnoverPeriodSeconds":0`, 10, "pass"},
		{"invalid-null", "inputs/turnover.json", `"turnoverPeriodSeconds":null`, 10, "incomplete"},
		{"invalid-string", "inputs/turnover.json", `"turnoverPeriodSeconds":"300"`, 10, "incomplete"},
		{"invalid-negative", "inputs/turnover.json", `"turnoverPeriodSeconds":-1`, 10, "incomplete"},
		{"invalid-fraction", "inputs/turnover.json", `"turnoverPeriodSeconds":300.5`, 10, "incomplete"},
		{"invalid-large", "inputs/turnover.json", `"turnoverPeriodSeconds":86401`, 10, "incomplete"},
		{"invalid-overflow", "inputs/turnover.json", `"turnoverPeriodSeconds":9223372036854775808`, 10, "incomplete"},
		{"invalid-duration", "inputs/turnover.json", `"durationSeconds":9223372036854775807`, 10, "incomplete"},
		{"invalid-apps", "inputs/turnover.json", `"applications":9223372036854775807`, 10, "incomplete"},
		{"conflicting-period", "inputs/turnover.json", `"turnoverPeriodSeconds":300`, 120, "incomplete"},
		{"conflicting-count", "inputs/session.json", `"turnoverPeriodSeconds":300,"scheduledRollouts":10`, 10, "incomplete"},
		{"matching-count", "inputs/session.json", `"turnoverPeriodSeconds":300,"scheduledRollouts":120`, 120, "pass"},
		{"cap", "inputs/session.json", `"turnoverPeriodSeconds":1`, 80000, "incomplete"},
		{"exact-cap", "inputs/session.json", `"turnoverPeriodSeconds":150`, 80000, "pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := fixture(t)
			apps := 120
			if tc.name == "cap" || tc.name == "exact-cap" {
				apps = 40000
			}
			writeArtifact(t, opts.Input, "inputs/seed.json", fmt.Sprintf(`{"applications":%d}`, apps))
			session := `"mode":"validation","steadySeconds":300,"baselineCycles":1,"expectedSteadyCanaries":1`
			if tc.name == "conflicting-period" {
				session += `,"turnoverPeriodSeconds":3600`
			}
			turnover := fmt.Sprintf(`"version":"v1","startedAt":"2026-09-09T10:00:00Z","admissionEnd":"2026-09-09T10:05:00Z","finishedAt":"2026-09-09T10:05:00Z","scheduled":%d,"completed":%d,"failed":0,"notStarted":0,"backlog":0,"outstanding":0`, tc.actual, tc.actual)
			if tc.path == "inputs/session.json" {
				session += "," + tc.fields
			} else if tc.path == "commands/turnover.json" {
				turnover += "," + tc.fields
			} else {
				writeArtifact(t, opts.Input, tc.path, "{"+tc.fields+"}")
			}
			if tc.path == "evidence/turnover/result.json" {
				writeArtifact(t, opts.Input, tc.path, "{"+turnover+","+tc.fields+"}")
			}
			writeArtifact(t, opts.Input, "inputs/session.json", "{"+session+"}")
			writeArtifact(t, opts.Input, "commands/turnover.json", "{"+turnover+"}")
			r, err := Build(t.Context(), opts)
			if err != nil || r.Status.Workload != tc.want {
				t.Fatalf("want %s: %+v, %v", tc.want, r.Status, err)
			}
			assertReferences(t, opts, r)
			if tc.want == "pass" {
				found := false
				for _, p := range r.DataPoints {
					if p.Name == "turnover.intended" {
						found = p.Value == float64(tc.actual)
					}
				}
				if !found || r.Experiment.TurnoverPeriodSeconds == nil {
					t.Fatal("missing resolved intent")
				}
			}
		})
	}
}

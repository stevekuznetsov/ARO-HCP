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
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/assessment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/report"
)

func reportFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	raw := []byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"container":"kas"},"values":[[1700000000,"1"]]}]}}`)
	hash := sha256.Sum256(raw)
	start := time.Unix(1700000000, 0).UTC()
	m := metrics.Manifest{Version: metrics.ManifestVersion, Start: start, End: start, Step: time.Second,
		Queries: []metrics.QueryResult{{Query: metrics.Query{Name: "cpu", Expr: "provided_query"}, Status: metrics.StatusSuccess, HTTPStatus: 200, File: "cpu.json", SHA256: hex.EncodeToString(hash[:])}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"manifest.json": b, "cpu.json": raw} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readReport(t *testing.T, path string) (report.ResultPage, []byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), `<script id="report-data" type="application/json">`)
	if !ok {
		t.Fatal("not a report")
	}
	data, _, _ := strings.Cut(rest, "</script>")
	var page report.ResultPage
	if err := json.Unmarshal([]byte(data), &page); err != nil {
		t.Fatal(err)
	}
	return page, b
}

func TestReportCLI(t *testing.T) {
	input, metricsDir := t.TempDir(), reportFixture(t)
	if err := os.Mkdir(filepath.Join(input, "evidence"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "evidence/outcome.json"), []byte(`{"exitCode":`), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "report.html")
	args := []string{"report", "render", "--input", input, "--metrics", metricsDir, "--metrics", filepath.Join(input, "bad-source"), "--metrics", metricsDir, "--output", output}
	var out bytes.Buffer
	if err := runReport(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	page, before := readReport(t, output)
	if page.Assessment == nil || page.Assessment.Status.Workload != "incomplete" || page.Assessment.Status.Qualification != "not_assessed" {
		t.Fatalf("input report did not automatically assess partial evidence: %+v", page.Assessment)
	}
	if page.Progress.Phase != "incomplete" || !page.Progress.Provisional || len(page.Progress.Warnings) == 0 || len(page.Sources) != 3 || page.Sources[1].Error == "" || len(page.Sources[2].Queries[0].Series[0].Points) != 1 {
		t.Fatalf("partial progress / repeated roots not retained: %+v", page)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("HTML not private: %v %v", info, err)
	}
	if _, err := os.Stat(output + ".assessment.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("report created an implicit assessment sidecar")
	}
	var result struct {
		Version       string        `json:"version"`
		Output        string        `json:"output"`
		Bytes         int           `json:"bytes"`
		Limits        report.Limits `json:"limits"`
		TotalSeries   int           `json:"totalSeries"`
		PreviewSeries int           `json:"previewSeries"`
		OmittedSeries int           `json:"omittedSeries"`
		PlottedPoints int           `json:"plottedPoints"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("CLI output is not JSON: %v", err)
	}
	if result.Version != "v1" || result.Output != output || result.Bytes != len(before) || result.Limits.MaxSeries != 200 || result.Limits.MaxPoints != 300 || result.TotalSeries != 2 || result.PreviewSeries != 2 || result.OmittedSeries != 0 || result.PlottedPoints != 2 {
		t.Fatalf("incorrect machine-readable report result: %+v", result)
	}
	if err := runReport(context.Background(), args, io.Discard); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing output accepted: %v", err)
	}
	_, after := readReport(t, output)
	if !bytes.Equal(before, after) {
		t.Fatal("existing report overwritten")
	}
}

func TestReportCLIPartialAndMetricsOnly(t *testing.T) {
	for _, flags := range [][]string{
		{"--input", t.TempDir()},
		{"--input", filepath.Join(t.TempDir(), "missing")},
		{"--metrics", reportFixture(t)},
	} {
		output := filepath.Join(t.TempDir(), "report.html")
		args := append([]string{"report", "render", "--output", output}, flags...)
		if err := runReport(context.Background(), args, io.Discard); err != nil {
			t.Fatalf("partial/metrics-only report rejected: %v", err)
		}
		page, _ := readReport(t, output)
		if !page.Progress.Provisional || page.Progress.Qualified {
			t.Fatal("missing evidence qualified")
		}
		if flags[0] == "--metrics" && page.Assessment != nil {
			t.Fatal("metrics-only report invented an assessment without a run input")
		}
	}
}

func TestReportCLIArgumentsAndOutputSafety(t *testing.T) {
	input := t.TempDir()
	for _, flags := range [][]string{
		nil, {"case", "render"}, {"report"}, {"report", "render"},
		{"report", "render", "--output", ""},
		{"report", "render", "--output", filepath.Join(input, "no-input.html")},
		{"report", "render", "--input", input, "--output", filepath.Join(input, "missing-parent", "report.html")},
		{"report", "render", "--input", input, "--output", input},
		{"report", "render", "--input", input, "--metrics", "", "--output", filepath.Join(input, "empty-metrics.html")},
		{"report", "render", "--input", input, "--output", filepath.Join(input, "extra.html"), "extra"},
		{"report", "render", "--input", input, "--kubeconfig", "SECRET"},
		{"report", "render", "--metrics", input, "--recorder-summary", "summary.json", "--output", filepath.Join(input, "bad-summary.html")},
		{"report", "render", "--metrics", input, "--request-evidence", "request.json", "--output", filepath.Join(input, "bad-request.html")},
	} {
		var out bytes.Buffer
		if err := runReport(context.Background(), flags, &out); err == nil || out.Len() != 0 {
			t.Fatalf("invalid argv accepted/output emitted: %v %v", flags, err)
		}
	}
	target := filepath.Join(input, "private")
	link := filepath.Join(input, "report.html")
	if err := os.WriteFile(target, []byte("SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := runReport(context.Background(), []string{"report", "render", "--input", input, "--output", link}, io.Discard); !errors.Is(err, os.ErrExist) {
		t.Fatalf("symlink output accepted: %v", err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "SECRET" {
		t.Fatal("symlink target modified")
	}
}

func TestReportCLIAssessment(t *testing.T) {
	input := t.TempDir()
	if err := os.Mkdir(filepath.Join(input, "inputs"), 0700); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(input, "inputs", "seed.json")
	if err := os.WriteFile(seed, []byte(`{"id":"explicit-id-not-directory-name","applications":3,"title":"A bounded experiment","question":"Does the workload complete?"}`), 0600); err != nil {
		t.Fatal(err)
	}
	built, err := assessment.Build(context.Background(), assessment.Options{Input: input})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		input   string
		mutate  func(*assessment.Result)
		wantErr string
	}{
		{name: "linked", input: input},
		{name: "assessment-only"},
		{name: "different-id", input: input, mutate: func(r *assessment.Result) { r.Experiment.ID = "other-id" }, wantErr: "experiment ID"},
		{name: "changed-evidence", input: input, mutate: func(r *assessment.Result) { r.Evidence[0].SHA256 = strings.Repeat("0", 64) }, wantErr: "evidence differs"},
		{name: "no-evidence-link", input: input, mutate: func(r *assessment.Result) { r.Evidence = nil }, wantErr: "no matching evidence"},
		{name: "unsupported-pass", input: input, mutate: func(r *assessment.Result) { r.Status.Workload = "pass" }, wantErr: "workload status"},
		{name: "unsupported-capture-complete", input: input, mutate: func(r *assessment.Result) { r.Status.Capture = "complete" }, wantErr: "capture status"},
		{name: "unsupported", mutate: func(r *assessment.Result) { r.SchemaVersion = "future" }, wantErr: "unsupported assessment"},
		{name: "bad-status", mutate: func(r *assessment.Result) { r.Status.Capture = "great" }, wantErr: "invalid assessment status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := built
			r.Evidence = append([]assessment.Evidence(nil), built.Evidence...)
			if tc.mutate != nil {
				tc.mutate(&r)
			}
			body, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			file, output := filepath.Join(dir, "assessment.json"), filepath.Join(dir, "report.html")
			if err := os.WriteFile(file, body, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"report", "render", "--assessment", file, "--output", output}
			if tc.input != "" {
				args = append(args, "--input", tc.input)
			}
			err = runReport(context.Background(), args, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("wanted %q, got %v", tc.wantErr, err)
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("invalid assessment produced HTML")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			page, _ := readReport(t, output)
			if page.Assessment == nil || page.Assessment.Experiment.ID != r.Experiment.ID || page.Assessment.Experiment.Title != r.Experiment.Title || !page.Assessment.GeneratedAt.Equal(r.GeneratedAt) {
				t.Fatal("imported assessment not preserved")
			}
			after, err := os.ReadFile(file)
			if err != nil || !bytes.Equal(body, after) {
				t.Fatal("imported assessment modified")
			}
		})
	}
}

func TestReportCLIImportedAssessmentAssociation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  string
		wantErr string
	}{
		{name: "stable"},
		{name: "deleted-canary", change: "delete", wantErr: "evidence cannot be verified: commands/canary-steady-1.json"},
		{name: "new-failed-canary", change: "failure", wantErr: `workload status "pass" differs from current input status "fail"`},
		{name: "different-run", change: "id", wantErr: "experiment ID"},
		{name: "summary-not-authorized", change: "omit-summary", wantErr: "supply the original --metrics and --recorder-summary"},
		{name: "metrics-not-authorized", change: "omit-metrics", wantErr: "supply the original --metrics and --recorder-summary"},
		{name: "deleted-summary", change: "delete-summary", wantErr: "evidence cannot be verified"},
		{name: "new-capture-failure", change: "capture-failure", wantErr: `capture status "unknown" differs from current input status "incomplete"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, external, metricsDir := t.TempDir(), t.TempDir(), reportFixture(t)
			put := func(path, body string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			put(filepath.Join(input, "inputs/seed.json"), `{"id":"stable-explicit-id","applications":3}`)
			put(filepath.Join(input, "inputs/session.json"), `{"mode":"validation","steadySeconds":60,"baselineCycles":1,"expectedSteadyCanaries":1}`)
			put(filepath.Join(input, "commands/turnover.json"), `{"version":"v1","startedAt":"2023-11-14T22:13:20Z","admissionEnd":"2023-11-14T22:14:20Z","finishedAt":"2023-11-14T22:14:20Z","scheduled":1,"completed":1,"failed":0,"notStarted":0,"backlog":0,"outstanding":0}`)
			put(filepath.Join(input, "evidence/outcome.json"), `{"exitCode":0,"shutdownOrExportFailed":false}`)
			const canary = `{"ack":{"started":"2023-11-14T22:13:20Z","completed":"2023-11-14T22:13:21Z","durationNanoseconds":1000000000},"ready":{"started":"2023-11-14T22:13:20Z","completed":"2023-11-14T22:13:21Z","durationNanoseconds":1000000000},"cleanup":{"started":"2023-11-14T22:13:21Z","completed":"2023-11-14T22:13:22Z","durationNanoseconds":1000000000}}`
			for _, phase := range []string{"baseline", "steady"} {
				put(filepath.Join(input, "commands/canary-"+phase+"-1.json"), canary)
				put(filepath.Join(input, "commands/canary-"+phase+"-1.exit"), "0\n")
			}
			summary := filepath.Join(external, "stream-summary.json")
			put(summary, `{"enableWatch":true,"enableLogs":true,"incomplete":false,"watch":{"required":4,"initialized":4,"errors":0,"gaps":0},"logs":{"errors":0,"gaps":0}}`)
			built, err := assessment.Build(context.Background(), assessment.Options{Input: input, Metrics: []string{metricsDir}, RecorderSummary: summary})
			if err != nil {
				t.Fatal(err)
			}
			if built.Status.Workload != "pass" || built.Status.Capture != "unknown" {
				t.Fatalf("fixture must pass with capture unknown: %+v", built.Status)
			}
			body, err := json.Marshal(built)
			if err != nil {
				t.Fatal(err)
			}
			imported, output := filepath.Join(external, "assessment.json"), filepath.Join(external, "report.html")
			put(imported, string(body))
			args := []string{"report", "render", "--input", input, "--assessment", imported, "--output", output}
			if tc.change != "omit-summary" {
				args = append(args, "--recorder-summary", summary)
			}
			if tc.change != "omit-metrics" {
				args = append(args, "--metrics", metricsDir)
			}
			switch tc.change {
			case "delete", "delete-summary":
				path := filepath.Join(input, "commands/canary-steady-1.json")
				if tc.change == "delete-summary" {
					path = summary
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "failure":
				put(filepath.Join(input, "commands/canary-steady-2.json"), canary)
				put(filepath.Join(input, "commands/canary-steady-2.exit"), "1\n")
			case "id":
				put(filepath.Join(input, "inputs/seed.json"), `{"id":"different-run","applications":3}`)
			case "capture-failure":
				args = append(args, "--metrics", filepath.Join(external, "missing-metrics"))
			}
			var out bytes.Buffer
			err = runReport(context.Background(), args, &out)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || out.Len() != 0 {
					t.Fatalf("wanted %q without success output, got %v, %s", tc.wantErr, err, out.String())
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unverified import produced HTML")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				page, _ := readReport(t, output)
				got, err := json.Marshal(page.Assessment)
				if err != nil || !bytes.Equal(got, body) {
					t.Fatal("stable import was changed instead of preserved")
				}
			}
			after, err := os.ReadFile(imported)
			if err != nil || !bytes.Equal(after, body) {
				t.Fatal("imported assessment file changed")
			}
		})
	}
}

func TestReportCLIRecorderSummary(t *testing.T) {
	input, dir := t.TempDir(), t.TempDir()
	summary, output := filepath.Join(dir, "stream-summary.json"), filepath.Join(dir, "report.html")
	body := []byte(`{"enableWatch":true,"enableLogs":true,"incomplete":true,"watch":{"required":4,"initialized":4,"errors":0,"gaps":0},"logs":{"errors":642,"gaps":216}}`)
	if err := os.WriteFile(summary, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runReport(context.Background(), []string{"report", "render", "--input", input, "--recorder-summary", summary, "--output", output}, io.Discard); err != nil {
		t.Fatal(err)
	}
	page, _ := readReport(t, output)
	if page.Assessment == nil || page.Assessment.Status.Capture != "incomplete" {
		t.Fatalf("recorder summary not assessed: %+v", page.Assessment)
	}
	found := false
	for _, p := range page.Assessment.DataPoints {
		found = found || p.Name == "recorder.logs.errors" && p.Value == 642
	}
	if !found {
		t.Fatal("recorder summary observations not embedded")
	}
}

func TestReportCLICancellationAndWriterError(t *testing.T) {
	output := filepath.Join(t.TempDir(), "report.html")
	args := []string{"report", "render", "--input", t.TempDir(), "--output", output}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runReport(ctx, args, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("output created after cancellation")
	}
	if err := runReport(context.Background(), args, reportErrorWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writer error ignored: %v", err)
	}
}

type reportErrorWriter struct{}

func (reportErrorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestReportCLILimits(t *testing.T) {
	dir := reportFixture(t)
	output := filepath.Join(t.TempDir(), "bounded.html")
	args := []string{"report", "render", "--metrics", dir, "--metrics", dir, "--output", output, "--max-series", "1", "--max-points", "2"}
	var out bytes.Buffer
	if err := runReport(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	page, _ := readReport(t, output)
	if page.TotalSeries != 2 || page.PreviewSeries != 1 || page.OmittedSeries != 1 || page.Limits.MaxPoints != 2 || page.Limits.MaxSeries != 1 {
		t.Fatalf("CLI limits not applied globally: %+v", page)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["omittedSeries"] != float64(1) {
		t.Fatalf("JSON omission count lost: %s %v", out.String(), err)
	}
	for _, flags := range [][]string{
		{"--max-series", "0"}, {"--max-series", "-1"}, {"--max-series", "1001"},
		{"--max-points", "0"}, {"--max-points", "1"}, {"--max-points", "501"}, {"--max-points", "invalid"},
	} {
		path := filepath.Join(t.TempDir(), "invalid.html")
		argv := append([]string{"report", "render", "--metrics", dir, "--output", path}, flags...)
		out.Reset()
		if err := runReport(context.Background(), argv, &out); err == nil || out.Len() != 0 {
			t.Fatalf("invalid limits accepted: %v %v", flags, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid limits created HTML")
		}
	}
}

func TestReportCLIRequestEvidence(t *testing.T) {
	for _, mode := range []string{"auto", "import", "missing-flag", "changed-source", "forged-code", "missing-file", "new-evidence"} {
		t.Run(mode, func(t *testing.T) {
			input, request := requestEvidenceCLIFixture(t)
			dir := t.TempDir()
			imported, output := filepath.Join(dir, "assessment.json"), filepath.Join(dir, "report.html")
			buildArgs := []string{"assessment", "build", "--input", input, "--output", imported}
			if mode != "new-evidence" {
				buildArgs = append(buildArgs, "--request-evidence", request)
			}
			if err := runAssessment(context.Background(), buildArgs, io.Discard); err != nil {
				t.Fatal(err)
			}
			args := []string{"report", "render", "--input", input, "--output", output}
			if mode != "auto" {
				args = append(args, "--assessment", imported)
			}
			if mode != "missing-flag" {
				args = append(args, "--request-evidence", request)
			}
			switch mode {
			case "changed-source":
				if err := os.WriteFile(filepath.Join(filepath.Dir(request), "record/logs.ndjson"), []byte("changed source"), 0600); err != nil {
					t.Fatal(err)
				}
			case "forged-code":
				body, err := os.ReadFile(imported)
				if err != nil {
					t.Fatal(err)
				}
				body = bytes.ReplaceAll(body, []byte(`"responseCode": 200`), []byte(`"responseCode": 201`))
				if err := os.WriteFile(imported, body, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-file":
				if err := os.Remove(request); err != nil {
					t.Fatal(err)
				}
			}
			err := runReport(context.Background(), args, io.Discard)
			if mode != "auto" && mode != "import" {
				if err == nil {
					t.Fatal("unverified or stale request evidence import produced report")
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatal("invalid import created output")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			page, html := readReport(t, output)
			f := page.Assessment.Failures[0]
			if page.Assessment.Status.Workload != "fail" || f.ApplicationOutcome != "server_success_observed" || f.CompletionObserved || f.ServerObservation == nil || f.ServerObservation.DurationSeconds != .108809 || !strings.Contains(string(html), "HTTP 200 in 108.809ms") || strings.Contains(string(html), "PRIVATE opaque") {
				t.Fatalf("report lost scoped server/client observations: %+v", f)
			}
		})
	}
}

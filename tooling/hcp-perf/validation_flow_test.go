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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/progress"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/report"
)

// This exercises CLI routing and real local capture/report storage, not a live
// pressure experiment. Canary/turnover evidence is explicitly fabricated.
func TestShortValidationFlow(t *testing.T) {
	t.Setenv("HCP_PERF_BEARER_TOKEN", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	input := t.TempDir()
	write := func(name string, body []byte) {
		t.Helper()
		path := filepath.Join(input, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cli := func(args ...string) []byte {
		t.Helper()
		var out bytes.Buffer
		if err := run(ctx, args, &out); err != nil {
			t.Fatalf("%v: %v; output: %s", args, err, &out)
		}
		return out.Bytes()
	}
	write("case.json", []byte(`{"version":"v1","mode":"validation","name":"validation-only","expectedProfile":"small","guestNodes":3,"applications":3,"seed":42,"cpuTreatment":"quiet","deadlinesSeconds":{"startup":60,"seeding":60,"calibration":60,"settle":30,"shutdown":30}}`))
	casePath := filepath.Join(input, "case.json")
	planJSON := cli("case", "plan", "--case", casePath, "--profile", "examples/small.json")
	var plan experiment.PlanResult
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Spec.Mode != "validation" || plan.Spec.Applications != 3 || plan.Spec.GuestNodes != 3 || plan.Spec.SteadySeconds != 300 || plan.Workload.Rollouts != 1 || plan.Canary.CadenceSeconds != 30 || plan.Canary.BaselineCycles != 5 {
		t.Fatalf("wrong short validation plan: %+v", plan)
	}
	steadyPhases := 0
	for _, phase := range plan.Phases {
		if phase.Name == "steady" {
			steadyPhases++
			if phase.DurationSeconds != 300 {
				t.Fatalf("validation steady phase is not 300 seconds: %+v", phase)
			}
		}
	}
	if steadyPhases != 1 {
		t.Fatalf("expected one steady phase, got %d", steadyPhases)
	}
	scheduleJSON := cli("case", "schedule", "--case", casePath, "--profile", "examples/small.json")
	var schedule []experiment.Rollout
	if err := json.Unmarshal(scheduleJSON, &schedule); err != nil {
		t.Fatal(err)
	}
	if len(schedule) != int(plan.Workload.Rollouts) || schedule[0].Offset != 0 || schedule[0].Round != 1 || schedule[0].Application < 0 || schedule[0].Application >= 3 {
		t.Fatalf("validation must truncate, not accelerate, hourly rollouts: %+v", schedule)
	}
	if again := cli("case", "schedule", "--case", casePath, "--profile", "examples/small.json"); !bytes.Equal(again, scheduleJSON) {
		t.Fatal("validation schedule is not deterministic")
	}
	write("plan.json", planJSON)
	write("inputs/turnover.json", []byte(`{"applications":3,"durationSeconds":300,"recoverySeconds":30}`))
	write("evidence/run.json", []byte(`{"mode":"validation","steadySeconds":300,"recoverySeconds":30,"canaryCadenceSeconds":30,"baselineCycles":5,"expectedSteadyCanaries":10,"scheduledRollouts":1,"guestNodes":3,"applications":3,"qualified":false}`))
	start := time.Now().UTC().Truncate(time.Second).Add(-6 * time.Minute)
	write("evidence/window-start.txt", []byte(start.Add(-30*time.Second).Format(time.RFC3339)))
	write("evidence/steady-start.txt", []byte(start.Format(time.RFC3339)))
	write("evidence/window-end.txt", []byte(start.Add(330*time.Second).Format(time.RFC3339)))
	write("evidence/outcome.json", []byte(`{"mode":"validation","exitCode":0,"shutdownOrExportFailed":false,"qualified":false}`))
	write("commands/baseline-budgets.json", []byte(`{"ack":3000000000,"ready":6000000000,"cleanup":3000000000}`))
	write("evidence/turnover/result.json", []byte(`{"scheduled":1,"completed":1,"failed":0,"notStarted":0,"backlog":0,"outstanding":0}`))
	for _, group := range []struct {
		name  string
		count int
		start time.Time
		step  time.Duration
	}{{"baseline", 5, start.Add(-25 * time.Second), 5 * time.Second}, {"steady", 10, start, 30 * time.Second}} {
		for i := 0; i < group.count; i++ {
			at := group.start.Add(time.Duration(i) * group.step)
			stage := func(at time.Time, duration time.Duration) progress.Stage {
				return progress.Stage{Started: at, Completed: at.Add(duration), Duration: int64(duration)}
			}
			sample := progress.Canary{Ack: stage(at, time.Second), Ready: stage(at, 2*time.Second), Cleanup: stage(at.Add(2*time.Second), time.Second)}
			body, err := json.Marshal(sample)
			if err != nil {
				t.Fatal(err)
			}
			name := fmt.Sprintf("commands/canary-%s-%d", group.name, i+1)
			write(name+".started", []byte(at.Format(time.RFC3339)))
			write(name+".json", body)
			write(name+".exit", []byte("0\n"))
		}
	}
	checkProgress := func(t *testing.T, p progress.Result) {
		t.Helper()
		if p.Qualified || p.Provisional || p.Phase != "finished" || p.Applications == nil || *p.Applications != 3 {
			t.Errorf("validation outcome must be finished, not qualified: %+v", p)
		}
		if p.Steady.DurationSeconds == nil || *p.Steady.DurationSeconds != 300 || p.Steady.Deadline == nil || !p.Steady.Deadline.Equal(start.Add(300*time.Second)) || p.Steady.ElapsedSeconds == nil || *p.Steady.ElapsedSeconds != 300 {
			t.Errorf("validation timing must not claim a two-hour window: %+v", p.Steady)
		}
		if p.Baseline.Passed != 5 || p.Baseline.Expected == nil || *p.Baseline.Expected != 5 || p.Baseline.Ready.Count != 5 {
			t.Errorf("fabricated baseline observations not retained: %+v", p.Baseline)
		}
		if p.SteadyCanaries.Expected == nil || *p.SteadyCanaries.Expected != 10 || p.SteadyCanaries.Passed != 10 || p.SteadyCanaries.Remaining == nil || *p.SteadyCanaries.Remaining != 0 || p.SteadyCanaries.Ready.Count != 10 {
			t.Errorf("validation cadence must yield 10 steady canaries, not the soak default: %+v", p.SteadyCanaries)
		}
		if p.Turnover == nil || p.Turnover.Scheduled != 1 || p.Turnover.Completed != 1 {
			t.Errorf("single validation rollout evidence missing: %+v", p.Turnover)
		}
		body, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(body, &metadata); err != nil {
			t.Fatal(err)
		}
		if string(metadata["mode"]) != `"validation"` {
			t.Errorf("progress/report must identify validation-only evidence, got mode %s", metadata["mode"])
		}
	}
	t.Run("progress", func(t *testing.T) {
		var p progress.Result
		if err := json.Unmarshal(cli("case", "progress", "--input", input), &p); err != nil {
			t.Fatal(err)
		}
		checkProgress(t, p)
	})

	queries := []metrics.Query{{Name: "cpu", Expr: "fixture_cpu"}, {Name: "up", Expr: "fixture_up"}}
	queryJSON, err := json.Marshal(queries)
	if err != nil {
		t.Fatal(err)
	}
	write("queries.json", queryJSON)
	type response struct {
		start time.Time
		expr  string
		body  []byte
	}
	responses := make(chan response, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		from, startErr := time.Parse(time.RFC3339Nano, q.Get("start"))
		to, endErr := time.Parse(time.RFC3339Nano, q.Get("end"))
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/query_range" || (q.Get("query") != "fixture_cpu" && q.Get("query") != "fixture_up") || q.Get("step") != "1" || startErr != nil || endErr != nil || to.Sub(from) != time.Second {
			t.Errorf("unexpected stream request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		body := []byte(fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"fixture-a","container":"validation-only"},"values":[[%.3f,"7"],[%.3f,"11"]]},{"metric":{"pod":"fixture-b","container":"validation-only"},"values":[[%.3f,"13"],[%.3f,"17"]]}]}}`, float64(from.UnixMilli())/1000, float64(to.UnixMilli())/1000, float64(from.UnixMilli())/1000, float64(to.UnixMilli())/1000))
		select {
		case responses <- response{from, q.Get("query"), body}:
		default:
			t.Error("unexpected extra stream request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	storage := t.TempDir()
	streamDir := filepath.Join(storage, "stream")
	var streamed metrics.StreamResult
	streamJSON := cli("metrics", "stream", "--endpoint", server.URL, "--queries", filepath.Join(input, "queries.json"), "--output", streamDir, "--duration", "2s", "--chunk", "1s", "--step", "1s", "--lag", "0s", "--drain", "10s", "--chunk-timeout", "5s")
	if err := json.Unmarshal(streamJSON, &streamed); err != nil {
		t.Fatal(err)
	}
	server.Close()
	close(responses)
	if streamed.Status != "complete" || streamed.DataQuality != metrics.StatusSuccess || streamed.PlannedChunks != 2 || streamed.CompletedChunks != 2 || streamed.PendingChunks != 0 || streamed.ErrorCount != 0 || streamed.End.Sub(streamed.Start) != 2*time.Second {
		t.Fatalf("short stream incomplete: %+v", streamed)
	}
	var checkpoint metrics.StreamResult
	if err := decodeFile(filepath.Join(streamDir, "manifest.json"), &checkpoint); err != nil || !reflect.DeepEqual(checkpoint, streamed) {
		t.Fatalf("stdout does not match durable stream checkpoint: %+v, %v", checkpoint, err)
	}
	wantRaw := map[string][]byte{}
	for response := range responses {
		wantRaw[response.start.Format(time.RFC3339Nano)+response.expr] = response.body
	}
	if len(wantRaw) != 4 {
		t.Fatalf("expected two queries in each of two chunks, got %d responses", len(wantRaw))
	}
	args := []string{"report", "render", "--input", input}
	rawFiles := 0
	for i := 0; i < streamed.CompletedChunks; i++ {
		dir := filepath.Join(streamDir, fmt.Sprintf("chunk-%06d", i))
		var manifest metrics.Manifest
		manifestJSON, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
			t.Fatal(err)
		}
		if len(manifest.Queries) != 2 || !manifest.Start.Equal(streamed.Start.Add(time.Duration(i)*time.Second)) || manifest.End.Sub(manifest.Start) != time.Second {
			t.Fatalf("incorrect chunk interval or queries: %+v", manifest)
		}
		for _, q := range manifest.Queries {
			raw, err := os.ReadFile(filepath.Join(dir, q.File))
			if err != nil {
				t.Fatal(err)
			}
			if q.Status != metrics.StatusSuccess || q.HTTPStatus != 200 || q.SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) || !bytes.Equal(raw, wantRaw[manifest.Start.Format(time.RFC3339Nano)+q.Expr]) {
				t.Fatalf("raw response/hash not preserved: %+v", q)
			}
			rawFiles++
		}
		// Report accepts exact export roots, so select every durable stream chunk.
		args = append(args, "--metrics", dir)
	}
	if rawFiles != 4 {
		t.Fatalf("expected four full raw response files, got %d", rawFiles)
	}
	output := filepath.Join(t.TempDir(), "validation.html")
	cli(append(args, "--output", output)...)
	// The consumer has only the HTML: neither a server nor source storage remains.
	for _, dir := range []string{input, storage} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	html, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	_, embedded, ok := strings.Cut(string(html), `<script id="report-data" type="application/json">`)
	if !ok {
		t.Fatal("HTML lacks embedded offline report data")
	}
	embedded, _, ok = strings.Cut(embedded, "</script>")
	if !ok {
		t.Fatal("HTML report data is not terminated")
	}
	var page report.ResultPage
	if err := json.Unmarshal([]byte(embedded), &page); err != nil {
		t.Fatal(err)
	}
	t.Run("report-progress", func(t *testing.T) { checkProgress(t, page.Progress) })
	if len(page.Sources) != 2 {
		t.Fatalf("report lost a stream chunk: %+v", page.Sources)
	}
	points := 0
	for _, source := range page.Sources {
		if source.Error != "" || len(source.Queries) != 2 || len(source.Manifest.Queries) != 2 {
			t.Fatalf("offline report lost queries: %+v", source)
		}
		for _, capture := range source.Manifest.Queries {
			body := wantRaw[source.Manifest.Start.Format(time.RFC3339Nano)+capture.Expr]
			if len(body) == 0 || capture.SHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) || capture.File != capture.Name+".json" {
				t.Fatalf("HTML lost full raw provenance: %+v", capture)
			}
		}
		for _, q := range source.Queries {
			if q.Status != metrics.StatusSuccess || len(q.Series) != 2 {
				t.Fatalf("offline report lost full label sets: %+v", q)
			}
			for _, series := range q.Series {
				if len(series.Points) != 2 || series.Samples != 2 || series.Coverage != 1 || series.Labels["container"] != "validation-only" {
					t.Fatalf("offline report lost samples or labels: %+v", series)
				}
				values, ok := map[string][2]float64{"fixture-a": {7, 11}, "fixture-b": {13, 17}}[series.Labels["pod"]]
				if !ok || series.Points[0].Value != values[0] || series.Points[1].Value != values[1] || series.Points[0].Time != float64(source.Manifest.Start.UnixMilli())/1000 || series.Points[1].Time != float64(source.Manifest.End.UnixMilli())/1000 {
					t.Fatalf("offline report changed fixture values or timestamps: %+v", series)
				}
				points += len(series.Points)
			}
		}
	}
	if points != 16 {
		t.Fatalf("expected all 16 fixture points embedded, got %d", points)
	}
	if !bytes.Contains(html, []byte("connect-src 'none'")) || !bytes.Contains(html, []byte("do not establish capacity")) || bytes.Contains(html, []byte(server.URL)) || regexp.MustCompile(`(?i)<(?:script|link|iframe|img)\b[^>]*\b(?:src|href)\s*=`).Match(html) {
		t.Fatal("report is not a self-contained, non-qualifying offline artifact")
	}
	t.Run("javascript-syntax", func(t *testing.T) {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skip("node not available; offline data assertions still ran")
		}
		_, script, ok := strings.Cut(string(html), "</script><script>")
		if !ok {
			t.Fatal("generated report JavaScript missing")
		}
		script, _, ok = strings.Cut(script, "</script>")
		if !ok {
			t.Fatal("generated report JavaScript not terminated")
		}
		cmd := exec.CommandContext(ctx, node, "--check")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated report JavaScript syntax: %v\n%s", err, out)
		}
	})
}

func TestValidationStreamCLIRejectsInvalidDurations(t *testing.T) {
	t.Setenv("HCP_PERF_BEARER_TOKEN", "")
	parent := t.TempDir()
	queries := filepath.Join(parent, "queries.json")
	if err := os.WriteFile(queries, []byte(`[{"name":"up","expr":"up"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid stream options reached endpoint: %s", r.URL)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer server.Close()
	for _, flags := range [][]string{
		{"--duration", "25h"}, {"--duration", "500ms"}, {"--chunk", "500ms"}, {"--step", "500ms"}, {"--lag", "-1s"},
	} {
		t.Run(strings.Join(flags, "="), func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "stream")
			args := []string{"metrics", "stream", "--endpoint", server.URL, "--queries", queries, "--output", output, "--duration", "2s", "--chunk", "1s", "--step", "1s", "--lag", "0s"}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := run(ctx, append(args, flags...), io.Discard); err == nil {
				t.Fatal("invalid stream options accepted")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("invalid stream options created storage: %v", err)
			}
		})
	}
}

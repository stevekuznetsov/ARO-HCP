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
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFailedMetricQueries(t *testing.T) {
	for _, scenario := range []string{"failed", "success", "missing", "duplicate", "file", "hash", "partial ref"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, content string) InputRef {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				return InputRef{Path: name, ID: name, Label: name, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
			}
			refs := RunSources{
				Spec:        write("spec.json", `{}`),
				Inventories: []InputRef{write("inventory.json", `{"items":[{"kind":"Deployment","metadata":{"name":"api","namespace":"test","uid":"deployment"},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"main","resources":{"requests":{"cpu":"100m","memory":"100Mi"}}}]}}}}]}`)},
				Pods:        write("pods.ndjson", ""), Deployments: write("deployments.ndjson", ""), StatefulSets: write("statefulsets.ndjson", ""),
			}
			queries := []map[string]string{}
			for _, name := range []string{"container-cpu", "container-cpu-time", "container-memory", "container-memory-time"} {
				queries = append(queries, map[string]string{"name": name, "status": "error"})
			}
			switch scenario {
			case "success":
				queries[0]["status"] = "success"
			case "missing":
				queries = queries[1:]
			case "duplicate":
				queries = append(queries, queries[0])
			case "file":
				queries[0]["file"] = "cpu.json"
			case "hash":
				queries[0]["sha256"] = strings.Repeat("a", 64)
			case "partial ref":
				refs.CPU.ID = "undeclared"
			}
			start := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
			manifest, err := json.Marshal(map[string]any{"start": start, "stepNanoseconds": int64(15 * time.Second), "queries": queries})
			if err != nil {
				t.Fatal(err)
			}
			refs.MetricsManifest = write("manifest.json", string(manifest))
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			reader := &sourceReader{ctx: context.Background(), root: root, sources: map[string]Source{}}
			var run Run
			_, err = exportResources(reader, RunInput{HCPNamespace: "test", Sources: refs}, &run, start, start.Add(time.Minute))
			if scenario != "failed" {
				if err == nil {
					t.Fatal("unsupported omission accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(run.Resources) != 1 || run.Resources[0].CPU != nil || run.Resources[0].Memory != nil || *run.Resources[0].MemoryRequest != 100 {
				t.Fatal("missing observations became usage or lost resource references")
			}
			for _, metric := range run.WholeHCP[:2] {
				if metric.Distribution != nil || metric.Excluded != 5 || !strings.Contains(metric.Note, "query failed") {
					t.Fatalf("missing observations lost: %+v", metric)
				}
			}
		})
	}
}

func TestMediumSavedDashboard(t *testing.T) {
	raw, err := os.ReadFile("../../docs/dashboard/medium.json")
	if err != nil {
		t.Fatal(err)
	}
	var dataset Dataset
	if err := json.Unmarshal(raw, &dataset); err != nil {
		t.Fatal(err)
	}
	if len(dataset.Sizes) != 4 {
		t.Fatal("Medium allocation and quick-campaign cohorts must remain separate panes")
	}
	q40, capacity, revised, size := dataset.Sizes[0], dataset.Sizes[1], dataset.Sizes[2], dataset.Sizes[3]
	if q40.ID != "medium-six-worker-hour" || len(q40.Runs) != 1 || !strings.Contains(q40.Budget.ExecutionSKU, "2/2/2") || q40.Budget.ModeledUSD != revised.Budget.ModeledUSD {
		t.Fatal("hour topology/cost distinction lost")
	}
	failedHour := q40.Runs[0]
	if failedHour.ActualSeconds != 3600.000295347 || failedHour.PlannedSeconds != 3600 || failedHour.Assessment.Execution != "failed" || failedHour.Assessment.RequestQuality != "failed" || failedHour.ApplicationPods != 2400 || failedHour.Applications != 480 || failedHour.GuestNodes != 16 {
		t.Fatal("full failed hour mislabeled as interrupted/inconclusive or wrong population")
	}
	if failedHour.Requests.Offered != 144000 || failedHour.Requests.Dispatched != 144000 || failedHour.Requests.Successful != 136112 || failedHour.Requests.Rejected429 != 7888 || failedHour.Requests.Timeouts != 0 || failedHour.Requests.Canceled != 0 || failedHour.Requests.GeneratorLimited != 0 || failedHour.RolloutsCompleted != 474 || failedHour.RolloutsScheduled != 480 || failedHour.Latencies[0].Distribution.P95 != 1.170067598 {
		t.Fatal("failed hour raw request/turnover/latency evidence changed")
	}
	if failedHour.CanaryCycles != 91 || failedHour.CanaryHardFailed == nil || *failedHour.CanaryHardFailed != 0 || !strings.Contains(strings.Join(failedHour.Notes, " "), "canaryFailures=1") || !strings.Contains(strings.Join(failedHour.Notes, " "), "recovered=false") {
		t.Fatal("measured canary cohort conflated with native aggregate/final recovery")
	}
	if capacity.ID != "medium-capacity-v7" || len(capacity.Runs) != 5 || capacity.Budget.ChargedCPUCores != revised.Budget.ChargedCPUCores {
		t.Fatal("fixed-allocation quick campaign missing")
	}
	for i, run := range capacity.Runs {
		early := i == 0 || i >= 3
		if run.PlannedSeconds != 180 || run.Requests.GeneratorLimited != 0 || run.Requests.Timeouts != 0 || run.Assessment.Execution != "completed" || run.Assessment.Health != "degraded" || run.Assessment.Capture != "incomplete" {
			t.Fatal("quick campaign policy/accounting changed")
		}
		if early {
			if run.ActualSeconds < 60 || run.ActualSeconds > 61 || run.Assessment.RequestQuality != "failed" || !strings.Contains(run.Assessment.Summary, "SCREENED-OUT SERVER") {
				t.Fatal("server checkpoint failure became accepted, inconclusive or full180s")
			}
		} else if run.ActualSeconds < 180 || run.ActualSeconds > 181 || run.Assessment.RequestQuality != "passed" {
			t.Fatal("full short screen acceptance lost")
		}
		if run.Requests.Successful != []int{3293, 7931, 5304, 3371, 2558}[i] || run.Requests.Canceled != []int{4, 0, 0, 23, 12}[i] || run.Latencies[0].Distribution.P95 != []float64{.68330454, .551505934, .780126198, .600159946, .764002847}[i] {
			t.Fatal("whole-run raw cohort overwritten by checkpoint or rounded histogram")
		}
		if run.ApplicationPods != []int{1200, 1200, 2400, 2400, 2400}[i] || run.GuestNodes != []int{8, 8, 16, 16, 16}[i] || run.Applications*5 != run.ApplicationPods {
			t.Fatal("per-case population/node count changed")
		}
	}
	if revised.ID != "medium-scheduler100" || len(revised.Runs) != 4 || revised.DefaultRun != "medium-scheduler100-v6-hour" || revised.Budget.RequestedCPUCores != 7.220 || size.Budget.RequestedCPUCores != 7.216 || revised.Budget.ChargedCPUCores != size.Budget.ChargedCPUCores {
		t.Fatal("allocation revisions or accounting conflated")
	}
	for i, run := range revised.Runs {
		if run.ApplicationPods != 1200 || run.Applications != 240 || run.GuestNodes != 8 || run.Assessment.Execution != "completed" || run.Assessment.Capture != "incomplete" || run.Requests.Timeouts != 0 {
			t.Fatal("revised run population/execution/capture changed")
		}
		if run.Latencies[0].Distribution.P95 != []float64{.275116668, .300230748, .366132458, .33297912}[i] {
			t.Fatal("raw percentile changed")
		}
		if i == 1 {
			if run.Requests.GeneratorLimited != 704 || run.Assessment.RequestQuality != "INCONCLUSIVE" || run.RolloutsCompleted != 240 {
				t.Fatal("V5 completed execution was rescored as accepted")
			}
		} else if run.Assessment.RequestQuality != "passed" || run.Requests.GeneratorLimited != 0 {
			t.Fatal("accepted request gate lost")
		}
	}
	accepted := revised.Runs[3]
	if accepted.Requests.Offered != 108000 || accepted.Requests.Dispatched != 108000 || accepted.Requests.Successful != 107782 || accepted.Requests.Rejected429 != 218 || accepted.RolloutsCompleted != 240 || accepted.Assessment.Health != "degraded" || accepted.ActualSeconds < 3600 || accepted.WholeHCP[0].Distribution.Count != 236 || accepted.WholeHCP[1].Distribution.Count != 239 {
		t.Fatal("V6 hour delivery or partial resource coverage changed")
	}
	if size.ID != "medium" || len(size.Runs) != 8 || len(size.Components) != 43 || size.DefaultRun != "medium-hour20" {
		t.Fatal("Medium measured cohort changed")
	}
	for i, qps := range []float64{5, 7, 10, 15, 20, 30, 20} {
		run := size.Runs[i]
		if run.OfferedQPS != qps || run.Population != "loaded" || run.ApplicationPods != 300 || run.Applications != 60 || run.GuestNodes != 3 {
			t.Fatalf("incorrect operating point: %s", run.ID)
		}
		if i < 6 && (run.ActualSeconds < 300 || run.ActualSeconds > 301 || run.Requests.SuccessfulQPS > qps || run.RolloutsCompleted != 5) {
			t.Fatal("screen duration, throughput or turnover changed")
		}
	}
	hour := size.Runs[6]
	if hour.Assessment.RequestQuality != "UNUSABLE" || hour.Assessment.Execution != "failed" || hour.Assessment.Capture != "incomplete" || hour.ActualSeconds >= hour.PlannedSeconds || hour.Requests.Canceled != 127 || hour.Requests.GeneratorLimited != 73 || hour.RolloutsCompleted != 17 || hour.RolloutsScheduled != 60 || hour.CanaryHardFailed == nil || *hour.CanaryHardFailed != 1 {
		t.Fatal("interrupted hour was qualified or lost evidence")
	}
	for _, usage := range hour.Resources {
		if usage.CPU != nil || usage.Memory != nil {
			t.Fatal("failed native range queries became measured resources")
		}
	}
	doubled := size.Runs[7]
	if doubled.ID != "medium-screen30-p600" || doubled.ApplicationPods != 600 || doubled.Applications != 120 || doubled.GuestNodes != 6 || doubled.OfferedQPS != 30 || doubled.ActualSeconds < 300 || doubled.ActualSeconds > 301 || doubled.RolloutsCompleted != 10 || doubled.RolloutsScheduled != 10 || doubled.TurnoverPeriodSeconds != 3600 {
		t.Fatal("population doubling lost its distinct residency, nodes or turnover")
	}
	if doubled.Requests.Successful != 8996 || doubled.Requests.Dispatched != 9000 || doubled.Requests.Rejected429 != 4 || doubled.Requests.Timeouts != 0 || doubled.Assessment.RequestQuality != "passed" || doubled.Assessment.Health != "degraded" || doubled.Assessment.Capture != "incomplete" {
		t.Fatal("600-Pod request or health/capture outcome changed")
	}
	if doubled.Latencies[0].ID != "api-all" || doubled.Latencies[0].Distribution.P95 != .202280204 {
		t.Fatal("raw nearest-rank p95 replaced by rounded histogram")
	}
	for _, run := range size.Runs {
		if run.ApplicationPods > 600 {
			t.Fatal("unmeasured seed failure became an API point")
		}
	}
	if !strings.Contains(size.Description, "no measured API point") {
		t.Fatal("seed failure caveat missing")
	}
	var output bytes.Buffer
	if err := Render(&output, dataset); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"600 Pods / 120 apps / 6 nodes", "1200-Pod attempt: NOT MEASURED", "1070 Ready Pods"} {
		if !strings.Contains(output.String(), text) {
			t.Fatalf("static population/diagnostic context missing: %s", text)
		}
	}
	marker := `<script id="dashboard-data" type="application/json">`
	start := strings.Index(output.String(), marker) + len(marker)
	end := start + strings.Index(output.String()[start:], "</script>")
	var embedded Dataset
	if err := json.Unmarshal(output.Bytes()[start:end], &embedded); err != nil || !reflect.DeepEqual(dataset, embedded) {
		t.Fatal("render changed the decoded dataset", err)
	}
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		chrome, err = exec.LookPath("chromium")
	}
	if err != nil {
		t.Skip("Chrome/Chromium unavailable; portable checks passed")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable; portable checks passed")
	}
	file := filepath.Join(t.TempDir(), "medium.html")
	if err := os.WriteFile(file, output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "testdata/review.mjs", chrome, file, t.TempDir(), os.Getenv("MEDIUM_DASHBOARD_REVIEW_DIR"))
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Medium browser checks failed: %v\n%s", err, result)
	} else {
		t.Log(string(result))
	}
}

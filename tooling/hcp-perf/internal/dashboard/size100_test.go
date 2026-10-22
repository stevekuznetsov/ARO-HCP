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
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSize100SavedDashboard(t *testing.T) {
	raw, err := os.ReadFile("../../docs/dashboard/size100.json")
	if err != nil {
		t.Fatal(err)
	}
	var dataset Dataset
	if err := json.Unmarshal(raw, &dataset); err != nil {
		t.Fatal(err)
	}
	if len(dataset.Sizes) != 1 || dataset.Sizes[0].ID != "size100" {
		t.Fatal("Size100 must be a separate size, not renamed Medium")
	}
	size := dataset.Sizes[0]
	if len(size.Runs) != 6 || len(size.Components) != 43 || size.DefaultRun != "size100-q15" || size.Budget.TargetUSD != .25 || size.Budget.ModeledUSD != .24986 || size.Budget.ChargedCPUCores != 4.960 || size.Budget.ChargedMemoryGiB != 20285.0/1024 || size.Budget.RequestedCPUCores != 4.741 || size.Budget.RequestedMemoryGiB != 18543.0/1024 {
		t.Fatal("Size100 allocation or measured cohort changed")
	}
	for i, run := range size.Runs[:4] {
		if run.OfferedQPS != []float64{5, 10, 15, 20}[i] || run.ApplicationPods != 300 || run.Applications != 60 || run.GuestNodes != 16 || run.PlannedSeconds != 180 || run.Requests.Successful != []int{900, 1799, 2689, 1143}[i] || run.Requests.Rejected429 != []int{0, 1, 11, 84}[i] || run.Requests.Canceled != []int{0, 0, 0, 8}[i] || run.Requests.Timeouts != 0 || run.Requests.GeneratorLimited != 0 {
			t.Fatalf("incorrect raw cohort for %s", run.ID)
		}
		if run.Latencies[0].ID != "api-all" || run.Latencies[0].Distribution.P95 != []float64{1.403548459, .622870461, .651952321, 1.353365819}[i] || run.Assessment.Health != "degraded" || run.Assessment.Capture != "incomplete" {
			t.Fatal("raw percentile or separate health/capture axes changed")
		}
		if i < 3 && (run.Assessment.RequestQuality != "passed" || run.ActualSeconds < 180 || run.ActualSeconds > 181 || run.RolloutsCompleted != 3) {
			t.Fatal("full short screen acceptance changed")
		}
		cpu, memory, containers := 0.0, 0.0, 0
		for _, resource := range run.Resources {
			cpu += *resource.CPURequest
			memory += *resource.MemoryRequest
			containers += resource.Containers
			if strings.Contains(resource.ComponentID, "olm-collect-profiles") {
				t.Fatal("completed historical Jobs charged as running components")
			}
		}
		if math.Abs(cpu-4.741) > 1e-9 || memory != 18543 || containers != 144 {
			t.Fatal("steady resources confused with scheduler/charged accounting")
		}
	}
	failed := size.Runs[3]
	if failed.ActualSeconds != 61.78640537 || failed.Requests.Dispatched != 1235 || failed.Assessment.RequestQuality != "failed" || failed.Assessment.Execution != "completed" || !strings.Contains(failed.Assessment.Summary, "76/1200") || failed.RolloutsCompleted != 1 || failed.RolloutsScheduled != 3 {
		t.Fatal("checkpoint rejection conflated with final drain or full180s")
	}
	for i, run := range size.Runs[4:] {
		if run.OfferedQPS != 15 || run.ApplicationPods != []int{600, 1200}[i] || run.Applications != []int{120, 240}[i] || run.GuestNodes != 16 || run.PlannedSeconds != 180 || run.ActualSeconds != []float64{61.600589904, 62.297076124}[i] || run.Assessment.RequestQuality != "failed" || run.Assessment.Execution != "completed" || run.Assessment.Health != "degraded" || run.Assessment.Capture != "incomplete" {
			t.Fatal("population screen outcome/duration/topology changed")
		}
		if run.Requests.Offered != []int{924, 934}[i] || run.Requests.Dispatched != []int{923, 934}[i] || run.Requests.Successful != []int{799, 832}[i] || run.Requests.Rejected429 != []int{106, 91}[i] || run.Requests.Canceled != []int{18, 11}[i] || run.Requests.GeneratorLimited != []int{1, 0}[i] || run.Requests.Timeouts != 0 || run.Latencies[0].Distribution.P95 != []float64{2.976062681, 2.686037817}[i] {
			t.Fatal("population final drain counts or raw latency changed")
		}
		if !strings.Contains(run.Assessment.Summary, []string{"106/900", "86/900"}[i]) || !strings.Contains(run.Assessment.Summary, "0cohorttimeout/cancel/generatorloss") || run.RolloutsCompleted != []int{1, 2}[i] || run.RolloutsScheduled != []int{6, 12}[i] {
			t.Fatal("complete decision cohort conflated with cutoff or intended full run")
		}
	}
	if !strings.Contains(strings.Join(size.Runs[4].Notes, " "), "entirelycanceledOffers1 atcutoff") {
		t.Fatal("canceled offer misclassified as generator ceiling")
	}
	var output bytes.Buffer
	if err := Render(&output, dataset); err != nil {
		t.Fatal(err)
	}
	marker := `<script id="dashboard-data" type="application/json">`
	start := strings.Index(output.String(), marker) + len(marker)
	end := start + strings.Index(output.String()[start:], "</script>")
	var embedded Dataset
	if err := json.Unmarshal(output.Bytes()[start:end], &embedded); err != nil || !reflect.DeepEqual(dataset, embedded) {
		t.Fatal("render changed decoded Size100 data", err)
	}
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		chrome, err = exec.LookPath("chromium")
	}
	if err != nil {
		t.Skip("Chrome unavailable; portable assertions passed")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable; portable assertions passed")
	}
	file := filepath.Join(t.TempDir(), "size100.html")
	if err := os.WriteFile(file, output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "testdata/review.mjs", chrome, file, t.TempDir(), os.Getenv("SIZE100_DASHBOARD_REVIEW_DIR"))
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Size100 browser checks: %v\n%s", err, result)
	} else {
		t.Log(string(result))
	}
}

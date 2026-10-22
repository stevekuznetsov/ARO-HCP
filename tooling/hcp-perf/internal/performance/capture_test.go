// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package performance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
)

func TestCaptureHistoryHTTPGateAndTransientRecovery(t *testing.T) {
	var failed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		end, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end"))
		if err != nil {
			t.Error(err)
			http.Error(w, "bad end", 400)
			return
		}
		stamp := strings.HasPrefix(r.URL.Query().Get("query"), "timestamp(")
		values := []any{}
		for _, offset := range []time.Duration{10 * time.Second, 5 * time.Second, 0} {
			at := end.Add(-offset)
			value := "1"
			if failed.Load() && offset == 5*time.Second {
				value = "0"
			}
			if stamp {
				value = strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64)
			}
			values = append(values, []any{float64(at.UnixMilli()) / 1000, value})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{"job": "scheduler", "instance": "one"}, "values": values}}}})
	}))
	defer server.Close()
	capture := testSpec().Capture
	capture.Endpoint = server.URL
	capture.Sources = []Source{{Job: "scheduler", Replicas: 1}}
	capture.LossGraceSeconds, capture.RecoveryGateSeconds = 60, 180
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "events"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tracker := &captureTracker{}
	j := &journal{file: f}
	if err := tracker.gate(t.Context(), server.Client(), capture, j); err != nil {
		t.Fatal(err)
	}
	// A separate tracker demonstrates a down scrape between otherwise-good
	// observations blocks admission, without aborting the live bounded policy.
	failed.Store(true)
	tracker = &captureTracker{}
	ready, err := tracker.check(t.Context(), server.Client(), capture, j)
	if err != nil || ready || tracker.status.Warnings != 1 || tracker.status.Recoveries != 1 {
		t.Fatalf("transient history: ready=%v err=%v status=%+v", ready, err, tracker.status)
	}
}

func historyPoints(base time.Time, offsets []int, values []float64) ([]metrics.Point, []metrics.Point) {
	var up, stamps []metrics.Point
	for i, offset := range offsets {
		at := base.Add(time.Duration(offset) * time.Second)
		labels := map[string]string{"job": "scheduler", "instance": "replica-1"}
		up = append(up, metrics.Point{Labels: labels, At: at, Value: values[i]})
		stamps = append(stamps, metrics.Point{Labels: labels, At: at, Value: float64(at.Unix())})
	}
	return up, stamps
}

func TestCaptureHistoryRecoveryAndLoss(t *testing.T) {
	base := time.Unix(1700000000, 0)
	capture := testSpec().Capture
	capture.LossGraceSeconds, capture.RecoveryGateSeconds = 60, 180
	source := Source{Job: "scheduler", Replicas: 1}
	for _, mode := range []string{"transient", "persistent", "between-polls", "late-recovery", "replay", "wrong-label", "one-success"} {
		t.Run(mode, func(t *testing.T) {
			tracker := &captureTracker{jobs: map[string]map[string]*captureTarget{}}
			up, stamps := historyPoints(base, []int{-5, 0}, []float64{1, 1})
			ready, _, err := tracker.observe(source, capture, up, stamps, base, base)
			if err != nil || !ready {
				t.Fatalf("initial admission: %v ready=%v", err, ready)
			}
			tracker.armed = true
			offsets, values := []int{5, 10, 15}, []float64{0, 1, 1}
			end := base.Add(15 * time.Second)
			switch mode {
			case "persistent":
				offsets, values = []int{5, 65}, []float64{0, 0}
				end = base.Add(65 * time.Second)
			case "between-polls":
				offsets, values = []int{5, 40, 70}, []float64{0, 1, 0}
				end = base.Add(70 * time.Second)
			case "late-recovery":
				offsets, values = []int{5, 70, 75}, []float64{0, 1, 1}
				end = base.Add(75 * time.Second)
			case "replay":
				offsets, values = []int{65}, []float64{1}
				end = base.Add(65 * time.Second)
			case "one-success":
				offsets, values = []int{5, 10}, []float64{0, 1}
				end = base.Add(10 * time.Second)
			}
			up, stamps = historyPoints(base, offsets, values)
			if mode == "replay" {
				stamps[0].Value = float64(base.Unix())
			}
			if mode == "wrong-label" {
				up[0].Labels["instance"] = "replacement"
			}
			ready, events, err := tracker.observe(source, capture, up, stamps, end, end)
			wantStop := mode == "persistent" || mode == "late-recovery" || mode == "replay" || mode == "wrong-label"
			if (err != nil) != wantStop {
				t.Fatalf("stop=%v: %v", wantStop, err)
			}
			if !wantStop && ready != (mode == "transient") {
				t.Fatalf("incorrect admission after %s", mode)
			}
			if mode == "transient" {
				if tracker.status.Warnings != 1 || tracker.status.Recoveries != 1 || !tracker.status.Incomplete || len(events) != 3 || events[1].RecoverySeconds != 5 {
					t.Fatalf("missing failure/recovery evidence: %+v %+v", tracker.status, events)
				}
			}
		})
	}
}

func TestCaptureRepeatedEvaluationIsNotConfirmation(t *testing.T) {
	base := time.Unix(1700000000, 0)
	capture := testSpec().Capture
	capture.LossGraceSeconds = 60
	tracker := &captureTracker{jobs: map[string]map[string]*captureTarget{}}
	up, stamps := historyPoints(base, []int{0, 5}, []float64{1, 1})
	stamps[1].Value = stamps[0].Value
	ready, _, err := tracker.observe(Source{Job: "scheduler", Replicas: 1}, capture, up, stamps, base.Add(5*time.Second), base.Add(5*time.Second))
	if err != nil || ready {
		t.Fatalf("replayed success counted twice: %v", err)
	}
}

func TestCaptureHistoryGateCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	capture := testSpec().Capture
	capture.Endpoint = server.URL
	capture.LossGraceSeconds, capture.RecoveryGateSeconds = 60, 180
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "events"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	tracker := &captureTracker{}
	if err := tracker.gate(ctx, server.Client(), capture, &journal{file: f}); err == nil {
		t.Fatal("gate ignored context deadline")
	}
	if tracker.status.Warnings == 0 || tracker.armed {
		t.Fatal("failed query admitted or missing evidence")
	}
}

func TestCaptureHistorySpecOptIn(t *testing.T) {
	spec := testSpec()
	spec.Capture.LossGraceSeconds = 60
	if spec.Validate() == nil {
		t.Fatal("half-configured policy accepted")
	}
	spec.Capture.RecoveryGateSeconds = 180
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	spec.Capture.JobTiming = map[string]JobTiming{"kas": {Interval: time.Minute, Timeout: 45 * time.Second, MaxAge: 165 * time.Second}}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	spec.Capture.RecoveryGateSeconds = 120
	if spec.Validate() == nil {
		t.Fatal("gate shorter than slow source policy accepted")
	}
}

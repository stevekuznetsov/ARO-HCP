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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestJobTimingValidation(t *testing.T) {
	for _, mode := range []string{"kas", "legacy", "default", "unknown-job", "zero", "timeout", "too-old", "too-short", "fraction", "overflow", "global165"} {
		t.Run(mode, func(t *testing.T) {
			spec := testSpec()
			timing := JobTiming{Interval: time.Minute, Timeout: 45 * time.Second, MaxAge: 165 * time.Second}
			job := "kas"
			switch mode {
			case "unknown-job":
				job = "kas-typo"
			case "zero":
				timing = JobTiming{}
			case "timeout":
				timing.Timeout = 61 * time.Second
			case "too-old":
				timing.MaxAge = 166 * time.Second
			case "too-short":
				timing.MaxAge = 104 * time.Second
			case "fraction":
				timing.Interval += time.Nanosecond
			case "overflow":
				timing.Interval = time.Duration(1<<63 - 1)
			case "global165":
				spec.Capture.MaxAgeSeconds = 165
			case "default":
				spec.Capture.MaxAgeSeconds = 0
			}
			spec.Capture.JobTiming = map[string]JobTiming{job: timing}
			if mode == "legacy" {
				spec.Capture.JobTiming = nil
			}
			before, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			wantValid := mode == "kas" || mode == "legacy" || mode == "default"
			if err := spec.Validate(); (err == nil) != wantValid {
				t.Fatalf("validation: %v", err)
			}
			after, err := json.Marshal(spec)
			if err != nil || string(before) != string(after) {
				t.Fatal("validation changed frozen input")
			}
			if wantValid && spec.Capture.maxAge("oas") != 45*time.Second {
				t.Fatal("KAS policy broadened OAS freshness")
			}
		})
	}
}

func TestCaptureJobTimingIsPerJob(t *testing.T) {
	for _, mode := range []string{"fresh", "kas-stale", "oas-stale", "oas-down", "wrong-job", "missing", "no-policy"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query().Get("query")
				job, age := "oas", 30*time.Second
				if strings.Contains(query, `job="kas"`) {
					job, age = "kas", 150*time.Second
				}
				if mode == "kas-stale" && job == "kas" {
					age = 170 * time.Second
				}
				if mode == "oas-stale" && job == "oas" {
					age = 50 * time.Second
				}
				value := 1.0
				if mode == "oas-down" && job == "oas" {
					value = 0
				}
				if strings.HasPrefix(query, "timestamp(") {
					value = float64(time.Now().Add(-age).Unix())
				}
				if mode == "wrong-job" {
					job = "healthy-unrelated-backend"
				}
				count := 3
				if mode == "missing" && job == "kas" {
					count = 2
				}
				points := []any{}
				for i := range count {
					points = append(points, map[string]any{"metric": map[string]string{"job": job, "instance": fmt.Sprint(i)}, "value": []any{time.Now().Unix(), fmt.Sprint(value)}})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": points}})
			}))
			defer server.Close()
			capture := testSpec().Capture
			capture.Endpoint = server.URL
			capture.Sources = []Source{{Job: "kas", Replicas: 3}, {Job: "oas", Replicas: 3}}
			capture.JobTiming = map[string]JobTiming{"kas": {Interval: time.Minute, Timeout: 45 * time.Second, MaxAge: 165 * time.Second}}
			if mode == "no-policy" {
				capture.JobTiming = nil
			}
			if err := captureReady(t.Context(), server.Client(), capture); (err == nil) != (mode == "fresh") {
				t.Fatalf("per-job freshness: %v", err)
			}
		})
	}
}

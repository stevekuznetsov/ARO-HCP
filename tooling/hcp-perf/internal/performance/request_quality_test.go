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
	"math"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func TestRequestQualityPolicy(t *testing.T) {
	for _, mode := range []string{"clean", "five-percent", "five-point-zero-one", "three-timeout", "three-plus-three", "overlap", "generator", "zero", "old-accounting", "403", "500", "403-timeout", "nan", "short", "soft-latency", "debt", "default-timeout"} {
		t.Run(mode, func(t *testing.T) {
			spec := testSpec()
			spec.MeasurementSeconds = 300
			spec.API = &apiload.Spec{DurationSeconds: 300, RatePerSecond: 100}
			spec.RequestQualityPolicy = &RequestQualityPolicy{MaxDegradedBasisPoints: 500}
			start := time.Now()
			api := &apiload.Result{RequestAccountingVersion: apiload.RequestAccountingVersion, Status: "completed", Started: start, SchedulingStopped: start.Add(300 * time.Second), Finished: start.Add(301 * time.Second), SuccessfulBodyQPS: 90, Counts: apiload.Counts{Offered: 30000, Dispatched: 30000, Completed: 30000, Successful: 30000}}
			spec.Seed.Applications = 0
			turnover := &workload.TurnoverResult{StartedAt: start, AdmissionEnd: start.Add(300 * time.Second), FinishedAt: start.Add(330 * time.Second)}
			wantStatus, wantCapacity, wantErr := "met", "acceptable", false
			switch mode {
			case "five-percent":
				api.Successful -= 1500
				api.Rejected429 = 1500
				api.DegradedRequests = 1500
				wantStatus = "degraded"
			case "five-point-zero-one":
				api.Successful -= 1503
				api.Rejected429 = 1503
				api.DegradedRequests = 1503
				wantStatus, wantCapacity, wantErr = "exceeded", "exceeded", true
			case "three-timeout":
				api.Successful -= 900
				api.Timeouts = 900
				api.DegradedRequests = 900
				wantStatus = "degraded"
			case "three-plus-three":
				api.Successful -= 1800
				api.Rejected429 = 900
				api.Timeouts = 900
				api.DegradedRequests = 1800
				wantStatus, wantCapacity, wantErr = "exceeded", "exceeded", true
			case "overlap":
				api.Successful -= 1500
				api.Rejected429 = 1500
				api.Timeouts = 1500
				api.DegradedRequests = 1500
				wantStatus = "degraded"
			case "generator":
				api.Successful--
				api.Dispatched--
				api.Completed--
				api.GeneratorLimited++
				wantCapacity, wantErr = "inconclusive", true
			case "zero":
				api.Dispatched = 0
				api.Completed = 0
				api.Successful = 0
				wantStatus, wantCapacity, wantErr = "not-measured", "unusable", true
			case "old-accounting":
				api.RequestAccountingVersion = ""
				wantStatus, wantCapacity, wantErr = "not-measured", "unusable", true
			case "403", "500":
				api.Successful--
				api.OtherHTTP++
				wantCapacity, wantErr = "unusable", true
			case "403-timeout":
				api.Successful--
				api.OtherHTTP++
				api.Timeouts++
				api.DegradedRequests++
				wantStatus, wantCapacity, wantErr = "degraded", "unusable", true
			case "nan":
				api.SuccessfulBodyQPS = math.NaN()
				wantStatus, wantCapacity, wantErr = "not-measured", "unusable", true
			case "short":
				api.SchedulingStopped = start.Add(time.Second)
				wantCapacity, wantErr = "inconclusive", true
			case "soft-latency":
				api.SuccessfulLatencyP95 = 9 * time.Second
				api.SuccessfulLatencyP99 = 10 * time.Second
			case "debt":
				turnover.Backlog = 1
				wantCapacity = "inconclusive"
				wantErr = true
			case "default-timeout":
				spec.RequestQualityPolicy = nil
				api.Successful--
				api.Timeouts++
				api.DegradedRequests++
				wantErr = true
			}
			// Retries/HTTP attempts are deliberately not the quality denominator.
			api.HTTPAttempts = 2 * api.Dispatched
			a, err := ValidateResult(spec, Results{API: api, Turnover: turnover}, mode == "soft-latency")
			if (err != nil) != wantErr {
				t.Fatalf("assessment %+v: %v", a, err)
			}
			if spec.RequestQualityPolicy == nil {
				if a.RequestQuality != nil {
					t.Fatal("retroactive policy applied")
				}
				return
			}
			q := a.RequestQuality
			if q == nil || q.Status != wantStatus || q.CapacityStatus != wantCapacity || q.Dispatched != api.Dispatched {
				t.Fatalf("quality %+v, want %s/%s", q, wantStatus, wantCapacity)
			}
			if mode == "overlap" && (q.DegradedFraction == nil || *q.DegradedFraction != 0.05) {
				t.Fatal("429 timeout counted twice")
			}
			if mode == "soft-latency" && a.PerformanceStatus != "degraded" {
				t.Fatal("latency degradation not reported")
			}
		})
	}
}

func TestRequestQualitySpecValidation(t *testing.T) {
	spec := testSpec()
	spec.RequestQualityPolicy = &RequestQualityPolicy{MaxDegradedBasisPoints: 500}
	if spec.Validate() == nil {
		t.Fatal("quality policy without API accepted")
	}
	spec.API = &apiload.Spec{Version: "v1", DurationSeconds: 3600, RatePerSecond: 1, MaxInFlight: 1, RequestTimeoutSeconds: 10, Namespaces: []string{"fresh-0"}, Targets: []apiload.Target{{Namespace: "fresh-0", APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Name: "app-000000", Weight: 1}}}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []int{-1, 10001} {
		spec.RequestQualityPolicy.MaxDegradedBasisPoints = value
		if spec.Validate() == nil {
			t.Fatalf("invalid threshold %d accepted", value)
		}
	}
}

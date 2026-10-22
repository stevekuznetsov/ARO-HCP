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
	"errors"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func TestValidateResultDeliveredWork(t *testing.T) {
	for _, mode := range []string{"success", "all403", "some429", "all429", "timeout", "canceled", "body", "generator", "debt", "outstanding", "short-hour", "canary"} {
		t.Run(mode, func(t *testing.T) {
			spec := testSpec()
			spec.API = &apiload.Spec{DurationSeconds: 3600, RatePerSecond: 1}
			start := time.Now()
			turnover := &workload.TurnoverResult{StartedAt: start, AdmissionEnd: start.Add(time.Hour), FinishedAt: start.Add(time.Hour + 30*time.Second), Scheduled: 1, Admitted: 1, Accepted: 1, Completed: 1}
			api := &apiload.Result{Status: "completed", Started: start, SchedulingStopped: start.Add(time.Hour), Finished: start.Add(time.Hour), Counts: apiload.Counts{Offered: 3600, Dispatched: 3600, Completed: 3600, Successful: 3600}}
			switch mode {
			case "all403":
				api.Successful = 0
				api.OtherHTTP = 3600
			case "some429":
				api.Successful = 1800
				api.Rejected429 = 1800
			case "all429":
				api.Successful = 0
				api.Rejected429 = 3600
			case "timeout":
				api.Successful--
				api.Timeouts++
			case "canceled":
				api.Status = "canceled"
				api.Canceled++
				api.Successful--
			case "body":
				api.Successful--
				api.BodyLimitErrors++
			case "generator":
				api.Successful--
				api.Completed--
				api.Dispatched--
				api.GeneratorLimited++
			case "debt":
				turnover.Completed = 0
				turnover.NotStarted = 1
				turnover.Backlog = 1
			case "outstanding":
				turnover.Outstanding = 1
			case "short-hour":
				turnover.FinishedAt = start.Add(5 * time.Minute)
				api.SchedulingStopped = start.Add(5 * time.Minute)
			}
			assessment, err := ValidateResult(spec, Results{Turnover: turnover, API: api}, mode == "canary")
			wantSuccess := mode == "success" || mode == "some429" || mode == "canary"
			if mode == "canary" && assessment.PerformanceStatus != "degraded" {
				t.Fatal("soft canary degradation was not recorded separately")
			}
			if (err == nil) != wantSuccess {
				t.Fatalf("assessment %+v: %v", assessment, err)
			}
			if !wantSuccess {
				var violation *WorkloadViolation
				if !errors.As(err, &violation) || assessment.Status != "violated" {
					t.Fatal("workload failure not classified separately")
				}
			}
			if mode == "short-hour" && assessment.FullWindow {
				t.Fatal("short coverage claimed whole hour")
			}
			if mode == "some429" && assessment.Shed429 != 1800 {
				t.Fatal("lost shedding evidence")
			}
		})
	}
}

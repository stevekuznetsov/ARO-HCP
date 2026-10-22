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
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"testing"
)

func TestScreenRequests(t *testing.T) {
	policy := RequestQualityPolicy{MaxDegradedBasisPoints: 500, ScreeningSeconds: []int{300, 600}, MinimumCompletedRequests: 100}
	for _, mode := range []string{"boundary", "exceeded", "overlap", "pending", "minimum", "generator", "permission"} {
		t.Run(mode, func(t *testing.T) {
			c := apiload.Checkpoint{AfterSeconds: 300, Counts: apiload.Counts{Offered: 10000, Dispatched: 10000, Completed: 10000, Successful: 9500, DegradedRequests: 500, Rejected429: 500}}
			want := "continue-not-qualified"
			switch mode {
			case "exceeded":
				c.Successful--
				c.DegradedRequests++
				c.Timeouts++
				want = "rejected"
			case "overlap":
				c.Timeouts = 500
			case "pending":
				c.Completed--
				c.Successful--
				want = "inconclusive"
			case "minimum":
				c.Completed = 99
				want = "inconclusive"
			case "generator":
				c.GeneratorLimited = 1
				want = "inconclusive"
			case "permission":
				c.OtherHTTP = 1
				c.Successful--
				want = "unusable"
			}
			if got := screenRequests(policy, c); got.Status != want {
				t.Fatalf("got %+v want %s", got, want)
			}
		})
	}
}

func TestScreeningSpecValidation(t *testing.T) {
	spec := testSpec()
	spec.API = &apiload.Spec{Version: "v1", DurationSeconds: 3600, RatePerSecond: 1, MaxInFlight: 1, RequestTimeoutSeconds: 10, Namespaces: []string{"fresh-0"}, Targets: []apiload.Target{{Namespace: "fresh-0", APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Name: "app-000000", Weight: 1}}}
	spec.RequestQualityPolicy = &RequestQualityPolicy{MaxDegradedBasisPoints: 500, ScreeningSeconds: []int{300, 600}, MinimumCompletedRequests: 100}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, checkpoints := range [][]int{{0}, {600, 300}, {300, 300}, {3600}, {60}} {
		spec.RequestQualityPolicy.ScreeningSeconds = checkpoints
		if spec.Validate() == nil {
			t.Fatalf("invalid checkpoints accepted: %v", checkpoints)
		}
	}
	spec.RequestQualityPolicy.ScreeningSeconds = []int{300}
	spec.RequestQualityPolicy.MinimumCompletedRequests = 0
	if spec.Validate() == nil {
		t.Fatal("missing sample minimum accepted")
	}
}

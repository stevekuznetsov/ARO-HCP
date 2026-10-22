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

import "github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"

type ScreeningDecision struct {
	Checkpoint             apiload.Checkpoint `json:"checkpoint"`
	Status                 string             `json:"status"`
	MaxDegradedBasisPoints int                `json:"maxDegradedBasisPoints"`
}

type screeningRejected struct{}

func (*screeningRejected) Error() string { return "predeclared request quality screen exceeded" }

func screenRequests(policy RequestQualityPolicy, checkpoint apiload.Checkpoint) ScreeningDecision {
	decision := ScreeningDecision{Checkpoint: checkpoint, Status: "inconclusive", MaxDegradedBasisPoints: policy.MaxDegradedBasisPoints}
	c := checkpoint.Counts
	if c.Completed < policy.MinimumCompletedRequests || c.Completed != c.Dispatched || c.Dispatched != c.Offered || c.GeneratorLimited != 0 || c.Dispatched > apiload.MaxAttempts || c.Dispatched <= 0 {
		return decision
	}
	if c.DegradedRequests < max(c.Timeouts, c.Rejected429) || c.DegradedRequests > c.Timeouts+c.Rejected429 || c.DegradedRequests > c.Completed || c.Successful+c.DegradedRequests != c.Completed || c.OtherHTTP != 0 || c.TransportErrors != 0 || c.Canceled != 0 || c.BodyLimitErrors != 0 {
		decision.Status = "unusable"
		return decision
	}
	decision.Status = "continue-not-qualified"
	if int64(c.DegradedRequests)*10000 > int64(c.Dispatched)*int64(policy.MaxDegradedBasisPoints) {
		decision.Status = "rejected"
	}
	return decision
}

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
	"strings"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

type Results struct {
	Screening []ScreeningDecision      `json:"screening,omitempty"`
	Turnover  *workload.TurnoverResult `json:"turnover,omitempty"`
	API       *apiload.Result          `json:"api,omitempty"`
}

type Assessment struct {
	HealthStatus      string                    `json:"healthStatus,omitempty"`
	CaptureStatus     string                    `json:"captureStatus,omitempty"`
	Warnings          []string                  `json:"warnings,omitempty"`
	RequestQuality    *RequestQualityAssessment `json:"requestQuality,omitempty"`
	PerformanceStatus string                    `json:"performanceStatus,omitempty"`
	Status            string                    `json:"status"`
	FullWindow        bool                      `json:"fullWindow"`
	Violations        []string                  `json:"violations"`
	Shed429           int                       `json:"shed429"`
}

// Final reporting axes do not change workload acceptance. Missing/incomplete
// telemetry and probe failures must not be labeled "within expectations".
func reportObservation(out *Outcome) {
	out.Workload.HealthStatus = "observed-healthy"
	out.Workload.CaptureStatus = "observed-not-lossless"
	healthDegraded := out.Stability.UnhealthySamples > 0 || out.Stability.Restarts > 0 || out.Stability.OOMs > 0 || out.Stability.ProbeFailures > 0 || out.CanaryFailures > 0 || out.Critical.UnknownSamples > 0
	for _, availability := range out.Critical.Availability {
		healthDegraded = healthDegraded || availability.Minimum < 2 || availability.BelowTwoSince != nil
	}
	if healthDegraded {
		out.Workload.HealthStatus = "degraded"
		out.Workload.Warnings = append(out.Workload.Warnings, "health degradation or incomplete critical evaluation observed")
	}
	captureIncomplete := out.CaptureHistory.Incomplete || out.CaptureHistory.Warnings > 0 || out.RecordStatus != "recorded-not-lossless" || out.MetricsStatus != "exported-not-qualified"
	if captureIncomplete {
		out.Workload.CaptureStatus = "incomplete"
		out.Workload.Warnings = append(out.Workload.Warnings, "capture gaps or incomplete recorder/export evidence")
	}
	if healthDegraded || captureIncomplete {
		out.Degraded = true
		out.Workload.PerformanceStatus = "degraded"
	}
}

type RequestQualityAssessment struct {
	Status                 string   `json:"status"`         // met, degraded (accepted), exceeded, not-measured
	CapacityStatus         string   `json:"capacityStatus"` // acceptable, exceeded, inconclusive, unusable
	MaxDegradedBasisPoints int      `json:"maxDegradedBasisPoints"`
	Dispatched             int      `json:"dispatched"`
	DegradedRequests       int      `json:"degradedRequests"`
	Rejected429            int      `json:"rejected429"`
	Timeouts               int      `json:"timeouts"`
	DegradedFraction       *float64 `json:"degradedFraction,omitempty"` // reporting only
}

// WorkloadViolation is a completed assessment, not a harness crash or permission
// to change budgets. Returning it makes the typed iteration action FAILED and
// prevents completed dependencies from advancing. No automatic retry is allowed.
type WorkloadViolation struct{ Violations []string }

func (e *WorkloadViolation) Error() string {
	return "workload requirements not met: " + strings.Join(e.Violations, "; ")
}

// AssessedFailure is emitted only after a completed execution, durable outcome
// and verified absence of owned competitor load. Lost or uncertain executions
// retain iteration's normal BLOCKED measurement semantics.
type AssessedFailure struct{ Err error }

func (e *AssessedFailure) Error() string { return e.Err.Error() }
func (e *AssessedFailure) Unwrap() error { return e.Err }

// ValidateResult checks delivered work, not merely whether generators returned
// nil. 429 shedding is measured separately; zero useful responses, generator
// debt, other HTTP errors and incomplete windows cannot qualify a workload.
func ValidateResult(spec Spec, results Results, degraded bool) (Assessment, error) {
	a := Assessment{Status: "met-not-qualified", FullWindow: true, Violations: []string{}}
	a.PerformanceStatus = "within-expectations"
	if degraded {
		a.PerformanceStatus = "degraded"
	}
	window := time.Duration(spec.MeasurementSeconds) * time.Second
	schedule, scheduleErr := experiment.RolloutsForPeriod(spec.Seed.Applications, spec.ScheduleSeed, window, time.Hour)
	t := results.Turnover
	if !spec.Unloaded && (t == nil || t.StartedAt.IsZero() || t.AdmissionEnd.Sub(t.StartedAt) != window || t.FinishedAt.Before(t.AdmissionEnd.Add(time.Duration(spec.RecoverySeconds)*time.Second))) {
		a.FullWindow = false
		a.Violations = append(a.Violations, "turnover window incomplete")
	}
	if spec.Unloaded && t != nil {
		a.Violations = append(a.Violations, "unloaded comparison unexpectedly ran turnover")
	}
	if t != nil && (scheduleErr != nil || t.Scheduled != len(schedule) || t.Completed != t.Scheduled || t.Admitted != t.Scheduled || t.Accepted != t.Scheduled || t.Backlog != 0 || t.Outstanding != 0 || t.NotStarted != 0 || t.Failed != 0) {
		a.Violations = append(a.Violations, "turnover work incomplete or debt remains")
	}
	if spec.API != nil {
		r := results.API
		if r == nil || r.Status != "completed" || r.Started.IsZero() || r.SchedulingStopped.Sub(r.Started) < window || r.Offered != spec.API.DurationSeconds*spec.API.RatePerSecond {
			a.FullWindow = false
			a.Violations = append(a.Violations, "API offer window incomplete")
		}
		if r != nil {
			a.Shed429 = r.Rejected429
			if r.Successful == 0 {
				a.Violations = append(a.Violations, "API workload unusable: no successful response bodies")
			}
			if r.GeneratorLimited != 0 || r.Dispatched != r.Offered || r.Completed != r.Dispatched {
				a.Violations = append(a.Violations, "API offered work not fully dispatched/completed")
			}
			if spec.RequestQualityPolicy == nil && (r.OtherHTTP != 0 || r.TransportErrors != 0 || r.Timeouts != 0 || r.Canceled != 0 || r.BodyLimitErrors != 0 || r.Successful+r.Rejected429 != r.Completed) {
				a.Violations = append(a.Violations, "API non-shedding request failures")
			}
		}
	}
	if policy := spec.RequestQualityPolicy; policy != nil {
		quality, issues := assessRequestQuality(*policy, results.API, a.FullWindow)
		if len(a.Violations) != 0 && quality.CapacityStatus == "acceptable" {
			quality.CapacityStatus = "inconclusive"
		}
		a.RequestQuality = &quality
		a.Violations = append(a.Violations, issues...)
	}
	if len(a.Violations) != 0 {
		a.Status = "violated"
		return a, &WorkloadViolation{Violations: append([]string(nil), a.Violations...)}
	}
	return a, nil
}

func assessRequestQuality(policy RequestQualityPolicy, r *apiload.Result, fullWindow bool) (RequestQualityAssessment, []string) {
	q := RequestQualityAssessment{Status: "not-measured", CapacityStatus: "unusable", MaxDegradedBasisPoints: policy.MaxDegradedBasisPoints}
	if r == nil {
		return q, []string{"request quality has no API results"}
	}
	q.Dispatched, q.DegradedRequests, q.Rejected429, q.Timeouts = r.Dispatched, r.DegradedRequests, r.Rejected429, r.Timeouts
	// Bound all arithmetic before the exact basis-point comparison. Old results
	// lack event-derived union accounting and are never retrospectively guessed.
	if policy.MaxDegradedBasisPoints < 0 || policy.MaxDegradedBasisPoints > 10000 || r.RequestAccountingVersion != apiload.RequestAccountingVersion || r.Dispatched <= 0 || r.Dispatched > apiload.MaxAttempts {
		return q, []string{"request quality denominator or union accounting unavailable"}
	}
	for _, count := range []int{r.Offered, r.Completed, r.Successful, r.Rejected429, r.Timeouts, r.DegradedRequests, r.OtherHTTP, r.TransportErrors, r.Canceled, r.BodyLimitErrors, r.GeneratorLimited} {
		if count < 0 || count > apiload.MaxAttempts {
			return q, []string{"request quality counters invalid"}
		}
	}
	if r.Completed > r.Dispatched || r.Dispatched > r.Offered || r.DegradedRequests < max(r.Rejected429, r.Timeouts) || r.DegradedRequests > r.Rejected429+r.Timeouts || r.DegradedRequests > r.Completed || r.Successful > r.Completed || r.Successful+r.DegradedRequests > r.Completed || r.Successful == 0 || math.IsNaN(r.SuccessfulBodyQPS) || math.IsInf(r.SuccessfulBodyQPS, 0) || r.SuccessfulBodyQPS < 0 {
		return q, []string{"request quality unusable or inconsistent response accounting"}
	}
	fraction := float64(r.DegradedRequests) / float64(r.Dispatched)
	q.DegradedFraction = &fraction
	q.Status, q.CapacityStatus = "met", "acceptable"
	if r.DegradedRequests > 0 {
		q.Status = "degraded"
	}
	var issues []string
	if int64(r.DegradedRequests)*10000 > int64(r.Dispatched)*int64(policy.MaxDegradedBasisPoints) {
		q.Status, q.CapacityStatus = "exceeded", "exceeded"
		issues = append(issues, "combined 429-or-timeout fraction exceeds request quality policy")
	}
	// Authentication, unexpected HTTP and non-timeout transport/body failures are
	// never hidden inside a percentage allowance, including 403+body timeout.
	if r.OtherHTTP != 0 || r.TransportErrors != 0 || r.Canceled != 0 || r.BodyLimitErrors != 0 || r.Successful+r.DegradedRequests != r.Completed {
		q.CapacityStatus = "unusable"
		issues = append(issues, "request quality has non-permitted HTTP, transport or body failures")
	}
	if !fullWindow || r.Status != "completed" || r.Completed != r.Dispatched || r.GeneratorLimited != 0 || r.Offered != r.Dispatched {
		if q.CapacityStatus != "unusable" {
			q.CapacityStatus = "inconclusive"
		}
		issues = append(issues, "request quality capacity inconclusive: incomplete delivery/window or generator-limited")
	}
	return q, issues
}

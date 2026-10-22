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

// Package experiment describes local experiment intentions, not observed state
// or a live runner. Planning performs no I/O and never optimizes coefficients.
package experiment

import (
	"fmt"
	"math/rand"
	"regexp"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

const (
	Version       = "v1"
	MaxGuestNodes = 1000
	// MaxApplications bounds population and schedule arithmetic.
	MaxApplications      = 40000
	MaxScheduledRollouts = 80000
	canaryCycles         = 5
	canaryMultiplier     = 3
)

var dnsLabel = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// Seconds is an integer duration in seconds in both Go configuration and JSON.
type Seconds int64

// Deadlines requires positive bounds of at most 24 hours per phase; an omitted
// or zero settle bound defaults to 300 seconds.
// These are planning bounds, not evidence that convergence can be achieved.
// Calibration is required as configuration even when a quiet case omits it.
type Deadlines struct {
	Startup     Seconds `json:"startup"`
	Seeding     Seconds `json:"seeding"`
	Calibration Seconds `json:"calibration"`
	Settle      Seconds `json:"settle"`
	Shutdown    Seconds `json:"shutdown"`
}

type Spec struct {
	Version               string    `json:"version"`
	Mode                  string    `json:"mode,omitempty"`
	SteadySeconds         int64     `json:"steadySeconds,omitempty"`
	TurnoverPeriodSeconds int64     `json:"turnoverPeriodSeconds,omitempty"`
	CanaryCadenceSeconds  int64     `json:"canaryCadenceSeconds,omitempty"`
	RecoverySeconds       int64     `json:"recoverySeconds,omitempty"`
	Name                  string    `json:"name"`
	ExpectedProfile       string    `json:"expectedProfile"`
	GuestNodes            int64     `json:"guestNodes"`
	Applications          int64     `json:"applications"`
	Seed                  int64     `json:"seed"`
	CPUTreatment          string    `json:"cpuTreatment"`
	DeadlinesSeconds      Deadlines `json:"deadlinesSeconds"`
}

// Workload records total application counts independent of guest nodes; system/canary pods and rollout
// surge are additional headroom. Each application also has one Service,
// ConfigMap, and Secret. No workload coefficient is inferred or optimized.
type Workload struct {
	ReplicasPerApplication   int64 `json:"replicasPerApplication"`
	ApplicationsPerNamespace int64 `json:"applicationsPerNamespace"`
	Applications             int64 `json:"applications"`
	Namespaces               int64 `json:"namespaces"`
	Pods                     int64 `json:"pods"`
	Rollouts                 int64 `json:"rollouts"`
}

// Phase specifies an upper time bound, except steady which is a fixed window.
// Recovery admits no new rollouts; contended cases retain CPU competition.
// Startup, seeding, calibration, and settle require future executor convergence
// predicates. Shutdown bounds stopping generators, not capture or HCP deletion.
type Phase struct {
	Name            string  `json:"name"`
	DurationSeconds Seconds `json:"durationSeconds"`
}

type Canary struct {
	Enabled              bool    `json:"enabled"`
	BaselineCycles       int     `json:"baselineCycles"`
	CadenceSeconds       Seconds `json:"cadenceSeconds"`
	MaxConcurrent        int     `json:"maxConcurrent"`
	Multiplier           int     `json:"multiplier"`
	AckBudgetSeconds     Seconds `json:"ackBudgetSeconds"`
	ReadyBudgetSeconds   Seconds `json:"readyBudgetSeconds"`
	CleanupBudgetSeconds Seconds `json:"cleanupBudgetSeconds"`
}

// PlanResult contains only resolved configuration, not execution status or
// measured canary deadlines. Cases without guest nodes omit the worker canary baseline.
type PlanResult struct {
	Version  string                 `json:"version"`
	Spec     Spec                   `json:"spec"`
	Sizing   sizing.ResolvedProfile `json:"sizing"`
	Workload Workload               `json:"workload"`
	Canary   Canary                 `json:"canary"`
	Phases   []Phase                `json:"phases"`
	// MaxDurationSeconds sums only the planned phase bounds. It is NOT a total
	// worker/job deadline: capture lifecycle and other executor overhead are not
	// modeled here and require separate explicit bounds in a future runner.
	MaxDurationSeconds Seconds `json:"maxDurationSeconds"`
}

// Plan validates all inputs before returning a deterministic, self-contained
// configuration. It does not mutate inputs. Errors return an empty result.
func Plan(spec Spec, profile sizing.Profile) (PlanResult, error) {
	if spec.Version != Version {
		return PlanResult{}, fmt.Errorf("version must be %q", Version)
	}
	if spec.Mode == "" {
		spec.Mode = "soak"
	}
	if spec.Mode != "soak" && spec.Mode != "validation" {
		return PlanResult{}, fmt.Errorf("mode must be soak or validation")
	}
	steady, cadence, recovery := int64(7200), int64(300), int64(600)
	if spec.Mode == "validation" {
		steady, cadence, recovery = 300, 30, 30
	}
	if spec.SteadySeconds == 0 {
		spec.SteadySeconds = steady
	}
	if spec.CanaryCadenceSeconds == 0 {
		spec.CanaryCadenceSeconds = cadence
	}
	if spec.RecoverySeconds == 0 {
		spec.RecoverySeconds = recovery
	}
	if spec.TurnoverPeriodSeconds == 0 {
		spec.TurnoverPeriodSeconds = 3600
	}
	if spec.TurnoverPeriodSeconds < 1 || spec.TurnoverPeriodSeconds > 86400 {
		return PlanResult{}, fmt.Errorf("turnoverPeriodSeconds must be in [1,86400]")
	}
	if spec.Mode == "soak" && spec.SteadySeconds != 7200 {
		return PlanResult{}, fmt.Errorf("soak steadySeconds must be 7200")
	}
	if spec.Mode == "validation" && (spec.SteadySeconds < 30 || spec.SteadySeconds > 1800) {
		return PlanResult{}, fmt.Errorf("validation steadySeconds must be in [30,1800]")
	}
	if spec.CanaryCadenceSeconds < 5 || spec.CanaryCadenceSeconds > spec.SteadySeconds {
		return PlanResult{}, fmt.Errorf("canaryCadenceSeconds must be in [5,steadySeconds]")
	}
	maxRecovery := int64(600)
	if spec.Mode == "validation" {
		maxRecovery = 120
	}
	if spec.RecoverySeconds < 1 || spec.RecoverySeconds > maxRecovery {
		return PlanResult{}, fmt.Errorf("recoverySeconds must be in [1,%d]", maxRecovery)
	}
	if spec.DeadlinesSeconds.Settle == 0 {
		spec.DeadlinesSeconds.Settle = 300
	}
	if len(spec.Name) > 63 || !dnsLabel.MatchString(spec.Name) {
		return PlanResult{}, fmt.Errorf("name must be a DNS label of 1-63 lowercase characters starting with a letter")
	}
	if spec.GuestNodes < 0 || spec.GuestNodes > MaxGuestNodes {
		return PlanResult{}, fmt.Errorf("guestNodes must be in [0,%d]", MaxGuestNodes)
	}
	if spec.Applications < 0 || spec.Applications > MaxApplications {
		return PlanResult{}, fmt.Errorf("applications must be in [0,%d]", MaxApplications)
	}
	if spec.GuestNodes == 0 && spec.Applications > 0 {
		return PlanResult{}, fmt.Errorf("applications must be zero when guestNodes is zero")
	}
	if spec.CPUTreatment != "quiet" && spec.CPUTreatment != "contended" {
		return PlanResult{}, fmt.Errorf("cpuTreatment must be quiet or contended")
	}
	if spec.ExpectedProfile == "" || spec.ExpectedProfile != profile.Name {
		return PlanResult{}, fmt.Errorf("expectedProfile must match profile name %q", profile.Name)
	}
	for _, deadline := range []struct {
		name    string
		seconds Seconds
	}{
		{"startup", spec.DeadlinesSeconds.Startup},
		{"seeding", spec.DeadlinesSeconds.Seeding},
		{"calibration", spec.DeadlinesSeconds.Calibration},
		{"settle", spec.DeadlinesSeconds.Settle},
		{"shutdown", spec.DeadlinesSeconds.Shutdown},
	} {
		if deadline.seconds <= 0 || deadline.seconds > 86400 {
			return PlanResult{}, fmt.Errorf("deadlinesSeconds.%s must be in [1,86400]", deadline.name)
		}
	}
	resolved, err := sizing.Resolve(profile)
	if err != nil {
		return PlanResult{}, fmt.Errorf("profile: %w", err)
	}
	rollouts, err := rolloutCount(spec.Applications, time.Duration(spec.SteadySeconds)*time.Second, time.Duration(spec.TurnoverPeriodSeconds)*time.Second)
	if err != nil {
		return PlanResult{}, err
	}
	// Bounds above keep all products, sums, and duration conversions in int64.
	result := PlanResult{
		Version: Version, Spec: spec, Sizing: resolved,
		Workload: Workload{
			ReplicasPerApplication: 5, ApplicationsPerNamespace: 20,
			Applications: spec.Applications, Namespaces: (spec.Applications + 19) / 20,
			Pods: spec.Applications * 5, Rollouts: rollouts,
		},
		Canary: Canary{
			Enabled: spec.GuestNodes > 0, BaselineCycles: canaryCycles,
			CadenceSeconds: Seconds(spec.CanaryCadenceSeconds), MaxConcurrent: 1, Multiplier: canaryMultiplier,
			AckBudgetSeconds: 30, ReadyBudgetSeconds: 120, CleanupBudgetSeconds: 120,
		},
		Phases: []Phase{
			{Name: "startup", DurationSeconds: spec.DeadlinesSeconds.Startup},
			{Name: "seeding", DurationSeconds: spec.DeadlinesSeconds.Seeding},
		},
	}
	if result.Canary.Enabled {
		// Baseline cycles are consecutive, not cadence-spaced. Ready includes
		// acknowledgement, so it must not be added a second time.
		result.Phases = append(result.Phases, Phase{Name: "baseline", DurationSeconds: canaryCycles * (result.Canary.ReadyBudgetSeconds + result.Canary.CleanupBudgetSeconds)})
	}
	if spec.CPUTreatment == "contended" {
		result.Phases = append(result.Phases, Phase{Name: "calibration", DurationSeconds: spec.DeadlinesSeconds.Calibration})
	}
	result.Phases = append(result.Phases,
		Phase{Name: "settle", DurationSeconds: spec.DeadlinesSeconds.Settle},
		Phase{Name: "steady", DurationSeconds: Seconds(spec.SteadySeconds)},
		Phase{Name: "recovery", DurationSeconds: Seconds(spec.RecoverySeconds)},
		Phase{Name: "shutdown", DurationSeconds: spec.DeadlinesSeconds.Shutdown},
	)
	for _, phase := range result.Phases {
		result.MaxDurationSeconds += phase.DurationSeconds
	}
	return result, nil
}

// Rollout is an intended pod-template change, not an attempted/completed action.
// Application is a zero-based index; Offset is relative to steady phase start.
type Rollout struct {
	Application int64         `json:"application"`
	Round       int           `json:"round"`
	Offset      time.Duration `json:"offsetNanoseconds"`
}

// Rollouts returns two hourly rounds, evenly staggered in integer nanoseconds.
// A local seeded permutation fixes application order, repeated in the second
// hour so each application turns over exactly hourly. No global RNG is used.
// At most 80,000 events are allocated. Zero applications returns an empty schedule.
// A future executor must retain overdue work as backlog, not overlap unfinished
// rollouts or interpret these intended offsets as proof of completion.
func Rollouts(applications int64, seed int64) ([]Rollout, error) {
	return RolloutsForPeriod(applications, seed, 2*time.Hour, time.Hour)
}

// rolloutCount validates bounds before multiplication or schedule allocation.
func rolloutCount(applications int64, duration, period time.Duration) (int64, error) {
	if applications < 0 || applications > MaxApplications {
		return 0, fmt.Errorf("applications must be in [0,%d]", MaxApplications)
	}
	if duration <= 0 || duration > 2*time.Hour || period < time.Second || period > 24*time.Hour {
		return 0, fmt.Errorf("require duration in (0,2h] and period in [1s,24h]")
	}
	// ceil(applications*duration/period) counts offsets strictly before the end,
	// including a partial final round. Bounded nanosecond products fit int64.
	count := (applications*int64(duration) + int64(period) - 1) / int64(period)
	if count > MaxScheduledRollouts {
		return 0, fmt.Errorf("schedule exceeds %d rollouts", MaxScheduledRollouts)
	}
	return count, nil
}

// RolloutsForPeriod repeats one seeded application permutation every period,
// retaining only offsets strictly before duration. It describes rollout intents,
// not client QPS or achieved throughput; overdue intents must remain backlog.
// Duration must be in (0,2h], period in [1s,24h], and the schedule at most
// MaxScheduledRollouts entries. Zero applications returns an empty schedule.
func RolloutsForPeriod(applications int64, seed int64, duration time.Duration, period time.Duration) ([]Rollout, error) {
	count, err := rolloutCount(applications, duration, period)
	if err != nil {
		return nil, err
	}
	events := make([]Rollout, 0, int(count))
	if count == 0 {
		return events, nil
	}
	order := rand.New(rand.NewSource(seed)).Perm(int(applications))
	for i := int64(0); i < count; i++ {
		round, slot := i/applications, i%applications
		events = append(events, Rollout{
			Application: int64(order[slot]), Round: int(round) + 1,
			Offset: time.Duration(round)*period + time.Duration(slot)*period/time.Duration(applications),
		})
	}
	return events, nil
}

// CanarySample contains successful observations from one consecutive baseline
// cycle. Ack is API acceptance since scale-up intent. Ready is readiness at the
// intended generation since that SAME intent, including Ack, not after Ack.
// Cleanup is time from scale-down intent until zero replicas and no canary pods.
// Failed/missing observations must not be passed off as successful samples.
type CanarySample struct {
	Ack     time.Duration
	Ready   time.Duration
	Cleanup time.Duration
}

type CanaryDeadlines struct {
	Ack     time.Duration
	Ready   time.Duration
	Cleanup time.Duration
}

// CalculateCanaryDeadlines requires exactly five successful, positive samples
// within absolute budgets. It returns min(3*maxObserved, budget) per stage.
func CalculateCanaryDeadlines(samples []CanarySample) (CanaryDeadlines, error) {
	if len(samples) != canaryCycles {
		return CanaryDeadlines{}, fmt.Errorf("need exactly %d successful baseline cycles", canaryCycles)
	}
	var result CanaryDeadlines
	for i, sample := range samples {
		if sample.Ack <= 0 || sample.Ready <= 0 || sample.Cleanup <= 0 ||
			sample.Ack > 30*time.Second || sample.Ready > 120*time.Second || sample.Cleanup > 120*time.Second ||
			sample.Ack > sample.Ready {
			return CanaryDeadlines{}, fmt.Errorf("sample %d: require positive ack <= ready and ack/ready/cleanup within 30s/120s/120s budgets", i)
		}
		result.Ack = max(result.Ack, sample.Ack)
		result.Ready = max(result.Ready, sample.Ready)
		result.Cleanup = max(result.Cleanup, sample.Cleanup)
	}
	// Validation bounds maxima before multiplication, even for hostile inputs.
	result.Ack = min(canaryMultiplier*result.Ack, 30*time.Second)
	result.Ready = min(canaryMultiplier*result.Ready, 120*time.Second)
	result.Cleanup = min(canaryMultiplier*result.Cleanup, 120*time.Second)
	return result, nil
}

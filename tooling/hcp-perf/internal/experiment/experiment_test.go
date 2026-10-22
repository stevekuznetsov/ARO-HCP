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

package experiment

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

func inputs() (Spec, sizing.Profile) {
	return Spec{
			Version: Version, Name: "small-test", ExpectedProfile: "small",
			GuestNodes: 3, Applications: 12, Seed: 42, CPUTreatment: "quiet",
			DeadlinesSeconds: Deadlines{Startup: 3600, Seeding: 1800, Calibration: 600, Settle: 300, Shutdown: 120},
		}, sizing.Profile{
			Version: sizing.Version, Name: "small",
			Anchor:      sizing.Budget{CPUMilli: 250, MemoryMiB: 1024},
			Rounding:    sizing.Budget{CPUMilli: 1, MemoryMiB: 1},
			SafetyLimit: sizing.Ratio{Numerator: 3, Denominator: 1}, GoMemLimitPercent: 80,
			Containers: []sizing.Container{{
				Identity:    sizing.Identity{Kind: "Deployment", Workload: "kube-apiserver", Container: "kube-apiserver"},
				CPURatio:    sizing.Ratio{Numerator: 1, Denominator: 1},
				MemoryRatio: sizing.Ratio{Numerator: 1, Denominator: 1},
			}},
		}
}

func TestPlan(t *testing.T) {
	for _, test := range []struct {
		nodes, applications, namespaces, pods int64
	}{
		{0, 0, 0, 0},
		{3, 0, 0, 0},
		{3, 1, 1, 5},
		{3, 12, 1, 60},
		{3, 20, 1, 100},
		{3, 21, 2, 105},
		{3, 40, 2, 200},
		{3, 41, 3, 205},
		{3, 120, 6, 600},
		{1, 12, 1, 60},
		{MaxGuestNodes, 12, 1, 60},
		{3, MaxApplications, 2000, 200000},
	} {
		for _, treatment := range []string{"quiet", "contended"} {
			t.Run(strconv.FormatInt(test.nodes, 10)+"/"+strconv.FormatInt(test.applications, 10)+"/"+treatment, func(t *testing.T) {
				spec, profile := inputs()
				spec.GuestNodes, spec.Applications, spec.CPUTreatment = test.nodes, test.applications, treatment
				before, err := json.Marshal(profile)
				if err != nil {
					t.Fatal(err)
				}
				got, err := Plan(spec, profile)
				if err != nil {
					t.Fatal(err)
				}
				wantWorkload := Workload{
					ReplicasPerApplication: 5, ApplicationsPerNamespace: 20,
					Applications: test.applications, Namespaces: test.namespaces, Pods: test.pods, Rollouts: test.applications * 2,
				}
				resolvedSpec := spec
				resolvedSpec.Mode, resolvedSpec.SteadySeconds = "soak", 7200
				resolvedSpec.TurnoverPeriodSeconds = 3600
				resolvedSpec.CanaryCadenceSeconds, resolvedSpec.RecoverySeconds = 300, 600
				if got.Workload != wantWorkload || got.Version != Version || got.Spec != resolvedSpec {
					t.Fatalf("unexpected plan: %+v", got)
				}
				wantCanary := Canary{
					Enabled: test.nodes > 0, BaselineCycles: 5, CadenceSeconds: 300, MaxConcurrent: 1,
					Multiplier: 3, AckBudgetSeconds: 30, ReadyBudgetSeconds: 120, CleanupBudgetSeconds: 120,
				}
				if got.Canary != wantCanary {
					t.Fatalf("canary = %+v, want %+v", got.Canary, wantCanary)
				}
				wantPhases := []Phase{{"startup", 3600}, {"seeding", 1800}}
				wantTotal := Seconds(13620)
				if test.nodes > 0 {
					wantPhases = append(wantPhases, Phase{"baseline", 1200})
					wantTotal += 1200
				}
				if treatment == "contended" {
					wantPhases = append(wantPhases, Phase{"calibration", 600})
					wantTotal += 600
				}
				wantPhases = append(wantPhases, Phase{"settle", 300}, Phase{"steady", 7200}, Phase{"recovery", 600}, Phase{"shutdown", 120})
				if !reflect.DeepEqual(got.Phases, wantPhases) || got.MaxDurationSeconds != wantTotal {
					t.Fatalf("phases = %+v, total = %d; want %+v, %d", got.Phases, got.MaxDurationSeconds, wantPhases, wantTotal)
				}
				resolved, err := sizing.Resolve(profile)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.Sizing, resolved) {
					t.Fatalf("sizing = %+v, want %+v", got.Sizing, resolved)
				}
				again, err := Plan(spec, profile)
				if err != nil || !reflect.DeepEqual(got, again) {
					t.Fatalf("planning is not deterministic: %+v, %v", again, err)
				}
				after, err := json.Marshal(profile)
				if err != nil || string(before) != string(after) {
					t.Fatalf("profile mutated: %s -> %s, %v", before, after, err)
				}
			})
		}
	}
}

func TestPlanTiming(t *testing.T) {
	for _, mode := range []string{"", "soak", "validation", "invalid"} {
		for _, steady := range []int64{math.MinInt64, -1, 0, 29, 30, 300, 301, 1800, 1801, 7199, 7200, 7201, math.MaxInt64} {
			for _, recovery := range []int64{-1, 0, 1, 30, 120, 121, 600, 601, math.MaxInt64} {
				spec, profile := inputs()
				spec.Mode, spec.SteadySeconds, spec.RecoverySeconds = mode, steady, recovery
				spec.DeadlinesSeconds.Settle = 30
				before := spec
				valid := mode != "invalid" && recovery >= 0 && recovery <= 600 && (steady == 0 || steady == 7200)
				if mode == "validation" {
					valid = recovery >= 0 && recovery <= 120 && (steady == 0 || (steady >= 30 && steady <= 1800))
				}
				got, err := Plan(spec, profile)
				if (err == nil) != valid || spec != before {
					t.Fatalf("mode=%q steady=%d recovery=%d: %+v, %v; valid=%t", mode, steady, recovery, got, err, valid)
				}
				if !valid {
					if !reflect.DeepEqual(got, PlanResult{}) {
						t.Fatal("invalid timing returned a partial plan")
					}
					continue
				}
				wantSteady, wantRecovery, wantCadence := int64(7200), int64(600), Seconds(300)
				if mode == "validation" {
					wantSteady, wantRecovery, wantCadence = 300, 30, 30
				}
				if steady != 0 {
					wantSteady = steady
				}
				if recovery != 0 {
					wantRecovery = recovery
				}
				if got.Spec.SteadySeconds != wantSteady || got.Spec.RecoverySeconds != wantRecovery || got.Canary.CadenceSeconds != wantCadence || got.Canary.BaselineCycles != 5 {
					t.Fatalf("unexpected resolved timing: %+v", got)
				}
				var total Seconds
				for _, phase := range got.Phases {
					total += phase.DurationSeconds
				}
				if total != got.MaxDurationSeconds || got.Phases[len(got.Phases)-3].DurationSeconds != Seconds(wantSteady) || got.Phases[len(got.Phases)-2].DurationSeconds != Seconds(wantRecovery) {
					t.Fatalf("incorrect phase bounds: %+v", got)
				}
				data, err := json.Marshal(got.Spec)
				var persisted Spec
				if err != nil || json.Unmarshal(data, &persisted) != nil || persisted != got.Spec || persisted.Mode == "" {
					t.Fatalf("resolved timing not persisted: %s, %v", data, err)
				}
			}
		}
	}
	for _, cadence := range []int64{-1, 0, 4, 5, 30, 300, 301, math.MaxInt64} {
		spec, profile := inputs()
		spec.Mode, spec.CanaryCadenceSeconds = "validation", cadence
		spec.DeadlinesSeconds.Settle = 0
		got, err := Plan(spec, profile)
		if (err == nil) != (cadence == 0 || cadence >= 5 && cadence <= 300) {
			t.Fatalf("cadence %d: %+v, %v", cadence, got, err)
		}
		if err == nil && got.Spec.DeadlinesSeconds.Settle != 300 {
			t.Fatal("missing settle default")
		}
	}
}

func TestShortPlanRolloutCounts(t *testing.T) {
	for _, apps := range []int64{0, 1, 12, 30, 120, MaxApplications} {
		for _, duration := range []int64{30, 300, 301, 1800} {
			spec, profile := inputs()
			spec.Mode, spec.SteadySeconds, spec.Applications = "validation", duration, apps
			got, err := Plan(spec, profile)
			if err != nil {
				t.Fatal(err)
			}
			events, err := Rollouts(apps, spec.Seed)
			if err != nil {
				t.Fatal(err)
			}
			var count int64
			for _, event := range events {
				if event.Offset < time.Duration(duration)*time.Second {
					count++
				}
			}
			if got.Workload.Rollouts != count {
				t.Fatalf("apps=%d duration=%d: count=%d want=%d", apps, duration, got.Workload.Rollouts, count)
			}
		}
	}
}

func TestPlanInvalid(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Spec, *sizing.Profile)
	}{
		{"version", func(s *Spec, _ *sizing.Profile) { s.Version = "v2" }},
		{"negative nodes", func(s *Spec, _ *sizing.Profile) { s.GuestNodes = -1 }},
		{"too many nodes", func(s *Spec, _ *sizing.Profile) { s.GuestNodes = MaxGuestNodes + 1 }},
		{"overflow nodes", func(s *Spec, _ *sizing.Profile) { s.GuestNodes = math.MaxInt64 }},
		{"applications without nodes", func(s *Spec, _ *sizing.Profile) { s.GuestNodes = 0 }},
		{"negative applications", func(s *Spec, _ *sizing.Profile) { s.Applications = -1 }},
		{"too many applications", func(s *Spec, _ *sizing.Profile) { s.Applications = MaxApplications + 1 }},
		{"overflow applications", func(s *Spec, _ *sizing.Profile) { s.Applications = math.MaxInt64 }},
		{"cpu treatment", func(s *Spec, _ *sizing.Profile) { s.CPUTreatment = "Quiet" }},
		{"missing cpu treatment", func(s *Spec, _ *sizing.Profile) { s.CPUTreatment = "" }},
		{"wrong profile", func(s *Spec, _ *sizing.Profile) { s.ExpectedProfile = "other" }},
		{"missing profile", func(s *Spec, _ *sizing.Profile) { s.ExpectedProfile = "" }},
		{"invalid profile", func(_ *Spec, p *sizing.Profile) { p.Anchor.CPUMilli = 0 }},
		{"overflow profile memory", func(_ *Spec, p *sizing.Profile) { p.Anchor.MemoryMiB = math.MaxInt64 }},
		{"missing startup", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Startup = 0 }},
		{"negative seeding", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Seeding = -1 }},
		{"unbounded settle", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Settle = 86401 }},
		{"overflow duration", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Startup = math.MaxInt64 }},
		{"missing calibration", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Calibration = 0 }},
		{"negative calibration", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Calibration = -1 }},
		{"unbounded calibration", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Calibration = 86401 }},
		{"overflow calibration", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Calibration = math.MaxInt64 }},
		{"missing shutdown", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Shutdown = 0 }},
		{"negative shutdown", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Shutdown = -1 }},
		{"unbounded shutdown", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Shutdown = 86401 }},
		{"overflow shutdown", func(s *Spec, _ *sizing.Profile) { s.DeadlinesSeconds.Shutdown = math.MaxInt64 }},
	}
	for _, name := range []string{"", "a.b", "a_b", "A", "-a", "a-", "0app", "a/b", "a\n", strings.Repeat("a", 64)} {
		tests = append(tests, struct {
			name   string
			change func(*Spec, *sizing.Profile)
		}{"name " + strconv.Quote(name), func(s *Spec, _ *sizing.Profile) { s.Name = name }})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, profile := inputs()
			test.change(&spec, &profile)
			got, err := Plan(spec, profile)
			if err == nil || !reflect.DeepEqual(got, PlanResult{}) {
				t.Fatalf("Plan = %+v, %v; want empty result and error", got, err)
			}
		})
	}
}

func TestPlanInputBoundaries(t *testing.T) {
	spec, profile := inputs()
	for _, seed := range []int64{math.MinInt64, 0, math.MaxInt64} {
		for _, name := range []string{"a", "a-0", strings.Repeat("a", 63)} {
			spec.Seed, spec.Name = seed, name
			spec.DeadlinesSeconds = Deadlines{Startup: 1, Seeding: 86400, Calibration: 86400, Settle: 86400, Shutdown: 1}
			if _, err := Plan(spec, profile); err != nil {
				t.Fatalf("valid boundary rejected: %v", err)
			}
		}
	}
}

func TestRollouts(t *testing.T) {
	for _, applications := range []int64{0, 1, 12, 21, 40, 41, 120, MaxApplications} {
		for _, seed := range []int64{math.MinInt64, 0, 42, math.MaxInt64} {
			events, err := Rollouts(applications, seed)
			if err != nil || int64(len(events)) != applications*2 {
				t.Fatalf("Rollouts(%d, %d) length = %d, %v", applications, seed, len(events), err)
			}
			again, err := Rollouts(applications, seed)
			if err != nil || !reflect.DeepEqual(events, again) {
				t.Fatalf("schedule is not deterministic: %v", err)
			}
			seen := make(map[[2]int64]bool)
			for i, event := range events {
				key := [2]int64{int64(event.Round), event.Application}
				if seen[key] || event.Application < 0 || event.Application >= applications {
					t.Fatalf("duplicate or invalid application: %+v", event)
				}
				seen[key] = true
				if event.Round != i/int(applications)+1 || event.Offset < time.Duration(event.Round-1)*time.Hour || event.Offset >= time.Duration(event.Round)*time.Hour {
					t.Fatalf("event outside its hourly round: %+v", event)
				}
				if i > 0 && event.Offset <= events[i-1].Offset {
					t.Fatalf("schedule not strictly staggered: %+v after %+v", event, events[i-1])
				}
				wantOffset := time.Duration(i) * time.Hour / time.Duration(applications)
				if event.Offset != wantOffset {
					t.Fatalf("offset = %v, want %v", event.Offset, wantOffset)
				}
				if event.Round == 2 {
					previous := events[i-int(applications)]
					if event.Application != previous.Application || event.Offset-previous.Offset != time.Hour {
						t.Fatalf("application not exactly hourly: %+v -> %+v", previous, event)
					}
				}
			}
		}
	}
	a, _ := Rollouts(5, 42)
	b, _ := Rollouts(5, 43)
	if reflect.DeepEqual(a, b) {
		t.Fatal("different seeds yielded identical application order")
	}
	for _, applications := range []int64{-1, MaxApplications + 1, math.MaxInt64} {
		if got, err := Rollouts(applications, 0); err == nil || got != nil {
			t.Fatalf("invalid applications %d: got %v, %v", applications, got, err)
		}
	}
}

func TestRolloutsForPeriod(t *testing.T) {
	for _, test := range []struct {
		period time.Duration
		count  int
		gap    time.Duration
	}{
		{time.Hour, 10, 30 * time.Second},
		{30 * time.Minute, 20, 15 * time.Second},
		{15 * time.Minute, 40, 7500 * time.Millisecond},
		{5 * time.Minute, 120, 2500 * time.Millisecond},
	} {
		t.Run(test.period.String(), func(t *testing.T) {
			events, err := RolloutsForPeriod(120, 42, 5*time.Minute, test.period)
			if err != nil || len(events) != test.count {
				t.Fatalf("count=%d want=%d: %v", len(events), test.count, err)
			}
			legacy, _ := Rollouts(120, 42)
			for i, event := range events {
				if event.Offset != time.Duration(i)*test.gap || event.Application != legacy[i].Application || event.Round != 1 {
					t.Fatalf("wrong offset or permutation: %+v", event)
				}
			}
		})
	}
	for _, apps := range []int64{0, 1, 7, 120, MaxApplications} {
		for _, period := range []time.Duration{time.Second, 7 * time.Second, 300 * time.Second, time.Hour, 24 * time.Hour} {
			for _, duration := range []time.Duration{time.Nanosecond, time.Second, 301*time.Second + 1, 2 * time.Hour} {
				want := (apps*int64(duration) + int64(period) - 1) / int64(period)
				events, err := RolloutsForPeriod(apps, 42, duration, period)
				if want > MaxScheduledRollouts {
					if err == nil || events != nil {
						t.Fatal("oversized schedule was allocated")
					}
					continue
				}
				if err != nil || int64(len(events)) != want {
					t.Fatalf("apps=%d duration=%s period=%s: count=%d want=%d err=%v", apps, duration, period, len(events), want, err)
				}
				again, _ := RolloutsForPeriod(apps, 42, duration, period)
				if !reflect.DeepEqual(events, again) {
					t.Fatal("nondeterministic schedule")
				}
				for i, event := range events {
					round, slot := int64(i)/apps, int64(i)%apps
					if event.Round != int(round)+1 || event.Offset != time.Duration(round)*period+time.Duration(slot)*period/time.Duration(apps) || event.Offset >= duration || (i > 0 && event.Offset <= events[i-1].Offset) {
						t.Fatalf("invalid offset: %+v", event)
					}
					if int64(i) >= apps && (event.Application != events[int64(i)-apps].Application || event.Offset-events[int64(i)-apps].Offset != period) {
						t.Fatalf("permutation changed between periods: %+v", event)
					}
				}
			}
		}
		legacy, _ := Rollouts(apps, 42)
		hourly, _ := RolloutsForPeriod(apps, 42, 2*time.Hour, time.Hour)
		if !reflect.DeepEqual(legacy, hourly) {
			t.Fatal("legacy hourly schedule changed")
		}
	}
}

func TestRolloutsForPeriodInvalid(t *testing.T) {
	for _, test := range []struct {
		apps             int64
		duration, period time.Duration
	}{
		{-1, time.Hour, time.Hour},
		{math.MaxInt64, time.Hour, time.Hour},
		{MaxApplications + 1, time.Hour, time.Hour},
		{1, 0, time.Hour},
		{1, -1, time.Hour},
		{1, 2*time.Hour + 1, time.Hour},
		{1, math.MaxInt64, time.Hour},
		{1, time.Hour, -1},
		{1, time.Hour, 0},
		{1, time.Hour, time.Second - 1},
		{1, time.Hour, 24*time.Hour + 1},
		{1, time.Hour, math.MaxInt64},
		{MaxApplications, 2 * time.Hour, time.Hour - 1},
	} {
		if events, err := RolloutsForPeriod(test.apps, 42, test.duration, test.period); err == nil || events != nil {
			t.Fatalf("invalid inputs accepted: %+v, count=%d, err=%v", test, len(events), err)
		}
	}
}

func TestPlanTurnoverPeriod(t *testing.T) {
	for _, period := range []int64{math.MinInt64, -1, 0, 1, 7, 300, 900, 1800, 3600, 86400, 86401, math.MaxInt64} {
		for _, apps := range []int64{0, 120, MaxApplications} {
			spec, profile := inputs()
			spec.Mode, spec.SteadySeconds, spec.Applications, spec.TurnoverPeriodSeconds = "validation", 300, apps, period
			before := spec
			got, err := Plan(spec, profile)
			resolvedPeriod := period
			if resolvedPeriod == 0 {
				resolvedPeriod = 3600
			}
			valid := resolvedPeriod >= 1 && resolvedPeriod <= 86400
			if valid {
				valid = (apps*300+resolvedPeriod-1)/resolvedPeriod <= MaxScheduledRollouts
			}
			if (err == nil) != valid || spec != before {
				t.Fatalf("apps=%d period=%d: %+v %v", apps, period, got, err)
			}
			if !valid {
				if !reflect.DeepEqual(got, PlanResult{}) {
					t.Fatal("invalid period returned partial plan")
				}
				continue
			}
			schedule, err := RolloutsForPeriod(apps, spec.Seed, 300*time.Second, time.Duration(resolvedPeriod)*time.Second)
			if err != nil || got.Spec.TurnoverPeriodSeconds != resolvedPeriod || got.Workload.Rollouts != int64(len(schedule)) || got.Workload.Pods != apps*5 || got.Workload.Applications != apps {
				t.Fatalf("period changed population or count: %+v %v", got, err)
			}
			data, err := json.Marshal(got.Spec)
			var persisted Spec
			if err != nil || json.Unmarshal(data, &persisted) != nil || persisted.TurnoverPeriodSeconds != resolvedPeriod {
				t.Fatalf("resolved period not persisted: %s %v", data, err)
			}
		}
	}
}

func TestCalculateCanaryDeadlines(t *testing.T) {
	samples := []CanarySample{
		{time.Second, 2 * time.Second, 3 * time.Second},
		{2 * time.Second, 5 * time.Second, 4 * time.Second},
		{time.Second, 3 * time.Second, 6 * time.Second},
		{time.Second, 2 * time.Second, 3 * time.Second},
		{time.Second, 2 * time.Second, 3 * time.Second},
	}
	got, err := CalculateCanaryDeadlines(samples)
	want := CanaryDeadlines{6 * time.Second, 15 * time.Second, 18 * time.Second}
	if err != nil || got != want {
		t.Fatalf("deadlines = %+v, %v; want %+v", got, err, want)
	}
	samples[0] = CanarySample{30 * time.Second, 120 * time.Second, 120 * time.Second}
	got, err = CalculateCanaryDeadlines(samples)
	want = CanaryDeadlines{30 * time.Second, 120 * time.Second, 120 * time.Second}
	if err != nil || got != want {
		t.Fatalf("capped deadlines = %+v, %v; want %+v", got, err, want)
	}
	for i := range samples {
		samples[i] = CanarySample{time.Nanosecond, time.Nanosecond, time.Nanosecond}
	}
	got, err = CalculateCanaryDeadlines(samples)
	if err != nil || got != (CanaryDeadlines{3, 3, 3}) {
		t.Fatalf("subsecond samples lost precision: %+v, %v", got, err)
	}
}

func TestCalculateCanaryDeadlinesInvalid(t *testing.T) {
	valid := CanarySample{time.Second, 2 * time.Second, time.Second}
	for _, count := range []int{0, 1, 4, 6} {
		samples := make([]CanarySample, count)
		for i := range samples {
			samples[i] = valid
		}
		if got, err := CalculateCanaryDeadlines(samples); err == nil || got != (CanaryDeadlines{}) {
			t.Fatalf("accepted %d samples: %+v, %v", count, got, err)
		}
	}
	for stage, budget := range []time.Duration{30 * time.Second, 120 * time.Second, 120 * time.Second} {
		for _, invalid := range []time.Duration{-1, 0, budget + 1, math.MaxInt64} {
			sample := valid
			switch stage {
			case 0:
				sample.Ack = invalid
			case 1:
				sample.Ready = invalid
			case 2:
				sample.Cleanup = invalid
			}
			samples := []CanarySample{valid, valid, valid, valid, sample}
			if got, err := CalculateCanaryDeadlines(samples); err == nil || got != (CanaryDeadlines{}) {
				t.Fatalf("accepted invalid sample %+v: %+v, %v", sample, got, err)
			}
		}
	}
	if _, err := CalculateCanaryDeadlines([]CanarySample{valid, valid, valid, valid, {3 * time.Second, 2 * time.Second, time.Second}}); err == nil {
		t.Fatal("accepted acknowledgement after readiness")
	}
}

func TestExampleCase(t *testing.T) {
	for _, name := range []string{"case", "validation-case"} {
		t.Run(name, func(t *testing.T) {
			var spec Spec
			var profile sizing.Profile
			for _, fixture := range []struct {
				path   string
				target any
			}{
				{"../../examples/" + name + ".json", &spec},
				{"../../examples/small.json", &profile},
			} {
				data, err := os.ReadFile(fixture.path)
				if err != nil {
					t.Fatal(err)
				}
				decoder := json.NewDecoder(strings.NewReader(string(data)))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(fixture.target); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Plan(spec, profile); err != nil {
				t.Fatalf("example case/profile rejected: %v", err)
			}
			if spec.GuestNodes != 3 || spec.Applications != 0 || spec.ExpectedProfile != "small" {
				t.Fatalf("example must be the three-node, zero-application small baseline: %+v", spec)
			}
			data, err := json.Marshal(spec)
			if err != nil || !strings.Contains(string(data), `"applications":0`) {
				t.Fatalf("explicit zero applications must be preserved in JSON: %s, %v", data, err)
			}
			if name == "validation-case" && (spec.Mode != "validation" || spec.SteadySeconds != 300 || spec.RecoverySeconds != 30 || spec.CanaryCadenceSeconds != 30 || spec.DeadlinesSeconds.Settle != 30) {
				t.Fatalf("unexpected validation example: %+v", spec)
			}
		})
	}
}

// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scan

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

func inputs(t *testing.T) (Spec, sizing.Profile) {
	t.Helper()
	data, err := os.ReadFile("../../examples/small.json")
	if err != nil {
		t.Fatal(err)
	}
	var profile sizing.Profile
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	return Spec{Version: Version, Name: "population", GuestNodes: 3,
		Applications: []int64{0, 3, 12, 30, 60, 90, 120}, Seed: 42, CPUTreatment: "quiet"}, profile
}

func TestPlan(t *testing.T) {
	for _, mode := range []string{"", "validation", "soak"} {
		for _, treatment := range []string{"quiet", "contended"} {
			t.Run(mode+"/"+treatment, func(t *testing.T) {
				spec, profile := inputs(t)
				spec.Mode, spec.CPUTreatment = mode, treatment
				before, _ := json.Marshal(profile)
				got, err := Plan(spec, profile)
				if err != nil {
					t.Fatal(err)
				}
				again, err := Plan(spec, profile)
				if err != nil || !reflect.DeepEqual(got, again) {
					t.Fatal("plan is not deterministic", err)
				}
				if got.Status != "planned" || got.Version != Version || got.CPUFillPercent != 90 ||
					got.ProfileSHA256 != fmt.Sprintf("%x", sha256.Sum256(before)) || len(got.Cases) != 7 {
					t.Fatalf("unexpected metadata: %+v", got)
				}
				steady, cadence, recovery, settle := int64(300), int64(30), int64(30), experiment.Seconds(30)
				if mode == "soak" {
					steady, cadence, recovery, settle = 7200, 300, 600, 300
				} else if got.Spec.Mode != "validation" {
					t.Fatal("validation must be the default")
				}
				for i, planned := range got.Cases {
					input := planned.Spec
					if input.Name != fmt.Sprintf("population-%05d", i) || input.Applications != spec.Applications[i] ||
						input.GuestNodes != 3 || input.Seed != 42 || input.CPUTreatment != treatment ||
						input.ExpectedProfile != profile.Name || input.SteadySeconds != steady ||
						input.CanaryCadenceSeconds != cadence || input.RecoverySeconds != recovery ||
						input.DeadlinesSeconds != (experiment.Deadlines{Startup: 3600, Seeding: 1800, Calibration: 600, Settle: settle, Shutdown: 120}) {
						t.Fatalf("unexpected case inputs: %+v", input)
					}
					want, err := experiment.Plan(input, profile)
					if err != nil || !reflect.DeepEqual(planned, want) || !reflect.DeepEqual(planned.Sizing, got.Cases[0].Sizing) {
						t.Fatalf("case %d diverged from experiment.Plan or fixed footprint: %v", i, err)
					}
					if planned.Workload.Pods != spec.Applications[i]*5 || planned.Workload.Rollouts != (spec.Applications[i]*steady+3599)/3600 {
						t.Fatalf("population or nominal hourly turnover changed: %+v", planned.Workload)
					}
				}
				after, _ := json.Marshal(profile)
				if string(before) != string(after) || spec.Mode != mode {
					t.Fatal("planner mutated inputs")
				}
			})
		}
	}
}

func TestInvalidPlan(t *testing.T) {
	for name, change := range map[string]func(*Spec){
		"version":               func(s *Spec) { s.Version = "v2" },
		"empty name":            func(s *Spec) { s.Name = "" },
		"invalid name":          func(s *Spec) { s.Name = "Upper-case" },
		"long name":             func(s *Spec) { s.Name = strings.Repeat("a", 58) },
		"negative nodes":        func(s *Spec) { s.GuestNodes = -1 },
		"node overflow":         func(s *Spec) { s.GuestNodes = math.MaxInt64 },
		"zero nodes with apps":  func(s *Spec) { s.GuestNodes = 0 },
		"empty population":      func(s *Spec) { s.Applications = nil },
		"nonzero first":         func(s *Spec) { s.Applications = []int64{1, 3} },
		"negative population":   func(s *Spec) { s.Applications = []int64{0, -1} },
		"duplicate":             func(s *Spec) { s.Applications = []int64{0, 3, 3} },
		"descending":            func(s *Spec) { s.Applications = []int64{0, 12, 3} },
		"pod ceiling":           func(s *Spec) { s.Applications = []int64{0, 121} },
		"application limit":     func(s *Spec) { s.GuestNodes = 1000; s.Applications = []int64{0, 40001} },
		"application overflow":  func(s *Spec) { s.Applications = []int64{0, math.MaxInt64} },
		"mode":                  func(s *Spec) { s.Mode = "optimizer" },
		"negative period":       func(s *Spec) { s.TurnoverPeriodSeconds = -1 },
		"large period":          func(s *Spec) { s.TurnoverPeriodSeconds = 86401 },
		"overflow period":       func(s *Spec) { s.TurnoverPeriodSeconds = math.MaxInt64 },
		"schedule cap":          func(s *Spec) { s.Mode = "soak"; s.TurnoverPeriodSeconds = 1 },
		"missing treatment":     func(s *Spec) { s.CPUTreatment = "" },
		"invalid treatment":     func(s *Spec) { s.CPUTreatment = "auto" },
		"missing evidence path": func(s *Spec) { s.HarnessEvidence = &HarnessEvidence{SHA256: strings.Repeat("a", 64)} },
		"bad evidence digest":   func(s *Spec) { s.HarnessEvidence = &HarnessEvidence{Path: "unread", SHA256: strings.Repeat("z", 64)} },
		"short evidence digest": func(s *Spec) { s.HarnessEvidence = &HarnessEvidence{Path: "unread", SHA256: "ab"} },
	} {
		t.Run(name, func(t *testing.T) {
			spec, profile := inputs(t)
			change(&spec)
			got, err := Plan(spec, profile)
			if err == nil || !reflect.DeepEqual(got, Result{}) {
				t.Fatalf("expected error and empty result, got %+v, %v", got, err)
			}
		})
	}
	spec, profile := inputs(t)
	profile.Anchor.CPUMilli = 0
	if got, err := Plan(spec, profile); err == nil || !reflect.DeepEqual(got, Result{}) {
		t.Fatalf("invalid profile accepted: %+v, %v", got, err)
	}
}

func TestPlanFixedPeriod(t *testing.T) {
	for _, period := range []int64{0, 3600, 1800, 900, 600, 300, 86400} {
		spec, profile := inputs(t)
		spec.TurnoverPeriodSeconds = period
		got, err := Plan(spec, profile)
		if err != nil {
			t.Fatal(err)
		}
		if period == 0 {
			period = 3600
		}
		if got.Spec.TurnoverPeriodSeconds != period {
			t.Fatal("unresolved scan period")
		}
		for i, c := range got.Cases {
			if c.Spec.TurnoverPeriodSeconds != period || c.Spec.Applications != spec.Applications[i] || c.Workload.Rollouts != (spec.Applications[i]*300+period-1)/period || c.Canary.BaselineCycles != 5 || c.Canary.CadenceSeconds != 30 {
				t.Fatalf("period changed population or canaries: %+v", c)
			}
		}
	}
}

func TestBoundariesAndSnapshots(t *testing.T) {
	spec, profile := inputs(t)
	spec.Name = strings.Repeat("a", 57)
	spec.GuestNodes = 1000
	spec.Applications = []int64{0, 40000}
	spec.Seed = math.MaxInt64
	spec.HarnessEvidence = &HarnessEvidence{Path: "/does-not-exist", SHA256: strings.Repeat("a", 64)}
	got, err := Plan(spec, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cases[1].Spec.Name) != 63 || got.Cases[1].Spec.Seed != math.MaxInt64 || got.Status != "planned" {
		t.Fatalf("unexpected boundary plan: %+v", got)
	}
	spec.Applications[1] = 1
	spec.HarnessEvidence.Path = "changed"
	profile.Anchor.CPUMilli *= 2
	if got.Spec.Applications[1] != 40000 || got.Spec.HarnessEvidence.Path != "/does-not-exist" || got.Cases[0].Sizing.Anchor == profile.Anchor {
		t.Fatal("returned plan aliases input")
	}
	changed, err := Plan(spec, profile)
	if err != nil || changed.ProfileSHA256 == got.ProfileSHA256 {
		t.Fatal("profile change must change fingerprint", err)
	}
	spec.GuestNodes, spec.Applications = 0, []int64{0}
	if got, err := Plan(spec, profile); err != nil || got.Cases[0].Canary.Enabled {
		t.Fatal("zero-node baseline should omit worker canary", err)
	}
}

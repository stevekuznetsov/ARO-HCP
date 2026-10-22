// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package scan plans a population axis at one fixed footprint, without I/O,
// execution, cost optimization, or claims about runtime capacity.
package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

const Version = "v1"

var namePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// HarnessEvidence is an unverified reference, never authorization to execute.
type HarnessEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Spec struct {
	Version               string           `json:"version"`
	Name                  string           `json:"name"`
	GuestNodes            int64            `json:"guestNodes"`
	Applications          []int64          `json:"applications"`
	Seed                  int64            `json:"seed"`
	TurnoverPeriodSeconds int64            `json:"turnoverPeriodSeconds,omitempty"`
	Mode                  string           `json:"mode,omitempty"`
	CPUTreatment          string           `json:"cpuTreatment"`
	HarnessEvidence       *HarnessEvidence `json:"harnessEvidence,omitempty"`
}

type Result struct {
	Version       string `json:"version"`
	Status        string `json:"status"`
	Spec          Spec   `json:"spec"`
	ProfileSHA256 string `json:"profileSHA256"`
	// CPUFillPercent is a target for a subsequent adversary primitive, not a
	// control-plane configuration change; quiet cases do not apply it.
	CPUFillPercent int64                   `json:"cpuFillPercent"`
	Cases          []experiment.PlanResult `json:"cases"`
}

// Plan holds one turnover period fixed across the population axis (default 3600s).
// The same seed is held across cases; population changes the stagger schedule.
// ProfileSHA256 hashes encoding/json's serialization of the input profile, not
// file bytes. Errors return no partial plan, and returned inputs are snapshots.
func Plan(spec Spec, profile sizing.Profile) (Result, error) {
	if spec.Version != Version {
		return Result{}, fmt.Errorf("version must be %q", Version)
	}
	// Reserve six characters for the stable, zero-based case index suffix.
	if len(spec.Name) > 57 || !namePattern.MatchString(spec.Name) {
		return Result{}, fmt.Errorf("name must be a DNS label of 1-57 lowercase characters starting with a letter")
	}
	if spec.GuestNodes < 0 || spec.GuestNodes > experiment.MaxGuestNodes {
		return Result{}, fmt.Errorf("guestNodes must be in [0,%d]", experiment.MaxGuestNodes)
	}
	if len(spec.Applications) == 0 || spec.Applications[0] != 0 {
		return Result{}, fmt.Errorf("applications must start with zero")
	}
	for i, apps := range spec.Applications {
		if apps < 0 || apps > experiment.MaxApplications || (i > 0 && apps <= spec.Applications[i-1]) {
			return Result{}, fmt.Errorf("applications must be strictly increasing unique integers in [0,%d]", experiment.MaxApplications)
		}
		if apps*5 > spec.GuestNodes*200 {
			return Result{}, fmt.Errorf("applications[%d]: application pods exceed guestNodes * 200; runtime headroom still requires validation", i)
		}
	}
	if spec.Mode == "" {
		spec.Mode = "validation"
	}
	if spec.Mode != "validation" && spec.Mode != "soak" {
		return Result{}, fmt.Errorf("mode must be validation or soak")
	}
	if spec.TurnoverPeriodSeconds == 0 {
		spec.TurnoverPeriodSeconds = 3600
	}
	if spec.HarnessEvidence != nil {
		evidence := *spec.HarnessEvidence
		digest, err := hex.DecodeString(evidence.SHA256)
		if evidence.Path == "" || err != nil || len(digest) != sha256.Size {
			return Result{}, fmt.Errorf("harnessEvidence requires a path and a 64-character hexadecimal sha256")
		}
		spec.HarnessEvidence = &evidence
	}
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		return Result{}, fmt.Errorf("profile: %w", err)
	}
	spec.Applications = append([]int64(nil), spec.Applications...)
	result := Result{
		Version: Version, Status: "planned", Spec: spec,
		ProfileSHA256: fmt.Sprintf("%x", sha256.Sum256(profileJSON)), CPUFillPercent: 90,
		Cases: make([]experiment.PlanResult, 0, len(spec.Applications)),
	}
	for i, apps := range spec.Applications {
		input := experiment.Spec{
			Version: experiment.Version, Name: fmt.Sprintf("%s-%05d", spec.Name, i),
			ExpectedProfile: profile.Name, GuestNodes: spec.GuestNodes, Applications: apps,
			Seed: spec.Seed, Mode: spec.Mode, CPUTreatment: spec.CPUTreatment,
			TurnoverPeriodSeconds: spec.TurnoverPeriodSeconds,
			SteadySeconds:         7200, CanaryCadenceSeconds: 300, RecoverySeconds: 600,
			DeadlinesSeconds: experiment.Deadlines{Startup: 3600, Seeding: 1800, Calibration: 600, Settle: 300, Shutdown: 120},
		}
		if spec.Mode == "validation" {
			input.SteadySeconds, input.CanaryCadenceSeconds, input.RecoverySeconds = 300, 30, 30
			input.DeadlinesSeconds.Settle = 30
		}
		planned, err := experiment.Plan(input, profile)
		if err != nil {
			return Result{}, fmt.Errorf("case %s: %w", input.Name, err)
		}
		result.Cases = append(result.Cases, planned)
	}
	return result, nil
}

// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/budget"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/iteration"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

func runBudget(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "allocate" && args[0] != "check") {
		return fmt.Errorf("usage: budget allocate|check --input FILE --output DIR [--prior-plan FILE --name NAME --kcm-cpu-milli N --catalog-cpu-milli N]")
	}
	f := flag.NewFlagSet("budget "+args[0], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	input := f.String("input", "", "self-contained budget input")
	output := f.String("output", "", "new output directory (existing for check)")
	prior := f.String("prior-plan", "", "frozen prior iteration spec; rebase old requests and preserve memory")
	name := f.String("name", "", "next candidate name")
	kcm := f.Int64("kcm-cpu-milli", 0, "protected KCM CPU request")
	catalog := f.Int64("catalog-cpu-milli", 0, "protected CPU request for each of three catalog registries")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *input == "" || *output == "" {
		return fmt.Errorf("input and output required; no positional arguments")
	}
	raw, err := os.ReadFile(*input)
	if err != nil {
		return err
	}
	s, err := budget.Decode(raw)
	if err != nil {
		return err
	}
	extras := map[string][]byte{}
	var plan iteration.Spec
	if *prior != "" {
		if *name == "" || *kcm <= 0 || *catalog <= 0 {
			return fmt.Errorf("prior-plan requires name and explicit positive CPU targets")
		}
		priorRaw, err := os.ReadFile(*prior)
		if err != nil {
			return err
		}
		if err := decodeFile(*prior, &plan); err != nil {
			return err
		}
		confirmed, err := os.ReadFile(*prior)
		if err != nil {
			return err
		}
		if budget.Hash(confirmed) != budget.Hash(priorRaw) {
			return fmt.Errorf("prior plan changed during preparation")
		}
		var desired json.RawMessage
		for _, action := range plan.Actions {
			if action.Candidate != nil {
				if desired != nil {
					return fmt.Errorf("expected one prior candidate")
				}
				desired = action.Candidate.DesiredSpec
			}
		}
		var wire struct {
			Sizes []struct {
				Effects struct {
					Policy struct {
						CPULimitEqualsRequest bool  `json:"cpuLimitEqualsRequest"`
						MemoryLimitPercent    int64 `json:"memoryLimitPercent"`
						GoMemoryLimitPercent  int64 `json:"goMemoryLimitPercent"`
						Containers            []struct {
							Workload             string            `json:"workload"`
							Container            string            `json:"container"`
							Requests             sizing.Quantities `json:"requests"`
							GoMemoryLimitPercent *int64            `json:"goMemoryLimitPercent"`
						} `json:"containers"`
					} `json:"containerResourcePolicy"`
				} `json:"effects"`
			} `json:"sizes"`
		}
		if err := json.Unmarshal(desired, &wire); err != nil {
			return err
		}
		if len(wire.Sizes) != 1 {
			return fmt.Errorf("expected one prior size")
		}
		policy := wire.Sizes[0].Effects.Policy
		if !policy.CPULimitEqualsRequest || policy.MemoryLimitPercent != 110 || policy.GoMemoryLimitPercent != s.GoMemLimitPercent {
			return fmt.Errorf("prior runtime policy differs from budget model")
		}
		requests := map[[2]string]sizing.Budget{}
		percentages := map[[2]string]int64{}
		for _, c := range wire.Sizes[0].Effects.Policy.Containers {
			cpu, e1 := strconv.ParseInt(strings.TrimSuffix(c.Requests.CPU, "m"), 10, 64)
			mem, e2 := strconv.ParseInt(strings.TrimSuffix(c.Requests.Memory, "Mi"), 10, 64)
			key := [2]string{c.Workload, c.Container}
			if e1 != nil || e2 != nil || c.Requests.CPU != fmt.Sprintf("%dm", cpu) || c.Requests.Memory != fmt.Sprintf("%dMi", mem) || cpu <= 0 || mem <= 0 || requests[key].CPUMilli != 0 {
				return fmt.Errorf("invalid/duplicate prior quantity for %v", key)
			}
			requests[key] = sizing.Budget{CPUMilli: cpu, MemoryMiB: mem}
			percentages[key] = policy.GoMemoryLimitPercent
			if c.GoMemoryLimitPercent != nil {
				percentages[key] = *c.GoMemoryLimitPercent
			}
		}
		if len(requests) != len(s.Rows) {
			return fmt.Errorf("prior policy coverage differs from input")
		}
		kcmCount, catalogCount := 0, 0
		for i := range s.Rows {
			row := &s.Rows[i]
			id := row.Identity
			old, ok := requests[[2]string{id.Workload, id.Container}]
			if !ok {
				return fmt.Errorf("missing prior row %+v", id)
			}
			row.OldRequests = old
			percent := s.GoMemLimitPercent
			if *row.NonGo {
				percent = 0
			}
			if percentages[[2]string{id.Workload, id.Container}] != percent {
				return fmt.Errorf("prior Go memory policy differs for %+v", id)
			}
			if id.InitContainer {
				continue
			}
			target := int64(0)
			if id.Workload == "kube-controller-manager" && id.Container == "kube-controller-manager" {
				target = *kcm
				kcmCount++
			}
			if id.Container == "registry" && (id.Workload == "certified-operators-catalog" || id.Workload == "community-operators-catalog" || id.Workload == "redhat-operators-catalog") {
				target = *catalog
				catalogCount++
			}
			if target != 0 {
				if target < row.Floors.CPUMilli {
					return fmt.Errorf("target would lower protected floor for %+v", id)
				}
				row.Floors.CPUMilli = target
				row.FloorBasis += fmt.Sprintf(" Next offline CPU target %dm; not a proven healthy minimum. Prior plan SHA256 %s.", target, budget.Hash(priorRaw))
			}
		}
		if kcmCount != 1 || catalogCount != 3 {
			return fmt.Errorf("expected one KCM and three catalog rows")
		}
		s.Name, s.PreserveMemory = *name, true
		extras["source-input.json"], extras["prior-plan.json"] = raw, priorRaw
		raw, err = budget.JSON(s)
		if err != nil {
			return err
		}
	} else if *name != "" || *kcm != 0 || *catalog != 0 {
		return fmt.Errorf("CPU target flags require prior-plan")
	}
	r, err := budget.Allocate(s)
	if err != nil {
		return err
	}
	files, err := budget.Files(raw, r)
	if err != nil {
		return err
	}
	for name, data := range extras {
		files[name] = data
	}
	if *prior != "" {
		// Do not copy stale live mutation guards or resource identities into an
		// executable lifecycle. This plan only verifies the candidate offline.
		plan.Actions = []iteration.Action{
			{ID: "cost", Kind: iteration.CostEstimate, Recovery: iteration.ReadOnly, Timeout: 60000000000, MaxAttempts: 1, Cost: &r.CostSpec},
			{ID: "sizing", Kind: iteration.SizingResolve, Recovery: iteration.ReadOnly, Timeout: 60000000000, MaxAttempts: 1, Sizing: &iteration.SizingParameters{Profile: r.Profile, HyperShiftDefaults: &s.Fallback}},
		}
		files["plan.json"], err = budget.JSON(plan)
		if err != nil {
			return err
		}
	}
	if err := budget.Store(*output, files, args[0] == "check"); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"inputSHA256": budget.Hash(raw), "costUSDPerHour": r.Ledger.CostUSDPerHour, "resources": r.Ledger.Resources, "resolverVerified": r.Ledger.ResolverVerified, "qualified": false})
}

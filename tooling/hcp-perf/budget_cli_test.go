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
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/budget"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/iteration"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

func TestBudgetCLI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "output")
	args := []string{"budget", "allocate", "--input", "examples/budget-small.json", "--output", dir}
	if err := run(context.Background(), args, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), args, io.Discard); err == nil {
		t.Fatal("overwrote output")
	}
	args[1] = "check"
	if err := run(context.Background(), args, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{}, {"allocate"}, {"allocate", "--input", "examples/budget-small.json", "--output", dir, "extra"}, {"allocate", "--unknown"}} {
		if err := runBudget(args, io.Discard); err == nil {
			t.Fatalf("accepted invalid args %v", args)
		}
	}
}

func TestBudgetPriorPlanIsOffline(t *testing.T) {
	raw, err := os.ReadFile("examples/budget-small.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := budget.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	r, err := budget.Allocate(s)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := json.Marshal(r.HyperShift["spec"])
	if err != nil {
		t.Fatal(err)
	}
	p := iteration.Spec{Version: "v1", Environment: iteration.Environment{ID: "test", Incarnation: "old"}, Actions: []iteration.Action{{ID: "candidate", Kind: iteration.CandidateApply, Recovery: iteration.GuardedMutation, Timeout: 60000000000, MaxAttempts: 1, Candidate: &startup.Candidate{ExpectedSpec: desired, DesiredSpec: desired}}}}
	prior, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "prior.json")
	if err := os.WriteFile(path, prior, 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "next")
	args := []string{"allocate", "--input", "examples/budget-small.json", "--output", dir, "--prior-plan", path, "--name", "next", "--kcm-cpu-milli", "50", "--catalog-cpu-milli", "50"}
	if err := runBudget(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	args[0] = "check"
	if err := runBudget(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	var plan iteration.Spec
	if err := decodeFile(filepath.Join(dir, "plan.json"), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 2 {
		t.Fatal("unexpected plan actions")
	}
	for _, a := range plan.Actions {
		if a.Recovery != iteration.ReadOnly || a.Candidate != nil || a.RP != nil || a.Startup != nil {
			t.Fatal("copied a live lifecycle action")
		}
	}
	saved, err := os.ReadFile(filepath.Join(dir, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := budget.Decode(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !next.PreserveMemory {
		t.Fatal("memory preservation not explicit")
	}
	for i, row := range next.Rows {
		if row.OldRequests != r.Ledger.Rows[i].Requests {
			t.Fatalf("old requests not rebased for %+v", row.Identity)
		}
	}
}

// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package budget

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

func example(t *testing.T) Spec {
	t.Helper()
	raw, err := os.ReadFile("../../examples/budget-small.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func tiny(t *testing.T) Spec {
	s := example(t)
	for _, row := range s.Rows {
		if row.Identity.Container == "kube-apiserver" && !row.Identity.InitContainer {
			row.Replicas, row.Weights, row.Floors = 1, Weights{"1", "1"}, sizing.Budget{CPUMilli: 1, MemoryMiB: 1}
			s.Rows = []Row{row}
			break
		}
	}
	s.Pods = []Pod{{Workload: "kube-apiserver", Replicas: 1, Regular: []int{0}, Init: []Init{}}}
	s.Downstream.Rows, s.Downstream.Budget = nil, sizing.Budget{}
	s.Node.PriceUSDPerHour, s.BudgetUSDPerHour, s.Node.Budget = "1", "1", sizing.Budget{CPUMilli: 100, MemoryMiB: 100}
	return s
}

func TestQuotaAndStages(t *testing.T) {
	q := example(t).Quota
	for cpu, want := range map[int64]string{1: "10", 3: "10", 10: "10", 23: "30", 183: "190"} {
		if got := quota(rat(cpu), q).RatString(); got != want {
			t.Fatalf("quota(%d)=%s, want %s", cpu, got, want)
		}
	}
	if got := quota(rat(1), Quota{PeriodUS: 50000, MinimumUS: 1500, QuantumUS: 1000}).RatString(); got != "30" {
		t.Fatal(got)
	}
	p := Pod{Regular: []int{0, 1, 2}}
	c := podCharge(p, []*big.Rat{rat(3), rat(3), rat(3)}, true, q)
	if c.Charge != "10" || c.ParentQuotaMilli != "10" || c.UncappedStageMaximumMilli != "30" {
		t.Fatalf("parent cap: %+v", c)
	}
	no, yes := false, true
	p = Pod{Regular: []int{0}, Init: []Init{{1, &no}, {2, &no}}, Overhead: sizing.Budget{CPUMilli: 3, MemoryMiB: 7}}
	v := []*big.Rat{rat(20), rat(50), rat(40)}
	c = podCharge(p, v, false, q)
	if c.Request != "57" || c.Charge != "62" {
		t.Fatalf("sequential init: %+v", c)
	}
	p.Init[0].Restartable = &yes
	c = podCharge(p, v, true, q)
	if c.Request != "93" || c.ParentQuotaMilli != "100" || c.Charge != "93" || c.SteadyQuotaMilli != "73" {
		t.Fatalf("restartable prefix: %+v", c)
	}
	c = podCharge(p, []*big.Rat{rat(1), rat(1), rat(1)}, false, q)
	if c.Charge != "11" {
		t.Fatalf("per-container ceiling: %+v", c)
	}
	// A later restartable sidecar must not be charged alongside an earlier
	// ordinary init container, even when it dominates the steady stage.
	p.Init = []Init{{1, &no}, {2, &yes}}
	c = podCharge(p, v, false, q)
	if c.Request != "67" || c.Charge != "73" {
		t.Fatalf("ordered prefix: %+v", c)
	}
}

func TestExplicitGoMaxProcs(t *testing.T) {
	s := tiny(t)
	s.Downstream.Rows = []DownstreamRow{}
	baseline, err := Allocate(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := baseline.Resolved.Containers[0].Env["GOMAXPROCS"]; exists {
		t.Fatal("GOMAXPROCS must remain absent unless requested")
	}
	value := int64(1)
	s.Rows[0].GoMaxProcs = &value
	raw, err := JSON(s)
	if err != nil {
		t.Fatal(err)
	}
	s, err = Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Allocate(s)
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved.Containers[0].Env["GOMAXPROCS"] != "1" || !reflect.DeepEqual(result.Ledger.Resources, baseline.Ledger.Resources) || result.Ledger.CostUSDPerHour != baseline.Ledger.CostUSDPerHour {
		t.Fatal("explicit runtime setting must propagate without changing resource charges")
	}
	for _, invalid := range []int64{-1, 0, 1025} {
		s.Rows[0].GoMaxProcs = &invalid
		if _, err := Allocate(s); err == nil {
			t.Fatalf("accepted invalid GOMAXPROCS %d", invalid)
		}
	}
	s.Rows[0].GoMaxProcs = &value
	nonGo := true
	s.Rows[0].NonGo = &nonGo
	if _, err := Allocate(s); err == nil {
		t.Fatal("accepted Go runtime setting for non-Go container")
	}
}

func TestGoldenPortableExample(t *testing.T) {
	s := example(t)
	before, _ := JSON(s)
	r, err := Allocate(s)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := JSON(s)
	if !bytes.Equal(before, after) {
		t.Fatal("allocator mutated its input")
	}
	// Constants are from the saved Python allocator's portable fixture tests.
	if len(r.Profile.Containers) != 92 || r.Ledger.CostUSDPerHour != "12493/100000" || r.Ledger.Resources["memoryMiB"].OfferingCharge != "10101" {
		t.Fatalf("golden mismatch: %+v", r.Ledger.Resources)
	}
	var pods, replicas int64
	for _, p := range r.Ledger.Pods {
		pods += p.Replicas
	}
	for _, row := range r.Ledger.Rows {
		replicas += row.Replicas
		if row.Requests.CPUMilli < row.Floors.CPUMilli || row.Requests.MemoryMiB < row.Floors.MemoryMiB {
			t.Fatalf("floor violation: %+v", row.Identity)
		}
	}
	if pods != 52 || replicas != 97 {
		t.Fatalf("inventory changed: %d pods/%d containers", pods, replicas)
	}
	for name, l := range r.Ledger.Resources {
		if number(Number(l.OfferingCharge)).Cmp(number(Number(l.Capacity))) > 0 || number(Number(l.NextGridCharge)).Cmp(number(Number(l.Capacity))) <= 0 {
			t.Fatalf("%s: not maximal under budget: %+v", name, l)
		}
	}
	if !r.Cost.Estimates[0].WithinTarget || r.Ledger.Qualified || r.Ledger.StartupTested || !r.Ledger.ResolverVerified {
		t.Fatal("incorrect verification/qualification flags")
	}
	for _, c := range r.Resolved.Containers {
		if c.Identity.Container == "haproxy" || c.Identity.Container == "router" {
			if len(c.Env) != 0 {
				t.Fatalf("non-Go env: %+v", c)
			}
		}
		if c.Identity.Workload == "cluster-version-operator" && c.Identity.Container == "cluster-version-operator" && c.Env["GOMEMLIMIT"] != "53687091B" {
			t.Fatalf("Go limit: %+v", c)
		}
	}
	second, err := Allocate(s)
	if err != nil || !reflect.DeepEqual(r, second) {
		t.Fatal("allocation is not deterministic", err)
	}
}

func TestBudget03AndNextCandidate(t *testing.T) {
	s := example(t)
	// Explicit deltas between the portable example and the saved budget-etcd-03
	// input. No private artifacts or Python interpreter are needed by this test.
	for i := range s.Rows {
		r := &s.Rows[i]
		switch r.Identity.Workload + "/" + r.Identity.Container {
		case "cluster-image-registry-operator/apiserver-token-minter":
			r.Floors = sizing.Budget{CPUMilli: 50, MemoryMiB: 32}
		case "cluster-version-operator/bootstrap":
			r.Floors = sizing.Budget{CPUMilli: 30, MemoryMiB: 64}
		case "control-plane-operator/control-plane-operator":
			r.Floors.CPUMilli = 60
			r.Weights.CPUMilli = "60"
		case "kube-apiserver/bootstrap":
			r.Floors.CPUMilli = 40
			r.Weights.CPUMilli = "40"
		case "kube-apiserver/kube-apiserver":
			r.Floors.CPUMilli = 99
			r.Weights.CPUMilli = "99"
		case "etcd/etcd":
			r.Floors.CPUMilli = 60
			r.Weights.CPUMilli = "60"
		}
	}
	prior, err := Allocate(s)
	if err != nil {
		t.Fatal(err)
	}
	if prior.Ledger.Resources["cpuMilli"].ProtectedCharge != "2250" || prior.Ledger.Resources["cpuMilli"].OfferingCharge != "2480" || prior.Ledger.Resources["memoryMiB"].OfferingCharge != "10115" {
		t.Fatalf("budget03 parity: %+v", prior.Ledger.Resources)
	}
	for i := range s.Rows {
		r := &s.Rows[i]
		r.OldRequests = prior.Ledger.Rows[i].Requests
		if r.Identity.Container == "registry" || r.Identity.Container == "kube-controller-manager" {
			r.Floors.CPUMilli = 50
		}
	}
	s.PreserveMemory = true
	next, err := Allocate(s)
	if err != nil {
		t.Fatal(err)
	}
	if next.Ledger.Resources["cpuMilli"].ProtectedCharge != "2360" || next.Ledger.Resources["cpuMilli"].OfferingCharge != "2470" || next.Ledger.CostUSDPerHour != "99541/800000" {
		t.Fatalf("candidate mismatch: %+v", next.Ledger.Resources)
	}
	for i, r := range next.Ledger.Rows {
		if r.Requests.MemoryMiB != prior.Ledger.Rows[i].Requests.MemoryMiB || r.Requests.CPUMilli < s.Rows[i].Floors.CPUMilli {
			t.Fatalf("memory/floor changed: %+v", r.Identity)
		}
		if (r.Identity.Container == "registry" || r.Identity.Container == "kube-controller-manager") && r.Requests.CPUMilli != 50 {
			t.Fatalf("target mismatch: %+v", r)
		}
	}
	for i := range s.Rows {
		if s.Rows[i].Identity.Container == "registry" {
			s.Rows[i].Floors.CPUMilli = 100
		}
	}
	_, err = Allocate(s)
	var infeasible *Infeasible
	if !errors.As(err, &infeasible) || infeasible.Protected != "2510" || infeasible.Capacity != "1000000/403" || infeasible.Deficit != "11530/403" {
		t.Fatalf("expected bounded infeasibility, got %v", err)
	}
}

func TestExactBoundsAndOverflow(t *testing.T) {
	s := tiny(t)
	s.Rows[0].Floors = sizing.Budget{CPUMilli: 20, MemoryMiB: 64}
	r, err := Allocate(s)
	if err != nil {
		t.Fatal(err)
	}
	if r.Profile.Anchor.CPUMilli <= 20 || r.Profile.Anchor.MemoryMiB <= 64 {
		t.Fatal("floors treated as fixed ceilings")
	}
	s.Node.Budget = sizing.Budget{CPUMilli: 20, MemoryMiB: 71}
	r, err = Allocate(s)
	if err != nil || r.Profile.Anchor != s.Rows[0].Floors || r.Ledger.CostUSDPerHour != "1" {
		t.Fatal("exact exhaustion", err)
	}
	s.Node.MemoryMiB = 70
	if _, err := Allocate(s); err == nil {
		t.Fatal("accepted infeasible memory")
	}
	s.Node.MemoryMiB = 71
	s.Node.CPUMilli = 19
	if _, err := Allocate(s); err == nil {
		t.Fatal("accepted infeasible CPU")
	}
	s = tiny(t)
	s.Rows[0].Replicas = 3
	s.Pods[0].Replicas = 3
	r, err = Allocate(s)
	if err != nil || r.Ledger.Resources["cpuMilli"].OfferingCharge != "90" || r.Ledger.Resources["memoryMiB"].OfferingCharge != "99" {
		t.Fatal("replicated rounding", err)
	}
	s = tiny(t)
	s.Rows[0].Weights = Weights{"1/1000000000000000000000000000000000000000000000000", "1"}
	if _, err := Allocate(s); err != nil {
		t.Fatal("arbitrary precision grid search", err)
	}
	s = tiny(t)
	s.Rows[0].Floors.MemoryMiB = math.MaxInt64
	s.Node.MemoryMiB = math.MaxInt64
	if _, err := Allocate(s); err == nil {
		t.Fatal("overflowing memory ceiling accepted")
	}
	s = tiny(t)
	s.PreserveMemory = true
	s.Rows[0].OldRequests.MemoryMiB = 1
	s.Rows[0].Floors.MemoryMiB = 2
	if _, err := Allocate(s); err == nil {
		t.Fatal("preserved memory below floor accepted")
	}
}

func TestInvalidInputs(t *testing.T) {
	for name, change := range map[string]func(*Spec){
		"zero floor":             func(s *Spec) { s.Rows[0].Floors.CPUMilli = 0 },
		"zero weight":            func(s *Spec) { s.Rows[0].Weights.CPUMilli = "0" },
		"zero period":            func(s *Spec) { s.Quota.PeriodUS = 0 },
		"negative overhead":      func(s *Spec) { s.Pods[0].Overhead.CPUMilli = -1 },
		"duplicate row":          func(s *Spec) { s.Rows = append(s.Rows, s.Rows[0]) },
		"duplicate binding":      func(s *Spec) { s.Pods[0].Regular = append(s.Pods[0].Regular, 0) },
		"unknown binding":        func(s *Spec) { s.Pods[0].Regular = []int{999} },
		"missing classification": func(s *Spec) { s.Rows[0].NonGo = nil },
		"count mismatch":         func(s *Spec) { s.Rows[0].Replicas++ },
		"audit mismatch":         func(s *Spec) { s.Rows[0].Source.SHA256 = strings.Repeat("a", 64) },
		"downstream mismatch":    func(s *Spec) { s.Downstream.CPUMilli++ },
		"exclusion overlap":      func(s *Spec) { s.ExcludedInstrumentation[0].Workload = s.Rows[0].Identity.Workload },
	} {
		t.Run(name, func(t *testing.T) {
			s := example(t)
			change(&s)
			if _, err := Allocate(s); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	s := example(t)
	raw, _ := JSON(s)
	for _, bad := range [][]byte{
		bytes.Replace(raw, []byte(`"cpuMilli": "10"`), []byte(`"cpuMilli": 1.5`), 1),
		bytes.Replace(raw, []byte(`"name":`), []byte(`"name":"duplicate","name":`), 1),
		bytes.Replace(raw, []byte(`"cpuMilli":`), []byte(`"cpuMilil":`), 1),
		bytes.Replace(raw, []byte(`"replicas": 1`), []byte(`"replicas": null`), 1),
		bytes.Replace(raw, []byte(`"name":`), []byte(`"Name":`), 1),
		bytes.Replace(raw, []byte(`"replicas": 1,`), nil, 1),
		append(append([]byte{}, raw...), []byte(` {}`)...),
	} {
		if _, err := Decode(bad); err == nil {
			t.Fatal("malformed input accepted")
		}
	}
}

func TestImmutableOutputs(t *testing.T) {
	s := tiny(t)
	raw, _ := JSON(s)
	r, err := Allocate(s)
	if err != nil {
		t.Fatal(err)
	}
	files, err := Files(raw, r)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "candidate")
	if err := Store(dir, files, false); err != nil {
		t.Fatal(err)
	}
	if err := Store(dir, files, true); err != nil {
		t.Fatal(err)
	}
	if err := Store(dir, files, false); err == nil {
		t.Fatal("overwrote existing directory")
	}
	if err := os.WriteFile(filepath.Join(dir, "candidate.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Store(dir, files, true); err == nil {
		t.Fatal("accepted changed output")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := Store(link, files, true); err == nil {
		t.Fatal("accepted symlink")
	}
}

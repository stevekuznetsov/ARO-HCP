// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cost

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
)

func fixture() Spec {
	return Spec{Version: "v1", Currency: "USD", FeeMicroUSDPerHCPHour: 250000, TargetPercent: 50,
		SKUs:       []SKU{{Name: "test", Region: "westus3", PriceMicroUSDPerHour: 1000000, CapacityBasis: "usable", AllocatableCPUMilli: 16000, AllocatableMemoryMiB: 64000, PriceSourceURL: "https://prices.azure.com/api/retail/prices", PriceRetrievedAt: "2026-09-09T00:00:00Z"}},
		DataPoints: []DataPoint{{Name: "aggregate", GuestNodes: 3, SourceKind: "requests", Demand: Demand{CPUMilli: 2000, MemoryMiB: 8000}}}}
}

func TestCalculate(t *testing.T) {
	for _, tt := range []struct {
		name       string
		demand     Demand
		cost       string
		bottleneck []string
		capacity   int64
	}{
		{"matching", Demand{CPUMilli: 2000, MemoryMiB: 8000}, "0.125000000", []string{"cpu", "memory"}, 8},
		{"cpu", Demand{CPUMilli: 4000, MemoryMiB: 8000}, "0.250000000", []string{"cpu"}, 4},
		{"memory", Demand{CPUMilli: 2000, MemoryMiB: 32000}, "0.500000000", []string{"memory"}, 2},
		{"noninteger", Demand{CPUMilli: 6000}, "0.375000000", []string{"cpu"}, 2},
		{"oversize", Demand{CPUMilli: 32000}, "2.000000000", []string{"cpu"}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := fixture()
			s.DataPoints[0].Demand = tt.demand
			r, err := Calculate(s)
			if err != nil {
				t.Fatal(err)
			}
			e := r.Estimates[0]
			if e.ResourceEquivalentCostUSDhr != tt.cost || !reflect.DeepEqual(e.Bottleneck, tt.bottleneck) || e.TheoreticalHomogeneousCapacity == nil || *e.TheoreticalHomogeneousCapacity != tt.capacity {
				t.Fatalf("unexpected estimate: %+v", e)
			}
			if e.BudgetCPUMilli != "2000" || e.BudgetMemoryMiB != "8000" || r.TargetBudgetUSDhr != "0.125000000" {
				t.Fatalf("incorrect envelope: %+v", e)
			}
		})
	}
}

func TestBudgetBoundary(t *testing.T) {
	for _, delta := range []int64{-1, 0, 1} {
		s := fixture()
		s.DataPoints[0].Demand.CPUMilli += delta
		r, err := Calculate(s)
		if err != nil || r.Estimates[0].WithinTarget != (delta <= 0) {
			t.Fatalf("delta %d: %+v, %v", delta, r, err)
		}
	}
	for _, percent := range []int64{100, 1000, 1001, math.MaxInt64} {
		s := fixture()
		s.TargetPercent = percent
		s.FeeMicroUSDPerHCPHour = math.MaxInt64
		if _, err := Calculate(s); err != nil {
			t.Fatalf("positive target %d: %v", percent, err)
		}
	}
}

func TestOverheadOptionalAndPool(t *testing.T) {
	s := fixture()
	sku := &s.SKUs[0]
	sku.CapacityBasis = "allocatable"
	sku.OverheadCPUMilli = 8000
	sku.OverheadMemoryMiB = 32000
	slots, nics := int64(10), int64(8)
	sku.UsablePodSlots = &slots
	sku.UsableNICs = &nics
	s.DataPoints[0].Demand.PodSlots = 5
	s.DataPoints[0].Demand.SwiftNICs = 2
	s.Pool = &Pool{HCPCount: 4, Nodes: []PoolNode{{SKUName: "test", Count: 3}}}
	r, err := Calculate(s)
	if err != nil {
		t.Fatal(err)
	}
	e := r.Estimates[0]
	if e.ResourceEquivalentCostUSDhr != "0.500000000" || e.RatioOfFee != "2.000000000" || e.BudgetCPUMilli != "1000" || e.BudgetMemoryMiB != "4000" || *e.TheoreticalHomogeneousCapacity != 2 || r.ActualPackedCostUSDhr != "0.750000000" {
		t.Fatalf("unexpected result: %+v", r)
	}
	s.DataPoints[0].Demand.SwiftNICs = 8
	r, err = Calculate(s)
	if err != nil || !reflect.DeepEqual(r.Estimates[0].Bottleneck, []string{"swiftNICs"}) {
		t.Fatalf("NIC not limiting: %+v %v", r, err)
	}
}

func TestZeroAndOverflow(t *testing.T) {
	s := fixture()
	s.DataPoints[0].Demand = Demand{}
	s.SKUs[0].AllocatableCPUMilli = 0
	s.SKUs[0].AllocatableMemoryMiB = 0
	r, err := Calculate(s)
	if err != nil || r.Estimates[0].ResourceEquivalentCostUSDhr != "0.000000000" || r.Estimates[0].TheoreticalHomogeneousCapacity != nil {
		t.Fatalf("zero demand: %+v %v", r, err)
	}
	s = fixture()
	s.SKUs[0].PriceMicroUSDPerHour = math.MaxInt64
	s.SKUs[0].AllocatableCPUMilli = 1
	s.DataPoints[0].Demand.CPUMilli = math.MaxInt64
	s.Pool = &Pool{HCPCount: 1, Nodes: []PoolNode{{SKUName: "test", Count: math.MaxInt64}}}
	r, err = Calculate(s)
	if err != nil || r.Estimates[0].WithinTarget || r.ActualPackedCostUSDhr != r.Estimates[0].ResourceEquivalentCostUSDhr {
		t.Fatalf("overflow: %+v %v", r, err)
	}
	a, _ := json.Marshal(r)
	r, _ = Calculate(s)
	b, _ := json.Marshal(r)
	if string(a) != string(b) {
		t.Fatal("nondeterministic output")
	}
}

func TestValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"version":             func(s *Spec) { s.Version = "v2" },
		"currency":            func(s *Spec) { s.Currency = "EUR" },
		"fee":                 func(s *Spec) { s.FeeMicroUSDPerHCPHour = 0 },
		"target":              func(s *Spec) { s.TargetPercent = 0 },
		"demand":              func(s *Spec) { s.DataPoints[0].Demand.CPUMilli = -1 },
		"source":              func(s *Spec) { s.DataPoints[0].SourceKind = "forecast" },
		"observed statistics": func(s *Spec) { s.DataPoints[0].SourceKind = "observed" },
		"mixed statistics":    func(s *Spec) { s.DataPoints[0].CPUStatistic = "mean" },
		"evidence":            func(s *Spec) { s.DataPoints[0].Evidence = []Evidence{{Path: "x", SHA256: "bad"}} },
		"overhead twice":      func(s *Spec) { s.SKUs[0].OverheadCPUMilli = 1 },
		"overhead negative":   func(s *Spec) { s.SKUs[0].OverheadCPUMilli = -1 },
		"overhead exceeds":    func(s *Spec) { s.SKUs[0].CapacityBasis = "allocatable"; s.SKUs[0].OverheadMemoryMiB = 64001 },
		"zero capacity":       func(s *Spec) { s.SKUs[0].AllocatableCPUMilli = 0 },
		"optional zero":       func(s *Spec) { z := int64(0); s.SKUs[0].UsableNICs = &z; s.DataPoints[0].Demand.SwiftNICs = 1 },
		"raw as allocatable":  func(s *Spec) { s.SKUs[0].CapacityBasis = "raw-illustration" },
		"basis missing":       func(s *Spec) { s.SKUs[0].CapacityBasis = "" },
		"price date":          func(s *Spec) { s.SKUs[0].PriceRetrievedAt = "unknown" },
		"price source":        func(s *Spec) { s.SKUs[0].PriceSourceURL = "not a URL" },
		"price":               func(s *Spec) { s.SKUs[0].PriceMicroUSDPerHour = -1 },
		"duplicate SKU":       func(s *Spec) { s.SKUs = append(s.SKUs, s.SKUs[0]) },
		"duplicate point":     func(s *Spec) { s.DataPoints = append(s.DataPoints, s.DataPoints[0]) },
		"pool zero HCP":       func(s *Spec) { s.Pool = &Pool{} },
		"pool unknown SKU":    func(s *Spec) { s.Pool = &Pool{HCPCount: 1, Nodes: []PoolNode{{SKUName: "unknown", Count: 1}}} },
	} {
		t.Run(name, func(t *testing.T) {
			s := fixture()
			mutate(&s)
			if _, err := Calculate(s); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestRawAndEvidence(t *testing.T) {
	s := fixture()
	sku := &s.SKUs[0]
	sku.CapacityBasis = "raw-illustration"
	sku.RawCPUMilli = sku.AllocatableCPUMilli
	sku.RawMemoryMiB = sku.AllocatableMemoryMiB
	sku.AllocatableCPUMilli = 0
	sku.AllocatableMemoryMiB = 0
	p := &s.DataPoints[0]
	p.SourceKind = "observed"
	p.CPUStatistic = "p95"
	p.MemoryStatistic = "rss"
	p.Evidence = []Evidence{{Path: "measurement.json", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	r, err := Calculate(s)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Estimates[0].DataPoint, *p) || len(r.Estimates[0].Warnings) != 4 {
		t.Fatalf("provenance lost: %+v", r)
	}
}

func TestExample(t *testing.T) {
	data, err := os.ReadFile("../../examples/cost.json")
	if err != nil {
		t.Fatal(err)
	}
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	r, err := Calculate(s)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"0.100750000", "0.201500000", "0.132250000", "0.264500000"} {
		if r.Estimates[i].ResourceEquivalentCostUSDhr != want || r.Estimates[i].WithinTarget != (i == 0) {
			t.Fatalf("example estimate %d: %+v", i, r.Estimates[i])
		}
	}
	if r.Estimates[0].BudgetCPUMilli != "2481" || r.Estimates[0].BudgetMemoryMiB != "10163" {
		t.Fatalf("incorrect nonintegral budget floors: %+v", r.Estimates[0])
	}
}

func TestOptionalDimensions(t *testing.T) {
	s := fixture()
	s.DataPoints[0].Demand.PodSlots = math.MaxInt64
	r, err := Calculate(s)
	if err != nil || r.Estimates[0].ResourceEquivalentCostUSDhr != "0.125000000" {
		t.Fatalf("unspecified dimension should be excluded: %+v %v", r, err)
	}
	z := int64(0)
	s.SKUs[0].UsablePodSlots = &z
	s.DataPoints[0].Demand.PodSlots = 0
	r, err = Calculate(s)
	if err != nil || r.Estimates[0].ResourceEquivalentCostUSDhr != "0.125000000" {
		t.Fatalf("explicit zero capacity and demand should be ignored: %+v %v", r, err)
	}
}

func TestComparisonBeforeDisplayRounding(t *testing.T) {
	s := fixture()
	s.SKUs[0].AllocatableCPUMilli = math.MaxInt64
	s.SKUs[0].AllocatableMemoryMiB = 0
	s.DataPoints[0].Demand = Demand{CPUMilli: math.MaxInt64/8 + 1}
	r, err := Calculate(s)
	if err != nil {
		t.Fatal(err)
	}
	if r.Estimates[0].ResourceEquivalentCostUSDhr != r.TargetBudgetUSDhr || r.Estimates[0].WithinTarget {
		t.Fatalf("exact comparison was lost to rounding: %+v", r.Estimates[0])
	}
}

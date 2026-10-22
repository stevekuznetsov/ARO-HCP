// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package cost estimates compute-only resource equivalents, not schedulability.
package cost

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"net/url"
	"time"
)

type Spec struct {
	Version               string      `json:"version"`
	Currency              string      `json:"currency"`
	FeeMicroUSDPerHCPHour int64       `json:"feeMicroUSDPerHCPHour"`
	TargetPercent         int64       `json:"targetPercent"`
	SKUs                  []SKU       `json:"skus"`
	DataPoints            []DataPoint `json:"dataPoints"`
	Pool                  *Pool       `json:"pool,omitempty"`
}

type SKU struct {
	Name                 string `json:"name"`
	Region               string `json:"region"`
	PriceMicroUSDPerHour int64  `json:"priceMicroUSDPerHour"`
	CapacityBasis        string `json:"capacityBasis"`
	AllocatableCPUMilli  int64  `json:"allocatableCPUMilli"`
	AllocatableMemoryMiB int64  `json:"allocatableMemoryMiB"`
	RawCPUMilli          int64  `json:"rawCPUMilli,omitempty"`
	RawMemoryMiB         int64  `json:"rawMemoryMiB,omitempty"`
	OverheadCPUMilli     int64  `json:"overheadCPUMilli"`
	OverheadMemoryMiB    int64  `json:"overheadMemoryMiB"`
	UsableNICs           *int64 `json:"usableNICs,omitempty"`
	UsablePodSlots       *int64 `json:"usablePodSlots,omitempty"`
	PriceSourceURL       string `json:"priceSourceURL"`
	PriceRetrievedAt     string `json:"priceRetrievedAt"`
}

type Demand struct {
	CPUMilli  int64 `json:"cpuMilli"`
	MemoryMiB int64 `json:"memoryMiB"`
	PodSlots  int64 `json:"podSlots"`
	SwiftNICs int64 `json:"swiftNICs"`
}

type Evidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Demand is the entire HCP aggregate, already including every replica.
type DataPoint struct {
	Name            string     `json:"name"`
	GuestNodes      int64      `json:"guestNodes"`
	Apps            int64      `json:"apps"`
	Demand          Demand     `json:"demand"`
	SourceKind      string     `json:"sourceKind"`
	CPUStatistic    string     `json:"cpuStatistic,omitempty"`
	MemoryStatistic string     `json:"memoryStatistic,omitempty"`
	Evidence        []Evidence `json:"evidence,omitempty"`
}

type Pool struct {
	HCPCount int64      `json:"hcpCount"`
	Nodes    []PoolNode `json:"nodes"`
}

type PoolNode struct {
	SKUName string `json:"skuName"`
	Count   int64  `json:"count"`
}

type Estimate struct {
	SKU                            SKU       `json:"sku"`
	DataPoint                      DataPoint `json:"dataPoint"`
	ResourceEquivalentCostUSDhr    string    `json:"resourceEquivalentCostUSDhr"`
	RatioOfFee                     string    `json:"ratioOfFee"`
	Bottleneck                     []string  `json:"bottleneck"`
	BudgetCPUMilli                 string    `json:"budgetCPUMilli"`
	BudgetMemoryMiB                string    `json:"budgetMemoryMiB"`
	WithinTarget                   bool      `json:"withinTarget"`
	TheoreticalHomogeneousCapacity *int64    `json:"theoreticalHomogeneousCapacity"`
	Warnings                       []string  `json:"warnings"`
}

type Result struct {
	Version               string     `json:"version"`
	Currency              string     `json:"currency"`
	Scope                 string     `json:"scope"`
	FeeMicroUSDPerHCPHour int64      `json:"feeMicroUSDPerHCPHour"`
	TargetPercent         int64      `json:"targetPercent"`
	TargetBudgetUSDhr     string     `json:"targetBudgetUSDhr"`
	Estimates             []Estimate `json:"estimates"`
	ActualPackedCostUSDhr string     `json:"actualPackedCostUSDhr,omitempty"`
	Pool                  *Pool      `json:"pool,omitempty"`
}

func rat(n int64) *big.Rat      { return new(big.Rat).SetInt64(n) }
func floor(r *big.Rat) *big.Int { return new(big.Int).Quo(r.Num(), r.Denom()) }
func usd(micro *big.Rat) string { return new(big.Rat).Quo(micro, rat(1000000)).FloatString(9) }

// Calculate is deterministic and performs no filesystem, pricing, or Azure calls.
// Monetary comparisons and integer bounds are exact; display strings are rounded.
func Calculate(s Spec) (Result, error) {
	fail := func(format string, args ...any) (Result, error) { return Result{}, fmt.Errorf(format, args...) }
	if s.Version != "v1" || s.Currency != "USD" || s.FeeMicroUSDPerHCPHour <= 0 || s.TargetPercent <= 0 || len(s.SKUs) == 0 || len(s.DataPoints) == 0 {
		return fail("require version v1, currency USD, positive fee/targetPercent, skus and dataPoints")
	}
	budget := new(big.Rat).Quo(new(big.Rat).Mul(rat(s.FeeMicroUSDPerHCPHour), rat(s.TargetPercent)), rat(100))
	r := Result{Version: s.Version, Currency: s.Currency, Scope: "compute-only; excludes PVC, network, service prices and infrastructure outside the supplied pool; resource equivalents are lower bounds, not density qualification", FeeMicroUSDPerHCPHour: s.FeeMicroUSDPerHCPHour, TargetPercent: s.TargetPercent, TargetBudgetUSDhr: usd(budget), Pool: s.Pool}
	points := map[string]bool{}
	for _, p := range s.DataPoints {
		d := p.Demand
		if p.Name == "" || points[p.Name] || p.GuestNodes < 0 || p.Apps < 0 || d.CPUMilli < 0 || d.MemoryMiB < 0 || d.PodSlots < 0 || d.SwiftNICs < 0 {
			return fail("invalid or duplicate data point %q", p.Name)
		}
		points[p.Name] = true
		if p.SourceKind != "requests" && p.SourceKind != "observed" {
			return fail("%s: sourceKind must be requests or observed", p.Name)
		}
		if p.SourceKind == "observed" && ((p.CPUStatistic != "mean" && p.CPUStatistic != "p95") || p.MemoryStatistic != "rss") {
			return fail("%s: observed demand requires cpuStatistic mean/p95 and memoryStatistic rss", p.Name)
		}
		if p.SourceKind == "requests" && (p.CPUStatistic != "" || p.MemoryStatistic != "") {
			return fail("%s: request demand must not carry observed statistics", p.Name)
		}
		for _, e := range p.Evidence {
			hash, err := hex.DecodeString(e.SHA256)
			if e.Path == "" || err != nil || len(hash) != 32 {
				return fail("%s: evidence requires a path and SHA-256", p.Name)
			}
		}
	}
	prices := map[string]int64{}
	for _, sku := range s.SKUs {
		u, err := url.Parse(sku.PriceSourceURL)
		_, dateErr := time.Parse(time.RFC3339, sku.PriceRetrievedAt)
		if sku.Name == "" || sku.Region == "" || prices[sku.Name] != 0 || sku.PriceMicroUSDPerHour <= 0 || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || dateErr != nil {
			return fail("%q: require unique name, region, positive price, source URL and RFC3339 retrieval date", sku.Name)
		}
		prices[sku.Name] = sku.PriceMicroUSDPerHour
		cpu, mem := sku.AllocatableCPUMilli, sku.AllocatableMemoryMiB
		switch sku.CapacityBasis {
		case "raw-illustration":
			if cpu != 0 || mem != 0 {
				return fail("%s: raw illustration must not specify allocatable capacity", sku.Name)
			}
			cpu, mem = sku.RawCPUMilli, sku.RawMemoryMiB
		case "allocatable", "usable":
			if sku.RawCPUMilli != 0 || sku.RawMemoryMiB != 0 {
				return fail("%s: raw capacity only allowed for raw-illustration", sku.Name)
			}
			if sku.CapacityBasis == "usable" && (sku.OverheadCPUMilli != 0 || sku.OverheadMemoryMiB != 0) {
				return fail("%s: usable capacity is already post-overhead", sku.Name)
			}
		default:
			return fail("%s: capacityBasis must be allocatable, usable or raw-illustration", sku.Name)
		}
		if cpu < 0 || mem < 0 || sku.OverheadCPUMilli < 0 || sku.OverheadMemoryMiB < 0 || sku.OverheadCPUMilli > cpu || sku.OverheadMemoryMiB > mem {
			return fail("%s: invalid capacity or overhead", sku.Name)
		}
		cpu -= sku.OverheadCPUMilli
		mem -= sku.OverheadMemoryMiB
		if (sku.UsableNICs != nil && *sku.UsableNICs < 0) || (sku.UsablePodSlots != nil && *sku.UsablePodSlots < 0) {
			return fail("%s: negative optional capacity", sku.Name)
		}
		for _, p := range s.DataPoints {
			e := Estimate{SKU: sku, DataPoint: p, Bottleneck: []string{}, Warnings: []string{}}
			if sku.CapacityBasis == "raw-illustration" {
				e.Warnings = append(e.Warnings, "Illustrative raw capacity minus configured overhead (default zero); allocatable capacity is unknown.")
			}
			if len(p.Evidence) == 0 {
				e.Warnings = append(e.Warnings, "Demand has no evidence references; not a known measured footprint.")
			} else {
				e.Warnings = append(e.Warnings, "Evidence references are carried forward, not verified by this calculator.")
			}
			dimensions := []struct {
				name             string
				demand, capacity int64
			}{{"cpu", p.Demand.CPUMilli, cpu}, {"memory", p.Demand.MemoryMiB, mem}}
			for _, optional := range []struct {
				name     string
				demand   int64
				capacity *int64
			}{{"podSlots", p.Demand.PodSlots, sku.UsablePodSlots}, {"swiftNICs", p.Demand.SwiftNICs, sku.UsableNICs}} {
				if optional.capacity != nil {
					dimensions = append(dimensions, struct {
						name             string
						demand, capacity int64
					}{optional.name, optional.demand, *optional.capacity})
				} else {
					e.Warnings = append(e.Warnings, optional.name+" capacity unspecified; dimension excluded.")
				}
			}
			share := rat(0)
			for _, dim := range dimensions {
				if dim.demand == 0 {
					continue
				}
				if dim.capacity == 0 {
					return fail("%s/%s: positive %s demand requires positive usable capacity", sku.Name, p.Name, dim.name)
				}
				fraction := new(big.Rat).SetFrac(big.NewInt(dim.demand), big.NewInt(dim.capacity))
				if cmp := fraction.Cmp(share); cmp >= 0 {
					if cmp > 0 {
						e.Bottleneck = nil
						share.Set(fraction)
					}
					e.Bottleneck = append(e.Bottleneck, dim.name)
				}
			}
			if share.Sign() > 0 {
				n := floor(new(big.Rat).Inv(share)).Int64()
				e.TheoreticalHomogeneousCapacity = &n
			}
			price := rat(sku.PriceMicroUSDPerHour)
			cost := new(big.Rat).Mul(price, share)
			e.ResourceEquivalentCostUSDhr = usd(cost)
			e.RatioOfFee = new(big.Rat).Quo(cost, rat(s.FeeMicroUSDPerHCPHour)).FloatString(9)
			e.WithinTarget = cost.Cmp(budget) <= 0
			budgetShare := new(big.Rat).Quo(budget, price)
			e.BudgetCPUMilli = floor(new(big.Rat).Mul(budgetShare, rat(cpu))).String()
			e.BudgetMemoryMiB = floor(new(big.Rat).Mul(budgetShare, rat(mem))).String()
			r.Estimates = append(r.Estimates, e)
		}
	}
	if s.Pool != nil {
		if s.Pool.HCPCount <= 0 || len(s.Pool.Nodes) == 0 {
			return fail("pool requires positive hcpCount and nodes")
		}
		total := rat(0)
		for _, n := range s.Pool.Nodes {
			price, ok := prices[n.SKUName]
			if !ok || n.Count <= 0 {
				return fail("pool: unknown SKU or nonpositive count for %q", n.SKUName)
			}
			total.Add(total, new(big.Rat).Mul(rat(price), rat(n.Count)))
		}
		r.ActualPackedCostUSDhr = usd(new(big.Rat).Quo(total, rat(s.Pool.HCPCount)))
	}
	return r, nil
}

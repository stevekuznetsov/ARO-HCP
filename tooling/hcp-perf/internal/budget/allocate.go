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
	"fmt"
	"math/big"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/cost"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

func rat(n int64) *big.Rat     { return new(big.Rat).SetInt64(n) }
func number(n Number) *big.Rat { r, _ := new(big.Rat).SetString(string(n)); return r }
func ceil(r *big.Rat) *big.Int {
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), rem)
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}
func dimension(b sizing.Budget, cpu bool) int64 {
	if cpu {
		return b.CPUMilli
	}
	return b.MemoryMiB
}

func quota(cpu *big.Rat, q Quota) *big.Rat {
	units := new(big.Rat).Quo(new(big.Rat).Mul(cpu, rat(q.PeriodUS)), new(big.Rat).Mul(rat(1000), rat(q.QuantumUS)))
	us := new(big.Int).Mul(ceil(units), big.NewInt(q.QuantumUS))
	if us.Cmp(big.NewInt(q.MinimumUS)) < 0 {
		us.SetInt64(q.MinimumUS)
	}
	return new(big.Rat).SetFrac(new(big.Int).Mul(us, big.NewInt(1000)), big.NewInt(q.PeriodUS))
}

type Charge struct {
	Request                   string `json:"request"`
	Charge                    string `json:"charge"`
	ParentQuotaMilli          string `json:"parentQuotaMilli,omitempty"`
	UncappedStageMaximumMilli string `json:"uncappedStageMaximumMilli,omitempty"`
	SteadyQuotaMilli          string `json:"steadyQuotaMilli,omitempty"`
	OldCharge                 string `json:"oldCharge,omitempty"`
	ReplicatedChargeDelta     string `json:"replicatedChargeDelta,omitempty"`
}

// Restartable init containers accumulate as a prefix, not alongside earlier
// ordinary init containers. The Pod parent can cap the sum of child quotas.
func podCharge(p Pod, values []*big.Rat, cpu bool, q Quota) Charge {
	var stages [][]int
	var sidecars []int
	for _, init := range p.Init {
		stage := append([]int{}, sidecars...)
		stages = append(stages, append(stage, init.Row))
		if *init.Restartable {
			sidecars = append(sidecars, init.Row)
		}
	}
	stages = append(stages, append(append([]int{}, p.Regular...), sidecars...))
	overhead := rat(dimension(p.Overhead, cpu))
	requested, charged, steady := rat(0), rat(0), rat(0)
	for _, stage := range stages {
		request, charge := rat(0), rat(0)
		for _, i := range stage {
			request.Add(request, values[i])
			if cpu {
				charge.Add(charge, quota(values[i], q))
			} else {
				charge.Add(charge, new(big.Rat).SetInt(ceil(new(big.Rat).Mul(values[i], big.NewRat(11, 10)))))
			}
		}
		request.Add(request, overhead)
		charge.Add(charge, overhead)
		if request.Cmp(requested) > 0 {
			requested.Set(request)
		}
		if charge.Cmp(charged) > 0 {
			charged.Set(charge)
		}
		steady.Set(charge)
	}
	result := Charge{Request: requested.RatString()}
	if cpu {
		parent := quota(requested, q)
		result.ParentQuotaMilli, result.UncappedStageMaximumMilli = parent.RatString(), charged.RatString()
		if charged.Cmp(parent) > 0 {
			charged.Set(parent)
		}
		if steady.Cmp(parent) > 0 {
			steady.Set(parent)
		}
		result.SteadyQuotaMilli = steady.RatString()
	}
	result.Charge = charged.RatString()
	return result
}

type ResourceLedger struct {
	Factor                  string `json:"factor"`
	Capacity                string `json:"capacity"`
	ProtectedCharge         string `json:"protectedCharge"`
	RemainingAfterProtected string `json:"remainingAfterProtected"`
	OfferingCharge          string `json:"offeringCharge"`
	OldOfferingCharge       string `json:"oldOfferingCharge"`
	Unused                  string `json:"unused"`
	NextGridCharge          string `json:"nextGridCharge,omitempty"`
	SteadyRequests          string `json:"steadyRequests"`
	SchedulerRequests       string `json:"schedulerRequests"`
}

type LedgerRow struct {
	Row
	LayoutInstances         string            `json:"layoutInstances"`
	Requests                sizing.Budget     `json:"requests"`
	Delta                   sizing.Budget     `json:"delta"`
	SteadyDonorContribution map[string]string `json:"steadyDonorContribution"`
	ChildQuotaMilli         string            `json:"childQuotaMilli"`
	Risks                   []string          `json:"risks"`
}

type LedgerPod struct {
	Pod
	Resources map[string]Charge `json:"resources"`
}

type Ledger struct {
	Version           string                    `json:"version"`
	Qualified         bool                      `json:"qualified"`
	StartupTested     bool                      `json:"startupTested"`
	ResolverVerified  bool                      `json:"resolverVerified"`
	CostUSDPerHour    string                    `json:"costUSDPerHour"`
	Resources         map[string]ResourceLedger `json:"resources"`
	Rows              []LedgerRow               `json:"rows"`
	Pods              []LedgerPod               `json:"pods"`
	MissingLayoutRows []int                     `json:"missingLayoutRows"`
}

type Result struct {
	Profile    sizing.Profile         `json:"profile"`
	Resolved   sizing.ResolvedProfile `json:"resolved"`
	HyperShift map[string]any         `json:"hypershift"`
	Ledger     Ledger                 `json:"ledger"`
	CostSpec   cost.Spec              `json:"costSpec"`
	Cost       cost.Result            `json:"cost"`
}

// Infeasible reports the exact bound without relaxing protected floors.
type Infeasible struct{ Resource, Protected, Capacity, Deficit string }

func (e *Infeasible) Error() string {
	return fmt.Sprintf("%s: protected allocations exceed budget (%s > %s; deficit %s)", e.Resource, e.Protected, e.Capacity, e.Deficit)
}

// Allocate ports the budget/v1 grid search using arbitrary-precision arithmetic.
// It never mutates the supplied rows, reads files or invokes other executables.
func Allocate(s Spec) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	result := Result{Ledger: Ledger{Version: s.Version, Resources: map[string]ResourceLedger{}, MissingLayoutRows: []int{}}}
	allocations := make([]sizing.Budget, len(s.Rows))
	offering := map[string]*big.Rat{}
	for _, cpu := range []bool{true, false} {
		name := "memoryMiB"
		if cpu {
			name = "cpuMilli"
		}
		capacity := new(big.Rat).Quo(new(big.Rat).Mul(rat(dimension(s.Node.Budget, cpu)), number(s.BudgetUSDPerHour)), number(s.Node.PriceUSDPerHour))
		fixed := rat(dimension(s.Downstream.Budget, cpu))
		weights, floors, old := make([]*big.Rat, len(s.Rows)), make([]*big.Rat, len(s.Rows)), make([]*big.Rat, len(s.Rows))
		for i, row := range s.Rows {
			weight := row.Weights.MemoryMiB
			if cpu {
				weight = row.Weights.CPUMilli
			}
			weights[i], floors[i], old[i] = number(weight), rat(dimension(row.Floors, cpu)), rat(dimension(row.OldRequests, cpu))
		}
		charge := func(values []*big.Rat) *big.Rat {
			total := new(big.Rat).Set(fixed)
			for _, pod := range s.Pods {
				c := podCharge(pod, values, cpu, s.Quota)
				total.Add(total, new(big.Rat).Mul(number(Number(c.Charge)), rat(pod.Replicas)))
			}
			return total
		}
		protected := charge(floors)
		if protected.Cmp(capacity) > 0 {
			return Result{}, &Infeasible{name, protected.RatString(), capacity.RatString(), new(big.Rat).Sub(protected, capacity).RatString()}
		}
		grid := big.NewInt(s.FactorGridDenominator)
		values := func(tick *big.Int) []*big.Rat {
			out := make([]*big.Rat, len(s.Rows))
			for i, weight := range weights {
				v := new(big.Rat).Mul(weight, new(big.Rat).SetFrac(tick, grid))
				out[i] = new(big.Rat).SetInt(new(big.Int).Quo(v.Num(), v.Denom()))
				if out[i].Cmp(floors[i]) < 0 {
					out[i].Set(floors[i])
				}
			}
			return out
		}
		low := new(big.Int)
		var chosen []*big.Rat
		factor, next := "preserved", ""
		if !cpu && s.PreserveMemory {
			chosen = make([]*big.Rat, len(old))
			for i, row := range s.Rows {
				chosen[i] = new(big.Rat).Set(old[i])
				if row.FixedMemoryMiB != nil {
					chosen[i] = rat(*row.FixedMemoryMiB)
					factor = "preserved-with-explicit-overrides"
				}
				if chosen[i].Cmp(floors[i]) < 0 {
					return Result{}, fmt.Errorf("row %d: preserved memory below protected floor", i)
				}
			}
		} else {
			active := 0
			for s.Rows[active].Replicas == 0 {
				active++
			}
			high := ceil(new(big.Rat).Mul(new(big.Rat).Quo(new(big.Rat).Add(capacity, rat(1)), weights[active]), new(big.Rat).SetInt(grid)))
			for low.Cmp(high) < 0 {
				mid := new(big.Int).Rsh(new(big.Int).Add(new(big.Int).Add(low, high), big.NewInt(1)), 1)
				if charge(values(mid)).Cmp(capacity) <= 0 {
					low.Set(mid)
				} else {
					high.Sub(mid, big.NewInt(1))
				}
			}
			chosen = values(low)
			factor = new(big.Rat).SetFrac(low, grid).RatString()
			next = charge(values(new(big.Int).Add(low, big.NewInt(1)))).RatString()
		}
		total := charge(chosen)
		if total.Cmp(capacity) > 0 {
			return Result{}, fmt.Errorf("%s: allocation exceeds capacity", name)
		}
		steady, scheduler := new(big.Rat).Set(fixed), new(big.Rat).Set(fixed)
		for i, v := range chosen {
			if !v.IsInt() || !v.Num().IsInt64() || v.Sign() <= 0 {
				return Result{}, fmt.Errorf("row %d: allocation exceeds resolver range", i)
			}
			if cpu {
				allocations[i].CPUMilli = v.Num().Int64()
			} else {
				allocations[i].MemoryMiB = v.Num().Int64()
			}
			steady.Add(steady, new(big.Rat).Mul(v, rat(s.Rows[i].Replicas)))
		}
		for i, pod := range s.Pods {
			if cpu {
				result.Ledger.Pods = append(result.Ledger.Pods, LedgerPod{Pod: pod, Resources: map[string]Charge{}})
			}
			c, previous := podCharge(pod, chosen, cpu, s.Quota), podCharge(pod, old, cpu, s.Quota)
			c.OldCharge = previous.Charge
			c.ReplicatedChargeDelta = new(big.Rat).Mul(new(big.Rat).Sub(number(Number(c.Charge)), number(Number(previous.Charge))), rat(pod.Replicas)).RatString()
			result.Ledger.Pods[i].Resources[name] = c
			scheduler.Add(scheduler, new(big.Rat).Mul(number(Number(c.Request)), rat(pod.Replicas)))
		}
		offering[name] = total
		result.Ledger.Resources[name] = ResourceLedger{factor, capacity.RatString(), protected.RatString(), new(big.Rat).Sub(capacity, protected).RatString(), total.RatString(), charge(old).RatString(), new(big.Rat).Sub(capacity, total).RatString(), next, steady.RatString(), scheduler.RatString()}
	}
	p := sizing.Profile{Version: "v1", Name: s.Name, Description: "Offline budget candidate; startup and throughput unqualified.", Rounding: sizing.Budget{CPUMilli: 1, MemoryMiB: 1}, SafetyLimit: sizing.Ratio{Numerator: 11, Denominator: 10}, CPULimitEqualsRequest: true, GoMemLimitPercent: s.GoMemLimitPercent}
	present := make([]*big.Int, len(s.Rows))
	for i := range present {
		present[i] = new(big.Int)
	}
	for _, pod := range s.Pods {
		for _, i := range pod.Regular {
			present[i].Add(present[i], big.NewInt(pod.Replicas))
		}
		for _, init := range pod.Init {
			present[init.Row].Add(present[init.Row], big.NewInt(pod.Replicas))
		}
	}
	for i, row := range s.Rows {
		c := sizing.Container{Identity: row.Identity, Floor: allocations[i], CPURatio: sizing.Ratio{Denominator: 1}, MemoryRatio: sizing.Ratio{Denominator: 1}}
		if row.GoMaxProcs != nil {
			value := *row.GoMaxProcs
			c.GoMaxProcs = &value
		}
		if row.Identity == (sizing.Identity{Kind: "Deployment", Workload: "kube-apiserver", Container: "kube-apiserver"}) {
			p.Anchor = allocations[i]
			c.CPURatio.Numerator, c.MemoryRatio.Numerator = 1, 1
		}
		if *row.NonGo {
			zero := int64(0)
			c.GoMemoryLimitPercent = &zero
		}
		p.Containers = append(p.Containers, c)
		delta := sizing.Budget{CPUMilli: allocations[i].CPUMilli - row.OldRequests.CPUMilli, MemoryMiB: allocations[i].MemoryMiB - row.OldRequests.MemoryMiB}
		risks := []string{row.StartupNote}
		if present[i].Sign() == 0 {
			result.Ledger.MissingLayoutRows = append(result.Ledger.MissingLayoutRows, i)
			risks = append(risks, "Absent from saved Pod layouts; allocation is NOT charged startup coverage.")
		}
		if !*row.QuietObserved {
			risks = append(risks, "No quiet usage evidence; weights are not measured demand.")
		}
		if delta.CPUMilli < 0 || delta.MemoryMiB < 0 {
			risks = append(risks, "Donor reduction may expose CPU/deadline or memory failures.")
		}
		if row.QuietPeakMemoryMiB != nil && number(*row.QuietPeakMemoryMiB).Cmp(new(big.Rat).SetInt(ceil(new(big.Rat).Mul(rat(allocations[i].MemoryMiB), big.NewRat(11, 10))))) > 0 {
			risks = append(risks, "Memory limit below historical quiet charge peak (not a startup minimum).")
		}
		donors := map[string]string{"cpuMilli": new(big.Int).Mul(big.NewInt(max(0, -delta.CPUMilli)), big.NewInt(row.Replicas)).String(), "memoryMiB": new(big.Int).Mul(big.NewInt(max(0, -delta.MemoryMiB)), big.NewInt(row.Replicas)).String()}
		result.Ledger.Rows = append(result.Ledger.Rows, LedgerRow{row, present[i].String(), allocations[i], delta, donors, quota(rat(allocations[i].CPUMilli), s.Quota).RatString(), risks})
	}
	result.Profile = p
	var err error
	if result.Resolved, err = sizing.Resolve(p); err != nil {
		return Result{}, err
	}
	if result.HyperShift, err = sizing.ToHyperShift(result.Resolved, s.Fallback); err != nil {
		return Result{}, err
	}
	result.Ledger.ResolverVerified = true
	share := new(big.Rat).Quo(offering["cpuMilli"], rat(s.Node.CPUMilli))
	if mem := new(big.Rat).Quo(offering["memoryMiB"], rat(s.Node.MemoryMiB)); mem.Cmp(share) > 0 {
		share = mem
	}
	result.Ledger.CostUSDPerHour = new(big.Rat).Mul(number(s.Node.PriceUSDPerHour), share).RatString()
	// The existing cost API takes integer units. Round fractional quota upward
	// conservatively; retain the exact charge and cost in the budget ledger.
	amounts := []*big.Rat{number(s.BudgetUSDPerHour), number(s.Node.PriceUSDPerHour), offering["cpuMilli"], offering["memoryMiB"]}
	ints := make([]int64, 4)
	for i, amount := range amounts {
		if i < 2 {
			amount = new(big.Rat).Mul(amount, rat(1000000))
			if !amount.IsInt() {
				return Result{}, fmt.Errorf("cost API requires integral micro-USD")
			}
		}
		v := ceil(amount)
		if !v.IsInt64() {
			return Result{}, fmt.Errorf("cost input exceeds int64")
		}
		ints[i] = v.Int64()
	}
	result.CostSpec = cost.Spec{Version: "v1", Currency: "USD", FeeMicroUSDPerHCPHour: ints[0], TargetPercent: 100, SKUs: []cost.SKU{{Name: s.Node.SKU, Region: s.Node.Region, PriceMicroUSDPerHour: ints[1], CapacityBasis: "raw-illustration", RawCPUMilli: s.Node.CPUMilli, RawMemoryMiB: s.Node.MemoryMiB, PriceSourceURL: s.Node.SourceURL, PriceRetrievedAt: s.Node.RetrievedAt}}, DataPoints: []cost.DataPoint{{Name: s.Name + "-quota-and-init-accounted", Demand: cost.Demand{CPUMilli: ints[2], MemoryMiB: ints[3]}, SourceKind: "requests"}}}
	if result.Cost, err = cost.Calculate(result.CostSpec); err != nil {
		return Result{}, err
	}
	return result, nil
}

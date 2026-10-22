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

package sizing

import (
	"cmp"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// ToHyperShift renders ClusterSizingConfiguration/cluster for an isolated personal
// management cluster. It targets the experimental containerResourcePolicy schema
// in hypershift-perf-sizing/api/scheduling/v1alpha1, without importing HyperShift.
// The single small class covers [0,+inf); defaults cover unlisted containers only
// in projector-owned workloads, not downstream operators' workloads.
// Input must come from Resolve. Unrepresentable settings and ambiguous identities
// are errors, not silently dropped. No cluster access or input mutation occurs.
func ToHyperShift(p ResolvedProfile, defaults Budget) (map[string]any, error) {
	if p.Version != Version || p.GoMemLimitPercent < 1 || p.GoMemLimitPercent > 100 {
		return nil, fmt.Errorf("invalid resolved profile version or goMemLimitPercent")
	}
	if p.SafetyLimit.Denominator <= 0 || p.SafetyLimit.Numerator <= p.SafetyLimit.Denominator {
		return nil, fmt.Errorf("HyperShift requires a safetyLimit greater than one with a positive denominator")
	}
	percent, remainder := new(big.Int), new(big.Int)
	percent.QuoRem(new(big.Int).Mul(big.NewInt(p.SafetyLimit.Numerator), big.NewInt(100)), big.NewInt(p.SafetyLimit.Denominator), remainder)
	if remainder.Sign() != 0 || !percent.IsInt64() || percent.Int64() > 1600 {
		return nil, fmt.Errorf("HyperShift safetyLimit must be an integer percentage not exceeding 1600")
	}
	defaultLimit, err := rounded(defaults.MemoryMiB, p.SafetyLimit, 0, 1)
	if defaults.CPUMilli <= 0 || defaults.MemoryMiB <= 0 || err != nil || defaultLimit > math.MaxInt64/(1<<20) {
		return nil, fmt.Errorf("default requests must be positive and memory limit must fit int64 bytes")
	}
	if len(p.Containers) < 1 || len(p.Containers) > 256 {
		return nil, fmt.Errorf("HyperShift requires 1..256 container overrides")
	}
	quantity := func(value, suffix string) (int64, error) {
		n, err := strconv.ParseInt(strings.TrimSuffix(value, suffix), 10, 64)
		if err != nil || n <= 0 || value != strconv.FormatInt(n, 10)+suffix {
			return 0, fmt.Errorf("expected a positive integer quantity with %s suffix, got %q", suffix, value)
		}
		return n, nil
	}
	containers := slices.Clone(p.Containers)
	slices.SortFunc(containers, func(a, b ResolvedContainer) int {
		if order := cmp.Compare(a.Identity.Workload, b.Identity.Workload); order != 0 {
			return order
		}
		return cmp.Compare(a.Identity.Container, b.Identity.Container)
	})
	entries := make([]map[string]any, 0, len(containers))
	seen := map[[2]string]bool{}
	for _, c := range containers {
		id := c.Identity
		if len(id.Workload) == 0 || len(id.Workload) > 253 || len(id.Container) == 0 || len(id.Container) > 63 || strings.TrimSpace(id.Workload) != id.Workload || strings.TrimSpace(id.Container) != id.Container {
			return nil, fmt.Errorf("invalid HyperShift workload/container identity: %+v", id)
		}
		key := [2]string{id.Workload, id.Container}
		if seen[key] {
			return nil, fmt.Errorf("HyperShift cannot distinguish kind/init identities for %s/%s", key[0], key[1])
		}
		seen[key] = true
		if _, err := quantity(c.Requests.CPU, "m"); err != nil {
			return nil, err
		}
		memory, err := quantity(c.Requests.Memory, "Mi")
		if err != nil {
			return nil, err
		}
		limit, err := rounded(memory, p.SafetyLimit, 0, 1)
		if err != nil || limit > math.MaxInt64/(1<<20) || c.Limits.Memory != strconv.FormatInt(limit, 10)+"Mi" {
			return nil, fmt.Errorf("%s/%s: memory limit is not representable by HyperShift", key[0], key[1])
		}
		wantCPU := ""
		if p.CPULimitEqualsRequest {
			wantCPU = c.Requests.CPU
		}
		if c.Limits.CPU != wantCPU {
			return nil, fmt.Errorf("%s/%s: CPU limit does not match cpuLimitEqualsRequest", key[0], key[1])
		}
		entry := map[string]any{"workload": id.Workload, "container": id.Container, "requests": c.Requests}
		percent := p.GoMemLimitPercent
		if c.GoMemoryLimitPercent != nil {
			percent = *c.GoMemoryLimitPercent
			if percent < 0 || percent > 100 {
				return nil, fmt.Errorf("goMemoryLimitPercent must be in [0,100]")
			}
			entry["goMemoryLimitPercent"] = percent
		}
		goMemory := new(big.Int).Mul(big.NewInt(memory*(1<<20)), big.NewInt(percent))
		goMemory.Quo(goMemory, big.NewInt(100))
		wantMemory := ""
		if percent != 0 {
			wantMemory = goMemory.String() + "B"
		}
		if c.Env["GOMEMLIMIT"] != wantMemory {
			return nil, fmt.Errorf("%s/%s: GOMEMLIMIT does not match resolved percentage", key[0], key[1])
		}
		for name, value := range c.Env {
			switch name {
			case "GOMEMLIMIT":
			case "GOMAXPROCS":
				procs, err := strconv.ParseInt(value, 10, 64)
				if err != nil || procs < 1 || procs > 1024 {
					return nil, fmt.Errorf("HyperShift goMaxProcs must be in [1,1024]")
				}
				entry["goMaxProcs"] = procs
			default:
				return nil, fmt.Errorf("HyperShift cannot project environment variable %q", name)
			}
		}
		entries = append(entries, entry)
	}
	policy := map[string]any{
		"defaultRequests":      Quantities{CPU: strconv.FormatInt(defaults.CPUMilli, 10) + "m", Memory: strconv.FormatInt(defaults.MemoryMiB, 10) + "Mi"},
		"goMemoryLimitPercent": p.GoMemLimitPercent, "containers": entries,
	}
	if percent.Int64()%100 == 0 {
		policy["memoryLimitMultiplier"] = percent.Int64() / 100
	} else {
		policy["memoryLimitPercent"] = percent.Int64()
	}
	if p.CPULimitEqualsRequest {
		policy["cpuLimitEqualsRequest"] = true
	}
	return map[string]any{
		"apiVersion": "scheduling.hypershift.openshift.io/v1alpha1",
		"kind":       "ClusterSizingConfiguration",
		"metadata":   map[string]any{"name": "cluster"},
		"spec": map[string]any{
			"concurrency":     map[string]any{"limit": 5, "slidingWindow": "0s"},
			"transitionDelay": map[string]any{"increase": "0s", "decrease": "20m"},
			"sizes": []any{map[string]any{
				"name": "small", "criteria": map[string]any{"from": 0},
				"management": map[string]any{"placeholders": 0},
				"effects": map[string]any{
					"maximumRequestsInflight": 150, "maximumMutatingRequestsInflight": 50,
					"containerResourcePolicy": policy,
				},
			}},
		},
	}, nil
}

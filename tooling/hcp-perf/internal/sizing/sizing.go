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

// Package sizing resolves KAS-relative resource budgets into experiment
// configuration. ToHyperShift renders an experimental HyperShift policy;
// neither resolving nor rendering a profile applies it to a cluster.
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

// Version is the supported input and output schema version.
const Version = "v1"

// Budget uses integer millicores and mebibytes, never floating-point quantities.
type Budget struct {
	CPUMilli  int64 `json:"cpuMilli"`
	MemoryMiB int64 `json:"memoryMiB"`
}

// Ratio is an exact nonnegative numerator over a strictly positive denominator.
// Both fields must be supplied, including the denominator for a zero ratio.
type Ratio struct {
	Numerator   int64 `json:"numerator"`
	Denominator int64 `json:"denominator"`
}

// Identity identifies one container within a single control-plane namespace.
// Kind is the case-sensitive Kubernetes workload kind (for example Deployment).
// InitContainer distinguishes spec.initContainers from spec.containers.
type Identity struct {
	Kind          string `json:"kind"`
	Workload      string `json:"workload"`
	Container     string `json:"container"`
	InitContainer bool   `json:"initContainer,omitempty"`
}

// Container defines independent CPU and memory coefficients and lower bounds.
type Container struct {
	Identity    Identity `json:"identity"`
	CPURatio    Ratio    `json:"cpuRatio"`
	MemoryRatio Ratio    `json:"memoryRatio"`
	Floor       Budget   `json:"floor"`
	// GoMaxProcs is absent by default and is never derived from CPU. If set,
	// it must be in [1, MaxInt32] so it is portable across Go architectures.
	GoMaxProcs *int64 `json:"goMaxProcs,omitempty"`
	// GoMemoryLimitPercent overrides the profile percentage; zero omits GOMEMLIMIT.
	GoMemoryLimitPercent *int64 `json:"goMemoryLimitPercent,omitempty"`
}

// Profile is explicit sizing input; there are no implicit numeric defaults.
// A small baseline uses Anchor={250,1024}, Rounding={1,1}, SafetyLimit={3,1},
// and GoMemLimitPercent=80. Zero floors are allowed, but requests must be positive.
type Profile struct {
	Version     string `json:"version"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Anchor is the exact request budget for the required regular container
	// Deployment/kube-apiserver/kube-apiserver, whose ratios must both be one.
	Anchor Budget `json:"anchor"`
	// Rounding specifies positive quanta. Requests and memory limits round up
	// to multiples of these units; quanta must divide the anchor exactly.
	Rounding Budget `json:"rounding"`
	// SafetyLimit must be strictly greater than one. It scales only the resolved
	// memory request, independently of the Go runtime's soft memory limit.
	SafetyLimit Ratio `json:"safetyLimit"`
	// CPULimitEqualsRequest opts into CPU limits; omission preserves unlimited CPU.
	CPULimitEqualsRequest bool `json:"cpuLimitEqualsRequest,omitempty"`
	// GoMemLimitPercent must be in [1,100]. GOMEMLIMIT is this percentage of
	// the resolved memory request, rounded down to whole bytes.
	GoMemLimitPercent int64       `json:"goMemLimitPercent"`
	Containers        []Container `json:"containers"`
}

// Quantities contains explicit resource quantities with m and Mi suffixes.
type Quantities struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// MemoryLimit contains the memory safety limit and an optional CPU limit.
type MemoryLimit struct {
	Memory string `json:"memory"`
	CPU    string `json:"cpu,omitempty"`
}

// ResolvedContainer holds effective requests, a memory limit, and Go runtime env.
// Env includes GOMEMLIMIT unless disabled, and GOMAXPROCS only when requested.
type ResolvedContainer struct {
	Identity             Identity          `json:"identity"`
	Requests             Quantities        `json:"requests"`
	Limits               MemoryLimit       `json:"limits"`
	Env                  map[string]string `json:"env,omitempty"`
	GoMemoryLimitPercent *int64            `json:"goMemoryLimitPercent,omitempty"`
}

// ResolvedProfile is resolved experiment configuration, not a HyperShift API.
// Containers are sorted by kind, workload, container, then regular before init.
// Configuration fields record the effective policies used for resolution.
type ResolvedProfile struct {
	Version               string              `json:"version"`
	Profile               string              `json:"profile"`
	Description           string              `json:"description,omitempty"`
	Anchor                Budget              `json:"anchor"`
	Rounding              Budget              `json:"rounding"`
	SafetyLimit           Ratio               `json:"safetyLimit"`
	CPULimitEqualsRequest bool                `json:"cpuLimitEqualsRequest,omitempty"`
	GoMemLimitPercent     int64               `json:"goMemLimitPercent"`
	Containers            []ResolvedContainer `json:"containers"`
}

// Resolve validates and resolves a profile without mutating it or performing I/O.
// For each resource, request = ceilToQuantum(max(anchor*ratio, floor));
// memory limit = ceilToQuantum(memory request*safetyLimit). CPU is unlimited unless opted in.
// All arithmetic is exact and
// overflow is an error, including memory quantities not representable as int64
// bytes. Errors return an empty result, never a partially resolved profile.
func Resolve(p Profile) (ResolvedProfile, error) {
	if p.Version != Version {
		return ResolvedProfile{}, fmt.Errorf("version must be %q", Version)
	}
	if strings.TrimSpace(p.Name) == "" {
		return ResolvedProfile{}, fmt.Errorf("name must not be empty")
	}
	if p.Anchor.CPUMilli <= 0 || p.Anchor.MemoryMiB <= 0 {
		return ResolvedProfile{}, fmt.Errorf("anchor cpuMilli and memoryMiB must be positive")
	}
	if p.Rounding.CPUMilli <= 0 || p.Rounding.MemoryMiB <= 0 {
		return ResolvedProfile{}, fmt.Errorf("rounding cpuMilli and memoryMiB must be positive")
	}
	if p.Anchor.CPUMilli%p.Rounding.CPUMilli != 0 || p.Anchor.MemoryMiB%p.Rounding.MemoryMiB != 0 {
		return ResolvedProfile{}, fmt.Errorf("rounding quanta must divide the anchor exactly")
	}
	if p.SafetyLimit.Denominator <= 0 || p.SafetyLimit.Numerator <= p.SafetyLimit.Denominator {
		return ResolvedProfile{}, fmt.Errorf("safetyLimit must have a positive denominator and be greater than one")
	}
	if p.GoMemLimitPercent < 1 || p.GoMemLimitPercent > 100 {
		return ResolvedProfile{}, fmt.Errorf("goMemLimitPercent must be in [1,100]")
	}

	result := ResolvedProfile{
		Version: Version, Profile: p.Name, Description: p.Description,
		Anchor: p.Anchor, Rounding: p.Rounding, SafetyLimit: p.SafetyLimit,
		CPULimitEqualsRequest: p.CPULimitEqualsRequest,
		GoMemLimitPercent:     p.GoMemLimitPercent,
		Containers:            make([]ResolvedContainer, 0, len(p.Containers)),
	}
	kas := Identity{Kind: "Deployment", Workload: "kube-apiserver", Container: "kube-apiserver"}
	foundKAS := false
	seen := make(map[Identity]bool, len(p.Containers))
	for i, c := range p.Containers {
		id := c.Identity
		if strings.TrimSpace(id.Kind) == "" || strings.TrimSpace(id.Workload) == "" || strings.TrimSpace(id.Container) == "" {
			return ResolvedProfile{}, fmt.Errorf("containers[%d]: kind, workload, and container must not be empty", i)
		}
		if strings.TrimSpace(id.Kind) != id.Kind || strings.TrimSpace(id.Workload) != id.Workload || strings.TrimSpace(id.Container) != id.Container {
			return ResolvedProfile{}, fmt.Errorf("containers[%d]: identity must not contain surrounding whitespace", i)
		}
		if seen[id] {
			return ResolvedProfile{}, fmt.Errorf("containers[%d]: duplicate identity %+v", i, id)
		}
		seen[id] = true
		if c.GoMaxProcs != nil && (*c.GoMaxProcs < 1 || *c.GoMaxProcs > math.MaxInt32) {
			return ResolvedProfile{}, fmt.Errorf("containers[%d]: goMaxProcs must be in [1,2147483647]", i)
		}
		percent := p.GoMemLimitPercent
		var percentOverride *int64
		if c.GoMemoryLimitPercent != nil {
			percent = *c.GoMemoryLimitPercent
			if percent < 0 || percent > 100 {
				return ResolvedProfile{}, fmt.Errorf("containers[%d]: goMemoryLimitPercent must be in [0,100]", i)
			}
			percentOverride = &percent
		}
		var request Budget
		for _, resource := range []struct {
			name                   string
			anchor, floor, quantum int64
			ratio                  Ratio
			request                *int64
		}{
			{"cpuMilli", p.Anchor.CPUMilli, c.Floor.CPUMilli, p.Rounding.CPUMilli, c.CPURatio, &request.CPUMilli},
			{"memoryMiB", p.Anchor.MemoryMiB, c.Floor.MemoryMiB, p.Rounding.MemoryMiB, c.MemoryRatio, &request.MemoryMiB},
		} {
			if resource.ratio.Numerator < 0 || resource.ratio.Denominator <= 0 || resource.floor < 0 {
				return ResolvedProfile{}, fmt.Errorf("containers[%d] %s: numerator and floor must be nonnegative; denominator must be positive", i, resource.name)
			}
			if id == kas && (resource.ratio.Numerator != resource.ratio.Denominator || resource.floor > resource.anchor) {
				return ResolvedProfile{}, fmt.Errorf("containers[%d] %s: kube-apiserver must have unit ratio and floor no greater than anchor", i, resource.name)
			}
			value, err := rounded(resource.anchor, resource.ratio, resource.floor, resource.quantum)
			if err != nil {
				return ResolvedProfile{}, fmt.Errorf("containers[%d] %s request: %w", i, resource.name, err)
			}
			*resource.request = value
		}
		memoryLimit, err := rounded(request.MemoryMiB, p.SafetyLimit, 0, p.Rounding.MemoryMiB)
		if err != nil {
			return ResolvedProfile{}, fmt.Errorf("containers[%d] memoryMiB limit: %w", i, err)
		}
		if request.MemoryMiB > math.MaxInt64/(1<<20) || memoryLimit > math.MaxInt64/(1<<20) {
			return ResolvedProfile{}, fmt.Errorf("containers[%d]: memory quantity overflows int64 bytes", i)
		}
		// The percentage product may exceed int64 even when the final byte
		// count fits. Divide only after multiplying to retain fractional bytes.
		memory := new(big.Int).Mul(big.NewInt(request.MemoryMiB*(1<<20)), big.NewInt(percent))
		memory.Quo(memory, big.NewInt(100))
		env := map[string]string{}
		if percent != 0 {
			env["GOMEMLIMIT"] = memory.String() + "B"
		}
		if c.GoMaxProcs != nil {
			env["GOMAXPROCS"] = strconv.FormatInt(*c.GoMaxProcs, 10)
		}
		limits := MemoryLimit{Memory: strconv.FormatInt(memoryLimit, 10) + "Mi"}
		if p.CPULimitEqualsRequest {
			limits.CPU = strconv.FormatInt(request.CPUMilli, 10) + "m"
		}
		result.Containers = append(result.Containers, ResolvedContainer{
			Identity:             id,
			Requests:             Quantities{CPU: strconv.FormatInt(request.CPUMilli, 10) + "m", Memory: strconv.FormatInt(request.MemoryMiB, 10) + "Mi"},
			Limits:               limits,
			Env:                  env,
			GoMemoryLimitPercent: percentOverride,
		})
		foundKAS = foundKAS || id == kas
	}
	if !foundKAS {
		return ResolvedProfile{}, fmt.Errorf("containers must include regular Deployment/kube-apiserver/kube-apiserver with unit ratios")
	}
	slices.SortFunc(result.Containers, func(a, b ResolvedContainer) int {
		for _, pair := range [][2]string{{a.Identity.Kind, b.Identity.Kind}, {a.Identity.Workload, b.Identity.Workload}, {a.Identity.Container, b.Identity.Container}} {
			if order := cmp.Compare(pair[0], pair[1]); order != 0 {
				return order
			}
		}
		if a.Identity.InitContainer == b.Identity.InitContainer {
			return 0
		}
		if a.Identity.InitContainer {
			return 1
		}
		return -1
	})
	return result, nil
}

// rounded expects validated nonnegative inputs and positive denominator/quantum.
// Arbitrary precision intermediates permit large ratios that cancel exactly.
func rounded(anchor int64, ratio Ratio, floor, quantum int64) (int64, error) {
	numerator := new(big.Int).Mul(big.NewInt(anchor), big.NewInt(ratio.Numerator))
	denominator := big.NewInt(ratio.Denominator)
	minimum := new(big.Int).Mul(big.NewInt(floor), denominator)
	if numerator.Cmp(minimum) < 0 {
		numerator = minimum
	}
	denominator.Mul(denominator, big.NewInt(quantum))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	quotient.Mul(quotient, big.NewInt(quantum))
	if !quotient.IsInt64() {
		return 0, fmt.Errorf("rounded quantity overflows int64")
	}
	if quotient.Sign() <= 0 {
		return 0, fmt.Errorf("rounded quantity must be positive")
	}
	return quotient.Int64(), nil
}

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

// Package adversary plans bounded CPU competitors and executes registered runs
// using live Kubernetes placement and scheduler request commitments.
package adversary

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Spec describes a v1 snapshot and the bounds for one competitor per target node.
// Memory values are per-competitor MiB, not per runnable worker. Live discovery
// checks request headroom, not RSS or future memory pressure. Runtime image
// selection belongs to RunSpec. Optional observations are absent in legacy runs.
type Spec struct {
	Version         string            `json:"version"`
	CollectedAt     time.Time         `json:"collectedAt"`
	Nodes           []Node            `json:"nodes"`
	DurationSeconds int64             `json:"durationSeconds"`
	MemoryRequestMi int64             `json:"memoryRequestMi"`
	MemoryLimitMi   int64             `json:"memoryLimitMi"`
	CPUFillPercent  int32             `json:"cpuFillPercent,omitempty"`
	TargetPods      map[string]string `json:"targetPods,omitempty"` // Pod UID -> node name.
}

// Node records scheduler request commitments, not measured CPU usage.
// A collector must supply ALL scheduler-effective CPU requests, including
// init containers and pod overhead. Plan does not compute those commitments.
type Node struct {
	Name                   string `json:"name"`
	Pool                   string `json:"pool"`
	Role                   string `json:"role"`
	HostsTarget            bool   `json:"hostsTarget"`
	CapacityCPUMilli       int64  `json:"capacityCPUMilli"`
	AllocatableCPUMilli    int64  `json:"allocatableCPUMilli"`
	CommittedCPUMilli      int64  `json:"committedCPUMilli"`
	ReserveCPUMilli        int64  `json:"reserveCPUMilli"`
	UID                    string `json:"uid,omitempty"`
	AllocatableMemoryBytes int64  `json:"allocatableMemoryBytes,omitempty"`
	CommittedMemoryBytes   int64  `json:"committedMemoryBytes,omitempty"`
	AllocatablePods        int64  `json:"allocatablePods,omitempty"`
	CommittedPods          int64  `json:"committedPods,omitempty"`
}

// Result validates only the declared snapshot. CollectedAt is preserved without
// consulting a clock or judging freshness. No actual contention, schedulability,
// or exact CPU entitlement is asserted; runtime verification is still required.
type Result struct {
	Version     string     `json:"version"`
	CollectedAt time.Time  `json:"collectedAt"`
	Validity    string     `json:"validity"`
	Nodes       []NodePlan `json:"nodes"`
	// This is a request-headroom policy, not empirically calibrated contention.
	CPUFillPercent int32 `json:"cpuFillPercent,omitempty"`
}

// NodePlan specifies one CPU-only competitor with continuously runnable workers
// sized to the full host capacity, not merely the requested CPU. CPURequestMilli
// is a scheduling request, not an exact entitlement. There is intentionally no
// CPU limit: neither competitor nor HCP CPU limits are set by this planner.
// DeadlineSeconds is a required relative runtime bound, not a wall-clock date.
type NodePlan struct {
	Name            string `json:"name"`
	CPURequestMilli int64  `json:"cpuRequestMilli"`
	Workers         int64  `json:"workers"`
	DeadlineSeconds int64  `json:"deadlineSeconds"`
	MemoryRequestMi int64  `json:"memoryRequestMi"`
	MemoryLimitMi   int64  `json:"memoryLimitMi"`
}

// Plan validates every node, selects only workers hosting the target, and sorts
// the resulting plans by name without modifying spec. Any invalid or nonviable
// target fails the whole plan; no partial plan or overcommit fallback is returned.
func Plan(spec Spec) (Result, error) {
	if spec.Version != "v1" {
		return Result{}, fmt.Errorf("version must be v1, got %q", spec.Version)
	}
	if spec.CollectedAt.IsZero() {
		return Result{}, fmt.Errorf("collectedAt must be an explicit nonzero timestamp")
	}
	if _, err := spec.CollectedAt.MarshalJSON(); err != nil {
		return Result{}, fmt.Errorf("invalid collectedAt: %w", err)
	}
	if spec.DurationSeconds <= 0 || spec.DurationSeconds > 24*60*60 {
		return Result{}, fmt.Errorf("durationSeconds must be between 1 and 86400")
	}
	if spec.MemoryRequestMi <= 0 || spec.MemoryLimitMi < spec.MemoryRequestMi {
		return Result{}, fmt.Errorf("memoryRequestMi must be positive and memoryLimitMi must be at least memoryRequestMi")
	}
	if spec.CPUFillPercent < 0 || spec.CPUFillPercent > 100 {
		return Result{}, fmt.Errorf("cpuFillPercent must be 1..100 when specified")
	}
	result := Result{
		Version:        spec.Version,
		CollectedAt:    spec.CollectedAt,
		Validity:       "declared-snapshot-only",
		CPUFillPercent: spec.CPUFillPercent,
	}
	seen := make(map[string]bool, len(spec.Nodes))
	for _, node := range spec.Nodes {
		if node.Name == "" || strings.TrimSpace(node.Name) != node.Name {
			return Result{}, fmt.Errorf("node name must be nonempty without surrounding whitespace, got %q", node.Name)
		}
		if seen[node.Name] {
			return Result{}, fmt.Errorf("duplicate node name %q", node.Name)
		}
		seen[node.Name] = true
		if node.Pool == "" || strings.TrimSpace(node.Pool) != node.Pool {
			return Result{}, fmt.Errorf("node %q: pool must be nonempty without surrounding whitespace", node.Name)
		}
		switch node.Role {
		case "worker", "infra", "system":
		default:
			return Result{}, fmt.Errorf("node %q: invalid role %q; expected worker, infra, or system", node.Name, node.Role)
		}
		if node.CapacityCPUMilli <= 0 || node.AllocatableCPUMilli < 0 || node.AllocatableCPUMilli > node.CapacityCPUMilli {
			return Result{}, fmt.Errorf("node %q: capacityCPUMilli must be positive and allocatableCPUMilli must be between zero and capacity", node.Name)
		}
		if node.CommittedCPUMilli < 0 || node.CommittedCPUMilli > node.AllocatableCPUMilli {
			return Result{}, fmt.Errorf("node %q: committedCPUMilli must be between zero and allocatable CPU", node.Name)
		}
		if node.ReserveCPUMilli < 0 {
			return Result{}, fmt.Errorf("node %q: reserveCPUMilli must not be negative", node.Name)
		}
		if !node.HostsTarget {
			continue
		}
		if node.Role != "worker" {
			return Result{}, fmt.Errorf("node %q: target is hosted on %s, not a worker", node.Name, node.Role)
		}
		// Compare before subtracting to avoid overflow and reject clamped-zero
		// headroom instead of silently scheduling an overcommitted competitor.
		headroom := node.AllocatableCPUMilli - node.CommittedCPUMilli
		if headroom <= node.ReserveCPUMilli {
			return Result{}, fmt.Errorf("node %q: no viable contended plan: no CPU request headroom after safety reserve", node.Name)
		}
		workers := node.CapacityCPUMilli / 1000
		if node.CapacityCPUMilli%1000 != 0 {
			workers++
		}
		result.Nodes = append(result.Nodes, NodePlan{
			Name:            node.Name,
			CPURequestMilli: headroom - node.ReserveCPUMilli,
			Workers:         workers,
			DeadlineSeconds: spec.DurationSeconds,
			MemoryRequestMi: spec.MemoryRequestMi,
			MemoryLimitMi:   spec.MemoryLimitMi,
		})
	}
	if len(result.Nodes) == 0 {
		return Result{}, fmt.Errorf("no viable contended plan: no worker nodes host the target")
	}
	sort.Slice(result.Nodes, func(i, j int) bool { return result.Nodes[i].Name < result.Nodes[j].Name })
	return result, nil
}

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

package adversary

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func validSpec() Spec {
	return Spec{
		Version: "v1", CollectedAt: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
		DurationSeconds: 300, MemoryRequestMi: 16, MemoryLimitMi: 32,
		Nodes: []Node{{
			Name: "worker-a", Pool: "workers", Role: "worker", HostsTarget: true,
			CapacityCPUMilli: 8000, AllocatableCPUMilli: 7500,
			CommittedCPUMilli: 6000, ReserveCPUMilli: 500,
		}},
	}
}

func TestPlanExample(t *testing.T) {
	data, err := os.ReadFile("../../examples/nodes.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec Spec
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Plan(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := Result{
		Version: "v1", CollectedAt: spec.CollectedAt, Validity: "declared-snapshot-only",
		Nodes: []NodePlan{
			{Name: "worker-a", CPURequestMilli: 1000, Workers: 8, DeadlineSeconds: 300, MemoryRequestMi: 16, MemoryLimitMi: 32},
			{Name: "worker-b", CPURequestMilli: 5000, Workers: 16, DeadlineSeconds: 300, MemoryRequestMi: 16, MemoryLimitMi: 32},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %#v, want %#v", got, want)
	}
	after, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("Plan mutated input")
	}
	for i, j := 0, len(spec.Nodes)-1; i < j; i, j = i+1, j-1 {
		spec.Nodes[i], spec.Nodes[j] = spec.Nodes[j], spec.Nodes[i]
	}
	reordered, err := Plan(spec)
	if err != nil || !reflect.DeepEqual(got, reordered) {
		t.Fatalf("input ordering changed plan: %#v, %v", reordered, err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Nodes []map[string]json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	for _, node := range wire.Nodes {
		if len(node) != 6 {
			t.Fatalf("unexpected node plan fields (CPU limits must be absent): %s", encoded)
		}
		for _, key := range []string{"name", "cpuRequestMilli", "workers", "deadlineSeconds", "memoryRequestMi", "memoryLimitMi"} {
			if _, ok := node[key]; !ok {
				t.Errorf("missing field %q in %s", key, encoded)
			}
		}
	}
}

func TestPlanInvalid(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Spec)
		want   string
	}{
		{"missing version", func(s *Spec) { s.Version = "" }, "version"},
		{"unknown version", func(s *Spec) { s.Version = "v2" }, "version"},
		{"missing timestamp", func(s *Spec) { s.CollectedAt = time.Time{} }, "collectedAt"},
		{"invalid timestamp", func(s *Spec) { s.CollectedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }, "collectedAt"},
		{"zero duration", func(s *Spec) { s.DurationSeconds = 0 }, "durationSeconds"},
		{"negative duration", func(s *Spec) { s.DurationSeconds = -1 }, "durationSeconds"},
		{"unbounded duration", func(s *Spec) { s.DurationSeconds = 86401 }, "durationSeconds"},
		{"extreme duration", func(s *Spec) { s.DurationSeconds = math.MaxInt64 }, "durationSeconds"},
		{"missing memory request", func(s *Spec) { s.MemoryRequestMi = 0 }, "memoryRequestMi"},
		{"negative memory request", func(s *Spec) { s.MemoryRequestMi = -1 }, "memoryRequestMi"},
		{"missing memory limit", func(s *Spec) { s.MemoryLimitMi = 0 }, "memoryLimitMi"},
		{"negative memory limit", func(s *Spec) { s.MemoryLimitMi = -1 }, "memoryLimitMi"},
		{"memory request exceeds limit", func(s *Spec) { s.MemoryLimitMi = 15 }, "memoryLimitMi"},
		{"empty nodes", func(s *Spec) { s.Nodes = nil }, "no worker nodes host the target"},
		{"no targets", func(s *Spec) { s.Nodes[0].HostsTarget = false }, "no worker nodes host the target"},
		{"empty name", func(s *Spec) { s.Nodes[0].Name = "" }, "node name"},
		{"whitespace name", func(s *Spec) { s.Nodes[0].Name = " worker-a" }, "node name"},
		{"empty pool", func(s *Spec) { s.Nodes[0].Pool = "" }, `node "worker-a": pool`},
		{"whitespace pool", func(s *Spec) { s.Nodes[0].Pool = " " }, "pool"},
		{"duplicate", func(s *Spec) { s.Nodes = append(s.Nodes, s.Nodes[0]) }, `duplicate node name "worker-a"`},
		{"invalid role", func(s *Spec) { s.Nodes[0].Role = "Worker" }, "invalid role"},
		{"missing role", func(s *Spec) { s.Nodes[0].Role = "" }, "invalid role"},
		{"infra target", func(s *Spec) { s.Nodes[0].Role = "infra" }, `node "worker-a": target is hosted on infra`},
		{"system target", func(s *Spec) { s.Nodes[0].Role = "system" }, "target is hosted on system"},
		{"zero capacity", func(s *Spec) { s.Nodes[0].CapacityCPUMilli = 0 }, "capacityCPUMilli"},
		{"negative capacity", func(s *Spec) { s.Nodes[0].CapacityCPUMilli = -1 }, "capacityCPUMilli"},
		{"negative allocatable", func(s *Spec) { s.Nodes[0].AllocatableCPUMilli = -1 }, "allocatableCPUMilli"},
		{"allocatable exceeds capacity", func(s *Spec) { s.Nodes[0].AllocatableCPUMilli = 8001 }, "allocatableCPUMilli"},
		{"negative commitments", func(s *Spec) { s.Nodes[0].CommittedCPUMilli = -1 }, "committedCPUMilli"},
		{"overcommitted", func(s *Spec) { s.Nodes[0].CommittedCPUMilli = 7501 }, "committedCPUMilli"},
		{"negative reserve", func(s *Spec) { s.Nodes[0].ReserveCPUMilli = -1 }, "reserveCPUMilli"},
		{"exact reserve", func(s *Spec) { s.Nodes[0].ReserveCPUMilli = 1500 }, `node "worker-a": no viable contended plan`},
		{"excess reserve", func(s *Spec) { s.Nodes[0].ReserveCPUMilli = 1501 }, "no CPU request headroom"},
		{"extreme reserve", func(s *Spec) { s.Nodes[0].ReserveCPUMilli = math.MaxInt64 }, "no CPU request headroom"},
		{"fully committed", func(s *Spec) { s.Nodes[0].CommittedCPUMilli = 7500 }, "no CPU request headroom"},
		{"no allocatable", func(s *Spec) { s.Nodes[0].AllocatableCPUMilli = 0; s.Nodes[0].CommittedCPUMilli = 0 }, "no CPU request headroom"},
		{"invalid unselected node", func(s *Spec) {
			s.Nodes = append(s.Nodes, Node{Name: "unused", Pool: "infra", Role: "invalid"})
		}, `node "unused": invalid role`},
		{"one nonviable target", func(s *Spec) {
			n := s.Nodes[0]
			n.Name, n.ReserveCPUMilli = "worker-b", 1500
			s.Nodes = append(s.Nodes, n)
		}, `node "worker-b": no viable contended plan`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validSpec()
			tt.change(&spec)
			got, err := Plan(spec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
			if !reflect.DeepEqual(got, Result{}) {
				t.Fatalf("returned partial plan on error: %#v", got)
			}
		})
	}
}

func TestPlanBoundaries(t *testing.T) {
	for _, capacity := range []int64{1, 999, 1000, 1001, 8500, math.MaxInt64} {
		t.Run(strconv.FormatInt(capacity, 10), func(t *testing.T) {
			spec := validSpec()
			spec.DurationSeconds = 86400
			spec.MemoryLimitMi = spec.MemoryRequestMi
			n := &spec.Nodes[0]
			n.CapacityCPUMilli, n.AllocatableCPUMilli = capacity, capacity
			n.CommittedCPUMilli, n.ReserveCPUMilli = capacity-1, 0
			got, err := Plan(spec)
			if err != nil {
				t.Fatal(err)
			}
			if got.Nodes[0].Workers != (capacity-1)/1000+1 || got.Nodes[0].CPURequestMilli != 1 {
				t.Fatalf("incorrect rounding or headroom: %#v", got.Nodes[0])
			}
			if got.CollectedAt != spec.CollectedAt || got.Validity != "declared-snapshot-only" {
				t.Fatalf("old snapshot metadata not preserved: %#v", got)
			}
			spec.CollectedAt = time.Date(2099, 1, 1, 0, 0, 0, 0, time.FixedZone("offset", 3600))
			spec.DurationSeconds = 1
			n.CommittedCPUMilli = 0
			got, err = Plan(spec)
			if err != nil || got.CollectedAt != spec.CollectedAt || got.Nodes[0].CPURequestMilli != capacity {
				t.Fatalf("future snapshot or zero commitments rejected: %#v, %v", got, err)
			}
		})
	}
}

func TestSpecRejectsNonIntegralNumbers(t *testing.T) {
	for _, number := range []string{"NaN", "Infinity", "1.5", "9223372036854775808", "1e309"} {
		for _, field := range []string{"durationSeconds", "memoryRequestMi", "memoryLimitMi"} {
			var spec Spec
			if err := json.Unmarshal([]byte(`{"`+field+`":`+number+`}`), &spec); err == nil {
				t.Errorf("accepted %s for %s", number, field)
			}
		}
	}
}

func FuzzPlanCPU(f *testing.F) {
	f.Add(int64(8000), int64(7500), int64(6000), int64(500))
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64), int64(0), int64(0))
	f.Add(int64(1), int64(1), int64(0), int64(1))
	f.Fuzz(func(t *testing.T, capacity, allocatable, committed, reserve int64) {
		spec := validSpec()
		n := &spec.Nodes[0]
		n.CapacityCPUMilli, n.AllocatableCPUMilli = capacity, allocatable
		n.CommittedCPUMilli, n.ReserveCPUMilli = committed, reserve
		got, err := Plan(spec)
		viable := capacity > 0 && allocatable >= 0 && allocatable <= capacity &&
			committed >= 0 && committed <= allocatable && reserve >= 0 && allocatable-committed > reserve
		if !viable {
			if err == nil || !reflect.DeepEqual(got, Result{}) {
				t.Fatalf("invalid snapshot returned plan: %#v, %v", got, err)
			}
			return
		}
		if err != nil || len(got.Nodes) != 1 {
			t.Fatalf("viable snapshot rejected: %#v, %v", got, err)
		}
		plan := got.Nodes[0]
		if plan.CPURequestMilli != allocatable-committed-reserve || plan.Workers != (capacity-1)/1000+1 {
			t.Fatalf("incorrect CPU arithmetic: %#v", plan)
		}
	})
}

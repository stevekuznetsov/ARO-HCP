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
	"encoding/json"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func baseline() Profile {
	return Profile{
		Version: Version, Name: "test", Anchor: Budget{250, 1024},
		Rounding: Budget{1, 1}, SafetyLimit: Ratio{3, 1}, GoMemLimitPercent: 80,
		Containers: []Container{{
			Identity: Identity{Kind: "Deployment", Workload: "kube-apiserver", Container: "kube-apiserver"},
			CPURatio: Ratio{1, 1}, MemoryRatio: Ratio{1, 1},
		}},
	}
}

func TestResolveBudgets(t *testing.T) {
	for _, tt := range []struct {
		name                string
		cpu, memory, safety Ratio
		floor, quantum      Budget
		requests            Quantities
		limits              MemoryLimit
		percent             int64
		goMemLimit          string
	}{
		{"anchor", Ratio{1, 1}, Ratio{1, 1}, Ratio{3, 1}, Budget{}, Budget{1, 1}, Quantities{"250m", "1024Mi"}, MemoryLimit{Memory: "3072Mi"}, 80, "858993459B"},
		{"independent ratios", Ratio{1, 5}, Ratio{3, 4}, Ratio{3, 1}, Budget{}, Budget{1, 1}, Quantities{"50m", "768Mi"}, MemoryLimit{Memory: "2304Mi"}, 80, "644245094B"},
		{"ceil fractions", Ratio{1, 3}, Ratio{1, 3}, Ratio{3, 1}, Budget{}, Budget{1, 1}, Quantities{"84m", "342Mi"}, MemoryLimit{Memory: "1026Mi"}, 100, "358612992B"},
		{"ceil quanta and rational safety", Ratio{1, 3}, Ratio{1, 3}, Ratio{3, 2}, Budget{}, Budget{10, 16}, Quantities{"90m", "352Mi"}, MemoryLimit{Memory: "528Mi"}, 80, "295279001B"},
		{"CPU floor only", Ratio{1, 10}, Ratio{1, 2}, Ratio{3, 1}, Budget{51, 100}, Budget{10, 16}, Quantities{"60m", "512Mi"}, MemoryLimit{Memory: "1536Mi"}, 80, "429496729B"},
		{"memory floor only", Ratio{1, 2}, Ratio{1, 10}, Ratio{3, 1}, Budget{10, 201}, Budget{10, 16}, Quantities{"130m", "208Mi"}, MemoryLimit{Memory: "624Mi"}, 80, "174483046B"},
		{"zero ratios with floors", Ratio{0, 1}, Ratio{0, 1}, Ratio{3, 1}, Budget{1, 1}, Budget{1, 1}, Quantities{"1m", "1Mi"}, MemoryLimit{Memory: "3Mi"}, 1, "10485B"},
		{"large cancelling ratios", Ratio{math.MaxInt64, math.MaxInt64}, Ratio{math.MaxInt64, math.MaxInt64}, Ratio{math.MaxInt64, math.MaxInt64 - 1}, Budget{}, Budget{1, 1}, Quantities{"250m", "1024Mi"}, MemoryLimit{Memory: "1025Mi"}, 80, "858993459B"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := baseline()
			p.Rounding, p.SafetyLimit, p.GoMemLimitPercent = tt.quantum, tt.safety, tt.percent
			p.Containers = append(p.Containers, Container{
				Identity: Identity{Kind: "Deployment", Workload: "other", Container: "main"},
				CPURatio: tt.cpu, MemoryRatio: tt.memory, Floor: tt.floor,
			})
			r, err := Resolve(p)
			if err != nil {
				t.Fatal(err)
			}
			got := r.Containers[1]
			if got.Requests != tt.requests || got.Limits != tt.limits || got.Env["GOMEMLIMIT"] != tt.goMemLimit {
				t.Fatalf("got %+v; want requests %+v, limits %+v, GOMEMLIMIT %s", got, tt.requests, tt.limits, tt.goMemLimit)
			}
			for _, c := range r.Containers {
				encoded, err := json.Marshal(c.Limits)
				if err != nil {
					t.Fatal(err)
				}
				if string(encoded) != `{"memory":"`+c.Limits.Memory+`"}` {
					t.Fatalf("limits must contain only memory: %s", encoded)
				}
			}
			if _, ok := got.Env["GOMAXPROCS"]; ok {
				t.Fatal("GOMAXPROCS must not be derived from CPU")
			}
			if r.Containers[0].Requests != (Quantities{"250m", "1024Mi"}) {
				t.Fatalf("KAS anchor changed: %+v", r.Containers[0])
			}
		})
	}
}

func TestResolveInvalid(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{"missing version", func(p *Profile) { p.Version = "" }, "version"},
		{"unknown version", func(p *Profile) { p.Version = "v2" }, "version"},
		{"empty name", func(p *Profile) { p.Name = " \t" }, "name"},
		{"zero CPU anchor", func(p *Profile) { p.Anchor.CPUMilli = 0 }, "anchor"},
		{"negative CPU anchor", func(p *Profile) { p.Anchor.CPUMilli = math.MinInt64 }, "anchor"},
		{"zero memory anchor", func(p *Profile) { p.Anchor.MemoryMiB = 0 }, "anchor"},
		{"negative memory anchor", func(p *Profile) { p.Anchor.MemoryMiB = -1 }, "anchor"},
		{"missing rounding", func(p *Profile) { p.Rounding = Budget{} }, "rounding"},
		{"negative CPU quantum", func(p *Profile) { p.Rounding.CPUMilli = -1 }, "rounding"},
		{"negative memory quantum", func(p *Profile) { p.Rounding.MemoryMiB = math.MinInt64 }, "rounding"},
		{"inexact CPU anchor", func(p *Profile) { p.Rounding.CPUMilli = 3 }, "divide the anchor"},
		{"inexact memory anchor", func(p *Profile) { p.Rounding.MemoryMiB = 3 }, "divide the anchor"},
		{"missing safety", func(p *Profile) { p.SafetyLimit = Ratio{} }, "safetyLimit"},
		{"zero safety denominator", func(p *Profile) { p.SafetyLimit.Denominator = 0 }, "safetyLimit"},
		{"negative safety denominator", func(p *Profile) { p.SafetyLimit.Denominator = -1 }, "safetyLimit"},
		{"negative safety numerator", func(p *Profile) { p.SafetyLimit.Numerator = math.MinInt64 }, "safetyLimit"},
		{"safety equals one", func(p *Profile) { p.SafetyLimit = Ratio{3, 3} }, "safetyLimit"},
		{"safety below one", func(p *Profile) { p.SafetyLimit = Ratio{1, 2} }, "safetyLimit"},
		{"zero percent", func(p *Profile) { p.GoMemLimitPercent = 0 }, "goMemLimitPercent"},
		{"negative percent", func(p *Profile) { p.GoMemLimitPercent = math.MinInt64 }, "goMemLimitPercent"},
		{"percent above 100", func(p *Profile) { p.GoMemLimitPercent = 101 }, "goMemLimitPercent"},
		{"max percent", func(p *Profile) { p.GoMemLimitPercent = math.MaxInt64 }, "goMemLimitPercent"},
		{"missing containers", func(p *Profile) { p.Containers = nil }, "must include"},
		{"missing KAS", func(p *Profile) { p.Containers[0].Identity.Workload = "other" }, "must include"},
		{"KAS init is not anchor", func(p *Profile) { p.Containers[0].Identity.InitContainer = true }, "must include"},
		{"KAS kind is not anchor", func(p *Profile) { p.Containers[0].Identity.Kind = "StatefulSet" }, "must include"},
		{"empty kind", func(p *Profile) { p.Containers[0].Identity.Kind = "" }, "must not be empty"},
		{"empty workload", func(p *Profile) { p.Containers[0].Identity.Workload = " " }, "must not be empty"},
		{"empty container", func(p *Profile) { p.Containers[0].Identity.Container = "\t" }, "must not be empty"},
		{"identity whitespace", func(p *Profile) { p.Containers[0].Identity.Container += " " }, "whitespace"},
		{"duplicate", func(p *Profile) { p.Containers = append(p.Containers, p.Containers[0]) }, "duplicate"},
		{"missing CPU ratio", func(p *Profile) { p.Containers[0].CPURatio = Ratio{} }, "denominator"},
		{"zero memory denominator", func(p *Profile) { p.Containers[0].MemoryRatio.Denominator = 0 }, "denominator"},
		{"negative CPU denominator", func(p *Profile) { p.Containers[0].CPURatio.Denominator = math.MinInt64 }, "denominator"},
		{"negative memory denominator", func(p *Profile) { p.Containers[0].MemoryRatio.Denominator = -1 }, "denominator"},
		{"negative CPU numerator", func(p *Profile) { p.Containers[0].CPURatio.Numerator = -1 }, "numerator"},
		{"negative memory numerator", func(p *Profile) { p.Containers[0].MemoryRatio.Numerator = math.MinInt64 }, "numerator"},
		{"negative CPU floor", func(p *Profile) { p.Containers[0].Floor.CPUMilli = -1 }, "floor"},
		{"negative memory floor", func(p *Profile) { p.Containers[0].Floor.MemoryMiB = math.MinInt64 }, "floor"},
		{"KAS CPU ratio", func(p *Profile) { p.Containers[0].CPURatio = Ratio{1, 2} }, "unit ratio"},
		{"KAS memory ratio", func(p *Profile) { p.Containers[0].MemoryRatio = Ratio{2, 1} }, "unit ratio"},
		{"KAS CPU floor", func(p *Profile) { p.Containers[0].Floor.CPUMilli = 251 }, "floor no greater"},
		{"KAS memory floor", func(p *Profile) { p.Containers[0].Floor.MemoryMiB = 1025 }, "floor no greater"},
		{"zero GOMAXPROCS", func(p *Profile) { p.Containers[0].GoMaxProcs = new(int64) }, "goMaxProcs"},
		{"negative GOMAXPROCS", func(p *Profile) { v := int64(-1); p.Containers[0].GoMaxProcs = &v }, "goMaxProcs"},
		{"overflow GOMAXPROCS", func(p *Profile) { v := int64(math.MaxInt64); p.Containers[0].GoMaxProcs = &v }, "goMaxProcs"},
		{"memory limit overflow", func(p *Profile) { p.Anchor.MemoryMiB = math.MaxInt64 }, "memoryMiB limit"},
		{"memory byte overflow", func(p *Profile) { p.Anchor.MemoryMiB = math.MaxInt64 / (1 << 20) }, "overflows int64 bytes"},
		{"safety product overflow", func(p *Profile) { p.SafetyLimit.Numerator = math.MaxInt64 }, "overflows int64"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := baseline()
			tt.mutate(&p)
			got, err := Resolve(p)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got result %+v, error %v; want error containing %q", got, err, tt.want)
			}
			if !reflect.DeepEqual(got, ResolvedProfile{}) {
				t.Fatalf("error returned partial result: %+v", got)
			}
		})
	}
}

func TestRoundedExtremes(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		anchor, floor, quantum int64
		ratio                  Ratio
		want                   int64
		wantErr                bool
	}{
		{"max cancellation", math.MaxInt64, 0, 1, Ratio{math.MaxInt64, math.MaxInt64}, math.MaxInt64, false},
		{"max floor product", 1, math.MaxInt64, 1, Ratio{1, math.MaxInt64}, math.MaxInt64, false},
		{"max denominator quantum product", math.MaxInt64, 0, math.MaxInt64, Ratio{math.MaxInt64, math.MaxInt64}, math.MaxInt64, false},
		{"tiny positive rounds up", 1, 0, 1, Ratio{1, math.MaxInt64}, 1, false},
		{"multiply overflow", math.MaxInt64, 0, 1, Ratio{math.MaxInt64, 1}, 0, true},
		{"rounding overflow", math.MaxInt64, 0, 2, Ratio{1, 1}, 0, true},
		{"floor rounding overflow", 1, math.MaxInt64, 2, Ratio{0, 1}, 0, true},
		{"zero without floor", 1, 0, 1, Ratio{0, 1}, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rounded(tt.anchor, tt.ratio, tt.floor, tt.quantum)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("got %d, %v; want %d, error=%t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestResolveLargeMemoryPercentage(t *testing.T) {
	p := baseline()
	p.Anchor.MemoryMiB = math.MaxInt64/(1<<20) - 1
	p.SafetyLimit = Ratio{math.MaxInt64, math.MaxInt64 - 1}
	p.GoMemLimitPercent = 100
	r, err := Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Containers[0].Env["GOMEMLIMIT"]; got != "9223372036852678656B" {
		t.Fatalf("large percentage multiplication: got %s", got)
	}
}

func TestResolveNoCPUSafetyOverflow(t *testing.T) {
	p := baseline()
	p.Anchor.CPUMilli = math.MaxInt64
	r, err := Resolve(p)
	if err != nil {
		t.Fatalf("valid CPU request must not be multiplied by safetyLimit: %v", err)
	}
	got := r.Containers[0]
	if got.Requests.CPU != "9223372036854775807m" || got.Limits.Memory != "3072Mi" {
		t.Fatalf("unexpected request or memory limit: %+v", got)
	}
}

func TestResolveInvalidNonAnchor(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*Container)
		want   string
	}{
		{"CPU request overflow", func(c *Container) { c.CPURatio.Numerator = math.MaxInt64 }, "cpuMilli request"},
		{"memory request overflow", func(c *Container) { c.MemoryRatio.Numerator = math.MaxInt64 }, "memoryMiB request"},
		{"CPU floor rounding overflow", func(c *Container) { c.Floor.CPUMilli = math.MaxInt64 }, "cpuMilli request"},
		{"memory floor rounding overflow", func(c *Container) { c.Floor.MemoryMiB = math.MaxInt64 }, "memoryMiB request"},
		{"zero CPU request", func(c *Container) { c.CPURatio.Numerator = 0 }, "must be positive"},
		{"zero memory request", func(c *Container) { c.MemoryRatio.Numerator = 0 }, "must be positive"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := baseline()
			p.Rounding = Budget{10, 16}
			c := p.Containers[0]
			c.Identity.Workload = "other"
			tt.mutate(&c)
			p.Containers = append(p.Containers, c)
			got, err := Resolve(p)
			if err == nil || !strings.Contains(err.Error(), tt.want) || !reflect.DeepEqual(got, ResolvedProfile{}) {
				t.Fatalf("got %+v, %v; want empty result and error containing %q", got, err, tt.want)
			}
		})
	}
}

func TestResolveExplicitGoMaxProcs(t *testing.T) {
	for _, value := range []int64{1, 2, math.MaxInt32} {
		p := baseline()
		p.Containers[0].GoMaxProcs = &value
		r, err := Resolve(p)
		if err != nil {
			t.Fatal(err)
		}
		var got int64
		if err := json.Unmarshal([]byte(r.Containers[0].Env["GOMAXPROCS"]), &got); err != nil || got != value {
			t.Fatalf("GOMAXPROCS: got %d, %v; want %d", got, err, value)
		}
	}
}

func TestResolveDeterminismAndIsolation(t *testing.T) {
	p := baseline()
	procs := int64(2)
	p.Containers[0].GoMaxProcs = &procs
	for _, id := range []Identity{
		{Kind: "StatefulSet", Workload: "kube-apiserver", Container: "kube-apiserver"},
		{Kind: "Deployment", Workload: "kube-apiserver", Container: "kube-apiserver", InitContainer: true},
		{Kind: "Deployment", Workload: "aaa", Container: "zzz"},
		{Kind: "Deployment", Workload: "aaa", Container: "aaa"},
	} {
		c := p.Containers[0]
		c.Identity, c.GoMaxProcs = id, nil
		p.Containers = append(p.Containers, c)
	}
	before, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(p)
	if string(before) != string(after) || procs != 2 {
		t.Fatal("Resolve mutated its input")
	}
	wantOrder := []Identity{p.Containers[4].Identity, p.Containers[3].Identity, p.Containers[0].Identity, p.Containers[2].Identity, p.Containers[1].Identity}
	for i, c := range first.Containers {
		if c.Identity != wantOrder[i] {
			t.Fatalf("container %d: got %+v, want %+v", i, c.Identity, wantOrder[i])
		}
	}
	if first.Containers[2].Env["GOMAXPROCS"] != "2" {
		t.Fatal("explicit GOMAXPROCS was not retained")
	}
	encoded, _ := json.Marshal(first)
	for i := 0; i < len(p.Containers)*2; i++ {
		p.Containers = append(p.Containers[1:], p.Containers[0])
		if i%2 == 0 {
			slices.Reverse(p.Containers)
		}
		r, err := Resolve(p)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(r)
		if string(got) != string(encoded) {
			t.Fatalf("input order changed output:\n%s\n%s", encoded, got)
		}
	}
	first.Containers[2].Env["GOMAXPROCS"] = "99"
	first.Containers[0].Env["GOMEMLIMIT"] = "1B"
	next, err := Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(next)
	if string(got) != string(encoded) || procs != 2 {
		t.Fatal("result aliases input or another result")
	}
}

func TestSmallExample(t *testing.T) {
	data, err := os.ReadFile("../../examples/small.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var p Profile
	if err := decoder.Decode(&p); err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Containers) != 5 || !strings.Contains(r.Description, "INCOMPLETE") || r.Profile != "small" || r.Version != Version {
		t.Fatalf("unexpected example metadata: %+v", r)
	}
	if r.Anchor != p.Anchor || r.Rounding != p.Rounding || r.SafetyLimit != p.SafetyLimit || r.GoMemLimitPercent != p.GoMemLimitPercent {
		t.Fatal("effective policies were not retained")
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "GOMAXPROCS") {
		t.Fatal("example must leave GOMAXPROCS unset")
	}
}

func TestJSONRejectsNonIntegers(t *testing.T) {
	for _, value := range []string{"1.5", "NaN", "Infinity", "9223372036854775808", "-9223372036854775809", "\"250\""} {
		t.Run(value, func(t *testing.T) {
			var p Profile
			if err := json.Unmarshal([]byte(`{"anchor":{"cpuMilli":`+value+`}}`), &p); err == nil {
				t.Fatalf("accepted non-int64 value %s", value)
			}
		})
	}
}

func FuzzResolveIntegers(f *testing.F) {
	f.Add(int64(250), int64(1), int64(1), int64(0), int64(1))
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64))
	f.Add(int64(math.MinInt64), int64(-1), int64(0), int64(-1), int64(0))
	f.Fuzz(func(t *testing.T, anchor, numerator, denominator, floor, quantum int64) {
		p := baseline()
		p.Anchor.CPUMilli, p.Rounding.CPUMilli = anchor, quantum
		p.Containers = append(p.Containers, Container{
			Identity: Identity{Kind: "Deployment", Workload: "other", Container: "main"},
			CPURatio: Ratio{numerator, denominator}, MemoryRatio: Ratio{numerator, denominator}, Floor: Budget{floor, floor},
		})
		r, err := Resolve(p)
		if err != nil {
			if !reflect.DeepEqual(r, ResolvedProfile{}) {
				t.Fatal("error returned partial result")
			}
			return
		}
		again, err := Resolve(p)
		if err != nil || !reflect.DeepEqual(r, again) {
			t.Fatal("resolution is not deterministic")
		}
	})
}

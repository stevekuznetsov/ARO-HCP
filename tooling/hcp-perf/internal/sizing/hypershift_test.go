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
	"strconv"
	"strings"
	"testing"
)

func TestToHyperShiftJSON(t *testing.T) {
	p := baseline()
	percent, procs := int64(0), int64(1024)
	helper := p.Containers[0]
	helper.Identity = Identity{Kind: "Deployment", Workload: "helper", Container: "init", InitContainer: true}
	helper.GoMemoryLimitPercent, helper.GoMaxProcs = &percent, &procs
	p.Containers = append(p.Containers, helper)
	r, err := Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(r)
	got, err := ToHyperShift(r, Budget{10, 64})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	// Exact wire contract, intentionally without a dependency on HyperShift types.
	want := `{
	  "apiVersion":"scheduling.hypershift.openshift.io/v1alpha1",
	  "kind":"ClusterSizingConfiguration","metadata":{"name":"cluster"},
	  "spec":{
	    "concurrency":{"limit":5,"slidingWindow":"0s"},
	    "transitionDelay":{"increase":"0s","decrease":"20m"},
	    "sizes":[{"name":"small","criteria":{"from":0},"management":{"placeholders":0},
	      "effects":{"maximumRequestsInflight":150,"maximumMutatingRequestsInflight":50,
	        "containerResourcePolicy":{
	          "defaultRequests":{"cpu":"10m","memory":"64Mi"},
	          "goMemoryLimitPercent":80,"memoryLimitMultiplier":3,
	          "containers":[
	            {"workload":"helper","container":"init","requests":{"cpu":"250m","memory":"1024Mi"},"goMemoryLimitPercent":0,"goMaxProcs":1024},
	            {"workload":"kube-apiserver","container":"kube-apiserver","requests":{"cpu":"250m","memory":"1024Mi"}}
	          ]
	        }
	      }
	    }]
	  }
	}`
	var actual, expected any
	if err := json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("wire contract mismatch:\n%s", encoded)
	}
	after, _ := json.Marshal(r)
	if string(before) != string(after) {
		t.Fatal("renderer mutated resolved profile")
	}
	slices.Reverse(r.Containers)
	again, err := ToHyperShift(r, Budget{10, 64})
	if err != nil {
		t.Fatal(err)
	}
	againJSON, _ := json.Marshal(again)
	if string(encoded) != string(againJSON) {
		t.Fatal("input order changed output")
	}
}

func TestToHyperShiftConstrained(t *testing.T) {
	p := baseline()
	p.CPULimitEqualsRequest = true
	p.SafetyLimit = Ratio{11, 10}
	helper := p.Containers[0]
	helper.Identity = Identity{Kind: "Deployment", Workload: "helper", Container: "init", InitContainer: true}
	helper.CPURatio, helper.MemoryRatio, helper.Floor = Ratio{0, 1}, Ratio{0, 1}, Budget{25, 64}
	zero := int64(0)
	helper.GoMemoryLimitPercent = &zero
	p.Containers = append(p.Containers, helper)
	r, err := Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Containers {
		if c.Limits.CPU != c.Requests.CPU {
			t.Fatalf("CPU is not constrained: %+v", c)
		}
		if _, exists := c.Env["GOMAXPROCS"]; exists {
			t.Fatalf("CPU limits must not inject GOMAXPROCS: %+v", c)
		}
		wantLimit := "1127Mi"
		if c.Identity.InitContainer {
			wantLimit = "71Mi"
			if c.Requests != (Quantities{"25m", "64Mi"}) || len(c.Env) != 0 {
				t.Fatalf("static non-Go init override was not preserved: %+v", c)
			}
		}
		if c.Limits.Memory != wantLimit {
			t.Fatalf("memory must round up to MiB: %+v", c)
		}
	}
	got, err := ToHyperShift(r, Budget{10, 64})
	if err != nil {
		t.Fatal(err)
	}
	policy := got["spec"].(map[string]any)["sizes"].([]any)[0].(map[string]any)["effects"].(map[string]any)["containerResourcePolicy"].(map[string]any)
	if policy["cpuLimitEqualsRequest"] != true || policy["memoryLimitPercent"] != int64(110) || policy["memoryLimitMultiplier"] != nil {
		t.Fatalf("unexpected constrained wire policy: %+v", policy)
	}
}

func TestGoMemoryOverride(t *testing.T) {
	for _, percent := range []int64{-1, 0, 1, 50, 100, 101} {
		p := baseline()
		p.Containers[0].GoMemoryLimitPercent = &percent
		r, err := Resolve(p)
		if percent < 0 || percent > 100 {
			if err == nil || !reflect.DeepEqual(r, ResolvedProfile{}) {
				t.Fatalf("accepted percent %d: %+v, %v", percent, r, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, present := r.Containers[0].Env["GOMEMLIMIT"]; present != (percent != 0) {
			t.Fatalf("GOMEMLIMIT presence for %d: %+v", percent, r)
		}
		if percent == 0 {
			encoded, err := json.Marshal(r.Containers[0])
			if err != nil || strings.Contains(string(encoded), `"env"`) || !strings.Contains(string(encoded), `"goMemoryLimitPercent":0`) {
				t.Fatalf("disabled runtime policy did not round-trip explicitly: %s, %v", encoded, err)
			}
		}
		if _, err := ToHyperShift(r, Budget{10, 64}); err != nil {
			t.Fatal(err)
		}
		*r.Containers[0].GoMemoryLimitPercent = 42
		if *p.Containers[0].GoMemoryLimitPercent != percent {
			t.Fatal("resolved percentage aliases input")
		}
	}
}

func TestToHyperShiftRejectsUnrepresentable(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*ResolvedProfile, *Budget)
		want   string
	}{
		{"fractional percentage", func(p *ResolvedProfile, _ *Budget) { p.SafetyLimit = Ratio{4, 3} }, "integer percentage"},
		{"large multiplier", func(p *ResolvedProfile, _ *Budget) { p.SafetyLimit = Ratio{17, 1} }, "16"},
		{"zero denominator", func(p *ResolvedProfile, _ *Budget) { p.SafetyLimit.Denominator = 0 }, "denominator"},
		{"missing CPU limit", func(p *ResolvedProfile, _ *Budget) { p.CPULimitEqualsRequest = true }, "CPU limit"},
		{"unexpected CPU limit", func(p *ResolvedProfile, _ *Budget) { p.Containers[0].Limits.CPU = "250m" }, "CPU limit"},
		{"GOMAXPROCS", func(p *ResolvedProfile, _ *Budget) { p.Containers[0].Env["GOMAXPROCS"] = "1025" }, "1024"},
		{"zero GOMAXPROCS", func(p *ResolvedProfile, _ *Budget) { p.Containers[0].Env["GOMAXPROCS"] = "0" }, "1024"},
		{"default CPU", func(_ *ResolvedProfile, b *Budget) { b.CPUMilli = 0 }, "positive"},
		{"default memory", func(_ *ResolvedProfile, b *Budget) { b.MemoryMiB = -1 }, "positive"},
		{"default overflow", func(_ *ResolvedProfile, b *Budget) { b.MemoryMiB = math.MaxInt64 }, "int64"},
		{"kind collision", func(p *ResolvedProfile, _ *Budget) {
			c := p.Containers[0]
			c.Identity.Kind = "StatefulSet"
			p.Containers = append(p.Containers, c)
		}, "cannot distinguish"},
		{"init collision", func(p *ResolvedProfile, _ *Budget) {
			c := p.Containers[0]
			c.Identity.InitContainer = true
			p.Containers = append(p.Containers, c)
		}, "cannot distinguish"},
		{"empty overrides", func(p *ResolvedProfile, _ *Budget) { p.Containers = nil }, "1..256"},
		{"too many overrides", func(p *ResolvedProfile, _ *Budget) { p.Containers = make([]ResolvedContainer, 257) }, "1..256"},
		{"long workload", func(p *ResolvedProfile, _ *Budget) { p.Containers[0].Identity.Workload = strings.Repeat("a", 254) }, "identity"},
		{"long container", func(p *ResolvedProfile, _ *Budget) { p.Containers[0].Identity.Container = strings.Repeat("a", 64) }, "identity"},
		{"bad quantity", func(p *ResolvedProfile, _ *Budget) { p.Containers[0].Requests.CPU = "0m" }, "quantity"},
		{"rounded safety", func(p *ResolvedProfile, _ *Budget) { p.Containers[0].Limits.Memory = "3073Mi" }, "not representable"},
		{"wrong GOMEMLIMIT", func(p *ResolvedProfile, _ *Budget) { p.Containers[0].Env["GOMEMLIMIT"] = "1B" }, "percentage"},
		{"unknown env", func(p *ResolvedProfile, _ *Budget) { p.Containers[0].Env["OTHER"] = "1" }, "cannot project"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Resolve(baseline())
			if err != nil {
				t.Fatal(err)
			}
			defaults := Budget{10, 64}
			tt.mutate(&r, &defaults)
			got, err := ToHyperShift(r, defaults)
			if got != nil || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %+v, %v; want no artifact and error containing %q", got, err, tt.want)
			}
		})
	}
}

func TestSmallCompleteExample(t *testing.T) {
	data, err := os.ReadFile("../../examples/small-complete.json")
	if err != nil {
		t.Fatal(err)
	}
	var p Profile
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Quantities{
		"kube-apiserver": {"250m", "1024Mi"}, "etcd": {"100m", "256Mi"},
		"kube-controller-manager": {"30m", "192Mi"}, "openshift-apiserver": {"40m", "256Mi"},
		"openshift-controller-manager": {"10m", "128Mi"}, "cluster-policy-controller": {"10m", "128Mi"},
		"kube-scheduler": {"10m", "64Mi"}, "packageserver": {"125m", "320Mi"},
		"control-plane-operator": {"550m", "128Mi"}, "hosted-cluster-config-operator": {"25m", "128Mi"},
		"ignition-server": {"150m", "128Mi"},
	}
	if len(r.Containers) != len(want) {
		t.Fatalf("unexpected inventory: %+v", r)
	}
	for _, c := range r.Containers {
		if c.Requests != want[c.Identity.Workload] {
			t.Fatalf("unexpected requests: %+v", c)
		}
	}
	if _, err := ToHyperShift(r, Budget{10, 64}); err != nil {
		t.Fatal(err)
	}
}

func TestSmallCatalogV2Example(t *testing.T) {
	var parent, candidate Profile
	for name, target := range map[string]*Profile{"small-complete": &parent, "small-catalog-v2": &candidate} {
		data, err := os.ReadFile("../../examples/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			t.Fatal(err)
		}
		if target.Name != name {
			t.Fatalf("profile name: got %q, want %q", target.Name, name)
		}
	}
	if len(candidate.Containers) != len(parent.Containers)+6 {
		t.Fatalf("expected only four catalog and two HAProxy additions: %+v", candidate.Containers)
	}
	unchanged := candidate
	unchanged.Name, unchanged.Description = parent.Name, parent.Description
	unchanged.Containers = candidate.Containers[:len(parent.Containers)]
	if !reflect.DeepEqual(unchanged, parent) {
		t.Fatal("candidate changed parent settings or existing container budgets")
	}
	r, err := Resolve(candidate)
	if err != nil {
		t.Fatal(err)
	}
	additions := map[string]string{
		"community-operators-catalog": "registry", "certified-operators-catalog": "registry",
		"redhat-operators-catalog": "registry", "redhat-marketplace-catalog": "registry",
		"ignition-server-proxy": "haproxy", "router": "router",
	}
	for _, c := range r.Containers {
		container, added := additions[c.Identity.Workload]
		if !added {
			continue
		}
		if c.Identity != (Identity{Kind: "Deployment", Workload: c.Identity.Workload, Container: container}) {
			t.Fatalf("unexpected identity: %+v", c.Identity)
		}
		if container == "registry" {
			if c.Requests != (Quantities{"50m", "256Mi"}) || c.Limits.Memory != "768Mi" ||
				!reflect.DeepEqual(c.Env, map[string]string{"GOMEMLIMIT": "214748364B"}) || c.GoMemoryLimitPercent != nil {
				t.Fatalf("catalog must inherit 80%% of request, not safety limit: %+v", c)
			}
		} else if c.Requests != (Quantities{"10m", "64Mi"}) || c.Limits.Memory != "192Mi" ||
			len(c.Env) != 0 || c.GoMemoryLimitPercent == nil || *c.GoMemoryLimitPercent != 0 {
			t.Fatalf("HAProxy must keep fallback budgets and disable Go memory management: %+v", c)
		}
	}
	got, err := ToHyperShift(r, Budget{10, 64})
	if err != nil {
		t.Fatal(err)
	}
	policy := got["spec"].(map[string]any)["sizes"].([]any)[0].(map[string]any)["effects"].(map[string]any)["containerResourcePolicy"].(map[string]any)
	var retained []map[string]any
	for _, entry := range policy["containers"].([]map[string]any) {
		workload := entry["workload"].(string)
		container, added := additions[workload]
		if !added {
			retained = append(retained, entry)
			continue
		}
		want := map[string]any{"workload": workload, "container": container, "requests": Quantities{"50m", "256Mi"}}
		if container != "registry" {
			want["requests"] = Quantities{"10m", "64Mi"}
			want["goMemoryLimitPercent"] = int64(0)
		}
		if !reflect.DeepEqual(entry, want) {
			t.Fatalf("unexpected rendered override: %+v; want %+v", entry, want)
		}
		delete(additions, workload)
	}
	if len(additions) != 0 {
		t.Fatalf("missing explicit workload overrides: %+v", additions)
	}
	policy["containers"] = retained
	baseline, err := Resolve(parent)
	if err != nil {
		t.Fatal(err)
	}
	want, err := ToHyperShift(baseline, Budget{10, 64})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("candidate changed unrelated rendered policy, defaults, or budgets")
	}
}

func TestDoubleSmallV1Example(t *testing.T) {
	var profiles [2]Profile
	var resolved [2]ResolvedProfile
	var rendered [2]map[string]any
	for i, name := range []string{"small-catalog-v2", "double-small-v1"} {
		data, err := os.ReadFile("../../examples/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&profiles[i]); err != nil {
			t.Fatal(err)
		}
		if profiles[i].Name != name || len(profiles[i].Containers) != 17 {
			t.Fatalf("unexpected profile name or inventory: %+v", profiles[i])
		}
		resolved[i], err = Resolve(profiles[i])
		if err != nil {
			t.Fatal(err)
		}
		if resolved[i].Profile != name {
			t.Fatalf("resolved profile lost provenance: %+v", resolved[i])
		}
		scale := int64(i + 1)
		rendered[i], err = ToHyperShift(resolved[i], Budget{10 * scale, 64 * scale})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Only anchors and floors change; these ratios resolve exactly at 1m/1Mi.
	wantProfile := profiles[0]
	wantProfile.Name, wantProfile.Description = profiles[1].Name, profiles[1].Description
	wantProfile.Anchor = Budget{2 * wantProfile.Anchor.CPUMilli, 2 * wantProfile.Anchor.MemoryMiB}
	wantProfile.Containers = slices.Clone(wantProfile.Containers)
	for i := range wantProfile.Containers {
		wantProfile.Containers[i].Floor.CPUMilli *= 2
		wantProfile.Containers[i].Floor.MemoryMiB *= 2
	}
	if !reflect.DeepEqual(profiles[1], wantProfile) {
		t.Fatal("candidate must double every anchor/floor and preserve all other profile settings")
	}
	quantity := func(value, suffix string) int64 {
		t.Helper()
		n, err := strconv.ParseInt(strings.TrimSuffix(value, suffix), 10, 64)
		if err != nil || n <= 0 || value != strconv.FormatInt(n, 10)+suffix {
			t.Fatalf("invalid quantity %q: %v", value, err)
		}
		return n
	}
	var subtotal Budget
	var safetyMiB int64
	for i, c := range resolved[1].Containers {
		parent := resolved[0].Containers[i]
		if c.Identity != parent.Identity {
			t.Fatalf("changed identity: %+v vs %+v", c.Identity, parent.Identity)
		}
		cpu := quantity(c.Requests.CPU, "m")
		memory := quantity(c.Requests.Memory, "Mi")
		limit := quantity(c.Limits.Memory, "Mi")
		if cpu != 2*quantity(parent.Requests.CPU, "m") || memory != 2*quantity(parent.Requests.Memory, "Mi") || limit != 2*quantity(parent.Limits.Memory, "Mi") {
			t.Fatalf("requests and memory safety limit must be exactly 2x: %+v vs %+v", c, parent)
		}
		wantEnv := map[string]string{}
		if c.GoMemoryLimitPercent == nil || *c.GoMemoryLimitPercent != 0 {
			// Recompute floor(80% of new request bytes), not 2x old rounded bytes.
			wantEnv["GOMEMLIMIT"] = strconv.FormatInt(memory*(1<<20)*80/100, 10) + "B"
		}
		if !reflect.DeepEqual(c.Env, wantEnv) {
			t.Fatalf("unexpected Go settings (GOMAXPROCS must remain unset): %+v", c)
		}
		subtotal.CPUMilli += cpu
		subtotal.MemoryMiB += memory
		safetyMiB += limit
	}
	if subtotal != (Budget{3040, 7808}) || safetyMiB != 23424 {
		t.Fatalf("unexpected one-of-each override subtotal: %+v, safety %dMi", subtotal, safetyMiB)
	}

	// Scale only resource fields in the baseline wire object; all scheduling,
	// inflight, runtime exceptions, and class settings must remain identical.
	for i, object := range rendered {
		sizes := object["spec"].(map[string]any)["sizes"].([]any)
		if len(sizes) != 1 || sizes[0].(map[string]any)["name"] != "small" {
			t.Fatalf("renderer changed the personal small class: %+v", sizes)
		}
		policy := sizes[0].(map[string]any)["effects"].(map[string]any)["containerResourcePolicy"].(map[string]any)
		defaults := policy["defaultRequests"].(Quantities)
		wantDefaults := []Quantities{{"10m", "64Mi"}, {"20m", "128Mi"}}[i]
		if defaults != wantDefaults || policy["memoryLimitMultiplier"] != int64(3) || policy["goMemoryLimitPercent"] != int64(80) {
			t.Fatalf("unexpected fallback or runtime policy: %+v", policy)
		}
		if i == 0 {
			policy["defaultRequests"] = Quantities{"20m", "128Mi"}
			for _, entry := range policy["containers"].([]map[string]any) {
				requests := entry["requests"].(Quantities)
				entry["requests"] = Quantities{
					CPU:    strconv.FormatInt(2*quantity(requests.CPU, "m"), 10) + "m",
					Memory: strconv.FormatInt(2*quantity(requests.Memory, "Mi"), 10) + "Mi",
				}
			}
		}
	}
	if !reflect.DeepEqual(rendered[1], rendered[0]) {
		t.Fatal("candidate wire policy is not exactly the baseline with all requests doubled")
	}
}

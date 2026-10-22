// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package card

import (
	"fmt"
	"math"
	"strings"
)

const ResourcesSchemaVersion = "hcp-perf.resources/v1"
const MaxResourcesBytes = 10 << 20

// Resources retains reviewed v1 metadata without opening any evidence references.
// Only ParseResources can construct a usable Resources value.
type Resources struct {
	data map[string]any
}

// ParseResources validates the native, half-open time grid and per-container
// budgets. Null is unknown; zero consumption is valid but zero budgets are not.
// Source metadata is retained as evidence labels, not verified by the renderer.
func ParseResources(data []byte) (*Resources, error) {
	if len(data) > MaxResourcesBytes {
		return nil, fmt.Errorf("resources JSON exceeds %d bytes", MaxResourcesBytes)
	}
	d, err := decode(data, ResourcesSchemaVersion)
	if err != nil {
		return nil, err
	}
	if id, ok := d["cardID"].(string); !ok || strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("resources cardID is required")
	}
	for _, key := range []string{"window.start", "window.end", "window.expectedStepSeconds"} {
		if err := numberAt(d, key, true); err != nil {
			return nil, err
		}
	}
	start, end, step := at(d, "window.start").(float64), at(d, "window.end").(float64), at(d, "window.expectedStepSeconds").(float64)
	if end <= start || end > 253402300799 || step <= 0 || math.Ceil((end-start)/step-1e-7) > 2000 {
		return nil, fmt.Errorf("invalid resources window; at most 2000 native slots allowed")
	}
	series, ok := d["series"].([]any)
	if !ok || len(series) == 0 || len(series) > 512 {
		return nil, fmt.Errorf("resources series must contain 1 to 512 containers")
	}
	ids, identities := map[string]bool{}, map[string]bool{}
	for i, raw := range series {
		s, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("series[%d] must be an object", i)
		}
		for _, key := range []string{"id", "workload", "pod", "container", "replicaLabel"} {
			if v, ok := s[key].(string); !ok || strings.TrimSpace(v) == "" {
				return nil, fmt.Errorf("series[%d].%s must be a nonempty string", i, key)
			}
		}
		id, identity := s["id"].(string), s["pod"].(string)+"/"+s["container"].(string)
		if ids[id] || identities[identity] {
			return nil, fmt.Errorf("duplicate resources id or container identity: %s", id)
		}
		ids[id], identities[identity] = true, true
		if _, ok := s["initContainer"].(bool); !ok {
			return nil, fmt.Errorf("%s initContainer must be boolean", id)
		}
		for _, key := range []string{"controlled", "instrumentation"} {
			if v := s[key]; v != nil {
				if _, ok := v.(bool); !ok {
					return nil, fmt.Errorf("%s %s must be boolean or null", id, key)
				}
			}
		}
		for _, key := range []string{"requests", "limits"} {
			if _, ok := s[key].(map[string]any); !ok {
				return nil, fmt.Errorf("%s %s must be an object", id, key)
			}
		}
		for _, key := range []string{"requests.cpuCores", "requests.memoryMiB", "limits.cpuCores", "limits.memoryMiB", "goMemoryLimitMiB"} {
			if err := numberAt(s, key, false); err != nil {
				return nil, fmt.Errorf("%s: %w", id, err)
			}
			if at(s, key) == float64(0) {
				return nil, fmt.Errorf("%s %s must be positive or null (unknown/not applicable)", id, key)
			}
		}
		points, ok := s["points"].([]any)
		if !ok || len(points) > 2000 {
			return nil, fmt.Errorf("%s points must be an array of at most 2000 points", id)
		}
		previous := -1.0
		for _, raw := range points {
			p, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s point must be an object", id)
			}
			if err := numberAt(p, "timestamp", true); err != nil {
				return nil, err
			}
			ts := p["timestamp"].(float64)
			if ts < start || ts >= end || ts <= previous || math.Abs(ts-(start+math.Round((ts-start)/step)*step)) > 1e-6 {
				return nil, fmt.Errorf("%s timestamps must increase on the native grid within the window", id)
			}
			previous = ts
			for _, key := range []string{"cpuCores", "usageMiB", "workingSetMiB", "rssMiB", "goManagedMiB"} {
				if err := numberAt(p, key, false); err != nil {
					return nil, fmt.Errorf("%s: %w", id, err)
				}
			}
		}
	}
	if _, ok := d["method"].(map[string]any); !ok {
		return nil, fmt.Errorf("resources method must be an object")
	}
	sources, ok := d["sources"].([]any)
	if !ok || len(sources) == 0 {
		return nil, fmt.Errorf("resources sources must be a nonempty array")
	}
	for _, raw := range sources {
		s, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resource source must be an object")
		}
		for _, key := range []string{"id", "file"} {
			if v, ok := s[key].(string); !ok || strings.TrimSpace(v) == "" {
				return nil, fmt.Errorf("resource source %s is required", key)
			}
		}
	}
	limitations, ok := d["limitations"].([]any)
	if !ok {
		return nil, fmt.Errorf("resources limitations must be an array")
	}
	for _, v := range limitations {
		if _, ok := v.(string); !ok {
			return nil, fmt.Errorf("resource limitations must be strings")
		}
	}
	return &Resources{data: d}, nil
}

// Sum only a complete known budget, never advertise a partial sum as a total.
func resourceRequests(series []any, excludeInstrumentation bool) map[string]any {
	result := map[string]any{}
	for _, metric := range []string{"cpuCores", "memoryMiB"} {
		sum, known := 0.0, true
		for _, raw := range series {
			s := raw.(map[string]any)
			if excludeInstrumentation && s["instrumentation"] == true {
				continue
			}
			if v, ok := at(s, "requests."+metric).(float64); ok {
				sum += v
			} else {
				known = false
			}
		}
		if known {
			result[metric] = sum
		}
	}
	return result
}

func (r *Resources) match(c *Card, contention *Contention) error {
	if r.data == nil || r.data["cardID"] != c.data["id"] {
		return fmt.Errorf("resources cardID must match card id")
	}
	start, end := at(r.data, "window.start").(float64), at(r.data, "window.end").(float64)
	for _, raw := range at(c.data, "apfComparison.arms").([]any) {
		a := raw.(map[string]any)
		from, fok := a["startEpoch"].(float64)
		to, tok := a["stopEpoch"].(float64)
		if !fok || !tok || from < start || to > end {
			return fmt.Errorf("resources window must contain exact card APF arm %v", a["id"])
		}
	}
	if contention != nil && (at(contention.data, "window.start") != start || at(contention.data, "window.end") != end) {
		return fmt.Errorf("resources and contention windows must match")
	}
	series := r.data["series"].([]any)
	if count, ok := at(c.data, "topology.activeNamespaceInventory.runningContainers").(float64); ok && count != float64(len(series)) {
		return fmt.Errorf("resources inventory count disagrees with card; cannot label a partial inventory whole HCP")
	}
	for _, scope := range []struct {
		name    string
		exclude bool
	}{{"excludingExperimentalKSM", true}, {"includingExperimentalKSM", false}} {
		totals := resourceRequests(series, scope.exclude)
		for _, check := range []struct {
			metric, card string
			scale        float64
		}{{"cpuCores", "cpuMilli", 1000}, {"memoryMiB", "memoryMiB", 1}} {
			v, vok := totals[check.metric].(float64)
			want, wok := at(c.data, "topology.requestAccounting."+scope.name+"."+check.card).(float64)
			if vok && wok && math.Abs(v*check.scale-want) > 1e-9*math.Max(1, want) {
				return fmt.Errorf("resources %s %s total disagrees with card inventory", scope.name, check.metric)
			}
		}
	}
	// Check only the explicitly identified main containers, never a Pod sum or
	// a metrics-proxy label. Unknown sidecar budgets stay unknown in the UI.
	for _, raw := range series {
		s := raw.(map[string]any)
		profile := ""
		switch {
		case s["workload"] == "kube-apiserver" && s["container"] == "kube-apiserver":
			profile = "kas"
		case s["workload"] == "etcd" && s["container"] == "etcd":
			profile = "etcd"
		case strings.HasSuffix(s["workload"].(string), "-operators-catalog") && s["container"] == "registry":
			profile = "catalog"
		}
		if profile == "" {
			continue
		}
		for _, check := range []struct {
			source, card string
			scale        float64
		}{
			{"requests.cpuCores", "cpuMilli", 1000}, {"requests.memoryMiB", "memoryMiB", 1},
			{"limits.memoryMiB", "hardMemoryMiB", 1}, {"goMemoryLimitMiB", "goSoftTargetBytes", 1048576},
		} {
			v, vok := at(s, check.source).(float64)
			want, wok := at(c.data, "resourceProfile."+profile+"."+check.card).(float64)
			if vok && wok && math.Abs(v*check.scale-want) > 1e-9*math.Max(1, want) {
				return fmt.Errorf("resources %v %s disagrees with card %s budget", s["id"], check.source, profile)
			}
		}
	}
	return nil
}

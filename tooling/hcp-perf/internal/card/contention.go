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
	"sort"
)

// ParseContention accepts only hcp-perf.contention/v1. Series contain id, label,
// capacityCores and points with timestamp, busyPercent, adversaryCores and
// psiPercent. A missing/null metric is unknown; timestamps must increase.
// Unknown v1 metadata is retained. Render matches summary arm IDs and exact
// start/end boundaries to the card before showing data from the two together.
// All displayed summary statistics are checked against finite points in the
// half-open window; expected counts come from the declared native grid.
func ParseContention(data []byte) (*Contention, error) {
	d, err := decode(data, ContentionSchemaVersion)
	if err != nil {
		return nil, err
	}
	if id, ok := d["id"].(string); !ok || id == "" {
		return nil, fmt.Errorf("contention id is required")
	}
	for key, unit := range map[string]string{"timestamp": "unixSeconds", "busyPercent": "percent", "adversaryCores": "CPUcores", "psiPercent": "percent"} {
		if at(d, "units."+key) != unit {
			return nil, fmt.Errorf("contention units.%s must be %s", key, unit)
		}
	}
	for _, key := range []string{"window.start", "window.end", "window.expectedStepSeconds"} {
		if err := numberAt(d, key, true); err != nil {
			return nil, err
		}
	}
	start, end := at(d, "window.start").(float64), at(d, "window.end").(float64)
	if end <= start || end > 253402300799 || at(d, "window.expectedStepSeconds").(float64) <= 0 {
		return nil, fmt.Errorf("invalid contention window")
	}
	step := at(d, "window.expectedStepSeconds").(float64)
	// Allow only sub-microsecond timestamp roundoff at grid boundaries.
	gridIndex := func(t float64) float64 { return math.Ceil((t-start)/step - 1e-7) }
	expectedPoints := gridIndex(end)
	if expectedPoints > 9007199254740991 || at(d, "window.expectedPointsPerNode") != expectedPoints {
		return nil, fmt.Errorf("window.expectedPointsPerNode disagrees with the native grid")
	}
	check := func(m map[string]any, key string, expected any) error {
		actual, exists := m[key]
		if !exists {
			return fmt.Errorf("%s is required", key)
		}
		if expected == nil {
			if actual != nil {
				return fmt.Errorf("%s must be null without valid samples", key)
			}
			return nil
		}
		if err := numberAt(m, key, true); err != nil {
			return err
		}
		v, ok := actual.(float64)
		want := expected.(float64)
		if !ok || math.Abs(v-want) > 1e-9*math.Max(1, math.Abs(want)) {
			return fmt.Errorf("%s disagrees with points/grid: got %v, expected %v", key, actual, want)
		}
		return nil
	}
	if err := check(d["window"].(map[string]any), "durationSeconds", end-start); err != nil {
		return nil, err
	}
	nodes, ok := d["series"].([]any)
	if !ok || len(nodes) == 0 {
		return nil, fmt.Errorf("contention series must be a nonempty array")
	}
	ids, labels := map[string]bool{}, map[string]bool{}
	pointsByID := map[string][]any{}
	capacityByID := map[string]float64{}
	for i, node := range nodes {
		n, ok := node.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("series[%d] must be an object", i)
		}
		id, iok := n["id"].(string)
		label, lok := n["label"].(string)
		if !iok || !lok || id == "" || label == "" || ids[id] || labels[label] {
			return nil, fmt.Errorf("series[%d] needs unique nonempty id and label", i)
		}
		ids[id], labels[label] = true, true
		if err := numberAt(n, "capacityCores", true); err != nil {
			return nil, err
		}
		capacity := n["capacityCores"].(float64)
		capacityByID[id] = capacity
		if capacity <= 0 {
			return nil, fmt.Errorf("series[%d] capacityCores must be positive", i)
		}
		points, ok := n["points"].([]any)
		if !ok {
			return nil, fmt.Errorf("series[%d].points must be an array", i)
		}
		pointsByID[id] = points
		previous := float64(-1)
		for j, raw := range points {
			p, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("series[%d].points[%d] must be an object", i, j)
			}
			if err := numberAt(p, "timestamp", true); err != nil {
				return nil, err
			}
			t := p["timestamp"].(float64)
			if t < start || t >= end || t <= previous {
				return nil, fmt.Errorf("series[%d] timestamps must increase within the window", i)
			}
			if math.Abs(t-(start+math.Round((t-start)/step)*step)) > 1e-6 {
				return nil, fmt.Errorf("series[%d] timestamp is not on the native grid", i)
			}
			previous = t
			for key, max := range map[string]float64{"busyPercent": 100, "adversaryCores": capacity, "psiPercent": 100} {
				if err := numberAt(p, key, false); err != nil {
					return nil, err
				}
				if v, ok := p[key].(float64); ok && v > max {
					return nil, fmt.Errorf("series[%d].points[%d].%s exceeds %g", i, j, key, max)
				}
			}
		}
	}
	summaries, ok := d["summary"].([]any)
	if !ok || len(summaries) == 0 {
		return nil, fmt.Errorf("contention summary is required")
	}
	summaryIDs := map[string]bool{}
	for _, raw := range summaries {
		s, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("contention summary entries must be objects")
		}
		id, ok := s["id"].(string)
		if !ok || id == "" || summaryIDs[id] {
			return nil, fmt.Errorf("contention summary requires unique nonempty IDs")
		}
		summaryIDs[id] = true
		for _, key := range []string{"start", "end"} {
			if err := numberAt(s, key, true); err != nil {
				return nil, err
			}
		}
		if s["start"].(float64) < start || s["end"].(float64) > end || s["end"].(float64) <= s["start"].(float64) {
			return nil, fmt.Errorf("contention summary outside window")
		}
		from, to := s["start"].(float64), s["end"].(float64)
		if err := check(s, "durationSeconds", to-from); err != nil {
			return nil, fmt.Errorf("summary %s: %w", id, err)
		}
		expected := gridIndex(to) - gridIndex(from)
		members, ok := s["nodes"].([]any)
		if !ok || len(members) != len(nodes) {
			return nil, fmt.Errorf("summary %s must contain every series node exactly once", id)
		}
		seen := map[string]bool{}
		for _, member := range members {
			n, ok := member.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("summary %s node must be an object", id)
			}
			nodeID, ok := n["id"].(string)
			if !ok || !ids[nodeID] || seen[nodeID] {
				return nil, fmt.Errorf("summary %s has missing, unknown or duplicate node ID", id)
			}
			seen[nodeID] = true
			for _, metric := range []string{"busyPercent", "adversaryCores", "psiPercent"} {
				stats, ok := n[metric].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("summary %s/%s requires %s statistics", id, nodeID, metric)
				}
				var values []float64
				var first, last any
				above90 := 0
				for _, rawPoint := range pointsByID[nodeID] {
					p := rawPoint.(map[string]any)
					t := p["timestamp"].(float64)
					v, valid := p[metric].(float64)
					if t < from || t >= to || !valid {
						continue
					}
					if first == nil {
						first = t
					}
					last = t
					values = append(values, v)
					if v > 90 {
						above90++
					}
				}
				sort.Float64s(values)
				count := float64(len(values))
				var minimum, maximum, median, fraction, coverage any
				if expected > 0 {
					coverage = count / expected
				}
				if len(values) > 0 {
					minimum, maximum = values[0], values[len(values)-1]
					median = (values[(len(values)-1)/2] + values[len(values)/2]) / 2
					fraction = float64(above90) / count
				}
				checks := map[string]any{"sampleCount": count, "expectedSampleCount": expected, "coverageFraction": coverage, "min": minimum, "max": maximum, "median": median, "firstTimestamp": first, "lastTimestamp": last}
				if metric == "busyPercent" {
					checks["fractionAbove90Percent"] = fraction
				}
				for key, want := range checks {
					if value, ok := stats[key].(float64); ok {
						if (key == "coverageFraction" || key == "fractionAbove90Percent") && value > 1 {
							return nil, fmt.Errorf("summary %s/%s/%s %s exceeds 1", id, nodeID, metric, key)
						}
						maximum := float64(100)
						if metric == "adversaryCores" {
							maximum = capacityByID[nodeID]
						}
						if (key == "min" || key == "max" || key == "median") && value > maximum {
							return nil, fmt.Errorf("summary %s/%s/%s %s exceeds metric range", id, nodeID, metric, key)
						}
					}
					// Counts and source timestamps are exact, not approximately equal.
					if key == "sampleCount" || key == "expectedSampleCount" || key == "firstTimestamp" || key == "lastTimestamp" {
						if got, exists := stats[key]; !exists || got != want {
							return nil, fmt.Errorf("summary %s/%s/%s %s disagrees with points/grid", id, nodeID, metric, key)
						}
					}
					if err := check(stats, key, want); err != nil {
						return nil, fmt.Errorf("summary %s/%s/%s: %w", id, nodeID, metric, err)
					}
				}
			}
		}
	}
	return &Contention{data: d}, nil
}

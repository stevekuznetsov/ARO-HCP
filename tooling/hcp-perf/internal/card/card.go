// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package card renders reviewed portable JSON, never the artifacts it cites.
package card

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"strings"
	"time"
)

const SchemaVersion = "hcp-perf.tshirt-card/v1"
const ContentionSchemaVersion = "hcp-perf.contention/v1"
const MaxInputBytes = 32 << 20

// Card retains the full reviewed schema. Absent numeric fields remain unknown,
// rather than becoming Go zero values and silently claiming zero failures.
type Card struct {
	data map[string]any
}

// Contention is a portable time-series sidecar. Unknown v1 metadata is retained.
// Its exact wire schema is validated separately from the main card.
type Contention struct {
	data map[string]any
}

func decode(data []byte, version string) (map[string]any, error) {
	if len(data) > MaxInputBytes {
		return nil, fmt.Errorf("JSON exceeds %d bytes", MaxInputBytes)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode portable JSON: %w", err)
	}
	if result["schemaVersion"] != version {
		return nil, fmt.Errorf("unsupported schemaVersion %v; expected %s", result["schemaVersion"], version)
	}
	return result, nil
}

// Parse accepts only the supported card version. Unknown v1 fields are retained;
// missing measurements are displayed as unknown. Identity and core sections are
// required, and present chart values must have valid units and ranges.
func Parse(data []byte) (*Card, error) {
	d, err := decode(data, SchemaVersion)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"id", "name", "status", "measuredDate"} {
		if s, ok := d[key].(string); !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("%s must be a nonempty string", key)
		}
	}
	if _, err := time.Parse("2006-01-02", d["measuredDate"].(string)); err != nil {
		return nil, fmt.Errorf("measuredDate: %w", err)
	}
	for _, key := range []string{"scope", "population", "densityScreens", "apfComparison", "health", "topology", "resourceProfile"} {
		if _, ok := d[key].(map[string]any); !ok {
			return nil, fmt.Errorf("%s must be an object", key)
		}
	}
	for _, path := range []string{"population.applicationPodsReady", "densityScreens.scoringSeconds", "apfComparison.client.offeredQPS", "apfComparison.client.durationSecondsPerArm", "health.safety.observations", "health.safety.recordedNewOOM", "health.safety.restartDeltas"} {
		if err := numberAt(d, path, false); err != nil {
			return nil, err
		}
	}
	for _, group := range []struct {
		path   string
		fields []string
	}{
		{"densityScreens.cases", []string{"applicationPods", "deployments", "scheduledRollouts", "completedRollouts", "completedCanaries", "softDegradedCanaries"}},
		{"apfComparison.arms", []string{"shares", "startEpoch", "stopEpoch", "offered", "dispatched", "httpAttempts", "successfulBodies", "http429", "timeouts", "generatorLimited", "successfulQPSActiveDenominatorIncludingDrainCompletions", "successfulGETP95Seconds", "successfulLISTP95Seconds", "hardCompletedCanaries", "boundedCompletedRollouts", "acceptedRollouts"}},
	} {
		items, ok := at(d, group.path).([]any)
		if !ok || len(items) == 0 {
			return nil, fmt.Errorf("%s must be a nonempty array", group.path)
		}
		for i, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s[%d] must be an object", group.path, i)
			}
			for _, field := range group.fields {
				if err := numberAt(m, field, false); err != nil {
					return nil, fmt.Errorf("%s[%d]: %w", group.path, i, err)
				}
			}
			if group.path == "apfComparison.arms" {
				if id, ok := m["id"].(string); !ok || id == "" {
					return nil, fmt.Errorf("apfComparison.arms[%d].id is required", i)
				}
			}
			start, sok := m["startEpoch"].(float64)
			stop, eok := m["stopEpoch"].(float64)
			if sok && eok && stop <= start {
				return nil, fmt.Errorf("%s[%d]: stopEpoch must exceed startEpoch", group.path, i)
			}
		}
	}
	return &Card{data: d}, nil
}

func at(d map[string]any, path string) any {
	var value any = d
	for _, key := range strings.Split(path, ".") {
		m, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = m[key]
	}
	return value
}

func numberAt(d map[string]any, path string, required bool) error {
	v := at(d, path)
	if v == nil && !required {
		return nil
	}
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 9007199254740991 {
		return fmt.Errorf("%s must be a finite nonnegative number within JavaScript's safe numeric range", path)
	}
	return nil
}

func number(v any, digits int) string {
	if f, ok := v.(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return fmt.Sprintf("%.*f", digits, f)
	}
	return "unknown"
}

func pretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "unknown"
	}
	return string(b)
}

func utc(v any) string {
	f, ok := v.(float64)
	if !ok || f > 253402300799 {
		return "unknown"
	}
	seconds, fraction := math.Modf(f)
	return time.Unix(int64(seconds), int64(fraction*1e9)).UTC().Format("2006-01-02 15:04:05.000")
}

// Render writes one self-contained HTML document. It performs no file reads,
// network requests or operations against a source/runtime environment.
func Render(out io.Writer, c *Card, contention *Contention) error {
	return RenderWithResources(out, c, contention, nil)
}

// RenderWithResources adds optional, identity-checked resource observations to
// the existing portable report. It never queries a runtime or reads citations.
func RenderWithResources(out io.Writer, c *Card, contention *Contention, resources *Resources) error {
	return RenderWithEvidence(out, c, contention, resources, nil)
}

// RenderWithEvidence adds optional client and canary observations. Every sidecar
// is bound to the card before any output is written; citations remain inert.
func RenderWithEvidence(out io.Writer, c *Card, contention *Contention, resources *Resources, experience *Experience) error {
	if c == nil || c.data == nil {
		return fmt.Errorf("card is required")
	}
	d := c.data
	view := map[string]any{"Card": d, "Title": "Proposed " + strings.Split(d["name"].(string), ":")[0] + " resource profile"}
	if contention != nil {
		if contention.data == nil {
			return fmt.Errorf("contention must be parsed before rendering")
		}
		// Sidecars identify the comparison via exact arm boundaries, not a
		// guessed filename relationship or a field absent from the wire schema.
		for _, rawArm := range at(d, "apfComparison.arms").([]any) {
			arm := rawArm.(map[string]any)
			matched := false
			for _, raw := range contention.data["summary"].([]any) {
				summary := raw.(map[string]any)
				if summary["id"] == arm["id"] && summary["start"] == arm["startEpoch"] && summary["end"] == arm["stopEpoch"] {
					matched = true
				}
			}
			if !matched {
				return fmt.Errorf("contention has no matching boundary summary for arm %v", arm["id"])
			}
		}
		view["Contention"] = contention.data
	}
	if resources != nil {
		if err := resources.match(c, contention); err != nil {
			return err
		}
		view["Resources"] = resources.data
		view["ResourceRequests"] = resourceRequests(resources.data["series"].([]any), true)
		view["InstrumentedRequests"] = resourceRequests(resources.data["series"].([]any), false)
	}
	if experience != nil {
		if err := experience.match(c, contention, resources); err != nil {
			return err
		}
		view["Experience"] = experience.data
		cycles := append([]any{}, experience.data["canaries"].([]any)...)
		for _, raw := range experience.data["arms"].([]any) {
			cycles = append(cycles, raw.(map[string]any)["canaries"].([]any)...)
		}
		view["ExperienceCanaries"] = cycles
		var incomplete []any
		for _, raw := range cycles {
			if raw.(map[string]any)["status"] != "completed" {
				incomplete = append(incomplete, raw)
			}
		}
		view["ExperienceIncomplete"] = incomplete
	}
	view["ClientBehavior"] = clientBehavior(c)
	pop := number(at(d, "population.applicationPodsReady"), 0)
	view["Lead"] = pop + " application Pods were recorded in the reference population."
	for _, item := range at(d, "densityScreens.cases").([]any) {
		m := item.(map[string]any)
		if m["applicationPods"] != nil && m["applicationPods"] == at(d, "population.applicationPodsReady") && m["completedRollouts"] != nil && m["completedRollouts"] == m["scheduledRollouts"] && m["completedCanaries"] != nil && m["completedCanaries"] == at(d, "densityScreens.steadyCanariesEach") {
			view["Lead"] = pop + " application Pods completed the population screens."
		}
	}
	noCrash := at(d, "health.safety.observations") != nil && at(d, "health.safety.observations").(float64) > 0 && at(d, "health.safety.recordedNewOOM") == float64(0) && at(d, "health.safety.restartDeltas") == float64(0)
	view["NoObservedCrash"] = noCrash
	var limitedArms []any
	for _, item := range at(d, "apfComparison.arms").([]any) {
		m := item.(map[string]any)
		if q, ok := m["http429"].(float64); ok && q > 0 {
			view["Shedding"] = true
		}
		if count, ok := m["generatorLimited"].(float64); ok && count > 0 {
			limitedArms = append(limitedArms, m)
		}
		if m["shares"] == float64(100) {
			view["Shares100Arm"] = m
		}
	}
	if view["Shedding"] == true && noCrash {
		view["Lead"] = view["Lead"].(string) + " Under added API pressure, requests were shed, and no OOM/restart was observed."
	}
	view["LimitedArms"] = limitedArms
	view["FirstArm"] = at(d, "apfComparison.arms").([]any)[0]
	// Marshal with encoding/json's default HTML escaping. Never mark source text
	// as trusted HTML/JS; only these freshly encoded JSON bytes enter the script.
	payload, err := json.Marshal(map[string]any{"card": d, "contention": view["Contention"], "resources": view["Resources"], "experience": view["Experience"]})
	if err != nil {
		return err
	}
	view["JSON"] = template.JS(payload)
	funcs := template.FuncMap{
		"n": number, "pretty": pretty, "utc": utc,
		"percent": func(a, b any) any {
			x, xok := a.(float64)
			y, yok := b.(float64)
			if xok && yok && y > 0 {
				return 100 * x / y
			}
			return nil
		},
		"knownNumber": func(v any) bool { _, ok := v.(float64); return ok },
		"text": func(v any) string {
			if v == nil {
				return "unknown"
			}
			return fmt.Sprint(v)
		},
		"div": func(v any, denominator float64) any {
			if f, ok := v.(float64); ok && denominator > 0 {
				return f / denominator
			}
			return nil
		},
		"mul": func(a, b any) any {
			x, xok := a.(float64)
			y, yok := b.(float64)
			if xok && yok && !math.IsInf(x*y, 0) {
				return x * y
			}
			return nil
		},
	}
	t, err := template.New("card").Funcs(funcs).Parse(pageHTML + pageCSS + pageJS + resourcesHTML + resourcesJS + experienceHTML + experienceJS)
	if err != nil {
		return err
	}
	var rendered bytes.Buffer
	if err := t.Execute(&rendered, view); err != nil {
		return fmt.Errorf("render card: %w", err)
	}
	_, err = io.Copy(out, &rendered)
	return err
}

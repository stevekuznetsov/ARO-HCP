// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dashboard

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strings"
)

//go:embed assets/page.html
var dashboardPage string

//go:embed assets/style.css
var dashboardStyle string

//go:embed assets/app.js
var dashboardScript string

// Render writes a self-contained dashboard. Only the embedded application assets
// are trusted markup; dataset strings are escaped in both HTML and JSON contexts.
func Render(w io.Writer, dataset Dataset) error {
	if dataset.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported dashboard schema %q", dataset.SchemaVersion)
	}
	if len(dataset.Sizes) == 0 {
		return fmt.Errorf("dashboard requires at least one size")
	}
	encoded, err := json.Marshal(dataset) // Keep encoding/json's HTML escaping enabled.
	if err != nil {
		return fmt.Errorf("encode dashboard: %w", err)
	}
	if len(encoded) > MaxInputBytes {
		return fmt.Errorf("dashboard exceeds %d bytes", MaxInputBytes)
	}
	functions := template.FuncMap{
		"money":    func(v float64) string { return fmt.Sprintf("$%.7f", v) },
		"number":   func(v float64) string { return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".") },
		"duration": func(v float64) string { return fmt.Sprintf("%.1f min", v/60) },
		"percent": func(r Requests) string {
			if r.Dispatched == 0 {
				return "Not measured"
			}
			return fmt.Sprintf("%.2f%%", float64(r.DegradedUnion)/float64(r.Dispatched)*100)
		},
		"reference": func(s Size) *Run {
			for i := range s.Runs {
				if s.Runs[i].ID == s.DefaultRun {
					return &s.Runs[i]
				}
			}
			return nil
		},
		"badge": func(s string) string {
			switch strings.ToLower(s) {
			case "pass", "passed":
				return "pass"
			case "fail", "failed":
				return "fail"
			default:
				return "neutral"
			}
		},
		"orUnknown": func(s string) string {
			if s == "" {
				return "Unknown"
			}
			return s
		},
	}
	tmpl, err := template.New("dashboard").Funcs(functions).Parse(dashboardPage)
	if err != nil {
		return fmt.Errorf("parse dashboard template: %w", err)
	}
	view := struct {
		Dataset
		CSS          template.CSS
		JS           template.JS
		JSON         template.JS
		Placeholders []string
	}{
		Dataset: dataset,
		CSS:     template.CSS(dashboardStyle),
		JS:      template.JS(dashboardScript),
		JSON:    template.JS(encoded),
	}
	if len(dataset.Sizes) == 1 && strings.EqualFold(dataset.Sizes[0].Name, "Small") {
		view.Placeholders = []string{"Medium", "Large", "XL", "XXL"}
	}
	if err := tmpl.Execute(w, view); err != nil {
		return fmt.Errorf("render dashboard: %w", err)
	}
	return nil
}

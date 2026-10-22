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
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/url"
	"path"
	"regexp"
	"strings"
)

const OverviewSchemaVersion = "hcp-perf.dashboard-overview/v1"

// OverviewSpec controls presentation only. Measurements and assessments come from
// the portable datasets; array order is the reviewed editorial order.
type OverviewSpec struct {
	SchemaVersion string              `json:"schemaVersion"`
	Title         string              `json:"title"`
	Introduction  string              `json:"introduction"`
	Caveats       []string            `json:"caveats"`
	Groups        []OverviewGroupSpec `json:"groups"`
}

type OverviewGroupSpec struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Input      string              `json:"input"`
	DetailPage string              `json:"detailPage"`
	Headline   string              `json:"headline"`
	Scope      string              `json:"scope"`
	SizeIDs    []string            `json:"sizeIDs"`
	Highlights []OverviewSelection `json:"highlights"`
}

type OverviewSelection struct {
	SizeID string `json:"sizeID"`
	RunID  string `json:"runID"`
	Label  string `json:"label"`
}

type Overview struct {
	OverviewSpec
	Groups   []OverviewGroup
	RunCount int
}

type OverviewGroup struct {
	OverviewGroupSpec
	Budget      Budget
	GeneratedAt string
	Panes       []OverviewPane
	Highlights  []OverviewRun
	RunCount    int
}

type OverviewPane struct {
	Size
	DetailURL string
	Runs      []OverviewRun
}

type OverviewRun struct {
	Run
	Anchor         string
	SelectionLabel string
	PaneName       string
	DetailURL      string
	Sources        []Source
}

var overviewID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
var overviewPath = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)

func (s OverviewSpec) Validate() error {
	if s.SchemaVersion != OverviewSchemaVersion || strings.TrimSpace(s.Title) == "" || len(s.Groups) == 0 || len(s.Groups) > 8 {
		return errors.New("overview requires supported schema, title and 1-8 groups")
	}
	ids, inputs := map[string]bool{}, map[string]bool{}
	for _, g := range s.Groups {
		if !overviewID.MatchString(g.ID) || ids[g.ID] || inputs[g.Input] || strings.TrimSpace(g.Name) == "" || len(g.SizeIDs) == 0 || len(g.Highlights) == 0 {
			return fmt.Errorf("group %q requires unique ID/input, name, panes and highlights", g.ID)
		}
		ids[g.ID], inputs[g.Input] = true, true
		for _, p := range []struct{ value, extension string }{{g.Input, ".json"}, {g.DetailPage, ".html"}} {
			if !fs.ValidPath(p.value) || !overviewPath.MatchString(p.value) || path.Ext(p.value) != p.extension {
				return fmt.Errorf("group %s: paths must be local relative %s files without traversal, URLs or fragments", g.ID, p.extension)
			}
		}
	}
	return nil
}

// BuildOverview resolves selections after the CLI's shared dataset validation.
// Every pane and run stays in the register, including unselected historical cases.
func BuildOverview(spec OverviewSpec, datasets []Dataset) (Overview, error) {
	result := Overview{OverviewSpec: spec}
	if err := spec.Validate(); err != nil {
		return result, err
	}
	if len(datasets) != len(spec.Groups) {
		return result, errors.New("overview dataset count mismatch")
	}
	for i, g := range spec.Groups {
		d := datasets[i]
		group := OverviewGroup{OverviewGroupSpec: g, GeneratedAt: d.GeneratedAt}
		sources := map[string]Source{}
		for _, source := range d.Sources {
			sources[source.ID] = source
		}
		panes, runs := map[string]bool{}, map[string]OverviewRun{}
		for pi, id := range g.SizeIDs {
			if panes[id] {
				return result, fmt.Errorf("duplicate pane %s", id)
			}
			var size *Size
			for j := range d.Sizes {
				if d.Sizes[j].ID == id {
					size = &d.Sizes[j]
					break
				}
			}
			if size == nil {
				return result, fmt.Errorf("unknown pane %s", id)
			}
			panes[id] = true
			if pi == 0 {
				group.Budget = size.Budget
			}
			b := size.Budget
			if b.FeeUSD <= 0 || b.FeeUSD != group.Budget.FeeUSD || b.TargetUSD != group.Budget.TargetUSD || b.ModeledUSD != group.Budget.ModeledUSD || b.ChargedCPUCores != group.Budget.ChargedCPUCores || b.ChargedMemoryGiB != group.Budget.ChargedMemoryGiB {
				return result, fmt.Errorf("group %s requires a common fee, target and charged budget", g.ID)
			}
			pane := OverviewPane{Size: *size, DetailURL: g.DetailPage + "#" + url.PathEscape(id)}
			for ri, r := range size.Runs {
				run := OverviewRun{Run: r, Anchor: fmt.Sprintf("run-%s-%d-%d", g.ID, pi+1, ri+1), PaneName: size.Name, DetailURL: pane.DetailURL}
				for _, ref := range r.Sources {
					source, ok := sources[ref]
					if !ok {
						return result, fmt.Errorf("run %s: unknown source %s", r.ID, ref)
					}
					run.Sources = append(run.Sources, source)
				}
				pane.Runs = append(pane.Runs, run)
				runs[id+"\x00"+r.ID] = run
				group.RunCount++
			}
			group.Panes = append(group.Panes, pane)
		}
		if len(panes) != len(d.Sizes) {
			return result, fmt.Errorf("group %s must include every portable pane", g.ID)
		}
		selected := map[string]bool{}
		for _, selection := range g.Highlights {
			key := selection.SizeID + "\x00" + selection.RunID
			run, ok := runs[key]
			if !ok || selected[key] || strings.TrimSpace(selection.Label) == "" {
				return result, fmt.Errorf("unknown, duplicate or unlabeled selection %s/%s", selection.SizeID, selection.RunID)
			}
			selected[key] = true
			run.SelectionLabel = selection.Label
			group.Highlights = append(group.Highlights, run)
		}
		result.RunCount += group.RunCount
		result.Groups = append(result.Groups, group)
	}
	return result, nil
}

//go:embed assets/overview.html
var overviewPage string

//go:embed assets/overview.css
var overviewStyle string

//go:embed assets/overview.js
var overviewScript string

func RenderOverview(w io.Writer, overview Overview) error {
	functions := template.FuncMap{
		"number":        func(v float64) string { return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".") },
		"money":         func(v float64) string { return fmt.Sprintf("$%.7f", v) },
		"targetPercent": func(b Budget) string { return fmt.Sprintf("%.0f%%", 100*b.TargetUSD/b.FeeUSD) },
		"status": func(s string) string {
			if s == "" {
				return "unknown"
			}
			return s
		},
		"tone": func(s string) string {
			switch strings.ToLower(s) {
			case "passed", "pass":
				return "pass"
			case "failed", "fail":
				return "fail"
			}
			return "neutral"
		},
		"date": func(s string) string {
			if len(s) >= 10 {
				return s[:10]
			}
			return s
		},
	}
	tmpl, err := template.New("overview").Funcs(functions).Parse(overviewPage)
	if err != nil {
		return err
	}
	return tmpl.Execute(w, struct {
		Overview
		CSS template.CSS
		JS  template.JS
	}{overview, template.CSS(overviewStyle), template.JS(overviewScript)})
}

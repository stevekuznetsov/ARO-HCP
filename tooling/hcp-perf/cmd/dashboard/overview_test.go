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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	core "github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/dashboard"
)

func overviewFixture() core.OverviewSpec {
	return core.OverviewSpec{SchemaVersion: core.OverviewSchemaVersion, Title: "Synthetic overview", Groups: []core.OverviewGroupSpec{{ID: "test", Name: "Test", Input: "portable.json", DetailPage: "detail.html", SizeIDs: []string{"test"}, Highlights: []core.OverviewSelection{{SizeID: "test", RunID: "run", Label: "Reference"}}}}}
}

func TestOverviewValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*core.OverviewSpec, *core.Dataset)
	}{
		{"schema", func(s *core.OverviewSpec, d *core.Dataset) { s.SchemaVersion = "wrong" }},
		{"no groups", func(s *core.OverviewSpec, d *core.Dataset) { s.Groups = nil }},
		{"too many groups", func(s *core.OverviewSpec, d *core.Dataset) {
			for len(s.Groups) < 9 {
				s.Groups = append(s.Groups, s.Groups[0])
			}
		}},
		{"duplicate group", func(s *core.OverviewSpec, d *core.Dataset) { s.Groups = append(s.Groups, s.Groups[0]) }},
		{"missing pane", func(s *core.OverviewSpec, d *core.Dataset) { s.Groups[0].SizeIDs = []string{"missing"} }},
		{"duplicate pane", func(s *core.OverviewSpec, d *core.Dataset) { s.Groups[0].SizeIDs = append(s.Groups[0].SizeIDs, "test") }},
		{"omitted pane", func(s *core.OverviewSpec, d *core.Dataset) {
			d.Sizes = append(d.Sizes, core.Size{ID: "another", Name: "Another"})
		}},
		{"unknown run", func(s *core.OverviewSpec, d *core.Dataset) { s.Groups[0].Highlights[0].RunID = "missing" }},
		{"duplicate run", func(s *core.OverviewSpec, d *core.Dataset) {
			s.Groups[0].Highlights = append(s.Groups[0].Highlights, s.Groups[0].Highlights[0])
		}},
		{"unknown source", func(s *core.OverviewSpec, d *core.Dataset) { d.Sizes[0].Runs[0].Sources = []string{"missing"} }},
		{"unknown component", func(s *core.OverviewSpec, d *core.Dataset) { d.Sizes[0].Runs[0].Resources[0].ComponentID = "missing" }},
		{"invalid distribution", func(s *core.OverviewSpec, d *core.Dataset) { d.Sizes[0].Runs[0].Latencies[0].Distribution.P95 = -1 }},
		{"zero fee", func(s *core.OverviewSpec, d *core.Dataset) { d.Sizes[0].Budget.FeeUSD = 0 }},
		{"budget mismatch", func(s *core.OverviewSpec, d *core.Dataset) {
			other := d.Sizes[0]
			other.ID = "other"
			other.Budget.TargetUSD = 1
			d.Sizes = append(d.Sizes, other)
			s.Groups[0].SizeIDs = append(s.Groups[0].SizeIDs, "other")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			spec, d := overviewFixture(), fixture()
			d.Sizes[0].Budget.FeeUSD = .25
			test.edit(&spec, &d)
			input, output := filepath.Join(dir, "spec.json"), filepath.Join(dir, "out.html")
			writeJSON(t, input, spec)
			writeJSON(t, filepath.Join(dir, "portable.json"), d)
			if err := execute(context.Background(), io.Discard, "overview", "--input", input, "--output", output); err == nil {
				t.Fatal("invalid overview accepted")
			}
			assertAbsent(t, output)
		})
	}
	for _, p := range []string{"../escape.json", "/absolute.json", "https://example.invalid/a.json", "//host/a.json", "a\\b.json", "a/../b.json", "a%2fb.json", "a.json?x", "a.json#x", "a.json\n", "javascript:alert(1)", "a.json/", "./a.json"} {
		t.Run(p, func(t *testing.T) {
			s := overviewFixture()
			s.Groups[0].Input = p
			if s.Validate() == nil {
				t.Fatal("unsafe input path accepted")
			}
			s = overviewFixture()
			s.Groups[0].DetailPage = p
			if s.Validate() == nil {
				t.Fatal("unsafe href accepted")
			}
		})
	}
}

func TestOverviewStrictInputs(t *testing.T) {
	specBytes, _ := json.Marshal(overviewFixture())
	d := fixture()
	d.Sizes[0].Budget.FeeUSD = .25
	data, _ := json.Marshal(d)
	for _, target := range []string{"spec.json", "portable.json"} {
		valid := specBytes
		if target == "portable.json" {
			valid = data
		}
		for name, bad := range map[string][]byte{"unknown": []byte(`{"unknown":true}`), "null": []byte("null"), "trailing": append(append([]byte{}, valid...), []byte(" {}")...), "oversized": bytes.Repeat([]byte(" "), core.MaxInputBytes+1)} {
			t.Run(target+name, func(t *testing.T) {
				dir := t.TempDir()
				writeJSON(t, filepath.Join(dir, "spec.json"), overviewFixture())
				writeJSON(t, filepath.Join(dir, "portable.json"), d)
				if err := os.WriteFile(filepath.Join(dir, target), bad, 0600); err != nil {
					t.Fatal(err)
				}
				out := filepath.Join(dir, "out.html")
				if err := execute(context.Background(), io.Discard, "overview", "--input", filepath.Join(dir, "spec.json"), "--output", out); err == nil {
					t.Fatal("invalid JSON accepted")
				}
				assertAbsent(t, out)
			})
		}
	}
	t.Run("confined symlink", func(t *testing.T) {
		dir := t.TempDir()
		external := filepath.Join(t.TempDir(), "external.json")
		writeJSON(t, external, d)
		writeJSON(t, filepath.Join(dir, "spec.json"), overviewFixture())
		if err := os.Symlink(external, filepath.Join(dir, "portable.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := readOverview(context.Background(), filepath.Join(dir, "spec.json")); err == nil {
			t.Fatal("escaping symlink accepted")
		}
	})
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		writeJSON(t, filepath.Join(dir, "spec.json"), overviewFixture())
		if err := os.Mkdir(filepath.Join(dir, "portable.json"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := readOverview(context.Background(), filepath.Join(dir, "spec.json")); err == nil {
			t.Fatal("nonregular input accepted")
		}
	})
}

func TestOverviewEscapingAndExclusiveOutput(t *testing.T) {
	dir := t.TempDir()
	spec, d := overviewFixture(), fixture()
	d.Sizes[0].Budget.FeeUSD = .25
	payload := `</script><script>globalThis.injected=true</script><img src=x onerror=alert(1)>`
	spec.Title = payload
	spec.Groups[0].Highlights[0].Label = payload
	d.Sources[0].Label = payload
	d.Sources[0].ID = "https://example.invalid/source"
	d.Sizes[0].Runs[0].Sources = []string{d.Sources[0].ID}
	d.Sizes[0].Runs[0].Notes = []string{payload}
	d.Sizes[0].Runs[0].Assessment.Summary = payload
	input, output := filepath.Join(dir, "spec.json"), filepath.Join(dir, "out.html")
	writeJSON(t, input, spec)
	writeJSON(t, filepath.Join(dir, "portable.json"), d)
	raw := NewRawOptions(Overview)
	raw.Input = input
	raw.Output = output
	v, err := raw.Validate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o, err := v.Complete(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertAbsent(t, output)
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	html := string(before)
	if strings.Contains(html, payload) || strings.Count(html, "<script>") != 1 || strings.Contains(html, "#ZgotmplZ") || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("unsafe or lost HTML escaping")
	}
	if strings.Contains(html, `href="https:`) || !strings.Contains(html, "https://example.invalid/source") {
		t.Fatal("source citation became a link or was lost")
	}
	if err := o.Run(context.Background()); !errors.Is(err, os.ErrExist) {
		t.Fatal("output overwritten", err)
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(before, after) {
		t.Fatal("output changed")
	}
	info, _ := os.Stat(output)
	if info.Mode().Perm() != 0600 {
		t.Fatal("wrong output permissions")
	}
}

func TestOverviewSavedDataAndLocalLinks(t *testing.T) {
	base := filepath.Join("..", "..", "docs", "dashboard")
	view, err := readOverview(context.Background(), filepath.Join(base, "overview.json"))
	if err != nil {
		t.Fatal(err)
	}
	if view.RunCount != 37 || len(view.Groups) != 3 || len(view.Groups[2].Panes) != 4 {
		t.Fatal("lost measured cases or panes")
	}
	if view.Groups[0].Highlights[0].Assessment.Health != "failed" || view.Groups[2].Highlights[0].ID != "medium-scheduler100-v6-hour" || view.Groups[2].Highlights[1].Assessment.Execution != "failed" {
		t.Fatal("wrong references")
	}
	var out bytes.Buffer
	if err := core.RenderOverview(&out, view); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, text := range []string{"75%", "100%", "150%", "106/900=11.777778%", "86/900=9.555556%", "76/1200HTTP429=6.333333%", "7888/144000=5.477778%", "met-not-qualified", "2/2/2", "2/2/1", "INCONCLUSIVE", "UNUSABLE"} {
		if !strings.Contains(html, text) {
			t.Errorf("missing supplied evidence %q", text)
		}
	}
	for _, text := range []string{"fetch(", "XMLHttpRequest", "<link ", "<img ", "<script src", ".innerHTML", "/home/", "_artifacts/"} {
		if strings.Contains(html, text) {
			t.Errorf("unexpected %q", text)
		}
	}
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(` id="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if ids[m[1]] {
			t.Fatal("duplicate anchor", m[1])
		}
		ids[m[1]] = true
	}
	for _, m := range regexp.MustCompile(` href="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		href := m[1]
		if strings.HasPrefix(href, "#") {
			if !ids[href[1:]] {
				t.Fatal("missing local anchor", href)
			}
			continue
		}
		parts := strings.SplitN(href, "#", 2)
		data, err := os.ReadFile(filepath.Join(base, parts[0]))
		if err != nil {
			t.Fatal("missing local file", href, err)
		}
		if len(parts) == 2 && !bytes.Contains(data, []byte(`id="`+parts[1]+`"`)) {
			t.Fatal("missing preserved pane", href)
		}
	}
	for _, g := range view.Groups {
		for _, p := range g.Panes {
			for _, r := range p.Runs {
				if !strings.Contains(html, r.ID) {
					t.Fatal("missing register run", r.ID)
				}
			}
		}
	}
}

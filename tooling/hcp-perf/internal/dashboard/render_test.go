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
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This fixture is synthetic and must never be published as measured evidence.
func renderFixture() Dataset {
	distribution := &Distribution{Count: 20, Expected: 22, Missing: 2, Min: .1, Q1: .2, Median: .3, Q3: .4, P95: .7, P99: .8, Max: 1, WhiskerLow: .1, WhiskerHigh: .8, OutlierCount: 1}
	memory := &Distribution{Count: 20, Expected: 22, Missing: 2, Min: 100, Q1: 200, Median: 300, Q3: 400, P95: 700, P99: 800, Max: 1000, WhiskerLow: 100, WhiskerHigh: 800, OutlierCount: 1}
	request, limit, threshold := .25, 1.0, .05
	size := Size{
		ID: "small", Name: "Small", Revision: "Synthetic test revision", Status: "Candidate",
		Description: "Synthetic test fixture, not measured results", DefaultRun: "hour",
		Budget:     Budget{FeeUSD: .25, TargetUSD: .1875, ModeledUSD: .1874427, TargetCPUCores: 3.722, TargetMemoryGiB: 14.888, ChargedCPUCores: 3.72, ChargedMemoryGiB: 14.884, Scope: "Synthetic scope; excludes shared services"},
		Components: []Component{{ID: "kas", Name: "Kube apiserver"}, {ID: "missing", Name: "Missing component"}, {ID: "zero", Name: "Zero component"}},
	}
	for i := range 39 {
		size.Components = append(size.Components, Component{ID: fmt.Sprintf("component-%d", i), Name: fmt.Sprintf("Synthetic component %d", i)})
	}
	for i, id := range []string{"screen-one", "screen-repeat", "unloaded", "strict", "hour"} {
		run := Run{
			ID: id, Label: "Synthetic " + id, Cohort: "pinned-envelope", Kind: "screen", Population: "loaded", OfferedQPS: 5,
			PlannedSeconds: 300, ActualSeconds: 300, ApplicationPods: 300,
			Assessment: Assessment{Execution: "complete", RequestQuality: "PASS", Health: "FAILED", Capture: "partial", Summary: "Synthetic supplied assessment"},
			Requests:   Requests{Offered: 1000, Dispatched: 1000, Successful: 970, DegradedUnion: 20, Rejected429: 10, Timeouts: 15, MaxDegradedFraction: &threshold},
			Latencies:  []MetricDistribution{{ID: "ack", Label: "Canary ACK", Unit: "s", Distribution: distribution, Censored: 2}, {ID: "ready", Label: "Canary Ready", Unit: "s"}},
			Resources:  []ComponentUsage{{ComponentID: "kas", Replicas: 3, Containers: 6, CPU: distribution, Memory: memory, CPURequest: &request, CPULimit: &limit}, {ComponentID: "missing"}, {ComponentID: "zero", CPU: &Distribution{Count: 20}, Memory: &Distribution{Count: 20}}},
			WholeHCP:   []MetricDistribution{{ID: "cpu", Distribution: distribution}},
			Notes:      []string{"Synthetic diagnostic note"}, Sources: []string{"fixture"},
		}
		if i == 2 {
			run.Population = "unloaded"
			run.ApplicationPods = 0
			run.Requests = Requests{}
		}
		if i == 3 {
			run.Cohort = "strict-policy"
			run.Requests.MaxDegradedFraction = nil
			run.ActualSeconds = 150
		}
		if i == 4 {
			run.Kind = "confirmation"
			run.PlannedSeconds, run.ActualSeconds = 3600, 3600
		}
		size.Runs = append(size.Runs, run)
	}
	return Dataset{SchemaVersion: SchemaVersion, Title: "Synthetic dashboard test", Sizes: []Size{size}, Sources: []Source{{ID: "fixture", Label: "Synthetic source", SHA256: strings.Repeat("a", 64)}}}
}

func TestRenderStandalone(t *testing.T) {
	var output bytes.Buffer
	if err := Render(&output, renderFixture()); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, text := range []string{
		"default-src 'none'", "connect-src 'none'", "img-src 'none'", "font-src 'none'", "base-uri 'none'", "form-action 'none'",
		"$0.1874427", "3.722", "14.888", "14.884", "Reference hour attempt", "Request quality: PASS", "Health: FAILED",
		"Synthetic screen-repeat", "available without JavaScript", "No external requests.", "SHA-256", "Not populated",
	} {
		if !strings.Contains(html, text) {
			t.Errorf("rendered output missing %q", text)
		}
	}
	for _, text := range []string{"<script src", "<link ", "<img ", "fetch(", "XMLHttpRequest", ".innerHTML", "#ZgotmplZ"} {
		if strings.Contains(html, text) {
			t.Errorf("rendered output contains forbidden token %q", text)
		}
	}
}

func TestRenderEscapesDataset(t *testing.T) {
	dataset := renderFixture()
	payload := `</script><script>globalThis.injected=true</script><img src=x onerror=alert(1)> & " quoted`
	dataset.Title = payload
	dataset.Sizes[0].Name = payload
	dataset.Sizes[0].Runs[0].Notes = []string{payload}
	dataset.Sources[0].Label = payload
	var output bytes.Buffer
	if err := Render(&output, dataset); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	if strings.Contains(html, payload) || strings.Contains(html, "<img src=x") || strings.Count(html, "<script") != 2 {
		t.Fatal("dataset escaped its HTML or script context")
	}
	start := strings.Index(html, `<script id="dashboard-data" type="application/json">`) + len(`<script id="dashboard-data" type="application/json">`)
	end := strings.Index(html[start:], "</script>") + start
	var decoded Dataset
	if err := json.Unmarshal([]byte(html[start:end]), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Title != payload || decoded.Sizes[0].Runs[0].Notes[0] != payload {
		t.Fatal("safe JSON embedding changed user data")
	}
}

func TestRenderEmptySize(t *testing.T) {
	var output bytes.Buffer
	dataset := Dataset{SchemaVersion: SchemaVersion, Title: "Synthetic empty dataset", Sizes: []Size{{ID: "empty", Name: "Empty"}}}
	if err := Render(&output, dataset); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"No reference run populated.", "Not populated. No measured runs supplied.", "No source citations supplied."} {
		if !strings.Contains(output.String(), text) {
			t.Errorf("empty dataset missing %q", text)
		}
	}
}

type failingRenderWriter struct{}

func (failingRenderWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRenderBoundsAndErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Dataset)
	}{
		{"schema", func(d *Dataset) { d.SchemaVersion = "wrong" }},
		{"no sizes", func(d *Dataset) { d.Sizes = nil }},
		{"oversized", func(d *Dataset) { d.Title = strings.Repeat("x", MaxInputBytes) }},
		{"nonfinite", func(d *Dataset) { d.Sizes[0].Budget.ModeledUSD = math.NaN() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := renderFixture()
			test.edit(&d)
			var output bytes.Buffer
			if err := Render(&output, d); err == nil || output.Len() != 0 {
				t.Fatalf("invalid input wrote %d bytes or returned no error: %v", output.Len(), err)
			}
		})
	}
	if err := Render(failingRenderWriter{}, renderFixture()); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writer error was lost: %v", err)
	}
}

func TestDashboardJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	cmd := exec.Command(node, "--check", "assets/app.js")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript syntax: %v\n%s", err, output)
	}
}

func TestDashboardOfflineBrowser(t *testing.T) {
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		chrome, err = exec.LookPath("chromium")
	}
	if err != nil {
		t.Skip("Chrome/Chromium unavailable")
	}
	var output bytes.Buffer
	if err := Render(&output, renderFixture()); err != nil {
		t.Fatal(err)
	}
	probe := `<script>(async () => {
const checks = [], check = (name, value) => checks.push([name, Boolean(value)]);
const id = name => document.getElementById(name);
const change = (name, value, event = 'change') => { id(name).value = value; id(name).dispatchEvent(new Event(event)); };
const tick = () => new Promise(resolve => setTimeout(resolve, 20));
try {
check('workspace initialized', !id('workspace').hidden);
check('default cohort screen', id('cohort').value === 'pinned-envelope' && id('kind').value === 'screen');
check('fixed latest confirmation', document.querySelector('.reference-summary').textContent.includes('Synthetic hour'));
check('independent default selected run', id('selected-run').value === 'hour');
check('repetitions not pooled', document.querySelectorAll('#latency-charts .chart-panel:first-child .box-row').length === 3);
check('missing distribution explicit', id('latency-charts').textContent.includes('Not tested/no completed samples'));
check('all points selectable', id('run-points').children.length === 5);
check('envelope points not pooled', document.querySelectorAll('.envelope-point').length === 5);
check('envelope shapes', document.querySelectorAll('[data-shape="hour"]').length === 1 && document.querySelectorAll('[data-shape="interrupted"]').length === 1);
check('run register collapsed', !document.querySelector('.run-register').open);
const repeatedPoints = [...document.querySelectorAll('.envelope-point[data-pods="300"][data-qps="5"]')];
check('repeats separated', new Set(repeatedPoints.map(point => point.getAttribute('transform'))).size === repeatedPoints.length);
check('supplied separate status', id('selected-badges').textContent.includes('Request quality: PASS') && id('selected-badges').textContent.includes('Health: FAILED'));
check('missing hard canary unknown', id('incidents').firstElementChild.textContent === 'UnknownHard canary failures');
check('accessible sample coverage', document.querySelector('#latency-charts svg').getAttribute('aria-label').includes('missing 2'));
check('unknown censoring not zero', id('component-detail').textContent.includes('Select a tile'));
change('population', 'loaded');
check('loaded filter', document.querySelectorAll('#latency-charts .chart-panel:first-child .box-row').length === 2);
change('cohort', 'strict-policy');
check('cohort filter', document.querySelectorAll('#latency-charts .chart-panel:first-child .box-row').length === 1);
document.querySelector('.envelope-point[data-run="strict"]').dispatchEvent(new KeyboardEvent('keydown', {key: 'Enter', bubbles: true}));
check('keyboard envelope selection', id('selected-run').value === 'strict');
check('legacy strict policy not zero', id('request-outcomes').textContent.includes('Legacy strict policy') && !id('request-outcomes').textContent.includes('Threshold 0%'));
check('legacy point neutral', document.querySelector('.envelope-point[data-run="strict"]').classList.contains('unknown'));
change('kind', 'confirmation');
check('empty scope', id('latency-charts').textContent.includes('No latency distributions'));
document.querySelector('#run-points button[data-run="hour"]').click();
check('case aligns scope', id('cohort').value === 'pinned-envelope' && id('kind').value === 'confirmation');
id('tab-latency').dispatchEvent(new KeyboardEvent('keydown', {key: 'ArrowRight', bubbles: true}));
check('arrow tab navigation', id('tab-composition').getAttribute('aria-selected') === 'true' && document.activeElement === id('tab-composition') && id('panel-latency').hidden);
check('positive tiles only', document.querySelectorAll('.tile').length === 2);
check('unavailable not zero', id('treemaps').textContent.includes('40 components unavailable; 1 measured zero'));
check('unavailable legend retained', [...document.querySelectorAll('.component-legend .unavailable')].some(node => node.textContent === 'Missing component Unavailable'));
check('whole HCP separate', id('treemaps').textContent.includes('not whole-HCP p75') && id('treemaps').textContent.includes('Whole-HCP p75:'));
const tileColor = document.querySelector('.tile').style.background;
document.querySelector('.tile').click();
check('component details focus', document.activeElement === id('component-detail'));
check('unknown cap retained', id('component-detail').textContent.includes('Memory limitUnknown'));
check('component totals', id('component-detail').textContent.includes('3 replicas / 6 containers'));
check('budget ratio', id('component-detail').textContent.includes('160%'));
change('percentile', '2', 'input');
check('discrete percentile', id('percentile-output').textContent === 'p99' && id('percentile').getAttribute('aria-valuetext') === 'p99');
check('stable tile color', document.querySelector('.tile').style.background === tileColor);
change('memory-metric', 'rss', 'input');
check('nil memory empty', id('treemaps').textContent.includes('No positive measured component values'));
change('run-slider', '1', 'input');
check('repeat selectable', id('selected-run').value === 'screen-repeat' && id('run-output').textContent.includes('case 2'));
change('selected-run', 'unloaded');
check('zero dispatch not measured', id('request-outcomes').textContent.includes('Not measured:') && !id('request-outcomes').querySelector('meter'));
id('tab-composition').dispatchEvent(new KeyboardEvent('keydown', {key: 'End', bubbles: true}));
check('end tab navigation', id('tab-resources').getAttribute('aria-selected') === 'true');
check('all components discoverable', document.querySelectorAll('.component-distribution').length === 42);
change('component-search', 'KAS', 'input');
check('component human alias', document.querySelectorAll('.component-distribution').length === 1);
const fold = document.querySelector('.component-distribution'); fold.open = true; fold.dispatchEvent(new Event('toggle'));
check('CPU memory comparison panels', fold.querySelectorAll('.chart-panel').length === 2);
check('request and limit ticks', fold.querySelectorAll('line[stroke-dasharray]').length === 1);
check('finite SVG', [...document.querySelectorAll('svg *')].every(node => [...node.attributes].every(attr => !/NaN|Infinity/.test(attr.value))));
check('resources no page overflow', document.documentElement.scrollWidth <= innerWidth);
id('tab-resources').dispatchEvent(new KeyboardEvent('keydown', {key: 'Home', bubbles: true}));
check('home tab navigation', id('tab-latency').getAttribute('aria-selected') === 'true');
location.hash = 'Medium'; await tick();
check('unpopulated size route', !id('placeholder').hidden && id('placeholder-copy').textContent.includes('No measured results') && id('workspace').hidden);
location.hash = 'methodology'; await tick();
check('documentation placeholder', id('placeholder-title').textContent === 'Methodology' && id('placeholder-copy').textContent.includes('later edition'));
location.hash = 'small'; await tick();
check('return size route', !id('workspace').hidden && id('placeholder').hidden);
if (matchMedia('(max-width: 800px)').matches) {
  check('mobile nav collapsed', getComputedStyle(id('navigation')).display === 'none');
  id('nav-toggle').click();
  check('mobile nav expanded', id('nav-toggle').getAttribute('aria-expanded') === 'true' && getComputedStyle(id('navigation')).display !== 'none');
  id('navigation').dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true}));
  check('mobile escape focus', document.activeElement === id('nav-toggle') && id('nav-toggle').getAttribute('aria-expanded') === 'false');
}

check('latency no page overflow', document.documentElement.scrollWidth <= innerWidth);
id('tab-composition').click();
check('composition no page overflow', document.documentElement.scrollWidth <= innerWidth);
check('inert citations', !id('run-metadata').querySelector('a'));
check('no network resource requests', performance.getEntriesByType('resource').length === 0);
} catch (error) { checks.push(['exception: ' + error.stack, false]); }
const result = document.createElement('p'); result.id = 'browser-check'; result.textContent = checks.every(item => item[1]) ? 'passed' : JSON.stringify(checks.filter(item => !item[1])); document.body.append(result);
})();</script>`
	path := filepath.Join(t.TempDir(), "synthetic-dashboard.html")
	if err := os.WriteFile(path, []byte(strings.Replace(output.String(), "</body>", probe+"</body>", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, size := range []string{"1440,1000", "390,844"} {
		t.Run(size, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, chrome, "--headless", "--no-sandbox", "--disable-gpu", "--no-first-run", "--disable-background-networking", "--disable-dev-shm-usage", "--user-data-dir="+t.TempDir(), "--window-size="+size, "--virtual-time-budget=2000", "--dump-dom", "file://"+path)
			result, err := cmd.Output()
			if err != nil {
				t.Fatalf("Chrome: %v", err)
			}
			if !bytes.Contains(result, []byte(`id="browser-check">passed</p>`)) {
				text := string(result)
				if start := strings.LastIndex(text, `<p id="browser-check">`); start >= 0 {
					text = text[start:]
				}
				t.Fatalf("browser checks failed: %s", text)
			}
		})
	}
}

func TestDashboardRealSavedBrowser(t *testing.T) {
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Skip("Chrome unavailable")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	raw, err := os.ReadFile("../../docs/dashboard/small.json")
	if err != nil {
		t.Fatal(err)
	}
	var dataset Dataset
	if err := json.Unmarshal(raw, &dataset); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Render(&output, dataset); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	artifacts := os.Getenv("DASHBOARD_REVIEW_DIR")
	if artifacts != "" {
		directory = artifacts // Explicit opt-in; never publish test-generated HTML by default.
	}
	html := filepath.Join(directory, "small-review.html")
	if err := os.WriteFile(html, output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "testdata/review.mjs", chrome, html, t.TempDir(), artifacts)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("saved-data browser review: %v\n%s", err, result)
	} else {
		t.Log(string(result))
	}
}

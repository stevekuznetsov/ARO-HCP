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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../docs/cards/small-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func parseFixture(t *testing.T, d map[string]any) *Card {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRenderReviewedCard(t *testing.T) {
	d := fixture(t)
	var out bytes.Buffer
	if err := Render(&out, parseFixture(t, d), nil); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"Proposed Small v1 resource profile", "application Pods completed the population screens.", "requests were shed, and no OOM/restart was observed.", "No observed OOM / restart", "No node time series supplied", "not the later APF comparison", "API-window interpretation; partial source coverage:", "Live incomplete: true", "not true APF capacity", "No manual QPS coverage", "PSI", "Content-Security-Policy"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	previous := -1
	for _, section := range []string{"findings", "method", "contention", "clientload", "resources", "evidence"} {
		index := strings.Index(html, `id="`+section+`"`)
		if index <= previous {
			t.Errorf("section %s missing or out of order", section)
		}
		previous = index
	}
	for _, a := range at(d, "apfComparison.arms").([]any) {
		arm := a.(map[string]any)
		for _, k := range []string{"offered", "successfulBodies", "http429", "timeouts", "generatorLimited", "hardCompletedCanaries", "boundedCompletedRollouts"} {
			if !strings.Contains(html, ">"+number(arm[k], 0)+"<") && !strings.Contains(html, ">"+number(arm[k], 0)+" /") {
				t.Errorf("missing rendered %s=%v", k, arm[k])
			}
		}
	}
	for _, bad := range []string{`<script src=`, `<link `, `<img `, `<iframe`, `fetch(`, `XMLHttpRequest`, `@import`, `url(`, `href="http`, "NaN", "Infinity", "ZgotmplZ"} {
		if strings.Contains(html, bad) {
			t.Errorf("unsafe or invalid output: %q", bad)
		}
	}
	if strings.Index(html, "id=\"findings\"") > strings.Index(html, "id=\"method\"") {
		t.Fatal("findings must come first")
	}
}

func TestRenderUsesInputAndEscapes(t *testing.T) {
	d := fixture(t)
	payload := `</script><script>alert("x")</script><img src="https://evil.invalid">`
	d["name"] = "Custom size: " + payload
	d["scope"].(map[string]any)["history"] = payload
	d["population"].(map[string]any)["applicationPodsReady"] = float64(417)
	d["health"].(map[string]any)["safety"].(map[string]any)["observations"] = float64(23)
	arm := at(d, "apfComparison.arms").([]any)[0].(map[string]any)
	arm["successfulQPSActiveDenominatorIncludingDrainCompletions"] = 7.12345
	arm["successfulBodies"] = float64(1234)
	d["citations"] = []any{map[string]any{"id": payload, "sourceFile": "javascript:alert(1)", "sha256": payload}}
	var out bytes.Buffer
	if err := Render(&out, parseFixture(t, d), nil); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"Proposed Custom size resource profile", "417 application Pods were recorded", "7.12", "23 safety observations", ">1234<", "&lt;/script&gt;", `\u003c/script\u003e`, "javascript:alert(1)"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing dynamic/escaped content %q", want)
		}
	}
	if strings.Contains(html, payload) || strings.Contains(html, `href="javascript:`) {
		t.Fatal("source markup or source URL became active")
	}
}

func TestMissingSafetyIsUnknown(t *testing.T) {
	d := fixture(t)
	delete(d["health"].(map[string]any), "safety")
	var out bytes.Buffer
	if err := Render(&out, parseFixture(t, d), nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "No observed OOM / restart") || strings.Contains(out.String(), "no OOM/restart was observed") {
		t.Fatal("missing safety data claimed absence of crashes")
	}
	if !strings.Contains(out.String(), "unknown safety observations") {
		t.Fatal("missing count was not unknown")
	}
}

func TestNarrativesFollowMeasurements(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		absent []string
	}{
		{"no generator limits", func(d map[string]any) {
			for _, raw := range at(d, "apfComparison.arms").([]any) {
				raw.(map[string]any)["generatorLimited"] = float64(0)
			}
		}, []string{"Generator-limited offers were recorded", "biased plateau"}},
		{"no shedding", func(d map[string]any) {
			for _, raw := range at(d, "apfComparison.arms").([]any) {
				raw.(map[string]any)["http429"] = float64(0)
			}
		}, []string{"requests were shed, and"}},
		{"recorded OOM", func(d map[string]any) { at(d, "health.safety").(map[string]any)["recordedNewOOM"] = float64(1) }, []string{"No observed OOM / restart", "no OOM/restart was observed"}},
		{"no observations", func(d map[string]any) { at(d, "health.safety").(map[string]any)["observations"] = float64(0) }, []string{"No observed OOM / restart", "no OOM/restart was observed"}},
		{"no 100 shares", func(d map[string]any) {
			at(d, "apfComparison.arms").([]any)[1].(map[string]any)["shares"] = float64(75)
		}, []string{"100-share active window"}},
		{"missing 100 resource values", func(d map[string]any) {
			delete(at(d, "resourceUse.comparison").(map[string]any), "shares100BusiestKAS")
		}, []string{"100-share active window"}},
		{"null 100 resource values", func(d map[string]any) {
			m := at(d, "resourceUse.comparison.shares100BusiestKAS").(map[string]any)
			m["cgroupUsageMaxMiB"] = nil
			m["goManagedMaxMiBRounded"] = nil
		}, []string{"100-share active window"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := fixture(t)
			tc.change(d)
			var out bytes.Buffer
			if err := Render(&out, parseFixture(t, d), nil); err != nil {
				t.Fatal(err)
			}
			// Reviewed prose is retained verbatim in provenance; test renderer-authored text.
			html := strings.Split(out.String(), "Full reviewed card JSON")[0]
			for _, bad := range append(tc.absent, "rather than a crash", "Both generators were locally limited", "Prometheus and recorder Jobs use infra pools", "fully busy available host samples", "poor interactive latency remains material") {
				if strings.Contains(html, bad) {
					t.Errorf("unsupported narrative %q", bad)
				}
			}
			if !strings.Contains(html, "Observers should run on infra; node exporters on measured workers") {
				t.Fatal("missing placement guidance")
			}
		})
	}
}

func TestContentionSummaryValidation(t *testing.T) {
	wire, err := os.ReadFile("../../docs/cards/small-v1-contention.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"stale all-null median", func(d map[string]any) {
			at(d, "summary").([]any)[3].(map[string]any)["nodes"].([]any)[0].(map[string]any)["psiPercent"].(map[string]any)["median"] = float64(100)
		}},
		{"missing node ID", func(d map[string]any) {
			delete(d["summary"].([]any)[0].(map[string]any)["nodes"].([]any)[0].(map[string]any), "id")
		}},
		{"duplicate node ID", func(d map[string]any) {
			d["summary"].([]any)[0].(map[string]any)["nodes"].([]any)[1].(map[string]any)["id"] = "worker-1"
		}},
		{"unknown node ID", func(d map[string]any) {
			d["summary"].([]any)[0].(map[string]any)["nodes"].([]any)[0].(map[string]any)["id"] = "other"
		}},
		{"missing node", func(d map[string]any) {
			s := d["summary"].([]any)[0].(map[string]any)
			s["nodes"] = s["nodes"].([]any)[1:]
		}},
		{"duplicate summary", func(d map[string]any) { s := d["summary"].([]any); d["summary"] = append(s, s[0]) }},
		{"wrong expected grid", func(d map[string]any) { d["window"].(map[string]any)["expectedPointsPerNode"] = float64(125) }},
		{"off grid", func(d map[string]any) {
			d["series"].([]any)[0].(map[string]any)["points"].([]any)[1].(map[string]any)["timestamp"] = 1789065171.0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d map[string]any
			if err := json.Unmarshal(wire, &d); err != nil {
				t.Fatal(err)
			}
			tc.change(d)
			b, _ := json.Marshal(d)
			if _, err := ParseContention(b); err == nil {
				t.Fatal("accepted inconsistent summary")
			}
		})
	}
	for _, field := range []string{"sampleCount", "expectedSampleCount", "coverageFraction", "min", "max", "median", "firstTimestamp", "lastTimestamp", "fractionAbove90Percent"} {
		t.Run(field, func(t *testing.T) {
			var d map[string]any
			if err := json.Unmarshal(wire, &d); err != nil {
				t.Fatal(err)
			}
			stats := d["summary"].([]any)[2].(map[string]any)["nodes"].([]any)[0].(map[string]any)["busyPercent"].(map[string]any)
			stats[field] = float64(999)
			b, _ := json.Marshal(d)
			if _, err := ParseContention(b); err == nil {
				t.Fatalf("accepted stale %s", field)
			}
			delete(stats, field)
			b, _ = json.Marshal(d)
			if _, err := ParseContention(b); err == nil {
				t.Fatalf("accepted missing %s", field)
			}
		})
	}
}

func TestContentionHalfOpenStatistics(t *testing.T) {
	// Four expected grid slots, one absent slot and one explicit null. The
	// sample exactly at end belongs to the next window, not this summary.
	d := map[string]any{
		"schemaVersion": ContentionSchemaVersion, "id": "half-open",
		"units":  map[string]any{"timestamp": "unixSeconds", "busyPercent": "percent", "adversaryCores": "CPUcores", "psiPercent": "percent"},
		"window": map[string]any{"start": 0, "end": 25, "durationSeconds": 25, "expectedStepSeconds": 5, "expectedPointsPerNode": 5},
		"series": []any{map[string]any{"id": "node", "label": "Worker", "capacityCores": 4, "points": []any{
			map[string]any{"timestamp": 0, "busyPercent": 20}, map[string]any{"timestamp": 5, "busyPercent": 40}, map[string]any{"timestamp": 15, "busyPercent": nil}, map[string]any{"timestamp": 20, "busyPercent": 100},
		}}},
		"summary": []any{map[string]any{"id": "custom-arm", "start": 0, "end": 20, "durationSeconds": 20, "nodes": []any{map[string]any{
			"id":             "node",
			"busyPercent":    map[string]any{"sampleCount": 2, "expectedSampleCount": 4, "coverageFraction": 0.5, "min": 20, "max": 40, "median": 30, "firstTimestamp": 0, "lastTimestamp": 5, "fractionAbove90Percent": 0},
			"adversaryCores": map[string]any{"sampleCount": 0, "expectedSampleCount": 4, "coverageFraction": 0, "min": nil, "max": nil, "median": nil, "firstTimestamp": nil, "lastTimestamp": nil},
			"psiPercent":     map[string]any{"sampleCount": 0, "expectedSampleCount": 4, "coverageFraction": 0, "min": nil, "max": nil, "median": nil, "firstTimestamp": nil, "lastTimestamp": nil},
		}}}},
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseContention(b); err != nil {
		t.Fatalf("valid half-open summary rejected: %v", err)
	}
}

func TestContentionSummaryWindowNames(t *testing.T) {
	d := fixture(t)
	wire, err := os.ReadFile("../../docs/cards/small-v1-contention.json")
	if err != nil {
		t.Fatal(err)
	}
	var sidecar map[string]any
	if err := json.Unmarshal(wire, &sidecar); err != nil {
		t.Fatal(err)
	}
	for i, raw := range at(d, "apfComparison.arms").([]any) {
		arm := raw.(map[string]any)
		name := []string{"reference-arm", "treatment-arm"}[i]
		for _, s := range sidecar["summary"].([]any) {
			summary := s.(map[string]any)
			if summary["id"] == arm["id"] {
				summary["id"] = name
			}
		}
		arm["id"] = name
	}
	wire, err = json.Marshal(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseContention(wire)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Render(&out, parseFixture(t, d), c); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"reference-arm", "treatment-arm", "pre-start", "burners-running-before-api", "post-stop-command"} {
		if !strings.Contains(out.String(), "<th>"+id+"</th>") {
			t.Errorf("summary window %s absent from table", id)
		}
	}
	if strings.Contains(out.String(), "<th>timeline</th>") {
		t.Fatal("timeline duplicates the individual windows")
	}
}

func TestParseValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"future", func(d map[string]any) { d["schemaVersion"] = "hcp-perf.tshirt-card/v2" }},
		{"identity", func(d map[string]any) { delete(d, "id") }},
		{"date", func(d map[string]any) { d["measuredDate"] = "yesterday" }},
		{"section", func(d map[string]any) { d["health"] = nil }},
		{"negative", func(d map[string]any) {
			at(d, "apfComparison.arms").([]any)[0].(map[string]any)["http429"] = float64(-1)
		}},
		{"string number", func(d map[string]any) { d["population"].(map[string]any)["applicationPodsReady"] = "600" }},
		{"empty arms", func(d map[string]any) { d["apfComparison"].(map[string]any)["arms"] = []any{} }},
		{"backwards window", func(d map[string]any) {
			a := at(d, "apfComparison.arms").([]any)[0].(map[string]any)
			a["stopEpoch"] = a["startEpoch"]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := fixture(t)
			tc.change(d)
			b, _ := json.Marshal(d)
			if _, err := Parse(b); err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
	for _, b := range [][]byte{[]byte("null"), []byte("{} {}"), []byte(`{"schemaVersion":"hcp-perf.tshirt-card/v1","x":1e999}`), bytes.Repeat([]byte(" "), MaxInputBytes+1)} {
		if _, err := Parse(b); err == nil {
			t.Fatal("accepted invalid JSON")
		}
	}
}

func TestContention(t *testing.T) {
	d := fixture(t)
	wire, err := os.ReadFile("../../docs/cards/small-v1-contention.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseContention(wire)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Render(&out, parseFixture(t, d), c); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Worker 1", "cpuBusyPercent", "sourceEvidence", "100", "null", "burnerCPUCores", "unknown (0 / 36)", "not an idle whole-cluster control"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
	c.data["summary"].([]any)[2].(map[string]any)["start"] = float64(1)
	if err := Render(&out, parseFixture(t, d), c); err == nil {
		t.Fatal("accepted mismatched card")
	}
	for _, invalid := range []string{strings.Replace(string(wire), `"busyPercent":100.0`, `"busyPercent":101.0`, 1), strings.Replace(string(wire), `"timestamp":1789065170.85`, `"timestamp":1789065165.85`, 1), strings.Replace(string(wire), `"adversaryCores":null`, `"adversaryCores":4.1`, 1), strings.Replace(string(wire), "/v1", "/v2", 1)} {
		if _, err := ParseContention([]byte(invalid)); err == nil {
			t.Fatal("accepted invalid contention")
		}
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken writer") }
func TestWriterError(t *testing.T) {
	if err := Render(brokenWriter{}, parseFixture(t, fixture(t)), nil); err == nil {
		t.Fatal("lost write failure")
	}
}

func TestChartJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := strings.TrimSuffix(strings.TrimPrefix(pageJS, `{{define "script"}}<script>`), "</script>{{end}}")
	cmd := exec.Command(node, "--check")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("invalid embedded JavaScript: %v\n%s", err, output)
	}
}

func TestOfflineBrowser(t *testing.T) {
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Skip("Chrome unavailable")
	}
	d := fixture(t)
	b, err := os.ReadFile("../../docs/cards/small-v1-contention.json")
	if err != nil {
		t.Fatal(err)
	}
	contention, err := ParseContention(b)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise an axis other than four cores and an absent metric field.
	// Removing an explicit null leaves the reviewed summary statistics valid.
	node := contention.data["series"].([]any)[0].(map[string]any)
	node["capacityCores"] = float64(8)
	delete(node["points"].([]any)[0].(map[string]any), "adversaryCores")
	b, err = json.Marshal(contention.data)
	if err != nil {
		t.Fatal(err)
	}
	contention, err = ParseContention(b)
	if err != nil {
		t.Fatal(err)
	}
	var report bytes.Buffer
	if err := Render(&report, parseFixture(t, d), contention); err != nil {
		t.Fatal(err)
	}
	// Probe the real layout and rendered SVGs without a web server or browser
	// dependencies. CSP remains active and prevents any network connection.
	probe := `<script>const p=document.createElement("p");p.id="browser-check";p.textContent="svg="+document.querySelectorAll("svg").length+";overflow="+(document.documentElement.scrollWidth>innerWidth)+";invalid="+[...document.querySelectorAll("svg *")].some(e=>[...e.attributes].some(a=>/NaN|Infinity/.test(a.value)));if(document.documentElement.scrollWidth>innerWidth)p.textContent+=";wide="+[...document.querySelectorAll("main,section,article,figure,p,code,pre,details,div")].filter(e=>e.getBoundingClientRect().right>innerWidth).map(e=>e.tagName+"."+e.className+"#"+e.id).join(",");document.body.append(p);</script>`
	probe += `<script>(()=>{const data=JSON.parse(document.getElementById("report-data").textContent).contention;const checks=[];for(const [key,metric] of [["cpuBusyPercent","busyPercent"],["burnerCPUCores","adversaryCores"]]){const s=document.querySelector('svg[data-metric="'+key+'"]'),fig=s.closest("figure"),toggles=[...fig.querySelectorAll('input[type="checkbox"]')],rows=[...s.querySelectorAll(".coverage-row")],traces=[...s.querySelectorAll(".worker-trace")];checks.push(toggles.length===data.series.length&&toggles.every(t=>t.checked&&t.labels.length===1));checks.push(rows.length===data.series.length);data.series.forEach((n,i)=>{const valid=n.points.filter(p=>typeof p[metric]==="number").length;checks.push(rows[i].querySelectorAll('[data-sample="present"]').length===valid);checks.push(rows[i].querySelectorAll('[data-sample="missing"]').length===data.window.expectedPointsPerNode-valid);checks.push(traces[i].querySelectorAll("circle").length===valid);});toggles.slice(1).forEach(t=>t.click());checks.push(traces.every((t,i)=>getComputedStyle(t).display===(i===0?"inline":"none")));checks.push(rows.every(r=>getComputedStyle(r).display!=="none"));toggles.slice(1).forEach(t=>t.click());checks.push(traces.every(t=>getComputedStyle(t).display!=="none"));checks.push(+s.dataset.axisMax===(key==="cpuBusyPercent"?100:8));}const busy=document.querySelector('svg[data-metric="cpuBusyPercent"] .worker-trace path');checks.push((busy.getAttribute("d").match(/M/g)||[]).length>1);const p=document.createElement("p");p.id="worker-check";p.textContent=checks.every(Boolean)?"passed":"failed:"+checks.map((v,i)=>v?null:i).filter(v=>v!==null).join(",");document.body.append(p);})();</script>`
	dir := t.TempDir()
	path := filepath.Join(dir, "card.html")
	if err := os.WriteFile(path, []byte(strings.Replace(report.String(), "</body>", probe+"</body>", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, size := range []string{"1440,1000", "390,844"} {
		t.Run(size, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, chrome, "--headless", "--no-sandbox", "--disable-gpu", "--no-first-run", "--disable-background-networking", "--disable-dev-shm-usage", "--user-data-dir="+t.TempDir(), "--window-size="+size, "--dump-dom", "file://"+path)
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("browser: %v", err)
			}
			if !strings.Contains(string(output), `id="browser-check">svg=6;overflow=false;invalid=false</p>`) {
				t.Fatalf("browser did not render six valid offline charts without page overflow; probe=%s", browserProbe(string(output)))
			}
			if !strings.Contains(string(output), `stroke-dasharray="7 3"`) || !strings.Contains(string(output), "PSI stall") {
				t.Fatal("contention series or PSI table absent from browser output")
			}
			if !strings.Contains(string(output), `id="worker-check">passed</p>`) {
				t.Fatal("worker isolation, coverage strips, null handling or dynamic axis browser checks failed")
			}
		})
	}
}

func browserProbe(s string) string {
	i := strings.LastIndex(s, `id="browser-check"`)
	if i < 0 {
		return "missing"
	}
	return strings.SplitN(s[i:], "</p>", 2)[0]
}

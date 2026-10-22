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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func experienceFixture(t *testing.T) *Experience {
	t.Helper()
	b, err := os.ReadFile("../../docs/cards/small-v1-experience.json")
	if err != nil {
		t.Fatal(err)
	}
	e, err := ParseExperience(b)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestExperienceEvidence(t *testing.T) {
	e := experienceFixture(t)
	if len(e.data["sources"].([]any)) != 45 || len(e.data["canaries"].([]any)) != 5 {
		t.Fatal("lost shared baseline or source metadata")
	}
	for i, raw := range e.data["arms"].([]any) {
		a := raw.(map[string]any)
		bins := a["bins"].([]any)
		last := bins[len(bins)-1].(map[string]any)
		if len(bins) != 19 || len(a["canaries"].([]any)) != 6 || last["dispatched"] != float64(1) || last["completed"] != []float64{53, 63}[i] || last["start"].(float64) >= a["stop"].(float64) || last["end"] != a["drainEnd"] {
			t.Fatal("lost partial bin, drain or steady cycles")
		}
	}
	var out bytes.Buffer
	if err := RenderWithEvidence(&out, parseFixture(t, fixture(t)), nil, resourceFixture(t), e); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	previous := -1
	for _, section := range []string{"findings", "experience", "component-usage", "method"} {
		i := strings.Index(html, `id="`+section+`"`)
		if i <= previous {
			t.Fatalf("section %s missing or out of order", section)
		}
		previous = i
	}
	for _, want := range []string{"Client behavior: most requests shed, successful LISTs near 10s tail, protected progress still completed.", "65.1%", "66.0%", "15.64 QPS", "14.61 QPS", "17 cycles: 5 shared baseline; 6 steady at 50 shares; 6 steady at 100 shares", "all completed requests in the bin", "Target Offered QPS", "Ready includes ACK", "not evaluated", "elapsed/censored", `"experience":{"arms":`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{`<script src=`, `fetch(`, `XMLHttpRequest`, `href="http`, "NaN", "Infinity", "ZgotmplZ"} {
		if strings.Contains(html, bad) {
			t.Errorf("unsafe/invalid output %q", bad)
		}
	}
}

func TestExperienceValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any, map[string]any, map[string]any)
	}{
		{"version", func(d, a, b map[string]any) { d["schemaVersion"] = "hcp-perf.experience/v2" }},
		{"cardID", func(d, a, b map[string]any) { delete(d, "cardID") }},
		{"window", func(d, a, b map[string]any) { at(d, "window").(map[string]any)["end"] = float64(0) }},
		{"unit", func(d, a, b map[string]any) { at(d, "method").(map[string]any)["timestampUnit"] = "milliseconds" }},
		{"bin cadence", func(d, a, b map[string]any) { at(d, "method").(map[string]any)["binSeconds"] = float64(20) }},
		{"unknown source", func(d, a, b map[string]any) { at(a, "sourceIDs").(map[string]any)["requests"] = "absent" }},
		{"unknown method source", func(d, a, b map[string]any) { at(d, "method.sourceIDs").(map[string]any)["intent"] = "absent" }},
		{"duplicate source", func(d, a, b map[string]any) { s := d["sources"].([]any); d["sources"] = append(s, s[0]) }},
		{"hash", func(d, a, b map[string]any) { d["sources"].([]any)[0].(map[string]any)["sha256"] = "not-a-hash" }},
		{"duplicate arm", func(d, a, b map[string]any) { d["arms"] = []any{a, a} }},
		{"arm boundary", func(d, a, b map[string]any) { a["stop"] = a["start"] }},
		{"arm exact timestamp", func(d, a, b map[string]any) { a["startedRFC3339"] = "2026-09-10T18:35:00Z" }},
		{"target", func(d, a, b map[string]any) { a["targetOfferedQPS"].([]any)[1].(map[string]any)["qps"] = float64(50) }},
		{"gap", func(d, a, b map[string]any) { b["start"] = b["start"].(float64) + 1 }},
		{"bin width", func(d, a, b map[string]any) { b["end"] = b["end"].(float64) - 1 }},
		{"missing drain", func(d, a, b map[string]any) { a["bins"] = a["bins"].([]any)[:18] }},
		{"invented offers", func(d, a, b map[string]any) { b["offered"] = float64(500) }},
		{"fraction denominator", func(d, a, b map[string]any) { b["rejectedFraction"] = b["http429"].(float64) / 500 }},
		{"fraction range", func(d, a, b map[string]any) { b["rejectedFraction"] = float64(57) }},
		{"rate", func(d, a, b map[string]any) { b["successfulQPS"] = float64(1) }},
		{"whole rate", func(d, a, b map[string]any) { at(a, "totals").(map[string]any)["successfulQPS"] = float64(16) }},
		{"negative", func(d, a, b map[string]any) { b["timeouts"] = float64(-1) }},
		{"fractional count", func(d, a, b map[string]any) { b["dispatched"] = 499.5 }},
		{"missing count", func(d, a, b map[string]any) { delete(b, "completed") }},
		{"sum dispatch", func(d, a, b map[string]any) { b["dispatched"] = float64(498) }},
		{"terminal total", func(d, a, b map[string]any) { at(a, "totals").(map[string]any)["completed"] = float64(8370) }},
		{"overlap", func(d, a, b map[string]any) { at(a, "totals").(map[string]any)["http429TimeoutOverlap"] = float64(1) }},
		{"attempts", func(d, a, b map[string]any) { at(a, "totals").(map[string]any)["httpAttempts"] = float64(1) }},
		{"latency", func(d, a, b map[string]any) { b["listSuccessP95Seconds"] = float64(-1) }},
		{"latency order", func(d, a, b map[string]any) { b["latencyP50Seconds"] = float64(100) }},
		{"duplicate canary", func(d, a, b map[string]any) { c := a["canaries"].([]any); a["canaries"] = append(c, c[0]) }},
		{"unknown canary source", func(d, a, b map[string]any) { a["canaries"].([]any)[0].(map[string]any)["id"] = "absent" }},
		{"canary arm", func(d, a, b map[string]any) { a["canaries"].([]any)[0].(map[string]any)["armID"] = "baseline" }},
		{"canary status", func(d, a, b map[string]any) { a["canaries"].([]any)[0].(map[string]any)["status"] = "passed" }},
		{"missing completion", func(d, a, b map[string]any) {
			at(a["canaries"].([]any)[0].(map[string]any), "stages.ack").(map[string]any)["completed"] = nil
		}},
		{"duration", func(d, a, b map[string]any) {
			at(a["canaries"].([]any)[0].(map[string]any), "stages.ack").(map[string]any)["durationSeconds"] = float64(-1)
		}},
		{"duration exact", func(d, a, b map[string]any) {
			at(a["canaries"].([]any)[0].(map[string]any), "stages.ack").(map[string]any)["durationNanoseconds"] = float64(1)
		}},
		{"baseline expectation", func(d, a, b map[string]any) {
			at(d["canaries"].([]any)[0].(map[string]any), "stages.ack").(map[string]any)["expectedSeconds"] = float64(1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := experienceFixture(t).data
			a := d["arms"].([]any)[0].(map[string]any)
			tc.edit(d, a, a["bins"].([]any)[0].(map[string]any))
			b, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseExperience(b); err == nil {
				t.Fatal("accepted inconsistent experience")
			}
		})
	}
	for _, wire := range [][]byte{[]byte("null"), []byte("{} {}"), []byte(`{"schemaVersion":"hcp-perf.experience/v1","x":1e999}`), bytes.Repeat([]byte(" "), MaxExperienceBytes+1)} {
		if _, err := ParseExperience(wire); err == nil {
			t.Fatal("accepted invalid input")
		}
	}
}

func TestExperienceCumulativeCompletions(t *testing.T) {
	e := experienceFixture(t)
	a := e.data["arms"].([]any)[0].(map[string]any)
	bins := a["bins"].([]any)
	first, second := bins[0].(map[string]any), bins[1].(map[string]any)
	if second["completed"].(float64) <= second["dispatched"].(float64) {
		t.Fatal("fixture must exercise valid completion carryover in the second bin")
	}
	// Preserve the final dispatched total but move all 499 initial dispatches
	// after their completions. A final-total check alone cannot detect this.
	second["dispatched"] = second["dispatched"].(float64) + first["dispatched"].(float64)
	first["dispatched"] = float64(0)
	wire, err := json.Marshal(e.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseExperience(wire); err == nil || !strings.Contains(err.Error(), "bin 0: cumulative completed exceeds cumulative dispatched") {
		t.Fatalf("expected cumulative ordering failure, got %v", err)
	}
}

func TestOutcomesPartitionOfflineBrowser(t *testing.T) {
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Skip("Chrome unavailable")
	}
	for _, scenario := range []string{"no experience", "disjoint", "overlap cancels other errors"} {
		t.Run(scenario, func(t *testing.T) {
			c := parseFixture(t, fixture(t))
			var e *Experience
			if scenario != "no experience" {
				e = experienceFixture(t)
			}
			if scenario == "overlap cancels other errors" {
				for i, raw := range e.data["arms"].([]any) {
					a := raw.(map[string]any)
					totals := a["totals"].(map[string]any)
					totals["http429TimeoutOverlap"], totals["otherErrors"] = float64(1), float64(1)
					totals["http200Timeouts"] = totals["http200Timeouts"].(float64) - 1
					cardArm := at(c.data, "apfComparison.arms").([]any)[i].(map[string]any)
					cardArm["http200WithBodyTimeout"] = totals["http200Timeouts"]
					for _, raw := range a["bins"].([]any) {
						b := raw.(map[string]any)
						if b["timeouts"].(float64) > 0 && b["http429"].(float64) > 0 {
							b["otherErrors"] = float64(1)
							break
						}
					}
					if cardArm["successfulBodies"].(float64)+cardArm["http429"].(float64)+cardArm["timeouts"].(float64)+cardArm["generatorLimited"].(float64) != cardArm["offered"] {
						t.Fatal("regression requires equal whole-arm count sums despite overlap")
					}
				}
				wire, err := json.Marshal(e.data)
				if err != nil {
					t.Fatal(err)
				}
				e, err = ParseExperience(wire)
				if err != nil {
					t.Fatal(err)
				}
			}
			var report bytes.Buffer
			if err := RenderWithEvidence(&report, c, nil, nil, e); err != nil {
				t.Fatal(err)
			}
			probe := `<script>const s=document.querySelector('#outcomes-chart svg'),p=document.createElement('p');p.id='outcomes-check';p.textContent='rects='+s.querySelectorAll('rect').length+';warnings='+[...s.querySelectorAll('text')].filter(n=>n.textContent==='Disjoint partition not established; see explicit counts in table.').length;document.body.append(p);</script>`
			path := filepath.Join(t.TempDir(), "outcomes.html")
			if err := os.WriteFile(path, []byte(strings.Replace(report.String(), "</body>", probe+"</body>", 1)), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, chrome, "--headless", "--no-sandbox", "--disable-gpu", "--no-first-run", "--disable-background-networking", "--disable-dev-shm-usage", "--user-data-dir="+t.TempDir(), "--dump-dom", "file://"+path)
			output, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			want := `id="outcomes-check">rects=0;warnings=2</p>`
			if scenario == "disjoint" {
				want = `id="outcomes-check">rects=8;warnings=0</p>`
			}
			if !strings.Contains(string(output), want) {
				t.Fatalf("outcomes chart did not produce %s", want)
			}
		})
	}
}

func TestExperienceCardBinding(t *testing.T) {
	for _, field := range []string{"id", "shares", "startEpoch", "stopEpoch", "offered", "dispatched", "httpAttempts", "successfulBodies", "http429", "timeouts", "generatorLimited", "http200WithBodyTimeout", "successfulQPSWallIncludingDrain", "hardCompletedCanaries", "softDegradedCanaries"} {
		t.Run(field, func(t *testing.T) {
			c := parseFixture(t, fixture(t))
			a := at(c.data, "apfComparison.arms").([]any)[0].(map[string]any)
			if field == "id" {
				a[field] = "other"
			} else {
				a[field] = a[field].(float64) + 1
			}
			var out bytes.Buffer
			if err := RenderWithEvidence(&out, c, nil, nil, experienceFixture(t)); err == nil || out.Len() != 0 {
				t.Fatal("accepted mismatched card or wrote partial output")
			}
		})
	}
	c := parseFixture(t, fixture(t))
	e := experienceFixture(t)
	e.data["cardID"] = "other"
	if err := RenderWithEvidence(&bytes.Buffer{}, c, nil, nil, e); err == nil {
		t.Fatal("accepted wrong card ID")
	}
	if err := RenderWithEvidence(&bytes.Buffer{}, c, nil, nil, &Experience{}); err == nil {
		t.Fatal("accepted unparsed experience")
	}
	r := resourceFixture(t)
	r.data["window"].(map[string]any)["end"] = at(r.data, "window.end").(float64) + 1
	if err := RenderWithEvidence(&bytes.Buffer{}, c, nil, r, experienceFixture(t)); err == nil {
		t.Fatal("accepted mismatched windows")
	}
	e = experienceFixture(t)
	payload := `</script><script>alert("x")</script>`
	e.data["method"].(map[string]any)["untrusted"] = payload
	var out bytes.Buffer
	if err := RenderWithEvidence(&out, c, nil, nil, e); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), payload) || !strings.Contains(out.String(), `\u003c/script\u003e`) {
		t.Fatal("unsafe experience embedding")
	}
	if err := RenderWithEvidence(brokenWriter{}, c, nil, nil, e); err == nil {
		t.Fatal("lost writer failure")
	}
}

func missingExperience(t *testing.T) (*Card, *Experience) {
	t.Helper()
	c, e := parseFixture(t, fixture(t)), experienceFixture(t)
	a := e.data["arms"].([]any)[0].(map[string]any)
	bins := a["bins"].([]any)
	// Move the first bin's observations into its neighbor, preserving totals.
	// This deliberately permits completion QPS above the configured offered rate.
	b0, b1 := bins[0].(map[string]any), bins[1].(map[string]any)
	for _, k := range []string{"completed", "successful", "http429", "timeouts", "otherErrors"} {
		b1[k] = b1[k].(float64) + b0[k].(float64)
		b0[k] = float64(0)
	}
	for _, b := range []map[string]any{b0, b1} {
		b["successfulQPS"] = b["successful"].(float64) / 10
		b["completionQPS"] = b["completed"].(float64) / 10
	}
	b0["rejectedFraction"] = nil
	b1["rejectedFraction"] = b1["http429"].(float64) / b1["completed"].(float64)
	for _, k := range []string{"latencyP50Seconds", "latencyP95Seconds", "successLatencyP95Seconds", "getSuccessP95Seconds", "listSuccessP95Seconds"} {
		b0[k] = nil
	}
	// An interior null must break the line, not connect adjacent observed bins.
	bins[5].(map[string]any)["getSuccessP95Seconds"] = nil
	cycle := a["canaries"].([]any)[0].(map[string]any)
	cycle["status"] = "failed"
	ack := at(cycle, "stages.ack").(map[string]any)
	ack["completed"], ack["completedRFC3339"] = nil, nil
	cleanup := at(cycle, "stages.cleanup").(map[string]any)
	for _, k := range []string{"start", "completed", "startedRFC3339", "completedRFC3339", "durationSeconds", "durationNanoseconds", "expectedSeconds", "hardDeadlineSeconds"} {
		cleanup[k] = nil
	}
	cardArm := at(c.data, "apfComparison.arms").([]any)[0].(map[string]any)
	cardArm["hardCompletedCanaries"] = float64(5)
	b, err := json.Marshal(e.data)
	if err != nil {
		t.Fatal(err)
	}
	e, err = ParseExperience(b)
	if err != nil {
		t.Fatal(err)
	}
	return c, e
}

func TestExperienceMissingAndFailed(t *testing.T) {
	c, e := missingExperience(t)
	var out bytes.Buffer
	if err := RenderWithEvidence(&out, c, nil, nil, e); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"Incomplete or failed canaries:", "shares50-canary-0 (failed)", "unknown (elapsed/censored", "<details open><summary>Every canary cycle"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing failure/unknown evidence %q", want)
		}
	}
	if strings.Contains(html, "protected progress still completed.") {
		t.Fatal("failure inherited success narrative")
	}
	for _, k := range []string{"http429", "dispatched", "successfulLISTP95Seconds", "hardCompletedCanaries"} {
		c := parseFixture(t, fixture(t))
		delete(at(c.data, "apfComparison.arms").([]any)[0].(map[string]any), k)
		if strings.Contains(clientBehavior(c), "most requests shed") {
			t.Fatalf("missing %s inherited degradation narrative", k)
		}
	}
	out.Reset()
	if err := RenderWithEvidence(&out, c, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No experience time series supplied") {
		t.Fatal("absent experience not explicit")
	}
}

func TestExperienceUnknownPhaseAndOverlappingOutcomes(t *testing.T) {
	c, e := missingExperience(t)
	a := e.data["arms"].([]any)[0].(map[string]any)
	cycle := a["canaries"].([]any)[0].(map[string]any)
	cycle["status"] = "unknown"
	for _, name := range []string{"ack", "ready", "cleanup"} {
		at(cycle, "stages."+name).(map[string]any)["expectedSeconds"] = nil
	}
	// No complete soft assessment: do not mistake missing expectations for zero
	// degradation or overwrite the independent card's recorded aggregate.
	b, err := json.Marshal(e.data)
	if err != nil {
		t.Fatal(err)
	}
	e, err = ParseExperience(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := RenderWithEvidence(&bytes.Buffer{}, c, nil, nil, e); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"completed": float64(10), "successful": float64(3), "http429": float64(5), "timeouts": float64(3), "otherErrors": float64(0), "http429TimeoutOverlap": float64(1), "successfulQPS": .3, "completionQPS": float64(1), "rejectedFraction": .5}
	if err := experienceRates(m, 10, true); err != nil {
		t.Fatalf("429 and timeout are overlapping flags, not disjoint categories: %v", err)
	}
	m["http429TimeoutOverlap"] = float64(0)
	if err := experienceRates(m, 10, true); err == nil {
		t.Fatal("accepted incorrect overlap")
	}
}

func TestExperienceJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := strings.TrimSuffix(strings.TrimPrefix(experienceJS, `{{define "experience-script"}}<script>`), "</script>{{end}}")
	cmd := exec.Command(node, "--check")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("invalid experience JavaScript: %v\n%s", err, output)
	}
}

func TestExperienceOfflineBrowser(t *testing.T) {
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Skip("Chrome unavailable")
	}
	for _, missing := range []bool{false, true} {
		c, e := parseFixture(t, fixture(t)), experienceFixture(t)
		if missing {
			c, e = missingExperience(t)
		}
		var report bytes.Buffer
		if err := RenderWithEvidence(&report, c, nil, nil, e); err != nil {
			t.Fatal(err)
		}
		probe := `<script>(()=>{const checks=[],e=JSON.parse(document.getElementById("report-data").textContent).experience;const check=(name,v)=>checks.push([name,!!v]);const root=document.getElementById("experience"),ids=["experience-throughput","experience-rejections","experience-latency","experience-canaries"],charts=ids.map(id=>document.querySelector("#"+id+" svg"));check("four charts",charts.every(Boolean));check("no page overflow",document.documentElement.scrollWidth<=innerWidth);check("no invalid SVG",!charts.some(s=>[...s.querySelectorAll("*")].some(n=>[...n.attributes].some(a=>/NaN|Infinity/.test(a.value)))));check("unique IDs",new Set([...document.querySelectorAll("[id]")].map(n=>n.id)).size===document.querySelectorAll("[id]").length);charts.forEach((s,i)=>{const box=s.getBoundingClientRect(),host=s.parentElement.getBoundingClientRect();check("chart dimensions "+i,box.width>=760&&box.height>200&&host.right<=innerWidth&&host.width>0);check("shared axis "+i,+s.dataset.windowStart===e.window.start&&+s.dataset.windowEnd===e.window.end);check("contained text "+i,[...s.querySelectorAll("text")].every(n=>{const b=n.getBBox();return b.x>=0&&b.y>=0&&b.x+b.width<=900&&b.y+b.height<=s.viewBox.baseVal.height;}));if(i<3){const caption=s.closest("figure").querySelector("figcaption").getBoundingClientRect();check("caption overlap "+i,caption.top>=box.bottom);}if(i<2)check("matching UTC ticks "+i,JSON.stringify([...s.querySelectorAll("[data-utc-tick]")].map(t=>t.dataset.utcTick))===JSON.stringify([...charts[2].querySelectorAll("[data-utc-tick]")].map(t=>t.dataset.utcTick)));});check("429 percent axis",charts[1].dataset.axisMax==="100");check("options off",["total","outcome","timeout","hard"].every(k=>!document.getElementById("experience-"+k+"-toggle").checked));check("baseline on",document.getElementById("experience-baseline-toggle").checked);check("no optional series",!root.querySelector('[data-series="completionQPS"],[data-series="latencyP95Seconds"],[data-reference="hard"],[data-reference="client-timeout"]'));const cycles=[...e.canaries,...e.arms.flatMap(a=>a.canaries)];check("51 table rows",document.querySelectorAll("#experience-cycle-table tbody tr").length===cycles.length*3);e.arms.forEach(a=>{const group=charts[0].querySelector('[data-series="successfulQPS"][data-arm="'+a.id+'"]');check("one segment per populated bin "+a.id,group.querySelectorAll("line").length===a.bins.filter(b=>b.completed>0).length);const last=a.bins.at(-1),segment=group.querySelector('line[data-bin-start="'+last.start+'"]');check("actual drain endpoint "+a.id,+segment.dataset.binEnd===a.drainEnd&&+segment.dataset.value===last.successfulQPS);const reject=charts[1].querySelector('[data-series="rejectedFraction"][data-arm="'+a.id+'"]');check("fraction percent "+a.id,[...reject.querySelectorAll("line")].every(n=>{const b=a.bins.find(b=>b.start===+n.dataset.binStart);return +n.dataset.value===100*b.rejectedFraction;}));const target=charts[0].querySelector('[data-series="targetOfferedQPS"][data-arm="'+a.id+'"]');check("active then zero target "+a.id,target.querySelector('[data-value="'+a.offeredQPS+'"]')&&target.querySelector('[data-value="0"]'));for(const key of ["getSuccessP95Seconds","listSuccessP95Seconds"]){const g=charts[2].querySelector('[data-series="'+key+'"][data-arm="'+a.id+'"]');check("raw quantiles "+a.id+key,[...g.querySelectorAll("circle")].every(n=>+n.dataset.value===a.bins.find(b=>b.start===+n.dataset.binStart)[key]));if(a.bins[5][key]===null)check("null latency gap",(g.querySelector("path").getAttribute("d").match(/M/g)||[]).length===2);}});["ack","ready","cleanup"].forEach(key=>{const g=charts[3].querySelector('[data-phase="'+key+'"]');check("baseline once "+key,g.querySelectorAll('[data-baseline="true"]').length===5);check("recorded completions "+key,g.querySelectorAll("circle[data-value]").length===cycles.filter(c=>typeof c.stages[key].completed==="number"&&typeof c.stages[key].durationSeconds==="number").length);check("baseline no soft line "+key,[...g.querySelectorAll('[data-reference="soft"]')].every(n=>!n.dataset.cycle.startsWith("baseline")));});check("separate canary scales",+charts[3].querySelector('[data-phase="ack"]').dataset.axisMax<+charts[3].querySelector('[data-phase="ready"]').dataset.axisMax);const incomplete=cycles.filter(c=>c.status!=="completed");if(incomplete.length){check("failure not zero",charts[3].querySelectorAll("[data-incomplete]").length===2);check("failure visible",root.querySelector('[role="status"]')&&document.getElementById("experience-cycle-table").closest("details").open);}document.getElementById("experience-baseline-toggle").click();check("baseline hides only baseline",!root.querySelector('[data-baseline="true"]')&&root.querySelectorAll('[data-baseline="false"]').length===cycles.filter(c=>c.phase!=="baseline").reduce((n,c)=>n+Object.values(c.stages).filter(p=>typeof p.completed==="number").length,0));document.getElementById("experience-hard-toggle").click();check("hard toggle",root.querySelectorAll('[data-reference="hard"]').length>0&&+root.querySelector('[data-phase="ready"]').dataset.axisMax>=120);document.getElementById("experience-total-toggle").click();check("total toggle",root.querySelectorAll('[data-series="completionQPS"]').length===2&&+document.querySelector("#experience-throughput svg").dataset.axisMax>50);document.getElementById("experience-outcome-toggle").click();document.getElementById("experience-timeout-toggle").click();check("latency toggles",root.querySelectorAll('[data-series="latencyP95Seconds"]').length===2&&root.querySelector('[data-reference="client-timeout"]'));const p=document.createElement("p");p.id="experience-browser-check";p.textContent=checks.every(([,ok])=>ok)?"passed":checks.filter(([,ok])=>!ok).map(([name])=>name).join(", ");document.body.append(p);})();</script>`
		path := filepath.Join(t.TempDir(), "experience.html")
		if err := os.WriteFile(path, []byte(strings.Replace(report.String(), "</body>", probe+"</body>", 1)), 0600); err != nil {
			t.Fatal(err)
		}
		for _, size := range []string{"1440,1000", "390,844"} {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cmd := exec.CommandContext(ctx, chrome, "--headless", "--no-sandbox", "--disable-gpu", "--no-first-run", "--disable-background-networking", "--disable-dev-shm-usage", "--user-data-dir="+t.TempDir(), "--window-size="+size, "--dump-dom", "file://"+path)
			output, err := cmd.Output()
			cancel()
			if err != nil {
				t.Fatalf("browser: %v", err)
			}
			if !strings.Contains(string(output), `id="experience-browser-check">passed</p>`) {
				probe := "missing"
				if i := strings.LastIndex(string(output), `id="experience-browser-check"`); i >= 0 {
					probe = strings.SplitN(string(output)[i:], "</p>", 2)[0]
				}
				t.Errorf("%s missing=%v: %s", size, missing, probe)
			}
		}
	}
}

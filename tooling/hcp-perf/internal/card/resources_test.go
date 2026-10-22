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
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resourceFixture(t *testing.T) *Resources {
	t.Helper()
	b, err := os.ReadFile("../../docs/cards/small-v1-resources.json")
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseResources(b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResourcesRecordedEvidence(t *testing.T) {
	r := resourceFixture(t)
	series := r.data["series"].([]any)
	if len(series) != 145 {
		t.Fatalf("inventory lost containers: %d", len(series))
	}
	for _, tc := range []struct {
		exclude     bool
		cpu, memory float64
	}{{true, 3.86, 14442}, {false, 3.87, 14506}} {
		total := resourceRequests(series, tc.exclude)
		if math.Abs(total["cpuCores"].(float64)-tc.cpu) > 1e-9 || total["memoryMiB"] != tc.memory {
			t.Fatalf("incorrect request scope: %v", total)
		}
	}
	goCount, cpuOver, usageOver, goOver := 0, 0, 0, 0
	for _, raw := range series {
		s := raw.(map[string]any)
		points := s["points"].([]any)
		if len(points) != 126 {
			t.Fatalf("%v lost raw points", s["id"])
		}
		peak := map[string]float64{}
		for _, p := range points {
			for _, key := range []string{"cpuCores", "usageMiB", "goManagedMiB"} {
				if v, ok := p.(map[string]any)[key].(float64); ok {
					peak[key] = math.Max(peak[key], v)
					if key == "goManagedMiB" {
						goCount++
					}
				}
			}
		}
		if peak["cpuCores"] > at(s, "requests.cpuCores").(float64) {
			cpuOver++
		}
		if peak["usageMiB"] > at(s, "requests.memoryMiB").(float64) {
			usageOver++
		}
		if target, ok := s["goMemoryLimitMiB"].(float64); ok && peak["goManagedMiB"] > target {
			goOver++
		}
		if strings.Contains(s["id"].(string), "zq2j4/kube-apiserver") {
			ratio := peak["cpuCores"] / at(s, "requests.cpuCores").(float64)
			if math.Abs(ratio-5.115297) > 0.00001 {
				t.Fatalf("KAS CPU/request ratio %f", ratio)
			}
		}
		if s["container"] == "etcd" {
			if peak["usageMiB"] <= at(s, "requests.memoryMiB").(float64) || peak["goManagedMiB"] >= s["goMemoryLimitMiB"].(float64) {
				t.Fatal("etcd cgroup excess is not a Go-target excess")
			}
		}
		if s["container"] == "etcd-metrics" && peak["goManagedMiB"] != 0 {
			t.Fatal("Go curve attributed to forwarding proxy")
		}
	}
	if goCount != 748 || cpuOver != 30 || usageOver != 15 || goOver != 4 {
		t.Fatalf("coverage/excess counts: %d %d %d %d", goCount, cpuOver, usageOver, goOver)
	}
	var out bytes.Buffer
	if err := RenderWithResources(&out, parseFixture(t, fixture(t)), nil, r); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"250m + 1 GiB", "100m + 256 MiB", "3.86 CPU<br>14.104 GiB", "3.87 CPU / 14.166 GiB", "Complete container inventory (145 containers)", "GOMEMLIMIT governs Go-managed memory", "Resource source metadata", `"resources":{"cardID":"small-v1"`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Count(html, `class="resource-raw"`) != 145 {
		t.Fatal("truncated raw inventory")
	}
	previous := -1
	for _, id := range []string{"budget", "findings", "component-usage", "method", "contention", "clientload", "resources", "evidence"} {
		i := strings.Index(html, `id="`+id+`"`)
		if i <= previous {
			t.Errorf("%s missing or out of order", id)
		}
		previous = i
	}
	if err := RenderWithResources(brokenWriter{}, parseFixture(t, fixture(t)), nil, r); err == nil {
		t.Fatal("lost writer error")
	}
}

func minimalResources(t *testing.T) map[string]any {
	t.Helper()
	r := resourceFixture(t)
	s := r.data["series"].([]any)[0].(map[string]any)
	s["points"] = s["points"].([]any)[:2]
	r.data["series"] = []any{s}
	return r.data
}

func TestResourcesValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"version", func(d map[string]any) { d["schemaVersion"] = "hcp-perf.resources/v2" }},
		{"identity", func(d map[string]any) { d["cardID"] = "" }},
		{"window", func(d map[string]any) { d["window"].(map[string]any)["end"] = float64(0) }},
		{"step", func(d map[string]any) { d["window"].(map[string]any)["expectedStepSeconds"] = float64(0) }},
		{"too many slots", func(d map[string]any) { d["window"].(map[string]any)["expectedStepSeconds"] = 0.001 }},
		{"empty series", func(d map[string]any) { d["series"] = []any{} }},
		{"duplicate", func(d map[string]any) { s := d["series"].([]any); d["series"] = append(s, s[0]) }},
		{"series limit", func(d map[string]any) { d["series"] = make([]any, 513) }},
		{"point limit", func(d map[string]any) { d["series"].([]any)[0].(map[string]any)["points"] = make([]any, 2001) }},
		{"requests type", func(d map[string]any) { d["series"].([]any)[0].(map[string]any)["requests"] = "unknown" }},
		{"zero request", func(d map[string]any) {
			at(d["series"].([]any)[0].(map[string]any), "requests").(map[string]any)["cpuCores"] = float64(0)
		}},
		{"negative", func(d map[string]any) {
			d["series"].([]any)[0].(map[string]any)["points"].([]any)[0].(map[string]any)["usageMiB"] = float64(-1)
		}},
		{"string metric", func(d map[string]any) {
			d["series"].([]any)[0].(map[string]any)["points"].([]any)[0].(map[string]any)["usageMiB"] = "42"
		}},
		{"off grid", func(d map[string]any) {
			d["series"].([]any)[0].(map[string]any)["points"].([]any)[0].(map[string]any)["timestamp"] = 1789065166.0
		}},
		{"outside", func(d map[string]any) {
			d["series"].([]any)[0].(map[string]any)["points"].([]any)[0].(map[string]any)["timestamp"] = at(d, "window.end")
		}},
		{"unsorted", func(d map[string]any) {
			p := d["series"].([]any)[0].(map[string]any)["points"].([]any)
			p[0], p[1] = p[1], p[0]
		}},
		{"duplicate timestamp", func(d map[string]any) { p := d["series"].([]any)[0].(map[string]any)["points"].([]any); p[1] = p[0] }},
		{"zero Go exception", func(d map[string]any) { d["series"].([]any)[0].(map[string]any)["goMemoryLimitMiB"] = float64(0) }},
		{"source metadata", func(d map[string]any) { d["sources"] = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := minimalResources(t)
			tc.change(d)
			b, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseResources(b); err == nil {
				t.Fatal("accepted invalid resource data")
			}
		})
	}
	for _, b := range [][]byte{[]byte("null"), []byte("{} {}"), []byte(`{"schemaVersion":"hcp-perf.resources/v1","x":1e999}`), bytes.Repeat([]byte(" "), MaxResourcesBytes+1)} {
		if _, err := ParseResources(b); err == nil {
			t.Fatal("accepted invalid or oversized input")
		}
	}
	d := minimalResources(t)
	s := d["series"].([]any)[0].(map[string]any)
	s["requests"] = map[string]any{"cpuCores": nil, "memoryMiB": nil}
	s["goMemoryLimitMiB"] = nil
	p := s["points"].([]any)[0].(map[string]any)
	p["cpuCores"] = nil
	p["usageMiB"] = float64(0)
	delete(p, "goManagedMiB")
	b, _ := json.Marshal(d)
	r, err := ParseResources(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := resourceRequests(r.data["series"].([]any), true); got["cpuCores"] != nil || got["memoryMiB"] != nil {
		t.Fatalf("unknown budget became zero: %v", got)
	}
}

func TestResourcesCardBindingAndEscaping(t *testing.T) {
	c := parseFixture(t, fixture(t))
	for _, change := range []func(*Resources){
		func(r *Resources) { r.data["cardID"] = "other" },
		func(r *Resources) { r.data["series"] = r.data["series"].([]any)[:144] },
		func(r *Resources) {
			r.data["series"].([]any)[0].(map[string]any)["requests"].(map[string]any)["cpuCores"] = 0.2
		},
		func(r *Resources) {
			r.data["window"].(map[string]any)["start"] = at(c.data, "apfComparison.arms").([]any)[0].(map[string]any)["stopEpoch"]
		},
		func(r *Resources) {
			for _, raw := range r.data["series"].([]any) {
				s := raw.(map[string]any)
				if s["container"] == "kube-apiserver" {
					s["requests"].(map[string]any)["cpuCores"] = 0.3
				}
			}
		},
	} {
		r := resourceFixture(t)
		change(r)
		if err := RenderWithResources(&bytes.Buffer{}, c, nil, r); err == nil {
			t.Fatal("accepted mismatched card/resources")
		}
	}
	if err := RenderWithResources(&bytes.Buffer{}, c, nil, &Resources{}); err == nil {
		t.Fatal("accepted unparsed resources")
	}
	r := resourceFixture(t)
	b, err := os.ReadFile("../../docs/cards/small-v1-contention.json")
	if err != nil {
		t.Fatal(err)
	}
	contention, err := ParseContention(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := RenderWithResources(&bytes.Buffer{}, c, contention, r); err != nil {
		t.Fatal(err)
	}
	r.data["window"].(map[string]any)["end"] = at(r.data, "window.end").(float64) + 5
	if err := RenderWithResources(&bytes.Buffer{}, c, contention, r); err == nil {
		t.Fatal("accepted unrelated contention window")
	}
	r = resourceFixture(t)
	payload := `</script><script>alert("x")</script>`
	r.data["method"].(map[string]any)["untrusted"] = payload
	var out bytes.Buffer
	if err := RenderWithResources(&out, c, nil, r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), payload) || !strings.Contains(out.String(), `\u003c/script\u003e`) {
		t.Fatal("unsafe resources JSON embedding")
	}
}

func TestResourcesOfflineBrowser(t *testing.T) {
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Skip("Chrome unavailable")
	}
	r := resourceFixture(t)
	// Exercise mixed replica budgets, unknown budgets, and an isolated sample.
	r.data["series"].([]any)[0].(map[string]any)["points"].([]any)[0].(map[string]any)["cpuCores"] = nil
	for _, raw := range r.data["series"].([]any) {
		s := raw.(map[string]any)
		if s["workload"] == "ovnkube-control-plane" && s["container"] == "socks-proxy" && strings.HasSuffix(s["pod"].(string), "jbmmr") {
			s["requests"].(map[string]any)["cpuCores"] = 0.02
		}
		if s["workload"] == "router" {
			s["requests"].(map[string]any)["memoryMiB"] = nil
			for _, rawPoint := range s["points"].([]any) {
				rawPoint.(map[string]any)["cpuCores"] = float64(0)
			}
		}
	}
	cardData := fixture(t)
	// The synthetic mixed request adds 10m to the reviewed inventory.
	at(cardData, "topology.requestAccounting.excludingExperimentalKSM").(map[string]any)["cpuMilli"] = float64(3870)
	at(cardData, "topology.requestAccounting.includingExperimentalKSM").(map[string]any)["cpuMilli"] = float64(3880)
	var out bytes.Buffer
	if err := RenderWithResources(&out, parseFixture(t, cardData), nil, r); err != nil {
		t.Fatal(err)
	}
	probe := `<script>(()=>{const checks=[];const check=(name,v)=>checks.push([name,!!v]);try{
const panels=[...document.querySelectorAll('.resource-panel')],data=JSON.parse(document.getElementById('report-data').textContent).resources;
check('three panels',panels.length===3);check('six charts',document.querySelectorAll('svg[data-resource-metric]').length===6);
check('all inventory',document.querySelectorAll('#resource-inventory tbody tr').length===145);
const resourceLabels=[...document.querySelectorAll('.resource-plot svg text')];
check('readable scaled resource labels',resourceLabels.length>0&&resourceLabels.every(t=>{const m=t.getScreenCTM();return m&&parseFloat(getComputedStyle(t).fontSize)*Math.hypot(m.c,m.d)>=11;}));
const narrow=matchMedia('(max-width:760px)').matches;
check('narrow-only scroll hint',(getComputedStyle(document.querySelector('.resource-scroll-hint')).display!=='none')===narrow);
if(narrow){
 const plots=[...document.querySelectorAll('.resource-plot')];
 check('resource plots scroll locally',plots.every(p=>{p.scrollLeft=100;const scrolled=p.scrollWidth>p.clientWidth&&p.scrollLeft>0;p.scrollLeft=0;return scrolled&&p.tabIndex===0;}));
 const cards=[...document.querySelectorAll('.budget-band article')].map(e=>e.getBoundingClientRect());
 check('compact mobile budget grid',Math.abs(cards[0].top-cards[1].top)<1&&cards[2].top>cards[0].top&&cards[2].width>cards[0].width);
}
const kas=panels[0],etcd=panels[1],svg=(p,k)=>p.querySelector('svg[data-resource-metric="'+k+'"]');
check('KAS replicas',kas.querySelectorAll('.resource-replicas input:checked').length===3);
check('CPU reference',svg(kas,'cpuCores').querySelectorAll('.resource-reference[data-kind=request]').length===1&&+svg(kas,'cpuCores').querySelector('.resource-reference[data-kind=request]').dataset.value===.25);
check('CPU excess',svg(kas,'cpuCores').querySelectorAll('.resource-excess').length>0);
check('KAS memory request safe',svg(kas,'usageMiB').querySelectorAll('.resource-excess[data-kind=request]').length===0);
check('Go excess separate',svg(kas,'usageMiB').querySelectorAll('.resource-excess[data-kind=go-target]').length>0);
check('etcd charged over request',svg(etcd,'usageMiB').querySelectorAll('.resource-excess[data-kind=request]').length>0);
check('etcd Go below target',svg(etcd,'usageMiB').querySelectorAll('.resource-excess[data-kind=go-target]').length===0);
check('bounded default memory',+svg(kas,'usageMiB').dataset.axisMax<1200);
check('KAS 5.115x',kas.querySelector('.resource-summary').textContent.includes('5.115x (411.5% vs budget)'));
check('KAS below request signed percent',kas.querySelector('.resource-summary').textContent.includes('0.918x (-8.2% vs budget)'));
const goTraces=[...svg(kas,'usageMiB').querySelectorAll('.resource-trace[data-metric=goManagedMiB]')];
check('Go gaps retained',goTraces.every(g=>(g.querySelector('.resource-line').getAttribute('d').match(/M/g)||[]).length>1));
check('Go nulls not zeros',goTraces.reduce((n,g)=>n+g.querySelectorAll('circle').length,0)===244);
const toggle=(p,text)=>[...p.querySelectorAll('label')].find(l=>l.textContent===text)?.querySelector('input');
toggle(kas,'Hard memory limit (expands axis)').click();check('hard limit expands axis',+svg(kas,'usageMiB').dataset.axisMax>3072);toggle(kas,'Hard memory limit (expands axis)').click();
toggle(kas,'RSS').click();check('RSS checkbox',svg(kas,'usageMiB').querySelectorAll('.resource-trace[data-metric=rssMiB]').length===3);
toggle(etcd,'Working set').click();check('working set no request excess',svg(etcd,'workingSetMiB').querySelectorAll('.resource-excess[data-kind=request]').length===0);
kas.querySelector('.resource-replicas input').click();check('replica toggles',svg(kas,'cpuCores').querySelectorAll('.resource-trace').length===2);kas.querySelector('.resource-replicas button').click();
check('phase boundaries',+svg(kas,'cpuCores').querySelector('.resource-phase').dataset.start===1789065307.316991);
const selects=document.querySelectorAll('#resource-picker select'),choose=(w,c)=>{selects[0].value=w;selects[0].dispatchEvent(new Event('change'));if(c){selects[1].value=c;selects[1].dispatchEvent(new Event('change'));}};
choose('ovnkube-control-plane','socks-proxy');check('explore workload',document.querySelector('#resource-selected h3').textContent.includes('socks-proxy'));
check('mixed per replica requests',document.querySelectorAll('#resource-selected svg[data-resource-metric=cpuCores] .resource-reference[data-kind=request]').length===2);
choose('redhat-operators-catalog','registry');check('known target without Go curve',document.querySelectorAll('#resource-selected [data-kind=GOMEMLIMIT]').length===1&&document.querySelectorAll('#resource-selected .resource-trace[data-metric=goManagedMiB]').length===0);
choose('router');check('unknown request no reference',document.querySelectorAll('#resource-selected svg[data-resource-metric=usageMiB] [data-kind=request]').length===0);
const routerSummaries=[...document.querySelectorAll('#resource-selected .resource-summary p')];
check('zero CPU signed percent',routerSummaries.length>0&&routerSummaries.every(p=>p.textContent.includes('0.000x (-100.0% vs budget)')));
check('unknown budget remains unknown',routerSummaries.every(p=>p.textContent.includes('/ unknown budget = unknown ratio;')));
check('non Go disabled',document.querySelector('#resource-selected label input:disabled')!==null);
choose('__all');check('no aggregate Go',document.querySelectorAll('#resource-selected .resource-trace[data-metric=goManagedMiB]').length===0);
check('aggregate budget',Math.abs(+document.querySelector('#resource-selected svg[data-resource-metric=cpuCores] .resource-reference[data-kind=request]').dataset.value-3.87)<1e-9);
check('aggregate only complete timestamps',document.querySelectorAll('#resource-selected svg[data-resource-metric=cpuCores] circle').length===125);
toggle(document.querySelector('#resource-selected'),'Include experimental instrumentation').click();check('instrumentation explicit',Math.abs(+document.querySelector('#resource-selected svg[data-resource-metric=cpuCores] .resource-reference[data-kind=request]').dataset.value-3.88)<1e-9);
const raw=document.querySelectorAll('.resource-raw');raw[144].open=true;raw[144].dispatchEvent(new Event('toggle'));check('tail raw complete',JSON.parse(raw[144].querySelector('pre').textContent).length===126);
check('finite SVG',[...document.querySelectorAll('svg *')].every(e=>[...e.attributes].every(a=>!/NaN|Infinity/.test(a.value))));
check('no overflow',document.documentElement.scrollWidth<=innerWidth);
}catch(e){checks.push(['exception '+e.stack,false]);}const p=document.createElement('p');p.id='browser-check';p.textContent=checks.every(c=>c[1])?'passed':JSON.stringify(checks.filter(c=>!c[1]));document.body.append(p);})();</script>`
	path := filepath.Join(t.TempDir(), "resources.html")
	if err := os.WriteFile(path, []byte(strings.Replace(out.String(), "</body>", probe+"</body>", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, size := range []string{"1440,1000", "390,844"} {
		t.Run(size, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, chrome, "--headless", "--no-sandbox", "--disable-gpu", "--no-first-run", "--disable-background-networking", "--disable-dev-shm-usage", "--user-data-dir="+t.TempDir(), "--window-size="+size, "--dump-dom", "file://"+path)
			b, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), `id="browser-check">passed</p>`) {
				t.Fatalf("resource browser checks: %s", browserProbe(string(b)))
			}
		})
	}
}

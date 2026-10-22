// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package report

const htmlHead = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'none'; img-src 'none'; font-src 'none'; base-uri 'none'; form-action 'none'">
<title>HCP performance evidence</title>
<style>
:root{color-scheme:light;--ink:#172f3b;--muted:#506571;--line:#c9d5d9;--paper:#f5f4ef;--accent:#006d77;--bad:#a32b24}
*{box-sizing:border-box}body{margin:0;background:var(--paper);color:var(--ink);font:15px/1.55 system-ui,sans-serif}main{max-width:1500px;margin:auto;padding:28px}header{border-top:6px solid var(--accent);padding:24px 0}h1{font-size:clamp(26px,4vw,44px);letter-spacing:-.04em;margin:4px 0}h2{font-size:22px;margin:24px 0 12px}h3{font-size:17px;margin:0 0 10px}p{margin:8px 0}.eyebrow{font-size:12px;letter-spacing:.15em;text-transform:uppercase;color:var(--accent)}.muted,small{color:var(--muted)}.notice{border-left:4px solid #b77922;padding:12px 16px;background:#fff3dc}.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(210px,1fr));gap:12px;margin:20px 0}.card,article{background:white;border:1px solid var(--line);padding:16px;min-width:0}.card strong{display:block;font-size:24px}.table-wrap{overflow:auto;max-height:540px;border:1px solid var(--line);background:white}table{border-collapse:collapse;width:100%;font-size:13px}th,td{padding:9px 12px;text-align:left;vertical-align:top;border-bottom:1px solid var(--line);overflow-wrap:anywhere;min-width:90px}th{position:sticky;top:0;background:#e6edef;z-index:1}td.labels{min-width:260px;max-width:650px;font-family:ui-monospace,monospace;font-size:12px}.bad{color:var(--bad)}.controls{display:flex;flex-wrap:wrap;gap:12px;align-items:end;margin:14px 0}label{display:flex;flex-direction:column;gap:4px}input,select,button{font:inherit;padding:7px;border:1px solid #80969e;background:white;color:var(--ink);max-width:100%}button{cursor:pointer;background:#e4f1ef}button:hover{background:#cce4e0}button:focus-visible,select:focus-visible,input:focus-visible{outline:3px solid #b77922}#panels{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,560px),1fr));gap:16px}svg{width:100%;height:auto;display:block;background:#fbfcfc}svg text{font:12px system-ui,sans-serif;fill:var(--muted)}.legend{margin:8px 0;padding:0;list-style:none;font-size:12px;overflow-wrap:anywhere}.swatch{display:inline-block;width:16px;height:3px;vertical-align:middle;margin-right:6px}.series-chooser{width:100%;height:130px;font:12px ui-monospace,monospace}.panel-heading{display:flex;justify-content:space-between;gap:12px}.expr{font:12px ui-monospace,monospace;overflow-wrap:anywhere}details{margin:18px 0}summary{cursor:pointer;font-weight:600}footer{border-top:2px solid var(--line);margin-top:32px;padding-top:18px;font-size:13px}#timeline{max-height:650px;overflow:auto;background:white;border:1px solid var(--line)}@media(max-width:600px){main{padding:14px}.controls>*{width:100%}th,td{padding:7px}header{padding:14px 0}}
.takeaways{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,260px),1fr));gap:16px}.takeaways ul{padding-left:20px}.check{display:flex;flex-direction:row;align-items:center;gap:8px}.display-labels{display:flex;flex-wrap:wrap;gap:8px 16px;border:1px solid var(--line);margin:12px 0;min-width:0}.chart-scroll,#timeline{overflow:auto}.chart-scroll svg,#timeline svg{min-width:760px}#timeline{max-height:640px}.expr,pre{overflow-wrap:anywhere;white-space:pre-wrap}summary{cursor:pointer}#purpose{max-width:90ch}.legend li{margin:6px 0}.controls>label{min-width:0;max-width:100%}.series-chooser{width:100%}
</style></head><body><main>
<header><div class="eyebrow">HCP / Local evidence snapshot</div><h1 id="experiment-title">Experiment evidence</h1><p id="purpose" class="muted"></p></header>
<div id="assessment-status" class="cards" aria-label="Independent assessment axes"></div>
<section id="failure-panel" hidden aria-labelledby="failure-heading"><h2 id="failure-heading">Failure and recovery</h2><p id="failure-summary" class="notice"></p><div id="failure-table"></div><p>Client wait is not server execution time. A client context deadline does not establish a server error or continuous unavailability.</p><div id="request-evidence"></div></section>
<section aria-label="Experiment takeaways" id="takeaway-list" class="takeaways"><article><h3>What worked</h3><p id="worked-lead"></p></article><article><h3>Evidence limits</h3><p id="limits-lead"></p></article><article id="carry-forward-card"><h3>Carry forward</h3><p id="carry-forward-lead"></p></article></section>
<details id="all-findings"><summary id="findings-summary">All findings</summary><h3>Workload and capture observations</h3><ul id="worked-list"></ul><h3>Evidence limits</h3><ul id="limits-list"></ul></details>
<details id="all-recommendations"><summary id="recommendations-summary">All recommendations</summary><ul id="carry-forward-list"></ul></details>
<p class="muted"><strong>Scope:</strong> recorded outcomes and budget comparisons alone do not establish capacity or continuous availability.</p>
<details><summary>Scoped data points and provenance</summary><p class="muted">Values retain the assessment's units, statistic, population and measurement method. These observations are not automatically transferable sizing recommendations. Evidence references show the assessment's recorded paths and hashes; they are not authentication or live links.</p><p id="assessment-metadata" class="muted"></p><div id="data-point-table"></div><div id="data-point-details"></div></details>
<noscript>This offline report requires JavaScript for its embedded tables and SVG charts. No network access is used.</noscript>
<details><summary>Run progress and snapshot</summary><div id="overview" class="cards"></div><p id="latest" class="muted"></p><p id="steady" class="muted"></p></details>
<h2>Phase and canary timeline</h2><p>A canary scales a Deployment from 0 to 1 replica, waits for a Ready Pod, then scales back to 0 and waits for removal. Each cycle has three lanes; Ready includes ACK, not an additional duration. Solid bars are completed stages; dashed bars are in progress; red hatched bars show client wait ending without stage completion.</p>
<details id="measurement-details"><summary>Canary identity, measurement and timeline details</summary><p>The client uses a bounded, protected APF identity, not an exempt identity; Deployment controllers, the scheduler and kubelets use their ordinary paths. A passed cycle means these client-observed stages completed, not that every path was continuously healthy. Soft expectations describe latency; only explicit hard stops cancel observation. Amber bars are completed late stages; red hatched waits are censored, not completed latency. Historical baseline budgets remain hard stops, never retroactive soft expectations.</p><p id="measurement-note" class="muted"></p><p class="muted">Removed is measured after scale-down. Incomplete stages show elapsed time to snapshot only while in progress. Short stages have a minimum-width marker; hover for exact timestamps. Only recognized phase starts appear in the top band; all commands remain in details.</p></details>
<label class="check"><input id="show-baseline" type="checkbox">Show baseline cycles</label><p id="cycle-status" class="muted"></p>
<div id="timeline"></div>
<h3>Cycle durations and expectations</h3><p class="muted">One row per visible cycle. Expected and hard-stop durations are seconds and are distinct. Stage-recorded thresholds take precedence over summary files. Completed elapsed uses the recorded duration, including overruns; bars are not clipped to expectations. Ended waits show explicit units, not completed latency; missing completions remain unknown.</p><div id="canary-table"></div>
<details><summary>Canary distributions by phase (including baseline)</summary><div id="phase-table"></div></details>
<h2>Explore metric previews</h2><p id="preview-status" class="notice"></p><p class="muted">These are bounded previews, not all raw observations. Each panel compares selected label sets from one query and one source. Select multiple series with Ctrl/Cmd or Shift. Initially four preview series are shown. Add panels to compare CPU, memory, GC, or scrape-up with pods on the timeline.</p>
<div class="controls"><label>Query name filter (regular expression)<input id="query-filter" placeholder="cpu|memory|gc|up" type="text"></label><label>Query / source<select id="query-choice"></select></label><button id="add-panel" type="button">Add chart panel</button></div><p id="filter-status" role="status"></p>
<div id="panels"></div>
<details><summary>All query counts and coverage</summary><p class="muted">Counts and aggregate coverage include every analyzed series, even when its labels and samples are omitted from this preview. Unreadable sources have unknown totals, not proven zero observations. Aggregate coverage is valid / expected grid positions across analyzed series, not continuous availability.</p><div id="query-table"></div>
<h3>Preview series metadata</h3><p class="muted">Only selected series are embedded, with at most 1000 metadata rows overall. Omitted series remain in the full local raw files referenced below. Values and extrema use raw query units: no name-based CPU/memory/GC unit conversions. Zero samples indicate scrape-down only when the expression actually measures scrape-up.</p><div id="metric-table"></div>
</details><h2>Source integrity and warnings</h2><div id="warning-table"></div><details><summary>Source metadata and integrity</summary><div id="source-table"></div></details>
<details><summary>Commands, conditions, and latest turnover</summary><div id="command-table"></div><div id="condition-table"></div><pre id="turnover"></pre></details>
<details><summary>Preview methodology, limits and privacy</summary><p><strong>Visualization only:</strong> default limits are 200 series overall and 300 points per series (configurable; hard ceilings 1000 series and 500 points). Scrape-up query names are prioritized, round-robin across sources, to retain all targets when the overall limit permits. Remaining slots are allocated round-robin across queries, promoting CPU/memory/GC names then KAS/etcd/CPO names. Non-up queries retain at most 20 series each. Within queries, KAS/etcd/CPO labels and non-bucket series are promoted; resource-query series then rank by maximum absolute value, with deterministic label tie-breaking. Histogram buckets use label order, not magnitude ranking. This heuristic is not statistical representativeness or evidence of importance; no capacity conclusions follow from selection.</p>
<p>For reduced series, floor(max-points / 2) equal-count buckets retain minimum and maximum in timestamp order. Short segments may disappear; original gap counts and full-sample metadata remain for selected series. Lines never bridge detected gaps. Missing leading/trailing samples affect coverage, not fabricated points. Query counts and coverage use all analyzed samples, not just preview series.</p>
<p>Raw source files are untouched and are the full-resolution evidence. Embedded manifest references retain filenames, SHA-256 hashes, windows and steps, but warnings are limited to 20 per query and diagnostic text to 2048 bytes; omitted warning counts are explicit. Full warnings remain in the local manifest/response and can be regenerated with metrics analyze. Hash checks detect modification, not authenticity. Partial or corrupt sources remain visible; invalid queries have no chart. Reports do not refresh, query live services, load external assets, or embed archives/raw logs. Metric labels and expressions may be sensitive: keep this HTML private.</p></details>
</main><script id="report-data" type="application/json">`

const htmlTail = `</script><script>
'use strict';
const data=JSON.parse(document.getElementById('report-data').textContent), p=data.progress;
const $=id=>document.getElementById(id), list=v=>v||[], fmt=v=>v==null?'unknown':typeof v==='number'?Number(v.toPrecision(6)).toString():String(v);
const at=v=>v&&!String(v).startsWith('0001-')&&Number.isFinite(Date.parse(v))?Date.parse(v)/1000:null;
const utc=t=>t==null?'unknown':new Date(t*1000).toISOString();
function el(tag,parent,text,cls){const n=document.createElement(tag);if(text!=null)n.textContent=String(text);if(cls)n.className=cls;if(parent)parent.appendChild(n);return n;}
function table(parent,headers,rows){const wrap=el('div',parent,null,'table-wrap'),t=el('table',wrap),head=el('tr',el('thead',t));headers.forEach(h=>el('th',head,h));const body=el('tbody',t);for(const row of rows){const tr=el('tr',body);row.forEach((v,i)=>el('td',tr,fmt(v),headers[i].includes('Labels')?'labels':''));}if(!rows.length){const td=el('td',el('tr',body),'No evidence available');td.colSpan=headers.length;}return t;}
function card(title,value,detail){const n=el('div',$('overview'),null,'card');el('small',n,title);el('strong',n,value);el('span',n,detail);}
const assessment=data.assessment,experiment=assessment?.experiment;
$('assessment-metadata').textContent=assessment?'Assessment generated '+utc(at(assessment.generatedAt))+' | schema '+fmt(assessment.schemaVersion)+' | analysis '+fmt(assessment.analysisVersion)+' | experiment ID '+fmt(experiment?.id):'No assessment embedded.';
const recordedText=v=>typeof v==='string'&&v.trim()&&v!=='unknown'&&v!=='unspecified';
const dimensions=[];
for(const [field,label] of [['guestNodes','nodes'],['pods','application Pods']])if(Number.isSafeInteger(experiment?.[field])&&experiment[field]>=0)dimensions.push(experiment[field]+' '+label);
if(!Number.isSafeInteger(experiment?.pods)&&Number.isSafeInteger(experiment?.applications)&&experiment.applications>=0)dimensions.push(experiment.applications+' applications');
const mode=experiment?.mode==='validation'?'Harness validation':experiment?.mode==='soak'?'Workload soak':'Experiment evidence';
const treatment=experiment?.cpuTreatment==='quiet'?'Quiet':experiment?.cpuTreatment==='contended'?'CPU-contended':'';
$('experiment-title').textContent=recordedText(experiment?.title)?experiment.title:(treatment?treatment+' '+mode.toLowerCase():mode)+(dimensions.length?': '+dimensions.join(', '):'');
$('purpose').textContent=recordedText(experiment?.question)?experiment.question:experiment?.mode==='validation'?'Did the Deployment lifecycle checks complete, and is the collected evidence usable for evaluating the harness?':'Did the workload complete, and is its evidence usable?';
if(!recordedText(experiment?.question))$('assessment-metadata').textContent+=' Recorded objective not supplied; the report question interprets the recorded configuration, not an experiment-authored purpose statement.';
const workload=['pass','fail','incomplete'].includes(assessment?.status?.workload)?assessment.status.workload:'unknown',capture=['complete','incomplete','unknown'].includes(assessment?.status?.capture)?assessment.status.capture:'unknown';
const performance=['within_expectations','degraded'].includes(assessment?.status?.performance)?assessment.status.performance:'unknown';
for(const [name,status] of [['Workload',workload==='pass'?'Passed':workload==='fail'?'Failed':workload==='incomplete'?'Incomplete':'Not analyzed'],['Performance',performance==='degraded'?'Degraded':performance==='within_expectations'?'Within expectations':'Unknown'],['Capture',capture==='complete'?'Complete':capture==='incomplete'?'Incomplete':'Unknown']]){const n=el('div',$('assessment-status'),null,'card');el('small',n,name);el('strong',n,status);}
if(workload==='fail')el('p',$('assessment-status').children[0],'Recorded workload scope; not a cluster-wide availability verdict.','muted');
const evidenceByID=new Map((Array.isArray(assessment?.evidence)?assessment.evidence:[]).filter(e=>e&&typeof e.id==='string').map(e=>[e.id,e]));
const pointsByID=new Map((Array.isArray(assessment?.dataPoints)?assessment.dataPoints:[]).filter(d=>d&&typeof d.id==='string').map(d=>[d.id,d]));
const findings=(Array.isArray(assessment?.findings)?assessment.findings:[]).filter(f=>f&&recordedText(f.statement)),recommendations=(Array.isArray(assessment?.carryForward)?assessment.carryForward:[]).filter(c=>c&&recordedText(c.recommendation));
$('findings-summary').textContent='All findings ('+findings.length+')';$('recommendations-summary').textContent='All recommendations ('+recommendations.length+')';
$('worked-lead').textContent=workload==='pass'?'The recorded workload passed. See the scoped cycle and rollout observations.':workload==='fail'?'The recorded workload failed. Review the failure evidence before repeating the run.':workload==='incomplete'?'Workload completion is not established by the available evidence.':'Not analyzed: no workload assessment is available.';
const completedCounts=[],steadyCount=p.steadyCanaries,turnover=p.turnover;
if(workload==='pass'){
  if(Number.isSafeInteger(steadyCount?.passed)&&Number.isSafeInteger(steadyCount?.expected)&&steadyCount.expected>0&&steadyCount.passed===steadyCount.expected)completedCounts.push(steadyCount.passed+'/'+steadyCount.expected+' steady canaries');
  if(Number.isSafeInteger(turnover?.completed)&&Number.isSafeInteger(turnover?.scheduled)&&turnover.scheduled>0&&turnover.completed===turnover.scheduled)completedCounts.push(turnover.completed+'/'+turnover.scheduled+' rollouts');
  if(completedCounts.length)$('worked-lead').textContent=completedCounts.join(' and ')+' completed. The recorded workload passed.';
}
if(workload==='pass'&&performance==='degraded')$('worked-lead').textContent='Completed, with latency degradation. '+(completedCounts.length?completedCounts.join(' and ')+' completed. ':'')+'Soft expectations were exceeded; completion still passed.';
if(Number.isSafeInteger(steadyCount?.skipped)&&steadyCount.skipped>0)$('worked-lead').textContent+=' '+fmt(steadyCount.passed)+' completed steady canaries; '+steadyCount.skipped+' skipped observation slots, not workload failures.';
$('limits-lead').textContent=capture==='incomplete'?'Capture is incomplete. Review recording gaps before carrying sizing conclusions forward.':capture==='complete'?'Capture is complete within the assessed scope, not proof of capacity or continuous availability.':'Capture completeness is unknown. Do not treat missing evidence as success.';
const logCounts=[];
if(capture==='incomplete')for(const [name,label] of [['recorder.logs.errors','log errors'],['recorder.logs.gaps','log gaps']]){const values=[...pointsByID.values()].filter(d=>d.name===name&&d.statistic==='count'&&d.unit==='count');if(values.length===1&&Number.isSafeInteger(values[0].value)&&values[0].value>=0)logCounts.push(values[0].value+' '+label);}
if(logCounts.length)$('limits-lead').textContent='Recording incomplete: '+logCounts.join(', ')+'. Review evidence limits before reuse.';
const next=recommendations.find(c=>c.kind==='fix_harness')||recommendations[0];
$('carry-forward-lead').textContent=!next?'Not analyzed: no carry-forward recommendation is available.':next.recommendation.length<=180?next.recommendation:next.kind==='fix_harness'?'Address the recorded harness issues before comparison. Review the recommended fixes, conditions and blockers.':next.kind==='retain'?'Retain the scoped observations; check their conditions and limitations before reuse.':'Review the proposed next step and its conditions before applying it.';
function evidenceDetails(parent,refs){
  const references=[...new Set((Array.isArray(refs)?refs:[]).filter(ref=>typeof ref==='string'))];
  if(!references.length)return;
  const group=el('details',parent);el('summary',group,'Evidence ('+references.length+' '+(references.length===1?'source':'sources')+')');
  for(const ref of references){const detail=el('div',group),e=evidenceByID.get(ref);el('strong',detail,'['+ref+']');
    if(!e){el('p',detail,'Unresolved evidence reference');continue;}
    el('p',detail,e.path||'Path unspecified','expr');el('p',detail,'SHA-256: '+(e.sha256||'not recorded'),'expr');
    if(e.schema)el('p',detail,'Schema: '+e.schema);if(e.query)el('p',detail,'Query: '+e.query,'expr');if(e.jsonPointer)el('p',detail,'JSON pointer: '+e.jsonPointer);if(e.units)el('p',detail,'Units: '+e.units);
    if(e.window)el('p',detail,'Window: '+utc(at(e.window.start))+' to '+utc(at(e.window.end)));
  }
}
for(const f of Array.isArray(assessment?.findings)?assessment.findings:[]){
  if(!f||typeof f.statement!=='string'||!f.statement.trim())continue;
  const status=['observed','inconclusive'].includes(f.status)?f.status:'inconclusive',category=['workload','performance','capture','measurement','resources'].includes(f.category)?f.category:'unknown';
  const worked=status==='observed'&&((category==='workload'&&workload==='pass')||(category==='capture'&&capture==='complete'));
  const item=el('li',$(worked?'worked-list':'limits-list'));el('span',item,f.statement);el('small',item,' ['+category+' / '+status+']');
  if(f.scope)el('p',item,'Scope: '+f.scope,'muted');evidenceDetails(item,f.evidence);
}
for(const c of Array.isArray(assessment?.carryForward)?assessment.carryForward:[]){
  if(!c||typeof c.recommendation!=='string'||!c.recommendation.trim())continue;
  const item=el('li',$('carry-forward-list'));el('span',item,c.recommendation);el('small',item,' ['+(['hypothesis','retain','fix_harness'].includes(c.kind)?c.kind:'unspecified kind')+']');
  if(c.action)el('p',item,'Action: '+c.action);if(c.scope)el('p',item,'Scope: '+c.scope,'muted');
  for(const [label,values] of [['Blocked by',c.blockedBy],['Condition',c.conditions]])for(const v of Array.isArray(values)?values:[])if(typeof v==='string')el('p',item,label+': '+v,'muted');
  evidenceDetails(item,c.evidenceRefs);
  for(const ref of Array.isArray(c.dataPointRefs)?c.dataPointRefs:[]){const d=pointsByID.get(ref);el('p',item,d?'Data point ['+ref+']: '+d.name+' = '+fmt(d.value)+' '+d.unit+' ('+d.statistic+'; '+d.measurementMethod+'; '+d.population+')':'Unresolved data point ['+fmt(ref)+']');}
}
for(const [id,description] of [['worked-list','positive workload or capture findings'],['limits-list','evidence limits'],['carry-forward-list','carry-forward recommendations']])if(!$(id).children.length)el('li',$(id),(assessment?'No ':'Not analyzed: no ')+description+' recorded.');
table($('data-point-table'),['Name','Value','Unit','Statistic','Measurement method','Population / scope','Applicability'],[...pointsByID.values()].map(d=>[d.name,d.value,d.unit,d.statistic,d.measurementMethod,d.population,d.applicability]));
for(const [id,d] of pointsByID){const detail=el('details',$('data-point-details'));el('summary',detail,'Data point ['+id+']: '+d.name+' / '+d.statistic);el('p',detail,'Window: '+utc(at(d.window?.start))+' to '+utc(at(d.window?.end)));
  for(const limit of Array.isArray(d.limitations)?d.limitations:[])if(typeof limit==='string')el('p',detail,'Limit: '+limit);
  if(d.provenance?.formula)el('p',detail,'Formula: '+d.provenance.formula,'expr');for(const ptr of Array.isArray(d.provenance?.jsonPointers)?d.provenance.jsonPointers:[])if(typeof ptr==='string')el('p',detail,'JSON pointer: '+ptr);
  evidenceDetails(detail,d.provenance?.evidenceRefs);
}
card('Latest phase',p.phase||'incomplete','Activity: '+(p.activityPhase||'unknown'));
card('Baseline canaries',fmt(p.baseline.passed)+' passed',fmt(p.baseline.failed)+' failed / '+fmt(p.baseline.pending)+' pending');
card('Steady canaries',fmt(p.steadyCanaries.passed)+' passed',fmt(p.steadyCanaries.failed)+' failed / '+fmt(p.steadyCanaries.pending)+' pending');
card('Metrics sources',list(data.sources).length, list(data.sources).reduce((n,s)=>n+list(s.queries).length,0)+' queries');
$('latest').textContent='Snapshot: '+utc(at(p.observedAt))+' | Last activity: '+utc(at(p.lastActivityAt))+' | Source: '+(p.lastActivitySource||'none')+' | Age: '+fmt(p.staleAgeSeconds)+' s | Applications: '+fmt(p.applications);
$('steady').textContent='Steady window: '+utc(at(p.steady.startedAt))+' to deadline '+utc(at(p.steady.deadline))+' | Elapsed '+fmt(p.steady.elapsedSeconds)+' s / Remaining '+fmt(p.steady.remainingSeconds)+' s / Overdue '+fmt(p.steady.overdueSeconds)+' s';
const phases=[['baseline',p.baseline],['steady',p.steadyCanaries]], stageNames=['ack','ready','cleanup'];
const stageLabels=['API acknowledgement','Pod ready (includes ACK)','Removed after scale-down'];
const measurementModes=[...new Set([experiment?.measurementMode,p.measurementMode,...phases.flatMap(([,g])=>list(g.samples).map(c=>c.measurementMode))].filter(recordedText))];
$('measurement-note').textContent='Ready and removal are client observations, not exact server transition times. '+(measurementModes.length?'Recorded measurement mode(s): '+measurementModes.join(', ')+'. ':'Measurement mode and poll interval are not recorded in this artifact; the method is unspecified, not assumed to be polling or watch. ')+'Historical context, not metadata for this run: earlier reports used a 5-second polling default. Do not interpret a 5-second observation as precise transition latency. A measurement mode alone does not establish a poll interval.';
const palette=['#006d77','#a04c00','#6943a3','#ba294d','#227341','#3567b1','#805b25','#626d27'];
const phaseRows=[],canaryRows=[],timelineRows=[],times=[],failureRows=[];
const failures=(Array.isArray(assessment?.failures)?assessment.failures:[]).filter(f=>f&&typeof f==='object');
const waitText=seconds=>seconds==null?'unknown':seconds<1?(seconds*1000).toFixed(1)+'ms':fmt(seconds)+' s';
const completedSeconds=s=>at(s?.completed)!=null&&s.durationNanoseconds>0?s.durationNanoseconds/1e9:null;
const timedOut=f=>['client_timeout','deadline_exceeded'].includes(f?.status);
const recoveryDetail=f=>f?.recovery?.status==='observed'?(f.recovery.zeroReplicasObserved===true?'; zero replicas observed':'')+(f.recovery.noOwnedPodsObserved===true?'; no owned pods observed':''):'';
const recoveryScope='Canary cleanup completed; this does not prove recovery of every component';
const reportedFailures=new Set();let recoveredCount=0;
function keepTime(t){if(t!=null)times.push(t);return t;}
keepTime(at(p.windowStart));keepTime(at(p.windowEnd));keepTime(at(p.steady.startedAt));keepTime(at(p.steady.deadline));
for(const [name,group] of phases){
  for(const stage of stageNames){const d=group[stage+'Seconds'];phaseRows.push([name,stage.toUpperCase(),group.expected,group.passed,group.degraded??0,group.failed,group.pending,group.skipped??0,group.remaining,d.count,d.count?d.median:null,d.count?d.p95:null,d.count?d.max:null,name==='steady'&&p.expectedDurationsNanoseconds?p.expectedDurationsNanoseconds[stage]/1e9:null,p.hardDeadlinesNanoseconds?p.hardDeadlinesNanoseconds[stage]/1e9:name==='steady'&&p.deadlinesNanoseconds?p.deadlinesNanoseconds[stage]/1e9:null]);}
  for(const c of list(group.samples)){
    const bars=[],durations=[];
    const failure=failures.find(f=>f.command===c.name),failedStage=c.failureStage||failure?.stage;
    for(const [i,stage] of stageNames.entries()){
      const s=c[stage]||{},start=keepTime(at(s.started)),end=keepTime(at(s.completed));
      const seconds=completedSeconds(s),blocked=stage==='ready'&&failedStage==='ack'&&at(c.ack?.completed)==null;
      const failed=stage===failedStage&&end==null,elapsed=s.durationNanoseconds>0?s.durationNanoseconds/1e9:failed&&failure?.elapsedSeconds>0?failure.elapsedSeconds:null;
      const expected=s.expectedDurationNanoseconds>0?s.expectedDurationNanoseconds/1e9:null;
      const budget=s.deadlineNanoseconds>0?s.deadlineNanoseconds/1e9:failed&&failure?failure.deadlineSeconds??null:p.hardDeadlinesNanoseconds?p.hardDeadlinesNanoseconds[stage]/1e9:expected==null&&name==='steady'&&p.deadlinesNanoseconds?p.deadlinesNanoseconds[stage]/1e9:null;
      const late=c.state==='completed'&&seconds!=null&&expected!=null&&seconds>expected;
      const label=stage.toUpperCase(),ended=failed?(timedOut(failure)?'Timed out after '+waitText(elapsed)+' (deadline '+waitText(budget)+'); '+label+' completion not observed':'Ended without '+label+' after '+waitText(elapsed)+' (deadline '+waitText(budget)+'); completed latency unknown'):null;
      durations.push(expected,budget,blocked?'Not measured: ACK stage failed':ended??seconds,failed?'Censored: completion unknown':late?'Completed late (degraded)':seconds!=null?'Completed':c.state==='inProgress'?'In progress':'Unknown');
      const stop=start!=null&&seconds!=null?start+seconds:end??(failed&&start!=null&&elapsed!=null?start+elapsed:start!=null&&c.state==='inProgress'?at(p.observedAt):null);
      keepTime(stop);if(!blocked&&start!=null&&stop!=null&&stop>=start)bars.push({stage:i,start,end:stop,color:failed?'#a32b24':late?'#b77922':palette[i],failed,late,pending:end==null&&!failed,label:failed?label+' not observed; client ended after '+waitText(elapsed)+'; deadline '+waitText(budget):stageLabels[i]+' '+fmt(seconds)+' s; expected '+fmt(expected)+' s; hard stop '+fmt(budget)+' s'+(late?' (completed with latency degradation)':'')+'; source: recorded durationNanoseconds; completion timestamp '+utc(end)+(stage==='cleanup'?recoveryDetail(failure):'')});
      if(failed){
        const cleanup=completedSeconds(c.cleanup);if(cleanup!=null)recoveredCount++;
        if(failure)reportedFailures.add(failure);
        failureRows.push([name+' / '+c.name+' / '+label+(recordedText(failure?.operation)?': '+failure.operation:''),waitText(budget),waitText(elapsed),ended,cleanup!=null?'Canary cleanup completed in '+waitText(cleanup)+recoveryDetail(failure):'Canary cleanup completion not observed',timedOut(failure)?'Client context deadline exceeded; server cause unknown':'Failure stage recorded; cause unknown']);
        if(timedOut(failure)&&s.deadlineNanoseconds==null&&s.expectedDurationNanoseconds==null&&budget!=null)el('p',$('failure-panel'),'Previous harness canceled at '+(budget<1?fmt(budget*1000)+'ms':fmt(budget)+'s')+'; no completion observed. Historical hard-client-timeout, not a soft expectation violation.');
      }
    }
    canaryRows.push([name,c.name,c.state,...durations,failedStage||'']);
    const laneLabels=stageLabels.map((label,i)=>{const bar=bars.find(b=>b.stage===i);return bar?.failed?bar.label:i===1&&failedStage==='ack'&&at(c.ack?.completed)==null?'Not measured: ACK stage failed':i===2&&completedSeconds(c.cleanup)!=null?label+' '+waitText(completedSeconds(c.cleanup))+recoveryDetail(failure):label;});
    timelineRows.push({phase:name,label:name+' / '+c.name+' / '+c.state,bars,laneLabels});
  }
}
for(const f of failures){
  if(!reportedFailures.has(f)){
    const recovered=f.recovery?.status==='observed';if(recovered)recoveredCount++;
    failureRows.push([fmt(f.command)+' / '+fmt(f.stage)+(recordedText(f.operation)?': '+f.operation:''),waitText(f.deadlineSeconds),waitText(f.elapsedSeconds>0?f.elapsedSeconds:null),f.completionObserved===true?'Completion observed':fmt(f.stage).toUpperCase()+' completion not observed; completed latency unknown',recovered?'Canary cleanup completed in '+waitText(f.recovery.elapsedSeconds>0?f.recovery.elapsedSeconds:null)+recoveryDetail(f):'Canary cleanup completion not observed',timedOut(f)?'Client context deadline exceeded; server cause unknown':'Failure recorded; cause unknown']);
  }
  evidenceDetails($('failure-panel'),f.evidenceRefs);
  const observation=f.serverObservation;
  if(observation&&typeof observation==='object'){
    const detail=el('article',$('request-evidence'));el('h3',detail,'Server-side observation: '+fmt(f.command));
    table(detail,['HTTP response code','Server-observed duration','Client completion'],[[observation.code??null,waitText(observation.durationSeconds),f.completionObserved===true?'Observed':'Not observed']]);
    el('p',detail,'Association limit: server-side evidence does not by itself prove that this client received the response or establish the outcome of an ambiguously completed write.');
    evidenceDetails(detail,observation.evidenceRefs);
  }
}
if(failureRows.length){
  $('failure-panel').hidden=false;$('failure-summary').textContent='Internal progress criterion missed. The failed client-observed stage is separate from server completion and subsequent canary cleanup.';
  table($('failure-table'),['Operation','Hard stop','Observed client wait','Completion status','Recovery','Cause'],failureRows);
  if(!$('request-evidence').children.length)el('p',$('request-evidence'),'Server-side completion and duration: unknown; no linked server observation supplied.');
  if(recoveredCount)el('p',$('failure-panel'),recoveryScope+'.');
  const successes=[];if(Number.isSafeInteger(steadyCount?.passed)&&steadyCount.passed>0)successes.push(steadyCount.passed+' steady canaries passed');
  if(recoveredCount)successes.push('failed-cycle canary cleanup completed');
  if(successes.length)$('worked-lead').textContent=successes.join('; ')+'. See Failure and recovery for scope.';
}
const phaseStarts=[],phaseCommands={seed:'seeding','record-start':'Capture submission','adversaries-start':'pressure start',turnover:'turnover','recovery-window':'recovery','failure-recovery':'failure recovery'};
for(const c of list(p.commands))if(Object.hasOwn(phaseCommands,c.name)){const t=keepTime(at(c.startedAt));if(t!=null&&!phaseStarts.some(m=>m.label===phaseCommands[c.name]&&m.t===t))phaseStarts.push({label:phaseCommands[c.name],t});}
timelineRows.sort((a,b)=>(a.bars[0]?.start??Infinity)-(b.bars[0]?.start??Infinity));
table($('phase-table'),['Phase','Stage','Expected cycles','Passed cycles (all stages)','Degraded completed cycles','Failed cycles (any stage)','Pending cycles','Skipped slots','Not attempted cycles','Completed stage samples','Median s','P95 s','Max s','Expected s (steady baseline)','Hard stop s'],phaseRows);
const queries=[];
for(const [si,s] of list(data.sources).entries()){keepTime(at(s.manifest.start));keepTime(at(s.manifest.end));for(const q of list(s.queries))queries.push({q,s,si});}
const compare=(a,b)=>a<b?-1:a>b?1:0;
queries.sort((a,b)=>compare(a.q.name,b.q.name)||a.si-b.si);
$('preview-status').textContent=fmt(data.previewSeries)+' of '+fmt(data.totalSeries)+' analyzed series included; '+fmt(data.omittedSeries)+' omitted from this HTML. Limits: '+fmt(data.limits.maxSeries)+' series overall / '+fmt(data.limits.maxPoints)+' points per series. Full-resolution observations remain in local raw files.';
for(const {q} of queries)for(const ss of list(q.series))ss.labelText=Object.keys(ss.labels||{}).sort().map(k=>k+'='+JSON.stringify(ss.labels[k])).join(', ');
const range=times.length?[Math.min(...times),Math.max(...times)]:[0,1];if(range[0]===range[1])range[1]++;
const W=900,L=85,R=20;
const x=t=>L+(t-range[0])/(range[1]-range[0])*(W-L-R);
function svgNode(tag,parent,attrs,text){const n=document.createElementNS('http://www.w3.org/2000/svg',tag);for(const [k,v] of Object.entries(attrs||{}))n.setAttribute(k,String(v));if(text!=null)n.textContent=text;parent.appendChild(n);return n;}
function svg(parent,height,title){const n=svgNode('svg',parent,{viewBox:'0 0 '+W+' '+height,role:'img','aria-label':title});svgNode('title',n,{},title);return n;}
function axis(s,y){svgNode('line',s,{x1:L,x2:W-R,y1:y,y2:y,stroke:'#80969e'});for(let i=0;i<=4;i++){const t=range[0]+i*(range[1]-range[0])/4;svgNode('text',s,{x:x(t),y:y+20,'text-anchor':i===0?'start':i===4?'end':'middle'},utc(t).slice(11,23));}svgNode('text',s,{x:L,y:y+38},utc(range[0])+' to '+utc(range[1])+' (UTC)');}
function phaseMarkers(s,top,bottom){for(const [label,t] of [['window start',at(p.windowStart)],['steady start',at(p.steady.startedAt)],['steady deadline',at(p.steady.deadline)],['window end',at(p.windowEnd)]])if(t!=null){const line=svgNode('line',s,{x1:x(t),x2:x(t),y1:top,y2:bottom,stroke:'#a9babf','stroke-dasharray':'4 4'});svgNode('title',line,{},label+' '+utc(t));}}
function drawTimeline(){
  $('timeline').replaceChildren();$('canary-table').replaceChildren();
  const showBaseline=$('show-baseline').checked,rows=timelineRows.filter(r=>showBaseline||r.phase!=='baseline');
  $('cycle-status').textContent=rows.length+' cycles shown; '+(showBaseline?0:p.baseline.samples?.length||0)+' baseline cycles hidden. All panels use the same UTC range, including hidden cycles.';
  table($('canary-table'),['Phase','Canary','State','ACK Expected s','ACK Hard stop s','ACK Completed elapsed s','ACK Status','Ready Expected s','Ready Hard stop s','Ready Completed elapsed s (includes ACK)','Ready Status','Removed Expected s','Removed Hard stop s','Removed Completed elapsed s','Removed Status','Failure stage'],canaryRows.filter(r=>showBaseline||r[0]!=='baseline'));
  if(!rows.length&&!phaseStarts.length){el('p',$('timeline'),'No visible canary cycles or phase starts. Enable baseline cycles to inspect baseline evidence, if recorded.');return;}
  const band=phaseStarts.length?55:10,height=band+rows.length*142+65,s=svg($('timeline'),height,'Canary cycles in three separate lanes on a common UTC timeline');phaseMarkers(s,band,height-55);
  for(const m of phaseStarts){const marker=svgNode('line',s,{x1:x(m.t),x2:x(m.t),y1:8,y2:42,stroke:'#506571','data-phase':m.label});svgNode('title',marker,{},m.label+' | '+utc(m.t)+(m.label==='Capture submission'?' | record-start command submitted; not recorder readiness or process start':' | command start (end not inferred)'));}
  if(phaseStarts.length)svgNode('text',s,{x:L,y:52},'Phase starts (hover markers for names and UTC timestamps)');
  rows.forEach((r,i)=>{const y=band+18+i*142,g=svgNode('g',s,{'data-cycle':r.label});svgNode('text',g,{x:L,y},r.label);
    r.laneLabels.forEach((label,j)=>{svgNode('text',g,{x:L,y:y+17+j*38},label);svgNode('line',g,{x1:L,x2:W-R,y1:y+27+j*38,y2:y+27+j*38,stroke:'#e0e8eb'});});
    for(const b of r.bars){const bx=x(b.start),by=y+21+b.stage*38,bw=Math.max(4,x(b.end)-bx),n=svgNode('rect',g,{x:bx,y:by,width:bw,height:12,fill:b.failed?'#fff0ee':b.pending?'none':b.color,stroke:b.color,'stroke-dasharray':b.pending?'4 3':'none','data-stage':stageNames[b.stage],'data-status':b.failed?'failed':b.pending?'inProgress':b.late?'degraded':'completed'});svgNode('title',n,{},b.label+' | '+utc(b.start)+' to '+utc(b.end));
      if(b.failed){let hatch='';for(let dx=0;dx<bw;dx+=6)hatch+='M'+(bx+dx)+' '+(by+12)+' l'+Math.min(4,bw-dx)+' -12 ';svgNode('path',g,{d:hatch,stroke:b.color,'stroke-width':1,'pointer-events':'none'});}
    }
  });axis(s,height-48);
}
$('show-baseline').onchange=drawTimeline;drawTimeline();
function panel(entry){
  const {q,s,si}=entry,article=el('article',$('panels')),heading=el('div',article,null,'panel-heading');el('h3',heading,q.name+' / source '+(si+1));const remove=el('button',heading,'Remove');remove.type='button';remove.onclick=()=>article.remove();
  el('p',article,'Preview in raw query units | source '+(si+1)+' | step '+fmt(s.manifest.stepNanoseconds/1e9)+' s','muted');
  el('p',article,list(q.series).length+' of '+q.totals.series+' analyzed series included; '+q.omittedSeries+' omitted.','muted');
  const metadata=el('details',article);el('summary',metadata,'Query and source details');el('p',metadata,q.expr,'expr');el('p',metadata,s.directory+' / '+q.name+'.json','expr');
  const fields=el('fieldset',article,null,'display-labels');el('legend',fields,'Display labels');
  const keys=[...new Set(list(q.series).flatMap(ss=>Object.keys(ss.labels||{})))].sort(),checks=[];
  let defaults=['namespace','pod','container'].filter(k=>keys.includes(k));
  if(!defaults.length)defaults=keys.includes('node')?['node']:['job','instance'].filter(k=>keys.includes(k));
  for(const key of keys){const label=el('label',fields,null,'check'),input=el('input',label);input.type='checkbox';input.value=key;input.checked=defaults.includes(key);el('span',label,key);checks.push(input);}
  const collision=el('p',article,null,'muted');collision.setAttribute('role','status');
  const label=el('label',article,'Preview series (display labels only)');const choose=el('select',label,null,'series-chooser');choose.multiple=true;
  list(q.series).forEach((ss,i)=>{const o=el('option',choose);o.value=i;o.selected=i<4;o.title=ss.labelText||'(empty label set)';});
  const chart=el('div',article,null,'chart-scroll'),legend=el('ul',article,null,'legend'),details=el('details',article);el('summary',details,'Full labels and selected series metadata');const detailTable=el('div',details);
  function draw(){chart.replaceChildren();legend.replaceChildren();detailTable.replaceChildren();
    const displayKeys=checks.filter(c=>c.checked).map(c=>c.value),names=list(q.series).map(ss=>displayKeys.filter(k=>Object.hasOwn(ss.labels||{},k)).map(k=>k+'='+JSON.stringify(ss.labels[k])).join(', ')||'(no display labels)'),counts=new Map();
    for(const name of names)counts.set(name,(counts.get(name)||0)+1);
    const displayNames=names.map((name,i)=>name+(counts.get(name)>1?' [series '+(i+1)+']':''));
    Array.from(choose.options).forEach((o,i)=>o.textContent=displayNames[i]);
    collision.textContent=[...counts.values()].some(n=>n>1)?'Distinct raw series share the chosen display labels. Stable [series N] suffixes distinguish them; no series are merged or aggregated.':'Display labels affect names only; raw series identities and samples are unchanged.';
    const selected=Array.from(choose.selectedOptions,o=>({ss:q.series[Number(o.value)],index:Number(o.value)}));
    table(detailTable,['Series','Labels (all)','Preview points','Samples','Coverage','Gaps'],selected.map(({ss,index})=>[index+1,ss.labelText,list(ss.points).length,ss.samples,fmt(ss.coverage*100)+'%',ss.gaps]));
    let lo=Infinity,hi=-Infinity;for(const {ss} of selected)for(const pt of list(ss.points)){lo=Math.min(lo,pt[1]);hi=Math.max(hi,pt[1]);}if(lo===Infinity){el('p',chart,q.error||'No preview samples. Select an included series; omitted series remain in the local raw source.','bad');return;}
    if(lo===hi){const pad=Math.abs(lo)*.05||1;lo-=pad;hi+=pad;}
    const h=285,top=18,bottom=230,y=v=>bottom-(v-lo)/(hi-lo)*(bottom-top),node=svg(chart,h,q.name+' in raw query units');
    for(let i=0;i<=4;i++){const v=lo+(hi-lo)*i/4;svgNode('line',node,{x1:L,x2:W-R,y1:y(v),y2:y(v),stroke:'#e0e8eb'});svgNode('text',node,{x:L-8,y:y(v)+4,'text-anchor':'end'},fmt(v));}
    phaseMarkers(node,top,bottom);axis(node,bottom);
    selected.forEach(({ss,index})=>{const color=palette[index%palette.length];let path='',previous=null;for(const pt of list(ss.points)){path+=(previous&&previous[2]===pt[2]?' L':' M')+x(pt[0])+' '+y(pt[1]);previous=pt;}svgNode('path',node,{d:path,fill:'none',stroke:color,'stroke-width':1.8,'data-series':index+1});for(const pt of list(ss.points)){const dot=svgNode('circle',node,{cx:x(pt[0]),cy:y(pt[1]),r:2,fill:color});svgNode('title',dot,{},utc(pt[0])+' | '+fmt(pt[1])+' raw | series '+(index+1)+' | '+ss.labelText);}
      const item=el('li',legend),swatch=el('span',item,null,'swatch');swatch.style.backgroundColor=color;el('span',item,displayNames[index]);item.title='series '+(index+1)+' | '+ss.labelText;
    });
  }
  choose.onchange=draw;checks.forEach(c=>c.onchange=draw);draw();
}
function choices(){const select=$('query-choice');select.replaceChildren();let re;try{re=new RegExp($('query-filter').value,'i');}catch(e){$('filter-status').textContent='Invalid regular expression; all query counts remain in the table.';$('add-panel').disabled=true;return;}for(const [i,entry] of queries.entries())if(re.test(entry.q.name)){const option=el('option',select,entry.q.name+' / source '+(entry.si+1)+' / '+list(entry.q.series).length+' preview series');option.value=i;}$('add-panel').disabled=!select.options.length;$('filter-status').textContent=select.options.length+' matching queries; '+queries.length+' total. Filtering does not load omitted raw series.';}
$('query-filter').oninput=choices;$('add-panel').onclick=()=>{const entry=queries[Number($('query-choice').value)];if(entry)panel(entry);};choices();
const preferred=queries.filter(e=>/cpu|memory|gc|(^|-)up($|-)/i.test(e.q.name));(preferred.length?preferred:queries).slice(0,4).forEach(panel);
table($('query-table'),['Query','Source','Status','Analyzed series','Preview series','Omitted series','All samples','Valid','Invalid','Expected','Missing approx','Incomplete series','Aggregate coverage','Full local raw reference'],queries.map(({q,s,si})=>[q.name,si+1,q.status,q.totals.series,list(q.series).length,q.omittedSeries,q.totals.samples,q.totals.validSamples,q.totals.invalidSamples,q.totals.expected,q.totals.missingApprox,q.totals.incompleteSeries,q.totals.coverage==null?'unknown':fmt(q.totals.coverage*100)+'%',s.directory+' / '+q.name+'.json']));
const metricRows=[];for(const {q,si} of queries){for(const ss of list(q.series))metricRows.push([q.name,si+1,ss.labelText,q.status,ss.samples,ss.validSamples,ss.invalidSamples,ss.expected,ss.missingApprox,fmt(ss.coverage*100)+'%',ss.min,ss.max,q.status==='error'?null:ss.gaps,q.status==='error'?null:ss.zeroSamples,list(ss.points).length,q.error||'',utc(ss.firstTimestamp),utc(ss.lastTimestamp)]);}
metricRows.sort((a,b)=>compare(a[0],b[0])||compare(a[2],b[2])||a[1]-b[1]);
table($('metric-table'),['Query','Source','Labels (all)','Status','Samples','Valid','Invalid','Expected','Missing approx','Coverage','Min raw','Max raw','Internal gaps','Zero samples','Plotted','Error','First UTC','Last UTC'],metricRows);
const sourceRows=[],warnings=[];for(const [i,s] of list(data.sources).entries()){sourceRows.push([i+1,s.directory,s.manifest.version,utc(at(s.manifest.start)),utc(at(s.manifest.end)),s.manifest.stepNanoseconds/1e9,s.error||'No analysis error']);if(s.error)warnings.push(['source '+(i+1),'Analysis / integrity',s.error]);for(const q of list(s.queries)){if(q.error)warnings.push(['source '+(i+1)+' / '+q.name,'Query error',q.error]);for(const w of list(q.warnings))warnings.push(['source '+(i+1)+' / '+q.name,'Warning',w]);}}
for(const [i,s] of list(data.sources).entries()){if(s.manifestWarningsOmitted)warnings.push(['source '+(i+1),'Omitted manifest warnings',s.manifestWarningsOmitted+' warnings omitted; inspect local manifest.json']);for(const q of list(s.queries))if(q.omittedWarnings)warnings.push(['source '+(i+1)+' / '+q.name,'Omitted analysis warnings',q.omittedWarnings+' warnings omitted; inspect full local evidence']);}
for(const w of list(p.warnings))warnings.push([w.source,'Progress warning',w.reason]);
table($('source-table'),['Source','Directory','Manifest version','Start UTC','End UTC','Step s','Analysis / integrity'],sourceRows);
table($('warning-table'),['Source','Kind','Detail'],warnings);
const manifests=el('details',$('source-table'));el('summary',manifests,'Manifest references (full source filenames and hashes; bounded warnings)');el('pre',manifests,JSON.stringify(list(data.sources).map(s=>({directory:s.directory,manifest:s.manifest})),null,2)).style.whiteSpace='pre-wrap';
table($('command-table'),['Command','State','Started UTC','Exit code'],list(p.commands).map(c=>[c.name,c.state,utc(at(c.startedAt)),c.exitCode]));
table($('condition-table'),['Condition','Status'],list(p.conditions).map(c=>[c.type,c.status]));
$('turnover').textContent=p.turnover?JSON.stringify(p.turnover,null,2):'No turnover evidence';
</script></body></html>`

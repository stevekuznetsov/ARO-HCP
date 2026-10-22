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

const pageJS = `{{define "script"}}<script>
"use strict";
(() => {
const report=JSON.parse(document.getElementById("report-data").textContent), c=report.card;
const colors=["#08796f","#bc5b21","#526f96","#857b6c","#8a4f73","#526540"];
const finite=v=>typeof v==="number"&&Number.isFinite(v)&&v>=0;
const fmt=(v,d=3)=>finite(v)?v.toFixed(d):"unknown";
const el=(tag,attrs={},text)=>{const e=document.createElementNS("http://www.w3.org/2000/svg",tag);Object.entries(attrs).forEach(([k,v])=>e.setAttribute(k,String(v)));if(text!==undefined)e.textContent=text;return e;};
function svg(id,width,height,title){const host=typeof id==="string"?document.getElementById(id):id;const s=el("svg",{viewBox:"0 0 "+width+" "+height,role:"img","aria-label":title});s.append(el("title",{},title));host.append(s);return s;}
function line(s,x1,y1,x2,y2,color="#ccd4cd",dash){s.append(el("line",{x1,y1,x2,y2,stroke:color,...(dash?{"stroke-dasharray":dash}:{})}));}
function text(s,x,y,value,attrs={}){s.append(el("text",{x,y,...attrs},value));}
function bar(s,x,y,width,height,color,title,outline=false){if(![x,y,width,height].every(Number.isFinite)||width<0)return;const r=el("rect",{x,y,width,height,fill:outline?"none":color,stroke:color,rx:2});r.append(el("title",{},title));s.append(r);}
function axis(s,left,right,y,max,unit){for(let i=0;i<=4;i++){const x=left+(right-left)*i/4;line(s,x,25,x,y);text(s,x,y+20,(max*i/4).toFixed(max<=10?1:0),{"text-anchor":"middle"});}text(s,right,y+40,unit,{"text-anchor":"end"});}
function legend(host,items){const p=document.createElement("p");p.className="legend";items.forEach(([label,color])=>{const span=document.createElement("span");span.style.setProperty("--color",color);span.textContent=label;p.append(span);});host.append(p);}
const arms=c.apfComparison.arms;
const cases=c.densityScreens.cases, densityMax=Math.max(1,...cases.map(x=>finite(x.applicationPods)?x.applicationPods:0));
const ds=svg("density-chart",800,60+cases.length*36,"Application Pod population for each density screen");
cases.forEach((d,i)=>{const y=28+i*36;text(ds,0,y+4,fmt(d.applicationPods,0)+" Pods");if(finite(d.applicationPods)){const x=125+610*d.applicationPods/densityMax;line(ds,125,y,735,y);line(ds,125,y,x,y,colors[0]);ds.append(el("circle",{cx:x,cy:y,r:5,fill:colors[0]}));text(ds,Math.min(x+12,745),y+4,fmt(d.completedRollouts,0)+"/"+fmt(d.scheduledRollouts,0),{"text-anchor":x>710?"end":"start"});}});
const offered=c.apfComparison.client?.offeredQPS;
const qmax=Math.max(1,finite(offered)?offered:0,...arms.map(a=>finite(a.successfulQPSActiveDenominatorIncludingDrainCompletions)?a.successfulQPSActiveDenominatorIncludingDrainCompletions:0));
const q=svg("throughput-chart",520,90+arms.length*88,"Offered and body-complete requests per second");
axis(q,100,460,30+arms.length*88,qmax,"requests / second");
arms.forEach((a,i)=>{const y=45+i*88,v=a.successfulQPSActiveDenominatorIncludingDrainCompletions;text(q,0,y+16,fmt(a.shares,0)+" shares");if(finite(offered)){bar(q,100,y,360*offered/qmax,22,colors[3],"Offered "+fmt(offered),true);text(q,105,y+15,"Offered "+fmt(offered,0));}if(finite(v)){bar(q,100,y+29,360*v/qmax,22,colors[0],"Body complete "+fmt(v));text(q,105+360*v/qmax,y+45,fmt(v));}});
const lmax=Math.max(1,...arms.flatMap(a=>[a.successfulGETP95Seconds,a.successfulLISTP95Seconds]).filter(finite));
const l=svg("latency-chart",520,90+arms.length*88,"Successful GET and LIST p95 latency in seconds");
axis(l,100,450,30+arms.length*88,Math.ceil(lmax),"seconds");
arms.forEach((a,i)=>{const y=45+i*88;text(l,0,y+16,fmt(a.shares,0)+" shares");[[a.successfulGETP95Seconds,colors[0],"GET"],[a.successfulLISTP95Seconds,colors[1],"LIST"]].forEach(([v,color,label],j)=>{if(finite(v)){bar(l,100,y+j*29,350*v/Math.ceil(lmax),22,color,label+" "+fmt(v)+" s");text(l,105+350*v/Math.ceil(lmax),y+16+j*29,fmt(v));}});});
legend(document.getElementById("latency-chart"),[["GET p95",colors[0]],["LIST p95",colors[1]]]);
const outcomeHost=document.getElementById("outcomes-chart"), maxOffers=Math.max(1,...arms.map(a=>a.offered).filter(finite));
const os=svg(outcomeHost,900,55+arms.length*66,"Request outcomes, counted against logical offers");
arms.forEach((a,i)=>{const y=22+i*66;const vals=[a.successfulBodies,a.http429,a.timeouts,a.generatorLimited];const totals=report.experience?.arms.find(e=>e.id===a.id)?.totals;text(os,0,y+17,fmt(a.shares,0)+" shares");if(totals?.http429TimeoutOverlap!==0||totals?.otherErrors!==0||!finite(a.offered)||a.offered<=0||!vals.every(finite)||vals.reduce((x,y)=>x+y,0)!==a.offered){text(os,105,y+17,"Disjoint partition not established; see explicit counts in table.");return;}let x=105;vals.forEach((v,j)=>{const width=640*v/maxOffers;bar(os,x,y,width,25,colors[j],fmt(v,0)+" of "+fmt(a.offered,0)+" offers ("+(100*v/a.offered).toFixed(1)+"%)");if(width>55)text(os,x+width/2,y+17,fmt(v,0),{"text-anchor":"middle",style:"fill:white"});x+=width;});text(os,755,y+17,"/ "+fmt(a.offered,0)+" offers");});
legend(outcomeHost,[["Successful bodies",colors[0]],["HTTP 429",colors[1]],["Timeouts",colors[2]],["Generator limited",colors[3]]]);
if(!report.contention)return;
const contention=report.contention, nodes=contention.series.map(n=>({label:n.label,cpuBusyPercent:n.points.map(p=>[p.timestamp,p.busyPercent]),burnerCPUCores:n.points.map(p=>[p.timestamp,p.adversaryCores])})), container=document.getElementById("contention-charts");
const times=nodes.flatMap(n=>[...(n.cpuBusyPercent||[]),...(n.burnerCPUCores||[])]).map(p=>p[0]);
arms.forEach(a=>{if(finite(a.startEpoch))times.push(a.startEpoch);if(finite(a.stopEpoch))times.push(a.stopEpoch);});
if(!times.length){container.textContent="No timestamped contention samples supplied.";return;}
let tmin=contention.window.start,tmax=contention.window.end;if(tmax<=tmin)tmax=tmin+1;
const tx=t=>70+790*(t-tmin)/(tmax-tmin), stamp=t=>new Date(t*1000).toISOString().slice(11,19);
const burnerMax=contention.series.reduce((max,n)=>Math.max(max,n.capacityCores),0);
[["cpuBusyPercent",100,"Management-worker CPU busy","% non-idle CPU"],["burnerCPUCores",burnerMax,"Runnable adversary CPU","CPU cores"]].forEach(([key,max,title,unit])=>{
const fig=document.createElement("figure"),heading=document.createElement("h3"),host=document.createElement("div"),toggles=document.createElement("fieldset"),toggleTitle=document.createElement("legend");heading.textContent=title;host.className="chart";toggles.className="worker-toggles";toggleTitle.textContent="Show workers: "+title;toggles.append(toggleTitle);fig.append(heading,toggles,host);container.append(fig);const s=svg(host,900,310+nodes.length*24,title+", UTC; missing samples break the line");s.setAttribute("data-metric",key);s.setAttribute("data-axis-max",max);
arms.forEach(a=>{if(!finite(a.startEpoch)||!finite(a.stopEpoch))return;bar(s,tx(a.startEpoch),35,tx(a.stopEpoch)-tx(a.startEpoch),165,"#e8e1d3",fmt(a.shares,0)+" shares arm");text(s,tx(a.startEpoch)+6,26,fmt(a.shares,0)+" shares");});
(contention.boundaries||[]).filter(b=>b.id==="burners-started"||b.id==="adversaries-stop-started").forEach(b=>{line(s,tx(b.timestamp),35,tx(b.timestamp),200,colors[1],"4 3");text(s,tx(b.timestamp)-4,190,b.id==="burners-started"?"Burners start":"Stop invoked",{transform:"rotate(-90 "+(tx(b.timestamp)-4)+" 190)",style:"fill:#bc5b21"});});
for(let i=0;i<=4;i++){const y=200-i*165/4;line(s,70,y,860,y);text(s,60,y+4,String(max*i/4),{"text-anchor":"end"});const t=tmin+(tmax-tmin)*i/4;text(s,tx(t),224,stamp(t),{"text-anchor":"middle"});}text(s,70,252,unit);text(s,860,252,"UTC",{"text-anchor":"end"});
text(s,70,278,"Sample coverage: colored tick = present; gray tick = No sample (not zero)");
nodes.forEach((n,i)=>{
const color=colors[i%colors.length],trace=el("g",{id:"trace-"+key+"-"+i,"data-worker":n.label,class:"worker-trace"});s.append(trace);
const label=document.createElement("label"),input=document.createElement("input"),name=document.createElement("span");input.type="checkbox";input.checked=true;input.setAttribute("aria-controls",trace.id);name.textContent=n.label;label.style.setProperty("--color",color);label.append(input,name);toggles.append(label);input.addEventListener("change",()=>{trace.style.display=input.checked?"":"none";});
let path="",active=false,previous=null;const points=n[key]||[];
points.forEach(p=>{if(!finite(p[1])){active=false;return;}if(previous!==null&&p[0]-previous>contention.window.expectedStepSeconds*1.5)active=false;const x=tx(p[0]),y=200-165*p[1]/max;path+=(active?" L":" M")+x.toFixed(3)+","+y.toFixed(3);active=true;previous=p[0];const dot=el("circle",{cx:x,cy:y,r:1.8,fill:color});dot.append(el("title",{},n.label+" / "+stamp(p[0])+" UTC / "+fmt(p[1])+" "+unit));trace.append(dot);});if(path)trace.append(el("path",{d:path,fill:"none",stroke:color,"stroke-width":2,"stroke-dasharray":i===1?"7 3":i===2?"2 3":"none"}));
// Coverage stays visible even when a trace is hidden or overlaps another worker.
const row=el("g",{class:"coverage-row","data-worker":n.label}),y=300+i*24,step=contention.window.expectedStepSeconds,slots=new Map(points.map(p=>[Math.round((p[0]-tmin)/step),p[1]]));text(row,60,y+4,n.label,{"text-anchor":"end"});
for(let slot=0;slot<contention.window.expectedPointsPerNode;slot++){const t=tmin+slot*step,present=finite(slots.get(slot)),tick=el("line",{x1:tx(t),x2:tx(t),y1:y-5,y2:y+5,stroke:present?color:"#a6aaa5","stroke-width":present?3:1,"data-sample":present?"present":"missing"});tick.append(el("title",{},n.label+" / "+stamp(t)+" UTC / "+(present?"Sample present":"No sample (not zero)")));row.append(tick);}s.append(row);
});
const summary=document.createElement("p");summary.className="series-summary";summary.textContent=nodes.map(n=>{const values=(n[key]||[]).map(p=>p[1]).filter(finite);return n.label+": "+values.length+" samples"+(values.length?", range "+fmt(values.reduce((a,b)=>Math.min(a,b)),2)+" to "+fmt(values.reduce((a,b)=>Math.max(a,b)),2)+" "+unit:", unavailable");}).join("; ");fig.append(summary);
});
})();
</script>{{end}}`

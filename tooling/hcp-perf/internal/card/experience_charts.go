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

const experienceJS = `{{define "experience-script"}}<script>
"use strict";
(() => {
const report=JSON.parse(document.getElementById("report-data").textContent), e=report.experience;
if(!e)return;
const teal="#08796f",orange="#bc5b21",purple="#687bc1",gray="#78827e",armColors=[teal,purple];
const finite=v=>typeof v==="number"&&Number.isFinite(v)&&v>=0;
const fmt=(v,d=3)=>finite(v)?v.toFixed(d):"unknown";
const stamp=t=>finite(t)?new Date(t*1000).toISOString():"unknown";
const short=t=>stamp(t).slice(11,19);
const node=(tag,attrs={},text)=>{const n=document.createElementNS("http://www.w3.org/2000/svg",tag);Object.entries(attrs).forEach(([k,v])=>n.setAttribute(k,String(v)));if(text!==undefined)n.textContent=text;return n;};
const add=(s,tag,attrs,text)=>{const n=node(tag,attrs,text);s.append(n);return n;};
const tip=(n,text)=>n.append(node("title",{},text));
const x=t=>70+790*(t-e.window.start)/(e.window.end-e.window.start);
const checked=id=>document.getElementById(id).checked;
const line=(s,x1,y1,x2,y2,color,dash,attrs={})=>add(s,"line",{x1,y1,x2,y2,stroke:color,"stroke-width":1.5,...(dash?{"stroke-dasharray":dash}:{}),...attrs});
function plot(id,height,title){const host=document.getElementById(id);host.replaceChildren();const s=add(host,"svg",{viewBox:"0 0 900 "+height,role:"img","aria-label":title,"data-window-start":e.window.start,"data-window-end":e.window.end});tip(s,title);return s;}
function axes(s,top,bottom,max,unit,showLabels=true){
e.arms.forEach((a,i)=>{
 const fill=armColors[i%armColors.length];
 const shade=add(s,"rect",{x:x(a.start),y:top,width:x(a.stop)-x(a.start),height:bottom-top,fill,"fill-opacity":.065,"data-arm-background":a.id});tip(shade,a.shares+" APF shares; configured "+a.offeredQPS+" QPS, not a shares-to-QPS conversion");
 if(showLabels)add(s,"text",{x:x((a.start+a.stop)/2),y:top-12,"text-anchor":"middle"},a.shares+" shares");
 [[a.start,"Clients start",null],[a.stop,"Scheduling stops","5 3"],[a.drainEnd,"API drain ends","1 3"]].forEach(([t,label,dash])=>tip(line(s,x(t),top,x(t),bottom,gray,dash,{"data-boundary":label}),label+" "+stamp(t)));
});
for(let i=0;i<=4;i++){const y=bottom-(bottom-top)*i/4;line(s,70,y,860,y,"#ccd4cd");add(s,"text",{x:60,y:y+4,"text-anchor":"end"},fmt(max*i/4,max<2?2:1));const t=e.window.start+(e.window.end-e.window.start)*i/4;add(s,"text",{x:x(t),y:bottom+19,"text-anchor":"middle","data-utc-tick":t},short(t));}
add(s,"text",{x:70,y:bottom+37},unit);add(s,"text",{x:860,y:bottom+37,"text-anchor":"end"},"UTC");
return v=>bottom-(bottom-top)*v/max;
}
function binTip(a,b,label,value){return a.shares+" shares / "+label+" "+fmt(value)+"\nCompletion interval ["+stamp(b.start)+", "+stamp(b.end)+(b.end===a.drainEnd?"]":" )")+"\nWidth "+fmt(b.end-b.start,6)+"s; completed n="+b.completed+", successful n="+b.successful+", HTTP 429="+b.http429+"; dispatched by start="+b.dispatched;}
function steps(s,a,key,y,color,scale=1,dash){
 const g=add(s,"g",{"data-arm":a.id,"data-series":key});
 // One horizontal segment per measured bin, never a bridge over an empty bin
 // or the inter-arm gap. Boundaries, including the final partial bin, survive.
 a.bins.forEach(b=>{if(!b.completed||!finite(b[key]))return;const v=b[key]*scale;const n=line(g,x(b.start),y(v),x(b.end),y(v),color,dash,{"data-bin-start":b.start,"data-bin-end":b.end,"data-value":v});tip(n,binTip(a,b,key,v));});
}
function clients(){
 const total=checked("experience-total-toggle"),outcome=checked("experience-outcome-toggle"),timeout=checked("experience-timeout-toggle");
 const qmax=Math.max(1,...e.arms.flatMap(a=>[a.offeredQPS,...a.bins.flatMap(b=>[b.successfulQPS,...(total?[b.completionQPS]:[])])]).filter(finite))*1.08;
 const q=plot("experience-throughput",260,"Successful body-complete throughput and configured target offered QPS");q.dataset.axisMax=qmax;
 const qy=axes(q,35,210,qmax,"requests / second");
 e.arms.forEach(a=>{steps(q,a,"successfulQPS",qy,teal);if(total)steps(q,a,"completionQPS",qy,gray,1,"5 3");const g=add(q,"g",{"data-arm":a.id,"data-series":"targetOfferedQPS"});a.targetOfferedQPS.forEach(b=>tip(line(g,x(b.start),qy(b.qps),x(b.end),qy(b.qps),purple,"6 3",{"data-value":b.qps}),"Target Offered QPS (configured): "+b.qps+" / ["+stamp(b.start)+", "+stamp(b.end)+"]"));line(g,x(a.stop),qy(a.offeredQPS),x(a.stop),qy(0),purple,"6 3");});
 const r=plot("experience-rejections",260,"HTTP 429 fraction: percent of all completed requests in each bin");r.dataset.axisMax=100;
 const ry=axes(r,35,210,100,"HTTP 429 / completed requests (%)");e.arms.forEach(a=>steps(r,a,"rejectedFraction",ry,orange,100));
 const deadline=report.card.apfComparison.client?.requestTimeoutSeconds;
 const fields=[["getSuccessP95Seconds",teal,"Successful GET p95"],["listSuccessP95Seconds",purple,"Successful LIST p95"],...(outcome?[["latencyP95Seconds",gray,"All-outcome p95"]]:[])];
 const lmax=Math.max(1,...e.arms.flatMap(a=>a.bins.flatMap(b=>fields.map(([key])=>b[key]))).filter(finite),timeout&&finite(deadline)?deadline:0)*1.08;
 const l=plot("experience-latency",260,"Successful GET and LIST raw per-bin p95 latency");l.dataset.axisMax=lmax;
 const ly=axes(l,35,210,lmax,"body-inclusive p95 seconds");
 e.arms.forEach(a=>fields.forEach(([key,color,label])=>{const g=add(l,"g",{"data-arm":a.id,"data-series":key});let path="",active=false;a.bins.forEach(b=>{if(!finite(b[key])){active=false;return;}const cx=x((b.start+b.end)/2),cy=ly(b[key]);path+=(active?" L":" M")+cx+","+cy;active=true;const p=add(g,"circle",{cx,cy,r:2.6,fill:color,"data-value":b[key],"data-bin-start":b.start});tip(p,binTip(a,b,label,b[key]));});if(path)add(g,"path",{d:path,fill:"none",stroke:color,"stroke-width":2,...(key==="latencyP95Seconds"?{"stroke-dasharray":"5 3"}:{})});}));
 if(timeout&&finite(deadline))tip(line(l,70,ly(deadline),860,ly(deadline),orange,"6 4",{"data-reference":"client-timeout"}),"Client request/body timeout "+deadline+"s; hard client reference, not an SLO");
}
const allCycles=[...e.canaries,...e.arms.flatMap(a=>a.canaries)];
function canaries(){
 const baseline=checked("experience-baseline-toggle"),hard=checked("experience-hard-toggle");
 const cycles=allCycles.filter(c=>baseline||c.phase!=="baseline");
 const s=plot("experience-canaries",420,"Canary ACK, Ready including ACK, and Cleanup durations at cycle start UTC");
 [["ack","ACK"],["ready","Ready (includes ACK)"],["cleanup","Cleanup"]].forEach(([key,label],i)=>{
  const g=add(s,"g",{"data-phase":key}),top=24+i*136,bottom=top+80;
  const max=Math.max(.001,...cycles.flatMap(c=>{const p=c.stages[key];return [p.durationSeconds,p.expectedSeconds,...(hard?[p.hardDeadlineSeconds]:[])];}).filter(finite))*1.12;g.dataset.axisMax=max;
  const y=axes(g,top,bottom,max,label+" seconds",false);
  cycles.forEach(c=>{
   const p=c.stages[key],cx=x(c.started),color=c.phase==="baseline"?gray:armColors[e.arms.findIndex(a=>a.id===c.armID)%armColors.length];
   const details=c.id+" / "+label+" / cycle status: "+c.status+"\nX = cycle preflight start: "+stamp(c.started)+"\nActual stage start: "+(p.startedRFC3339??"unknown")+"\nActual stage completion: "+(p.completedRFC3339??"unknown")+"\nRecorded "+(finite(p.completed)?"duration":"elapsed/censored (not completed)")+": "+fmt(p.durationSeconds,9)+"s ("+(p.durationNanoseconds??"unknown")+"ns)\nSoft expectation: "+fmt(p.expectedSeconds,9)+"s; hard deadline: "+fmt(p.hardDeadlineSeconds)+"s";
   // A local reference segment belongs to this cycle, so varying expectations
   // and absent baseline expectations never become invented global thresholds.
   if(finite(p.expectedSeconds))tip(line(g,cx-6,y(p.expectedSeconds),cx+6,y(p.expectedSeconds),orange,"3 2",{"data-reference":"soft","data-cycle":c.id}),details);
   if(hard&&finite(p.hardDeadlineSeconds))tip(line(g,cx-6,y(p.hardDeadlineSeconds),cx+6,y(p.hardDeadlineSeconds),gray,"3 2",{"data-reference":"hard","data-cycle":c.id}),details);
   if(!finite(p.completed)||!finite(p.durationSeconds)){
    const cy=finite(p.durationSeconds)?y(p.durationSeconds):top+8;
    const cross=add(g,"path",{d:"M"+(cx-4)+","+(cy-4)+" l8,8 m-8,0 l8,-8",fill:"none",stroke:orange,"stroke-width":2,"data-incomplete":c.id,"data-censored":finite(p.durationSeconds)});tip(cross,details+"\nMissing duration is marked at the lane top, not at zero.");return;
   }
   const over=finite(p.expectedSeconds)&&p.durationSeconds>p.expectedSeconds;
   const dot=add(g,"circle",{cx,cy:y(p.durationSeconds),r:3.5,fill:c.phase==="baseline"?"#fffef9":color,stroke:color,"stroke-width":1.5,"data-cycle":c.id,"data-baseline":c.phase==="baseline","data-value":p.durationSeconds});tip(dot,details);
   if(over)tip(add(g,"circle",{cx,cy:y(p.durationSeconds),r:6,fill:"none",stroke:orange,"stroke-width":1.5,"data-soft-excess":c.id}),details+"\nAbove soft expectation: degradation, not an abort.");
  });
 });
}
["experience-total-toggle","experience-outcome-toggle","experience-timeout-toggle"].forEach(id=>document.getElementById(id).addEventListener("change",clients));
["experience-baseline-toggle","experience-hard-toggle"].forEach(id=>document.getElementById(id).addEventListener("change",canaries));
clients();canaries();
})();
</script>{{end}}`

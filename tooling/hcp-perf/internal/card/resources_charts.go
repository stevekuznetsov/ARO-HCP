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

// This separate IIFE runs even when the optional contention script returns early.
const resourcesJS = `{{define "resource-script"}}<script>
"use strict";
(() => {
const report=JSON.parse(document.getElementById("report-data").textContent), r=report.resources;
if(!r)return;
const finite=v=>typeof v==="number"&&Number.isFinite(v)&&v>=0;
const fmt=(v,d=3)=>finite(v)?v.toFixed(d):"unknown";
const stamp=t=>new Date(t*1000).toISOString().slice(11,19);
const colors=["#08796f","#3568aa","#63573d","#963d64","#527f31","#406e78"];
const orange="#bc5b21",purple="#8045ab";
const dom=(tag,parent,text)=>{const e=document.createElement(tag);if(text!==undefined)e.textContent=text;if(parent)parent.append(e);return e;};
const svgEl=(tag,parent,attrs={},text)=>{const e=document.createElementNS("http://www.w3.org/2000/svg",tag);Object.entries(attrs).forEach(([k,v])=>e.setAttribute(k,String(v)));if(text!==undefined)e.textContent=text;parent.append(e);return e;};
const expected=Math.ceil((r.window.end-r.window.start)/r.window.expectedStepSeconds-1e-7);
const short=s=>s.pod.split("-").slice(-1)[0]+" / "+s.container;
const stats=(s,key)=>{const p=s.points.filter(p=>finite(p[key]));return {count:p.length,peak:p.length?p.reduce((v,p)=>Math.max(v,p[key]),0):null,first:p[0]?.timestamp,last:p[p.length-1]?.timestamp};};
const signedPercent=v=>Number.isFinite(v)?v.toFixed(1):"unknown";
const ratio=(v,b)=>finite(v)&&finite(b)&&b>0?fmt(v/b)+"x ("+signedPercent(100*(v/b-1))+"% vs budget)":"unknown ratio";
const sumKnown=values=>values.every(finite)?values.reduce((a,b)=>a+b,0):null;
const metrics=["cpuCores","usageMiB","workingSetMiB","rssMiB","goManagedMiB"];
// Aggregates require every member at the exact native timestamp. Never sum Go
// targets or partially attributed Go curves into a fictitious HCP runtime.
function aggregate(includeInstrumentation){
 const members=r.series.filter(s=>includeInstrumentation||!s.instrumentation), maps=members.map(s=>new Map(s.points.map(p=>[Math.round((p.timestamp-r.window.start)/r.window.expectedStepSeconds),p])));
 const points=Array.from({length:expected},(_,i)=>{const p={timestamp:r.window.start+i*r.window.expectedStepSeconds,goManagedMiB:null};metrics.filter(k=>k!=="goManagedMiB").forEach(k=>p[k]=sumKnown(maps.map(m=>m.get(i)?.[k])));return p;});
 return {id:"aggregate",pod:"whole-HCP",container:includeInstrumentation?"including instrumentation":"excluding instrumentation",requests:{cpuCores:sumKnown(members.map(s=>s.requests.cpuCores)),memoryMiB:sumKnown(members.map(s=>s.requests.memoryMiB))},limits:{},goMemoryLimitMiB:null,points};
}
function panel(host,title,series,isAggregate=false){
 host.replaceChildren();const root=dom("div",host);root.className="resource-panel";dom("h3",root,title);
 const controls=dom("div",root);controls.className="resource-controls";
 let memory="usageMiB",rss=false,showGo=true,hard=false,includeInstrumentation=false;
 const selected=new Set(series.map(s=>s.id));
 function check(parent,label,value,fn,disabled=false,kind="checkbox",name=""){
  const l=dom("label",parent),input=dom("input",l);input.type=kind;input.checked=value;input.disabled=disabled;if(name)input.name=name;dom("span",l,label);input.addEventListener("change",()=>fn(input.checked));return input;
 }
 const radioName="memory-"+title;
 [["usageMiB","Cgroup usage"],["workingSetMiB","Working set"]].forEach(([key,label])=>check(controls,label,key===memory,()=>{memory=key;draw();},false,"radio",radioName));
 check(controls,"RSS",false,v=>{rss=v;draw();});
 const hasGo=series.some(s=>s.points.some(p=>finite(p.goManagedMiB)));
 check(controls,hasGo?"Go-managed":"Go-managed unavailable (no reviewed curve)",hasGo,v=>{showGo=v;draw();},!hasGo);
 check(controls,"Hard memory limit (expands axis)",false,v=>{hard=v;draw();},!series.some(s=>finite(s.limits.memoryMiB)));
 if(isAggregate)check(controls,"Include experimental instrumentation",false,v=>{includeInstrumentation=v;draw();});
 const replicas=dom("fieldset",root);replicas.className="resource-controls resource-replicas";dom("legend",replicas,isAggregate?"Complete-timestamp namespace aggregate; no Go sum":"Replicas / containers (all selected by default)");
 const inputs=[];
 if(!isAggregate){const all=dom("button",replicas,"All replicas");all.type="button";all.onclick=()=>{series.forEach(s=>selected.add(s.id));inputs.forEach(i=>i.checked=true);draw();};series.forEach((s,i)=>{const input=check(replicas,short(s),true,v=>{if(v)selected.add(s.id);else selected.delete(s.id);draw();});input.parentElement.style.setProperty("--replica",colors[i%colors.length]);input.parentElement.title=s.id;inputs.push(input);});}
 const legend=dom("p",root,"Replica colors: selected cgroup metric (solid), Go-managed (faint dashed), RSS (fine dashed). Request: dashed black if shared, otherwise replica color. GOMEMLIMIT: dotted purple. Excess: orange above request; purple Go-managed above its own target.");legend.className="muted";
 const charts=dom("div",root);charts.className="chart-pair";const summary=dom("div",root);summary.className="resource-summary";
 function draw(){
  charts.replaceChildren();summary.replaceChildren();const active=isAggregate?[aggregate(includeInstrumentation)]:series.filter(s=>selected.has(s.id));
  if(!active.length){dom("p",charts,"No replicas selected. Coverage below still describes every supplied replica.");}
  else [["cpuCores","CPU (cores)"],[memory,(memory==="usageMiB"?"Cgroup usage":"Working set")+" (MiB)"]].forEach(([key,label])=>{
   const cpu=key==="cpuCores",keys=cpu?[key]:[key,...(rss?["rssMiB"]:[]),...(showGo&&hasGo?["goManagedMiB"]:[])];
   const budget=s=>cpu?s.requests.cpuCores:s.requests.memoryMiB;
   const maxima=active.flatMap(s=>[...keys.map(k=>stats(s,k).peak),budget(s),...(!cpu&&showGo?[s.goMemoryLimitMiB]:[]),...(!cpu&&hard?[s.limits.memoryMiB]:[])]).filter(finite);
   const max=Math.max(cpu?0.01:1,...maxima)*1.08;
   const fig=dom("figure",charts);fig.className="resource-chart";dom("h4",fig,label);
   const plot=dom("div",fig);plot.className="resource-plot";plot.tabIndex=0;plot.setAttribute("role","region");plot.setAttribute("aria-label",title+" "+label+" scrollable plot");
   const svg=svgEl("svg",plot,{viewBox:"0 0 520 275",role:"img","aria-label":title+" "+label+", shared UTC","data-resource-metric":key,"data-axis-max":max});svgEl("title",svg,{},title+" "+label);
   const x=t=>55+445*(t-r.window.start)/(r.window.end-r.window.start),y=v=>220-180*v/max;
   report.card.apfComparison.arms.forEach(a=>{svgEl("rect",svg,{x:x(a.startEpoch),y:40,width:x(a.stopEpoch)-x(a.startEpoch),height:180,fill:"#e8e1d3",class:"resource-phase","data-start":a.startEpoch,"data-end":a.stopEpoch});svgEl("text",svg,{x:x(a.startEpoch)+3,y:27},fmt(a.shares,0)+" shares");});
   for(let i=0;i<=4;i++){const v=max*i/4;svgEl("line",svg,{x1:55,x2:500,y1:y(v),y2:y(v),stroke:"#ccd4cd"});svgEl("text",svg,{x:49,y:y(v)+4,"text-anchor":"end"},fmt(v,cpu?2:0));}
   for(let i=0;i<=2;i++){const t=r.window.start+(r.window.end-r.window.start)*i/2;svgEl("text",svg,{x:x(t),y:243,"text-anchor":i===0?"start":i===2?"end":"middle"},stamp(t));}svgEl("text",svg,{x:500,y:265,"text-anchor":"end"},"UTC");
   const shared=active.every(s=>budget(s)===budget(active[0]));
   const reference=(v,color,dash,kind,s)=>{if(!finite(v))return;const l=svgEl("line",svg,{x1:55,x2:500,y1:y(v),y2:y(v),stroke:color,"stroke-dasharray":dash,class:"resource-reference","data-kind":kind,"data-value":v,"data-series":s.id});svgEl("title",l,{},short(s)+" "+kind+" "+fmt(v));};
   active.forEach((s,index)=>{
    const color=colors[(isAggregate?0:series.indexOf(s))%colors.length];
    if(!shared||index===0)reference(budget(s),shared?"#202a29":color,"7 4","request",s);
    if(!cpu&&showGo)reference(s.goMemoryLimitMiB,purple,"2 4","GOMEMLIMIT",s);
    if(!cpu&&hard)reference(s.limits.memoryMiB,"#7c3030","10 3 2 3","hard-memory",s);
    keys.forEach(metric=>{
     const go=metric==="goManagedMiB",secondary=metric!==key,b=go?s.goMemoryLimitMiB:metric===key?budget(s):null,excess=go?purple:orange;
     const trace=svgEl("g",svg,{class:"resource-trace","data-series":s.id,"data-metric":metric});let previous=null,path="";
     s.points.forEach(p=>{
      const v=p[metric];if(!finite(v)){previous=null;return;}
      const adjacent=previous&&p.timestamp-previous.timestamp<=r.window.expectedStepSeconds*1.5;
      path+=(adjacent?" L":" M")+x(p.timestamp)+","+y(v);
      const over=finite(b)&&v>b,dot=svgEl("circle",trace,{cx:x(p.timestamp),cy:y(v),r:secondary?1:1.6,fill:over?excess:color,"data-over-budget":over,"data-timestamp":p.timestamp});svgEl("title",dot,{},short(s)+" / "+stamp(p.timestamp)+" UTC / "+metric+" "+fmt(v)+" / budget "+fmt(b));
      if(adjacent&&finite(b)&&(v>b||previous[metric]>b)){
       let t1=previous.timestamp,v1=previous[metric],t2=p.timestamp,v2=v;
       if((v1>b)!==(v2>b)){const cross=t1+(t2-t1)*(b-v1)/(v2-v1);if(v1>b){t2=cross;v2=b;}else{t1=cross;v1=b;}}
       svgEl("path",trace,{d:"M"+x(t1)+","+y(v1)+" L"+x(t2)+","+y(v2),fill:"none",stroke:excess,"stroke-width":3.5,class:"resource-excess","data-kind":go?"go-target":"request"});
      }
      previous=p;
     });
     if(path){const line=svgEl("path",trace,{d:path,fill:"none",stroke:color,"stroke-width":secondary?1.2:1.7,opacity:secondary?.6:1,"stroke-dasharray":go?"5 3":metric==="rssMiB"?"2 3":"none",class:"resource-line"});trace.prepend(line);}
    });
   });
   dom("figcaption",fig,"Sampled observations; no lines or highlighted areas bridge missing slots. Hover a dot for replica, UTC, value and its own budget.");
  });
  const described=isAggregate?active:series;
  described.forEach(s=>{
   const p=dom("p",summary);p.dataset.series=s.id;dom("strong",p,short(s)+(selected.has(s.id)||isAggregate?"":" (hidden)")+": ");
   [["cpuCores",s.requests.cpuCores,"CPU"],[memory,s.requests.memoryMiB,memory==="usageMiB"?"Cgroup usage":"Working set"],["goManagedMiB",s.goMemoryLimitMiB,"Go-managed"]].forEach(([key,b,label])=>{const v=stats(s,key);dom("span",p,label+" peak "+fmt(v.peak)+" / "+fmt(b)+" budget = "+ratio(v.peak,b)+"; ");});
   dom("span",p,"hard memory "+fmt(s.limits.memoryMiB)+" MiB. ");
   metrics.forEach(k=>{const v=stats(s,k);dom("span",p,k+" coverage "+v.count+"/"+expected+(v.count?" ("+stamp(v.first)+"-"+stamp(v.last)+" UTC)":" (unknown; no valid samples)")+"; ");});
  });
 }
 draw();
}
const primary=document.getElementById("resource-primary");
[["KAS / all main replicas","kube-apiserver","kube-apiserver"],["Etcd / all main replicas","etcd","etcd"]].forEach(([title,workload,container])=>{const group=r.series.filter(s=>s.workload===workload&&s.container===container);if(group.length)panel(dom("div",primary),title,group);});
const picker=document.getElementById("resource-picker"),presets=dom("div",picker);presets.className="resource-controls";
const form=dom("div",picker);form.className="resource-controls";const searchLabel=dom("label",form,"Search workload"),search=dom("input",searchLabel);search.type="search";search.placeholder="Filter actual workload names";
const workloadLabel=dom("label",form,"Workload"),workload=dom("select",workloadLabel),containerLabel=dom("label",form,"Container"),container=dom("select",containerLabel);
const selectedHost=document.getElementById("resource-selected"),workloads=[...new Set(r.series.map(s=>s.workload))].sort();
function option(select,value,label=value){const o=dom("option",select,label);o.value=value;}
function renderSelected(){const all=workload.value==="__all";container.disabled=all;if(all){panel(selectedHost,"All HCP / complete-timestamp aggregate",[aggregate(false)],true);return;}const group=r.series.filter(s=>s.workload===workload.value&&(container.value==="__all"||s.container===container.value));if(group.length)panel(selectedHost,workload.value+" / "+(container.value==="__all"?"all containers":container.value),group);else selectedHost.replaceChildren();}
function containers(){container.replaceChildren();option(container,"__all","All containers");[...new Set(r.series.filter(s=>s.workload===workload.value).map(s=>s.container))].sort().forEach(c=>option(container,c));if([...container.options].some(o=>o.value===workload.value))container.value=workload.value;renderSelected();}
function filter(){const old=workload.value;workload.replaceChildren();workloads.filter(w=>w.toLowerCase().includes(search.value.toLowerCase())).forEach(w=>option(workload,w));if(!search.value)option(workload,"__all","All HCP (aggregate)");if([...workload.options].some(o=>o.value===old))workload.value=old;containers();}
[["KAS","kube-apiserver"],["Etcd","etcd"],["CPO","control-plane-operator"],["Catalogs","catalog"],["All HCP","__all"]].forEach(([label,value])=>{const b=dom("button",presets,label);b.type="button";b.onclick=()=>{search.value=value==="catalog"?"-operators-catalog":"";filter();if(value!=="catalog")workload.value=value;containers();};});
search.oninput=filter;workload.onchange=containers;container.onchange=renderSelected;filter();workload.value=workloads.includes("control-plane-operator")?"control-plane-operator":workloads[0];containers();
const byID=new Map(r.series.map(s=>[s.id,s]));document.querySelectorAll(".resource-raw").forEach(details=>details.addEventListener("toggle",()=>{if(details.open){const pre=details.querySelector("pre");if(!pre.textContent)pre.textContent=JSON.stringify(byID.get(details.dataset.seriesId).points,null,2);}}));
})();
</script>{{end}}`

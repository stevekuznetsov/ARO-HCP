#!/usr/bin/env python3
"""Fund KAS bootstrap deadlines within the existing offering envelope."""
from fractions import Fraction as F
import json
from pathlib import Path
import cost_allocate_v11 as v11
import cost_allocate_v4 as shared

CHANGES={('kube-apiserver','bootstrap'):(50,64),('kube-apiserver','kube-apiserver'):(79,468),('control-plane-operator','control-plane-operator'):(60,65)}

def derive(root):
    report,profile=v11.derive(root)
    model=report['model']
    rows=model['rows']
    index={(r['identity']['workload'],r['identity']['container']):r for r in rows}
    old_limits={key:row['memoryLimitMiB'] for key,row in index.items()}
    for key,(cpu,memory) in CHANGES.items():
        row=index[key]
        row['requests']={'cpuMilli':cpu,'memoryMiB':memory}
        row['memoryLimitMiB']=(memory*110+99)//100
    pods={p['metadata']['uid']:p for p in json.loads((root/'_artifacts/perf-resource-locked-scan/density-v6-apps60/before/full-pods.json').read_text())['items']}
    for group in model['podGroups']:
        spec=pods[group['uid']]['spec']
        cpu=lambda c:index[(group['workload'],c['name'])]['requests']['cpuMilli']
        group.update(shared.q.pod_quota([cpu(c) for c in spec['containers']],[(cpu(c),c.get('restartPolicy')=='Always') for c in spec.get('initContainers',[])],100000,1000,1000))
        if group['workload']=='kube-apiserver':
            group['chargedMemoryMiB']+=sum(index[('kube-apiserver',c['name'])]['memoryLimitMiB']-old_limits[('kube-apiserver',c['name'])] for c in spec['containers'])
    effective=470+sum(F(g['effectiveQuotaMilli']) for g in model['podGroups'])
    charged=2090+sum(g['chargedMemoryMiB'] for g in model['podGroups'])
    model.update(declaredControlledCPUMilli=sum(r['requests']['cpuMilli']*r['replicas'] for r in rows),effectiveOfferingCPUMilli=str(effective),effectiveControlledCPUMilli=str(effective-470),unusedEffectiveCPUMilli=str(F(1000000,403)-effective),offeringChargedMemoryMiB=charged,controlledChargedMemoryMiB=charged-2090,unusedChargedMemoryMiB=str(F(4096000,403)-charged))
    model['declaredOfferingCPUMilli']=model['declaredControlledCPUMilli']+470
    model['quotaAwareUSDPerHour']=str(F('.806')*max(effective/16000,F(charged,65536)))
    assert effective<=F(1000000,403) and charged<=F(4096000,403)
    for entry,row in zip(profile['containers'],rows):
        entry['floor']=row['requests']
        if row['identity']['workload']==row['identity']['container']=='kube-apiserver':
            profile['anchor']=row['requests']
    profile['name']='small-cost-v12'
    profile['description']='KAS bootstrap repeats 50-second apply timeout at10m quota/24Mi limit, removing all API endpoints during backoff. Bootstrap50m/64Mi funded KAS79m/468Mi and CPO60m. No readiness/liveness change. Not qualified.'
    report.update(version='v12',targetedCPUChanges=[dict(workload=k[0],container=k[1],cpuMilli=v[0]) for k,v in CHANGES.items()])
    for path in (Path(__file__),Path(__file__).with_name('test_cost_allocate_v12.py')):
        report['sourceHashes'][str(path.relative_to(root))]=shared.q.v1.digest(path.read_bytes())
    return report,profile

if __name__=='__main__':
    shared.main(version='v12',derive_fn=derive,parent_version='v11',resource='both')

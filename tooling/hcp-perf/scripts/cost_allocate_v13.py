#!/usr/bin/env python3
"""Reallocate measured bootstrap/etcd headroom to KAS CRD installation."""
from copy import deepcopy
from fractions import Fraction as F
import json
from pathlib import Path
import cost_allocate_v12 as v12
import cost_allocate_v4 as shared

CHANGES={('kube-apiserver','bootstrap'):(30,40),('kube-apiserver','kube-apiserver'):(99,560),('etcd','etcd'):(26,203)}

def derive(root):
    report,profile=v12.derive(root)
    model=report['model']
    rows=model['rows']
    previous=deepcopy(rows)
    index={(r['identity']['workload'],r['identity']['container']):r for r in rows}
    for key,(cpu,memory) in CHANGES.items():
        index[key]['requests']=dict(cpuMilli=cpu,memoryMiB=memory)
        index[key]['memoryLimitMiB']=(memory*110+99)//100
    pods={p['metadata']['uid']:p for p in json.loads((root/'_artifacts/perf-resource-locked-scan/density-v6-apps60/before/full-pods.json').read_text())['items']}
    for group in model['podGroups']:
        spec=pods[group['uid']]['spec']
        row=lambda c:index[(group['workload'],c['name'])]
        cpu=lambda c:row(c)['requests']['cpuMilli']
        group.update(shared.q.pod_quota([cpu(c) for c in spec['containers']],[(cpu(c),c.get('restartPolicy')=='Always') for c in spec.get('initContainers',[])],100000,1000,1000))
        group['chargedMemoryMiB']=shared.q.v1.scheduling_charge(spec,lambda c,kind:[0,0,row(c)['memoryLimitMiB']])[2]
    effective=470+sum(F(g['effectiveQuotaMilli']) for g in model['podGroups'])
    charged=2090+sum(g['chargedMemoryMiB'] for g in model['podGroups'])
    model.update(declaredControlledCPUMilli=sum(r['requests']['cpuMilli']*r['replicas'] for r in rows),effectiveOfferingCPUMilli=str(effective),effectiveControlledCPUMilli=str(effective-470),unusedEffectiveCPUMilli=str(F(1000000,403)-effective),offeringChargedMemoryMiB=charged,controlledChargedMemoryMiB=charged-2090,unusedChargedMemoryMiB=str(F(4096000,403)-charged))
    model['declaredOfferingCPUMilli']=model['declaredControlledCPUMilli']+470
    model['controlledRequestMemoryMiB']=sum(r['requests']['memoryMiB']*r['replicas'] for r in rows)
    model['offeringRequestMemoryMiB']=model['controlledRequestMemoryMiB']+2090
    model['steadyOfferingMemoryLimitChargeMiB']=2090+sum(r['memoryLimitMiB']*r['replicas'] for r in rows)
    model['initStageExtraMemoryMiB']=charged-model['steadyOfferingMemoryLimitChargeMiB']
    model['quotaAwareUSDPerHour']=str(F('.806')*max(effective/16000,F(charged,65536)))
    assert effective<=F(1000000,403) and charged<=F(4096000,403)
    for key in ('cpuScale','memoryScale'):
        model.pop(key,None)
    for entry,row in zip(profile['containers'],rows):
        entry['floor']=row['requests']
        if row['identity']['workload']==row['identity']['container']=='kube-apiserver':
            profile['anchor']=row['requests']
    profile.update(name='small-cost-v13',description='Fresh V12 KAS OOM515Mi during CRD apply with94percent CPUpressure. KAS99m/560Mi funded bootstrap30m/40Mi (observed peak32Mi, about1CPUsecond) and etcd203Mi (observed152Mi). Fixed envelope, no readiness/liveness change. Not qualified.')
    report.update(version='v13',memoryDeltas=[dict(identity=r['identity'],replicas=r['replicas'],oldRequestMiB=p['requests']['memoryMiB'],newRequestMiB=r['requests']['memoryMiB'],oldLimitMiB=p['memoryLimitMiB'],newLimitMiB=r['memoryLimitMiB']) for p,r in zip(previous,rows)],targetedCPUChanges=[dict(workload=k[0],container=k[1],cpuMilli=v[0]) for k,v in CHANGES.items()])
    report.pop('protectedMemory',None)
    for path in (Path(__file__),Path(__file__).with_name('test_cost_allocate_v13.py')):
        report['sourceHashes'][str(path.relative_to(root))]=shared.q.v1.digest(path.read_bytes())
    return report,profile

if __name__=='__main__':
    shared.main(version='v13',derive_fn=derive,parent_version='v12',resource='both')

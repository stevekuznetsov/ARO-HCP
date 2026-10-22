#!/usr/bin/env python3
"""V9 memory consolidation plus startup CPU allocations for OLM and token minters."""
from copy import deepcopy
from fractions import Fraction as F
import json
from pathlib import Path
import cost_allocate_v4 as shared
import cost_allocate_v9 as v9

def derive(root):
    report,profile=v9.derive(root)
    q=shared.q
    old=json.loads((root/'_artifacts/small-cost-v7/allocation.json').read_text())
    model=report['model']
    rows=model['rows']
    source=json.loads((root/'_artifacts/small-cost-v1/allocation.json').read_text())['selected']['small-cost-v1']['rows']
    pods={p['metadata']['uid']:p for p in json.loads((root/'_artifacts/perf-resource-locked-scan/density-v6-apps60/before/full-pods.json').read_text())['items']}
    keys=[(r['identity']['workload'],r['identity']['container']) for r in rows]
    index={k:i for i,k in enumerate(keys)}
    fixed={(r['workload'],r['container']):r['cpuMilli'] for r in old['protectedCPU']}
    fixed.update({('catalog-operator','catalog-operator'):20,('olm-operator','olm-operator'):20})
    for key in keys:
        if 'token-minter' in key[1]:
            fixed[key]=10
    def groups(values):
        result=[]
        for group in model['podGroups']:
            spec=pods[group['uid']]['spec']
            cpu=lambda c:values[index[(group['workload'],c['name'])]]
            result.append(q.pod_quota([cpu(c) for c in spec['containers']],[(cpu(c),c.get('restartPolicy')=='Always') for c in spec.get('initContainers',[])],100000,1000,1000))
        return result
    charge=lambda v:470+sum(F(g['effectiveQuotaMilli']) for g in groups(v))
    scale,values=shared.fixed_memory(source,fixed,F(1000000,403),charge,field='cpuMilli')
    for row,cpu in zip(rows,values):
        row['requests']['cpuMilli']=cpu
    for group,quota in zip(model['podGroups'],groups(values)):
        group.update(quota)
    model['declaredControlledCPUMilli']=sum(r['requests']['cpuMilli']*r['replicas'] for r in rows)
    model['declaredOfferingCPUMilli']=model['declaredControlledCPUMilli']+470
    model['effectiveOfferingCPUMilli']=str(charge(values))
    model['effectiveControlledCPUMilli']=str(charge(values)-470)
    model['cpuScale']=str(scale)
    model['unusedEffectiveCPUMilli']=str(F(1000000,403)-charge(values))
    model['fallback']['cpuMilli']=int(10*scale)
    model['quotaAwareUSDPerHour']=str(F('.806')*max(charge(values)/16000,F(model['offeringChargedMemoryMiB'],65536)))
    assert F(model['quotaAwareUSDPerHour'])<=F('.125')
    profile['name']='small-cost-v10'
    profile['description']='V9 consolidated memory; protect catalog/OLM main20m and tokenminter10m to avoid known startup probe CPU starvation. Strict0.125rawD16 perPodinit/replica cost. No readiness/liveness relaxation. Not qualified.'
    for entry,row in zip(profile['containers'],rows):
        entry['floor']=deepcopy(row['requests'])
        if row['identity']['workload']==row['identity']['container']=='kube-apiserver':
            profile['anchor']=deepcopy(row['requests'])
    report['version']='v10'
    report['protectedCPU']=[dict(workload=k[0],container=k[1],cpuMilli=v) for k,v in fixed.items()]
    report['hypothesis']+=' CPU allocations target event-confirmed OLM/catalog liveness and tokenminter startup kills, remaining CPU scaled within unchanged envelope.'
    for path in (Path(__file__),Path(__file__).with_name('test_cost_allocate_v10.py')):
        report['sourceHashes'][str(path.relative_to(root))]=q.v1.digest(path.read_bytes())
    return report,profile

if __name__=='__main__':
    shared.main(version='v10',derive_fn=derive,parent_version='v9',resource='both')

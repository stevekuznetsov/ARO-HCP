#!/usr/bin/env python3
"""Fund etcd health helper at20m using exactly offsetting CPO/KAS quota reductions."""
from fractions import Fraction as F
import json
from pathlib import Path
import cost_allocate_v10 as v10
import cost_allocate_v4 as shared

CHANGES={('etcd','healthz'):20,('control-plane-operator','control-plane-operator'):90,('kube-apiserver','kube-apiserver'):117}

def derive(root):
    report,profile=v10.derive(root)
    model=report['model']
    rows=model['rows']
    index={(r['identity']['workload'],r['identity']['container']):r for r in rows}
    for key,value in CHANGES.items():
        index[key]['requests']['cpuMilli']=value
    pods={p['metadata']['uid']:p for p in json.loads((root/'_artifacts/perf-resource-locked-scan/density-v6-apps60/before/full-pods.json').read_text())['items']}
    for group in model['podGroups']:
        spec=pods[group['uid']]['spec']
        cpu=lambda c:index[(group['workload'],c['name'])]['requests']['cpuMilli']
        group.update(shared.q.pod_quota([cpu(c) for c in spec['containers']],[(cpu(c),c.get('restartPolicy')=='Always') for c in spec.get('initContainers',[])],100000,1000,1000))
    effective=470+sum(F(g['effectiveQuotaMilli']) for g in model['podGroups'])
    model['declaredControlledCPUMilli']=sum(r['requests']['cpuMilli']*r['replicas'] for r in rows)
    model['declaredOfferingCPUMilli']=model['declaredControlledCPUMilli']+470
    model['effectiveOfferingCPUMilli']=str(effective)
    model['effectiveControlledCPUMilli']=str(effective-470)
    model['unusedEffectiveCPUMilli']=str(F(1000000,403)-effective)
    model['quotaAwareUSDPerHour']=str(F('.806')*max(effective/16000,F(model['offeringChargedMemoryMiB'],65536)))
    assert effective<=F(1000000,403) and F(model['quotaAwareUSDPerHour'])<=F('.125')
    for entry,row in zip(profile['containers'],rows):
        entry['floor']=row['requests']
        if row['identity']['workload']==row['identity']['container']=='kube-apiserver':
            profile['anchor']=row['requests']
    profile['name']='small-cost-v11'
    profile['description']='V10 memory preserved. Etcd server healthy but10m health helper client TLS retries starve: healthz20m offset CPO90m/KAS117m. Exact quota/parent budget0.125; no probe/image change; not qualified.'
    report['version']='v11'
    report['targetedCPUChanges']=[dict(workload=k[0],container=k[1],cpuMilli=v) for k,v in CHANGES.items()]
    for path in (Path(__file__),Path(__file__).with_name('test_cost_allocate_v11.py')):
        report['sourceHashes'][str(path.relative_to(root))]=shared.q.v1.digest(path.read_bytes())
    return report,profile

if __name__=='__main__':
    shared.main(version='v11',derive_fn=derive,parent_version='v10',resource='both')

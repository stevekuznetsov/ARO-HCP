#!/usr/bin/env python3
"""Consolidated startup memory allowances, exact inherited CPU and init accounting."""
from copy import deepcopy
from fractions import Fraction as F
import json
import cost_allocate_v4 as shared

FIXED = {
    ('cluster-storage-operator','cluster-storage-operator'):64,
    ('ingress-operator','ingress-operator'):96,
    ('cluster-network-operator','cluster-network-operator'):80,
    ('cluster-image-registry-operator','cluster-image-registry-operator'):64,
    ('catalog-operator','catalog-operator'):64,
    ('olm-operator','olm-operator'):64,
    ('control-plane-pki-operator','control-plane-pki-operator'):64,
    ('capi-provider','manager'):64,
    ('cluster-api','manager'):64,
    ('kube-scheduler','kube-scheduler'):64,
    ('openshift-route-controller-manager','openshift-route-controller-manager'):64,
    ('csi-snapshot-controller-operator','csi-snapshot-controller-operator'):64,
    ('openshift-controller-manager','openshift-controller-manager'):96,
    ('cluster-policy-controller','cluster-policy-controller'):80,
    ('hosted-cluster-config-operator','hosted-cluster-config-operator'):80,
    ('kube-controller-manager','kube-controller-manager'):144,
    ('openshift-apiserver','openshift-apiserver'):192,
    ('packageserver','packageserver'):176,
    ('kube-apiserver','kube-apiserver'):511,
    ('etcd','etcd'):271,
}

def derive(root):
    q=shared.q
    hashes={}
    def read(path,expected=None):
        data=(root/path).read_bytes()
        sha=q.v1.digest(data)
        if expected and sha!=expected:
            raise ValueError('historical hash mismatch: '+path)
        hashes[path]=sha
        return json.loads(data)
    old=read('_artifacts/small-cost-v8/allocation.json')
    for name,item in read('_artifacts/small-cost-v8/output-hashes.json').items():
        read('_artifacts/small-cost-v8/'+name,item['sha256'])
    profile=read(old['profile']['path'],old['profile']['sha256'])
    source=read('_artifacts/small-cost-v1/allocation.json')['selected']['small-cost-v1']['rows']
    budgets=read('_artifacts/small-constrained-v1/analysis/candidate-budgets.json')
    inputs=read('_artifacts/small-constrained-v1/analysis/input-hashes.json')
    path=budgets['snapshots'][-1]['path']+'/full-pods.json'
    pods={p['metadata']['uid']:p for p in read(path,inputs['files'][path]['sha256'])['items']}
    model=deepcopy(old['model'])
    rows=model['rows']
    keys=[(r['identity']['workload'],r['identity']['container']) for r in rows]
    index={k:i for i,k in enumerate(keys)}
    assert len(index)==len(rows)
    fixed={(r['workload'],r['container']):r['memoryMiB'] for r in old['protectedMemory']}
    fixed.update(FIXED)
    def groups(values):
        return [q.v1.scheduling_charge(pods[g['uid']]['spec'],lambda c,kind:[0,0,(values[index[(g['workload'],c['name'])]]*110+99)//100])[2] for g in model['podGroups']]
    charge=lambda v:2090+sum(groups(v))
    cap=F(4096000,403)
    scale,values=shared.fixed_memory(source,fixed,cap,charge)
    deltas=[]
    for row,mem in zip(rows,values):
        deltas.append(dict(identity=row['identity'],replicas=row['replicas'],oldRequestMiB=row['requests']['memoryMiB'],newRequestMiB=mem,oldLimitMiB=row['memoryLimitMiB'],newLimitMiB=(mem*110+99)//100))
        row['requests']['memoryMiB']=mem
        row['memoryLimitMiB']=(mem*110+99)//100
    for group,mem in zip(model['podGroups'],groups(values)):
        group['chargedMemoryMiB']=mem
    model.update(memoryScale=str(scale),controlledRequestMemoryMiB=sum(r['requests']['memoryMiB']*r['replicas'] for r in rows),controlledChargedMemoryMiB=charge(values)-2090,
        offeringChargedMemoryMiB=charge(values),unusedChargedMemoryMiB=str(cap-charge(values)))
    model['offeringRequestMemoryMiB']=model['controlledRequestMemoryMiB']+2090
    model['steadyOfferingMemoryLimitChargeMiB']=2090+sum(r['memoryLimitMiB']*r['replicas'] for r in rows)
    model['initStageExtraMemoryMiB']=charge(values)-model['steadyOfferingMemoryLimitChargeMiB']
    model['quotaAwareUSDPerHour']=str(F('.806')*max(F(model['effectiveOfferingCPUMilli'])/16000,F(charge(values),65536)))
    assert F(model['quotaAwareUSDPerHour'])<=F('.125')
    model['fallback']['memoryMiB']=int(64*scale)
    profile['name']='small-cost-v9'
    profile['description']='Consolidated startup memory hypotheses from V7 OOMs and historical operator footprints; CPU unchanged; full per-Pod init-stage quota/limit accounting within0.125 raw D16s_v6. Not qualified. Fallback '+str(model['fallback'])
    profile['containers']=[]
    for row in rows:
        identity=row['identity']
        kas=identity['workload']==identity['container']=='kube-apiserver'
        entry=dict(identity=identity,floor=row['requests'],cpuRatio=dict(numerator=int(kas),denominator=1),memoryRatio=dict(numerator=int(kas),denominator=1))
        if row['nonGoException']:
            entry['goMemoryLimitPercent']=0
        profile['containers'].append(entry)
        if kas:
            profile['anchor']=row['requests']
    from pathlib import Path
    for path in (Path(__file__),Path(__file__).with_name('test_cost_allocate_v9.py'),Path(shared.__file__),Path(q.__file__),Path(q.v1.__file__)):
        hashes[str(path.relative_to(root))]=q.v1.digest(path.read_bytes())
    return dict(version='v9',model=model,memoryDeltas=deltas,sourceHashes=hashes,protectedMemory=[dict(workload=k[0],container=k[1],memoryMiB=v) for k,v in fixed.items()],
        quotaAssumptions=old['quotaAssumptions'],coverage=old['coverage'],qualified=False,startupTested=False,limits='fixed downstream requests do not enforce actual usage; saved52Pod benchmark layout excludes new workloads/rollout surge',
        hypothesis='Main64Mi allowances exploit only correctly-accounted init maxima. Larger ingress/CNO/OCM and serving requests are fixed; remaining proportions rescaled, not claimed minimums.'),profile

if __name__=='__main__':
    shared.main(version='v9',derive_fn=derive,parent_version='v8',resource='both')

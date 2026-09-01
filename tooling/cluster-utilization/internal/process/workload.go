// Copyright 2025 Microsoft Corporation
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

package process

import (
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/Azure/ARO-HCP/tooling/cluster-utilization/internal/model"
)

// wlKey identifies a workload after normalization and pool attribution.
type wlKey struct {
	namespace string
	workload  string
	wtype     string
	pool      string
}

type ident struct {
	workload string
	wtype    string
}

// clusterSnap is one cluster's slice of a SnapshotRows.
type clusterSnap struct {
	usage    []model.UsageRow
	requests []model.ResourceRow
	owners   []model.OwnerRow
	podNodes []model.PodNodeRow
}

// resolver maps pods to workloads and pools for a cluster snapshot.
type resolver struct {
	podWorkload map[[2]string]ident
	podPool     map[[2]string]string
}

func newResolver(snaps ...clusterSnap) *resolver {
	r := &resolver{podWorkload: map[[2]string]ident{}, podPool: map[[2]string]string{}}
	for _, s := range snaps {
		for _, o := range s.owners {
			if o.Workload != "" {
				r.podWorkload[[2]string{o.Namespace, o.Pod}] = ident{workload: o.Workload, wtype: o.WorkloadType}
			}
		}
		for _, pn := range s.podNodes {
			r.podPool[[2]string{pn.Namespace, pn.Pod}] = poolCategory(poolNameFromNode(pn.Node))
		}
	}
	return r
}

func (r *resolver) workload(ns, pod string) ident {
	if id, ok := r.podWorkload[[2]string{ns, pod}]; ok && id.workload != "" {
		return id
	}
	return ident{workload: stripPodSuffix(pod), wtype: "pod"}
}

func (r *resolver) pool(ns, pod string) string {
	if c, ok := r.podPool[[2]string{ns, pod}]; ok && c != "" {
		return c
	}
	return "unknown"
}

// buildPeakWorkloads produces the concurrent composition at the peak moments:
// CPU fields come from the CPU-peak snapshot, memory fields from the memory-peak
// snapshot, summed across the pods alive at each moment.
func buildPeakWorkloads(cpuSnap, memSnap clusterSnap, norm *Normalizer) []model.WorkloadAgg {
	res := newResolver(cpuSnap, memSnap)
	type acc struct {
		agg     model.WorkloadAgg
		cpuPods map[string]struct{}
		memPods map[string]struct{}
	}
	accs := map[wlKey]*acc{}
	get := func(rawNS, pod string) *acc {
		id := res.workload(rawNS, pod)
		nsNorm, _ := norm.Apply(rawNS)
		wl := normalizeWorkloadName(id.workload, id.wtype)
		k := wlKey{nsNorm, wl, id.wtype, res.pool(rawNS, pod)}
		a := accs[k]
		if a == nil {
			a = &acc{agg: model.WorkloadAgg{Namespace: nsNorm, Workload: wl, WorkloadType: id.wtype, PoolCategory: k.pool},
				cpuPods: map[string]struct{}{}, memPods: map[string]struct{}{}}
			accs[k] = a
		}
		return a
	}
	for _, u := range cpuSnap.usage {
		a := get(u.Namespace, u.Pod)
		a.agg.CPUUsage += u.CPU
		a.cpuPods[u.Namespace+"/"+u.Pod] = struct{}{}
	}
	for _, rr := range cpuSnap.requests {
		get(rr.Namespace, rr.Pod).agg.CPURequest += rr.CPU
	}
	for _, u := range memSnap.usage {
		a := get(u.Namespace, u.Pod)
		a.agg.MemUsage += u.Mem
		a.memPods[u.Namespace+"/"+u.Pod] = struct{}{}
	}
	for _, rr := range memSnap.requests {
		get(rr.Namespace, rr.Pod).agg.MemRequest += rr.Mem
	}
	out := make([]model.WorkloadAgg, 0, len(accs))
	for _, a := range accs {
		a.agg.Replicas = maxInt(len(a.cpuPods), len(a.memPods))
		out = append(out, a.agg)
	}
	sortByMemUsage(out)
	return out
}

// buildWindowWorkloads aggregates every instance over the window: per-pod usage
// is reduced to the p95 across all instances, per-pod request is representative,
// and Replicas is the count of distinct instances seen.
func buildWindowWorkloads(win clusterSnap, norm *Normalizer, percentile float64) []model.WorkloadAgg {
	res := newResolver(win)
	type acc struct {
		ns, workload, wtype, pool string
		cpuUse, memUse            []float64
		cpuReq, memReq            []float64
		pods                      map[string]struct{}
	}
	accs := map[wlKey]*acc{}
	get := func(rawNS, pod string) *acc {
		id := res.workload(rawNS, pod)
		nsNorm, _ := norm.Apply(rawNS)
		wl, wt := collapseWindowName(id.workload, id.wtype, norm)
		k := wlKey{nsNorm, wl, wt, res.pool(rawNS, pod)}
		a := accs[k]
		if a == nil {
			a = &acc{ns: nsNorm, workload: wl, wtype: wt, pool: k.pool, pods: map[string]struct{}{}}
			accs[k] = a
		}
		a.pods[rawNS+"/"+pod] = struct{}{}
		return a
	}
	for _, u := range win.usage {
		a := get(u.Namespace, u.Pod)
		if u.CPU > 0 {
			a.cpuUse = append(a.cpuUse, u.CPU)
		}
		if u.Mem > 0 {
			a.memUse = append(a.memUse, u.Mem)
		}
	}
	for _, rr := range win.requests {
		a := get(rr.Namespace, rr.Pod)
		if rr.CPU > 0 {
			a.cpuReq = append(a.cpuReq, rr.CPU)
		}
		if rr.Mem > 0 {
			a.memReq = append(a.memReq, rr.Mem)
		}
	}
	out := make([]model.WorkloadAgg, 0, len(accs))
	for _, a := range accs {
		out = append(out, model.WorkloadAgg{
			Namespace: a.ns, Workload: a.workload, WorkloadType: a.wtype, PoolCategory: a.pool,
			Replicas:   len(a.pods),
			CPUUsage:   pctl(a.cpuUse, percentile),
			MemUsage:   pctl(a.memUse, percentile),
			CPURequest: maxOf(a.cpuReq),
			MemRequest: maxOf(a.memReq),
		})
	}
	sortByMemUsage(out)
	return out
}

// pctl returns the linear-interpolation percentile of vals (p in (0,1); p<=0 or
// >=1 returns the max).
func pctl(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	if p <= 0 || p >= 1 || len(s) == 1 {
		return s[len(s)-1]
	}
	rank := p * float64(len(s)-1)
	lo, hi := int(math.Floor(rank)), int(math.Ceil(rank))
	if lo == hi {
		return s[lo]
	}
	return s[lo] + (rank-float64(lo))*(s[hi]-s[lo])
}

func maxOf(vals []float64) float64 {
	m := 0.0
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// buildNodePools groups a cluster's nodes into AKS agent pools categorized
// system/infra/user.
func buildNodePools(nodes []model.NodeRow) []model.NodePool {
	type agg struct {
		count        int
		cpu, mem     float64
		instanceType string
	}
	m := map[string]*agg{}
	for _, n := range nodes {
		a := m[n.Pool]
		if a == nil {
			a = &agg{}
			m[n.Pool] = a
		}
		a.count++
		a.cpu += n.CPUAllocatable
		a.mem += n.MemAllocatable
		if a.instanceType == "" {
			a.instanceType = n.InstanceType
		}
	}
	out := make([]model.NodePool, 0, len(m))
	for name, a := range m {
		out = append(out, model.NodePool{Name: name, Category: poolCategory(name), InstanceType: a.instanceType,
			NodeCount: a.count, CPUAllocatable: a.cpu, MemAllocatable: a.mem})
	}
	sort.Slice(out, func(i, j int) bool {
		if catRank(out[i].Category) != catRank(out[j].Category) {
			return catRank(out[i].Category) < catRank(out[j].Category)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

var poolNameRe = regexp.MustCompile(`^aks-([a-z0-9]+)-`)

func poolNameFromNode(node string) string {
	if m := poolNameRe.FindStringSubmatch(node); m != nil {
		return m[1]
	}
	return "unknown"
}

func poolCategory(pool string) string {
	switch {
	case pool == model.PoolSystem:
		return model.PoolSystem
	case strings.HasPrefix(pool, model.PoolInfra):
		return model.PoolInfra
	default:
		return model.PoolUser
	}
}

func catRank(cat string) int {
	switch cat {
	case model.PoolSystem:
		return 0
	case model.PoolInfra:
		return 1
	default:
		return 2
	}
}

// normalizeWorkloadName collapses CronJob-run identifiers in Job workload names.
func normalizeWorkloadName(name, wtype string) string {
	if wtype != "job" {
		return name
	}
	parts := strings.Split(name, "-")
	for len(parts) > 1 {
		last := parts[len(parts)-1]
		if isAllDigits(last) || isHashLike(last) {
			parts = parts[:len(parts)-1]
			continue
		}
		break
	}
	return strings.Join(parts, "-")
}

// collapseWindowName aggressively collapses per-instance identifiers for the
// all-history window track, where owner resolution can legitimately fail on
// high-churn namespaces (thousands of ReplicaSet generations / Job runs exceed
// the datasource's per-query series cap). It:
//   - resolves ReplicaSet names to their Deployment by stripping the hash,
//   - collapses CronJob run suffixes,
//   - normalizes unowned pod names that look like per-HCP namespaces (velero
//     names backup pods after the HCP: "ocm-arohcpint-<id>" -> "ocm"), and
//   - strips generated pod suffixes (e.g. DaemonSet "node-agent-wfjhw").
func collapseWindowName(name, wtype string, norm *Normalizer) (string, string) {
	switch wtype {
	case "replicaset":
		return stripGeneratedSuffix(name), "deployment"
	case "job":
		return normalizeWorkloadName(name, "job"), "job"
	case "pod":
		if n, _ := norm.Apply(name); n != name {
			return n, "pod"
		}
		return stripGeneratedSuffix(name), "pod"
	default:
		return name, wtype
	}
}

// stripGeneratedSuffix removes trailing generated segments: unix timestamps,
// hashes, and 5-char generateName suffixes. Short numeric ordinals (e.g. the
// "1" in "shard-1") are preserved.
func stripGeneratedSuffix(name string) string {
	parts := strings.Split(name, "-")
	for len(parts) > 1 {
		last := parts[len(parts)-1]
		if (isAllDigits(last) && len(last) >= 6) || isHashLike(last) || isGenerateName(last) {
			parts = parts[:len(parts)-1]
			continue
		}
		break
	}
	return strings.Join(parts, "-")
}

// isGenerateName reports whether s is a Kubernetes 5-char generateName suffix
// (exactly 5 lowercase alphanumerics).
func isGenerateName(s string) bool {
	if len(s) != 5 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// stripPodSuffix recovers a stable name for unowned pods. Per-node pods embed the
// AKS node name (starts with "aks-"), so cut there first.
func stripPodSuffix(pod string) string {
	if i := strings.Index(pod, "-aks-"); i > 0 {
		return pod[:i]
	}
	parts := strings.Split(pod, "-")
	if len(parts) < 2 {
		return pod
	}
	last := parts[len(parts)-1]
	if isAllDigits(last) {
		return strings.Join(parts[:len(parts)-1], "-")
	}
	if isHashLike(last) && len(parts) >= 3 && isHashLike(parts[len(parts)-2]) {
		return strings.Join(parts[:len(parts)-2], "-")
	}
	if isHashLike(last) {
		return strings.Join(parts[:len(parts)-1], "-")
	}
	return pod
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isHashLike(s string) bool {
	if len(s) < 5 {
		return false
	}
	hasDigit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'z':
		default:
			return false
		}
	}
	return hasDigit
}

func sortByMemUsage(w []model.WorkloadAgg) {
	sort.Slice(w, func(i, j int) bool {
		if w[i].MemUsage != w[j].MemUsage {
			return w[i].MemUsage > w[j].MemUsage
		}
		if w[i].Namespace != w[j].Namespace {
			return w[i].Namespace < w[j].Namespace
		}
		return w[i].Workload < w[j].Workload
	})
}

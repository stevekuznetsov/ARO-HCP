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

// Package collect queries Grafana per region: it finds each cluster's busiest
// CPU/memory instant from low-cardinality node metrics, snapshots the concurrent
// workload composition at those instants via the PromQL @ modifier, and also
// pulls per-pod aggregates over the whole window for every instance that ran.
package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/Azure/ARO-HCP/tooling/cluster-utilization/internal/grafana"
	"github.com/Azure/ARO-HCP/tooling/cluster-utilization/internal/model"
)

// Grafana is the subset of the grafana client used here (for testability).
type Grafana interface {
	ListDatasources(ctx context.Context) ([]grafana.Datasource, error)
	InstantQuery(ctx context.Context, dsUID, query string) ([]grafana.Sample, error)
	RangeQuery(ctx context.Context, dsUID, query, from, to string, stepSeconds int) ([]grafana.Series, error)
}

// Options controls a collection run.
type Options struct {
	GrafanaURL        string
	CacheDir          string
	DatasourcePattern string
	Window            string
	Step              string
	Percentile        float64
	NamespaceBatch    int
	Concurrency       int
	Refresh           bool
	MaxCacheAge       time.Duration
	DatasourceTimeout time.Duration
}

var poolNameRe = regexp.MustCompile(`^aks-([a-z0-9]+)-`)

func poolFromNode(node string) string {
	if m := poolNameRe.FindStringSubmatch(node); m != nil {
		return m[1]
	}
	return "unknown"
}

func isOCM(ns string) bool { return strings.HasPrefix(ns, "ocm-") }

// region groups the services + hcps datasources for one region.
type region struct {
	name        string
	servicesUID string
	hcpsUID     string
}

// Run performs the collection and writes one cache file per region.
func Run(ctx context.Context, log logr.Logger, gc Grafana, opts Options) error {
	if opts.Window == "" {
		opts.Window = "14d"
	}
	if opts.Step == "" {
		opts.Step = "5m"
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	if opts.MaxCacheAge <= 0 {
		opts.MaxCacheAge = 24 * time.Hour
	}
	if opts.DatasourceTimeout <= 0 {
		opts.DatasourceTimeout = 45 * time.Minute
	}

	datasources, err := gc.ListDatasources(ctx)
	if err != nil {
		return fmt.Errorf("listing datasources: %w", err)
	}
	var dsFilter *regexp.Regexp
	if opts.DatasourcePattern != "" {
		if dsFilter, err = regexp.Compile(opts.DatasourcePattern); err != nil {
			return fmt.Errorf("invalid --datasource-pattern: %w", err)
		}
	}
	regions := groupRegions(datasources, dsFilter)
	if len(regions) == 0 {
		return fmt.Errorf("no regions with a services datasource found")
	}
	log.Info("selected regions", "count", len(regions))

	if err := os.MkdirAll(opts.CacheDir, 0o755); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}

	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures int
	for _, r := range regions {
		wg.Add(1)
		sem <- struct{}{}
		go func(r region) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := collectRegion(ctx, log, gc, opts, r); err != nil {
				log.Error(err, "region collection failed", "region", r.name)
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}(r)
	}
	wg.Wait()

	if failures > 0 {
		log.Info("collection finished with failures", "failed", failures, "regions", len(regions))
	} else {
		log.Info("collection finished", "regions", len(regions))
	}
	return nil
}

// groupRegions pairs services-<region> with hcps-<region>.
func groupRegions(datasources []grafana.Datasource, filter *regexp.Regexp) []region {
	svc := map[string]string{}
	hcp := map[string]string{}
	for _, ds := range datasources {
		if ds.Type != "prometheus" {
			continue
		}
		if filter != nil && !filter.MatchString(ds.UID) {
			continue
		}
		switch {
		case strings.HasPrefix(ds.UID, model.WorkspaceServices+"-"):
			svc[strings.TrimPrefix(ds.UID, model.WorkspaceServices+"-")] = ds.UID
		case strings.HasPrefix(ds.UID, model.WorkspaceHCPs+"-"):
			hcp[strings.TrimPrefix(ds.UID, model.WorkspaceHCPs+"-")] = ds.UID
		}
	}
	var out []region
	for name, uid := range svc {
		out = append(out, region{name: name, servicesUID: uid, hcpsUID: hcp[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func collectRegion(ctx context.Context, log logr.Logger, gc Grafana, opts Options, r region) error {
	path := cachePath(opts.CacheDir, instanceID(opts.GrafanaURL), r.name)

	// Resume from a partial checkpoint if present (unless --refresh). A complete
	// (WindowDone), fresh cache is skipped entirely.
	cache := model.RegionCache{
		SchemaVersion: model.CacheSchemaVersion,
		GrafanaURL:    opts.GrafanaURL,
		Region:        r.name,
		ServicesUID:   r.servicesUID,
		HCPsUID:       r.hcpsUID,
		Window:        opts.Window,
		Step:          opts.Step,
		Percentile:    opts.Percentile,
	}
	if !opts.Refresh {
		if existing, ok := loadPartial(path); ok && existing.SchemaVersion == model.CacheSchemaVersion {
			fresh := time.Since(existing.CollectedAt) < opts.MaxCacheAge
			if existing.WindowDone && fresh {
				log.Info("cache complete; skipping", "region", r.name)
				return nil
			}
			cache = existing
			log.Info("resuming region from checkpoint", "region", r.name,
				"snapClustersDone", len(cache.SnapClusters), "windowDone", cache.WindowDone)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, opts.DatasourceTimeout)
	defer cancel()
	started := time.Now()

	// Phase 1: peak search (cheap node signals).
	if len(cache.Peaks) == 0 {
		peaks, err := findPeaks(ctx, gc, r.servicesUID, opts.Window, opts.Step)
		if err != nil {
			if strings.Contains(err.Error(), "no such host") {
				log.V(1).Info("stale services datasource; skipping region", "region", r.name)
				return nil
			}
			return fmt.Errorf("peak search: %w", err)
		}
		if len(peaks) == 0 {
			log.V(1).Info("no clusters/peaks found; skipping region", "region", r.name)
			return nil
		}
		cache.Peaks = peaks
		cache.CollectedAt = time.Now().UTC()
		if err := writeCache(path, cache); err != nil {
			return err
		}
		log.Info("region peaks found", "region", r.name, "clusters", len(peaks))
	}

	// Phase 2: per-cluster peak snapshots (checkpoint after each cluster).
	done := map[string]struct{}{}
	for _, c := range cache.SnapClusters {
		done[c] = struct{}{}
	}
	for i, p := range cache.Peaks {
		if _, ok := done[p.Cluster]; ok {
			continue
		}
		t0 := time.Now()
		if p.CPUUnix > 0 {
			appendSnapshot(&cache.CPUPeak, snapshotAt(ctx, log, gc, opts, r, p.Cluster, p.CPUUnix, "cpu"))
		}
		if p.MemUnix > 0 {
			appendSnapshot(&cache.MemPeak, snapshotAt(ctx, log, gc, opts, r, p.Cluster, p.MemUnix, "memory"))
		}
		cache.SnapClusters = append(cache.SnapClusters, p.Cluster)
		cache.CollectedAt = time.Now().UTC()
		if err := writeCache(path, cache); err != nil {
			return err
		}
		log.Info("snapshot captured", "region", r.name, "cluster", p.Cluster,
			"progress", fmt.Sprintf("%d/%d", i+1, len(cache.Peaks)),
			"cpuPods", len(cache.CPUPeak.Usage), "memPods", len(cache.MemPeak.Usage), "took", time.Since(t0).Round(time.Second))
	}

	// Phase 3: nodes (cheap; always refreshed so a partial checkpoint is repaired).
	if nodes := collectNodes(ctx, gc, r.servicesUID); len(nodes) > 0 {
		cache.Nodes = nodes
	}

	// Phase 4: window (all-instance) track. Scoped to non-ocm namespaces: HCP
	// (ocm-*) composition is captured by the peak snapshot, and enumerating the
	// hundreds of per-HCP namespaces per-pod over the window is prohibitively slow
	// and low-value (HCP control planes are steady, so peak ~= window). This keeps
	// the tuning tables focused on platform and ephemeral non-HCP workloads.
	if !cache.WindowDone {
		var namespaces []string
		for _, ns := range namespacesOf(cache.CPUPeak.Usage, cache.MemPeak.Usage) {
			if !isOCM(ns) {
				namespaces = append(namespaces, ns)
			}
		}
		log.Info("collecting window track", "region", r.name, "namespaces", len(namespaces))
		cache.Window_ = collectWindow(ctx, log, gc, opts, r, namespaces)
		cache.WindowDone = true
		cache.CollectedAt = time.Now().UTC()
		if err := writeCache(path, cache); err != nil {
			return err
		}
	}

	log.Info("region complete", "region", r.name, "clusters", len(cache.Peaks),
		"cpuPeakUsage", len(cache.CPUPeak.Usage), "memPeakUsage", len(cache.MemPeak.Usage),
		"windowUsage", len(cache.Window_.Usage), "nodes", len(cache.Nodes), "took", time.Since(started).Round(time.Second))
	return nil
}

// loadPartial reads an existing region cache for resumption.
func loadPartial(path string) (model.RegionCache, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return model.RegionCache{}, false
	}
	var c model.RegionCache
	if err := json.Unmarshal(b, &c); err != nil {
		return model.RegionCache{}, false
	}
	return c, true
}

// findPeaks range-queries the low-cardinality node signals and returns each
// cluster's argmax CPU and memory timestamps.
func findPeaks(ctx context.Context, gc Grafana, servicesUID, window, step string) ([]model.PeakTime, error) {
	from := "now-" + window
	memSer, err := gc.RangeQuery(ctx, servicesUID, qPeakMem, from, "now", 1800)
	if err != nil {
		return nil, err
	}
	cpuSer, _ := gc.RangeQuery(ctx, servicesUID, qPeakCPU, from, "now", 1800)

	byCluster := map[string]*model.PeakTime{}
	get := func(cl string) *model.PeakTime {
		p := byCluster[cl]
		if p == nil {
			p = &model.PeakTime{Cluster: cl}
			byCluster[cl] = p
		}
		return p
	}
	for _, s := range memSer {
		cl := s.Labels["cluster"]
		if cl == "" {
			continue
		}
		t, _ := s.ArgMax()
		get(cl).MemUnix = int64(t)
	}
	for _, s := range cpuSer {
		cl := s.Labels["cluster"]
		if cl == "" {
			continue
		}
		t, _ := s.ArgMax()
		get(cl).CPUUnix = int64(t)
	}
	out := make([]model.PeakTime, 0, len(byCluster))
	for _, p := range byCluster {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })
	return out, nil
}

// snapshotAt captures one cluster's composition at instant ts for one resource,
// batched by namespace to stay under the timeseries-per-query cap.
func snapshotAt(ctx context.Context, log logr.Logger, gc Grafana, opts Options, r region, cluster string, ts int64, resource string) model.SnapshotRows {
	var s model.SnapshotRows
	nsAll := namespacesAtPeak(ctx, gc, r.servicesUID, cluster, ts)
	if len(nsAll) == 0 {
		return s
	}
	var platform, ocm []string
	for _, ns := range nsAll {
		if isOCM(ns) {
			ocm = append(ocm, ns)
		} else {
			platform = append(platform, ns)
		}
	}

	// usage: services AMW (every pod), batched over all namespaces
	usageBuild := func(reg string) string {
		if resource == "cpu" {
			return cpuUsageAt(cluster, reg, ts)
		}
		return memUsageAt(cluster, reg, ts)
	}
	batched(ctx, log, gc, r.servicesUID, nsAll, opts.NamespaceBatch, "snap-usage", usageBuild, func(rows []grafana.Sample) {
		for _, x := range rows {
			if u, ok := usageRow(x, resource); ok {
				s.Usage = append(s.Usage, u)
			}
		}
	})

	// requests: services (platform) + hcps (ocm)
	for _, sc := range dsScopes(r, platform, ocm) {
		batched(ctx, log, gc, sc.uid, sc.ns, opts.NamespaceBatch, "snap-req", func(reg string) string { return requestsAt(cluster, resource, reg, ts) },
			func(rows []grafana.Sample) { s.Requests = append(s.Requests, requestRows(rows, resource)...) })
	}

	// owners: services relabel (platform) + hcps fallback (ocm)
	batched(ctx, log, gc, r.servicesUID, platform, opts.NamespaceBatch, "snap-owners", func(reg string) string { return relabelOwnersAt(cluster, reg, ts) },
		func(rows []grafana.Sample) { s.Owners = append(s.Owners, relabelRows(rows)...) })
	if r.hcpsUID != "" && len(ocm) > 0 {
		var podOwners []grafana.Sample
		batched(ctx, log, gc, r.hcpsUID, ocm, opts.NamespaceBatch, "snap-podowner", func(reg string) string { return podOwnerAt(cluster, reg, ts) },
			func(rows []grafana.Sample) { podOwners = append(podOwners, rows...) })
		rsOwners, _ := gc.InstantQuery(ctx, r.hcpsUID, replicaSetOwnerAt(cluster, ts))
		s.Owners = append(s.Owners, joinOwners(podOwners, rsOwners)...)
	}

	// pod->node: services (platform) + hcps (ocm)
	for _, sc := range dsScopes(r, platform, ocm) {
		batched(ctx, log, gc, sc.uid, sc.ns, opts.NamespaceBatch, "snap-podinfo", func(reg string) string { return podInfoAt(cluster, reg, ts) },
			func(rows []grafana.Sample) { s.PodNodes = append(s.PodNodes, podNodeRows(rows)...) })
	}
	return s
}

// namespacesAtPeak lists the namespaces with pods at ts (aggregated query).
func namespacesAtPeak(ctx context.Context, gc Grafana, servicesUID, cluster string, ts int64) []string {
	rows, err := gc.InstantQuery(ctx, servicesUID, namespacesAt(cluster, ts))
	if err != nil {
		return nil
	}
	set := map[string]struct{}{}
	for _, s := range rows {
		if ns := s.Labels["namespace"]; ns != "" {
			set[ns] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for ns := range set {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

type dsScope struct {
	uid string
	ns  []string
}

func dsScopes(r region, platform, ocm []string) []dsScope {
	out := []dsScope{{r.servicesUID, platform}}
	if r.hcpsUID != "" {
		out = append(out, dsScope{r.hcpsUID, ocm})
	}
	return out
}

func requestRows(rows []grafana.Sample, resource string) []model.ResourceRow {
	var out []model.ResourceRow
	for _, s := range rows {
		ns, pod, c := s.Labels["namespace"], s.Labels["pod"], s.Labels["container"]
		if ns == "" || pod == "" || c == "" {
			continue
		}
		rr := model.ResourceRow{Cluster: s.Labels["cluster"], Namespace: ns, Pod: pod, Container: c}
		if resource == "cpu" {
			rr.CPU = s.Value
		} else {
			rr.Mem = s.Value
		}
		out = append(out, rr)
	}
	return out
}

func podNodeRows(rows []grafana.Sample) []model.PodNodeRow {
	var out []model.PodNodeRow
	for _, s := range rows {
		ns, pod, node := s.Labels["namespace"], s.Labels["pod"], s.Labels["node"]
		if ns == "" || pod == "" || node == "" {
			continue
		}
		out = append(out, model.PodNodeRow{Cluster: s.Labels["cluster"], Namespace: ns, Pod: pod, Node: node})
	}
	return out
}

func appendSnapshot(dst *model.SnapshotRows, src model.SnapshotRows) {
	dst.Usage = append(dst.Usage, src.Usage...)
	dst.Requests = append(dst.Requests, src.Requests...)
	dst.Owners = append(dst.Owners, src.Owners...)
	dst.PodNodes = append(dst.PodNodes, src.PodNodes...)
}

func usageRow(s grafana.Sample, resource string) (model.UsageRow, bool) {
	ns, pod, c := s.Labels["namespace"], s.Labels["pod"], s.Labels["container"]
	if ns == "" || pod == "" || c == "" {
		return model.UsageRow{}, false
	}
	u := model.UsageRow{Cluster: s.Labels["cluster"], Namespace: ns, Pod: pod, Container: c}
	if resource == "cpu" {
		u.CPU = s.Value
	} else {
		u.Mem = s.Value
	}
	return u, true
}

func relabelRows(rows []grafana.Sample) []model.OwnerRow {
	out := make([]model.OwnerRow, 0, len(rows))
	for _, s := range rows {
		ns, pod := s.Labels["namespace"], s.Labels["pod"]
		if ns == "" || pod == "" {
			continue
		}
		out = append(out, model.OwnerRow{Cluster: s.Labels["cluster"], Namespace: ns, Pod: pod,
			Workload: s.Labels["workload"], WorkloadType: s.Labels["workload_type"]})
	}
	return out
}

// joinOwners resolves ReplicaSet-owned pods to their Deployment. KSM is
// inconsistent about case (kube_replicaset_owner uses owner_kind="deployment",
// kube_pod_owner uses "ReplicaSet").
func joinOwners(podOwners, rsOwners []grafana.Sample) []model.OwnerRow {
	rsToDeploy := map[[3]string]string{}
	for _, s := range rsOwners {
		if strings.EqualFold(s.Labels["owner_kind"], "Deployment") {
			rsToDeploy[[3]string{s.Labels["cluster"], s.Labels["namespace"], s.Labels["replicaset"]}] = s.Labels["owner_name"]
		}
	}
	out := make([]model.OwnerRow, 0, len(podOwners))
	for _, s := range podOwners {
		cl, ns, pod := s.Labels["cluster"], s.Labels["namespace"], s.Labels["pod"]
		if ns == "" || pod == "" {
			continue
		}
		kind, name := s.Labels["owner_kind"], s.Labels["owner_name"]
		wtype, wname := kind, name
		if strings.EqualFold(kind, "ReplicaSet") {
			if dep, ok := rsToDeploy[[3]string{cl, ns, name}]; ok {
				wtype, wname = "Deployment", dep
			}
		}
		out = append(out, model.OwnerRow{Cluster: cl, Namespace: ns, Pod: pod, Workload: wname, WorkloadType: strings.ToLower(wtype)})
	}
	return out
}

func collectNodes(ctx context.Context, gc Grafana, uid string) []model.NodeRow {
	type nk struct{ cluster, node string }
	byNode := map[nk]*model.NodeRow{}
	get := func(cluster, node string) *model.NodeRow {
		k := nk{cluster, node}
		n := byNode[k]
		if n == nil {
			n = &model.NodeRow{Cluster: cluster, Node: node, Pool: poolFromNode(node)}
			byNode[k] = n
		}
		return n
	}
	assign := func(query string, set func(n *model.NodeRow, v float64)) {
		rows, err := gc.InstantQuery(ctx, uid, query)
		if err != nil {
			return
		}
		for _, s := range rows {
			if node := s.Labels["node"]; node != "" {
				set(get(s.Labels["cluster"], node), s.Value)
			}
		}
	}
	assign(qNodeCapCPU, func(n *model.NodeRow, v float64) { n.CPUCapacity = v })
	assign(qNodeCapMem, func(n *model.NodeRow, v float64) { n.MemCapacity = v })
	assign(qNodeCapPods, func(n *model.NodeRow, v float64) { n.PodsCapacity = v })
	assign(qNodeAllocCPU, func(n *model.NodeRow, v float64) { n.CPUAllocatable = v })
	assign(qNodeAllocMem, func(n *model.NodeRow, v float64) { n.MemAllocatable = v })

	out := make([]model.NodeRow, 0, len(byNode))
	for _, n := range byNode {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cluster != out[j].Cluster {
			return out[i].Cluster < out[j].Cluster
		}
		return out[i].Node < out[j].Node
	})
	return out
}

// collectWindow gathers per-pod usage (p95), requests (max over window) and
// owners over the whole window for every instance that ran, batched by namespace.
// A coarse subquery step keeps the 14d evaluations cheap; sustained p95 usage
// does not need fine resolution.
func collectWindow(ctx context.Context, log logr.Logger, gc Grafana, opts Options, r region, namespaces []string) model.SnapshotRows {
	var s model.SnapshotRows
	if len(namespaces) == 0 {
		return s
	}
	step := opts.Step
	if d, err := time.ParseDuration(step); err == nil && d < time.Hour {
		step = "1h" // coarsen fine steps for the expensive 14d subqueries
	}
	var platform, ocm []string
	for _, ns := range namespaces {
		if isOCM(ns) {
			ocm = append(ocm, ns)
		} else {
			platform = append(platform, ns)
		}
	}

	// usage: services AMW, all namespaces
	type ukey struct{ cluster, ns, pod, container string }
	usage := map[ukey]*model.UsageRow{}
	getU := func(cl, ns, pod, c string) *model.UsageRow {
		k := ukey{cl, ns, pod, c}
		u := usage[k]
		if u == nil {
			u = &model.UsageRow{Cluster: cl, Namespace: ns, Pod: pod, Container: c}
			usage[k] = u
		}
		return u
	}
	collectUsage := func(rows []grafana.Sample, set func(u *model.UsageRow, v float64)) {
		for _, x := range rows {
			ns, pod, c := x.Labels["namespace"], x.Labels["pod"], x.Labels["container"]
			if ns == "" || pod == "" || c == "" {
				continue
			}
			set(getU(x.Labels["cluster"], ns, pod, c), x.Value)
		}
	}
	batched(ctx, log, gc, r.servicesUID, namespaces, opts.NamespaceBatch, "window-mem", func(reg string) string { return memWindow(reg, opts.Window, step, opts.Percentile) },
		func(rows []grafana.Sample) { collectUsage(rows, func(u *model.UsageRow, v float64) { u.Mem = v }) })
	batched(ctx, log, gc, r.servicesUID, namespaces, opts.NamespaceBatch, "window-cpu", func(reg string) string { return cpuWindow(reg, opts.Window, step, opts.Percentile) },
		func(rows []grafana.Sample) { collectUsage(rows, func(u *model.UsageRow, v float64) { u.CPU = v }) })
	for _, u := range usage {
		s.Usage = append(s.Usage, *u)
	}

	// requests: services (platform) + hcps (ocm)
	type rkey struct{ cluster, ns, pod, container string }
	reqs := map[rkey]*model.ResourceRow{}
	getR := func(cl, ns, pod, c string) *model.ResourceRow {
		k := rkey{cl, ns, pod, c}
		rr := reqs[k]
		if rr == nil {
			rr = &model.ResourceRow{Cluster: cl, Namespace: ns, Pod: pod, Container: c}
			reqs[k] = rr
		}
		return rr
	}
	collectReq := func(rows []grafana.Sample, set func(rr *model.ResourceRow, v float64)) {
		for _, x := range rows {
			ns, pod, c := x.Labels["namespace"], x.Labels["pod"], x.Labels["container"]
			if ns == "" || pod == "" || c == "" {
				continue
			}
			set(getR(x.Labels["cluster"], ns, pod, c), x.Value)
		}
	}
	reqScopes := []struct {
		uid string
		ns  []string
	}{{r.servicesUID, platform}}
	if r.hcpsUID != "" {
		reqScopes = append(reqScopes, struct {
			uid string
			ns  []string
		}{r.hcpsUID, ocm})
	}
	for _, sc := range reqScopes {
		batched(ctx, log, gc, sc.uid, sc.ns, opts.NamespaceBatch, "window-reqcpu", func(reg string) string { return requestsWindow(reg, "cpu", opts.Window, step) },
			func(rows []grafana.Sample) { collectReq(rows, func(rr *model.ResourceRow, v float64) { rr.CPU = v }) })
		batched(ctx, log, gc, sc.uid, sc.ns, opts.NamespaceBatch, "window-reqmem", func(reg string) string { return requestsWindow(reg, "memory", opts.Window, step) },
			func(rows []grafana.Sample) { collectReq(rows, func(rr *model.ResourceRow, v float64) { rr.Mem = v }) })
	}
	for _, rr := range reqs {
		s.Requests = append(s.Requests, *rr)
	}

	// No window owner queries: mapping pods to workloads over 14d via the relabel
	// rule exceeds the series cap on churny namespaces and is unreliable. Window
	// pod names collapse cleanly via name heuristics (stripGeneratedSuffix), and
	// the peak snapshot carries authoritative owners.

	// pod->node over window for pool attribution of ephemeral pods
	pnScopes := []struct {
		uid string
		ns  []string
	}{{r.servicesUID, platform}}
	if r.hcpsUID != "" {
		pnScopes = append(pnScopes, struct {
			uid string
			ns  []string
		}{r.hcpsUID, ocm})
	}
	for _, sc := range pnScopes {
		batched(ctx, log, gc, sc.uid, sc.ns, opts.NamespaceBatch, "window-podinfo", func(reg string) string { return podInfoWindow(reg, opts.Window, step) },
			func(rows []grafana.Sample) {
				for _, x := range rows {
					ns, pod, node := x.Labels["namespace"], x.Labels["pod"], x.Labels["node"]
					if ns == "" || pod == "" || node == "" {
						continue
					}
					s.PodNodes = append(s.PodNodes, model.PodNodeRow{Cluster: x.Labels["cluster"], Namespace: ns, Pod: pod, Node: node})
				}
			})
	}

	sortUsage(s.Usage)
	return s
}

// queryConcurrency bounds how many namespace-batch queries a single batched()
// call runs in parallel.
const queryConcurrency = 2

// batched runs build(nsRegex) over namespace chunks in parallel, recursively
// halving a chunk whose query fails (e.g. a timeout, cost, or series-count
// limit). A single namespace that still fails is skipped.
func batched(ctx context.Context, log logr.Logger, gc Grafana, uid string, namespaces []string, size int, label string, build func(nsRegex string) string, sink func(rows []grafana.Sample)) {
	if len(namespaces) == 0 {
		return
	}
	var mu sync.Mutex
	safeSink := func(rows []grafana.Sample) {
		mu.Lock()
		sink(rows)
		mu.Unlock()
	}
	var fetch func(ns []string)
	fetch = func(ns []string) {
		if len(ns) == 0 || ctx.Err() != nil {
			return
		}
		rows, err := gc.InstantQuery(ctx, uid, build(anchoredNSRegex(ns)))
		if err == nil {
			safeSink(rows)
			return
		}
		if strings.Contains(err.Error(), "no such host") || ctx.Err() != nil {
			return
		}
		if len(ns) == 1 {
			log.V(1).Info("query failed for namespace; skipping", "datasource", uid, "label", label, "namespace", ns[0], "err", err.Error())
			return
		}
		mid := len(ns) / 2
		fetch(ns[:mid])
		fetch(ns[mid:])
	}

	chunks := chunkNamespaces(namespaces, size)
	sem := make(chan struct{}, queryConcurrency)
	var wg sync.WaitGroup
	for _, chunk := range chunks {
		wg.Add(1)
		sem <- struct{}{}
		go func(c []string) {
			defer wg.Done()
			defer func() { <-sem }()
			fetch(c)
		}(chunk)
	}
	wg.Wait()
}

func namespacesOf(rowsets ...[]model.UsageRow) []string {
	set := map[string]struct{}{}
	for _, rows := range rowsets {
		for _, u := range rows {
			if u.Namespace != "" {
				set[u.Namespace] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for ns := range set {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

func sortUsage(u []model.UsageRow) {
	sort.Slice(u, func(i, j int) bool {
		if u[i].Cluster != u[j].Cluster {
			return u[i].Cluster < u[j].Cluster
		}
		if u[i].Namespace != u[j].Namespace {
			return u[i].Namespace < u[j].Namespace
		}
		if u[i].Pod != u[j].Pod {
			return u[i].Pod < u[j].Pod
		}
		return u[i].Container < u[j].Container
	})
}

func cachePath(dir, instance, region string) string {
	safe := func(s string) string {
		s = strings.NewReplacer("/", "_", " ", "_", ":", "_").Replace(s)
		if s == "" {
			return "unknown"
		}
		return s
	}
	return filepath.Join(dir, safe(instance), safe(region)+".json")
}

// instanceID derives a per-Grafana-instance directory name from the URL.
func instanceID(grafanaURL string) string {
	u, err := url.Parse(grafanaURL)
	host := grafanaURL
	if err == nil && u.Host != "" {
		host = u.Host
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	if host == "" {
		return "default"
	}
	return host
}

func writeCache(path string, cache model.RegionCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cache); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

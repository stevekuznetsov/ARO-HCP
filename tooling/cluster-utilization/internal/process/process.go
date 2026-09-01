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

// Package process reads the raw per-region caches, splits them into per-cluster
// units, resolves pod->workload, normalizes namespaces, and produces the report.
package process

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/go-logr/logr"

	"github.com/Azure/ARO-HCP/tooling/cluster-utilization/internal/model"
)

// Options controls a processing run.
type Options struct {
	CacheDir       string
	Output         string
	NormalizeRules string
}

// Run reads caches, analyzes them, and writes the report JSON.
func Run(log logr.Logger, opts Options) error {
	var fileRules []Rule
	if opts.NormalizeRules != "" {
		r, err := LoadRules(opts.NormalizeRules)
		if err != nil {
			return fmt.Errorf("loading normalize rules: %w", err)
		}
		fileRules = r
	}
	norm, err := NewNormalizer(fileRules)
	if err != nil {
		return err
	}

	caches, err := loadCaches(opts.CacheDir)
	if err != nil {
		return err
	}
	if len(caches) == 0 {
		return fmt.Errorf("no cache files found in %s (run `collect` first)", opts.CacheDir)
	}
	log.Info("loaded region caches", "count", len(caches))

	report := buildReport(caches, norm)

	f, err := os.Create(opts.Output)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return err
	}
	log.Info("wrote report", "file", opts.Output, "units", len(report.Units))
	return nil
}

func loadCaches(dir string) ([]model.RegionCache, error) {
	var out []model.RegionCache
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(d.Name()) != ".json" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var c model.RegionCache
		if err := json.Unmarshal(b, &c); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		if c.SchemaVersion != model.CacheSchemaVersion {
			return nil
		}
		out = append(out, c)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading cache dir %s: %w", dir, err)
	}
	return out, nil
}

func buildReport(caches []model.RegionCache, norm *Normalizer) model.Report {
	report := model.Report{
		SchemaVersion:     model.ReportSchemaVersion,
		GeneratedAt:       time.Now().UTC(),
		NamespaceMatchers: norm.Matchers(),
	}
	for _, c := range caches {
		if report.Window == "" {
			report.Window = c.Window
			report.Percentile = c.Percentile
			report.GrafanaURL = c.GrafanaURL
		}
		for _, cluster := range clustersIn(c) {
			unit := buildUnit(c, cluster, norm)
			if unit.Totals.NodeCount == 0 && len(unit.PeakWorkloads) == 0 && len(unit.Workloads) == 0 {
				continue
			}
			report.Units = append(report.Units, unit)
		}
	}
	sort.Slice(report.Units, func(i, j int) bool {
		a, b := report.Units[i], report.Units[j]
		if a.Env != b.Env {
			return a.Env < b.Env
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		return a.Cluster < b.Cluster
	})
	return report
}

func clustersIn(c model.RegionCache) []string {
	set := map[string]struct{}{}
	for _, p := range c.Peaks {
		set[p.Cluster] = struct{}{}
	}
	for _, n := range c.Nodes {
		set[n.Cluster] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for cl := range set {
		if cl != "" {
			out = append(out, cl)
		}
	}
	sort.Strings(out)
	return out
}

func buildUnit(c model.RegionCache, cluster string, norm *Normalizer) model.Unit {
	env, region, role := parseCluster(cluster)
	if region == "" {
		region = c.Region
	}
	cpuSnap := filterSnap(c.CPUPeak, cluster)
	memSnap := filterSnap(c.MemPeak, cluster)
	winSnap := filterSnap(c.Window_, cluster)

	unit := model.Unit{
		Env: env, Region: region, Cluster: cluster, Role: role, GrafanaURL: c.GrafanaURL,
		NodePools:     buildNodePools(nodesOf(c.Nodes, cluster)),
		PeakWorkloads: buildPeakWorkloads(cpuSnap, memSnap, norm),
		Workloads:     buildWindowWorkloads(winSnap, norm, c.Percentile),
	}
	for _, p := range c.Peaks {
		if p.Cluster == cluster {
			unit.PeakCPU, unit.PeakMem = p.CPUUnix, p.MemUnix
		}
	}
	for _, p := range unit.NodePools {
		unit.Totals.CPUAllocatable += p.CPUAllocatable
		unit.Totals.MemAllocatable += p.MemAllocatable
		unit.Totals.NodeCount += p.NodeCount
	}
	for _, w := range unit.PeakWorkloads {
		unit.Totals.CPURequests += w.CPURequest
		unit.Totals.MemRequests += w.MemRequest
		unit.Totals.CPUUsage += w.CPUUsage
		unit.Totals.MemUsage += w.MemUsage
		unit.Totals.PodCount += w.Replicas
	}
	unit.Totals.WorkloadCount = len(unit.PeakWorkloads)
	return unit
}

func filterSnap(s model.SnapshotRows, cluster string) clusterSnap {
	var cs clusterSnap
	for _, u := range s.Usage {
		if u.Cluster == cluster {
			cs.usage = append(cs.usage, u)
		}
	}
	for _, r := range s.Requests {
		if r.Cluster == cluster {
			cs.requests = append(cs.requests, r)
		}
	}
	for _, o := range s.Owners {
		if o.Cluster == cluster {
			cs.owners = append(cs.owners, o)
		}
	}
	for _, pn := range s.PodNodes {
		if pn.Cluster == cluster {
			cs.podNodes = append(cs.podNodes, pn)
		}
	}
	return cs
}

func nodesOf(nodes []model.NodeRow, cluster string) []model.NodeRow {
	var out []model.NodeRow
	for _, n := range nodes {
		if n.Cluster == cluster {
			out = append(out, n)
		}
	}
	return out
}

// clusterNameRe parses "<env>-<region>-<role>-<ordinal>".
var clusterNameRe = regexp.MustCompile(`^([a-z0-9]+)-([a-z0-9]+)-(mgmt|svc)-(\d+)$`)

func parseCluster(name string) (env, region, role string) {
	if m := clusterNameRe.FindStringSubmatch(name); m != nil {
		return m[1], m[2], m[3]
	}
	return "unknown", "", "other"
}

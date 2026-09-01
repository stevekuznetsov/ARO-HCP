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

package collect

import (
	"fmt"
	"sort"
	"strings"
)

// --- low-cardinality node signals for per-cluster peak finding (range query) ---
const (
	// Cluster memory pressure: total minus available, per cluster (few series).
	qPeakMem = `sum by (cluster) (node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes)`
	// Cluster CPU busyness: non-idle core-seconds rate, per cluster.
	qPeakCPU = `sum by (cluster) (rate(node_cpu_seconds_total{mode!="idle"}[5m]))`
)

// --- current-value node/pool queries ---
const (
	qNodeCapCPU   = `kube_node_status_capacity{resource="cpu"}`
	qNodeCapMem   = `kube_node_status_capacity{resource="memory"}`
	qNodeCapPods  = `kube_node_status_capacity{resource="pods"}`
	qNodeAllocCPU = `kube_node_status_allocatable{resource="cpu"}`
	qNodeAllocMem = `kube_node_status_allocatable{resource="memory"}`
)

// --- @-pinned snapshot queries (instant, evaluated at unix time ts), scoped to
// a namespace regex so a single query stays under the timeseries-per-query cap ---

// namespacesAt counts pods per namespace at ts (aggregated -> few series).
func namespacesAt(cluster string, ts int64) string {
	return fmt.Sprintf(`count by (namespace) (container_memory_working_set_bytes{container!="", cluster=%q} @ %d)`, cluster, ts)
}
func memUsageAt(cluster, nsRegex string, ts int64) string {
	return fmt.Sprintf(`max by (cluster, namespace, pod, container) (container_memory_working_set_bytes{container!="", cluster=%q, namespace=~"%s"} @ %d)`, cluster, nsRegex, ts)
}
func cpuUsageAt(cluster, nsRegex string, ts int64) string {
	return fmt.Sprintf(`max by (cluster, namespace, pod, container) (rate(container_cpu_usage_seconds_total{container!="", cluster=%q, namespace=~"%s"}[5m] @ %d))`, cluster, nsRegex, ts)
}
func requestsAt(cluster, resource, nsRegex string, ts int64) string {
	return fmt.Sprintf(`kube_pod_container_resource_requests{resource=%q, cluster=%q, namespace=~"%s"} @ %d`, resource, cluster, nsRegex, ts)
}
func relabelOwnersAt(cluster, nsRegex string, ts int64) string {
	return fmt.Sprintf(`namespace_workload_pod:kube_pod_owner:relabel{cluster=%q, namespace=~"%s"} @ %d`, cluster, nsRegex, ts)
}
func podOwnerAt(cluster, nsRegex string, ts int64) string {
	return fmt.Sprintf(`kube_pod_owner{cluster=%q, namespace=~"%s"} @ %d`, cluster, nsRegex, ts)
}
func replicaSetOwnerAt(cluster string, ts int64) string {
	return fmt.Sprintf(`kube_replicaset_owner{cluster=%q} @ %d`, cluster, ts)
}
func podInfoAt(cluster, nsRegex string, ts int64) string {
	return fmt.Sprintf(`kube_pod_info{cluster=%q, namespace=~"%s"} @ %d`, cluster, nsRegex, ts)
}

// --- window (all-instance) queries, per pod over the whole window ---

func memWindow(nsRegex, window, step string, percentile float64) string {
	inner := fmt.Sprintf(`max by (cluster, namespace, pod, container) (container_memory_working_set_bytes{container!="", namespace=~"%s"})`, nsRegex)
	return overTime(inner, window, step, percentile)
}
func cpuWindow(nsRegex, window, step string, percentile float64) string {
	inner := fmt.Sprintf(`max by (cluster, namespace, pod, container) (rate(container_cpu_usage_seconds_total{container!="", namespace=~"%s"}[5m]))`, nsRegex)
	return overTime(inner, window, step, percentile)
}
func requestsWindow(nsRegex, resource, window, step string) string {
	return fmt.Sprintf(`max by (cluster, namespace, pod, container) (max_over_time(kube_pod_container_resource_requests{resource=%q, namespace=~"%s"}[%s:%s]))`, resource, nsRegex, window, step)
}
func relabelOwnersWindow(nsRegex, window, step string) string {
	return fmt.Sprintf(`max by (cluster, namespace, pod, workload, workload_type) (last_over_time(namespace_workload_pod:kube_pod_owner:relabel{namespace=~"%s"}[%s:%s]))`, nsRegex, window, step)
}
func podOwnerWindow(nsRegex, window, step string) string {
	return fmt.Sprintf(`max by (cluster, namespace, pod, owner_kind, owner_name) (last_over_time(kube_pod_owner{namespace=~"%s"}[%s:%s]))`, nsRegex, window, step)
}
func replicaSetOwnerWindow(window, step string) string {
	return fmt.Sprintf(`max by (cluster, namespace, replicaset, owner_kind, owner_name) (last_over_time(kube_replicaset_owner[%s:%s]))`, window, step)
}
func podInfoWindow(nsRegex, window, step string) string {
	return fmt.Sprintf(`max by (cluster, namespace, pod, node) (last_over_time(kube_pod_info{namespace=~"%s"}[%s:%s]))`, nsRegex, window, step)
}

// overTime wraps a per-pod instant selector in the over-time reduction.
func overTime(inner, window, step string, percentile float64) string {
	if percentile > 0 && percentile < 1 {
		return fmt.Sprintf("quantile_over_time(%g, (%s)[%s:%s])", percentile, inner, window, step)
	}
	return fmt.Sprintf("max_over_time((%s)[%s:%s])", inner, window, step)
}

// anchoredNSRegex builds an anchored alternation for a set of namespaces.
func anchoredNSRegex(namespaces []string) string {
	escaped := make([]string, len(namespaces))
	for i, ns := range namespaces {
		escaped[i] = regexpEscape(ns)
	}
	return "^(" + strings.Join(escaped, "|") + ")$"
}

func regexpEscape(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`, `.`, `\.`, `+`, `\+`, `*`, `\*`, `?`, `\?`,
		`(`, `\(`, `)`, `\)`, `[`, `\[`, `]`, `\]`, `{`, `\{`, `}`, `\}`,
		`|`, `\|`, `^`, `\^`, `$`, `\$`,
	)
	return r.Replace(s)
}

// chunkNamespaces splits a namespace set into de-duplicated, sorted slices of at
// most size entries. size <= 0 means a single chunk.
func chunkNamespaces(namespaces []string, size int) [][]string {
	uniq := map[string]struct{}{}
	for _, ns := range namespaces {
		if ns != "" {
			uniq[ns] = struct{}{}
		}
	}
	all := make([]string, 0, len(uniq))
	for ns := range uniq {
		all = append(all, ns)
	}
	sort.Strings(all)
	if len(all) == 0 {
		return nil
	}
	if size <= 0 || size >= len(all) {
		return [][]string{all}
	}
	var out [][]string
	for i := 0; i < len(all); i += size {
		end := i + size
		if end > len(all) {
			end = len(all)
		}
		out = append(out, all[i:end])
	}
	return out
}

// batchNamespaces renders chunkNamespaces as anchored regexes (used in tests).
func batchNamespaces(namespaces []string, size int) []string {
	chunks := chunkNamespaces(namespaces, size)
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, anchoredNSRegex(c))
	}
	return out
}

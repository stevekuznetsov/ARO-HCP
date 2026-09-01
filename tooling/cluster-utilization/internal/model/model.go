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

// Package model defines the on-disk cache schema (output of `collect`) and the
// normalized report schema (output of `process`, input to `render`).
package model

import "time"

// CacheSchemaVersion is bumped when the on-disk cache format changes
// incompatibly, so stale caches are ignored rather than misread.
const CacheSchemaVersion = 4

// Workspace identifiers (datasource UID prefixes).
const (
	WorkspaceServices = "services" // platform / service / management-cluster workloads + all cadvisor
	WorkspaceHCPs     = "hcps"     // hosted control planes (ocm-* namespaces) kube-state-metrics
)

// Node-pool categories, derived from the AKS agent-pool name.
const (
	PoolSystem = "system"
	PoolInfra  = "infra"
	PoolUser   = "user"
)

// RegionCache is the raw data captured by `collect` for one region, pairing the
// services datasource (all cadvisor usage + platform kube-state-metrics + nodes)
// with the hcps datasource (ocm-* kube-state-metrics). A single region contains
// multiple Kubernetes clusters, distinguished by the `cluster` label on each row.
//
// It holds two complementary datasets:
//   - CPUPeak/MemPeak: a coherent instant snapshot of every cluster at its
//     busiest CPU (resp. memory) moment in the window, via the PromQL @ modifier.
//     Drives the treemap, utilization cards and node-pool "used".
//   - Window: per-pod aggregates over the whole window for EVERY instance that
//     ran (usage = p95 over each pod's lifetime, requests = max over the window),
//     even if instances were never concurrent. Drives the top-N tuning tables.
type RegionCache struct {
	SchemaVersion int       `json:"schemaVersion"`
	CollectedAt   time.Time `json:"collectedAt"`
	GrafanaURL    string    `json:"grafanaURL"`
	Region        string    `json:"region"`
	ServicesUID   string    `json:"servicesUID"`
	HCPsUID       string    `json:"hcpsUID,omitempty"`
	Window        string    `json:"window"`
	Step          string    `json:"step"`
	Percentile    float64   `json:"percentile"`

	Peaks   []PeakTime   `json:"peaks"`   // per-cluster t*_cpu / t*_mem
	CPUPeak SnapshotRows `json:"cpuPeak"` // composition at each cluster's CPU peak
	MemPeak SnapshotRows `json:"memPeak"` // composition at each cluster's memory peak
	Nodes   []NodeRow    `json:"nodes"`
	Window_ SnapshotRows `json:"windowRows"` // per-pod over the whole window (all instances)

	// Checkpoint progress, so an interrupted collection resumes instead of
	// re-issuing every query. A region is complete when WindowDone is true.
	SnapClusters []string `json:"snapClusters,omitempty"` // clusters whose peak snapshots are captured
	WindowDone   bool     `json:"windowDone,omitempty"`
}

// PeakTime records a cluster's busiest CPU and memory instants (unix seconds).
type PeakTime struct {
	Cluster string `json:"cluster"`
	CPUUnix int64  `json:"cpuUnix"`
	MemUnix int64  `json:"memUnix"`
}

// SnapshotRows bundles the per-container rows for one snapshot (a peak moment or
// the window aggregate). For the peak snapshots only the relevant usage field is
// populated (CPUPeak fills CPU, MemPeak fills Mem); the window fills both.
type SnapshotRows struct {
	Usage    []UsageRow    `json:"usage,omitempty"`
	Requests []ResourceRow `json:"requests,omitempty"`
	Owners   []OwnerRow    `json:"owners,omitempty"`
	PodNodes []PodNodeRow  `json:"podNodes,omitempty"`
}

// NodeRow captures a single node's capacity/allocatable, pool and instance type.
type NodeRow struct {
	Cluster        string  `json:"cluster"`
	Node           string  `json:"node"`
	Pool           string  `json:"pool"`
	InstanceType   string  `json:"instanceType,omitempty"`
	CPUCapacity    float64 `json:"cpuCapacity"`
	MemCapacity    float64 `json:"memCapacity"`
	CPUAllocatable float64 `json:"cpuAllocatable"`
	MemAllocatable float64 `json:"memAllocatable"`
	PodsCapacity   float64 `json:"podsCapacity"`
}

// ResourceRow is a per-container CPU/memory request or limit.
type ResourceRow struct {
	Cluster   string  `json:"cluster"`
	Namespace string  `json:"namespace"`
	Pod       string  `json:"pod"`
	Container string  `json:"container"`
	CPU       float64 `json:"cpu"` // cores
	Mem       float64 `json:"mem"` // bytes
}

// UsageRow is a per-container usage value (snapshot value at a peak, or the
// per-pod percentile over the window).
type UsageRow struct {
	Cluster   string  `json:"cluster"`
	Namespace string  `json:"namespace"`
	Pod       string  `json:"pod"`
	Container string  `json:"container"`
	CPU       float64 `json:"cpu"` // cores
	Mem       float64 `json:"mem"` // bytes
}

// OwnerRow maps a pod to its stable workload identifier.
type OwnerRow struct {
	Cluster      string `json:"cluster"`
	Namespace    string `json:"namespace"`
	Pod          string `json:"pod"`
	Workload     string `json:"workload"`
	WorkloadType string `json:"workloadType"`
}

// PodNodeRow maps a pod to its node (for node-pool attribution).
type PodNodeRow struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Node      string `json:"node"`
}

// -------- report schema (process -> render) --------

// ReportSchemaVersion is bumped when the report format changes incompatibly.
const ReportSchemaVersion = 4

// Report is the normalized, analysis-ready output of `process`.
type Report struct {
	SchemaVersion     int               `json:"schemaVersion"`
	GeneratedAt       time.Time         `json:"generatedAt"`
	Window            string            `json:"window"`
	Percentile        float64           `json:"percentile"`
	GrafanaURL        string            `json:"grafanaURL"`
	NamespaceMatchers map[string]string `json:"namespaceMatchers,omitempty"`
	Units             []Unit            `json:"units"`
}

// Unit is one Kubernetes cluster's full picture.
type Unit struct {
	Env        string     `json:"env"`
	Region     string     `json:"region"`
	Cluster    string     `json:"cluster"`
	Role       string     `json:"role"`
	GrafanaURL string     `json:"grafanaURL,omitempty"`
	PeakCPU    int64      `json:"peakCPU,omitempty"` // unix seconds of the CPU peak
	PeakMem    int64      `json:"peakMem,omitempty"` // unix seconds of the memory peak
	NodePools  []NodePool `json:"nodePools"`
	// Totals reflect the peak moment (concurrent capacity vs requested vs used).
	Totals Totals `json:"totals"`
	// PeakWorkloads: concurrent composition at the peak moment (cpu fields from
	// the CPU peak, mem fields from the memory peak). Drives treemap/cards.
	PeakWorkloads []WorkloadAgg `json:"peakWorkloads"`
	// Workloads: all-instance per-pod aggregates over the window. Drives the
	// top-N tuning tables. Here CPU/Mem Usage/Request are PER-POD values and
	// Replicas is the count of distinct instances seen over the window.
	Workloads []WorkloadAgg `json:"workloads"`
}

// NodePool is an AKS agent pool's capacity, categorized system/infra/user.
type NodePool struct {
	Name           string  `json:"name"`
	Category       string  `json:"category"`
	InstanceType   string  `json:"instanceType,omitempty"`
	NodeCount      int     `json:"nodeCount"`
	CPUAllocatable float64 `json:"cpuAllocatable"`
	MemAllocatable float64 `json:"memAllocatable"`
}

// Totals aggregates capacity/requests/usage at any level.
type Totals struct {
	CPUAllocatable float64 `json:"cpuAllocatable"`
	MemAllocatable float64 `json:"memAllocatable"`
	CPURequests    float64 `json:"cpuRequests"`
	MemRequests    float64 `json:"memRequests"`
	CPUUsage       float64 `json:"cpuUsage"`
	MemUsage       float64 `json:"memUsage"`
	NodeCount      int     `json:"nodeCount"`
	PodCount       int     `json:"podCount"`
	WorkloadCount  int     `json:"workloadCount"`
}

// Add accumulates o into t.
func (t *Totals) Add(o Totals) {
	t.CPUAllocatable += o.CPUAllocatable
	t.MemAllocatable += o.MemAllocatable
	t.CPURequests += o.CPURequests
	t.MemRequests += o.MemRequests
	t.CPUUsage += o.CPUUsage
	t.MemUsage += o.MemUsage
	t.NodeCount += o.NodeCount
	t.PodCount += o.PodCount
	t.WorkloadCount += o.WorkloadCount
}

// WorkloadAgg is a workload rolled up by (normalized namespace, workload, type,
// node-pool category). Interpretation of the value fields depends on which list
// it appears in (see Unit.PeakWorkloads vs Unit.Workloads).
type WorkloadAgg struct {
	Namespace    string  `json:"namespace"`
	Workload     string  `json:"workload"`
	WorkloadType string  `json:"workloadType"`
	PoolCategory string  `json:"poolCategory"`
	Replicas     int     `json:"replicas"`
	CPURequest   float64 `json:"cpuRequest"`
	CPUUsage     float64 `json:"cpuUsage"`
	MemRequest   float64 `json:"memRequest"`
	MemUsage     float64 `json:"memUsage"`
}

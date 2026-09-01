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
	"testing"

	"github.com/Azure/ARO-HCP/tooling/cluster-utilization/internal/model"
)

func TestNormalizeApply(t *testing.T) {
	n, _ := NewNormalizer(nil)
	cases := map[string]string{"ocm-int-2abx3-mycluster": "ocm", "klusterlet-2abx3": "klusterlet", "aro-hcp": "aro-hcp"}
	for in, want := range cases {
		if got, _ := n.Apply(in); got != want {
			t.Errorf("Apply(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseCluster(t *testing.T) {
	tests := []struct{ name, env, region, role string }{
		{"int-westus3-mgmt-1", "int", "westus3", "mgmt"},
		{"prod-eastus2-svc-1", "prod", "eastus2", "svc"},
		{"weirdname", "unknown", "", "other"},
	}
	for _, tt := range tests {
		e, r, ro := parseCluster(tt.name)
		if e != tt.env || r != tt.region || ro != tt.role {
			t.Errorf("parseCluster(%q) = (%q,%q,%q)", tt.name, e, r, ro)
		}
	}
}

func TestPoolCategory(t *testing.T) {
	cases := map[string]string{"system": "system", "infra1": "infra", "userswft1": "user", "u64d8ds61": "user"}
	for in, want := range cases {
		if got := poolCategory(in); got != want {
			t.Errorf("poolCategory(%q) = %q", in, got)
		}
	}
}

func TestStripPodSuffix(t *testing.T) {
	cases := map[string]string{
		"kube-apiserver-7d9f8b6c4-x2k9p":      "kube-apiserver",
		"etcd-0":                              "etcd",
		"eraser-aks-u64d8ds61-40245257-kzffz": "eraser",
		"plainname":                           "plainname",
	}
	for in, want := range cases {
		if got := stripPodSuffix(in); got != want {
			t.Errorf("stripPodSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPctl(t *testing.T) {
	if got := pctl([]float64{1, 2, 3, 4}, 0.95); got < 3.8 || got > 4 {
		t.Errorf("p95 = %v, want ~3.85", got)
	}
	if got := pctl(nil, 0.95); got != 0 {
		t.Errorf("empty pctl = %v", got)
	}
}

func TestBuildPeakWorkloads(t *testing.T) {
	n, _ := NewNormalizer(nil)
	// two HCPs' kube-apiserver alive at the memory peak; collapse to ocm/kube-apiserver
	memSnap := clusterSnap{
		owners: []model.OwnerRow{
			{Namespace: "ocm-int-a", Pod: "kube-apiserver-1", Workload: "kube-apiserver", WorkloadType: "deployment"},
			{Namespace: "ocm-int-b", Pod: "kube-apiserver-2", Workload: "kube-apiserver", WorkloadType: "deployment"},
		},
		podNodes: []model.PodNodeRow{
			{Namespace: "ocm-int-a", Pod: "kube-apiserver-1", Node: "aks-userswft1-1-vmss0"},
			{Namespace: "ocm-int-b", Pod: "kube-apiserver-2", Node: "aks-userswft1-1-vmss1"},
		},
		usage: []model.UsageRow{
			{Namespace: "ocm-int-a", Pod: "kube-apiserver-1", Container: "kube-apiserver", Mem: 2 << 30},
			{Namespace: "ocm-int-b", Pod: "kube-apiserver-2", Container: "kube-apiserver", Mem: 1 << 30},
		},
		requests: []model.ResourceRow{
			{Namespace: "ocm-int-a", Pod: "kube-apiserver-1", Container: "kube-apiserver", Mem: 1 << 30},
			{Namespace: "ocm-int-b", Pod: "kube-apiserver-2", Container: "kube-apiserver", Mem: 1 << 30},
		},
	}
	ws := buildPeakWorkloads(clusterSnap{}, memSnap, n)
	if len(ws) != 1 {
		t.Fatalf("expected 1 workload, got %d: %+v", len(ws), ws)
	}
	w := ws[0]
	if w.Namespace != "ocm" || w.Workload != "kube-apiserver" || w.PoolCategory != "user" {
		t.Errorf("identity/pool wrong: %+v", w)
	}
	if w.MemUsage != float64(3<<30) || w.MemRequest != float64(2<<30) || w.Replicas != 2 {
		t.Errorf("peak sums wrong: %+v", w)
	}
}

func TestBuildWindowWorkloadsP95(t *testing.T) {
	n, _ := NewNormalizer(nil)
	// three instances of a test workload that never ran concurrently
	win := clusterSnap{
		owners: []model.OwnerRow{
			{Namespace: "test", Pod: "e2e-1", Workload: "e2e", WorkloadType: "deployment"},
			{Namespace: "test", Pod: "e2e-2", Workload: "e2e", WorkloadType: "deployment"},
			{Namespace: "test", Pod: "e2e-3", Workload: "e2e", WorkloadType: "deployment"},
		},
		podNodes: []model.PodNodeRow{
			{Namespace: "test", Pod: "e2e-1", Node: "aks-userswft1-1-vmss0"},
			{Namespace: "test", Pod: "e2e-2", Node: "aks-userswft1-1-vmss0"},
			{Namespace: "test", Pod: "e2e-3", Node: "aks-userswft1-1-vmss0"},
		},
		usage: []model.UsageRow{
			{Namespace: "test", Pod: "e2e-1", Container: "c", CPU: 1},
			{Namespace: "test", Pod: "e2e-2", Container: "c", CPU: 2},
			{Namespace: "test", Pod: "e2e-3", Container: "c", CPU: 3},
		},
		requests: []model.ResourceRow{
			{Namespace: "test", Pod: "e2e-1", Container: "c", CPU: 0.5},
		},
	}
	ws := buildWindowWorkloads(win, n, 0.95)
	if len(ws) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(ws))
	}
	w := ws[0]
	if w.Replicas != 3 {
		t.Errorf("instances = %d, want 3", w.Replicas)
	}
	if w.CPUUsage < 2.8 || w.CPUUsage > 3 { // p95 across {1,2,3}
		t.Errorf("per-pod p95 usage = %v, want ~2.9", w.CPUUsage)
	}
	if w.CPURequest != 0.5 {
		t.Errorf("per-pod request = %v, want 0.5", w.CPURequest)
	}
}

func TestBuildNodePools(t *testing.T) {
	nodes := []model.NodeRow{
		{Cluster: "c", Node: "aks-system-1-vmss0", Pool: "system", CPUAllocatable: 4},
		{Cluster: "c", Node: "aks-system-1-vmss1", Pool: "system", CPUAllocatable: 4},
		{Cluster: "c", Node: "aks-userswft1-1-vmss0", Pool: "userswft1", CPUAllocatable: 8},
	}
	pools := buildNodePools(nodes)
	if len(pools) != 2 || pools[0].Category != "system" || pools[0].NodeCount != 2 || pools[1].Category != "user" {
		t.Errorf("pools wrong: %+v", pools)
	}
}

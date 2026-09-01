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
	"testing"

	"github.com/Azure/ARO-HCP/tooling/cluster-utilization/internal/grafana"
)

func TestGroupRegions(t *testing.T) {
	ds := []grafana.Datasource{
		{UID: "services-uksouth", Type: "prometheus"},
		{UID: "hcps-uksouth", Type: "prometheus"},
		{UID: "services-westus3", Type: "prometheus"},
		{UID: "geneva", Type: "grafana-something"},
	}
	regions := groupRegions(ds, nil)
	if len(regions) != 2 {
		t.Fatalf("expected 2 regions, got %d: %+v", len(regions), regions)
	}
	// sorted by name: uksouth, westus3
	if regions[0].name != "uksouth" || regions[0].servicesUID != "services-uksouth" || regions[0].hcpsUID != "hcps-uksouth" {
		t.Errorf("uksouth region wrong: %+v", regions[0])
	}
	if regions[1].name != "westus3" || regions[1].hcpsUID != "" {
		t.Errorf("westus3 region wrong: %+v", regions[1])
	}
}

func TestPoolFromNode(t *testing.T) {
	cases := map[string]string{
		"aks-userswft3-40057497-vmss000000": "userswft3",
		"aks-system-12345678-vmss000001":    "system",
		"weird-node":                        "unknown",
	}
	for in, want := range cases {
		if got := poolFromNode(in); got != want {
			t.Errorf("poolFromNode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBatchNamespaces(t *testing.T) {
	ns := []string{"c", "a", "b", "a", ""}
	got := batchNamespaces(ns, 2)
	if len(got) != 2 || got[0] != `^(a|b)$` || got[1] != `^(c)$` {
		t.Errorf("batches = %v", got)
	}
}

func TestSnapshotQueriesUseAtModifier(t *testing.T) {
	if got := memUsageAt("c1", "^(a)$", 123); !contains(got, "@ 123") || !contains(got, `cluster="c1"`) {
		t.Errorf("memUsageAt = %q", got)
	}
	if got := cpuUsageAt("c1", "^(a)$", 123); !contains(got, "[5m] @ 123") {
		t.Errorf("cpuUsageAt = %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

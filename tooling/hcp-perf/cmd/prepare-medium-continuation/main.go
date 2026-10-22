// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/iteration"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"os"
	"reflect"
	"syscall"
	"time"
)

const root = "/home/stevekuznetsov/code/Azure/ARO-HCP/src/github.com/Azure/ARO-HCP-perf-runner"
const out = root + "/_artifacts/medium-capacity-live-20260918T1313Z"

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func main() {
	syscall.Umask(0077)
	var original iteration.Spec
	b, e := os.ReadFile(root + "/_artifacts/sequential-medium-20260918T1125Z/medium-startup.spec.json")
	must(e)
	must(json.Unmarshal(b, &original))
	s := original
	s.Environment.Incarnation = "medium150-continuation-20260918T1340Z"
	s.Actions = nil
	var observe iteration.Action
	for _, a := range original.Actions {
		if a.Kind == iteration.StartupObserve {
			observe = a
		}
	}
	clients, e := startup.NewClients(observe.Startup.Kubeconfig, true)
	must(e)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id, e := startup.Discover(ctx, clients, observe.Startup.ResourceID, startup.Identity{})
	must(e)
	if id.HCUID != "f89195f2-2745-4492-bffc-1228af976a10" || id.HCPUID != "81727d2d-1382-4bca-b66c-db1ad51fc58b" || id.NamespaceUID != "3644e51e-5689-4993-892e-ef1a5b274112" {
		panic("live identity mismatch")
	}
	for _, a := range original.Actions {
		switch a.Kind {
		case iteration.CostEstimate:
			s.Actions = append(s.Actions, a)
		case iteration.RetentionRefresh:
			a.Retention.Duration = 2 * time.Hour
			a.Timeout = 2*time.Hour + 5*time.Minute
			s.Actions = append(s.Actions, a)
		case iteration.StartupObserve:
			a.Kind = iteration.StartupObserveExisting
			a.ObservationIdentity = &id
			a.DependsOn = []iteration.Dependency{{Action: "retain", Milestone: iteration.Started}}
			a.Startup.MaxDuration = 45 * time.Minute
			a.Timeout = 50 * time.Minute
			found := false
			for i := range a.Startup.Components {
				c := &a.Startup.Components[i]
				if c.Kind != "Deployment" || c.Name != "router" {
					continue
				}
				for j := range c.Containers {
					r := &c.Containers[j]
					if r.Name != "router" {
						continue
					}
					r.Resources.Requests[corev1.ResourceName("aro.openshift.io/swift-nic")] = resource.MustParse("1")
					r.Resources.Limits[corev1.ResourceName("aro.openshift.io/swift-nic")] = resource.MustParse("1")
					found = true
				}
			}
			if !found {
				panic("router missing")
			}
			s.Actions = append(s.Actions, a)
		case iteration.RPAdminCredential:
			a.DependsOn = []iteration.Dependency{{Action: "observe", Milestone: iteration.Completed}}
			if _, e := os.Stat(a.RP.CredentialPath); !os.IsNotExist(e) {
				panic("credential path exists or uncertain")
			}
			s.Actions = append(s.Actions, a)
		case iteration.RPCreateNodePool:
			a.DependsOn = []iteration.Dependency{{Action: "observe", Milestone: iteration.Completed}, {Action: "credential", Milestone: iteration.Completed}}
			s.Actions = append(s.Actions, a)
		}
	}
	must(s.Validate())
	for _, a := range s.Actions {
		if a.Kind == iteration.RPCreateCluster || a.Kind == iteration.CandidateApply || a.Kind == iteration.RPDelete {
			panic("forbidden replay")
		}
	}
	// Verify the only component expectation change is the exact extended resource.
	var fresh iteration.Spec
	must(json.Unmarshal(b, &fresh))
	for _, a := range fresh.Actions {
		if a.Kind != iteration.StartupObserve {
			continue
		}
		for _, x := range s.Actions {
			if x.Startup == nil {
				continue
			}
			for i, c := range x.Startup.Components {
				old := a.Startup.Components[i]
				if c.Name == "router" {
					old.Containers[0].Resources.Requests["aro.openshift.io/swift-nic"] = resource.MustParse("1")
					old.Containers[0].Resources.Limits["aro.openshift.io/swift-nic"] = resource.MustParse("1")
				}
				if !reflect.DeepEqual(old, c) {
					panic("unexpected component change")
				}
			}
		}
	}
	b, e = json.MarshalIndent(s, "", "  ")
	must(e)
	b = append(b, '\n')
	f, e := os.OpenFile(out+"/medium-continuation.spec.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
	must(e)
	_, e = f.Write(b)
	must(e)
	must(f.Sync())
	must(f.Close())
	fmt.Printf("validated continuation only; sha256=%x\n", sha256.Sum256(b))
}

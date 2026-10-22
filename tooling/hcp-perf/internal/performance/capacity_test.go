// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package performance

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
)

func capacityFixture(count int) (Spec, []corev1.Node) {
	s := testSpec()
	s.Seed.GuestNodes, s.Seed.Applications = int64(count), int64(count)*20
	s.ExpectedGuestNodes = nil
	nodes := []corev1.Node{}
	for i := range count {
		name, uid := fmt.Sprintf("node-%02d", i), fmt.Sprintf("uid-%02d", i)
		s.ExpectedGuestNodes = append(s.ExpectedGuestNodes, GuestNodeIdentity{Name: name, UID: uid})
		nodes = append(nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid), Labels: map[string]string{corev1.LabelOSStable: "linux"}}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("110"), corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}})
	}
	return s, nodes
}

func TestGuestNodeContract(t *testing.T) {
	for _, count := range []int{0, 2, 3, 6, 12, 64, 65} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, _ := capacityFixture(count)
			if err := s.ValidateRun(); (err == nil) != (count >= 3 && count <= 64) {
				t.Fatalf("node bound: %v", err)
			}
		})
	}
	for name, mutate := range map[string]func(*Spec){
		"omitted":        func(s *Spec) { s.ExpectedGuestNodes = nil },
		"count":          func(s *Spec) { s.Seed.GuestNodes++ },
		"duplicate-name": func(s *Spec) { s.ExpectedGuestNodes[1].Name = s.ExpectedGuestNodes[0].Name },
		"duplicate-uid":  func(s *Spec) { s.ExpectedGuestNodes[1].UID = s.ExpectedGuestNodes[0].UID },
		"empty-uid":      func(s *Spec) { s.ExpectedGuestNodes[0].UID = "" },
		"long-uid":       func(s *Spec) { s.ExpectedGuestNodes[0].UID = strings.Repeat("x", 129) },
		"invalid-uid":    func(s *Spec) { s.ExpectedGuestNodes[0].UID = "a\nb" },
		"invalid-name":   func(s *Spec) { s.ExpectedGuestNodes[0].Name = "not/a/node" },
		"unsorted": func(s *Spec) {
			s.ExpectedGuestNodes[0], s.ExpectedGuestNodes[1] = s.ExpectedGuestNodes[1], s.ExpectedGuestNodes[0]
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := capacityFixture(6)
			mutate(&s)
			if s.ValidateRun() == nil {
				t.Fatal("accepted invalid explicit node inventory")
			}
		})
	}
}

func TestGrowingPopulationKeepsFixedClientMix(t *testing.T) {
	for _, count := range []int{3, 6, 12} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, nodes := capacityFixture(count)
			s.API = &apiload.Spec{Version: "v1", DurationSeconds: 3600, RatePerSecond: 30, MaxInFlight: 10, RequestTimeoutSeconds: 10}
			for i := range 3 {
				ns := fmt.Sprintf("fresh-%d", i)
				s.API.Namespaces = append(s.API.Namespaces, ns)
				s.API.Targets = append(s.API.Targets, apiload.Target{Namespace: ns, APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Name: fmt.Sprintf("app-%06d", i*20), Weight: 1})
			}
			if err := s.Validate(); err != nil {
				t.Fatal(err)
			}
			bundle, err := seed.Render(s.Seed)
			if err != nil || bundle.Namespaces != int64(count) || bundle.Pods != int64(count)*100 {
				t.Fatalf("population distribution changed: namespaces=%d pods=%d err=%v", bundle.Namespaces, bundle.Pods, err)
			}
			for _, expected := range []string{"replicas: 5", "maxSurge: 1", "maxUnavailable: 0"} {
				if !strings.Contains(bundle.Templates["deployment.yaml"], expected) {
					t.Fatalf("seed turnover contract missing %q", expected)
				}
			}
			if allocation, err := capacity(nodes, nil, s); err != nil || len(allocation) != count {
				t.Fatalf("capacity: %v %v", allocation, err)
			}
		})
	}
}

func TestGuestNodeSafetyAndIdentity(t *testing.T) {
	for _, count := range []int{3, 6, 12} {
		for _, mode := range []string{"missing", "extra", "extra-ineligible", "duplicate", "uid", "empty-uid", "not-ready", "cordon", "deleting", "windows", "no-schedule", "no-execute", "memory", "disk", "pid", "unknown-pressure"} {
			t.Run(fmt.Sprintf("%d/%s", count, mode), func(t *testing.T) {
				s, nodes := capacityFixture(count)
				switch mode {
				case "missing":
					nodes = nodes[1:]
				case "extra", "extra-ineligible":
					n := *nodes[0].DeepCopy()
					n.Name, n.UID = "extra", "extra"
					n.Spec.Unschedulable = mode == "extra-ineligible"
					nodes = append(nodes, n)
				case "duplicate":
					nodes[1] = nodes[0]
				case "uid":
					nodes[0].UID = "replacement"
				case "empty-uid":
					nodes[0].UID = ""
				case "not-ready":
					nodes[0].Status.Conditions = nil
				case "cordon":
					nodes[0].Spec.Unschedulable = true
				case "deleting":
					nodes[0].DeletionTimestamp = ptr.To(metav1.Now())
				case "windows":
					nodes[0].Labels[corev1.LabelOSStable] = "windows"
				case "no-schedule":
					nodes[0].Spec.Taints = []corev1.Taint{{Effect: corev1.TaintEffectNoSchedule}}
				case "no-execute":
					nodes[0].Spec.Taints = []corev1.Taint{{Effect: corev1.TaintEffectNoExecute}}
				default:
					condition := corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue}
					if mode == "disk" {
						condition.Type = corev1.NodeDiskPressure
					}
					if mode == "pid" {
						condition.Type = corev1.NodePIDPressure
					}
					if mode == "unknown-pressure" {
						condition.Status = corev1.ConditionUnknown
					}
					nodes[0].Status.Conditions = append(nodes[0].Status.Conditions, condition)
				}
				if guestNodesSafe(nodes, s) == nil {
					t.Fatal("poll accepted unsafe or changed inventory")
				}
				if _, err := capacity(nodes, nil, s); err == nil {
					t.Fatal("admission accepted unsafe or changed inventory")
				}
			})
		}
	}
}

func TestCapacityFreshReuseAndRestoreRolloutHeadroom(t *testing.T) {
	for _, count := range []int{3, 6, 12} {
		for _, mode := range []string{"fresh", "reuse", "restore"} {
			for _, outstanding := range []int{1, 10, 16, 100} {
				t.Run(fmt.Sprintf("%d/%s/%d", count, mode, outstanding), func(t *testing.T) {
					s, nodes := capacityFixture(count)
					s.MaxOutstanding = outstanding
					var pods []corev1.Pod
					if mode != "fresh" {
						s.ExistingPopulation = &PopulationReference{}
					}
					s.RestorePopulation = mode == "restore"
					// Existing population occupies exactly 100 Pods/node, never
					// subtracted by a label-based guess or charged a second time.
					if mode == "reuse" {
						for _, node := range nodes {
							for range 100 {
								pods = append(pods, corev1.Pod{Spec: corev1.PodSpec{NodeName: node.Name, Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m"), corev1.ResourceMemory: resource.MustParse("8Mi")}}}}}})
							}
						}
					}
					extra := (7*min(int64(outstanding), s.Seed.Applications) + 1 + int64(count) - 1) / int64(count)
					share := 100 + extra
					for i := range nodes {
						nodes[i].Status.Allocatable = corev1.ResourceList{corev1.ResourcePods: *resource.NewQuantity(share+2, resource.DecimalSI), corev1.ResourceCPU: *resource.NewMilliQuantity(share*5, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(share*(8<<20), resource.BinarySI)}
					}
					if _, err := capacity(nodes, pods, s); err != nil {
						t.Fatal("exact reservation rejected", err)
					}
					for _, r := range []corev1.ResourceName{corev1.ResourcePods, corev1.ResourceCPU, corev1.ResourceMemory} {
						original := nodes[0].Status.Allocatable[r]
						q := original.DeepCopy()
						delta := resource.MustParse("1")
						if r == corev1.ResourceCPU {
							delta = resource.MustParse("1m")
						}
						q.Sub(delta)
						nodes[0].Status.Allocatable[r] = q
						if _, err := capacity(nodes, pods, s); err == nil {
							t.Fatalf("ignored per-node %s rollout headroom", r)
						}
						nodes[0].Status.Allocatable[r] = original
					}
				})
			}
		}
	}
}

func TestCapacityPlatformRequests(t *testing.T) {
	for _, mode := range []string{"slots", "cpu", "memory", "overhead", "init", "sidecar-init", "terminating", "unassigned", "unknown-node", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			s, nodes := capacityFixture(6)
			p := corev1.Pod{Spec: corev1.PodSpec{NodeName: nodes[0].Name, Containers: []corev1.Container{{Name: "app"}}}}
			requests := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}}
			pods := []corev1.Pod{}
			switch mode {
			case "slots":
				for range 7 {
					pods = append(pods, p)
				}
			case "cpu", "terminating", "terminal":
				p.Spec.Containers[0].Resources = requests
				if mode == "terminating" {
					p.DeletionTimestamp = ptr.To(metav1.Now())
				}
				if mode == "terminal" {
					p.Status.Phase = corev1.PodSucceeded
				}
			case "memory":
				p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")}
			case "overhead":
				p.Spec.Overhead = requests.Requests
			case "init":
				p.Spec.InitContainers = []corev1.Container{{Name: "init", Resources: requests}}
			case "sidecar-init":
				p.Spec.InitContainers = []corev1.Container{{Name: "sidecar", RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways), Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}}}, {Name: "init", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}}}}
			case "unassigned":
				p.Spec.NodeName = ""
			case "unknown-node":
				p.Spec.NodeName = "other"
			}
			pods = append(pods, p)
			if _, err := capacity(nodes, pods, s); (err == nil) != (mode == "terminal") {
				t.Fatalf("platform accounting: %v", err)
			}
		})
	}
}

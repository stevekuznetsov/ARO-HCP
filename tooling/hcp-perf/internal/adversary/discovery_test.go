// Copyright 2026 Microsoft Corporation
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

package adversary

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func minimalRunSpec() RunSpec {
	spec := runtimeSpec()
	spec.ReserveCPUMilli, spec.MemoryRequestMi, spec.MemoryLimitMi = nil, 0, 0
	return spec
}

func assertReadOnly(t *testing.T, client *fake.Clientset) {
	t.Helper()
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatalf("read-only operation performed %s on %s", action.GetVerb(), action.GetResource())
		}
	}
}

func TestDiscoverDynamicHeadroom(t *testing.T) {
	ctx := context.Background()
	var objects []runtime.Object
	for i, cores := range []int64{4, 16, 32} {
		name := fmt.Sprintf("worker-%d", i)
		node := runtimeNode(name)
		node.Status.Capacity[corev1.ResourceCPU] = *resource.NewMilliQuantity(cores*1000, resource.DecimalSI)
		node.Status.Allocatable[corev1.ResourceCPU] = *resource.NewMilliQuantity(cores*1000-125, resource.DecimalSI)
		objects = append(objects, node, runtimePod("hcp", name, name, fmt.Sprintf("%dm", cores*1000-int64(500+i*100))))
	}
	// No execution namespace is needed, and irrelevant nonworker nodes are ignored.
	unused := runtimeNode("infra")
	unused.Labels[workerRoleLabel] = "infra"
	objects = append(objects, unused)
	client := fake.NewClientset(objects...)
	spec := minimalRunSpec()
	got, err := Discover(ctx, client, spec)
	if err != nil {
		t.Fatal(err)
	}
	assertReadOnly(t, client)
	if !reflect.DeepEqual(spec, minimalRunSpec()) || got.Spec.CPUFillPercent != 90 || got.Plan.CPUFillPercent != 90 || got.Snapshot.CPUFillPercent != 90 {
		t.Fatalf("defaults mutated input or policy missing: %#v", got)
	}
	if got.Registration != "" || got.Phase != "discovered" || len(got.Jobs) != 0 || !got.PlacementValid || got.SubmissionIncomplete {
		t.Fatalf("discovery pretended to submit resources: %#v", got)
	}
	if len(got.Plan.Nodes) != 3 || len(got.Snapshot.TargetPods) != 3 {
		t.Fatalf("wrong target placement: %#v", got)
	}
	for i, cores := range []int64{4, 16, 32} {
		node, plan := got.Snapshot.Nodes[i], got.Plan.Nodes[i]
		headroom := int64(375 + i*100)
		if node.ReserveCPUMilli != (headroom+9)/10 || plan.CPURequestMilli != headroom-(headroom+9)/10 || plan.Workers != cores {
			t.Fatalf("not based on each node's free headroom: %#v, %#v", node, plan)
		}
		if node.UID == "" || got.Snapshot.TargetPods["hcp-"+node.Name] != node.Name || plan.MemoryRequestMi != 32 || plan.MemoryLimitMi != 128 {
			t.Fatalf("missing identities or memory policy: %#v", got)
		}
	}
	// Start resolves exactly the same plan rather than maintaining its own policy.
	started, err := Start(ctx, runtimeClient(objects...), spec)
	if err != nil {
		t.Fatal(err)
	}
	started.Plan.CollectedAt, started.Snapshot.CollectedAt = got.Plan.CollectedAt, got.Snapshot.CollectedAt
	if !reflect.DeepEqual(started.Plan, got.Plan) || !reflect.DeepEqual(started.Snapshot, got.Snapshot) || !reflect.DeepEqual(started.Spec, got.Spec) {
		t.Fatal("Start and Discover resolved differently")
	}
}

func TestDiscoverPolicyAndPreflight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*RunSpec, *corev1.Node, *corev1.Pod)
		want    string
		request int64
	}{
		{"default", func(*RunSpec, *corev1.Node, *corev1.Pod) {}, "", 6750},
		{"fill one", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.CPUFillPercent = 1 }, "", 75},
		{"fill full", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.CPUFillPercent = 100 }, "", 7500},
		{"zero reserve", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.ReserveCPUMilli = reservePtr(0) }, "", 7500},
		{"reserve override", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.ReserveCPUMilli = reservePtr(250) }, "", 7250},
		{"conflict", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) {
			s.ReserveCPUMilli = reservePtr(0)
			s.CPUFillPercent = 90
		}, "mutually exclusive", 0},
		{"negative fill", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.CPUFillPercent = -1 }, "cpuFillPercent", 0},
		{"excess fill", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.CPUFillPercent = 101 }, "cpuFillPercent", 0},
		{"negative reserve", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.ReserveCPUMilli = reservePtr(-1) }, "reserveCPUMilli", 0},
		{"one milli free", func(_ *RunSpec, _ *corev1.Node, p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("7999m")
		}, "no CPU request headroom", 0},
		{"two milli free", func(_ *RunSpec, _ *corev1.Node, p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("7998m")
		}, "", 1},
		{"missing capacity", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) { delete(n.Status.Capacity, corev1.ResourceCPU) }, "capacityCPUMilli", 0},
		{"missing allocatable", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) { delete(n.Status.Allocatable, corev1.ResourceCPU) }, "allocatable CPU", 0},
		{"missing memory", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) { delete(n.Status.Allocatable, corev1.ResourceMemory) }, "memory request headroom", 0},
		{"missing slots", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) { delete(n.Status.Allocatable, corev1.ResourcePods) }, "pod slots", 0},
		{"full slots", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) {
			n.Status.Allocatable[corev1.ResourcePods] = resource.MustParse("1")
		}, "pod slots", 0},
		{"one slot free", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) {
			n.Status.Allocatable[corev1.ResourcePods] = resource.MustParse("2")
		}, "", 6750},
		{"no node UID", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) { n.UID = "" }, "no UID", 0},
		{"no pod UID", func(_ *RunSpec, _ *corev1.Node, p *corev1.Pod) { p.UID = "" }, "no UID", 0},
		{"NoSchedule", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) {
			n.Spec.Taints = []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}}
		}, "untolerated taint", 0},
		{"NoExecute", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) {
			n.Spec.Taints = []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoExecute}}
		}, "untolerated taint", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, node, pod := minimalRunSpec(), runtimeNode("worker"), runtimePod("hcp", "api", "worker", "500m")
			tc.change(&spec, node, pod)
			client := runtimeClient(node, pod)
			got, err := Discover(context.Background(), client, spec)
			assertReadOnly(t, client)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) || !reflect.DeepEqual(got, RunStatus{}) {
					t.Fatalf("expected no plan and error %q: %#v, %v", tc.want, got, err)
				}
			} else if err != nil || got.Plan.Nodes[0].CPURequestMilli != tc.request {
				t.Fatalf("expected request %d: %#v, %v", tc.request, got, err)
			}
		})
	}
}

func TestDiscoverMemoryRequests(t *testing.T) {
	for _, extra := range []int64{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			node := runtimeNode("worker")
			node.Status.Allocatable[corev1.ResourceMemory] = resource.MustParse("100Mi")
			node.Status.Allocatable[corev1.ResourcePods] = resource.MustParse("3")
			target := runtimePod("hcp", "api", "worker", "500m")
			other := runtimePod("other", "neighbor", "worker", "0")
			always := corev1.ContainerRestartPolicyAlways
			other.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("10Mi")
			other.Spec.InitContainers = []corev1.Container{
				{Name: "sidecar", RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("20Mi")}}},
				{Name: "init", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("40Mi")}}},
			}
			other.Spec.Overhead = corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(8*1024*1024+extra, resource.BinarySI)}
			done := runtimePod("other", "done", "worker", "100")
			done.Status.Phase = corev1.PodFailed
			done.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1Ti")
			client := runtimeClient(node, target, other, done)
			got, err := Discover(context.Background(), client, minimalRunSpec())
			assertReadOnly(t, client)
			if extra != 0 {
				if err == nil || !strings.Contains(err.Error(), "memory request headroom") {
					t.Fatalf("accepted insufficient memory: %v", err)
				}
				return
			}
			if err != nil || got.Snapshot.Nodes[0].CommittedMemoryBytes != 68*1024*1024 || got.Snapshot.Nodes[0].CommittedPods != 2 {
				t.Fatalf("incorrect effective memory requests or pod slots: %#v, %v", got, err)
			}
		})
	}
}

func TestStatusPlacementDrift(t *testing.T) {
	for _, kind := range []string{"unchanged", "moved", "new-unscheduled", "new-same-node", "removed-pod", "terminal-pod", "removed-node", "replaced-node", "role", "read-error"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			pod, node := runtimePod("hcp", "api", "worker", "500m"), runtimeNode("worker")
			client := runtimeClient(node, pod)
			started, err := Start(ctx, client, minimalRunSpec())
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "moved":
				pod.Spec.NodeName = "other-worker"
				_, err = client.CoreV1().Pods("hcp").Update(ctx, pod, metav1.UpdateOptions{})
			case "new-unscheduled", "new-same-node":
				newNode := ""
				if kind == "new-same-node" {
					newNode = "worker"
				}
				_, err = client.CoreV1().Pods("hcp").Create(ctx, runtimePod("hcp", "new", newNode, "0"), metav1.CreateOptions{})
			case "removed-pod":
				err = client.CoreV1().Pods("hcp").Delete(ctx, pod.Name, metav1.DeleteOptions{})
			case "terminal-pod":
				pod.Status.Phase = corev1.PodSucceeded
				_, err = client.CoreV1().Pods("hcp").UpdateStatus(ctx, pod, metav1.UpdateOptions{})
			case "removed-node":
				err = client.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{})
			case "replaced-node", "role":
				if kind == "role" {
					node.Labels[workerRoleLabel] = "infra"
				} else {
					node.UID = "replacement"
				}
				_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			case "read-error":
				client.PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
					if action.GetNamespace() == "hcp" {
						return true, nil, errors.New("forbidden")
					}
					return false, nil, nil
				})
				client.PrependReactor("get", "nodes", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("forbidden") })
			}
			if err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			got, err := Status(ctx, client, "capture", "test-run")
			assertReadOnly(t, client)
			if err != nil || got.Phase != "pending" || !reflect.DeepEqual(got.Plan, started.Plan) || !reflect.DeepEqual(got.Snapshot, started.Snapshot) {
				t.Fatalf("drift changed execution or retuned plan: %#v, %v", got, err)
			}
			want := "drifted"
			if kind == "unchanged" {
				want = "valid"
			}
			if kind == "read-error" {
				want = "unverified"
			}
			if got.PlacementValidity != want || got.PlacementValid != (kind == "unchanged") || (len(got.Drift) == 0) != (kind == "unchanged") {
				t.Fatalf("incorrect placement observations: %#v", got)
			}
			stopped, err := Stop(ctx, client, "capture", "test-run")
			if err != nil || stopped.Phase != "stopped" || stopped.PlacementValidity != want {
				t.Fatalf("placement checks blocked cleanup: %#v, %v", stopped, err)
			}
		})
	}
}

func TestLegacyRegistration(t *testing.T) {
	// Literal old wire format: integer reserve, no policy, node UID, memory
	// observations or pod identities. Do not regenerate with today's types.
	const data = `{"spec":{"version":"v1","id":"test-run","namespace":"capture","image":"registry.example/hcp-perf@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","targetNamespace":"hcp","reserveCPUMilli":250,"memoryRequestMi":16,"memoryLimitMi":32,"durationSeconds":10},"snapshot":{"version":"v1","collectedAt":"2026-01-01T00:00:00Z","nodes":[{"name":"worker","pool":"workers","role":"worker","hostsTarget":true,"capacityCPUMilli":4000,"allocatableCPUMilli":3750,"committedCPUMilli":500,"reserveCPUMilli":250}],"durationSeconds":10,"memoryRequestMi":16,"memoryLimitMi":32},"plan":{"version":"v1","collectedAt":"2026-01-01T00:00:00Z","validity":"declared-snapshot-only","nodes":[{"name":"worker","cpuRequestMilli":3000,"workers":4,"deadlineSeconds":10,"memoryRequestMi":16,"memoryLimitMi":32}]}}`
	immutable := true
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "adversaries-test-run", Namespace: "capture", UID: types.UID("registration"), ResourceVersion: "1", Labels: runLabels("test-run"), Annotations: map[string]string{digestAnnotation: fmt.Sprintf("%x", sha256.Sum256([]byte(data)))}}, Immutable: &immutable, Data: map[string]string{"run.json": data}}
	client := runtimeClient(cm)
	ctx := context.Background()
	_, record, err := loadRegistration(ctx, client, "capture", "test-run")
	if err != nil {
		t.Fatal(err)
	}
	if record.Spec.ReserveCPUMilli == nil || *record.Spec.ReserveCPUMilli != 250 {
		t.Fatal("legacy reserve lost")
	}
	encoded, err := json.Marshal(record)
	if err != nil || string(encoded) != data {
		t.Fatalf("legacy wire format changed: %s, %v", encoded, err)
	}
	if _, err := client.BatchV1().Jobs("capture").Create(ctx, desiredJob(cm, record, record.Plan.Nodes[0]), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	got, err := Status(ctx, client, "capture", "test-run")
	if err != nil || got.PlacementValid || got.PlacementValidity != "unverified-legacy" {
		t.Fatalf("legacy status: %#v, %v", got, err)
	}
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "nodes" || action.GetResource().Resource == "pods" && action.GetNamespace() == "hcp" {
			t.Fatal("legacy run attempted placement validation")
		}
	}
	got, err = Stop(ctx, client, "capture", "test-run")
	if err != nil || got.Phase != "stopped" {
		t.Fatalf("legacy cleanup failed: %#v, %v", got, err)
	}
}

func TestAdversariesExample(t *testing.T) {
	data, err := os.ReadFile("../../examples/adversaries.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec RunSpec
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.ReserveCPUMilli != nil || spec.CPUFillPercent != 0 || spec.MemoryRequestMi != 0 || spec.MemoryLimitMi != 0 {
		t.Fatal("example should demonstrate minimal policy inputs")
	}
	client := fake.NewClientset(runtimeNode("discovered-worker"), runtimePod(spec.TargetNamespace, "api", "discovered-worker", "1"))
	if _, err := Discover(context.Background(), client, spec); err != nil {
		t.Fatal(err)
	}
	assertReadOnly(t, client)
}

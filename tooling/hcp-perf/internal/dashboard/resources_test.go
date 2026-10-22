// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dashboard

import (
	"math"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCPUIntervals(t *testing.T) {
	values := []sample{{10, 1}, {20, 1}, {30, 5}, {40, 0}, {50, 2}, {110, 5}, {120, 6}}
	times := []sample{{10, 10}, {20, 10}, {30, 30}, {40, 40}, {50, 50}, {110, 110}, {120, 0}}
	got := cpuIntervals(values, times, 0, 120)
	if len(got) != 2 || got[0].start != 10 || got[0].end != 30 || got[0].value != .2 || got[1].start != 40 {
		t.Fatalf("repeat/reset/gap/zero-source handling: %+v", got)
	}
	if v, ok := integrated(got, 15, 30); !ok || math.Abs(v-.2) > 1e-9 {
		t.Fatalf("piecewise integration: %v %v", v, ok)
	}
	if _, ok := integrated(got, 30, 45); ok {
		t.Fatal("reset interval filled")
	}
	if got := cpuIntervals(values, times, 11, 29); len(got) != 0 {
		t.Fatal("window boundary crossed")
	}
}

func TestCompleteSumAndFreshMemory(t *testing.T) {
	c := &resourceComponent{containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}}, usage: ComponentUsage{Replicas: 2, Containers: 4}}
	if _, ok := componentSum(map[string]float64{"a/main": 1, "b/main": 2, "c/sidecar": 3, "d/sidecar": 4}, c); ok {
		t.Fatal("mismatched replica sets formed a complete component")
	}
	if _, ok := completeSum(map[string]float64{"a": 2}, 2); ok {
		t.Fatal("missing replica treated as zero")
	}
	if _, ok := completeSum(map[string]float64{"a": 2, "b": math.NaN()}, 2); ok {
		t.Fatal("duplicate runtime included")
	}
	if v, ok := completeSum(map[string]float64{"a": 2, "b": 3}, 2); !ok || v != 5 {
		t.Fatal("complete sum incorrect")
	}
	if _, ok := freshGauge([]sample{{50, 5}}, []sample{{50, 0}}, 50, 1, 100); ok {
		t.Fatal("zero source timestamp accepted")
	}
	if _, ok := freshGauge([]sample{{50, 5}}, []sample{{50, 2}}, 50, 1, 100); ok {
		t.Fatal("stale source timestamp accepted")
	}
	if _, ok := freshGauge([]sample{{50, 5}}, nil, 50, 1, 100); ok {
		t.Fatal("freshness inferred from other metric")
	}
}

func TestResourceReferences(t *testing.T) {
	containers := []corev1.Container{{Name: "go", Env: []corev1.EnvVar{{Name: "GOMEMLIMIT", Value: "64MiB"}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("100Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("40m"), corev1.ResourceMemory: resource.MustParse("200Mi")}}}, {Name: "unlimited"}}
	u := resourceReferences(containers, 3)
	if u.Containers != 6 || u.CPULimit != nil || u.MemoryLimit != nil || u.GoTarget == nil || *u.GoTarget != 192 || u.CPURequest == nil || math.Abs(*u.CPURequest-.06) > 1e-9 || len(u.Notes) < 2 {
		t.Fatalf("incorrect partial references: %+v", u)
	}
	if resourceReferences([]corev1.Container{{Name: "native"}}, 1).GoTarget != nil {
		t.Fatal("missing Go target is not zero")
	}
}

func TestOwnershipDoesNotStripNames(t *testing.T) {
	controller := true
	pod := inventoryObject{Kind: "Pod", Metadata: metav1.ObjectMeta{Name: "api-abc-xyz", Labels: map[string]string{"app": "api"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-abc", UID: "rs-uid", Controller: &controller}}}}
	component := &resourceComponent{object: inventoryObject{Kind: "Deployment", Metadata: metav1.ObjectMeta{Name: "api", UID: "deployment-uid"}}}
	components := map[string]*resourceComponent{"api": component}
	if name, _ := matchingComponent(pod, components, nil); name != "" {
		t.Fatal("ownership guessed from name")
	}
	component.object.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}
	if name, fallback := matchingComponent(pod, components, nil); name != "api" || !fallback {
		t.Fatal("unique selector fallback must be labeled")
	}
	components["other"] = &resourceComponent{object: component.object}
	if name, _ := matchingComponent(pod, components, nil); name != "" {
		t.Fatal("ambiguous selector accepted")
	}
}

func TestMetricIdentity(t *testing.T) {
	p := &podIdentity{object: inventoryObject{Metadata: metav1.ObjectMeta{Name: "api", UID: "1234-abcd"}}}
	p.object.Spec.Containers = []corev1.Container{{Name: "api", Image: "image"}}
	p.object.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "api", ContainerID: "containerd://runtime1"}}
	pods := map[string]*podIdentity{"1234-abcd": p}
	m := map[string]string{"pod": "api", "container": "api", "image": "image", "id": "/pod1234_abcd.slice/runtime1", "name": "runtime1"}
	if metricPod(m, pods) != p {
		t.Fatal("exact identity rejected")
	}
	m["name"] = "runtime2"
	if metricPod(m, pods) != nil {
		t.Fatal("different process identity joined")
	}
	m["name"] = "runtime1"
	m["id"] = "/pod0000_abcd.slice/runtime1"
	if metricPod(m, pods) != nil {
		t.Fatal("different pod UID joined")
	}
}

func TestRuntimeReplacementTimeline(t *testing.T) {
	oldStart := time.Unix(10, 0)
	newStart := time.Unix(90, 0)
	p := &podIdentity{}
	p.object.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", ContainerID: "containerd://old", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(oldStart)}}}}
	status := func(id string, started time.Time) recordedContainerStatus {
		s := recordedContainerStatus{Name: "main", ContainerID: id}
		s.State.State = "running"
		s.State.StartedAt = started
		return s
	}
	p.runtimes = []runtimeObservation{{at: 80, statuses: []recordedContainerStatus{status("", oldStart)}}, {at: 95, statuses: []recordedContainerStatus{status("", newStart)}}}
	m := map[string]string{"container": "main", "name": "old"}
	if !runtimeMatches(p, m, 81, 85) {
		t.Fatal("unchanged old incarnation rejected")
	}
	if runtimeMatches(p, m, 96, 100) {
		t.Fatal("stale inventory ID survived recorder replacement")
	}
	m["name"] = "new"
	if runtimeMatches(p, m, 96, 100) {
		t.Fatal("replacement runtime guessed from start time")
	}
	p.runtimes[1].statuses[0].ContainerID = "containerd://new"
	if !runtimeMatches(p, m, 96, 100) {
		t.Fatal("authoritatively recorded preflight replacement rejected")
	}
	if runtimeMatches(p, m, 81, 85) {
		t.Fatal("future replacement receipt applied backwards")
	}
	if runtimeMatches(p, m, 85, 100) {
		t.Fatal("CPU interval crossed incarnation boundary")
	}
	if runtimeMatches(p, m, 156, 160) {
		t.Fatal("stale runtime receipt accepted")
	}
}

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
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

func TestPerformanceBaselinesHistoricalRestarts(t *testing.T) {
	spec := testSpec()
	spec.Startup.ProbeFailurePolicy = startup.Availability
	component := spec.Startup.Components[0]
	component.Replicas, component.Endpoints, component.ReadyEndpoints = 1, "", 0
	spec.Startup.Components = []startup.Component{component}
	n, controller := int32(1), true
	podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: component.Containers[0].Name, Resources: component.Containers[0].Resources}}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: "cp", UID: "deployment", Generation: 1}, Spec: appsv1.DeploymentSpec{Replicas: &n, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "kas"}}, Template: corev1.PodTemplateSpec{Spec: podSpec}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kas", Namespace: "cp", UID: "pod", Labels: map[string]string{"app": "kas"}, OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", UID: "deployment", Controller: &controller}}}, Spec: podSpec, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: component.Containers[0].Name, Ready: true, RestartCount: 5, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.NewTime(time.Now().Add(-time.Hour))}}}}}}
	hc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "hypershift.openshift.io/v1beta1", "kind": "HostedCluster", "metadata": map[string]any{"name": "hc", "namespace": "hc", "uid": "hc-uid", "annotations": map[string]any{startup.ResourceIDAnnotation: spec.Startup.ResourceID}}, "spec": map[string]any{"clusterID": "cluster-id"}}}
	hcp := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "hypershift.openshift.io/v1beta1", "kind": "HostedControlPlane", "metadata": map[string]any{"name": "hcp", "namespace": "cp", "uid": "hcp-uid", "annotations": map[string]any{startup.HostedClusterAnnotation: "hc/hc"}}}}
	csc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "scheduling.hypershift.openshift.io/v1alpha1", "kind": "ClusterSizingConfiguration", "metadata": map[string]any{"name": "cluster"}, "spec": map[string]any{"fixed": true}}}
	clients := startup.Clients{Kubernetes: kubefake.NewClientset(d, p, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cp", UID: "cp-uid"}}), Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{startup.HostedClusters: "HostedClusterList", startup.HostedControlPlanes: "HostedControlPlaneList", startup.SizingConfigurations: "ClusterSizingConfigurationList"}, hc, hcp, csc)}
	identity, err := startup.Discover(t.Context(), clients, spec.Startup.ResourceID, startup.Identity{})
	if err != nil {
		t.Fatal(err)
	}
	g := &safety{}
	if _, err := g.check(t.Context(), clients, spec, identity, true); err != nil {
		t.Fatalf("historical failures rejected recovered initial health: %v", err)
	}
	p.Status.ContainerStatuses[0].RestartCount++
	p.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt = metav1.Now()
	if _, err := clients.Kubernetes.CoreV1().Pods("cp").UpdateStatus(t.Context(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.check(t.Context(), clients, spec, identity, false); err == nil {
		t.Fatal("new OOM was ignored")
	}
}

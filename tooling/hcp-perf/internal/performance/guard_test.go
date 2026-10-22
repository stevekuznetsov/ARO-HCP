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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

func guardFixture(t *testing.T) (startup.Clients, Spec, startup.Identity, *corev1.Pod, *appsv1.Deployment) {
	t.Helper()
	s := testSpec()
	s.Startup.Components = s.Startup.Components[:1]
	c := &s.Startup.Components[0]
	c.Replicas = 1
	c.Endpoints = ""
	c.ReadyEndpoints = 0
	object := func(api, kind, ns, name, uid string, spec map[string]any, annotations map[string]string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": api, "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns, "uid": uid}, "spec": spec}}
		o.SetAnnotations(annotations)
		return o
	}
	hc := object("hypershift.openshift.io/v1beta1", "HostedCluster", "hc", "cluster", "hc-uid", map[string]any{"clusterID": "cluster-id"}, map[string]string{startup.ResourceIDAnnotation: s.Startup.ResourceID})
	hcp := object("hypershift.openshift.io/v1beta1", "HostedControlPlane", "control", "cluster", "hcp-uid", map[string]any{"clusterID": "cluster-id"}, map[string]string{startup.HostedClusterAnnotation: "hc/cluster"})
	candidate := object("scheduling.hypershift.openshift.io/v1alpha1", "ClusterSizingConfiguration", "", "cluster", "candidate", map[string]any{"fixed": true}, nil)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: "control", UID: "deploy", Generation: 1}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "kas"}}, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: c.Name, Resources: c.Containers[0].Resources}}}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	set := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "control", UID: "rs", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: ptr.To(true)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "control", UID: "pod-1", Labels: map[string]string{"app": "kas"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs", UID: "rs", Controller: ptr.To(true)}}}, Spec: *d.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: c.Name}}}}
	clients := startup.Clients{Kubernetes: kubefake.NewClientset(d, set, pod, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "control", UID: "ns"}}), Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{startup.HostedClusters: "HostedClusterList", startup.HostedControlPlanes: "HostedControlPlaneList", startup.SizingConfigurations: "ClusterSizingConfigurationList"}, hc, hcp, candidate)}
	id, err := startup.Discover(t.Context(), clients, s.Startup.ResourceID, startup.Identity{})
	if err != nil {
		t.Fatal(err)
	}
	return clients, s, id, pod, d
}

func TestExistingCompetitorsMustBeSuspendedAndDrained(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "other-run", Namespace: "capture", Labels: map[string]string{"app.kubernetes.io/managed-by": "hcp-perf"}}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "cpu-burn"}}}}}}
	client := kubefake.NewClientset(job)
	if noCompetitors(t.Context(), client) == nil {
		t.Fatal("accepted unsuspended competitor")
	}
	job.Spec.Suspend = ptr.To(true)
	if _, err := client.BatchV1().Jobs("capture").Update(t.Context(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := noCompetitors(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "capture", Labels: job.Labels}, Spec: job.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if _, err := client.CoreV1().Pods("capture").Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if noCompetitors(t.Context(), client) == nil {
		t.Fatal("accepted lingering CPU load")
	}
}

func TestGuardSoftReadinessAndHardFailures(t *testing.T) {
	for _, mode := range []string{"ready", "initial-controller", "transient", "persistent", "oom", "restarts-across-uid"} {
		t.Run(mode, func(t *testing.T) {
			clients, s, id, pod, d := guardFixture(t)
			g := &safety{}
			if mode == "initial-controller" {
				d.Status.UpdatedReplicas = 0
				_, _ = clients.Kubernetes.AppsV1().Deployments("control").UpdateStatus(t.Context(), d, metav1.UpdateOptions{})
				if _, err := g.check(t.Context(), clients, s, id, true); err == nil {
					t.Fatal("available replicas substituted for controller convergence")
				}
				return
			}
			if _, err := g.check(t.Context(), clients, s, id, true); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "transient", "persistent":
				pod.Status.Conditions = nil
				if mode == "persistent" {
					g.unreadySince = time.Now().Add(-121 * time.Second)
				}
			case "oom":
				pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.Now()}
			case "restarts-across-uid":
				pod.Status.ContainerStatuses[0].RestartCount = 2
				_, _ = clients.Kubernetes.CoreV1().Pods("control").UpdateStatus(t.Context(), pod, metav1.UpdateOptions{})
				if _, err := g.check(t.Context(), clients, s, id, false); err != nil {
					t.Fatal("two restarts hard-stopped", err)
				}
				pod.UID = "pod-2"
				pod.Status.ContainerStatuses[0].RestartCount = 1
			}
			_, _ = clients.Kubernetes.CoreV1().Pods("control").Update(t.Context(), pod, metav1.UpdateOptions{})
			_, err := g.check(t.Context(), clients, s, id, false)
			wantHard := mode == "persistent" || mode == "oom" || mode == "restarts-across-uid"
			if (err != nil) != wantHard {
				t.Fatalf("mode %s: %v", mode, err)
			}
		})
	}
}

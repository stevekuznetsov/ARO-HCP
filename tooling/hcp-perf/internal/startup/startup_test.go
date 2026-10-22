// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package startup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const testResourceID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/customer/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/cluster"

func dynamicClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{HostedClusters: "HostedClusterList", HostedControlPlanes: "HostedControlPlaneList", SizingConfigurations: "ClusterSizingConfigurationList"}, objects...)
}

func object(kind, namespace, name, uid string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "hypershift.openshift.io/v1beta1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": namespace, "uid": uid}, "spec": spec}}
}

func candidateObject() *unstructured.Unstructured {
	value := object("ClusterSizingConfiguration", "", "cluster", "csc-uid", map[string]any{"value": "old"})
	value.SetAPIVersion("scheduling.hypershift.openshift.io/v1alpha1")
	value.SetResourceVersion("7")
	return value
}

func TestDiscoveryExactIdentityAndReplacement(t *testing.T) {
	const clusterID = "140ce363-e35f-4513-b031-cc7e754685b5"
	const csid = "2sr5e1kh4uueg1t09jdnjuphnrlalkbf"
	hc := object("HostedCluster", "hc", "random-name", "hc-uid", map[string]any{"clusterID": clusterID})
	hc.SetAnnotations(map[string]string{ResourceIDAnnotation: testResourceID})
	hc.SetLabels(map[string]string{"api.openshift.com/id": csid})
	hcp := object("HostedControlPlane", "control", "random-name", "hcp-uid", map[string]any{"clusterID": clusterID})
	hcp.SetLabels(map[string]string{"api.openshift.com/id": csid})
	hcp.SetAnnotations(map[string]string{HostedClusterAnnotation: "hc/random-name"})
	clients := Clients{Dynamic: dynamicClient(hc, hcp), Kubernetes: kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "control", UID: "namespace-uid"}})}
	identity, err := Discover(t.Context(), clients, testResourceID, Identity{})
	if err != nil || identity.HCPNamespace != "control" || identity.CSID != csid || identity.ClusterID != clusterID {
		t.Fatalf("discover exact annotated identity: %+v %v", identity, err)
	}
	hcp.SetUID("replacement")
	if _, err := clients.Dynamic.Resource(HostedControlPlanes).Namespace("control").Update(t.Context(), hcp, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(t.Context(), clients, testResourceID, identity); err == nil {
		t.Fatal("accepted HCP identity replacement")
	}
	missing, err := Discover(t.Context(), clients, testResourceID+"-other", Identity{})
	if err != nil || missing.HCUID != "" {
		t.Fatal("discovery guessed identity from name")
	}
}

func TestCandidatePreconditionsDryRunAndEmptyGuard(t *testing.T) {
	for _, scenario := range []string{"success", "uid", "rv", "spec", "occupied", "dry-run"} {
		t.Run(scenario, func(t *testing.T) {
			client := dynamicClient(candidateObject())
			candidate := Candidate{Kubeconfig: "/external/kubeconfig", Name: "cluster", UID: "csc-uid", ResourceVersion: "7", ExpectedSpec: json.RawMessage(`{"value":"old"}`), DesiredSpec: json.RawMessage(`{"value":"new"}`)}
			switch scenario {
			case "uid":
				candidate.UID = "wrong"
			case "rv":
				candidate.ResourceVersion = "8"
			case "spec":
				candidate.ExpectedSpec = json.RawMessage(`{"value":"wrong"}`)
			case "occupied":
				_, err := client.Resource(HostedClusters).Namespace("hc").Create(t.Context(), object("HostedCluster", "hc", "existing", "uid", map[string]any{}), metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			writes := 0
			client.PrependReactor("update", "clustersizingconfigurations", func(action clienttesting.Action) (bool, runtime.Object, error) {
				update := action.(clienttesting.UpdateAction)
				if len(action.(clienttesting.UpdateActionImpl).UpdateOptions.DryRun) != 0 {
					if scenario == "dry-run" {
						return true, nil, errors.New("admission rejected")
					}
					return true, update.GetObject().DeepCopyObject(), nil
				}
				writes++
				return false, nil, nil
			})
			err := Apply(t.Context(), Clients{Dynamic: client}, candidate)
			if scenario == "success" {
				if err != nil || writes != 1 {
					t.Fatalf("guarded apply failed: writes=%d err=%v", writes, err)
				}
			} else if err == nil || writes != 0 {
				t.Fatalf("unsafe candidate write: writes=%d err=%v", writes, err)
			}
		})
	}
}

func TestCurrentHealthVersusFreshQualification(t *testing.T) {
	resources := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}}
	component := Component{Kind: "StatefulSet", Name: "etcd", Replicas: 1, Containers: []Container{{Name: "etcd", Resources: resources}}}
	replicas := int32(1)
	controller := true
	set := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "etcd", Namespace: "control", UID: "set"}, Spec: appsv1.StatefulSetSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "etcd"}}, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "etcd", Resources: resources}}}}}, Status: appsv1.StatefulSetStatus{ReadyReplicas: 1}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "etcd-0", Namespace: "control", UID: "pod", Labels: map[string]string{"app": "etcd"}, OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", UID: "set", Controller: &controller}}}, Spec: *set.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "etcd", RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.Now()}}}}}}
	kube := kubefake.NewClientset(set, pod)
	clients := Clients{Kubernetes: kube, Dynamic: dynamicClient(candidateObject())}
	spec := Spec{CandidateName: "cluster", CandidateSpec: json.RawMessage(`{"value":"old"}`), Components: []Component{component}}
	history := healthHistory{restarts: map[string]int32{}, ooms: map[string]string{}, probeEvents: map[string]int32{}}
	identity := Identity{HCPNamespace: "control"}
	first := Sample{At: time.Now()}
	if err := inspect(t.Context(), clients, spec, identity, &first, &history, false); err != nil {
		t.Fatal(err)
	}
	qualify(&first, &history, 5*time.Minute, true)
	if first.NewOOMs != 1 || first.NewRestarts != 1 || first.Healthy {
		t.Fatalf("missed incremental failure: %+v", first)
	}
	second := Sample{At: first.At.Add(time.Minute)}
	if err := inspect(t.Context(), clients, spec, identity, &second, &history, false); err != nil {
		t.Fatal(err)
	}
	qualify(&second, &history, 5*time.Minute, true)
	third := Sample{At: second.At.Add(5 * time.Minute)}
	if err := inspect(t.Context(), clients, spec, identity, &third, &history, false); err != nil {
		t.Fatal(err)
	}
	qualify(&third, &history, 5*time.Minute, true)
	if !third.HealthyWindow || third.FreshQualified || third.NewOOMs != 0 {
		t.Fatalf("historical OOM confused with current health: %+v", third)
	}
	pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("64Mi")
	if _, err := kube.CoreV1().Pods("control").Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	drift := Sample{}
	if err := inspect(t.Context(), clients, spec, identity, &drift, &history, false); err != nil || len(drift.Issues) == 0 {
		t.Fatal("used desired template instead of observed pod resources")
	}
	event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "control", UID: types.UID("probe")}, InvolvedObject: corev1.ObjectReference{UID: "pod"}, Reason: "Unhealthy", Count: 1}
	if _, err := kube.CoreV1().Events("control").Create(t.Context(), event, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	probe := Sample{}
	if err := inspect(t.Context(), clients, spec, identity, &probe, &history, false); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, issue := range probe.Issues {
		found = found || issue == "new probe failure event"
	}
	if !found {
		t.Fatal("probe failure was not observed")
	}
}

func TestExternalKubeconfigRejectsExecutablesAndRelativePaths(t *testing.T) {
	for name, body := range map[string]string{
		"exec":           `{"apiVersion":"v1","kind":"Config","users":[{"name":"user","user":{"exec":{"command":"untrusted","apiVersion":"client.authentication.k8s.io/v1"}}}]}`,
		"relative-token": `{"apiVersion":"v1","kind":"Config","users":[{"name":"user","user":{"tokenFile":"relative-token"}}]}`,
		"relative-ca":    `{"apiVersion":"v1","kind":"Config","clusters":[{"name":"cluster","cluster":{"certificate-authority":"relative-ca"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kubeconfig")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewClients(path); err == nil {
				t.Fatal("unsafe external kubeconfig accepted")
			}
		})
	}
}

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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestHistoricalDeploymentSavedCatalogEvidence(t *testing.T) {
	root := os.Getenv("HCP_PERF_EVIDENCE_ROOT")
	if root == "" {
		t.Skip("set HCP_PERF_EVIDENCE_ROOT to audit the preserved prerequisite evidence offline")
	}
	read := func(name, pin string, out any) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, "intermediate100-prereqs-20260921T1150Z", name))
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != pin {
			t.Fatalf("saved evidence drift: %s", name)
		}
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
	var pods corev1.PodList
	var deployments appsv1.DeploymentList
	var spec Spec
	var sample Sample
	read("cp-pods-final.json", "8b2c88d0fe00bde6096854ecd3fee4250bfe4e13b121d40119521ce31be6bceb", &pods)
	read("cp-deployments-current.json", "4b909fac76540e54e7f31180d22b534e75723519b94d3a5d590523349226ce85", &deployments)
	read("startup-effective-current.json", "01f02f983a8e250938622843a499718abf617e1926f19b42f64b6f41cebdcea4", &spec)
	read("profile-final.json", "d72fdec7412f9841d7b2b705be567346ee1be5f7dbbcbf0da0404f8138d1c6a1", &sample)
	var profile Component
	for _, component := range spec.Components {
		if component.Kind == "Deployment" && component.Name == "certified-operators-catalog" {
			profile = component
		}
	}
	if profile.Name == "" {
		t.Fatal("saved catalog profile missing")
	}
	found := false
	for _, pod := range pods.Items {
		if pod.UID != "c32fa486-093d-4efb-aadb-3f7016b0f7c5" {
			continue
		}
		found = true
		owner := metav1.GetControllerOf(&pod)
		if pod.Status.Phase != corev1.PodSucceeded || owner == nil || owner.Kind != "ReplicaSet" || owner.UID != "48fa9263-ab29-4e3d-8cbc-9bf15460a269" {
			t.Fatal("terminal ownership evidence changed")
		}
		if matchesProfile(pod.Spec, profile) {
			t.Fatal("old Pod unexpectedly matches current profile")
		}
		if pod.Spec.Containers[0].Resources.Requests.Cpu().MilliValue() != 112 || pod.Spec.Containers[0].Resources.Requests.Memory().Value() != 747<<20 {
			t.Fatal("not the expected nonzero-resource historical Pod")
		}
		for _, statuses := range [][]corev1.ContainerStatus{pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses} {
			for _, status := range statuses {
				if status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 || status.RestartCount != 0 {
					t.Fatal("saved terminal status changed")
				}
			}
		}
		if !slices.Equal(sample.Issues, []string{"observed pod resources mismatch: " + pod.Name}) || sample.Coverage.ReadyPods != 66 || sample.NewRestarts != 0 || sample.NewOOMs != 0 {
			t.Fatal("saved native baseline changed")
		}
	}
	if !found {
		t.Fatal("historical Pod missing from saved inventory")
	}
	found = false
	for _, deployment := range deployments.Items {
		if deployment.Name == profile.Name {
			found = true
			if !matchesProfile(deployment.Spec.Template.Spec, profile) || deployment.Status.UpdatedReplicas != 1 || deployment.Status.ReadyReplicas != 1 {
				t.Fatal("saved current Deployment does not match profile/readiness")
			}
		}
	}
	if !found {
		t.Fatal("saved Deployment missing")
	}
	t.Log("Saved raw Pod/profile mismatch confirmed; ReplicaSet object was not captured, so obsolete/zero-replica classification still requires native inspection. No synthetic owner evidence substituted.")
}

func TestHistoricalDeploymentPods(t *testing.T) {
	for _, convergence := range []bool{false, true} {
		for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
			for _, scenario := range []string{
				"obsolete", "zero-resources", "deleting-terminal", "hash-only-difference", "current-terminal", "current-terminal-drift",
				"missing-rs", "foreign-rs", "non-controller-rs", "foreign-pod", "non-controller-pod", "selector-mismatch",
				"rs-still-desired", "rs-replicas-unknown", "pending", "running", "unknown", "deleting-active", "crash-loop",
				"missing-ready", "current-image-drift", "template-drift", "replicas", "unconverged", "endpoints",
				"restart", "oom", "probe-kill", "disabled",
			} {
				t.Run(string(phase)+"/"+scenario+map[bool]string{true: "/snapshot", false: "/observe"}[convergence], func(t *testing.T) {
					controller, one, zero := true, int32(1), int32(0)
					image := "registry/image@sha256:" + strings.Repeat("a", 64)
					profile := Component{Kind: "Deployment", Name: "catalog", Replicas: 1, Endpoints: "catalog", ReadyEndpoints: 1, Containers: []Container{{Name: "registry", Image: image, Resources: testResources()}}}
					template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "catalog"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "registry", Image: image, Resources: testResources()}}}}
					deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "catalog", Namespace: "cp", UID: "deployment", Generation: 2}, Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "catalog"}}, Template: template}, Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
					currentRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "current", Namespace: "cp", UID: "current-rs", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "catalog", UID: deployment.UID, Controller: &controller}}}, Spec: appsv1.ReplicaSetSpec{Replicas: &one, Template: *template.DeepCopy()}}
					oldRS := currentRS.DeepCopy()
					oldRS.Name, oldRS.UID, oldRS.Spec.Replicas = "old", "old-rs", &zero
					oldRS.Spec.Template.Spec.Containers[0].Image = "registry/image@sha256:" + strings.Repeat("b", 64)
					ready := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: "cp", UID: "ready", Labels: map[string]string{"app": "catalog"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "current", UID: currentRS.UID, Controller: &controller}}}, Spec: *template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
					old := ready.DeepCopy()
					old.Name, old.UID, old.OwnerReferences[0].Name, old.OwnerReferences[0].UID = "historical", "historical", "old", oldRS.UID
					old.Spec, old.Status.Phase = *oldRS.Spec.Template.Spec.DeepCopy(), phase
					// Even an inconsistent Ready condition must not count a terminal Pod.
					endpoint := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "catalog", Namespace: "cp"}, Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "10.0.0.1"}}}}}
					want := ""
					switch scenario {
					case "zero-resources":
						old.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
					case "deleting-terminal":
						now := metav1.Now()
						old.DeletionTimestamp = &now
					case "hash-only-difference":
						oldRS.Spec.Template = *template.DeepCopy()
						oldRS.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "old-hash"
						want = "terminal pod in current deployment: historical"
					case "current-terminal", "current-terminal-drift":
						old.OwnerReferences[0].Name, old.OwnerReferences[0].UID = currentRS.Name, currentRS.UID
						if scenario == "current-terminal" {
							old.Spec = *template.Spec.DeepCopy()
						}
						want = "terminal pod in current deployment: historical"
					case "missing-rs", "foreign-rs", "non-controller-rs", "foreign-pod", "non-controller-pod", "selector-mismatch":
						want = "undeclared pod: historical"
						if scenario == "foreign-rs" {
							oldRS.OwnerReferences[0].UID = "other-deployment"
						}
						if scenario == "non-controller-rs" {
							oldRS.OwnerReferences[0].Controller = nil
						}
						if scenario == "foreign-pod" {
							old.OwnerReferences[0].UID = "unknown-rs"
						}
						if scenario == "non-controller-pod" {
							old.OwnerReferences[0].Controller = nil
						}
						if scenario == "selector-mismatch" {
							old.Labels["app"] = "other"
						}
					case "rs-still-desired":
						oldRS.Spec.Replicas = &one
						want = "observed pod resources mismatch: historical"
					case "rs-replicas-unknown":
						oldRS.Spec.Replicas = nil
						want = "observed pod resources mismatch: historical"
					case "pending", "running", "unknown", "deleting-active", "crash-loop":
						old.Status.Phase = corev1.PodRunning
						if scenario == "pending" {
							old.Status.Phase = corev1.PodPending
						}
						if scenario == "unknown" {
							old.Status.Phase = corev1.PodUnknown
						}
						if scenario == "deleting-active" {
							now := metav1.Now()
							old.DeletionTimestamp = &now
						}
						if scenario == "crash-loop" {
							old.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "registry", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
						}
						want = "observed pod resources mismatch: historical"
					case "missing-ready":
						ready.Status.Conditions = nil
						want = "observed pods not ready: Deployment/catalog"
					case "current-image-drift":
						ready.Spec.Containers[0].Image = "wrong"
						want = "observed pod resources mismatch: ready"
					case "template-drift":
						deployment.Spec.Template.Spec.Containers[0].Image = "wrong"
						want = "template resources mismatch: Deployment/catalog"
					case "replicas":
						deployment.Status.AvailableReplicas = 0
						want = "replicas not ready: Deployment/catalog"
					case "unconverged":
						if !convergence {
							return
						}
						deployment.Status.UpdatedReplicas = 0
						want = "controller not converged: Deployment/catalog"
					case "endpoints":
						endpoint.Subsets = nil
						want = "endpoints not ready: Deployment/catalog"
					case "restart":
						old.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "registry", RestartCount: 2}}
					case "oom":
						old.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.Now()}}}}
					case "probe-kill":
						want = "new probe-triggered container kill"
					case "disabled":
						profile.Lifecycle = &Lifecycle{Mode: "disabled", Reason: "test"}
						want = "observed pod resources mismatch: historical"
					}
					objects := []runtime.Object{deployment, currentRS, ready, old, endpoint}
					if scenario != "missing-rs" {
						objects = append(objects, oldRS)
					}
					if scenario == "probe-kill" {
						objects = append(objects, &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "kill", Namespace: "cp", UID: "kill"}, InvolvedObject: corev1.ObjectReference{UID: old.UID}, Reason: "Killing", Message: "failed liveness probe"})
					}
					clients := Clients{Kubernetes: kubefake.NewClientset(objects...), Dynamic: dynamicClient(candidateObject())}
					spec := Spec{CandidateName: "cluster", CandidateSpec: json.RawMessage(`{"value":"old"}`), Components: []Component{profile}}
					history := healthHistory{restarts: map[string]int32{}, ooms: map[string]string{}, probeEvents: map[string]int32{}}
					sample := Sample{}
					if err := inspect(t.Context(), clients, spec, Identity{HCPNamespace: "cp"}, &sample, &history, convergence); err != nil {
						t.Fatal(err)
					}
					qualify(&sample, &history, 0, true)
					if want != "" && !slices.Contains(sample.Issues, want) {
						t.Fatalf("missing %q: %+v", want, sample)
					}
					if want == "" && len(sample.Issues) != 0 {
						t.Fatalf("unexpected issues: %+v", sample)
					}
					wantHealthy := want == "" && scenario != "restart" && scenario != "oom"
					if sample.Healthy != wantHealthy {
						t.Fatalf("health mismatch: %+v", sample)
					}
					if scenario == "restart" && sample.NewRestarts != 2 {
						t.Fatal("historical restart lost")
					}
					if scenario == "oom" && sample.NewOOMs != 1 {
						t.Fatal("historical OOM lost")
					}
					if scenario == "crash-loop" && !slices.Contains(sample.Issues, "container crash loop: historical/registry") {
						t.Fatal("active crash loop lost")
					}
					if wantHealthy && (sample.Coverage.ReadyPods != 1 || sample.Coverage.ObservedWorkloads != 1 || sample.Coverage.RequiredPods != 1 || sample.Coverage.ReadyEndpoints != 1) {
						t.Fatalf("historical Pod changed coverage: %+v", sample.Coverage)
					}
					if scenario == "restart" || scenario == "oom" {
						second := Sample{}
						if err := inspect(t.Context(), clients, spec, Identity{HCPNamespace: "cp"}, &second, &history, convergence); err != nil {
							t.Fatal(err)
						}
						if second.NewRestarts != 0 || second.NewOOMs != 0 || !history.violated {
							t.Fatal("history deduplication/fresh qualification changed")
						}
					}
					for _, action := range clients.Kubernetes.(*kubefake.Clientset).Actions() {
						if action.GetVerb() != "get" && action.GetVerb() != "list" {
							t.Fatalf("inspection mutated evidence: %+v", action)
						}
					}
				})
			}
		}
	}
}

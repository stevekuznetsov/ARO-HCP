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
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func testResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}}
}

func TestHistoricalJobProfile(t *testing.T) {
	for _, scenario := range []string{"unchanged", "replaced-job", "replaced-owner", "not-complete", "image-drift"} {
		t.Run(scenario, func(t *testing.T) {
			controller := true
			cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "profiles", Namespace: "cp", UID: "cron"}, Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: "new"}}}}}}}}
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "profiles-old", Namespace: "cp", UID: "job", OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: "profiles", UID: "cron", Controller: &controller}}}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: "old"}}}}}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
			switch scenario {
			case "replaced-job":
				job.UID = "replacement"
			case "replaced-owner":
				job.OwnerReferences[0].UID = "replacement"
			case "not-complete":
				job.Status.Conditions = nil
			case "image-drift":
				job.Spec.Template.Spec.Containers[0].Image = "other"
			}
			profiles := []Component{
				{Kind: "CronJob", Name: "profiles", Lifecycle: &Lifecycle{Mode: "present", Reason: "current template"}, Containers: []Container{{Name: "worker", Image: "new"}}},
				{Kind: "Job", Name: "profiles-old", ExpectedUID: "job", ExpectedOwnerUID: "cron", Lifecycle: &Lifecycle{Mode: "completed", Reason: "frozen historical run"}, Containers: []Container{{Name: "worker", Image: "old"}}},
			}
			sample, history := Sample{}, healthHistory{}
			if err := inspectBatch(t.Context(), Clients{Kubernetes: kubefake.NewClientset(cron, job)}, profiles, "cp", nil, map[string]bool{}, &sample, &history); err != nil {
				t.Fatal(err)
			}
			if (len(sample.Issues) != 0) != (scenario != "unchanged") {
				t.Fatalf("unexpected historical assessment: %+v", sample)
			}
		})
	}
}

func TestBatchLifecyclesAndOwnership(t *testing.T) {
	for _, scenario := range []string{"completed", "pending", "failed", "failed-pod", "completed-pod-image-drift", "optional-absent", "cron-generated", "cron-disabled", "disabled-active", "wrong-owner", "resource-drift", "standalone", "standalone-unknown"} {
		t.Run(scenario, func(t *testing.T) {
			controller, suspended := true, true
			profile := Component{Kind: "Job", Name: "featuregate-generator", Lifecycle: &Lifecycle{Mode: "completed", Reason: "bootstrap job"}, Containers: []Container{{Name: "worker"}}}
			template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}}}
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: profile.Name, Namespace: "cp", UID: "job"}, Spec: batchv1.JobSpec{Template: template}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
			pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "generated-pod", Namespace: "cp", UID: "pod", OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}}}, Spec: template.Spec, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
			var objects []runtime.Object
			expectIssue := false
			switch scenario {
			case "pending":
				job.Status.Conditions = nil
				expectIssue = true
			case "failed":
				job.Status.Conditions[0].Type = batchv1.JobFailed
				expectIssue = true
			case "failed-pod":
				pod.Status.Phase = corev1.PodFailed
				expectIssue = true
			case "completed-pod-image-drift":
				profile.Containers[0].Image = "current"
				job.Spec.Template.Spec.Containers = []corev1.Container{{Name: "worker", Image: "current"}}
				pod.Spec.Containers = []corev1.Container{{Name: "worker", Image: "old"}}
				expectIssue = true
			case "optional-absent":
				profile.Lifecycle.Mode = "optional"
			case "cron-generated", "cron-disabled", "disabled-active", "wrong-owner", "resource-drift":
				profile.Kind, profile.Name, profile.Lifecycle.Mode = "CronJob", "olm-collect-profiles", "present"
				cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: profile.Name, Namespace: "cp", UID: "cron"}, Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: template}}}}
				job.Name = "olm-collect-profiles-12345"
				job.OwnerReferences = []metav1.OwnerReference{{Kind: "CronJob", Name: cron.Name, UID: cron.UID, Controller: &controller}}
				if scenario == "cron-disabled" || scenario == "disabled-active" {
					profile.Lifecycle.Mode = "disabled"
					cron.Spec.Suspend = &suspended
				}
				if scenario == "disabled-active" {
					job.Status.Conditions = nil
					job.Status.Active = 1
					expectIssue = true
				}
				if scenario == "wrong-owner" {
					job.OwnerReferences[0].UID = "replaced-cron"
					expectIssue = true
				}
				if scenario == "resource-drift" {
					job.Spec.Template.Spec.Containers = []corev1.Container{{Name: "unexpected"}}
					expectIssue = true
				}
				objects = append(objects, cron)
			case "standalone", "standalone-unknown":
				profile.Kind, profile.Name = "Pod", pod.Name
				pod.OwnerReferences = nil
				if scenario == "standalone-unknown" {
					profile.Name = "another-pod"
					expectIssue = true
				}
			}
			pods := []corev1.Pod{pod}
			if scenario == "optional-absent" {
				pods = nil
			} else if !strings.HasPrefix(scenario, "standalone") {
				objects = append(objects, job)
			}
			clients := Clients{Kubernetes: kubefake.NewClientset(objects...)}
			matched := map[string]bool{}
			sample, history := Sample{}, healthHistory{}
			if err := inspectBatch(t.Context(), clients, []Component{profile}, "cp", pods, matched, &sample, &history); err != nil {
				t.Fatal(err)
			}
			if (len(sample.Issues) > 0) != expectIssue {
				t.Fatalf("unexpected lifecycle assessment: %+v", sample)
			}
			if !expectIssue && len(pods) > 0 && !matched["pod"] {
				t.Fatal("valid UID-owned batch/standalone pod not covered")
			}
			if scenario == "wrong-owner" && matched["pod"] {
				t.Fatal("accepted generated name without CronJob UID ownership")
			}
		})
	}
}

func TestObserverArmsOnlyAfterEmptyDiscoveryAndCandidateRead(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "existing-target"}[existing], func(t *testing.T) {
			objects := []runtime.Object{candidateObject()}
			if existing {
				hc := object("HostedCluster", "hc", "cluster", "hc-uid", map[string]any{"clusterID": "cs-id"})
				hc.SetAnnotations(map[string]string{ResourceIDAnnotation: testResourceID})
				objects = append(objects, hc)
			}
			client := dynamicClient(objects...)
			listed, candidateRead := false, false
			client.PrependReactor("list", "hostedclusters", func(clienttesting.Action) (bool, runtime.Object, error) { listed = true; return false, nil, nil })
			client.PrependReactor("get", "clustersizingconfigurations", func(clienttesting.Action) (bool, runtime.Object, error) { candidateRead = true; return false, nil, nil })
			spec := Spec{Kubeconfig: "/external/kubeconfig", ResourceID: testResourceID, CandidateName: "cluster", CandidateSpec: json.RawMessage(`{"value":"old"}`), MaxDuration: 5 * time.Minute, HealthyWindow: 5 * time.Minute, PollInterval: 5 * time.Second}
			// Validation requires the core serving inventory, even while absent.
			for _, key := range []string{"kube-apiserver", "openshift-apiserver", "packageserver", "etcd"} {
				kind := "Deployment"
				if key == "etcd" {
					kind = "StatefulSet"
				}
				spec.Components = append(spec.Components, Component{Kind: kind, Name: key, Replicas: 3, Endpoints: key, ReadyEndpoints: 3, Containers: []Container{{Name: key, Resources: testResources()}}})
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			armed := false
			_, err := Observe(ctx, Clients{Dynamic: client, Kubernetes: kubefake.NewClientset()}, spec, t.TempDir(), func() error {
				if !listed || !candidateRead {
					t.Fatal("armed before actual Kubernetes preflight")
				}
				armed = true
				cancel()
				return nil
			})
			if err == nil || armed == existing {
				t.Fatalf("unexpected readiness: armed=%t existing=%t err=%v", armed, existing, err)
			}
		})
	}
}

func TestDiscoveryAnnotationAndNamespaceFence(t *testing.T) {
	for _, scenario := range []string{"annotation-only", "opposite-case", "missing-csid", "conflicting-csid", "missing-reference", "wrong-reference", "conflicting-cluster-id", "missing-namespace-uid", "replaced-namespace"} {
		t.Run(scenario, func(t *testing.T) {
			hc := object("HostedCluster", "hc", "cluster", "hc-uid", map[string]any{"clusterID": "cs-id"})
			hc.SetLabels(map[string]string{"api.openshift.com/id": "cs-id"})
			hc.SetAnnotations(map[string]string{ResourceIDAnnotation: strings.ToUpper(testResourceID)})
			hcp := object("HostedControlPlane", "cp", "unrelated-name", "hcp-uid", map[string]any{})
			hcp.SetAnnotations(map[string]string{HostedClusterAnnotation: "hc/cluster"})
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cp", UID: "namespace-uid"}}
			switch scenario {
			case "missing-csid":
				hc.SetLabels(nil)
			case "conflicting-csid":
				hcp.SetLabels(map[string]string{"api.openshift.com/id": "other-csid"})
			case "missing-reference":
				hcp.SetAnnotations(nil)
				hcp.Object["spec"] = map[string]any{"clusterID": "cs-id"}
			case "wrong-reference":
				hcp.SetAnnotations(map[string]string{HostedClusterAnnotation: "other/cluster"})
			case "conflicting-cluster-id":
				hcp.Object["spec"] = map[string]any{"clusterID": "wrong"}
			case "missing-namespace-uid":
				namespace.UID = ""
			}
			clients := Clients{Dynamic: dynamicClient(hc, hcp), Kubernetes: kubefake.NewClientset(namespace)}
			previous := Identity{}
			if scenario == "replaced-namespace" {
				previous.NamespaceUID = "old-uid"
			}
			identity, err := Discover(t.Context(), clients, testResourceID, previous)
			switch scenario {
			case "missing-csid":
				if err != nil || identity.CSID != "" || identity.HCPUID == "" {
					t.Fatal("missing CSID must remain unknown, not copied from UUID")
				}
			case "annotation-only", "opposite-case":
				if err != nil || identity.HCPUID != "hcp-uid" || identity.CSID != "cs-id" {
					t.Fatalf("annotation discovery failed: %+v %v", identity, err)
				}
			case "missing-reference", "wrong-reference":
				if err != nil || identity.HCPUID != "" {
					t.Fatal("used clusterID/naming fallback without HC annotation")
				}
			default:
				if err == nil {
					t.Fatal("identity conflict was accepted")
				}
			}
		})
	}
}

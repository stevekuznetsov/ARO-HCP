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

package capture

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

func TestAdversariesExcludedFromCapture(t *testing.T) {
	client, s := started(t)
	makeReady(t, client, s)
	adversaryLabels := map[string]string{ManagedByLabel: "hcp-perf", SessionIDLabel: s.ID, ComponentLabel: "adversary"}
	for _, pod := range []*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "adversary", Labels: adversaryLabels}},
		{ObjectMeta: metav1.ObjectMeta{Name: "unrelated"}},
		// Matching labels alone must not make a pod part of the capture session.
		{ObjectMeta: metav1.ObjectMeta{Name: "wrong-controller", Labels: labels(s.ID), OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "apps/v1", Kind: "StatefulSet", Name: resourceName, UID: "old-statefulset", Controller: ptr(true)},
		}}},
	} {
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		if _, err := client.CoreV1().Pods(s.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(s.Namespace).Create(t.Context(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "adversary", Labels: adversaryLabels},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimLost},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	svc, err := client.CoreV1().Services(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if klabels.SelectorFromSet(svc.Spec.Selector).Matches(klabels.Set(adversaryLabels)) || svc.Spec.Selector[ComponentLabel] != "capture" {
		t.Fatal("Service selects adversary pods")
	}
	observed, err := Status(t.Context(), client, s.ID)
	if err != nil || observed.Phase != PhaseReady || len(observed.Pods) != 1 || len(observed.Storage) != 1 || observed.Pods[0].Name != "prometheus-0" {
		t.Fatalf("other workloads affected capture readiness: %+v %v", observed, err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "persistentvolumeclaims" {
			selector := action.(clienttesting.ListAction).GetListRestrictions().Labels
			if selector.Empty() || selector.Matches(klabels.Set(adversaryLabels)) || !selector.Matches(klabels.Set(labels(s.ID))) {
				t.Fatalf("capture list not filtered: %v", selector)
			}
		}
		if action.GetVerb() == "list" && action.GetResource().Resource == "pods" && !action.(clienttesting.ListAction).GetListRestrictions().Labels.Empty() {
			t.Fatal("pod list must not hide capture label drift")
		}
	}
	if _, err := Stop(t.Context(), client, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.CoreV1().Pods(s.Namespace).Delete(t.Context(), "prometheus-0", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	observed, err = Status(t.Context(), client, s.ID)
	if err != nil || observed.Phase != PhaseStopped || len(observed.Pods) != 0 {
		t.Fatalf("adversaries prevented capture stopped status: %+v %v", observed, err)
	}
	if err := Cleanup(t.Context(), client, s.ID, true); err == nil {
		t.Fatal("cleanup deleted active adversaries")
	}
}

func TestPodControllerIdentity(t *testing.T) {
	for _, change := range []string{"uid", "kind", "apiVersion", "name", "not controller", "missing"} {
		t.Run(change, func(t *testing.T) {
			client, s := started(t)
			makeReady(t, client, s)
			pod, err := client.CoreV1().Pods(s.Namespace).Get(t.Context(), "prometheus-0", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			owner := &pod.OwnerReferences[0]
			switch change {
			case "uid":
				owner.UID = "replacement"
			case "kind":
				owner.Kind = "Job"
			case "apiVersion":
				owner.APIVersion = "batch/v1"
			case "name":
				owner.Name = "other"
			case "not controller":
				owner.Controller = ptr(false)
			case "missing":
				pod.OwnerReferences = nil
			}
			if _, err := client.CoreV1().Pods(s.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			observed, err := Status(t.Context(), client, s.ID)
			if err == nil || observed.Phase != PhaseFailed {
				t.Fatalf("expected collector with wrong ownership was ignored: %+v %v", observed, err)
			}
			stopping, err := Stop(t.Context(), client, s.ID)
			if err == nil || stopping.DesiredReplicas != 0 || stopping.Phase != PhaseStopping {
				t.Fatalf("Stop must scale down despite collector ownership diagnostic failure: %+v %v", stopping, err)
			}
			observed, err = Status(t.Context(), client, s.ID)
			if err == nil || observed.Phase != PhaseFailed {
				t.Fatalf("live collector with wrong ownership allowed stopped status: %+v %v", observed, err)
			}
		})
	}
}

func TestCaptureLabelDriftCannotReportStopped(t *testing.T) {
	for _, change := range []string{"all removed", "session changed", "component changed", "managed-by removed", "other name same UID"} {
		t.Run(change, func(t *testing.T) {
			client, s := started(t)
			makeReady(t, client, s)
			pod, err := client.CoreV1().Pods(s.Namespace).Get(t.Context(), "prometheus-0", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "session changed":
				pod.Labels[SessionIDLabel] = "other"
			case "component changed":
				pod.Labels[ComponentLabel] = "adversary"
			case "managed-by removed":
				delete(pod.Labels, ManagedByLabel)
			default:
				pod.Labels = nil
			}
			if change == "other name same UID" {
				if err := client.CoreV1().Pods(s.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				pod.Name = "prometheus-1"
				if _, err := client.CoreV1().Pods(s.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := client.CoreV1().Pods(s.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			observed, err := Status(t.Context(), client, s.ID)
			if err == nil || observed.Phase != PhaseFailed {
				t.Fatalf("label drift allowed readiness: %+v %v", observed, err)
			}
			stopping, err := Stop(t.Context(), client, s.ID)
			if err == nil || stopping.Phase != PhaseStopping || stopping.DesiredReplicas != 0 {
				t.Fatalf("label drift blocked scale-down or hid diagnostic failure: %+v %v", stopping, err)
			}
			sts, err := client.AppsV1().StatefulSets(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
			if err != nil || *sts.Spec.Replicas != 0 {
				t.Fatalf("Stop did not actually scale down: %+v %v", sts, err)
			}
			observed, err = WaitStopped(t.Context(), client, s.ID)
			if err == nil || observed.Phase != PhaseFailed {
				t.Fatalf("live collector hidden after scale-down: %+v %v", observed, err)
			}
		})
	}
}

func TestStopScalesBeforeDiagnosticReads(t *testing.T) {
	for _, resource := range []string{"events", "clusterroles", "clusterrolebindings"} {
		t.Run(resource, func(t *testing.T) {
			client := testClient()
			spec := testSpec()
			spec.KubernetesDiscovery = true
			s, err := Start(t.Context(), client, spec, testConfig)
			if err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			client.PrependReactor("*", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, resourceName, errors.New("SECRET"))
			})
			observed, err := Stop(t.Context(), client, s.ID)
			if err == nil || strings.Contains(err.Error(), "SECRET") || observed.DesiredReplicas != 0 || observed.Phase != PhaseStopping || len(observed.Inventory) != 2 {
				t.Fatalf("diagnostic denial hid scale-down result: %+v %v", observed, err)
			}
			actions := client.Actions()
			if len(actions) < 3 || actions[0].GetVerb() != "get" || actions[0].GetResource().Resource != "namespaces" || actions[1].GetVerb() != "get" || actions[1].GetResource().Resource != "statefulsets" || actions[2].GetVerb() != "patch" || actions[2].GetResource().Resource != "statefulsets" {
				t.Fatalf("diagnostic requests preceded scale-down: %+v", actions)
			}
			sts, err := client.AppsV1().StatefulSets(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
			if err != nil || *sts.Spec.Replicas != 0 {
				t.Fatalf("diagnostic denial prevented scale-down: %+v %v", sts, err)
			}
		})
	}
}

func TestCleanupRequiresQuiescentNamespace(t *testing.T) {
	for _, phase := range []corev1.PodPhase{"", corev1.PodPending, corev1.PodRunning, corev1.PodUnknown, corev1.PodSucceeded, corev1.PodFailed} {
		t.Run(string(phase), func(t *testing.T) {
			client, s := started(t)
			if err := Cleanup(t.Context(), client, s.ID, true); err == nil {
				t.Fatal("cleanup accepted running capture, even without pods")
			}
			if _, err := Stop(t.Context(), client, s.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := client.CoreV1().Pods(s.Namespace).Create(t.Context(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "adversary", DeletionTimestamp: ptr(metav1.Now())},
				Status:     corev1.PodStatus{Phase: phase},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			err := Cleanup(t.Context(), client, s.ID, true)
			terminal := phase == corev1.PodSucceeded || phase == corev1.PodFailed
			if (err == nil) != terminal {
				t.Fatalf("incorrect cleanup decision for phase %q: %v", phase, err)
			}
			for _, action := range client.Actions() {
				if !terminal && action.GetVerb() == "delete" {
					t.Fatal("refused cleanup still deleted resources")
				}
			}
		})
	}
	client, s := started(t)
	if _, err := Stop(t.Context(), client, s.ID); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("SECRET")
	})
	if err := Cleanup(t.Context(), client, s.ID, true); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("cleanup did not fail closed on pod list error: %v", err)
	}
}

func TestCleanupRequiresQuiescentJobs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		suspend   *bool
		condition batchv1.JobCondition
		livePod   bool
		allowed   bool
	}{
		{name: "pending without pods"},
		{name: "explicitly unsuspended", suspend: ptr(false)},
		{name: "complete false", condition: batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionFalse}},
		{name: "failed unknown", condition: batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionUnknown}},
		{name: "failure target not terminal", condition: batchv1.JobCondition{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue}},
		{name: "success criteria not terminal", condition: batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue}},
		{name: "complete", condition: batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}, allowed: true},
		{name: "failed", condition: batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}, allowed: true},
		{name: "suspended", suspend: ptr(true), allowed: true},
		{name: "suspended with terminating pod", suspend: ptr(true), livePod: true},
		{name: "terminal with live pod", condition: batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}, livePod: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, s := started(t)
			if _, err := Stop(t.Context(), client, s.ID); err != nil {
				t.Fatal(err)
			}
			// Unlabelled Jobs must be checked, not just capture/adversary-owned Jobs.
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "recorder"}, Spec: batchv1.JobSpec{Suspend: tc.suspend}}
			if tc.condition.Type != "" {
				job.Status.Conditions = []batchv1.JobCondition{tc.condition}
			}
			if _, err := client.BatchV1().Jobs(s.Namespace).Create(t.Context(), job, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if tc.livePod {
				if _, err := client.CoreV1().Pods(s.Namespace).Create(t.Context(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "recorder-pod", DeletionTimestamp: ptr(metav1.Now())},
					Status:     corev1.PodStatus{Phase: corev1.PodRunning},
				}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			client.ClearActions()
			err := Cleanup(t.Context(), client, s.ID, true)
			if (err == nil) != tc.allowed {
				t.Fatalf("incorrect Job cleanup decision: %v", err)
			}
			listed := false
			for _, action := range client.Actions() {
				if action.GetVerb() == "list" && action.GetResource().Resource == "jobs" {
					listed = true
					if !action.(clienttesting.ListAction).GetListRestrictions().Labels.Empty() || action.GetNamespace() != s.Namespace {
						t.Fatal("cleanup must inspect all Jobs in the session namespace")
					}
				}
				if !tc.allowed && action.GetVerb() == "delete" {
					t.Fatal("refused cleanup deleted resources")
				}
			}
			if !listed {
				t.Fatal("cleanup did not list Jobs")
			}
		})
	}
	t.Run("list error", func(t *testing.T) {
		client, s := started(t)
		if _, err := Stop(t.Context(), client, s.ID); err != nil {
			t.Fatal(err)
		}
		client.ClearActions()
		client.PrependReactor("list", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "", errors.New("SECRET"))
		})
		if err := Cleanup(t.Context(), client, s.ID, true); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("cleanup did not fail closed on Job list denial: %v", err)
		}
		for _, action := range client.Actions() {
			if action.GetVerb() == "delete" {
				t.Fatal("Job list denial did not prevent deletion")
			}
		}
	})
}

func TestRecorderPVCExcludedFromStatus(t *testing.T) {
	client, s := started(t)
	makeReady(t, client, s)
	if _, err := client.CoreV1().PersistentVolumeClaims(s.Namespace).Create(t.Context(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "recorder", Labels: map[string]string{ManagedByLabel: "hcp-perf", SessionIDLabel: s.ID, ComponentLabel: "recorder"}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimLost},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	observed, err := Status(t.Context(), client, s.ID)
	if err != nil || observed.Phase != PhaseReady || len(observed.Storage) != 1 || observed.Storage[0].Name != resourceName {
		t.Fatalf("recorder PVC affected capture status: %+v %v", observed, err)
	}
	for _, object := range observed.Inventory {
		if object.Name == "recorder" {
			t.Fatal("recorder PVC included in capture inventory")
		}
	}
}

func TestKubernetesDiscovery(t *testing.T) {
	client := testClient()
	spec := testSpec()
	spec.KubernetesDiscovery = true
	s, err := Start(t.Context(), client, spec, "scrape_configs:\n- job_name: nodes\n  kubernetes_sd_configs:\n  - role: node\n")
	if err != nil || len(s.Inventory) != 8 {
		t.Fatalf("discovery start failed: %+v %v", s, err)
	}
	sts, err := client.AppsV1().StatefulSets(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sa, err := client.CoreV1().ServiceAccounts(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !*sts.Spec.Template.Spec.AutomountServiceAccountToken || !*sa.AutomountServiceAccountToken || sts.Spec.Template.Spec.ServiceAccountName != sa.Name {
		t.Fatal("discovery token mounting not enabled")
	}
	role, err := client.RbacV1().ClusterRoles().Get(t.Context(), s.Namespace+"-prometheus", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"nodes", "services", "endpoints", "pods"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"nodes/metrics"}, Verbs: []string{"get"}},
		{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}},
		{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"get", "list", "watch"}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("discovery permissions exceeded read-only allowlist: %+v", role.Rules)
	}
	binding, err := client.RbacV1().ClusterRoleBindings().Get(t.Context(), role.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}) || !reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: resourceName, Namespace: s.Namespace}}) {
		t.Fatalf("binding grants access to wrong subjects or role: %+v", binding)
	}
	for _, object := range []metav1.Object{role, binding} {
		owner := metav1.GetControllerOf(object)
		if !owned(object, s.ID) || object.GetNamespace() != "" || owner == nil || owner.APIVersion != "v1" || owner.Kind != "Namespace" || owner.Name != s.Namespace || string(owner.UID) != "namespaces-"+s.Namespace {
			t.Fatalf("RBAC missing namespace GC owner: %+v", object)
		}
	}
	observed, err := Status(t.Context(), client, s.ID)
	if err != nil || len(observed.Inventory) != 8 || len(observed.Missing) != 0 {
		t.Fatalf("RBAC absent from status: %+v %v", observed, err)
	}
	cm, err := client.CoreV1().ConfigMaps(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var persisted Spec
	if err := json.Unmarshal([]byte(cm.Data["spec.json"]), &persisted); err != nil || !persisted.KubernetesDiscovery {
		t.Fatalf("discovery option not persisted: %+v %v", persisted, err)
	}
	if _, err := Stop(t.Context(), client, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(t.Context(), client, s.ID, true); err != nil {
		t.Fatal(err)
	}
	// The fake does not implement garbage collection. Owner references above
	// verify that real Kubernetes GC will remove these cluster-scoped objects.
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource != "namespaces" {
			t.Fatal("cleanup should delegate RBAC deletion to namespace owner GC")
		}
	}
}

func TestDiscoveryRBACFailures(t *testing.T) {
	for _, resource := range []string{"clusterroles", "clusterrolebindings"} {
		t.Run(resource, func(t *testing.T) {
			client := testClient()
			client.PrependReactor("create", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("SECRET")
			})
			spec := testSpec()
			spec.KubernetesDiscovery = true
			s, err := Start(t.Context(), client, spec, testConfig)
			want := 3
			if resource == "clusterrolebindings" {
				want = 4
			}
			if err == nil || strings.Contains(err.Error(), "SECRET") || len(s.Inventory) != want {
				t.Fatalf("incorrect RBAC failure inventory: %+v %v", s, err)
			}
			observed, err := Status(t.Context(), client, s.ID)
			if err != nil || observed.Phase != PhaseFailed || len(observed.Inventory) != want || len(observed.Missing) != 8-want {
				t.Fatalf("partial RBAC not observable: %+v %v", observed, err)
			}
		})
	}
	for _, resource := range []string{"clusterroles", "clusterrolebindings"} {
		t.Run("ownership-"+resource, func(t *testing.T) {
			client := testClient()
			spec := testSpec()
			spec.KubernetesDiscovery = true
			s, err := Start(t.Context(), client, spec, testConfig)
			if err != nil {
				t.Fatal(err)
			}
			client.PrependReactor("get", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				metadata := metav1.ObjectMeta{Name: s.Namespace + "-prometheus", Labels: labels(s.ID), OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "v1", Kind: "Namespace", Name: s.Namespace, UID: "old-namespace", Controller: ptr(true)},
				}}
				if resource == "clusterroles" {
					return true, &rbacv1.ClusterRole{ObjectMeta: metadata}, nil
				}
				return true, &rbacv1.ClusterRoleBinding{ObjectMeta: metadata}, nil
			})
			if _, err := Status(t.Context(), client, s.ID); err == nil {
				t.Fatal("Status accepted RBAC owned by a different namespace UID")
			}
		})
	}
}

func TestDiscoveryDisabledNeedsNoRBAC(t *testing.T) {
	client, s := started(t)
	if _, err := Status(t.Context(), client, s.ID); err != nil {
		t.Fatal(err)
	}
	for _, action := range client.Actions() {
		if action.GetResource().Group == rbacv1.GroupName {
			t.Fatalf("disabled discovery requires RBAC access: %+v", action)
		}
	}
	data, err := json.Marshal(testSpec())
	if err != nil || !strings.Contains(string(data), `"kubernetesDiscovery":false`) {
		t.Fatalf("false discovery option must be explicit in JSON: %s %v", data, err)
	}
}

func TestDiscoveryDoesNotAdoptExistingRBAC(t *testing.T) {
	for _, bindingCollision := range []bool{false, true} {
		client := testClient()
		spec := testSpec()
		spec.KubernetesDiscovery = true
		name := "hcp-perf-" + spec.ID + "-prometheus"
		metadata := metav1.ObjectMeta{Name: name, Labels: map[string]string{ManagedByLabel: "other"}}
		if bindingCollision {
			if _, err := client.RbacV1().ClusterRoleBindings().Create(t.Context(), &rbacv1.ClusterRoleBinding{ObjectMeta: metadata}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := client.RbacV1().ClusterRoles().Create(t.Context(), &rbacv1.ClusterRole{ObjectMeta: metadata}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		client.ClearActions()
		s, err := Start(t.Context(), client, spec, testConfig)
		if err == nil || s.Phase != PhaseFailed {
			t.Fatalf("existing RBAC adopted: %+v %v", s, err)
		}
		for _, action := range client.Actions() {
			if action.GetVerb() != "create" {
				t.Fatalf("collision caused mutation or cleanup: %+v", action)
			}
		}
		if _, err := Status(t.Context(), client, s.ID); err == nil {
			t.Fatal("Status accepted foreign colliding RBAC")
		}
	}
}

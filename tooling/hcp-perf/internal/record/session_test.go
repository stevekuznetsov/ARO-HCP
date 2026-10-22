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

package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func sessionSpec() SessionSpec {
	return SessionSpec{Version: "v1", ID: "observations", Namespace: "hcp-perf-example", Image: "example.azurecr.io/hcp-perf@sha256:" + strings.Repeat("a", 64),
		StorageClass: "managed-premium", StorageGi: 4, CPURequestMilli: 100, MemoryRequestMi: 128, MemoryLimitMi: 512,
		Namespaces: []string{"future-hcp"}, IntervalSeconds: 5, DurationSeconds: 60}
}

func sessionClient() *fake.Clientset {
	c := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sessionSpec().Namespace, UID: "namespace-uid", Labels: map[string]string{managedByLabel: "hcp-perf"}}})
	// The fake tracker does not implement API-server UID/resourceVersion defaults.
	c.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		object := action.(k8stesting.CreateAction).GetObject().(metav1.Object)
		object.SetUID(types.UID(action.GetResource().Resource + "-" + object.GetName()))
		object.SetResourceVersion("1")
		return false, nil, nil
	})
	return c
}

func startSession(t *testing.T) (*fake.Clientset, SessionSpec) {
	t.Helper()
	c, spec := sessionClient(), sessionSpec()
	if _, err := Start(t.Context(), c, spec); err != nil {
		t.Fatal(err)
	}
	return c, spec
}

func TestSessionValidation(t *testing.T) {
	for name, mutate := range map[string]func(*SessionSpec){
		"version":          func(s *SessionSpec) { s.Version = "v2" },
		"id empty":         func(s *SessionSpec) { s.ID = "" },
		"id long":          func(s *SessionSpec) { s.ID = strings.Repeat("a", 41) },
		"id invalid":       func(s *SessionSpec) { s.ID = "UPPER" },
		"namespace":        func(s *SessionSpec) { s.Namespace = "" },
		"image tag":        func(s *SessionSpec) { s.Image = "registry/image:latest" },
		"image URL":        func(s *SessionSpec) { s.Image = "https://" + s.Image },
		"image digest":     func(s *SessionSpec) { s.Image = "registry/image@sha256:" + strings.Repeat("A", 64) },
		"storage class":    func(s *SessionSpec) { s.StorageClass = "" },
		"storage zero":     func(s *SessionSpec) { s.StorageGi = 0 },
		"storage overflow": func(s *SessionSpec) { s.StorageGi = math.MaxInt64 },
		"cpu":              func(s *SessionSpec) { s.CPURequestMilli = 0 },
		"memory":           func(s *SessionSpec) { s.MemoryRequestMi = 0 },
		"memory order":     func(s *SessionSpec) { s.MemoryLimitMi = s.MemoryRequestMi - 1 },
		"memory overflow":  func(s *SessionSpec) { s.MemoryLimitMi = math.MaxInt64 },
		"targets":          func(s *SessionSpec) { s.Namespaces = nil },
		"all namespaces":   func(s *SessionSpec) { s.Namespaces = []string{""} },
		"duplicates":       func(s *SessionSpec) { s.Namespaces = []string{"hcp", "hcp"} },
		"interval":         func(s *SessionSpec) { s.IntervalSeconds = 4 },
		"duration":         func(s *SessionSpec) { s.DurationSeconds = 86401 },
	} {
		t.Run(name, func(t *testing.T) {
			c, spec := sessionClient(), sessionSpec()
			mutate(&spec)
			if _, err := Start(t.Context(), c, spec); err == nil {
				t.Fatal("invalid spec accepted")
			}
			if len(c.Actions()) != 0 {
				t.Fatal("validation contacted Kubernetes")
			}
		})
	}
	spec := sessionSpec()
	spec.ID = strings.Repeat("a", 40)
	if err := spec.Validate(); err != nil {
		t.Fatalf("maximum ID rejected: %v", err)
	}
	if spec.recorderSpec().OutputDir != "/data/observations" {
		t.Fatal("output path must be fixed")
	}
	example := readJSON[SessionSpec](t, "../../examples/record.json")
	if err := example.Validate(); err != nil {
		t.Fatalf("invalid example: %v", err)
	}
}

func TestSessionLiveOptionsAndRBAC(t *testing.T) {
	for _, watch := range []bool{false, true} {
		for _, logs := range []bool{false, true} {
			c, spec := sessionClient(), sessionSpec()
			spec.EnableWatch, spec.EnableLogs, spec.RawLogs, spec.MaxStreamBytes = watch, logs, logs, 4096
			spec.MaxConcurrentLogs, spec.MaxObservationBytes = 128, 8<<30
			spec.LogRequestQPS, spec.LogRequestBurst = 30, 60
			spec.StreamSyncIntervalMillis = 250
			if _, err := Start(t.Context(), c, spec); err != nil {
				t.Fatal(err)
			}
			cm, err := c.CoreV1().ConfigMaps(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var worker Spec
			if err := json.Unmarshal([]byte(cm.Data["spec.json"]), &worker); err != nil {
				t.Fatal(err)
			}
			if worker.EnableWatch != watch || worker.EnableLogs != logs || worker.RawLogs != logs || worker.MaxStreamBytes != 4096 || worker.MaxConcurrentLogs != 128 || worker.MaxObservationBytes != 8<<30 {
				t.Fatalf("options not propagated: %+v", worker)
			}
			var registered SessionSpec
			if err := json.Unmarshal([]byte(cm.Data["session.json"]), &registered); err != nil {
				t.Fatal(err)
			}
			if worker.LogRequestQPS != 30 || worker.LogRequestBurst != 60 || registered.LogRequestQPS != 30 || registered.LogRequestBurst != 60 || worker.StreamSyncIntervalMillis != 250 || registered.StreamSyncIntervalMillis != 250 {
				t.Fatal("explicit log request budgets not persisted in worker and registration")
			}
			ns, _ := c.CoreV1().Namespaces().Get(t.Context(), spec.Namespace, metav1.GetOptions{})
			role, err := c.RbacV1().ClusterRoles().Get(t.Context(), sessionRBACName(ns, spec.ID), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			verbs := []string{"get", "list"}
			if watch {
				verbs = append(verbs, "watch")
			}
			want := []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"pods", "events"}, Verbs: verbs},
				{APIGroups: []string{"apps"}, Resources: []string{"deployments", "statefulsets"}, Verbs: verbs},
			}
			if logs {
				want = append(want, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}})
			}
			if !reflect.DeepEqual(role.Rules, want) {
				t.Fatalf("opt-in RBAC exceeds request: %+v", role.Rules)
			}
			status := sessionStatus(spec.Namespace, spec.ID)
			if _, _, err := loadSession(t.Context(), c, &status); err != nil {
				t.Fatalf("live registration cannot reload: %v", err)
			}
		}
	}
	c, spec := sessionClient(), sessionSpec()
	spec.EnableLogs = true
	if _, err := Start(t.Context(), c, spec); err == nil || len(c.Actions()) != 0 {
		t.Fatal("unacknowledged logs created resources")
	}
}

func TestSessionNamespaceRequired(t *testing.T) {
	for _, mode := range []string{"missing", "unowned", "terminating", "no UID"} {
		t.Run(mode, func(t *testing.T) {
			c := sessionClient()
			ns, _ := c.CoreV1().Namespaces().Get(t.Context(), sessionSpec().Namespace, metav1.GetOptions{})
			switch mode {
			case "missing":
				if err := c.CoreV1().Namespaces().Delete(t.Context(), ns.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			case "unowned":
				ns.Labels = nil
			case "terminating":
				now := metav1.Now()
				ns.DeletionTimestamp = &now
			case "no UID":
				ns.UID = ""
			}
			if mode != "missing" {
				if _, err := c.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			c.ClearActions()
			if _, err := Start(t.Context(), c, sessionSpec()); err == nil {
				t.Fatal("unsafe namespace accepted")
			}
			for _, a := range c.Actions() {
				if a.GetVerb() != "get" {
					t.Fatalf("unexpected mutation: %v", a)
				}
			}
		})
	}
	c := sessionClient()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Start(ctx, c, sessionSpec()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if len(c.Actions()) != 0 {
		t.Fatal("canceled start contacted Kubernetes")
	}
}

func TestSessionRendering(t *testing.T) {
	c, spec := startSession(t)
	name := "record-" + spec.ID
	var creates []string
	for _, a := range c.Actions() {
		if a.GetVerb() == "create" {
			creates = append(creates, a.GetResource().Resource)
		}
		if a.GetVerb() == "get" && a.(k8stesting.GetAction).GetName() != spec.Namespace {
			t.Fatalf("read target namespace before it exists: %v", a)
		}
	}
	wantCreates := []string{"configmaps", "serviceaccounts", "clusterroles", "clusterrolebindings", "persistentvolumeclaims", "jobs"}
	if !reflect.DeepEqual(creates, wantCreates) {
		t.Fatalf("creation order: %v", creates)
	}
	cm, err := c.CoreV1().ConfigMaps(spec.Namespace).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Immutable == nil || !*cm.Immutable {
		t.Fatal("registration is mutable")
	}
	var worker Spec
	if err := json.Unmarshal([]byte(cm.Data["spec.json"]), &worker); err != nil {
		t.Fatal(err)
	}
	wantWorker := spec.recorderSpec()
	wantWorker.LogRequestQPS, wantWorker.LogRequestBurst = 20, 40
	wantWorker.StreamSyncIntervalMillis = 1000
	if !reflect.DeepEqual(worker, wantWorker) {
		t.Fatalf("worker spec differs: %+v", worker)
	}
	var registered SessionSpec
	if err := json.Unmarshal([]byte(cm.Data["session.json"]), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.LogRequestQPS != 20 || registered.LogRequestBurst != 40 || registered.StreamSyncIntervalMillis != 1000 {
		t.Fatal("resolved defaults missing from immutable registration")
	}
	job, err := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *job.Spec.BackoffLimit != 0 || *job.Spec.Completions != 1 || *job.Spec.Parallelism != 1 || *job.Spec.ActiveDeadlineSeconds != spec.DurationSeconds+120 || job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("unsafe Job bounds: %+v", job.Spec)
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever || pod.NodeSelector["aro-hcp.azure.com/role"] != "infra" || !reflect.DeepEqual(pod.Tolerations, []corev1.Toleration{{Key: "infra", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}}) {
		t.Fatal("incorrect placement or restart policy")
	}
	if !*pod.SecurityContext.RunAsNonRoot || *pod.SecurityContext.FSGroup != 2000 || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("unsafe pod security context")
	}
	if len(pod.Containers) != 1 || len(pod.InitContainers) != 0 {
		t.Fatal("expected exactly one worker")
	}
	container := pod.Containers[0]
	wantProbe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/hcp-perf", "record", "health", "--input", "/data/observations", "--require-ready"}}}, PeriodSeconds: 2, FailureThreshold: 3}
	if !reflect.DeepEqual(container.ReadinessProbe, wantProbe) {
		t.Fatalf("incorrect recorder readiness probe: %+v", container.ReadinessProbe)
	}
	if !reflect.DeepEqual(container.Command, []string{"/hcp-perf"}) || !reflect.DeepEqual(container.Args, []string{"record", "worker", "--input", "/config/spec.json"}) || container.Image != spec.Image || len(container.Env) != 0 {
		t.Fatal("incorrect worker invocation")
	}
	if !*container.SecurityContext.ReadOnlyRootFilesystem || *container.SecurityContext.AllowPrivilegeEscalation || !reflect.DeepEqual(container.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Fatal("unsafe container security context")
	}
	if container.Resources.Requests.Cpu().MilliValue() != spec.CPURequestMilli || container.Resources.Requests.Memory().Value() != spec.MemoryRequestMi*1024*1024 || container.Resources.Limits.Memory().Value() != spec.MemoryLimitMi*1024*1024 {
		t.Fatal("incorrect resource bounds")
	}
	if pod.ServiceAccountName != name || *pod.AutomountServiceAccountToken || len(pod.Volumes) != 3 {
		t.Fatal("incorrect service account/volumes")
	}
	if pod.Volumes[0].PersistentVolumeClaim.ClaimName != name || pod.Volumes[0].PersistentVolumeClaim.ReadOnly || container.VolumeMounts[0].MountPath != "/data" || container.VolumeMounts[0].ReadOnly {
		t.Fatal("dedicated writable PVC not mounted")
	}
	projection := pod.Volumes[2].Projected
	if projection == nil || len(projection.Sources) != 3 || projection.Sources[0].ServiceAccountToken.Path != "token" || projection.Sources[0].ServiceAccountToken.ExpirationSeconds == nil || projection.Sources[1].ConfigMap.Name != "kube-root-ca.crt" || projection.Sources[2].DownwardAPI.Items[0].FieldRef.FieldPath != "metadata.namespace" {
		t.Fatal("missing projected in-cluster credentials")
	}
	pvc, err := c.CoreV1().PersistentVolumeClaims(spec.Namespace).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pvc.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) || *pvc.Spec.StorageClassName != spec.StorageClass || pvc.Spec.Resources.Requests.Storage().Value() != spec.StorageGi*1024*1024*1024 || pvc.Spec.DataSource != nil || pvc.Spec.VolumeName != "" || metav1.GetControllerOf(pvc).Kind != "ConfigMap" {
		t.Fatal("PVC must be dedicated, empty and independent of Job lifetime")
	}
	if pvc.Labels[RecordIDLabel] != spec.ID || pvc.Labels["hcp-perf.azure.com/session-id"] != "" {
		t.Fatal("recorder and capture identities conflated")
	}
	ns, _ := c.CoreV1().Namespaces().Get(t.Context(), spec.Namespace, metav1.GetOptions{})
	role, err := c.RbacV1().ClusterRoles().Get(t.Context(), sessionRBACName(ns, spec.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods", "events"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments", "statefulsets"}, Verbs: []string{"get", "list"}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) || !sessionOwned(role, spec.ID, ns, "Namespace", "v1") {
		t.Fatal("RBAC exceeds read allowlist or lacks namespace ownership")
	}
	binding, err := c.RbacV1().ClusterRoleBindings().Get(t.Context(), role.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: spec.Namespace}}) || binding.RoleRef.Name != role.Name || !sessionOwned(binding, spec.ID, ns, "Namespace", "v1") {
		t.Fatal("incorrect RBAC binding")
	}
	other := ns.DeepCopy()
	other.UID = "new-namespace-uid"
	if sessionRBACName(other, spec.ID) == role.Name {
		t.Fatal("RBAC identity must include namespace UID")
	}
	other.Name = strings.Repeat("n", 63)
	if len(sessionRBACName(other, strings.Repeat("a", 40))) > 63 {
		t.Fatal("RBAC name exceeds label length")
	}
}

func TestSessionCreateOnlyAndPartialInventory(t *testing.T) {
	resources := []string{"configmaps", "serviceaccounts", "clusterroles", "clusterrolebindings", "persistentvolumeclaims", "jobs"}
	for index, resourceName := range resources {
		t.Run(resourceName, func(t *testing.T) {
			c := sessionClient()
			c.PrependReactor("create", resourceName, func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: resourceName}, "collision")
			})
			s, err := Start(t.Context(), c, sessionSpec())
			if err == nil || s.Phase != "partial" || s.Namespace != sessionSpec().Namespace || len(s.Inventory) != index+1 {
				t.Fatalf("partial inventory lost: %+v, %v", s, err)
			}
			if (s.Storage != nil) != (resourceName == "jobs") {
				t.Fatal("incorrect confirmed PVC inventory")
			}
			actions := c.Actions()
			if actions[len(actions)-1].GetResource().Resource != resourceName || actions[len(actions)-1].GetVerb() != "create" {
				t.Fatal("continued requests after collision")
			}
			for _, a := range actions {
				if a.GetVerb() != "get" && a.GetVerb() != "create" {
					t.Fatalf("unexpected adoption/cleanup: %v", a)
				}
			}
		})
	}
	c, spec := startSession(t)
	c.ClearActions()
	if _, err := Start(t.Context(), c, spec); err == nil {
		t.Fatal("duplicate session adopted")
	}
	if len(c.Actions()) != 2 || c.Actions()[1].GetResource().Resource != "configmaps" {
		t.Fatal("duplicate start created additional resources")
	}
}

func TestSessionUnownedRoleCollision(t *testing.T) {
	c, spec := sessionClient(), sessionSpec()
	ns, _ := c.CoreV1().Namespaces().Get(t.Context(), spec.Namespace, metav1.GetOptions{})
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: sessionRBACName(ns, spec.ID)}, Rules: []rbacv1.PolicyRule{{Verbs: []string{"*"}, Resources: []string{"*"}, APIGroups: []string{"*"}}}}
	original, err := c.RbacV1().ClusterRoles().Create(t.Context(), role, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(t.Context(), c, spec); err == nil {
		t.Fatal("unowned role adopted")
	}
	current, _ := c.RbacV1().ClusterRoles().Get(t.Context(), role.Name, metav1.GetOptions{})
	if !reflect.DeepEqual(current, original) {
		t.Fatal("collision overwrote unowned role")
	}
	jobs, _ := c.BatchV1().Jobs(spec.Namespace).List(t.Context(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatal("Job created using unowned role")
	}
	if _, err := Status(t.Context(), c, spec.Namespace, spec.ID); err == nil {
		t.Fatal("status accepted unowned role")
	}
}

func TestSessionJobStatusAndEvidence(t *testing.T) {
	for _, phase := range []string{"pending", "running", "completed", "failed", "stopping", "stopped", "missing"} {
		t.Run(phase, func(t *testing.T) {
			c, spec := startSession(t)
			job, _ := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
			switch phase {
			case "running":
				job.Status.Active = 1
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: spec.Namespace, Labels: sessionLabels(spec.ID), OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}},
					Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: sessionPtr(metav1.Now()), ContainerStatuses: []corev1.ContainerStatus{{Name: "record", Ready: true}}}}
				if _, err := c.CoreV1().Pods(spec.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "completed":
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Message: "SECRET"}}
			case "failed":
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "SECRET"}}
			case "stopping":
				job.Spec.Suspend = sessionPtr(true)
			case "stopped":
				job.Spec.Suspend = sessionPtr(true)
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuspended, Status: corev1.ConditionTrue}}
			}
			if _, err := c.BatchV1().Jobs(spec.Namespace).Update(t.Context(), job, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if phase == "missing" {
				if err := c.BatchV1().Jobs(spec.Namespace).Delete(t.Context(), job.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			s, err := Status(t.Context(), c, spec.Namespace, spec.ID)
			want := phase
			if phase == "missing" {
				want = "partial"
			}
			if s.Phase != want || (err != nil) != (phase == "missing") || s.Storage == nil || s.Storage.UID == "" {
				t.Fatalf("incorrect lifecycle/evidence: %+v, %v", s, err)
			}
			data, _ := json.Marshal(s)
			if strings.Contains(string(data), "SECRET") {
				t.Fatal("status leaked API messages")
			}
		})
	}
}

func TestSessionStartupGate(t *testing.T) {
	for _, mode := range []string{"ready", "active-only", "pending", "unknown", "unready", "no-status", "other-container", "no-start", "zero-start", "deleting", "succeeded", "failed"} {
		t.Run(mode, func(t *testing.T) {
			c, spec := startSession(t)
			job, _ := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
			job.Status.Active = 1
			if _, err := c.BatchV1().Jobs(spec.Namespace).UpdateStatus(t.Context(), job, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: spec.Namespace, Labels: sessionLabels(spec.ID), OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: sessionPtr(metav1.Now()), ContainerStatuses: []corev1.ContainerStatus{{Name: "record", Ready: true}}}}
			switch mode {
			case "pending":
				pod.Status.Phase = corev1.PodPending
			case "unknown":
				pod.Status.Phase = corev1.PodUnknown
			case "unready":
				pod.Status.ContainerStatuses[0].Ready = false
			case "no-status":
				pod.Status.ContainerStatuses = nil
			case "other-container":
				pod.Status.ContainerStatuses[0].Name = "other"
			case "no-start":
				pod.Status.StartTime = nil
			case "zero-start":
				pod.Status.StartTime = &metav1.Time{}
			case "deleting":
				pod.DeletionTimestamp = sessionPtr(metav1.Now())
			case "succeeded":
				pod.Status.Phase = corev1.PodSucceeded
			case "failed":
				pod.Status.Phase = corev1.PodFailed
			}
			if mode != "active-only" {
				if _, err := c.CoreV1().Pods(spec.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			s, err := Status(t.Context(), c, spec.Namespace, spec.ID)
			want := "pending"
			if mode == "ready" {
				want = "running"
			}
			count := 1
			if mode == "active-only" || mode == "succeeded" || mode == "failed" {
				count = 0
			}
			if err != nil || s.Phase != want || s.NonterminalPods != count {
				t.Fatalf("startup phase or writer count incorrect: %+v, %v", s, err)
			}
			if mode == "ready" && (!s.Pods[0].Ready || s.Pods[0].StartTime.IsZero()) {
				t.Fatal("readiness evidence missing")
			}
		})
	}
}

func TestSessionStop(t *testing.T) {
	c, spec := startSession(t)
	job, _ := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: spec.Namespace, Labels: sessionLabels(spec.ID), OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if _, err := c.CoreV1().Pods(spec.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c.ClearActions()
	s, err := Stop(t.Context(), c, spec.Namespace, spec.ID)
	if err != nil || s.Phase != "stopping" || !s.StopRequested || s.NonterminalPods != 1 || s.Storage == nil {
		t.Fatalf("stop prematurely finished: %+v, %v", s, err)
	}
	patches := 0
	for _, a := range c.Actions() {
		if a.GetVerb() == "patch" {
			patches++
			p := a.(k8stesting.PatchAction)
			var ops []struct {
				Op, Path string
				Value    any
			}
			if err := json.Unmarshal(p.GetPatch(), &ops); err != nil {
				t.Fatal(err)
			}
			if p.GetPatchType() != types.JSONPatchType || a.GetResource().Resource != "jobs" || len(ops) != 3 || ops[0].Op != "test" || ops[0].Path != "/metadata/uid" || ops[0].Value != string(job.UID) || ops[1].Path != "/metadata/resourceVersion" || ops[1].Value != job.ResourceVersion || ops[2].Path != "/spec/suspend" || ops[2].Value != true {
				t.Fatalf("unguarded mutation: %s", p.GetPatch())
			}
		} else if a.GetVerb() != "get" && a.GetVerb() != "list" {
			t.Fatalf("unexpected destructive stop action: %v", a)
		}
	}
	if patches != 1 {
		t.Fatalf("expected exactly one patch, got %d", patches)
	}
	job, _ = c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), job.Name, metav1.GetOptions{})
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuspended, Status: corev1.ConditionTrue}}
	if _, err := c.BatchV1().Jobs(spec.Namespace).Update(t.Context(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	s, err = Status(t.Context(), c, spec.Namespace, spec.ID)
	if err != nil || s.Phase != "stopping" {
		t.Fatal("suspension acknowledged while live pod exists")
	}
	if err := c.CoreV1().Pods(spec.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s, err = Status(t.Context(), c, spec.Namespace, spec.ID)
	if err != nil || s.Phase != "stopped" || s.Storage == nil {
		t.Fatalf("stopped session lost evidence: %+v, %v", s, err)
	}
}

func TestSessionStopOwnershipAndConflicts(t *testing.T) {
	for _, mode := range []string{"missing job", "labels", "owner UID", "deleting", "no UID", "no version", "conflict", "registration", "namespace"} {
		t.Run(mode, func(t *testing.T) {
			c, spec := startSession(t)
			job, _ := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
			switch mode {
			case "labels":
				job.Labels = nil
			case "owner UID":
				job.OwnerReferences[0].UID = "foreign"
			case "deleting":
				now := metav1.Now()
				job.DeletionTimestamp = &now
			case "no UID":
				job.UID = ""
			case "no version":
				job.ResourceVersion = ""
			case "registration":
				cm, _ := c.CoreV1().ConfigMaps(spec.Namespace).Get(t.Context(), job.Name, metav1.GetOptions{})
				cm.OwnerReferences[0].UID = "foreign"
				if _, err := c.CoreV1().ConfigMaps(spec.Namespace).Update(t.Context(), cm, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "namespace":
				ns, _ := c.CoreV1().Namespaces().Get(t.Context(), spec.Namespace, metav1.GetOptions{})
				ns.Labels = nil
				if _, err := c.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "conflict":
				c.PrependReactor("patch", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "jobs"}, job.Name, fmt.Errorf("SECRET"))
				})
			}
			if _, err := c.BatchV1().Jobs(spec.Namespace).Update(t.Context(), job, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if mode == "missing job" {
				if err := c.BatchV1().Jobs(spec.Namespace).Delete(t.Context(), job.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			c.ClearActions()
			s, err := Stop(t.Context(), c, spec.Namespace, spec.ID)
			if err == nil || s.Phase == "stopped" || s.StopRequested || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe stop result: %+v, %v", s, err)
			}
			for _, a := range c.Actions() {
				if a.GetVerb() == "patch" && mode != "conflict" {
					t.Fatalf("mutated unowned Job: %v", a)
				}
			}
		})
	}
}

func TestSessionStatusRejectsPodLabelDrift(t *testing.T) {
	c, spec := startSession(t)
	job, _ := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
	job.Spec.Suspend = sessionPtr(true)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuspended, Status: corev1.ConditionTrue}}
	if _, err := c.BatchV1().Jobs(spec.Namespace).Update(t.Context(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: spec.Namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if _, err := c.CoreV1().Pods(spec.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	s, err := Status(t.Context(), c, spec.Namespace, spec.ID)
	if err == nil || s.Phase == "stopped" {
		t.Fatalf("label drift concealed active pod: %+v, %v", s, err)
	}
}

func TestSessionStatusRejectsOrphanedPods(t *testing.T) {
	for _, signal := range []string{"PVC", "service account", "name", "generate name", "unrelated"} {
		t.Run(signal, func(t *testing.T) {
			c, spec := startSession(t)
			job, err := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			job.Spec.Suspend = sessionPtr(true)
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuspended, Status: corev1.ConditionTrue}}
			if _, err := c.BatchV1().Jobs(spec.Namespace).Update(t.Context(), job, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			// Model a released pod with every label and owner reference removed.
			// Test each remaining association independently.
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: spec.Namespace},
				Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			switch signal {
			case "PVC":
				pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: job.Name}}}}
			case "service account":
				pod.Spec.ServiceAccountName = job.Name
			case "name":
				pod.Name = job.Name + "-abcde"
			case "generate name":
				pod.GenerateName = job.Name + "-"
			case "unrelated":
				pod.Name = job.Name + "other-abcde"
				pod.Spec.ServiceAccountName = "other"
				pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "prometheus"}}}}
			}
			if _, err := c.CoreV1().Pods(spec.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			s, err := Status(t.Context(), c, spec.Namespace, spec.ID)
			if signal == "unrelated" {
				if err != nil || s.Phase != "stopped" {
					t.Fatalf("unrelated pod blocked status: %+v, %v", s, err)
				}
				return
			}
			if err == nil || s.Phase != "partial" || !strings.Contains(err.Error(), "pod ownership mismatch") || s.Storage == nil {
				t.Fatalf("orphan concealed or evidence lost: %+v, %v", s, err)
			}
			// Stop may suspend the Job, but must not acknowledge orphan inactivity.
			s, err = Stop(t.Context(), c, spec.Namespace, spec.ID)
			if err == nil || s.Phase != "partial" || !s.StopRequested {
				t.Fatalf("stop concealed orphan: %+v, %v", s, err)
			}
		})
	}
}

func TestSessionStatusMissingResourcesPreventCompletion(t *testing.T) {
	for _, resourceName := range []string{"namespaces", "configmaps", "persistentvolumeclaims", "serviceaccounts", "clusterroles", "clusterrolebindings", "jobs"} {
		t.Run(resourceName, func(t *testing.T) {
			c, spec := startSession(t)
			job, err := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			if _, err := c.BatchV1().Jobs(spec.Namespace).Update(t.Context(), job, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			c.PrependReactor("get", resourceName, func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), action.(k8stesting.GetAction).GetName())
			})
			s, err := Status(t.Context(), c, spec.Namespace, spec.ID)
			if err == nil || s.Phase != "partial" {
				t.Fatalf("missing resource permitted completion: %+v, %v", s, err)
			}
			if resourceName != "namespaces" && resourceName != "configmaps" && len(s.Missing) != 1 {
				t.Fatalf("missing resource not inventoried: %+v", s)
			}
		})
	}
}

func TestSessionErrorFixedClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "arbitrary reason", err: &apierrors.StatusError{ErrStatus: metav1.Status{Reason: "SECRET admission payload", Message: "SECRET", Code: 500}}, want: "apiFailure"},
		{name: "forbidden code", err: &apierrors.StatusError{ErrStatus: metav1.Status{Reason: "SECRET admission payload", Message: "SECRET", Code: 403}}, want: "forbidden"},
		{name: "wrapped reason", err: fmt.Errorf("SECRET: %w", &apierrors.StatusError{ErrStatus: metav1.Status{Reason: "SECRET", Code: 500}}), want: "apiFailure"},
		{name: "plain error", err: errors.New("SECRET"), want: "apiFailure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := "test: Kubernetes API request failed (" + tc.want + ")"
			if got := sessionError("test", tc.err).Error(); got != want {
				t.Fatalf("unsafe error classification: %q, want %q", got, want)
			}
		})
	}
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		err := sessionError("test", fmt.Errorf("SECRET: %w", sentinel))
		if !errors.Is(err, sentinel) || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("cancellation identity lost or payload leaked: %v", err)
		}
	}
}

func TestSessionStopPreservesConfirmedMutationOnDiagnosticFailure(t *testing.T) {
	c, spec := startSession(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.PrependReactor("patch", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})
	s, err := Stop(ctx, c, spec.Namespace, spec.ID)
	if !errors.Is(err, context.Canceled) || !s.StopRequested || s.Phase != "stopping" || len(s.Inventory) != 3 {
		t.Fatalf("confirmed stop mutation/inventory lost: %+v, %v", s, err)
	}
}

func TestSessionStopRejectsReplacedJobDuringPatch(t *testing.T) {
	c, spec := startSession(t)
	c.PrependReactor("patch", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		gvr := batchv1.SchemeGroupVersion.WithResource("jobs")
		object, err := c.Tracker().Get(gvr, spec.Namespace, action.(k8stesting.PatchAction).GetName())
		if err != nil {
			t.Fatal(err)
		}
		job := object.(*batchv1.Job)
		job.UID = "replacement"
		if err := c.Tracker().Update(gvr, job, spec.Namespace); err != nil {
			t.Fatal(err)
		}
		return false, nil, nil
	})
	s, err := Stop(t.Context(), c, spec.Namespace, spec.ID)
	if err == nil || s.StopRequested {
		t.Fatalf("replacement accepted: %+v, %v", s, err)
	}
	job, err := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.Suspend != nil && *job.Spec.Suspend {
		t.Fatal("replacement Job was suspended")
	}
}

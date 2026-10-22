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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const testConfig = "global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: self\n    static_configs:\n      - targets: ['localhost:9090']\n"

func testSpec() Spec {
	return Spec{Version: "v1", ID: "test-1", Image: "quay.io/prometheus/prometheus@sha256:" + strings.Repeat("a", 64),
		StorageClass: "managed-premium", StorageGi: 32, RetentionHours: 24,
		CPURequestMilli: 500, MemoryRequestMi: 1024, MemoryLimitMi: 2048, KubernetesDiscovery: false}
}

// Real apiservers populate these fields. The fake does not; assign them so
// tests exercise the same patch preconditions as production.
func testClient() *fake.Clientset {
	client := fake.NewClientset()
	client.PrependReactor("create", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		object, err := meta.Accessor(action.(clienttesting.CreateAction).GetObject())
		if err != nil {
			return true, nil, err
		}
		object.SetUID(types.UID(action.GetResource().Resource + "-" + object.GetName()))
		object.SetResourceVersion("1")
		return false, nil, nil
	})
	return client
}

func started(t *testing.T) (*fake.Clientset, SessionStatus) {
	t.Helper()
	client := testClient()
	s, err := Start(t.Context(), client, testSpec(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	return client, s
}

func TestValidate(t *testing.T) {
	tests := map[string]func(*Spec){
		"version omitted":     func(s *Spec) { s.Version = "" },
		"version unknown":     func(s *Spec) { s.Version = "v2" },
		"empty ID":            func(s *Spec) { s.ID = "" },
		"long ID":             func(s *Spec) { s.ID = strings.Repeat("a", 41) },
		"uppercase ID":        func(s *Spec) { s.ID = "ABC" },
		"path ID":             func(s *Spec) { s.ID = "../../default" },
		"dotted ID":           func(s *Spec) { s.ID = "a.b" },
		"leading hyphen":      func(s *Spec) { s.ID = "-abc" },
		"trailing hyphen":     func(s *Spec) { s.ID = "abc-" },
		"tag":                 func(s *Spec) { s.Image = "quay.io/prometheus/prometheus:latest" },
		"short digest":        func(s *Spec) { s.Image = "image@sha256:" + strings.Repeat("a", 63) },
		"long digest":         func(s *Spec) { s.Image += "a" },
		"nonhex digest":       func(s *Spec) { s.Image = "image@sha256:" + strings.Repeat("g", 64) },
		"uppercase digest":    func(s *Spec) { s.Image = "image@sha256:" + strings.Repeat("A", 64) },
		"missing image name":  func(s *Spec) { s.Image = "@sha256:" + strings.Repeat("a", 64) },
		"image whitespace":    func(s *Spec) { s.Image += "\n" },
		"default storage":     func(s *Spec) { s.StorageClass = "" },
		"bad storage class":   func(s *Spec) { s.StorageClass = "bad/class" },
		"zero storage":        func(s *Spec) { s.StorageGi = 0 },
		"negative storage":    func(s *Spec) { s.StorageGi = -1 },
		"huge storage":        func(s *Spec) { s.StorageGi = MaxStorageGi + 1 },
		"zero retention":      func(s *Spec) { s.RetentionHours = 0 },
		"negative retention":  func(s *Spec) { s.RetentionHours = -1 },
		"long retention":      func(s *Spec) { s.RetentionHours = 169 },
		"zero cpu":            func(s *Spec) { s.CPURequestMilli = 0 },
		"negative cpu":        func(s *Spec) { s.CPURequestMilli = -1 },
		"zero memory request": func(s *Spec) { s.MemoryRequestMi = 0 },
		"zero memory limit":   func(s *Spec) { s.MemoryLimitMi = 0 },
		"small memory limit":  func(s *Spec) { s.MemoryLimitMi = s.MemoryRequestMi - 1 },
		"overflow memory":     func(s *Spec) { s.MemoryLimitMi = 1 << 62 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			spec := testSpec()
			mutate(&spec)
			client := testClient()
			if _, err := Start(t.Context(), client, spec, testConfig); err == nil {
				t.Fatal("invalid spec accepted")
			}
			if len(client.Actions()) != 0 {
				t.Fatal("invalid spec made API requests")
			}
		})
	}
	spec := testSpec()
	spec.ID, spec.StorageGi, spec.RetentionHours = strings.Repeat("a", 40), MaxStorageGi, 168
	if err := spec.Validate(); err != nil {
		t.Fatalf("valid boundary values rejected: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, config := range []string{
		"", "[]", "null", "scrape_configs: []", "scrape_configs: {}", "scrape_configs: [null]",
		"scrape_configs: [{job_name: ''}]", "scrape_configs: [{job_name: a}, {job_name: a}]",
		"scrape_configs: [{job_name: a, job_name: b}]", "scrape_configs: [SECRET",
		"scrape_configs: [{job_name: a}]\nscrape_configs: [{job_name: b}]",
		testConfig + "---\nscrape_configs: [{job_name: other}]\n",
		strings.Repeat("a", 900*1024+1),
	} {
		client := testClient()
		_, err := Start(t.Context(), client, testSpec(), config)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("invalid configuration accepted or leaked: %v", err)
		}
		if len(client.Actions()) != 0 {
			t.Fatal("invalid configuration made API requests")
		}
	}
	for _, config := range []string{testConfig, "---\n" + testConfig, `{"scrape_configs":[{"job_name":"self","static_configs":[{"targets":["localhost:9090"]}]}]}`} {
		if err := validateConfig(config); err != nil {
			t.Fatalf("valid YAML/JSON rejected: %v", err)
		}
	}
}

func TestStartResources(t *testing.T) {
	client, s := started(t)
	if s.Phase != PhasePending || s.DesiredReplicas != 1 || s.ReadyReplicas != 0 || len(s.Inventory) != 6 {
		t.Fatalf("Start must report provisioning, not readiness: %+v", s)
	}
	ns, _ := client.CoreV1().Namespaces().Get(t.Context(), s.Namespace, metav1.GetOptions{})
	if s.Namespace != "hcp-perf-test-1" || !owned(ns, s.ID) || ns.Labels["network.openshift.io/policy-group"] != "monitoring" {
		t.Fatalf("incorrect namespace metadata: %+v", ns)
	}
	sts, _ := client.AppsV1().StatefulSets(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	pod := sts.Spec.Template.Spec
	if pod.NodeSelector["aro-hcp.azure.com/role"] != "infra" || !reflect.DeepEqual(pod.Tolerations, []corev1.Toleration{{Key: "infra", Value: "true", Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule}}) {
		t.Fatalf("infra placement missing: %+v", pod)
	}
	if *pod.AutomountServiceAccountToken || *pod.SecurityContext.RunAsUser != 1000 || *pod.SecurityContext.FSGroup != 2000 || !*pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("unsafe pod security settings")
	}
	container := pod.Containers[0]
	if container.Image != testSpec().Image || *container.SecurityContext.AllowPrivilegeEscalation || !*container.SecurityContext.ReadOnlyRootFilesystem || !reflect.DeepEqual(container.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Fatal("incorrect image or container security")
	}
	if container.Resources.Requests.Cpu().MilliValue() != 500 || container.Resources.Requests.Memory().Value() != 1024*1024*1024 || container.Resources.Limits.Memory().Value() != 2048*1024*1024 {
		t.Fatal("resource quantities were not applied")
	}
	args := strings.Join(container.Args, " ")
	for _, arg := range []string{"--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus", "--storage.tsdb.retention.time=24h"} {
		if !strings.Contains(args, arg) {
			t.Fatalf("missing argument %s", arg)
		}
	}
	if strings.Contains(args, "enable-admin-api") || strings.Contains(args, "enable-lifecycle") || *pod.TerminationGracePeriodSeconds < 120 {
		t.Fatal("unsafe lifecycle settings")
	}
	if container.ReadinessProbe.HTTPGet.Path != "/-/ready" || container.LivenessProbe.HTTPGet.Path != "/-/healthy" || container.StartupProbe.HTTPGet.Path != "/-/ready" {
		t.Fatal("missing health probes")
	}
	if len(sts.Spec.VolumeClaimTemplates) != 0 || pod.Volumes[1].PersistentVolumeClaim.ClaimName != resourceName {
		t.Fatal("data must use an explicit independent PVC")
	}
	pvc, _ := client.CoreV1().PersistentVolumeClaims(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if len(pvc.OwnerReferences) != 0 || *pvc.Spec.StorageClassName != "managed-premium" || pvc.Spec.Resources.Requests.Storage().Value() != 32*1024*1024*1024 {
		t.Fatal("PVC must retain data independently of the StatefulSet")
	}
	cm, _ := client.CoreV1().ConfigMaps(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	var persisted Spec
	if err := json.Unmarshal([]byte(cm.Data["spec.json"]), &persisted); err != nil {
		t.Fatal(err)
	}
	if !*cm.Immutable || cm.Data["prometheus.yml"] != testConfig || !reflect.DeepEqual(persisted, testSpec()) {
		t.Fatal("configuration/spec not persisted immutably")
	}
	sa, _ := client.CoreV1().ServiceAccounts(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if *sa.AutomountServiceAccountToken {
		t.Fatal("service account automatically mounts token")
	}
	svc, _ := client.CoreV1().Services(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if svc.Spec.Type != corev1.ServiceTypeClusterIP || svc.Spec.Ports[0].Port != 9090 || !reflect.DeepEqual(svc.Spec.Selector, sts.Spec.Template.Labels) {
		t.Fatal("service does not select the capture pod")
	}
	for _, action := range client.Actions() {
		if action.GetResource().Group == "rbac.authorization.k8s.io" {
			t.Fatal("Start must not grant RBAC")
		}
		if action.GetVerb() == "create" {
			object, _ := meta.Accessor(action.(clienttesting.CreateAction).GetObject())
			if !owned(object, s.ID) {
				t.Fatal("resource missing ownership labels")
			}
		}
	}
}

func TestExistingNamespaceNeverAdopted(t *testing.T) {
	for _, namespaceLabels := range []map[string]string{nil, labels(testSpec().ID), labels("someone-else")} {
		client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "hcp-perf-" + testSpec().ID, Labels: namespaceLabels}})
		s, err := Start(t.Context(), client, testSpec(), testConfig)
		if err == nil || len(s.Inventory) != 0 || len(client.Actions()) != 1 {
			t.Fatalf("existing namespace adopted or modified: %+v, %v", s, err)
		}
	}
	client, original := started(t)
	client.ClearActions()
	spec := testSpec()
	spec.StorageGi++
	if _, err := Start(t.Context(), client, spec, testConfig+"# changed\n"); err == nil {
		t.Fatal("Start allowed a session update")
	}
	if len(client.Actions()) != 1 {
		t.Fatal("Start modified existing child resources")
	}
	cm, _ := client.CoreV1().ConfigMaps(original.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if cm.Data["prometheus.yml"] != testConfig {
		t.Fatal("existing config changed")
	}
}

func TestPartialFailureRetainsInventory(t *testing.T) {
	resources := []string{"namespaces", "serviceaccounts", "configmaps", "persistentvolumeclaims", "services", "statefulsets"}
	for index, resource := range resources {
		t.Run(resource, func(t *testing.T) {
			client := testClient()
			client.PrependReactor("create", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, resourceName, errors.New("SECRET"))
			})
			s, err := Start(t.Context(), client, testSpec(), testConfig)
			if err == nil || strings.Contains(err.Error(), "SECRET") || len(s.Inventory) != index {
				t.Fatalf("incorrect partial inventory or error: %+v %v", s, err)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "create" {
					t.Fatalf("failure should not perform cleanup or further reads: %v", action)
				}
			}
			if index > 0 {
				observed, err := Status(t.Context(), client, testSpec().ID)
				if err != nil || observed.Phase != PhaseFailed || len(observed.Inventory) != index || len(observed.Missing) != 6-index {
					t.Fatalf("cannot inspect partial start: %+v %v", observed, err)
				}
				if err := Cleanup(t.Context(), client, testSpec().ID, true); err == nil {
					t.Fatal("cleanup must require a stopped capture, even after partial start")
				}
			}
		})
	}
}

func TestOwnershipChecks(t *testing.T) {
	for _, operation := range []string{"status", "stop", "cleanup"} {
		t.Run(operation, func(t *testing.T) {
			client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "hcp-perf-test-1", Labels: labels("other")}})
			var err error
			switch operation {
			case "status":
				_, err = Status(t.Context(), client, testSpec().ID)
			case "stop":
				_, err = Stop(t.Context(), client, testSpec().ID)
			case "cleanup":
				err = Cleanup(t.Context(), client, testSpec().ID, true)
			}
			if err == nil || len(client.Actions()) != 1 || client.Actions()[0].GetVerb() != "get" {
				t.Fatal("unowned namespace was accessed or modified")
			}
		})
	}
	client, s := started(t)
	sts, _ := client.AppsV1().StatefulSets(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	sts.Labels = labels("other")
	if _, err := client.AppsV1().StatefulSets(s.Namespace).Update(t.Context(), sts, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	if _, err := Stop(t.Context(), client, s.ID); err == nil {
		t.Fatal("Stop accepted unowned StatefulSet")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" || action.GetVerb() == "update" {
			t.Fatal("Stop modified unowned StatefulSet")
		}
	}
}

func makeReady(t *testing.T, client *fake.Clientset, s SessionStatus) {
	t.Helper()
	sts, _ := client.AppsV1().StatefulSets(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	sts.Status.ReadyReplicas, sts.Status.ObservedGeneration = 1, sts.Generation
	if _, err := client.AppsV1().StatefulSets(s.Namespace).UpdateStatus(t.Context(), sts, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pvc, _ := client.CoreV1().PersistentVolumeClaims(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	pvc.Status.Phase = corev1.ClaimBound
	if _, err := client.CoreV1().PersistentVolumeClaims(s.Namespace).UpdateStatus(t.Context(), pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := client.CoreV1().Pods(s.Namespace).Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "prometheus-0", Namespace: s.Namespace, Labels: labels(s.ID),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(sts, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStopRetainsPVCAndWaitsForActivePods(t *testing.T) {
	client, s := started(t)
	makeReady(t, client, s)
	ready, err := WaitReady(t.Context(), client, s.ID)
	if err != nil || ready.Phase != PhaseReady {
		t.Fatalf("server not ready: %+v %v", ready, err)
	}
	before, _ := client.CoreV1().PersistentVolumeClaims(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	stopping, err := Stop(t.Context(), client, s.ID)
	if err != nil || stopping.Phase != PhaseStopping || stopping.DesiredReplicas != 0 {
		t.Fatalf("Stop must not claim active pods have stopped: %+v %v", stopping, err)
	}
	if _, err := WaitReady(t.Context(), client, s.ID); err == nil {
		t.Fatal("WaitReady accepted a stopping session")
	}
	pod, _ := client.CoreV1().Pods(s.Namespace).Get(t.Context(), "prometheus-0", metav1.GetOptions{})
	pod.DeletionTimestamp = ptr(metav1.Now())
	if _, err := client.CoreV1().Pods(s.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	observed, err := Status(t.Context(), client, s.ID)
	if err != nil || observed.Phase != PhaseStopping {
		t.Fatalf("terminating pod must still count as active: %+v %v", observed, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	observed, err = WaitStopped(ctx, client, s.ID)
	if !errors.Is(err, context.DeadlineExceeded) || observed.Phase != PhaseStopping {
		t.Fatalf("wait did not return last observation and deadline: %+v %v", observed, err)
	}
	if err := client.CoreV1().Pods(s.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	stopped, err := WaitStopped(t.Context(), client, s.ID)
	if err != nil || stopped.Phase != PhaseStopped {
		t.Fatalf("session did not become stopped: %+v %v", stopped, err)
	}
	if _, err := Stop(t.Context(), client, s.ID); err != nil {
		t.Fatalf("repeated Stop failed: %v", err)
	}
	after, _ := client.CoreV1().PersistentVolumeClaims(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if !reflect.DeepEqual(before, after) || len(stopped.Inventory) != 6 {
		t.Fatal("Stop changed PVC or removed session resources")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource != "pods" {
			t.Fatal("Stop deleted evidence")
		}
		if action.GetVerb() == "patch" {
			var patch []map[string]any
			if err := json.Unmarshal(action.(clienttesting.PatchAction).GetPatch(), &patch); err != nil {
				t.Fatal(err)
			}
			if len(patch) != 3 || patch[0]["path"] != "/metadata/uid" || patch[0]["value"] != "statefulsets-prometheus" || patch[1]["path"] != "/metadata/resourceVersion" || patch[1]["value"] != "1" {
				t.Fatalf("Stop missing object preconditions: %+v", patch)
			}
		}
	}
}

func TestStopRejectsReplacedStatefulSet(t *testing.T) {
	for _, field := range []string{"uid", "resourceVersion"} {
		t.Run(field, func(t *testing.T) {
			client, s := started(t)
			client.PrependReactor("patch", "statefulsets", func(action clienttesting.Action) (bool, runtime.Object, error) {
				object, err := client.Tracker().Get(appsv1.SchemeGroupVersion.WithResource("statefulsets"), s.Namespace, resourceName)
				if err != nil {
					return true, nil, err
				}
				sts := object.(*appsv1.StatefulSet)
				if field == "uid" {
					sts.UID = "replacement"
				} else {
					sts.ResourceVersion = "2"
				}
				if err := client.Tracker().Update(action.GetResource(), sts, s.Namespace); err != nil {
					return true, nil, err
				}
				return false, nil, nil
			})
			if _, err := Stop(t.Context(), client, s.ID); err == nil {
				t.Fatal("Stop overwrote changed StatefulSet")
			}
			sts, _ := client.AppsV1().StatefulSets(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
			if *sts.Spec.Replicas != 1 {
				t.Fatal("precondition failure still scaled StatefulSet")
			}
		})
	}
}

func TestCleanupConsentAndPreconditions(t *testing.T) {
	client, s := started(t)
	if _, err := Stop(t.Context(), client, s.ID); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	if err := Cleanup(t.Context(), client, s.ID, false); err == nil || len(client.Actions()) != 0 {
		t.Fatal("cleanup without consent made API calls")
	}
	client.PrependReactor("delete", "namespaces", func(action clienttesting.Action) (bool, runtime.Object, error) {
		options := action.(clienttesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions == nil || *options.Preconditions.UID != "namespaces-hcp-perf-test-1" || *options.Preconditions.ResourceVersion != "1" {
			t.Fatal("namespace deletion missing UID/resourceVersion preconditions")
		}
		return false, nil, nil
	})
	if err := Cleanup(t.Context(), client, s.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Namespaces().Get(t.Context(), s.Namespace, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("cleanup did not submit namespace deletion")
	}
}

func TestCancellation(t *testing.T) {
	client := testClient()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Start(ctx, client, testSpec(), testConfig); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start ignored cancellation: %v", err)
	}
	if _, err := Status(ctx, client, testSpec().ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Status ignored cancellation: %v", err)
	}
	if _, err := Stop(ctx, client, testSpec().ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop ignored cancellation: %v", err)
	}
	if err := Cleanup(ctx, client, testSpec().ID, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("Cleanup ignored cancellation: %v", err)
	}
	if _, err := WaitReady(ctx, client, testSpec().ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitReady ignored cancellation: %v", err)
	}
	if len(client.Actions()) != 0 {
		t.Fatal("canceled operation made API calls")
	}
	client, s := started(t)
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	last, err := WaitReady(ctx, client, s.ID)
	if !errors.Is(err, context.DeadlineExceeded) || last.Phase != PhasePending {
		t.Fatalf("WaitReady did not preserve observation on timeout: %+v %v", last, err)
	}
	client = testClient()
	client.PrependReactor("create", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.Canceled
	})
	last, err = Start(t.Context(), client, testSpec(), testConfig)
	if !errors.Is(err, context.Canceled) || len(last.Inventory) != 2 {
		t.Fatalf("cancellation lost partial inventory: %+v %v", last, err)
	}
}

func TestStatusDiagnosticsAndNoConfigExport(t *testing.T) {
	client, s := started(t)
	sts, err := client.AppsV1().StatefulSets(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CoreV1().Pods(s.Namespace).Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "prometheus-0", Namespace: s.Namespace, Labels: labels(s.ID),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(sts, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}},
		Status: corev1.PodStatus{Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "SECRET"}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: resourceName, RestartCount: 3,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "SECRET"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 2, Message: "SECRET"}},
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		_, err := client.CoreV1().Events(s.Namespace).Create(t.Context(), &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("event-%02d", i), Namespace: s.Namespace},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "prometheus-0"},
			Type:           "Warning", Reason: "FailedScheduling", Message: "SECRET", Count: 1,
			LastTimestamp: metav1.NewTime(time.Unix(int64(i), 0)),
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	observed, err := Status(t.Context(), client, s.ID)
	if err != nil || observed.Phase != PhaseFailed || len(observed.Pods) != 1 || len(observed.Events) != MaxEvents {
		t.Fatalf("missing failure diagnostics: %+v %v", observed, err)
	}
	if observed.Events[0].Time.Unix() != 29 || observed.Events[MaxEvents-1].Time.Unix() != 10 || observed.Pods[0].Conditions[0].Reason != "Unschedulable" || *observed.Pods[0].Containers[0].LastExitCode != 2 {
		t.Fatalf("incorrect diagnostic details: %+v", observed)
	}
	data, _ := json.Marshal(observed)
	for _, sensitive := range []string{"SECRET", "scrape_configs", "localhost:9090", "prometheus.yml", "spec.json"} {
		if strings.Contains(string(data), sensitive) {
			t.Fatalf("status leaks configuration/diagnostic message: %s", data)
		}
	}
}

func TestReadinessRequiresObservedPodAndStorage(t *testing.T) {
	for _, mutation := range []string{"no pods", "unbound PVC", "unobserved generation", "terminating pod", "unready pod"} {
		t.Run(mutation, func(t *testing.T) {
			client, s := started(t)
			makeReady(t, client, s)
			switch mutation {
			case "no pods":
				if err := client.CoreV1().Pods(s.Namespace).Delete(t.Context(), "prometheus-0", metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			case "unbound PVC":
				pvc, _ := client.CoreV1().PersistentVolumeClaims(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
				pvc.Status.Phase = corev1.ClaimPending
				if _, err := client.CoreV1().PersistentVolumeClaims(s.Namespace).UpdateStatus(t.Context(), pvc, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "unobserved generation":
				sts, _ := client.AppsV1().StatefulSets(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
				sts.Generation++
				if _, err := client.AppsV1().StatefulSets(s.Namespace).Update(t.Context(), sts, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			default:
				pod, _ := client.CoreV1().Pods(s.Namespace).Get(t.Context(), "prometheus-0", metav1.GetOptions{})
				if mutation == "terminating pod" {
					pod.DeletionTimestamp = ptr(metav1.Now())
				} else {
					pod.Status.Conditions[0].Status = corev1.ConditionFalse
				}
				if _, err := client.CoreV1().Pods(s.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			observed, err := Status(t.Context(), client, s.ID)
			if err != nil || observed.Phase != PhasePending {
				t.Fatalf("incorrect readiness: %+v %v", observed, err)
			}
		})
	}
}

func TestExample(t *testing.T) {
	data, err := os.ReadFile("../../examples/capture.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("example spec invalid: %v", err)
	}
}

func TestOperationErrorUsesFixedClassifications(t *testing.T) {
	for _, classification := range []string{
		"NotFound", "Forbidden", "Unauthorized", "AlreadyExists", "Conflict", "Invalid",
		"BadRequest", "TooManyRequests", "Timeout", "ServerTimeout", "ServiceUnavailable", "InternalError", "Gone",
	} {
		t.Run(classification, func(t *testing.T) {
			serverError := &apierrors.StatusError{ErrStatus: metav1.Status{
				Reason: metav1.StatusReason(classification), Message: "SECRET",
				Details: &metav1.StatusDetails{Name: "SECRET"},
			}}
			got := operationError("test operation", fmt.Errorf("SECRET: %w", serverError))
			want := "test operation: Kubernetes API request failed (" + classification + ")"
			if got.Error() != want {
				t.Fatalf("unsafe or incorrect classification: got %q want %q", got, want)
			}
		})
	}
	for _, code := range []int32{0, 403, 500} {
		client := testClient()
		client.PrependReactor("create", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, &apierrors.StatusError{ErrStatus: metav1.Status{
				Reason: metav1.StatusReason("SECRET_CONFIG_IN_CUSTOM_REASON"), Message: "SECRET_MESSAGE", Code: code,
			}}
		})
		_, err := Start(t.Context(), client, testSpec(), testConfig)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("custom server reason leaked through Start: %v", err)
		}
		if code == 0 && err.Error() != "create namespace (existing namespaces are never adopted): Kubernetes API request failed (Unknown)" {
			t.Fatalf("unrecognized reason must have a fixed fallback: %v", err)
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		err := operationError("test operation", fmt.Errorf("SECRET: %w", cause))
		if !errors.Is(err, cause) || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("cancellation lost or leaked wrapper: %v", err)
		}
	}
}

func TestInvalidIDOnExistingSessionOperations(t *testing.T) {
	client := testClient()
	if _, err := Status(t.Context(), client, "../default"); err == nil {
		t.Fatal("Status accepted an invalid ID")
	}
	if _, err := Stop(t.Context(), client, "../default"); err == nil {
		t.Fatal("Stop accepted an invalid ID")
	}
	if err := Cleanup(t.Context(), client, "../default", true); err == nil {
		t.Fatal("Cleanup accepted an invalid ID")
	}
	if len(client.Actions()) != 0 {
		t.Fatal("invalid ID made Kubernetes requests")
	}
}

func TestStatusReadErrorsReturnPartialInventory(t *testing.T) {
	for _, resource := range []string{"namespaces", "serviceaccounts", "configmaps", "services", "statefulsets", "persistentvolumeclaims", "pods", "events"} {
		t.Run(resource, func(t *testing.T) {
			client, s := started(t)
			client.PrependReactor("*", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, resourceName, errors.New("SECRET"))
			})
			observed, err := Status(t.Context(), client, s.ID)
			if err == nil || strings.Contains(err.Error(), "SECRET") || observed.Phase == PhaseReady {
				t.Fatalf("read failure hidden or leaked: %+v %v", observed, err)
			}
			if resource != "namespaces" && len(observed.Inventory) == 0 {
				t.Fatal("lost partial inventory")
			}
		})
	}
}

func TestEventPagination(t *testing.T) {
	client, s := started(t)
	pages := 0
	client.PrependReactor("list", "events", func(action clienttesting.Action) (bool, runtime.Object, error) {
		options := action.(clienttesting.ListActionImpl).ListOptions
		if options.Limit != 100 || (pages == 1 && options.Continue != "next") {
			t.Fatalf("unexpected event pagination: %+v", options)
		}
		pages++
		list := &corev1.EventList{}
		if pages == 1 {
			list.Continue = "next"
		} else if pages != 2 {
			t.Fatal("unexpected extra page")
		}
		for i := range 25 {
			list.Items = append(list.Items, corev1.Event{
				LastTimestamp: metav1.NewTime(time.Unix(int64(pages*100+i), 0)),
				Reason:        "FailedScheduling", Count: 1,
			})
		}
		return true, list, nil
	})
	observed, err := Status(t.Context(), client, s.ID)
	if err != nil || pages != 2 || len(observed.Events) != MaxEvents || observed.Events[0].Time.Unix() != 224 || observed.Events[MaxEvents-1].Time.Unix() != 205 {
		t.Fatalf("incorrect bounded paginated events: %+v %v", observed.Events, err)
	}
}

func TestStopPostMutationReadFailure(t *testing.T) {
	client, s := started(t)
	patched := false
	client.PrependReactor("patch", "statefulsets", func(clienttesting.Action) (bool, runtime.Object, error) {
		patched = true
		return false, nil, nil
	})
	client.PrependReactor("get", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
		if patched {
			return true, nil, context.Canceled
		}
		return false, nil, nil
	})
	observed, err := Stop(t.Context(), client, s.ID)
	if !errors.Is(err, context.Canceled) || observed.DesiredReplicas != 0 || observed.Phase != PhaseStopping || len(observed.Inventory) != 2 {
		t.Fatalf("lost successful scale-down on observation error: %+v %v", observed, err)
	}
}

func ExampleStart() {
	// A production caller supplies a kubernetes.Interface built from its own
	// explicitly selected kubeconfig. A fake keeps this example offline.
	client := fake.NewClientset()
	spec := Spec{
		Version: "v1", ID: "example",
		// Placeholder: replace with a verified Prometheus server image digest.
		Image:        "quay.io/prometheus/prometheus@sha256:" + strings.Repeat("0", 64),
		StorageClass: "managed-premium", StorageGi: 32, RetentionHours: 24,
		CPURequestMilli: 500, MemoryRequestMi: 1024, MemoryLimitMi: 2048,
		KubernetesDiscovery: false,
	}
	// Configuration is supplied separately and stored as a ConfigMap, not a
	// Secret. This job scrapes only Prometheus itself, not cluster metrics.
	config := "scrape_configs:\n  - job_name: self\n    static_configs:\n      - targets: ['localhost:9090']\n"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Start(ctx, client, spec, config)
	if err != nil {
		fmt.Println("inspect retained inventory:", s.Inventory, err)
		return
	}
	fmt.Println(s.Namespace, s.Phase, s.DesiredReplicas, s.ReadyReplicas)
	// Output: hcp-perf-example pending 1 0
}

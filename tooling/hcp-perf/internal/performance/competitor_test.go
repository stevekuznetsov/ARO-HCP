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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/adversary"
)

func TestStoppedWorkersRequireFrozenJobs(t *testing.T) {
	for _, mode := range []string{"valid", "uid", "pods", "missing", "running", "spec"} {
		t.Run(mode, func(t *testing.T) {
			run := adversary.RunSpec{ID: "case", Namespace: "capture", TargetNamespace: "hcp", CPUFillPercent: 90}
			nodes := []adversary.NodePlan{{Name: "node", CPURequestMilli: 100}}
			spec := Spec{Competitor: &Competitor{Run: run, Nodes: nodes}}
			spec.Competitor.Run.TargetNamespace = ""
			status := adversary.RunStatus{RegistrationUID: "registration", Spec: run, Plan: adversary.Result{Nodes: nodes}, Phase: "stopped", StopRequested: true, Jobs: []adversary.JobStatus{{Name: "job", UID: "uid", Node: "node", Phase: "suspended"}}}
			switch mode {
			case "uid":
				status.Jobs[0].UID = "replacement"
			case "pods":
				status.NonterminalPods = 1
			case "missing":
				status.Jobs = nil
			case "running":
				status.StopRequested = false
			case "spec":
				status.Spec.ID = "other"
			}
			err := stoppedWorkers(status, spec, []competitorWorker{{Job: "job", JobUID: "uid", Node: "node"}}, "hcp")
			if (err == nil) != (mode == "valid") {
				t.Fatalf("%s: %v", mode, err)
			}
		})
	}
}

func TestFinalizerRepeatsStopWithinBudget(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-a", UID: "node-uid", Labels: map[string]string{"aro-hcp.azure.com/role": "worker", "kubernetes.azure.com/agentpool": "workers"}},
		Status:     corev1.NodeStatus{Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}, Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi"), corev1.ResourcePods: resource.MustParse("110")}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	target := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "hcp", UID: "target-uid"}, Spec: corev1.PodSpec{NodeName: node.Name}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "capture", Labels: map[string]string{"app.kubernetes.io/managed-by": "hcp-perf"}}}
	client := fake.NewClientset(node, target, namespace)
	client.PrependReactor("create", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject().(metav1.Object)
		object.SetUID(types.UID("uid-" + object.GetName()))
		object.SetResourceVersion("1")
		return false, nil, nil
	})
	spec := adversary.RunSpec{Version: "v1", ID: "fresh", Namespace: "capture", TargetNamespace: "hcp", Image: "registry.example/burn@sha256:" + strings.Repeat("a", 64), CPUFillPercent: 50, MemoryRequestMi: 16, MemoryLimitMi: 32, DurationSeconds: 60}
	identity, err := adversary.Start(t.Context(), client, spec)
	if err != nil {
		t.Fatal(err)
	}
	patches := 0
	client.PrependReactor("patch", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches <= 4 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "jobs"}, action.(clienttesting.PatchAction).GetName(), errors.New("concurrent update"))
		}
		return false, nil, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := stopCompetitor(ctx, client, spec, &identity); err != nil {
		t.Fatal(err)
	}
	if patches != 5 {
		t.Fatalf("expected retry across Stop calls, got %d patches", patches)
	}
	status, err := adversary.Status(t.Context(), client, spec.Namespace, spec.ID)
	if err != nil || status.Phase != "stopped" || status.NonterminalPods != 0 {
		t.Fatalf("cleanup not verified: %+v %v", status, err)
	}
}

func TestCleanupFailureStillAttemptsExport(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "capture unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	spec := testSpec()
	spec.Capture.Endpoint = server.URL
	spec.Competitor = &Competitor{Run: adversary.RunSpec{ID: "run", Namespace: "capture"}}
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "events"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out := Outcome{Started: time.Now().Add(-time.Minute)}
	identity := adversary.RunStatus{RegistrationUID: "registered"}
	err = finalizeCapture(ctx, spec, dir, server.Client(), fake.NewClientset(), true, &identity, &out, &journal{file: f})
	if err == nil || requests != 1 || out.CleanupStatus != "unverified-load-may-remain" || out.MetricsStatus != "partial-or-failed" {
		t.Fatalf("cleanup/export finalization: %+v requests=%d err=%v", out, requests, err)
	}
}

func TestEveryCompetitorWorkerRequired(t *testing.T) {
	for _, mode := range []string{"ready", "one-pending", "unready", "deleting", "extra", "wrong-node", "replaced", "container-stopped"} {
		t.Run(mode, func(t *testing.T) {
			expected := adversary.RunStatus{RegistrationUID: "registration", Plan: adversary.Result{Nodes: []adversary.NodePlan{{Name: "a"}, {Name: "b"}}}}
			for _, node := range []string{"a", "b"} {
				expected.Jobs = append(expected.Jobs, adversary.JobStatus{Name: "job-" + node, Node: node, UID: types.UID("job-" + node)})
			}
			status := expected
			status.Jobs = append([]adversary.JobStatus(nil), expected.Jobs...)
			status.Phase = "running"
			status.PlacementValid = true
			status.NonterminalPods = 2
			for i := range status.Jobs {
				job := &status.Jobs[i]
				job.Phase = "running"
				job.Pods = []adversary.PodStatus{{Name: "pod-" + job.Node, UID: types.UID("pod-" + job.Node), Node: job.Node, Phase: corev1.PodRunning, Ready: true, Containers: []corev1.ContainerStatus{{Name: "cpu-burn", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}}
			}
			job := &status.Jobs[1]
			switch mode {
			case "one-pending":
				job.Phase = "pending"
				job.Pods[0].Phase = corev1.PodPending
			case "unready":
				job.Pods[0].Ready = false
			case "deleting":
				job.Pods[0].Deleting = true
			case "extra":
				job.Pods = append(job.Pods, job.Pods[0])
			case "wrong-node":
				job.Pods[0].Node = "a"
			case "replaced":
				job.UID = "replacement"
			case "container-stopped":
				job.Pods[0].Containers[0].State.Running = nil
			}
			if _, err := runningWorkers(status, expected); (err == nil) != (mode == "ready") {
				t.Fatalf("worker gate: %v", err)
			}
		})
	}
}

func TestWorkerCPUSourceTimestamp(t *testing.T) {
	for _, mode := range []string{"fresh", "stale", "zero", "other-pod", "duplicate", "before-start"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			worker := competitorWorker{Node: "a", Pod: "exact-pod", PodUID: "pod-uid", Started: now.Add(-time.Minute)}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query().Get("query")
				if !strings.Contains(query, `pod="exact-pod"`) || !strings.Contains(query, `container="cpu-burn"`) {
					t.Error("query not bound to exact worker")
				}
				value := 0.5
				if strings.HasPrefix(query, "timestamp(") {
					value = float64(now.Unix())
					if mode == "stale" {
						value -= 120
					}
					if mode == "before-start" {
						value -= 40
					}
				} else if mode == "zero" {
					value = 0
				}
				pod := "exact-pod"
				if mode == "other-pod" {
					pod = "other"
				}
				point := map[string]any{"metric": map[string]string{"job": "cadvisor", "namespace": "capture", "pod": pod, "container": "cpu-burn"}, "value": []any{now.Unix(), fmt.Sprint(value)}}
				points := []any{point}
				if mode == "duplicate" {
					points = append(points, point)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": points}})
			}))
			defer server.Close()
			spec := testSpec()
			spec.Capture.Endpoint = server.URL
			spec.Competitor = &Competitor{Run: adversary.RunSpec{Namespace: "capture"}, CPUJob: "cadvisor"}
			f, err := os.OpenFile(filepath.Join(t.TempDir(), "events"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if mode == "before-start" {
				worker.Started = now.Add(-20 * time.Second)
			}
			err = workerCPU(t.Context(), server.Client(), spec, worker, &journal{file: f})
			if (err == nil) != (mode == "fresh") {
				t.Fatalf("source freshness: %v", err)
			}
		})
	}
}

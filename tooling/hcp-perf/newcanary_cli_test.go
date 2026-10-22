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

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/requesttrace"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func TestCanaryCLIExpectationsOverHTTP(t *testing.T) {
	const (
		deploymentPath = "/apis/apps/v1/namespaces/guest/deployments"
		setPath        = "/apis/apps/v1/namespaces/guest/replicasets"
		podPath        = "/api/v1/namespaces/guest/pods"
		scaleDelay     = 10 * time.Millisecond
	)
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "canary", Namespace: "guest", UID: "deployment", ResourceVersion: "1", Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(0)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "canary"}}},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1},
	}
	set := &appsv1.ReplicaSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "canary-rs", Namespace: "guest", UID: "rs", ResourceVersion: "1",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))}},
		Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(0))},
	}
	var pods []corev1.Pod
	var writes []int32
	var mu sync.Mutex
	streams := map[string]map[chan watch.Event]bool{}
	lists, watches := map[string]int{}, map[string]int{}
	closed := make(chan struct{}, 32)
	shutdown := make(chan struct{})
	kubeconfig := clusterKubeconfig(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Query().Get("watch") == "true" {
			switch r.URL.Path {
			case deploymentPath, setPath, podPath:
			default:
				t.Errorf("unexpected watch: %s", r.URL)
				http.NotFound(w, r)
				return
			}
			stream := make(chan watch.Event, 32)
			mu.Lock()
			if r.URL.Query().Get("resourceVersion") != deployment.ResourceVersion {
				t.Errorf("watch did not resume from LIST: %s", r.URL)
			}
			if streams[r.URL.Path] == nil {
				streams[r.URL.Path] = map[chan watch.Event]bool{}
			}
			streams[r.URL.Path][stream] = true
			watches[r.URL.Path]++
			mu.Unlock()
			defer func() {
				mu.Lock()
				delete(streams[r.URL.Path], stream)
				mu.Unlock()
				closed <- struct{}{}
			}()
			// Flush the handshake before waiting for scale-triggered events.
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-shutdown:
					return
				case event := <-stream:
					if err := json.NewEncoder(w).Encode(struct {
						Type   watch.EventType `json:"type"`
						Object runtime.Object  `json:"object"`
					}{event.Type, event.Object}); err != nil {
						return
					}
					w.(http.Flusher).Flush()
				}
			}
		}

		var scale autoscalingv1.Scale
		if r.Method == http.MethodPut && r.URL.Path == deploymentPath+"/canary/scale" {
			// The actual typed client can send protobuf even with JSON responses.
			data, err := io.ReadAll(r.Body)
			if err == nil {
				err = runtime.DecodeInto(scheme.Codecs.UniversalDeserializer(), data, &scale)
			}
			if err != nil {
				t.Errorf("decode scale: %v", err)
				http.Error(w, "invalid scale", http.StatusBadRequest)
				return
			}
			select {
			case <-time.After(scaleDelay):
			case <-r.Context().Done():
				return
			}
		}
		response := func() runtime.Object {
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodGet {
				listMeta := metav1.ListMeta{ResourceVersion: deployment.ResourceVersion}
				switch r.URL.Path {
				case deploymentPath:
					if r.URL.Query().Get("fieldSelector") != "metadata.name=canary" {
						t.Errorf("deployment LIST lacks name selector: %s", r.URL)
					}
					lists[r.URL.Path]++
					return &appsv1.DeploymentList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"}, ListMeta: listMeta, Items: []appsv1.Deployment{*deployment.DeepCopy()}}
				case setPath:
					lists[r.URL.Path]++
					return &appsv1.ReplicaSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSetList"}, ListMeta: listMeta, Items: []appsv1.ReplicaSet{*set.DeepCopy()}}
				case podPath:
					lists[r.URL.Path]++
					return (&corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, ListMeta: listMeta, Items: pods}).DeepCopy()
				case deploymentPath + "/canary":
					return deployment.DeepCopy()
				}
			}
			if r.Method != http.MethodPut || r.URL.Path != deploymentPath+"/canary/scale" {
				return nil
			}
			if scale.Name != deployment.Name || scale.Namespace != deployment.Namespace || scale.UID != deployment.UID || scale.ResourceVersion != deployment.ResourceVersion {
				t.Errorf("missing scale identity/resourceVersion guard: %+v", scale.ObjectMeta)
				return nil
			}
			replicas := scale.Spec.Replicas
			if replicas != int32(1-len(writes)%2) {
				t.Errorf("unexpected scale sequence: %v then %d", writes, replicas)
				return nil
			}
			writes = append(writes, replicas)
			deployment.Generation++
			deployment.ResourceVersion = fmt.Sprint(deployment.Generation)
			deployment.Spec.Replicas = ptr.To(replicas)
			deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: deployment.Generation, Replicas: replicas, UpdatedReplicas: replicas, ReadyReplicas: replicas, AvailableReplicas: replicas}
			set.ResourceVersion = deployment.ResourceVersion
			set.Spec.Replicas, set.Status.Replicas = ptr.To(replicas), replicas
			// Queue immutable snapshots under the lock; only watch handlers write HTTP.
			publish := func(path string, eventType watch.EventType, object runtime.Object) {
				for stream := range streams[path] {
					select {
					case stream <- watch.Event{Type: eventType, Object: object}:
					default:
						t.Errorf("watch buffer overflow for %s", path)
					}
				}
			}
			publish(setPath, watch.Modified, set.DeepCopy())
			if replicas == 1 {
				name := fmt.Sprintf("canary-pod-%d", deployment.Generation)
				pods = []corev1.Pod{{
					TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "guest", UID: types.UID(name), ResourceVersion: deployment.ResourceVersion,
						Labels: map[string]string{"app": "canary"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(set, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}},
					Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
				}}
				publish(podPath, watch.Added, pods[0].DeepCopy())
			} else {
				for _, pod := range pods {
					publish(podPath, watch.Deleted, pod.DeepCopy())
				}
				pods = nil
			}
			publish(deploymentPath, watch.Modified, deployment.DeepCopy())
			return &autoscalingv1.Scale{
				TypeMeta: metav1.TypeMeta{APIVersion: "autoscaling/v1", Kind: "Scale"},
				ObjectMeta: metav1.ObjectMeta{Name: deployment.Name, Namespace: deployment.Namespace,
					UID: deployment.UID, ResourceVersion: deployment.ResourceVersion},
				Spec: scale.Spec, Status: autoscalingv1.ScaleStatus{Replicas: replicas},
			}
		}()
		if response == nil {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	})
	t.Cleanup(func() { close(shutdown) })

	for cycle, tc := range []struct {
		name     string
		flags    []string
		expected [3]time.Duration
	}{
		{"degraded", []string{"--ack-expected=1ms", "--ready-expected=2ms", "--cleanup-expected=3ms"}, [3]time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}},
		{"continues after degradation", []string{"--ack-expected=1ms", "--ready-expected=2ms", "--cleanup-expected=3ms"}, [3]time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}},
		{"omitted expectations", nil, [3]time.Duration{}},
		{"explicit zero expectations", []string{"--ack-expected=0s", "--ready-expected=0s", "--cleanup-expected=0s"}, [3]time.Duration{}},
	} {
		if !t.Run(tc.name, func(t *testing.T) {
			args := []string{"canary", "run", "--namespace=guest", "--deployment=canary", "--kubeconfig", kubeconfig, "--timeout=10s", "--ack-budget=1s", "--ready-budget=2s", "--cleanup-budget=2s"}
			requestLog := filepath.Join(t.TempDir(), "requests.ndjson")
			if cycle == 0 {
				args = append(args, "--request-log", requestLog)
			}
			var out bytes.Buffer
			if err := run(t.Context(), append(args, tc.flags...), &out); err != nil {
				t.Fatalf("canary run: %v; output: %s", err, &out)
			}
			var result workload.Result
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatalf("decode CLI result: %v; output: %s", err, &out)
			}
			if cycle == 0 {
				data, err := os.ReadFile(requestLog)
				if err != nil {
					t.Fatal(err)
				}
				decoder := json.NewDecoder(bytes.NewReader(data))
				reads, writes := 0, 0
				for decoder.More() {
					var o requesttrace.Observation
					if err := decoder.Decode(&o); err != nil {
						t.Fatal(err)
					}
					if o.Namespace != "guest" || o.Kind != "headerReceived" || o.StatusCode != 200 || strings.Contains(o.Path, "?") {
						t.Fatalf("invalid canary HTTP observation: %+v", o)
					}
					if o.Method == "PUT" && o.Path == deploymentPath+"/canary/scale" {
						writes++
					} else if o.Method == "GET" {
						reads++
					}
				}
				if reads != 13 || writes != 2 {
					t.Fatalf("missing canary HTTP evidence: reads=%d writes=%d", reads, writes)
				}
			}
			performance := "not_evaluated"
			if tc.expected[0] != 0 {
				performance = "degraded"
			}
			if result.Performance != performance || result.Degraded != (performance == "degraded") {
				t.Fatalf("incorrect performance assessment: %s", &out)
			}
			for i, stage := range []workload.Stage{result.Ack, result.Ready, result.Cleanup} {
				deadline := []time.Duration{time.Second, 2 * time.Second, 2 * time.Second}[i]
				// JSON drops monotonic clock readings, so wall-clock subtraction can differ slightly.
				timingDelta := stage.Duration - stage.Completed.Sub(stage.Started)
				if stage.Started.IsZero() || stage.Completed.IsZero() || timingDelta.Abs() > time.Millisecond || stage.Duration < scaleDelay || stage.Duration > deadline || stage.Deadline != deadline || stage.ExpectedDuration != tc.expected[i] || stage.ExpectationExceeded != (tc.expected[i] > 0) {
					t.Errorf("stage %d lost actual timing or CLI thresholds: %+v", i, stage)
				}
			}
			generation := int64(2 + 2*cycle)
			if result.Ack.Attempts != 1 || result.Cleanup.Attempts != 1 || result.DeploymentUID != "deployment" || result.Generation != generation || result.PodUID != types.UID(fmt.Sprintf("canary-pod-%d", generation)) {
				t.Errorf("incorrect cycle identity or writes: %s", &out)
			}
			if result.Preflight.Completed.IsZero() || result.Preflight.Completed.After(result.Ack.Started) || !result.Ack.Started.Equal(result.Ready.Started) || result.Cleanup.Started.Before(result.Ready.Completed) {
				t.Errorf("incorrect phase ordering: %s", &out)
			}
			if len(result.Last.Pods) != 1 || result.Last.Deployment.Ready != 1 || len(result.CleanupLast.Pods) != 0 || result.CleanupLast.Deployment.Desired != 0 || result.CleanupLast.Deployment.Ready != 0 || result.CleanupLast.Deployment.ObservedGeneration != generation+1 {
				t.Errorf("readiness or cleanup snapshot lost: %s", &out)
			}
			// Readiness can use the listed RS ownership; cleanup requires all three updates.
			if result.MeasurementMode != "watch" || result.Watch == nil || result.Watch.Events < 5 || result.Watch.Events > 6 || result.Watch.Gaps != 0 || result.Watch.Reconnects != 0 || len(result.Watch.Errors) != 0 {
				t.Errorf("expected gap-free streaming measurements: %s", &out)
			}
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			for range 6 {
				select {
				case <-closed:
				case <-deadline.C:
					t.Fatal("canary left HTTP watches open")
				}
			}
			mu.Lock()
			defer mu.Unlock()
			for _, path := range []string{deploymentPath, setPath, podPath} {
				if lists[path] != 2*(cycle+1) || watches[path] != 2*(cycle+1) || len(streams[path]) != 0 {
					t.Errorf("%s: lists=%d watches=%d active=%d", path, lists[path], watches[path], len(streams[path]))
				}
			}
		}) {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(writes, []int32{1, 0, 1, 0, 1, 0, 1, 0}) || *deployment.Spec.Replicas != 0 || *set.Spec.Replicas != 0 || len(pods) != 0 {
		t.Fatalf("cycles did not restore reusable fixture: writes=%v deployment=%+v set=%+v pods=%v", writes, deployment.Spec.Replicas, set.Spec.Replicas, pods)
	}
}

func TestCanaryCLIExpectationFlagValidation(t *testing.T) {
	var calls atomic.Int64
	kubeconfig := clusterKubeconfig(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected API call", http.StatusBadRequest)
	})
	for _, flag := range []string{"ack-expected", "ready-expected", "cleanup-expected"} {
		for _, tc := range []struct {
			name, value, want string
		}{
			{"negative", "=-1ms", "expectations must be nonnegative"},
			{"above hard bound", "=3s", "at most their respective hard deadlines"},
			{"malformed", "=bad", "invalid value"},
			{"missing value", "", "flag needs an argument"},
		} {
			t.Run(flag+"/"+tc.name, func(t *testing.T) {
				args := []string{"canary", "run", "--namespace=guest", "--deployment=canary", "--kubeconfig", kubeconfig, "--ack-budget=1s", "--ready-budget=2s", "--cleanup-budget=2s", "--" + flag + tc.value}
				err := run(t.Context(), args, io.Discard)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("run(%v) = %v, want %q", args, err, tc.want)
				}
			})
		}
		t.Run(flag+"/inappropriate command", func(t *testing.T) {
			var out bytes.Buffer
			err := run(t.Context(), []string{"capture", "status", "--id=example", "--kubeconfig", kubeconfig, "--" + flag + "=1ms"}, &out)
			if err == nil || !strings.Contains(err.Error(), "--"+flag+" does not apply") || out.Len() != 0 {
				t.Fatalf("inappropriate flag: err=%v output=%s", err, &out)
			}
		})
	}
	var out bytes.Buffer
	err := run(t.Context(), []string{"canary", "run", "--kubeconfig", kubeconfig, "--unknown-expected=1ms"}, &out)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") || out.Len() != 0 {
		t.Fatalf("unknown flag: err=%v output=%s", err, &out)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("invalid flags caused %d API requests", n)
	}
}

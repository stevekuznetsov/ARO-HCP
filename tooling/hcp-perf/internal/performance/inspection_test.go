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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

func TestGuardLargeInventoryAvoidsSerialControllerReads(t *testing.T) {
	clients, spec, id, pod, deployment := guardFixture(t)
	kube := clients.Kubernetes.(*kubefake.Clientset)
	base := spec.Startup.Components[0]
	for i := 1; i < 47; i++ {
		component := base
		component.Name = fmt.Sprintf("controller-%d", i)
		spec.Startup.Components = append(spec.Startup.Components, component)
		d := deployment.DeepCopy()
		d.Name, d.UID = component.Name, types.UID(component.Name)
		d.Spec.Selector.MatchLabels = map[string]string{"app": component.Name}
		p := pod.DeepCopy()
		p.Name, p.UID, p.Labels = component.Name, types.UID(component.Name), d.Spec.Selector.MatchLabels
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: component.Name, UID: d.UID, Controller: ptr.To(true)}}
		rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: "control", UID: d.UID, OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: ptr.To(true)}}}}
		for _, object := range []runtime.Object{d, p, rs} {
			if err := kube.Tracker().Add(object); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A serial second read of 47 controllers costs >=940ms, beyond this
	// scaled 500ms cycle. Batched inventory remains complete under the same bound.
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	kube.PrependReactor("get", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		if err := wait(ctx, 20*time.Millisecond); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	sample, err := (&safety{}).check(ctx, clients, spec, id, false)
	if err != nil || !sample.Healthy || !sample.Coverage.Complete || sample.Coverage.ObservedWorkloads != 47 || sample.Coverage.ReadyPods != 47 {
		t.Fatalf("large inventory did not complete: %+v, %v", sample, err)
	}
	for _, action := range kube.Actions() {
		if action.Matches("get", "deployments") {
			t.Fatal("redundant per-controller GET consumed inspection budget")
		}
	}
	// Reproduce the old second-pass algorithm against the same inventory and
	// per-request delay. This is a synthetic deadline reproduction, not proof
	// of the initiating error in the historical live run.
	legacyCtx, stop := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer stop()
	ctx = legacyCtx
	var legacyErr error
	for _, component := range spec.Startup.Components {
		_, legacyErr = kube.AppsV1().Deployments("control").Get(legacyCtx, component.Name, metav1.GetOptions{})
		if legacyErr != nil {
			break
		}
	}
	if !errors.Is(legacyErr, context.DeadlineExceeded) {
		t.Fatalf("serial deadline reproduction failed: %v", legacyErr)
	}
	t.Log("47-controller snapshot passed 500ms budget; legacy 20ms/controller second pass exhausted it")
}

func TestGuardInspectionRetainsDeadlineStage(t *testing.T) {
	for _, stage := range []string{"deployments", "events", "restart-pods", "restart-owners"} {
		t.Run(stage, func(t *testing.T) {
			clients, spec, id, _, _ := guardFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			resource, occurrence := stage, 1
			if stage == "restart-pods" {
				resource, occurrence = "pods", 2
			}
			if stage == "restart-owners" {
				resource, occurrence = "replicasets", 2
			}
			calls := 0
			clients.Kubernetes.(*kubefake.Clientset).PrependReactor("list", resource, func(ktesting.Action) (bool, runtime.Object, error) {
				calls++
				if calls != occurrence {
					return false, nil, nil
				}
				<-ctx.Done()
				return true, nil, ctx.Err()
			})
			_, err := (&safety{}).check(ctx, clients, spec, id, false)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), strings.Split(stage, "-")[0]) {
				t.Fatalf("lost initiating inspection stage/cause: %v", err)
			}
		})
	}
}

func TestGuardFailureCancelsBeforePersistenceAndRetainsFirstCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	failure := &guardFailure{}
	cause := fmt.Errorf("restart owner inspection failed: %w", context.DeadlineExceeded)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	dir := t.TempDir()
	go func() {
		defer close(done)
		failure.fail(ctx, cancel, cause, func(value *GuardFailure) error {
			close(entered)
			<-release
			return publish(dir, "guard-failure.json", value)
		})
	}()
	<-entered
	if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) || ctx.Err() != context.Canceled {
		t.Error("persistence delayed cancellation or lost cause")
	}
	close(release)
	<-done
	failure.fail(ctx, cancel, errors.New("capture canceled"), func(*GuardFailure) error {
		t.Error("secondary failure replaced initiating cause")
		return nil
	})
	data, err := os.ReadFile(filepath.Join(dir, "guard-failure.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved GuardFailure
	if err := json.Unmarshal(data, &saved); err != nil || saved.Error != cause.Error() || saved.At.IsZero() || !errors.Is(failure.err, context.DeadlineExceeded) {
		t.Fatalf("cause not persisted: %s, %v", data, err)
	}
	out := Outcome{GuardFailure: failure.failure, Error: errors.Join(errors.New("seed failed; population retained"), failure.err).Error()}
	if err := publish(dir, "outcome.json", out); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(dir, "outcome.json"))
	if err != nil || !strings.Contains(string(data), cause.Error()) {
		t.Fatalf("outcome lost cause: %s, %v", data, err)
	}
}

func TestGuardFailurePersistenceErrorDoesNotMaskCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	g := &guardFailure{}
	writeErr := errors.New("evidence disk failure")
	g.fail(ctx, cancel, context.DeadlineExceeded, func(*GuardFailure) error { return writeErr })
	if !errors.Is(g.err, writeErr) || !errors.Is(g.err, context.DeadlineExceeded) || context.Cause(ctx) != context.DeadlineExceeded {
		t.Fatalf("lost original or persistence failure: %v", g.err)
	}
}

func TestGuardInspectionErrorsRemainFatal(t *testing.T) {
	for _, resource := range []string{"deployments", "statefulsets", "daemonsets", "endpoints", "pods", "replicasets", "events"} {
		t.Run(resource, func(t *testing.T) {
			clients, spec, id, _, _ := guardFixture(t)
			spec.Startup.Components[0].Endpoints = "serving"
			if err := clients.Kubernetes.(*kubefake.Clientset).Tracker().Add(&corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "serving", Namespace: "control"}}); err != nil {
				t.Fatal(err)
			}
			denied := apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "serving", errors.New("denied"))
			clients.Kubernetes.(*kubefake.Clientset).PrependReactor("*", resource, func(ktesting.Action) (bool, runtime.Object, error) {
				return true, nil, denied
			})
			_, err := (&safety{}).check(t.Context(), clients, spec, id, false)
			if !errors.Is(err, denied) {
				t.Fatalf("inspection error bypassed or lost: %v", err)
			}
		})
	}
}

func TestCriticalMonitorIndependentOfBlockedProfile(t *testing.T) {
	clients, spec, id, _, _ := guardFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, profileDone, criticalDone := make(chan struct{}), make(chan error, 1), make(chan struct{})
	clients.Kubernetes.(*kubefake.Clientset).PrependReactor("list", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		close(entered)
		<-ctx.Done()
		return true, nil, ctx.Err()
	})
	go func() {
		_, err := (&safety{}).check(ctx, clients, spec, id, false)
		profileDone <- err
	}()
	<-entered
	monitor := &criticalMonitor{}
	var calls atomic.Int32
	go func() {
		defer close(criticalDone)
		monitor.run(ctx, 20*time.Millisecond, 10*time.Millisecond, 80*time.Millisecond, func(context.Context) CriticalSample {
			calls.Add(1)
			return CriticalSample{Finished: time.Now(), Ready: map[string]int{"kube-apiserver": 3, "etcd": 3, "authenticated-guest-livez": 2}}
		}, func(CriticalSample) error { return nil }, func(err error) { t.Error(err); cancel() })
	}()
	time.Sleep(110 * time.Millisecond)
	cancel()
	<-criticalDone
	if err := <-profileDone; !errors.Is(err, context.Canceled) || calls.Load() < 4 {
		t.Fatalf("profile blocked critical monitor or lost cancellation: calls=%d err=%v", calls.Load(), err)
	}
}

type inspectionTransport func(*http.Request) (*http.Response, error)

func (f inspectionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNodeInspectionBoundedConcurrencyAndCancellation(t *testing.T) {
	for _, mode := range []string{"slow", "blocked", "pressure", "not-ready", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			var active, peak, completed, requests atomic.Int32
			firstWave := make(chan struct{})
			kube, err := kubernetes.NewForConfig(&rest.Config{Host: "https://offline.invalid", Transport: inspectionTransport(func(r *http.Request) (*http.Response, error) {
				if !strings.Contains(r.URL.Path, "/nodes/") {
					return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))}, nil
				}
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				if requests.Add(1) == 4 {
					close(firstWave)
				}
				select {
				case <-firstWave:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				if mode == "blocked" {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				if err := wait(r.Context(), 80*time.Millisecond); err != nil {
					return nil, err
				}
				completed.Add(1)
				if mode == "forbidden" {
					return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`))}, nil
				}
				node := corev1.Node{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
				if mode == "pressure" {
					node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue})
				}
				if mode == "not-ready" {
					node.Status.Conditions = nil
				}
				data, _ := json.Marshal(node)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			pods := make([]corev1.Pod, 8)
			for i := range pods {
				pods[i].Spec.NodeName = fmt.Sprintf("node-%d", i)
			}
			err = (&safety{}).ancillarySafety(ctx, startup.Clients{Kubernetes: kube}, testSpec(), "control", pods, time.Now())
			if (err != nil) != (mode != "slow") || peak.Load() != 4 || active.Load() != 0 {
				t.Fatalf("mode=%s err=%v peak=%d active=%d", mode, err, peak.Load(), active.Load())
			}
			if mode == "blocked" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline cause lost: %v", err)
			}
			if mode == "slow" && completed.Load() != 8 {
				t.Fatal("not all node checks completed")
			}
		})
	}
}

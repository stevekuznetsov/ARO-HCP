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

package workload

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestWatchListsAndRegistrationBeforeMutation(t *testing.T) {
	f := newFixture()
	f.onScale = func(n int32) {
		if len(f.allStreams) != 3*(2-int(n)) {
			t.Fatalf("mutation before all watches registered: %d", len(f.allStreams))
		}
	}
	result, err := RunCanary(t.Context(), f.client, testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if result.MeasurementMode != "watch" || result.Watch.Events == 0 || result.Watch.Gaps != 0 || result.Watch.Reconnects != 0 {
		t.Fatalf("bad diagnostics: %+v", result)
	}
	lists, watches, gets := map[string]int{}, map[string]int{}, 0
	for _, a := range f.client.Actions() {
		if a.GetNamespace() != "guest" {
			t.Fatalf("not namespace scoped: %v", a)
		}
		kind := a.GetResource().Resource
		switch a.GetVerb() {
		case "get":
			gets++
		case "list":
			lists[kind]++
			fields := a.(clienttesting.ListAction).GetListRestrictions().Fields.String()
			if (kind == "deployments" && fields != "metadata.name=canary") || (kind != "deployments" && fields != "") {
				t.Fatalf("incorrect list selector: %v", a)
			}
		case "watch":
			watches[kind]++
			r := a.(clienttesting.WatchAction).GetWatchRestrictions()
			want := map[string]string{"deployments": "deployment-list-rv", "replicasets": "rs-list-rv", "pods": "pod-list-rv"}[kind]
			if r.ResourceVersion != want || (kind == "deployments" && r.Fields.String() != "metadata.name=canary") {
				t.Fatalf("incorrect watch RV/selector: %v", a)
			}
		}
	}
	if gets != 1 {
		t.Fatalf("only cleanup should GET, got %d", gets)
	}
	for _, kind := range []string{"deployments", "replicasets", "pods"} {
		if lists[kind] != 2 || watches[kind] != 2 {
			t.Fatalf("repeated reads during a phase: %v %v", lists, watches)
		}
	}
	for _, stream := range f.allStreams {
		if !stream.IsStopped() {
			t.Fatal("watch leaked")
		}
	}
}

func TestWatchRecordsReadyDuringAck(t *testing.T) {
	f := newFixture()
	client := contextClient{Interface: f.client, update: func(ctx context.Context, scale *autoscalingv1.Scale) (*autoscalingv1.Scale, error) {
		response, err := f.client.AppsV1().Deployments("guest").UpdateScale(ctx, "canary", scale, metav1.UpdateOptions{})
		if scale.Spec.Replicas == 1 {
			// Hold the response after publishing events, not before controller work.
			select {
			case <-time.After(40 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return response, err
	}}
	result, err := RunCanary(t.Context(), client, testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready.Completed.Before(result.Ack.Completed) || result.Ready.Duration != result.Ready.Completed.Sub(result.Ready.Started) || result.Ready.Duration >= 5*time.Second {
		t.Fatalf("ready timestamp delayed until ACK/cleanup: %+v", result)
	}
}

func TestWatchGapFailsEarlyAndReestablishesCleanup(t *testing.T) {
	for _, mode := range []string{"closed", "410", "wrong object", "wrong event"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture()
			f.onScale = func(n int32) {
				if n != 1 {
					return
				}
				switch mode {
				case "closed":
					f.streams["pods"].Stop()
				case "410":
					f.streams["pods"].Error(&metav1.Status{Code: 410, Reason: metav1.StatusReasonExpired, Message: "resource version expired"})
				case "wrong object":
					f.streams["pods"].Add(&appsv1.Deployment{})
				case "wrong event":
					f.streams["pods"].Action(watch.EventType("bad"), &corev1.Pod{})
				}
				f.pods[0].Status.Phase = corev1.PodPending
			}
			client := contextClient{Interface: f.client, update: func(ctx context.Context, scale *autoscalingv1.Scale) (*autoscalingv1.Scale, error) {
				response, err := f.client.AppsV1().Deployments("guest").UpdateScale(ctx, "canary", scale, metav1.UpdateOptions{})
				if scale.Spec.Replicas == 1 {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return response, err
			}}
			spec := testSpec()
			spec.Deadlines.Ack = time.Second
			spec.Deadlines.Ready = time.Second
			result, err := RunCanary(t.Context(), client, spec)
			if err == nil || !strings.Contains(err.Error(), "invalid watch measurement") || result.Ack.Duration >= time.Second || !result.Ready.Completed.IsZero() || result.Cleanup.Completed.IsZero() || fmt.Sprint(f.writes) != "[1 0]" || result.Watch.Gaps != 1 {
				t.Fatalf("gap hidden or cleanup lost: %v %+v", err, result)
			}
			for _, stream := range f.allStreams {
				if !stream.IsStopped() {
					t.Fatal("watch leaked")
				}
			}
		})
	}
}

func TestCleanupMutationDespiteObservationFailure(t *testing.T) {
	f := newFixture()
	original := errors.New("cleanup LIST unavailable")
	f.client.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if len(f.writes) > 0 {
			return true, nil, original
		}
		return false, nil, nil
	})
	result, err := RunCanary(t.Context(), f.client, testSpec())
	if !errors.Is(err, original) || fmt.Sprint(f.writes) != "[1 0]" || !result.Cleanup.Completed.IsZero() {
		t.Fatalf("cleanup not attempted: %v %+v", err, result)
	}
}

func TestWatchEstablishmentFailureStopsPartialStreams(t *testing.T) {
	for _, resource := range []string{"deployments", "replicasets", "pods"} {
		t.Run(resource, func(t *testing.T) {
			f := newFixture()
			f.client.PrependWatchReactor(resource, func(a clienttesting.Action) (bool, watch.Interface, error) {
				return true, nil, errors.New("watch forbidden")
			})
			result, err := RunCanary(t.Context(), f.client, testSpec())
			if err == nil || len(f.writes) != 0 || !result.Preflight.Completed.IsZero() {
				t.Fatalf("failed watch mutated: %v %+v", err, result)
			}
			for _, stream := range f.allStreams {
				if !stream.IsStopped() {
					t.Fatal("partial stream leaked")
				}
			}
		})
	}
}

func TestWatchStoreCrossKindOrderingAndTerminalPods(t *testing.T) {
	f := newFixture()
	w, err := newCanaryWatch(t.Context(), f.client, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.stop()
	w.uid = "deployment"
	w.selector = labels.SelectorFromSet(map[string]string{"app": "canary"})
	w.arm(11, 1)
	d := f.deployment.DeepCopy()
	d.Generation = 11
	d.Spec.Replicas = ptr.To(int32(1))
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: 11, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	p := newPod()
	p.OwnerReferences[0].UID = "new-rs"
	for _, item := range []canaryEvent{{kind: "deployments", event: watch.Event{Type: watch.Modified, Object: d}}, {kind: "pods", event: watch.Event{Type: watch.Added, Object: &p}}} {
		item.received = time.Now()
		if err := w.apply(item); err != nil {
			t.Fatal(err)
		}
		w.evaluate(time.Now())
	}
	if !w.completed.IsZero() || len(w.pods) != 1 {
		t.Fatal("unknown RS counted or pod lost")
	}
	rs := f.set.DeepCopy()
	rs.UID = "new-rs"
	rs.Spec.Replicas = ptr.To(int32(1))
	rs.Status.Replicas = 1
	if err := w.apply(canaryEvent{kind: "replicasets", event: watch.Event{Type: watch.Added, Object: rs}}); err != nil {
		t.Fatal(err)
	}
	w.evaluate(time.Now())
	if w.completed.IsZero() {
		t.Fatal("RS arrival did not complete cached pod readiness")
	}
	w.completed = time.Time{}
	w.arm(12, 0)
	d.Generation = 12
	d.Spec.Replicas = ptr.To(int32(0))
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: 12}
	_ = w.apply(canaryEvent{kind: "deployments", event: watch.Event{Type: watch.Modified, Object: d}})
	p.Status.Phase = corev1.PodSucceeded
	_ = w.apply(canaryEvent{kind: "pods", event: watch.Event{Type: watch.Modified, Object: &p}})
	w.evaluate(time.Now())
	if !w.completed.IsZero() {
		t.Fatal("terminal pod ignored during cleanup")
	}
	_ = w.apply(canaryEvent{kind: "pods", event: watch.Event{Type: watch.Deleted, Object: &p}})
	w.evaluate(time.Now())
	if !w.completed.IsZero() {
		t.Fatal("nonzero RS ignored during cleanup")
	}
	_ = w.apply(canaryEvent{kind: "replicasets", event: watch.Event{Type: watch.Deleted, Object: rs}})
	w.evaluate(time.Now())
	if w.completed.IsZero() || w.knownSets[rs.UID] != rs.Name {
		t.Fatal("RS deletion did not retain ownership/converge")
	}
}

func TestWatchStopDrainsAcceptedFailures(t *testing.T) {
	for _, mode := range []string{"410", "closed", "intentional stop"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture()
			w, err := newCanaryWatch(t.Context(), f.client, testSpec(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer w.stop()
			w.uid = "deployment"
			w.selector = labels.SelectorFromSet(map[string]string{"app": "canary"})
			w.arm(11, 1)
			w.completed = time.Now()
			// The reader has accepted a failure, but the processor has not run.
			// stop must join readers, then drain this queue before declaring success.
			switch mode {
			case "410":
				w.queue <- canaryEvent{kind: "pods", received: time.Now(), event: watch.Event{Type: watch.Error, Object: &metav1.Status{Code: 410, Reason: metav1.StatusReasonExpired}}}
			case "closed":
				w.queue <- canaryEvent{kind: "pods", received: time.Now(), err: errors.New("watch closed before phase completed")}
			}
			w.mu.Lock()
			w.start()
			done := make(chan struct{})
			go func() { w.stop(); close(done) }()
			<-w.ctx.Done() // Force shutdown before the processor can acquire its lock.
			w.mu.Unlock()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("stop did not join/drain")
			}
			if mode == "intentional stop" {
				if w.err != nil || w.stats.Gaps != 0 {
					t.Fatalf("intentional close treated as gap: %v", w.err)
				}
			} else {
				_, completed, err := w.wait(t.Context())
				if err == nil || !completed.IsZero() || w.stats.Gaps != 1 {
					t.Fatalf("queued failure lost: %v %+v", err, w.stats)
				}
			}
		})
	}
}

func TestWatchPreArmReceiptCannotBecomeFreshWhenAppliedLater(t *testing.T) {
	for _, timing := range []string{"before", "equal", "after"} {
		t.Run(timing, func(t *testing.T) {
			f := newFixture()
			w, err := newCanaryWatch(t.Context(), f.client, testSpec(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer w.stop()
			w.uid = "deployment"
			w.selector = labels.SelectorFromSet(map[string]string{"app": "canary"})
			w.arm(11, 1)
			received := w.armedAt
			switch timing {
			case "before":
				received = received.Add(-time.Nanosecond)
			case "after":
				received = received.Add(time.Nanosecond)
			}
			pod := newPod()
			// The Pod receipt precedes arm but its queued event is applied after
			// the new generation. Both timestamps retain their monotonic component.
			d := f.deployment.DeepCopy()
			d.Generation = 11
			d.Spec.Replicas = ptr.To(int32(1))
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: 11, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
			for _, item := range []canaryEvent{
				{kind: "deployments", received: w.armedAt.Add(2 * time.Nanosecond), event: watch.Event{Type: watch.Modified, Object: d}},
				{kind: "pods", received: received, event: watch.Event{Type: watch.Added, Object: &pod}},
			} {
				if err := w.apply(item); err != nil {
					t.Fatal(err)
				}
				w.evaluate(time.Now())
			}
			if (timing == "after") != !w.completed.IsZero() {
				t.Fatalf("incorrect freshness: %s old=%v completed=%v", timing, w.oldPods, w.completed)
			}
		})
	}
}

func TestWatchCompletionUsesOnlyContributingReceipts(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		t.Run(fmt.Sprint(cleanup), func(t *testing.T) {
			f := newFixture()
			w, err := newCanaryWatch(t.Context(), f.client, testSpec(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer w.stop()
			w.uid = "deployment"
			w.selector = labels.SelectorFromSet(map[string]string{"app": "canary"})
			replicas := int32(1)
			if cleanup {
				replicas = 0
			}
			w.arm(11, replicas)
			t1, t2, t3, t4 := w.armedAt.Add(time.Millisecond), w.armedAt.Add(2*time.Millisecond), w.armedAt.Add(3*time.Millisecond), w.armedAt.Add(4*time.Millisecond)
			d := f.deployment.DeepCopy()
			d.Generation = 11
			d.Spec.Replicas = ptr.To(replicas)
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: 11, Replicas: replicas, UpdatedReplicas: replicas, ReadyReplicas: replicas, AvailableReplicas: replicas}
			pod := newPod()
			pod.OwnerReferences[0].UID = "new-rs"
			rs := f.set.DeepCopy()
			rs.UID = "new-rs"
			rs.Spec.Replicas = ptr.To(replicas)
			rs.Status.Replicas = replicas
			foreign := newPod()
			foreign.UID = "foreign"
			foreign.OwnerReferences = nil
			foreignRS := f.set.DeepCopy()
			foreignRS.UID = "foreign-rs"
			foreignRS.OwnerReferences = nil
			podType := watch.Added
			if cleanup {
				podType = watch.Deleted
			}
			// A later bookmark/unrelated object is enqueued ahead of earlier
			// required state from another stream. It must not bias completion.
			for _, item := range []canaryEvent{
				{kind: "pods", received: t3, event: watch.Event{Type: podType, Object: &pod}},
				{kind: "pods", received: t4, event: watch.Event{Type: watch.Bookmark, Object: &metav1.PartialObjectMetadata{}}},
				{kind: "pods", received: t4, event: watch.Event{Type: watch.Added, Object: &foreign}},
				{kind: "replicasets", received: t4, event: watch.Event{Type: watch.Added, Object: foreignRS}},
				{kind: "replicasets", received: t2, event: watch.Event{Type: watch.Added, Object: rs}},
				{kind: "deployments", received: t1, event: watch.Event{Type: watch.Modified, Object: d}},
			} {
				if err := w.apply(item); err != nil {
					t.Fatal(err)
				}
				w.evaluate(item.received)
			}
			if !w.completed.Equal(t3) || !w.last.ObservedAt.Equal(t3) {
				t.Fatalf("completion includes unrelated receipt or loses tombstone: got %v want %v", w.completed, t3)
			}
		})
	}
}

func TestWatchInvalidListsAndBookmarks(t *testing.T) {
	for _, mode := range []string{"missing", "no RV", "RS error"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture()
			resource := "deployments"
			if mode == "RS error" {
				resource = "replicasets"
			}
			f.client.PrependReactor("list", resource, func(a clienttesting.Action) (bool, runtime.Object, error) {
				if mode == "RS error" {
					return true, nil, errors.New("RS unavailable")
				}
				list := &appsv1.DeploymentList{}
				if mode == "no RV" {
					list.Items = []appsv1.Deployment{*f.deployment}
				}
				return true, list, nil
			})
			if _, err := RunCanary(t.Context(), f.client, testSpec()); err == nil || len(f.writes) != 0 {
				t.Fatal("invalid LIST allowed mutation")
			}
		})
	}
	f := newFixture()
	w, err := newCanaryWatch(t.Context(), f.client, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.stop()
	w.uid = "deployment"
	w.selector = labels.SelectorFromSet(map[string]string{"app": "canary"})
	if err := w.apply(canaryEvent{kind: "pods", event: watch.Event{Type: watch.Bookmark, Object: &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "opaque-bookmark"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := w.apply(canaryEvent{kind: "deployments", event: watch.Event{Type: watch.Deleted, Object: f.deployment}}); err == nil {
		t.Fatal("deleted fixture accepted")
	}
	rs := f.set.DeepCopy()
	rs.OwnerReferences = append(rs.OwnerReferences, owner("Deployment", "other", "other"))
	if err := w.apply(canaryEvent{kind: "replicasets", event: watch.Event{Type: watch.Added, Object: rs}}); err == nil {
		t.Fatal("ambiguous owner accepted")
	}
}

func TestWatchOverflowInvalidatesMeasurement(t *testing.T) {
	f := newFixture()
	w, err := newCanaryWatch(t.Context(), f.client, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.stop()
	w.uid = "deployment"
	w.selector = labels.SelectorFromSet(map[string]string{"app": "canary"})
	w.arm(11, 1)
	// Block processing while preloading more events than the bounded queue can
	// retain. No events may be silently dropped into an otherwise valid sample.
	w.queue = make(chan canaryEvent, 1)
	w.mu.Lock()
	for i := 0; i < 10; i++ {
		f.streams["pods"].Add(ptr.To(newPod()))
	}
	w.start()
	deadline := time.Now().Add(time.Second)
	for len(f.streams["pods"].ResultChan()) > 7 && time.Now().Before(deadline) {
		goruntime.Gosched()
	}
	w.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, completed, err := w.wait(ctx)
	if err == nil || !strings.Contains(err.Error(), "buffer overflow") || !completed.IsZero() {
		t.Fatalf("lost event accepted: %v", err)
	}
}

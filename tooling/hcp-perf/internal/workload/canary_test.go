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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	typedappsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
)

// Scripted LIST snapshots and real fake watch streams model controller events.
type fixture struct {
	client     *fake.Clientset
	deployment *appsv1.Deployment
	set        *appsv1.ReplicaSet
	pods       []corev1.Pod
	writes     []int32
	onScale    func(int32)
	onRead     func()
	streams    map[string]*watch.RaceFreeFakeWatcher
	allStreams []*watch.RaceFreeFakeWatcher
}

func newFixture() *fixture {
	f := &fixture{
		client:  fake.NewClientset(),
		streams: map[string]*watch.RaceFreeFakeWatcher{},
		deployment: &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "canary", Namespace: "guest", UID: "deployment", ResourceVersion: "10", Generation: 10},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(0)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "canary"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "canary"}}}},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 10},
		},
		set: &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "canary-rs", Namespace: "guest", UID: "rs", OwnerReferences: []metav1.OwnerReference{owner("Deployment", "canary", "deployment")}}},
	}
	f.client.PrependReactor("get", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if f.onRead != nil {
			f.onRead()
		}
		return true, f.deployment.DeepCopy(), nil
	})
	f.client.PrependReactor("list", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if f.onRead != nil {
			f.onRead()
		}
		return true, &appsv1.DeploymentList{ListMeta: metav1.ListMeta{ResourceVersion: "deployment-list-rv"}, Items: []appsv1.Deployment{*f.deployment.DeepCopy()}}, nil
	})
	f.client.PrependWatchReactor("*", func(action clienttesting.Action) (bool, watch.Interface, error) {
		stream := watch.NewRaceFreeFake()
		f.streams[action.GetResource().Resource] = stream
		f.allStreams = append(f.allStreams, stream)
		return true, stream, nil
	})
	f.client.PrependReactor("list", "replicasets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, &appsv1.ReplicaSetList{ListMeta: metav1.ListMeta{ResourceVersion: "rs-list-rv"}, Items: []appsv1.ReplicaSet{*f.set.DeepCopy()}}, nil
	})
	f.client.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, (&corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "pod-list-rv"}, Items: f.pods}).DeepCopy(), nil
	})
	f.client.PrependReactor("update", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		scale := action.(clienttesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
		if action.GetSubresource() != "scale" || scale.UID != f.deployment.UID || scale.ResourceVersion != f.deployment.ResourceVersion {
			return true, nil, fmt.Errorf("missing scale/UID/resourceVersion guard")
		}
		f.writes = append(f.writes, scale.Spec.Replicas)
		previousPods := f.pods
		if *f.deployment.Spec.Replicas != scale.Spec.Replicas {
			f.deployment.Generation++
		}
		f.deployment.Spec.Replicas = ptr.To(scale.Spec.Replicas)
		f.deployment.ResourceVersion = fmt.Sprint(f.deployment.Generation)
		f.deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: f.deployment.Generation, Replicas: scale.Spec.Replicas, UpdatedReplicas: scale.Spec.Replicas, ReadyReplicas: scale.Spec.Replicas, AvailableReplicas: scale.Spec.Replicas}
		f.pods = nil
		if scale.Spec.Replicas == 1 {
			f.pods = []corev1.Pod{newPod()}
		}
		if f.onScale != nil {
			f.onScale(scale.Spec.Replicas)
		}
		f.streams["replicasets"].Modify(f.set.DeepCopy())
		for _, old := range previousPods {
			found := false
			for _, pod := range f.pods {
				found = found || old.UID == pod.UID
			}
			if !found {
				f.streams["pods"].Delete(old.DeepCopy())
			}
		}
		for _, pod := range f.pods {
			f.streams["pods"].Modify(pod.DeepCopy())
		}
		f.streams["deployments"].Modify(f.deployment.DeepCopy())
		// Like the real scale subresource, this response has no generation.
		return true, &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{UID: f.deployment.UID, ResourceVersion: f.deployment.ResourceVersion}, Spec: scale.Spec}, nil
	})
	return f
}

func owner(kind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "apps/v1", Kind: kind, Name: name, UID: uid, Controller: ptr.To(true)}
}

func newPod() corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "canary-pod", Namespace: "guest", UID: "new-pod", CreationTimestamp: metav1.NewTime(time.Now()), Labels: map[string]string{"app": "canary"}, OwnerReferences: []metav1.OwnerReference{owner("ReplicaSet", "canary-rs", "rs")}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func testSpec() Spec {
	return Spec{Namespace: "guest", Name: "canary", Deadlines: experiment.CanaryDeadlines{Ack: 100 * time.Millisecond, Ready: 100 * time.Millisecond, Cleanup: 100 * time.Millisecond}}
}

func TestRunCanary(t *testing.T) {
	f := newFixture()
	result, err := RunCanary(context.Background(), f.client, testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.writes) != "[1 0]" || result.Generation != 11 || result.PodUID != "new-pod" || result.DeploymentUID != "deployment" {
		t.Fatalf("wrong cycle: writes=%v result=%+v", f.writes, result)
	}
	for _, stage := range []Stage{result.Preflight, result.Ack, result.Ready, result.Cleanup} {
		if stage.Started.IsZero() || stage.Completed.Before(stage.Started) || stage.Duration <= 0 {
			t.Fatalf("invalid stage: %+v", stage)
		}
	}
	if result.Ack.Started != result.Ready.Started || result.Cleanup.Started.Before(result.Ready.Completed) || result.Ack.Attempts != 1 || result.Cleanup.Attempts != 1 {
		t.Fatalf("invalid measurements: %+v", result)
	}
	if result.Preflight.Completed.After(result.Ack.Started) || result.Preflight.Duration != result.Preflight.Completed.Sub(result.Preflight.Started) || result.Preflight.Attempts != 0 {
		t.Fatalf("preflight overlaps measured cycle: %+v", result)
	}
	if len(result.Last.Pods) != 1 || len(result.CleanupLast.Pods) != 0 || result.CleanupLast.Deployment.ObservedGeneration != 12 {
		t.Fatalf("snapshots lost: %+v", result)
	}
	if result.Performance != "not_evaluated" || result.Degraded {
		t.Fatalf("unset expectations inferred degradation: %+v", result)
	}
}

func TestCanaryPerformanceClassification(t *testing.T) {
	for _, stageName := range []string{"ack", "ready", "cleanup"} {
		for _, tc := range []struct {
			name        string
			expected    time.Duration
			duration    time.Duration
			completed   bool
			exceeded    bool
			performance string
		}{
			{"below", 10, 9, true, false, "within_expectations"},
			{"equal", 10, 10, true, false, "within_expectations"},
			{"above", 10, 11, true, true, "degraded"},
			{"unset", 0, time.Hour, true, false, "not_evaluated"},
			{"incomplete late", 10, 11, false, true, "not_evaluated"},
			{"not started", 10, 0, false, false, "not_evaluated"},
		} {
			t.Run(stageName+"/"+tc.name, func(t *testing.T) {
				result := Result{}
				stage := map[string]*Stage{"ack": &result.Ack, "ready": &result.Ready, "cleanup": &result.Cleanup}[stageName]
				stage.ExpectedDuration, stage.Duration = tc.expected, tc.duration
				if tc.completed {
					stage.Completed = time.Now()
				}
				result.finalizePerformance()
				if stage.ExpectationExceeded != tc.exceeded || result.Performance != tc.performance || result.Degraded != (tc.performance == "degraded") {
					t.Fatalf("incorrect classification: %+v", result)
				}
			})
		}
	}
}

func TestCanarySoftExpectationsContinueToCompletion(t *testing.T) {
	for _, stageName := range []string{"ack", "ready", "cleanup"} {
		t.Run(stageName, func(t *testing.T) {
			f := newFixture()
			spec := testSpec()
			spec.Deadlines = experiment.CanaryDeadlines{Ack: 200 * time.Millisecond, Ready: 200 * time.Millisecond, Cleanup: 200 * time.Millisecond}
			// Only the delayed stage has an expectation; zero never selects a heuristic.
			switch stageName {
			case "ack":
				spec.Expectations.Ack = time.Millisecond
			case "ready":
				spec.Expectations.Ready = time.Millisecond
			case "cleanup":
				spec.Expectations.Cleanup = time.Millisecond
			}
			f.onScale = func(n int32) {
				if (stageName == "ready" && n == 1) || (stageName == "cleanup" && n == 0) {
					f.deployment.Status.ObservedGeneration--
				}
			}
			var deliveries []<-chan struct{}
			defer func() {
				for _, done := range deliveries {
					<-done
				}
			}()
			client := contextClient{Interface: f.client, update: func(ctx context.Context, scale *autoscalingv1.Scale) (*autoscalingv1.Scale, error) {
				if stageName == "ack" && scale.Spec.Replicas == 1 {
					select {
					case <-time.After(10 * time.Millisecond):
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				response, err := f.client.AppsV1().Deployments("guest").UpdateScale(ctx, "canary", scale, metav1.UpdateOptions{})
				if (stageName == "ready" && scale.Spec.Replicas == 1) || (stageName == "cleanup" && scale.Spec.Replicas == 0) {
					// Delay only the watch event, not ACK. The goroutine owns its
					// snapshot and stream reference, never the mutable fixture.
					deployment, stream := f.deployment.DeepCopy(), f.streams["deployments"]
					deployment.Status.ObservedGeneration = deployment.Generation
					done := make(chan struct{})
					deliveries = append(deliveries, done)
					go func() {
						defer close(done)
						time.Sleep(10 * time.Millisecond)
						stream.Modify(deployment)
					}()
				}
				return response, err
			}}
			result, err := RunCanary(t.Context(), client, spec)
			if err != nil || result.Performance != "degraded" || !result.Degraded || fmt.Sprint(f.writes) != "[1 0]" {
				t.Fatalf("soft expectation stopped cycle: %v %+v", err, result)
			}
			for name, stage := range map[string]Stage{"ack": result.Ack, "ready": result.Ready, "cleanup": result.Cleanup} {
				if stage.Completed.IsZero() || stage.Duration != stage.Completed.Sub(stage.Started) || stage.Deadline != 200*time.Millisecond || stage.ExpectationExceeded != (name == stageName) {
					t.Fatalf("incorrect %s measurement: %+v", name, stage)
				}
				if name == stageName && (stage.Duration < 10*time.Millisecond || stage.ExpectedDuration != time.Millisecond) {
					t.Fatalf("late completion censored: %+v", stage)
				}
			}
			data, err := json.Marshal(result)
			if err != nil || !strings.Contains(string(data), `"expectedDurationNanoseconds":1000000`) || !strings.Contains(string(data), `"deadlineNanoseconds":200000000`) || !strings.Contains(string(data), `"expectationExceeded":true`) {
				t.Fatalf("missing persisted thresholds: %s %v", data, err)
			}
		})
	}
}

func TestCanaryExpectationsAreIndependent(t *testing.T) {
	for _, expectations := range []experiment.CanaryDeadlines{
		{Ack: 100 * time.Millisecond},
		{Ready: 100 * time.Millisecond},
		{Cleanup: 100 * time.Millisecond},
		{Ack: 100 * time.Millisecond, Ready: 50 * time.Millisecond},
		{Ack: 100 * time.Millisecond, Ready: 100 * time.Millisecond, Cleanup: 100 * time.Millisecond},
	} {
		t.Run(fmt.Sprint(expectations), func(t *testing.T) {
			f := newFixture()
			spec := testSpec()
			spec.Expectations = expectations
			result, err := RunCanary(t.Context(), f.client, spec)
			if err != nil || result.Performance != "within_expectations" || result.Degraded {
				t.Fatalf("independent expectations rejected: %v %+v", err, result)
			}
		})
	}
}

func TestCanaryHardDeadlinesWithExpectations(t *testing.T) {
	for _, stageName := range []string{"ack", "ready", "cleanup"} {
		for _, degraded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/degraded=%t", stageName, degraded), func(t *testing.T) {
				f := newFixture()
				spec := testSpec()
				spec.Expectations = experiment.CanaryDeadlines{Ack: time.Millisecond, Ready: time.Millisecond, Cleanup: time.Millisecond}
				if !degraded {
					spec.Expectations = spec.Deadlines
				}
				f.onScale = func(n int32) {
					if stageName == "ready" && n == 1 {
						f.pods[0].Status.Phase = corev1.PodPending
					}
				}
				client := contextClient{Interface: f.client, update: func(ctx context.Context, scale *autoscalingv1.Scale) (*autoscalingv1.Scale, error) {
					if (stageName == "ack" && scale.Spec.Replicas == 1) || (stageName == "cleanup" && scale.Spec.Replicas == 0) {
						if stageName == "ack" {
							// The write committed, but its ACK never arrives.
							_, _ = f.client.AppsV1().Deployments("guest").UpdateScale(ctx, "canary", scale, metav1.UpdateOptions{})
						}
						<-ctx.Done()
						return nil, ctx.Err()
					}
					if degraded {
						time.Sleep(10 * time.Millisecond)
					}
					return f.client.AppsV1().Deployments("guest").UpdateScale(ctx, "canary", scale, metav1.UpdateOptions{})
				}}
				result, err := RunCanary(t.Context(), client, spec)
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), stageName+":") || result.Cleanup.Attempts != 1 || result.Degraded != degraded {
					t.Fatalf("hard failure or partial degradation lost: %v %+v", err, result)
				}
				stage := map[string]Stage{"ack": result.Ack, "ready": result.Ready, "cleanup": result.Cleanup}[stageName]
				if !stage.Completed.IsZero() || !stage.ExpectationExceeded || stage.Duration < stage.Deadline {
					t.Fatalf("hard failure recorded as completion or elapsed lost: %+v", stage)
				}
				if stageName != "cleanup" && result.Cleanup.Completed.IsZero() {
					t.Fatalf("hard failure prevented cleanup: %+v", result)
				}
			})
		}
	}
}

func TestInvalidSpecsDoNotCallAPI(t *testing.T) {
	if result, err := RunCanary(context.Background(), nil, testSpec()); err == nil || result.Preflight != (Stage{}) {
		t.Fatalf("nil client accepted or preflight started: %v %+v", err, result)
	}
	for _, change := range []func(*Spec){
		func(s *Spec) { s.Name = "" }, func(s *Spec) { s.Namespace = "not/a/name" },
		func(s *Spec) { s.Deadlines.Ack = 0 }, func(s *Spec) { s.Deadlines.Ready = 0 }, func(s *Spec) { s.Deadlines.Cleanup = 0 },
		func(s *Spec) { s.Deadlines.Ack = 31 * time.Second }, func(s *Spec) { s.Deadlines.Ready = 121 * time.Second }, func(s *Spec) { s.Deadlines.Cleanup = 121 * time.Second },
		func(s *Spec) { s.PreflightTimeout = -1 }, func(s *Spec) { s.PreflightTimeout = 30*time.Second + time.Nanosecond },
		func(s *Spec) { s.Expectations.Ack = -1 }, func(s *Spec) { s.Expectations.Ready = -1 }, func(s *Spec) { s.Expectations.Cleanup = -1 },
		func(s *Spec) { s.Expectations.Ack = s.Deadlines.Ack + 1 }, func(s *Spec) { s.Expectations.Ready = s.Deadlines.Ready + 1 }, func(s *Spec) { s.Expectations.Cleanup = s.Deadlines.Cleanup + 1 },
	} {
		f := newFixture()
		spec := testSpec()
		change(&spec)
		if result, err := RunCanary(context.Background(), f.client, spec); err == nil || len(f.client.Actions()) != 0 || result.Preflight != (Stage{}) {
			t.Fatalf("invalid spec accepted or called API: %+v", spec)
		}
	}
}

func TestBrokenFixturesAreNotMutated(t *testing.T) {
	for name, change := range map[string]func(*fixture){
		"one replica":      func(f *fixture) { f.deployment.Spec.Replicas = ptr.To(int32(1)) },
		"stale generation": func(f *fixture) { f.deployment.Status.ObservedGeneration-- },
		"old replicas":     func(f *fixture) { f.deployment.Status.Replicas = 1 },
		"paused":           func(f *fixture) { f.deployment.Spec.Paused = true },
		"deleting":         func(f *fixture) { f.deployment.DeletionTimestamp = ptr.To(metav1.Now()) },
		"empty selector":   func(f *fixture) { f.deployment.Spec.Selector = &metav1.LabelSelector{} },
		"bad selector": func(f *fixture) {
			f.deployment.Spec.Selector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "app", Operator: "invalid"}}
		},
		"template mismatch":  func(f *fixture) { f.deployment.Spec.Template.Labels = nil },
		"old pod":            func(f *fixture) { f.pods = []corev1.Pod{newPod()} },
		"mislabeled old pod": func(f *fixture) { p := newPod(); p.Labels = nil; f.pods = []corev1.Pod{p} },
		"ambiguous RS": func(f *fixture) {
			f.set.OwnerReferences = append(f.set.OwnerReferences, owner("Deployment", "other", "other"))
		},
		"ambiguous pod": func(f *fixture) {
			p := newPod()
			p.OwnerReferences = append(p.OwnerReferences, owner("ReplicaSet", "other", "other"))
			f.pods = []corev1.Pod{p}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			change(f)
			result, err := RunCanary(context.Background(), f.client, testSpec())
			if err == nil || len(f.writes) != 0 || !result.Cleanup.Started.IsZero() {
				t.Fatalf("broken fixture mutated or passed: %v %+v", err, result)
			}
			if result.Preflight.Started.IsZero() || !result.Preflight.Completed.IsZero() || result.Preflight.Duration <= 0 || result.Ack != (Stage{Deadline: testSpec().Deadlines.Ack}) || result.Ready != (Stage{Deadline: testSpec().Deadlines.Ready}) {
				t.Fatalf("invalid preflight failure measurements: %+v", result)
			}
		})
	}
}

func TestReadinessDoesNotFalsePass(t *testing.T) {
	for name, change := range map[string]func(*fixture){
		"unobserved generation":    func(f *fixture) { f.deployment.Status.ObservedGeneration-- },
		"not updated":              func(f *fixture) { f.deployment.Status.UpdatedReplicas = 0 },
		"not available":            func(f *fixture) { f.deployment.Status.AvailableReplicas = 0 },
		"pending but ready":        func(f *fixture) { f.pods[0].Status.Phase = corev1.PodPending },
		"succeeded but ready":      func(f *fixture) { f.pods[0].Status.Phase = corev1.PodSucceeded },
		"not ready":                func(f *fixture) { f.pods[0].Status.Conditions = nil },
		"terminating":              func(f *fixture) { f.pods[0].DeletionTimestamp = ptr.To(metav1.Now()) },
		"same labels wrong RS UID": func(f *fixture) { f.pods[0].OwnerReferences[0].UID = "other" },
		"wrong RS name":            func(f *fixture) { f.pods[0].OwnerReferences[0].Name = "other" },
		"noncontroller pod":        func(f *fixture) { f.pods[0].OwnerReferences[0].Controller = ptr.To(false) },
		"wrong deployment UID":     func(f *fixture) { f.set.OwnerReferences[0].UID = "other" },
		"wrong owner kind":         func(f *fixture) { f.pods[0].OwnerReferences[0].Kind = "Job" },
		"wrong owner API":          func(f *fixture) { f.set.OwnerReferences[0].APIVersion = "example/v1" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			// Establish foreign RS ownership in the LIST, not concurrently on
			// another stream whose relative delivery order is unspecified.
			if name == "wrong deployment UID" || name == "wrong owner API" {
				change(f)
			}
			f.onScale = func(n int32) {
				if n == 1 {
					change(f)
				}
			}
			result, err := RunCanary(context.Background(), f.client, testSpec())
			if !errors.Is(err, context.DeadlineExceeded) || !result.Ready.Completed.IsZero() || result.Cleanup.Completed.IsZero() || fmt.Sprint(f.writes) != "[1 0]" {
				t.Fatalf("false pass or cleanup failed: %v %+v", err, result)
			}
		})
	}
}

func TestWaitsForObservedGeneration(t *testing.T) {
	f := newFixture()
	f.onScale = func(n int32) {
		if n == 1 {
			stale := f.deployment.DeepCopy()
			stale.Status.ObservedGeneration--
			f.streams["deployments"].Modify(stale)
		}
	}
	if _, err := RunCanary(context.Background(), f.client, testSpec()); err != nil {
		t.Fatalf("did not wait for generation event: %v", err)
	}
}

func TestPreviouslySeenUIDDoesNotCount(t *testing.T) {
	f := newFixture()
	old := newPod()
	old.OwnerReferences = nil
	f.pods = []corev1.Pod{old} // Matching labels alone neither block preflight nor establish ownership.
	result, err := RunCanary(context.Background(), f.client, testSpec())
	if !errors.Is(err, context.DeadlineExceeded) || !result.Ready.Completed.IsZero() {
		t.Fatalf("previously seen UID passed: %v %+v", err, result)
	}
}

func TestFreshPodDoesNotDependOnCreationClock(t *testing.T) {
	for _, offset := range []time.Duration{0, -time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			f := newFixture()
			var created metav1.Time
			f.onScale = func(n int32) {
				if n != 1 {
					return
				}
				// Round-trip through the API's second-precision timestamp format.
				data, err := json.Marshal(metav1.NewTime(time.Now().Add(offset)))
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &created); err != nil {
					t.Fatal(err)
				}
				f.pods[0].CreationTimestamp = created
			}
			result, err := RunCanary(context.Background(), f.client, testSpec())
			if err != nil || result.PodUID != "new-pod" || result.Ready.Completed.IsZero() || result.Cleanup.Completed.IsZero() {
				t.Fatalf("fresh owned Running/Ready pod rejected: %v %+v", err, result)
			}
			if !result.Last.Pods[0].Created.Equal(created.Time) || created.Nanosecond() != 0 {
				t.Fatalf("API timestamp not retained for diagnostics: %+v", result.Last.Pods[0])
			}
		})
	}
}

func TestEmptyPodUIDDoesNotCount(t *testing.T) {
	f := newFixture()
	f.onScale = func(n int32) {
		if n == 1 {
			f.pods[0].UID = ""
		}
	}
	result, err := RunCanary(context.Background(), f.client, testSpec())
	if err == nil || !result.Ready.Completed.IsZero() || result.Cleanup.Completed.IsZero() {
		t.Fatalf("empty UID passed or cleanup failed: %v %+v", err, result)
	}
}

func TestResultJSON(t *testing.T) {
	result := Result{
		Preflight: Stage{Duration: 456 * time.Nanosecond},
		Ack:       Stage{Duration: 123 * time.Nanosecond},
		Last:      Snapshot{Pods: []PodStatus{{}}},
	}
	result.finalizePerformance()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ack":{"started":"0001-01-01T00:00:00Z","completed":"0001-01-01T00:00:00Z","durationNanoseconds":123,"attempts":0},"ready":{"started":"0001-01-01T00:00:00Z","completed":"0001-01-01T00:00:00Z","durationNanoseconds":0,"attempts":0},"cleanup":{"started":"0001-01-01T00:00:00Z","completed":"0001-01-01T00:00:00Z","durationNanoseconds":0,"attempts":0},"deploymentUID":"","generation":0,"podUID":"","last":{"observedAt":"0001-01-01T00:00:00Z","deployment":{"uid":"","generation":0,"observedGeneration":0,"desired":0,"replicas":0,"updated":0,"ready":0,"available":0,"reasons":null},"pods":[{"name":"","uid":"","created":"0001-01-01T00:00:00Z","phase":"","ready":false,"deleting":false,"reasons":null}]},"cleanupLast":{"observedAt":"0001-01-01T00:00:00Z","deployment":{"uid":"","generation":0,"observedGeneration":0,"desired":0,"replicas":0,"updated":0,"ready":0,"available":0,"reasons":null},"pods":null}}`
	want = `{"preflight":{"started":"0001-01-01T00:00:00Z","completed":"0001-01-01T00:00:00Z","durationNanoseconds":456,"attempts":0},` + want[1:]
	want = strings.ReplaceAll(want, `"attempts":0}`, `"attempts":0,"expectationExceeded":false}`)
	want = `{"performance":"not_evaluated","degraded":false,` + want[1:]
	if string(data) != want {
		t.Fatalf("result JSON contract changed:\n got: %s\nwant: %s", data, want)
	}
}

func TestPendingSnapshotSurvivesCleanup(t *testing.T) {
	f := newFixture()
	f.onScale = func(n int32) {
		if n != 1 {
			return
		}
		p := &f.pods[0]
		p.Status.Phase = corev1.PodPending
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "secret-message"}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "secret-message"}}}}
		p.Spec.Containers = []corev1.Container{{Name: "main", Env: []corev1.EnvVar{{Name: "TOKEN", Value: "secret-env"}}}}
	}
	result, err := RunCanary(context.Background(), f.client, testSpec())
	data, marshalErr := json.Marshal(result)
	if !errors.Is(err, context.DeadlineExceeded) || marshalErr != nil || len(result.Last.Pods) != 1 || len(result.CleanupLast.Pods) != 0 || !strings.Contains(string(data), "Unschedulable") || !strings.Contains(string(data), "ImagePullBackOff") || strings.Contains(string(data), "secret-") {
		t.Fatalf("bad diagnostic snapshot: err=%v result=%s", err, data)
	}
}

// The generated fake reactors do not receive contexts. This thin wrapper makes
// cancellation tests honor the same request contexts as the real typed client.
type contextClient struct {
	kubernetes.Interface
	get    func(context.Context) (*appsv1.Deployment, error)
	list   func(context.Context) (*appsv1.DeploymentList, error)
	update func(context.Context, *autoscalingv1.Scale) (*autoscalingv1.Scale, error)
}
type contextApps struct {
	typedappsv1.AppsV1Interface
	get    func(context.Context) (*appsv1.Deployment, error)
	list   func(context.Context) (*appsv1.DeploymentList, error)
	update func(context.Context, *autoscalingv1.Scale) (*autoscalingv1.Scale, error)
}
type contextDeployments struct {
	typedappsv1.DeploymentInterface
	get    func(context.Context) (*appsv1.Deployment, error)
	list   func(context.Context) (*appsv1.DeploymentList, error)
	update func(context.Context, *autoscalingv1.Scale) (*autoscalingv1.Scale, error)
}

func (c contextClient) AppsV1() typedappsv1.AppsV1Interface {
	return contextApps{c.Interface.AppsV1(), c.get, c.list, c.update}
}
func (c contextApps) Deployments(ns string) typedappsv1.DeploymentInterface {
	return contextDeployments{c.AppsV1Interface.Deployments(ns), c.get, c.list, c.update}
}
func (c contextDeployments) List(ctx context.Context, opts metav1.ListOptions) (*appsv1.DeploymentList, error) {
	if c.list != nil {
		return c.list(ctx)
	}
	return c.DeploymentInterface.List(ctx, opts)
}
func (c contextDeployments) Get(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.Deployment, error) {
	if c.get != nil {
		return c.get(ctx)
	}
	return c.DeploymentInterface.Get(ctx, name, opts)
}
func (c contextDeployments) UpdateScale(ctx context.Context, name string, scale *autoscalingv1.Scale, opts metav1.UpdateOptions) (*autoscalingv1.Scale, error) {
	if c.update == nil {
		return c.DeploymentInterface.UpdateScale(ctx, name, scale, opts)
	}
	return c.update(ctx, scale)
}

func TestPreflightIndependentOfFittedAck(t *testing.T) {
	for _, budget := range []time.Duration{0, time.Second, 30 * time.Second} {
		t.Run(budget.String(), func(t *testing.T) {
			f := newFixture()
			spec := testSpec()
			spec.Deadlines.Ack = time.Millisecond
			spec.PreflightTimeout = budget
			var preflightCtx context.Context
			client := contextClient{Interface: f.client, list: func(ctx context.Context) (*appsv1.DeploymentList, error) {
				if len(f.writes) == 0 {
					if preflightCtx != nil && ctx != preflightCtx {
						t.Fatal("preflight reads do not share a budget")
					}
					preflightCtx = ctx
					select {
					case <-time.After(10 * time.Millisecond):
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return f.client.AppsV1().Deployments("guest").List(ctx, metav1.ListOptions{})
			}}
			f.client.PrependReactor("list", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if len(f.writes) == 0 {
					time.Sleep(10 * time.Millisecond)
				}
				return false, nil, nil
			})
			result, err := RunCanary(context.Background(), client, spec)
			if err != nil || fmt.Sprint(f.writes) != "[1 0]" || result.Ready.Completed.IsZero() || result.Cleanup.Completed.IsZero() {
				t.Fatalf("slow preflight consumed write budget: %v %+v", err, result)
			}
			if budget == 0 {
				budget = 30 * time.Second
			}
			deadline, ok := preflightCtx.Deadline()
			if !ok || deadline.Before(result.Preflight.Started.Add(budget)) || deadline.After(result.Preflight.Completed.Add(budget)) || preflightCtx.Err() != context.Canceled {
				t.Fatalf("incorrect preflight budget or context not canceled: %v %+v", deadline, result.Preflight)
			}
			if result.Preflight.Duration < 40*time.Millisecond || result.Preflight.Duration != result.Preflight.Completed.Sub(result.Preflight.Started) || result.Preflight.Completed.After(result.Ready.Started) || result.Ack.Duration >= spec.Deadlines.Ack {
				t.Fatalf("preflight timing leaked into cycle: %+v", result)
			}
		})
	}
}

func TestPreflightFailureDoesNotStartCycle(t *testing.T) {
	for _, mode := range []string{"initial timeout", "cancel", "API error"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture()
			spec := testSpec()
			spec.PreflightTimeout = 10 * time.Millisecond
			spec.Expectations = experiment.CanaryDeadlines{Ack: time.Millisecond, Ready: 2 * time.Millisecond, Cleanup: 3 * time.Millisecond}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var original error = context.DeadlineExceeded
			client := contextClient{Interface: f.client, list: func(callCtx context.Context) (*appsv1.DeploymentList, error) {
				if mode == "API error" {
					original = errors.New("deployment LIST failed")
					return nil, original
				}
				if mode == "cancel" {
					original = context.Canceled
					cancel()
				}
				<-callCtx.Done()
				return nil, callCtx.Err()
			}}
			result, err := RunCanary(ctx, client, spec)
			if !errors.Is(err, original) || !strings.Contains(err.Error(), "preflight") || len(f.writes) != 0 || result.Ack != (Stage{Deadline: spec.Deadlines.Ack, ExpectedDuration: spec.Expectations.Ack}) || result.Ready != (Stage{Deadline: spec.Deadlines.Ready, ExpectedDuration: spec.Expectations.Ready}) || result.Cleanup != (Stage{Deadline: spec.Deadlines.Cleanup, ExpectedDuration: spec.Expectations.Cleanup}) {
				t.Fatalf("preflight failure lost or cycle started: %v %+v", err, result)
			}
			if result.Performance != "not_evaluated" || result.Degraded {
				t.Fatalf("preflight failure classified unstarted stages: %+v", result)
			}
			if result.Preflight.Started.IsZero() || !result.Preflight.Completed.IsZero() || result.Preflight.Duration <= 0 || result.Preflight.Attempts != 0 {
				t.Fatalf("invalid failed preflight stage: %+v", result.Preflight)
			}
		})
	}
}

func TestAckFailureStillCleansUpWithoutRetry(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "conflict", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attempts := 0
			var original error
			client := contextClient{Interface: f.client, update: func(callCtx context.Context, scale *autoscalingv1.Scale) (*autoscalingv1.Scale, error) {
				attempts++
				if scale.Spec.Replicas == 0 {
					if callCtx.Err() != nil {
						t.Fatalf("cleanup inherited cancellation: %v", callCtx.Err())
					}
					return f.client.AppsV1().Deployments("guest").UpdateScale(callCtx, "canary", scale, metav1.UpdateOptions{})
				}
				// Simulate a committed write whose response was lost.
				if mode == "timeout" || mode == "cancel" {
					_, _ = f.client.AppsV1().Deployments("guest").UpdateScale(callCtx, "canary", scale, metav1.UpdateOptions{})
					if mode == "cancel" {
						cancel()
					}
					<-callCtx.Done()
					original = callCtx.Err()
				} else if mode == "conflict" {
					original = apierrors.NewConflict(schema.GroupResource{Resource: "deployments"}, "canary", errors.New("conflict"))
				} else {
					original = apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "canary", errors.New("denied"))
				}
				return nil, original
			}}
			result, err := RunCanary(ctx, client, testSpec())
			if !errors.Is(err, original) || attempts != 2 || result.Ack.Attempts != 1 || !result.Ack.Completed.IsZero() || !result.Ready.Completed.IsZero() || result.Cleanup.Completed.IsZero() {
				t.Fatalf("lost ack failure/cleanup: %v %+v attempts=%d", err, result, attempts)
			}
		})
	}
}

func TestCleanupFailureNeverPassesAndPreservesOriginal(t *testing.T) {
	for _, ackFails := range []bool{false, true} {
		t.Run(fmt.Sprint(ackFails), func(t *testing.T) {
			f := newFixture()
			ackErr := errors.New("ack failed")
			client := contextClient{Interface: f.client, update: func(ctx context.Context, scale *autoscalingv1.Scale) (*autoscalingv1.Scale, error) {
				if scale.Spec.Replicas == 0 {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if ackFails {
					return nil, ackErr
				}
				return f.client.AppsV1().Deployments("guest").UpdateScale(ctx, "canary", scale, metav1.UpdateOptions{})
			}}
			result, err := RunCanary(context.Background(), client, testSpec())
			if !errors.Is(err, context.DeadlineExceeded) || (ackFails && !errors.Is(err, ackErr)) || !result.Cleanup.Completed.IsZero() || result.Cleanup.Duration <= 0 {
				t.Fatalf("cleanup failure hidden: %v %+v", err, result)
			}
			if result.Preflight.Completed.IsZero() || result.Preflight.Duration != result.Preflight.Completed.Sub(result.Preflight.Started) || result.Preflight.Completed.After(result.Ack.Started) {
				t.Fatalf("cleanup failure changed preflight measurements: %+v", result)
			}
		})
	}
}

func TestCleanupWaitsForPodsAndGeneration(t *testing.T) {
	for _, mode := range []string{"pod", "generation"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture()
			f.onScale = func(n int32) {
				if n == 0 {
					if mode == "pod" {
						f.pods = []corev1.Pod{newPod()}
						f.pods[0].DeletionTimestamp = ptr.To(metav1.Now())
					} else {
						f.deployment.Status.ObservedGeneration--
					}
				}
			}
			result, err := RunCanary(context.Background(), f.client, testSpec())
			if !errors.Is(err, context.DeadlineExceeded) || result.Ready.Completed.IsZero() || !result.Cleanup.Completed.IsZero() {
				t.Fatalf("cleanup falsely passed: %v %+v", err, result)
			}
		})
	}
}

func TestReplacementIsNeverScaledDown(t *testing.T) {
	f := newFixture()
	f.onScale = func(n int32) {
		if n == 1 {
			f.deployment.UID = "replacement"
		}
	}
	result, err := RunCanary(context.Background(), f.client, testSpec())
	if err == nil || fmt.Sprint(f.writes) != "[1]" || !result.Cleanup.Completed.IsZero() || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("replacement modified: %v %+v writes=%v", err, result, f.writes)
	}
}

func TestUnrelatedMatchingPodsAreIgnored(t *testing.T) {
	f := newFixture()
	f.onScale = func(n int32) {
		unrelated := newPod()
		unrelated.Name, unrelated.UID = "unrelated", "unrelated"
		unrelated.OwnerReferences[0].UID = "unrelated-rs"
		f.pods = append(f.pods, unrelated)
	}
	spec := testSpec()
	result, err := RunCanary(context.Background(), f.client, spec)
	if err != nil || len(result.Last.Pods) != 1 || len(result.CleanupLast.Pods) != 0 {
		t.Fatalf("unrelated matching pods affected cycle: %v %+v", err, result)
	}
}

func TestReadyCancellationStillCleansUp(t *testing.T) {
	f := newFixture()
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), struct{}{}, "identity"))
	defer cancel()
	client := contextClient{Interface: f.client, update: func(callCtx context.Context, scale *autoscalingv1.Scale) (*autoscalingv1.Scale, error) {
		if scale.Spec.Replicas == 0 && (callCtx.Err() != nil || callCtx.Value(struct{}{}) != "identity") {
			t.Fatal("cleanup lost context values or inherited cancellation")
		}
		return f.client.AppsV1().Deployments("guest").UpdateScale(callCtx, "canary", scale, metav1.UpdateOptions{})
	}}
	f.onScale = func(n int32) {
		if n == 1 {
			f.pods[0].Status.Phase = corev1.PodPending
			cancel()
		}
	}
	result, err := RunCanary(ctx, client, testSpec())
	if !errors.Is(err, context.Canceled) || !result.Ready.Completed.IsZero() || result.Cleanup.Completed.IsZero() {
		t.Fatalf("ready cancellation hidden or cleanup canceled: %v %+v", err, result)
	}
}

func TestConcurrentTemplateChangeAborts(t *testing.T) {
	for _, stage := range []int32{0, 1} {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			f := newFixture()
			f.onScale = func(n int32) {
				if n == stage {
					f.deployment.Generation++
				}
			}
			result, err := RunCanary(context.Background(), f.client, testSpec())
			if err == nil || !strings.Contains(err.Error(), "fixture changed") || (stage == 1 && !result.Ready.Completed.IsZero()) || (stage == 0 && !result.Cleanup.Completed.IsZero()) {
				t.Fatalf("concurrent change passed: %v %+v", err, result)
			}
		})
	}
}

func TestWatchAPIFailureIsReturned(t *testing.T) {
	f := newFixture()
	f.onScale = func(n int32) {
		if n == 1 {
			f.streams["pods"].Error(&metav1.Status{Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonExpired, Message: "pod watch expired"})
		}
	}
	result, err := RunCanary(context.Background(), f.client, testSpec())
	if err == nil || !strings.Contains(err.Error(), "pod watch expired") || !result.Ready.Completed.IsZero() || result.Cleanup.Completed.IsZero() {
		t.Fatalf("watch failure hidden: %v %+v", err, result)
	}
}

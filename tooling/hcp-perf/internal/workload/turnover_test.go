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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	typedappsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
)

type turnoverFixture struct {
	client  *fake.Clientset
	d       *appsv1.Deployment
	sets    []appsv1.ReplicaSet
	pods    []corev1.Pod
	writes  []*appsv1.Deployment
	onWrite func()
}

func newTurnoverFixture(t *testing.T, app int64) *turnoverFixture {
	t.Helper()
	ns, name := TurnoverApplication("apps", app)
	f := &turnoverFixture{client: fake.NewClientset(), d: &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name), ResourceVersion: "10", Generation: 10},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(5)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "unchanged"}}}},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: ptr.To(intstr.FromInt32(1)), MaxUnavailable: ptr.To(intstr.FromInt32(0))}},
		},
	}}
	f.converge()
	f.client.PrependReactor("get", "deployments", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() != ns || a.(clienttesting.GetAction).GetName() != name {
			t.Fatalf("wrong namespace identity or global name: %v", a)
		}
		return true, f.d.DeepCopy(), nil
	})
	f.client.PrependReactor("list", "replicasets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() != ns {
			t.Fatal("cross-namespace ReplicaSet list")
		}
		return true, (&appsv1.ReplicaSetList{Items: f.sets}).DeepCopy(), nil
	})
	f.client.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() != ns {
			t.Fatal("cross-namespace Pod list")
		}
		return true, (&corev1.PodList{Items: f.pods}).DeepCopy(), nil
	})
	f.client.PrependReactor("update", "deployments", func(a clienttesting.Action) (bool, runtime.Object, error) {
		d := a.(clienttesting.UpdateAction).GetObject().(*appsv1.Deployment)
		if a.GetSubresource() != "" || d.UID != f.d.UID || d.ResourceVersion != f.d.ResourceVersion || d.Spec.Template.Annotations[RolloutIDAnnotation] == "" || d.Spec.Template.Annotations[RolloutIDAnnotation] == f.d.Spec.Template.Annotations[RolloutIDAnnotation] {
			t.Fatalf("missing update guards or unique annotation: %+v", d)
		}
		expected := f.d.DeepCopy()
		expected.Spec.Template.Annotations = d.Spec.Template.Annotations
		if !reflect.DeepEqual(expected, d) {
			t.Fatal("update changed more than pod template annotations")
		}
		f.writes = append(f.writes, d.DeepCopy())
		f.d = d.DeepCopy()
		// Deliberately not initial+1: target must come from this response.
		f.d.Generation += 7
		f.d.ResourceVersion = fmt.Sprint(f.d.Generation)
		f.converge()
		if f.onWrite != nil {
			f.onWrite()
		}
		return true, f.d.DeepCopy(), nil
	})
	return f
}

func (f *turnoverFixture) converge() {
	d := f.d
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 5, UpdatedReplicas: 5, ReadyReplicas: 5, AvailableReplicas: 5}
	hash := fmt.Sprintf("hash-%d", d.Generation)
	rs := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: d.Name + "-" + hash, Namespace: d.Namespace, UID: types.UID(d.Name + "-" + hash), Labels: map[string]string{appsv1.DefaultDeploymentUniqueLabelKey: hash}, OwnerReferences: []metav1.OwnerReference{owner("Deployment", d.Name, d.UID)}}, Spec: appsv1.ReplicaSetSpec{Template: *d.Spec.Template.DeepCopy()}}
	rs.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = hash
	f.sets = append(f.sets, rs)
	f.pods = nil
	for i := 0; i < 5; i++ {
		metadata := rs.Spec.Template.ObjectMeta.DeepCopy()
		metadata.Name, metadata.Namespace, metadata.UID = fmt.Sprintf("%s-%d", rs.Name, i), d.Namespace, types.UID(fmt.Sprintf("%s-%d", rs.UID, i))
		metadata.OwnerReferences = []metav1.OwnerReference{owner("ReplicaSet", rs.Name, rs.UID)}
		f.pods = append(f.pods, corev1.Pod{ObjectMeta: *metadata, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}})
	}
}

func turnoverTestOptions(t *testing.T, f *turnoverFixture, schedule []experiment.Rollout) TurnoverOptions {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return TurnoverOptions{
		Factory: func(ns string) (kubernetes.Interface, error) {
			if f == nil || ns != f.d.Namespace {
				t.Fatalf("unexpected client factory namespace %s", ns)
			}
			return f.client, nil
		},
		OutputDir: filepath.Join(t.TempDir(), "turnover"), Schedule: schedule,
		Now: func() time.Time { return now },
		Wait: func(ctx context.Context, d time.Duration) error {
			if d <= 0 {
				t.Fatalf("nonpositive wait (busy loop): %s", d)
			}
			now = now.Add(d)
			return ctx.Err()
		},
		PollInterval: time.Second,
	}
}

func turnoverTestSpec() TurnoverSpec {
	return TurnoverSpec{Version: "v1", GuestNodes: 3, Applications: 40, Seed: 42, NamespacePrefix: "apps", DurationSeconds: 10, RecoverySeconds: 2, MaxOutstanding: 1}
}

func readTurnoverEvents(t *testing.T, dir string) []TurnoverEvent {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var events []TurnoverEvent
	for scanner.Scan() {
		var event TurnoverEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Sequence != int64(len(events)+1) {
			t.Fatalf("nonsequential evidence: %+v", event)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestTurnoverCompletionAndEvidence(t *testing.T) {
	f := newTurnoverFixture(t, 21)
	schedule := []experiment.Rollout{{Application: 21, Round: 1}, {Application: 21, Round: 2}}
	opts := turnoverTestOptions(t, f, schedule)
	f.onWrite = func() {
		events := readTurnoverEvents(t, opts.OutputDir)
		last := events[len(events)-1]
		if last.Type != "attempt" || last.Operation != "update deployment" {
			t.Fatalf("write issued before intent evidence: %+v", last)
		}
	}
	result, err := RunTurnover(context.Background(), turnoverTestSpec(), opts)
	if err != nil || result.Completed != 2 || result.Accepted != 2 || result.Failed != 0 || result.NotStarted != 0 || result.Outstanding != 0 || result.PeakOutstanding != 1 || len(f.writes) != 2 {
		t.Fatalf("bad completion: %v %+v", err, result)
	}
	for i, r := range result.Intents {
		if r.Namespace != "apps-1" || r.Deployment != "app-000021" || r.Generation != int64(17+i*7) || r.Attempts != 1 || r.AcceptedAt.IsZero() || !r.CompletedAt.After(r.AcceptedAt) || !r.ScheduledAt.Equal(result.StartedAt) {
			t.Fatalf("bad intent evidence: %+v", r)
		}
	}
	if result.Intents[1].AdmittedAt.Before(result.Intents[0].CompletedAt) || result.Intents[0].RolloutID == result.Intents[1].RolloutID {
		t.Fatal("same-app overlap or reused rollout ID")
	}
	for _, path := range []string{opts.OutputDir, filepath.Join(opts.OutputDir, "result.json"), filepath.Join(opts.OutputDir, "events.ndjson")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("nonprivate evidence: %s %v", path, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(opts.OutputDir, "result.json"))
	var persisted TurnoverResult
	if err != nil || json.Unmarshal(data, &persisted) != nil || persisted.Completed != 2 || persisted.Spec.TurnoverPeriodSeconds != 3600 {
		t.Fatalf("missing final evidence: %s %v", data, err)
	}
	events := readTurnoverEvents(t, opts.OutputDir)
	if events[0].Type != "pending" || events[1].Type != "pending" || events[len(events)-1].Type != "stopped" {
		t.Fatal("missing initial intents or stop evidence")
	}
}

func TestTurnoverCanonicalScheduleIsNotAccelerated(t *testing.T) {
	f := newTurnoverFixture(t, 0)
	spec := turnoverTestSpec()
	spec.DurationSeconds = 1
	expected, _ := experiment.Rollouts(spec.Applications, spec.Seed)
	f = newTurnoverFixture(t, expected[0].Application)
	opts := turnoverTestOptions(t, f, nil)
	result, err := RunTurnover(context.Background(), spec, opts)
	if err != nil || result.Scheduled != 1 || result.Admitted != 1 || result.Completed != 1 || result.NotStarted != 0 || len(f.writes) != 1 {
		t.Fatalf("schedule compressed or lost: %v %+v", err, result)
	}
	for i, intent := range result.Intents {
		if intent.Intent != expected[i] || intent.ScheduledAt.Sub(result.StartedAt) != expected[i].Offset {
			t.Fatalf("schedule changed at %d", i)
		}
	}
}

func TestTurnoverShortCanonicalWindow(t *testing.T) {
	spec := turnoverTestSpec()
	spec.Applications, spec.DurationSeconds, spec.RecoverySeconds = 3, 300, 30
	schedule, err := experiment.Rollouts(spec.Applications, spec.Seed)
	if err != nil || len(schedule) != 6 || schedule[0].Offset != 0 || schedule[1].Offset != 20*time.Minute {
		t.Fatalf("canonical hourly cadence changed: %+v %v", schedule, err)
	}
	f := newTurnoverFixture(t, schedule[0].Application)
	opts := turnoverTestOptions(t, f, nil)
	start := opts.Now()
	f.onWrite = func() {
		if !opts.Now().Equal(start) {
			t.Fatal("first rollout did not start at t=0")
		}
	}
	result, err := RunTurnover(t.Context(), spec, opts)
	if err != nil || result.Scheduled != 1 || result.Admitted != 1 || result.Accepted != 1 || result.Completed != 1 || result.NotStarted != 0 || result.Backlog != 0 || result.Failed != 0 || result.Outstanding != 0 || len(result.Intents) != 1 || len(f.writes) != 1 {
		t.Fatalf("short run counted future workload: %+v %v", result, err)
	}
	intent := result.Intents[0]
	if intent.Intent != schedule[0] || !intent.ScheduledAt.Equal(start) || !intent.AttemptedAt.Equal(start) || intent.Generation != 17 || len(intent.Last.Pods) != 5 || len(f.sets) != 2 || !intent.CompletedAt.After(intent.AcceptedAt) {
		t.Fatalf("real rollout chain did not complete: %+v", intent)
	}
	if result.AdmissionEnd.Sub(start) != 300*time.Second || result.FinishedAt.Sub(start) != 330*time.Second {
		t.Fatalf("short run changed steady/recovery windows: %+v", result)
	}
	for _, event := range readTurnoverEvents(t, opts.OutputDir) {
		if event.Backlog != 0 || (event.Intent != nil && event.Intent.Intent != schedule[0]) {
			t.Fatalf("future workload leaked into evidence: %+v", event)
		}
	}
	data, err := os.ReadFile(filepath.Join(opts.OutputDir, "result.json"))
	var persisted TurnoverResult
	if err != nil || json.Unmarshal(data, &persisted) != nil || persisted.Scheduled != 1 || persisted.Completed != 1 || persisted.NotStarted != 0 || persisted.Backlog != 0 || len(persisted.Intents) != 1 {
		t.Fatalf("persisted counts include future workload: %s %v", data, err)
	}
}

func TestTurnoverSuppliedScheduleWindow(t *testing.T) {
	for _, offset := range []time.Duration{0, 299 * time.Second, 300 * time.Second, 301 * time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			spec := turnoverTestSpec()
			spec.Applications, spec.DurationSeconds, spec.RecoverySeconds = 1, 300, 30
			spec.TurnoverPeriodSeconds = 300
			schedule := []experiment.Rollout{{Application: 0, Round: 1, Offset: offset}, {Application: 0, Round: 2, Offset: time.Hour}}
			original := append([]experiment.Rollout(nil), schedule...)
			f := newTurnoverFixture(t, 0)
			opts := turnoverTestOptions(t, f, schedule)
			result, err := RunTurnover(t.Context(), spec, opts)
			want := 0
			if offset < 300*time.Second {
				want = 1
			}
			if err != nil || result.Scheduled != want || result.Completed != want || result.NotStarted != 0 || result.Backlog != 0 || len(result.Intents) != want || len(f.writes) != want || !reflect.DeepEqual(schedule, original) {
				t.Fatalf("supplied schedule window changed: %+v %v", result, err)
			}
			if want == 1 && (result.Intents[0].Intent != original[0] || result.Intents[0].AttemptedAt.Sub(result.StartedAt) != offset) {
				t.Fatalf("supplied schedule accelerated: %+v", result.Intents[0])
			}
			if want == 0 && len(f.client.Actions()) != 0 {
				t.Fatal("out-of-window schedule touched API")
			}
			if result.FinishedAt.Sub(result.StartedAt) != 330*time.Second {
				t.Fatal("filtered schedule did not hold run window")
			}
		})
	}
}

func TestTurnoverRecoveryNeverAdmitsBacklog(t *testing.T) {
	f := newTurnoverFixture(t, 0)
	spec := turnoverTestSpec()
	spec.DurationSeconds, spec.RecoverySeconds = 1, 5
	schedule := []experiment.Rollout{{Application: 0, Round: 1}, {Application: 0, Round: 2}, {Application: 1, Round: 1}, {Application: 2, Round: 1, Offset: 2 * time.Second}}
	opts := turnoverTestOptions(t, f, schedule)
	result, err := RunTurnover(context.Background(), spec, opts)
	if err != nil || result.Scheduled != 3 || result.Completed != 1 || result.NotStarted != 2 || result.Backlog != 2 || result.Admitted != 1 || len(f.writes) != 1 || result.PeakOutstanding != 1 {
		t.Fatalf("recovery admitted/coalesced work: %v %+v", err, result)
	}
	for _, intent := range result.Intents[1:] {
		if intent.State != "pending" || intent.Attempts != 0 || !intent.AdmittedAt.IsZero() {
			t.Fatalf("unadmitted backlog changed: %+v", intent)
		}
	}
}

func TestTurnoverFalseCompletion(t *testing.T) {
	for name, change := range map[string]func(*turnoverFixture){
		"unobserved":          func(f *turnoverFixture) { f.d.Status.ObservedGeneration-- },
		"not updated":         func(f *turnoverFixture) { f.d.Status.UpdatedReplicas-- },
		"not ready":           func(f *turnoverFixture) { f.d.Status.ReadyReplicas-- },
		"not available":       func(f *turnoverFixture) { f.d.Status.AvailableReplicas-- },
		"old replicas":        func(f *turnoverFixture) { f.d.Status.Replicas++ },
		"wrong RS owner UID":  func(f *turnoverFixture) { f.sets[1].OwnerReferences[0].UID = "other" },
		"wrong RS owner API":  func(f *turnoverFixture) { f.sets[1].OwnerReferences[0].APIVersion = "other/v1" },
		"wrong RS annotation": func(f *turnoverFixture) { f.sets[1].Spec.Template.Annotations[RolloutIDAnnotation] = "other" },
		"wrong RS template":   func(f *turnoverFixture) { f.sets[1].Spec.Template.Spec.Containers[0].Image = "other" },
		"wrong RS hash":       func(f *turnoverFixture) { f.sets[1].Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "other" },
		"ambiguous RS": func(f *turnoverFixture) {
			f.sets[1].OwnerReferences = append(f.sets[1].OwnerReferences, owner("Deployment", "other", "other"))
		},
		"wrong pod owner UID":  func(f *turnoverFixture) { f.pods[0].OwnerReferences[0].UID = "other" },
		"wrong pod owner name": func(f *turnoverFixture) { f.pods[0].OwnerReferences[0].Name = "other" },
		"wrong pod owner kind": func(f *turnoverFixture) { f.pods[0].OwnerReferences[0].Kind = "Job" },
		"wrong pod owner API":  func(f *turnoverFixture) { f.pods[0].OwnerReferences[0].APIVersion = "other/v1" },
		"noncontroller pod":    func(f *turnoverFixture) { f.pods[0].OwnerReferences[0].Controller = ptr.To(false) },
		"ambiguous pod": func(f *turnoverFixture) {
			f.pods[0].OwnerReferences = append(f.pods[0].OwnerReferences, owner("ReplicaSet", "other", "other"))
		},
		"wrong pod annotation":   func(f *turnoverFixture) { f.pods[0].Annotations[RolloutIDAnnotation] = "other" },
		"wrong pod hash":         func(f *turnoverFixture) { f.pods[0].Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "other" },
		"missing UID":            func(f *turnoverFixture) { f.pods[0].UID = "" },
		"missing selector label": func(f *turnoverFixture) { delete(f.pods[0].Labels, "app") },
		"pending pod":            func(f *turnoverFixture) { f.pods[0].Status.Phase = corev1.PodPending },
		"not ready pod":          func(f *turnoverFixture) { f.pods[0].Status.Conditions = nil },
		"deleting new pod":       func(f *turnoverFixture) { f.pods[0].DeletionTimestamp = ptr.To(metav1.Now()) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newTurnoverFixture(t, 0)
			f.onWrite = func() { change(f) }
			opts := turnoverTestOptions(t, f, []experiment.Rollout{{Application: 0, Round: 1}, {Application: 0, Round: 2}})
			result, err := RunTurnover(context.Background(), turnoverTestSpec(), opts)
			if err == nil || result.Completed != 0 || result.Failed != 1 || result.NotStarted != 1 || len(f.writes) != 1 || result.Intents[0].CompletedAt != (time.Time{}) {
				t.Fatalf("false success or superseding write: %v %+v", err, result)
			}
		})
	}
}

func TestTurnoverOldPodsBlockEvenAfterRSDeletion(t *testing.T) {
	for _, reparent := range []bool{false, true} {
		t.Run(fmt.Sprint(reparent), func(t *testing.T) {
			f := newTurnoverFixture(t, 0)
			old := *f.pods[0].DeepCopy()
			old.DeletionTimestamp = ptr.To(metav1.Now())
			old.Labels = nil
			if reparent {
				old.OwnerReferences = nil
			}
			f.onWrite = func() {
				f.sets = f.sets[1:]
				f.pods = append(f.pods, old)
			}
			opts := turnoverTestOptions(t, f, []experiment.Rollout{{Application: 0, Round: 1}})
			result, err := RunTurnover(context.Background(), turnoverTestSpec(), opts)
			if err == nil || result.Completed != 0 || result.Accepted != 1 {
				t.Fatalf("old deleting Pod ignored: %v %+v", err, result)
			}
		})
	}
}

func TestTurnoverWaitsForOldPodRemoval(t *testing.T) {
	f := newTurnoverFixture(t, 0)
	old := *f.pods[0].DeepCopy()
	f.onWrite = func() { f.pods = append(f.pods, old) }
	opts := turnoverTestOptions(t, f, []experiment.Rollout{{Application: 0, Round: 1}})
	wait := opts.Wait
	opts.Wait = func(ctx context.Context, d time.Duration) error {
		if len(f.writes) == 1 && len(f.pods) == 6 && opts.Now().Sub(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) >= time.Second {
			f.pods = f.pods[:5]
		}
		return wait(ctx, d)
	}
	result, err := RunTurnover(context.Background(), turnoverTestSpec(), opts)
	if err != nil || result.Completed != 1 || result.Intents[0].CompletedAt.Sub(result.StartedAt) != 2*time.Second {
		t.Fatalf("did not wait for actual old Pod removal: %v %+v", err, result)
	}
}

func TestTurnoverReadRetriesAreBoundedAndRecorded(t *testing.T) {
	for _, failures := range []int{2, 100} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			f := newTurnoverFixture(t, 0)
			reads := 0
			f.client.PrependReactor("get", "deployments", func(a clienttesting.Action) (bool, runtime.Object, error) {
				reads++
				if reads <= failures {
					return true, nil, apierrors.NewTooManyRequests("throttled", 1)
				}
				return false, nil, nil
			})
			spec := turnoverTestSpec()
			spec.DurationSeconds = 100
			opts := turnoverTestOptions(t, f, []experiment.Rollout{{Application: 0, Round: 1}})
			result, err := RunTurnover(context.Background(), spec, opts)
			if failures == 2 && (err != nil || result.Completed != 1 || result.Intents[0].AttemptedAt.Sub(result.StartedAt) != 3*time.Second) {
				t.Fatalf("read retry failed or no backoff: %v %+v", err, result)
			}
			if failures == 100 && (err == nil || reads != 6 || result.Failed != 1 || len(f.writes) != 0) {
				t.Fatalf("unbounded retries: reads=%d %v %+v", reads, err, result)
			}
			errorsRecorded := 0
			for _, e := range readTurnoverEvents(t, opts.OutputDir) {
				if e.Type == "response" && e.Error != "" {
					errorsRecorded++
				}
			}
			if errorsRecorded != min(failures, 6) {
				t.Fatalf("read errors hidden: %d", errorsRecorded)
			}
		})
	}
}

func TestTurnoverWriteErrorsNeverRetryOrSupersede(t *testing.T) {
	for name, failure := range map[string]error{
		"429":       apierrors.NewTooManyRequests("throttled", 1),
		"conflict":  apierrors.NewConflict(schema.GroupResource{Resource: "deployments"}, "app", errors.New("conflict")),
		"forbidden": apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "app", errors.New("denied")),
		"ambiguous": context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			f := newTurnoverFixture(t, 0)
			updates := 0
			f.client.PrependReactor("update", "deployments", func(a clienttesting.Action) (bool, runtime.Object, error) {
				updates++
				if name == "ambiguous" {
					f.d = a.(clienttesting.UpdateAction).GetObject().(*appsv1.Deployment).DeepCopy()
				}
				return true, nil, failure
			})
			opts := turnoverTestOptions(t, f, []experiment.Rollout{{Application: 0, Round: 1}, {Application: 0, Round: 2}})
			result, err := RunTurnover(context.Background(), turnoverTestSpec(), opts)
			if err == nil || result.Completed != 0 || result.Accepted != 0 || result.Failed != 1 || result.NotStarted != 1 || updates != 1 || result.Intents[0].Attempts != 1 || !strings.Contains(result.Intents[0].Error, failure.Error()) {
				t.Fatalf("write failure hidden/retried: %v %+v", err, result)
			}
			for _, action := range f.client.Actions() {
				if action.GetVerb() != "get" && action.GetVerb() != "list" && action.GetVerb() != "update" {
					t.Fatalf("unexpected cleanup or repair: %v", action)
				}
			}
		})
	}
}

func TestTurnoverInvalidSpecsDoNotTouchAPIOrDisk(t *testing.T) {
	for _, change := range []func(*TurnoverSpec, *TurnoverOptions){
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.Version = "v2" },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.GuestNodes = -1 },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.GuestNodes = 1001 },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.Applications = -1 },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.Applications = 40001 },
		func(s *TurnoverSpec, o *TurnoverOptions) {
			s.Applications = 0
			o.Schedule = []experiment.Rollout{{Round: 1}}
		},
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.NamespacePrefix = "bad/name" },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.NamespacePrefix = strings.Repeat("a", 60) },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.DurationSeconds = -1 },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.DurationSeconds = 7201 },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.TurnoverPeriodSeconds = -1 },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.TurnoverPeriodSeconds = 86401 },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.TurnoverPeriodSeconds = math.MaxInt64 },
		func(s *TurnoverSpec, _ *TurnoverOptions) {
			s.Applications, s.DurationSeconds, s.TurnoverPeriodSeconds = 40000, 7200, 1
		},
		func(_ *TurnoverSpec, o *TurnoverOptions) {
			o.Schedule = make([]experiment.Rollout, experiment.MaxScheduledRollouts+1)
		},
		func(_ *TurnoverSpec, o *TurnoverOptions) { o.Schedule = []experiment.Rollout{{Round: 3}} },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.RecoverySeconds = -1 },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.RecoverySeconds = 601 },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.MaxOutstanding = 0 },
		func(s *TurnoverSpec, _ *TurnoverOptions) { s.MaxOutstanding = 101 },
		func(_ *TurnoverSpec, o *TurnoverOptions) { o.Factory = nil },
		func(_ *TurnoverSpec, o *TurnoverOptions) { o.Wait = nil },
		func(_ *TurnoverSpec, o *TurnoverOptions) { o.RequestTimeout = 31 * time.Second },
		func(_ *TurnoverSpec, o *TurnoverOptions) { o.PollInterval = -1 },
		func(_ *TurnoverSpec, o *TurnoverOptions) {
			o.Schedule = []experiment.Rollout{{Application: 40, Round: 1}}
		},
		func(_ *TurnoverSpec, o *TurnoverOptions) { o.Schedule = []experiment.Rollout{{Round: 0}} },
		func(_ *TurnoverSpec, o *TurnoverOptions) {
			o.Schedule = []experiment.Rollout{{Round: 1, Offset: -time.Second}}
		},
		func(_ *TurnoverSpec, o *TurnoverOptions) {
			o.Schedule = []experiment.Rollout{{Round: 1}, {Round: 2, Offset: time.Hour}, {Round: 2, Offset: time.Hour}}
		},
		func(_ *TurnoverSpec, o *TurnoverOptions) {
			o.Schedule = []experiment.Rollout{{Round: 1, Offset: 2 * time.Hour}}
		},
		func(_ *TurnoverSpec, o *TurnoverOptions) { o.Schedule = []experiment.Rollout{{Round: 1}, {Round: 1}} },
		func(_ *TurnoverSpec, o *TurnoverOptions) {
			o.Schedule = []experiment.Rollout{{Round: 1, Offset: time.Second}, {Round: 2}}
		},
	} {
		f := newTurnoverFixture(t, 0)
		spec, opts := turnoverTestSpec(), turnoverTestOptions(t, f, nil)
		change(&spec, &opts)
		if _, err := RunTurnover(context.Background(), spec, opts); err == nil || len(f.client.Actions()) != 0 {
			t.Fatalf("invalid spec accepted/touched API: %+v", spec)
		}
		if _, err := os.Stat(opts.OutputDir); !os.IsNotExist(err) {
			t.Fatalf("invalid spec touched disk: %v", err)
		}
	}
}

func TestTurnoverBrokenFixturesNeverMutate(t *testing.T) {
	for name, change := range map[string]func(*turnoverFixture){
		"replicas":      func(f *turnoverFixture) { f.d.Spec.Replicas = ptr.To(int32(4)) },
		"surge":         func(f *turnoverFixture) { f.d.Spec.Strategy.RollingUpdate.MaxSurge = ptr.To(intstr.FromInt32(2)) },
		"unavailable":   func(f *turnoverFixture) { f.d.Spec.Strategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromInt32(1)) },
		"recreate":      func(f *turnoverFixture) { f.d.Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType },
		"paused":        func(f *turnoverFixture) { f.d.Spec.Paused = true },
		"deleting":      func(f *turnoverFixture) { f.d.DeletionTimestamp = ptr.To(metav1.Now()) },
		"unversioned":   func(f *turnoverFixture) { f.d.ResourceVersion = "" },
		"empty UID":     func(f *turnoverFixture) { f.d.UID = "" },
		"selector":      func(f *turnoverFixture) { f.d.Spec.Selector = nil },
		"not converged": func(f *turnoverFixture) { f.d.Status.ObservedGeneration-- },
		"missing pod":   func(f *turnoverFixture) { f.pods = f.pods[:4] },
	} {
		t.Run(name, func(t *testing.T) {
			f := newTurnoverFixture(t, 0)
			change(f)
			result, err := RunTurnover(context.Background(), turnoverTestSpec(), turnoverTestOptions(t, f, []experiment.Rollout{{Round: 1}}))
			if err == nil || result.Failed != 1 || result.Intents[0].Attempts != 0 || len(f.writes) != 0 {
				t.Fatalf("broken fixture mutated: %v %+v", err, result)
			}
		})
	}
}

func TestTurnoverZeroApplicationsHoldsSteadyWindow(t *testing.T) {
	for _, duration := range []int64{0, 1, 7200} {
		t.Run(fmt.Sprint(duration), func(t *testing.T) {
			spec := turnoverTestSpec()
			spec.Applications, spec.DurationSeconds, spec.RecoverySeconds = 0, duration, 0
			spec.TurnoverPeriodSeconds = 1
			opts := turnoverTestOptions(t, nil, nil) // Factory fails if called.
			result, err := RunTurnover(context.Background(), spec, opts)
			if duration == 0 {
				duration = 7200
			}
			if err != nil || result.Spec.GuestNodes != 3 || result.Spec.Applications != 0 || result.Spec.DurationSeconds != duration || result.FinishedAt.Sub(result.StartedAt) != time.Duration(duration)*time.Second || !result.FinishedAt.Equal(result.AdmissionEnd) {
				t.Fatalf("empty run did not hold steady window: %v %+v", err, result)
			}
			if result.Scheduled != 0 || result.Admitted != 0 || result.Accepted != 0 || result.Completed != 0 || result.Failed != 0 || result.NotStarted != 0 || result.Backlog != 0 || result.Outstanding != 0 || result.PeakOutstanding != 0 || len(result.Intents) != 0 {
				t.Fatalf("empty run reported work: %+v", result)
			}
			events := readTurnoverEvents(t, opts.OutputDir)
			if len(events) != 2 || events[0].Type != "backlog" || events[1].Type != "stopped" || events[1].At.Sub(events[0].At) != time.Duration(duration)*time.Second {
				t.Fatalf("empty run evidence lost hold: %+v", events)
			}
		})
	}
}

func TestTurnoverCancellationPreservesObjects(t *testing.T) {
	f := newTurnoverFixture(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	f.onWrite = cancel
	opts := turnoverTestOptions(t, f, []experiment.Rollout{{Round: 1}, {Round: 2}})
	result, err := RunTurnover(ctx, turnoverTestSpec(), opts)
	if !errors.Is(err, context.Canceled) || len(f.writes) != 1 || f.d.Spec.Template.Annotations[RolloutIDAnnotation] == "" || result.Failed != 1 || result.NotStarted != 1 || result.Completed != 0 {
		t.Fatalf("cancellation ignored or rolled back: %v %+v", err, result)
	}
}

func TestTurnoverEmptyHoldCancellation(t *testing.T) {
	spec := turnoverTestSpec()
	spec.Applications, spec.DurationSeconds = 0, 7200
	opts := turnoverTestOptions(t, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wait := opts.Wait
	opts.Wait = func(ctx context.Context, d time.Duration) error {
		if d != 2*time.Hour {
			t.Fatalf("empty run did not wait for steady window: %s", d)
		}
		cancel()
		return wait(ctx, time.Minute)
	}
	result, err := RunTurnover(ctx, spec, opts)
	if !errors.Is(err, context.Canceled) || result.FinishedAt.Sub(result.StartedAt) != time.Minute || result.Scheduled != 0 {
		t.Fatalf("empty hold ignored cancellation: %+v %v", result, err)
	}
}

func TestTurnoverApplicationCountIndependentOfNodes(t *testing.T) {
	for _, nodes := range []int64{1, 3} {
		for _, apps := range []int64{1, 21, 41, 40000} {
			t.Run(fmt.Sprintf("nodes-%d/apps-%d", nodes, apps), func(t *testing.T) {
				spec := turnoverTestSpec()
				spec.GuestNodes, spec.Applications = nodes, apps
				f := newTurnoverFixture(t, apps-1)
				opts := turnoverTestOptions(t, f, []experiment.Rollout{{Application: apps - 1, Round: 1}})
				result, err := RunTurnover(t.Context(), spec, opts)
				if err != nil || result.Completed != 1 || result.Scheduled != 1 || len(f.writes) != 1 {
					t.Fatalf("application count tied to nodes: %+v %v", result, err)
				}
			})
		}
	}
}

type turnoverContextClient struct {
	kubernetes.Interface
	get func(context.Context) (*appsv1.Deployment, error)
}
type turnoverContextApps struct {
	typedappsv1.AppsV1Interface
	get func(context.Context) (*appsv1.Deployment, error)
}
type turnoverContextDeployments struct {
	typedappsv1.DeploymentInterface
	get func(context.Context) (*appsv1.Deployment, error)
}

func (c turnoverContextClient) AppsV1() typedappsv1.AppsV1Interface {
	return turnoverContextApps{c.Interface.AppsV1(), c.get}
}
func (c turnoverContextApps) Deployments(ns string) typedappsv1.DeploymentInterface {
	return turnoverContextDeployments{c.AppsV1Interface.Deployments(ns), c.get}
}
func (c turnoverContextDeployments) Get(ctx context.Context, _ string, _ metav1.GetOptions) (*appsv1.Deployment, error) {
	return c.get(ctx)
}

func TestTurnoverRequestContextsAreBounded(t *testing.T) {
	f := newTurnoverFixture(t, 0)
	opts := turnoverTestOptions(t, f, []experiment.Rollout{{Round: 1}})
	opts.RequestTimeout = time.Millisecond
	calls := 0
	opts.Factory = func(string) (kubernetes.Interface, error) {
		return turnoverContextClient{Interface: f.client, get: func(ctx context.Context) (*appsv1.Deployment, error) {
			calls++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > time.Millisecond {
				t.Fatal("request missing bounded deadline")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}}, nil
	}
	result, err := RunTurnover(context.Background(), turnoverTestSpec(), opts)
	if err == nil || calls > 6 || result.Completed != 0 || len(f.writes) != 0 {
		t.Fatalf("network timeout failed: calls=%d err=%v", calls, err)
	}
}

func TestTurnoverOutstandingBoundAndNamespaceIdentities(t *testing.T) {
	first, second, third := newTurnoverFixture(t, 0), newTurnoverFixture(t, 20), newTurnoverFixture(t, 40)
	spec := turnoverTestSpec()
	spec.Applications, spec.MaxOutstanding, spec.DurationSeconds = 41, 2, 1
	opts := turnoverTestOptions(t, nil, []experiment.Rollout{{Application: 0, Round: 1}, {Application: 20, Round: 1}, {Application: 40, Round: 1}})
	identities := map[string]*turnoverFixture{"apps-0": first, "apps-1": second, "apps-2": third}
	var requested []string
	opts.Factory = func(ns string) (kubernetes.Interface, error) {
		requested = append(requested, ns)
		return identities[ns].client, nil
	}
	result, err := RunTurnover(context.Background(), spec, opts)
	if err != nil || result.PeakOutstanding != 2 || result.Completed != 2 || result.NotStarted != 1 || result.Backlog != 1 || fmt.Sprint(requested) != "[apps-0 apps-1]" || len(third.client.Actions()) != 0 {
		t.Fatalf("outstanding limit or identity boundary violated: %v %+v requests=%v", err, result, requested)
	}
}

func TestTurnoverReplacementAndConcurrentChange(t *testing.T) {
	for _, change := range []string{"replacement", "generation", "annotation"} {
		t.Run(change, func(t *testing.T) {
			f := newTurnoverFixture(t, 0)
			opts := turnoverTestOptions(t, f, []experiment.Rollout{{Round: 1}, {Round: 2}})
			wait := opts.Wait
			opts.Wait = func(ctx context.Context, d time.Duration) error {
				switch change {
				case "replacement":
					f.d.UID = "replacement"
				case "generation":
					f.d.Generation++
				case "annotation":
					f.d.Spec.Template.Annotations[RolloutIDAnnotation] = "external-rollout"
				}
				return wait(ctx, d)
			}
			result, err := RunTurnover(context.Background(), turnoverTestSpec(), opts)
			if err == nil || result.Failed != 1 || result.Accepted != 1 || result.Completed != 0 || result.NotStarted != 1 || len(f.writes) != 1 {
				t.Fatalf("external change passed or was superseded: %v %+v", err, result)
			}
		})
	}
}

func TestTurnoverUnrelatedPodsDoNotBlock(t *testing.T) {
	f := newTurnoverFixture(t, 0)
	unrelated := *f.pods[0].DeepCopy()
	unrelated.UID = "unrelated"
	unrelated.OwnerReferences[0].UID = "unrelated-rs"
	unrelated.DeletionTimestamp = ptr.To(metav1.Now())
	f.pods = append(f.pods, unrelated)
	f.onWrite = func() { f.pods = append(f.pods, unrelated) }
	result, err := RunTurnover(context.Background(), turnoverTestSpec(), turnoverTestOptions(t, f, []experiment.Rollout{{Round: 1}}))
	if err != nil || result.Completed != 1 || len(result.Intents[0].Last.Pods) != 5 {
		t.Fatalf("unrelated Pod blocked completion: %v %+v", err, result)
	}
}

func TestTurnoverRequiresNewOutputDirectory(t *testing.T) {
	f := newTurnoverFixture(t, 0)
	opts := turnoverTestOptions(t, f, []experiment.Rollout{{Round: 1}})
	opts.OutputDir = t.TempDir()
	if _, err := RunTurnover(context.Background(), turnoverTestSpec(), opts); err == nil || len(f.client.Actions()) != 0 {
		t.Fatalf("existing output directory accepted: %v", err)
	}
}

func TestTurnoverMissingNamespaceCredentialsRecorded(t *testing.T) {
	opts := turnoverTestOptions(t, nil, []experiment.Rollout{{Round: 1}, {Round: 2}})
	opts.Factory = func(string) (kubernetes.Interface, error) { return nil, errors.New("missing namespace credential") }
	result, err := RunTurnover(context.Background(), turnoverTestSpec(), opts)
	if err == nil || result.Failed != 1 || result.NotStarted != 1 || result.Intents[0].Attempts != 0 || !strings.Contains(result.Intents[0].Error, "missing namespace credential") {
		t.Fatalf("missing credentials hidden: %v %+v", err, result)
	}
}

func TestTurnoverScaleBoundAndExample(t *testing.T) {
	schedule, err := experiment.Rollouts(40000, 42)
	if err != nil || len(schedule) != 80000 || schedule[40000].Offset != time.Hour {
		t.Fatalf("canonical scale schedule changed: len=%d err=%v", len(schedule), err)
	}
	ns, name := TurnoverApplication("apps", 39999)
	if ns != "apps-1999" || name != "app-039999" {
		t.Fatalf("global index mapping changed: %s/%s", ns, name)
	}
	f, err := os.Open("../../examples/turnover.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var spec TurnoverSpec
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil || spec.Version != "v1" || spec.GuestNodes != 3 || spec.Applications != 0 || spec.DurationSeconds != 7200 || spec.TurnoverPeriodSeconds != 3600 || spec.MaxOutstanding < 1 || spec.MaxOutstanding > 100 || spec.RecoverySeconds > 600 {
		t.Fatalf("invalid canonical example: %+v %v", spec, err)
	}
}

func TestTurnoverPeriodPreservesBacklogAndConcurrency(t *testing.T) {
	for _, period := range []int64{3600, 1800, 900, 300} {
		t.Run(fmt.Sprint(period), func(t *testing.T) {
			spec := turnoverTestSpec()
			spec.Applications, spec.DurationSeconds, spec.TurnoverPeriodSeconds = 120, 300, period
			schedule, err := experiment.RolloutsForPeriod(spec.Applications, spec.Seed, 300*time.Second, time.Duration(period)*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			f := newTurnoverFixture(t, schedule[0].Application)
			f.onWrite = func() { f.d.Status.ReadyReplicas = 0 }
			opts := turnoverTestOptions(t, f, nil)
			result, err := RunTurnover(t.Context(), spec, opts)
			want := int(120 * 300 / period)
			if err == nil || result.Scheduled != want || result.Admitted != 1 || result.Accepted != 1 || result.Failed != 1 || result.Backlog != want-1 || result.NotStarted != want-1 || result.PeakOutstanding != 1 || result.Spec.MaxOutstanding != 1 || result.Spec.Applications != 120 || len(f.writes) != 1 {
				t.Fatalf("period changed concurrency or lost backlog: %+v %v", result, err)
			}
			for i, intent := range result.Intents {
				if intent.Intent != schedule[i] || intent.ScheduledAt.Sub(result.StartedAt) != schedule[i].Offset || (i > 0 && (intent.Attempts != 0 || !intent.AdmittedAt.IsZero())) {
					t.Fatalf("original intent lost or admitted despite saturation: %+v", intent)
				}
			}
			data, err := os.ReadFile(filepath.Join(opts.OutputDir, "result.json"))
			var persisted TurnoverResult
			if err != nil || json.Unmarshal(data, &persisted) != nil || persisted.Spec.TurnoverPeriodSeconds != period || persisted.Scheduled != want || persisted.Backlog != want-1 {
				t.Fatalf("period or backlog not persisted: %s %v", data, err)
			}
		})
	}
}

func TestTurnoverRepeatedPeriodsNeverSupersede(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(fmt.Sprint(slow), func(t *testing.T) {
			spec := turnoverTestSpec()
			spec.Applications, spec.DurationSeconds, spec.RecoverySeconds, spec.TurnoverPeriodSeconds = 1, 10, 10, 1
			spec.MaxOutstanding = 100
			f := newTurnoverFixture(t, 0)
			opts := turnoverTestOptions(t, f, nil)
			if slow {
				opts.PollInterval = 17 * time.Second
			}
			result, err := RunTurnover(t.Context(), spec, opts)
			want := 10
			if slow {
				want = 1
			}
			if err != nil || result.Scheduled != 10 || result.Completed != want || result.Backlog != 10-want || result.NotStarted != 10-want || result.PeakOutstanding != 1 || len(f.writes) != want {
				t.Fatalf("repeated periods overlapped or dropped: %+v %v", result, err)
			}
			for i, intent := range result.Intents {
				if intent.Intent.Round != i+1 || intent.Intent.Offset != time.Duration(i)*time.Second {
					t.Fatalf("round or original offset changed: %+v", intent)
				}
				if i > 0 && !intent.AdmittedAt.IsZero() && intent.AdmittedAt.Before(result.Intents[i-1].CompletedAt) {
					t.Fatal("unfinished rollout superseded")
				}
			}
		})
	}
}

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
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// WatchDiagnostics counts both phases. Reconnects stays zero: any stream gap
// invalidates that phase, rather than silently relisting into a valid sample.
type WatchDiagnostics struct {
	Events     int      `json:"events"`
	Reconnects int      `json:"reconnects"`
	Gaps       int      `json:"gaps"`
	Errors     []string `json:"errors,omitempty"`
}

type canaryEvent struct {
	kind     string
	event    watch.Event
	err      error
	received time.Time
}

type canaryPodRemoval struct {
	pod      *corev1.Pod
	received time.Time
}

// Each kind has its own opaque LIST RV and ordered stream. Cross-kind delivery
// is not linearizable: completion uses the latest client receipt contributing
// to the satisfied state, not processing time or unrelated events/bookmarks.
// Unknown-RS pods are retained until the RS stream establishes their ownership.
// Zero means observed Deployment/RS zero and no cached owned pods (even terminal
// or deleting pods block it), not a server-wide atomic absence guarantee.
type canaryWatch struct {
	conservativeDrain bool
	mu                sync.Mutex
	deployment        *appsv1.Deployment
	sets              map[types.UID]*appsv1.ReplicaSet
	knownSets         map[types.UID]string
	pods              map[types.UID]*corev1.Pod
	oldPods           map[types.UID]bool
	uid               types.UID
	selector          labels.Selector
	generation        int64
	replicas          int32
	last              Snapshot
	completed         time.Time
	armedAt           time.Time
	deploymentReceipt time.Time
	setReceipts       map[types.UID]time.Time
	podReceipts       map[types.UID]time.Time
	removedPods       []canaryPodRemoval
	removedReceipt    time.Time
	err               error
	abort             context.CancelFunc
	stats             WatchDiagnostics
	streams           map[string]watch.Interface
	cancel            context.CancelFunc
	ctx               context.Context
	queue             chan canaryEvent
	changed           chan struct{}
	wg                sync.WaitGroup
	readers           sync.WaitGroup
	stopOnce          sync.Once
}

func newCanaryWatch(ctx context.Context, client kubernetes.Interface, spec Spec, known map[types.UID]string) (w *canaryWatch, err error) {
	w = &canaryWatch{sets: map[types.UID]*appsv1.ReplicaSet{}, knownSets: map[types.UID]string{}, pods: map[types.UID]*corev1.Pod{}, oldPods: map[types.UID]bool{}, streams: map[string]watch.Interface{}, queue: make(chan canaryEvent, 256), changed: make(chan struct{}, 1)}
	w.setReceipts = map[types.UID]time.Time{}
	w.podReceipts = map[types.UID]time.Time{}
	for uid, name := range known {
		w.knownSets[uid] = name
	}
	// Establishment is bounded by ctx, but canceling preflight must not kill
	// watch response bodies during Ready. stop cancels and joins all readers.
	w.ctx, w.cancel = context.WithCancel(context.WithoutCancel(ctx))
	establishCancel := context.AfterFunc(ctx, w.cancel)
	defer func() {
		establishCancel()
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			w.stop()
			w = nil
		}
	}()
	deploymentOptions := metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", spec.Name).String()}
	deployments, err := client.AppsV1().Deployments(spec.Namespace).List(ctx, deploymentOptions)
	if err != nil {
		return w, fmt.Errorf("list deployments: %w", err)
	}
	if len(deployments.Items) != 1 {
		return w, fmt.Errorf("fixture requires exactly one Deployment")
	}
	w.deployment = deployments.Items[0].DeepCopy()
	sets, err := client.AppsV1().ReplicaSets(spec.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return w, fmt.Errorf("list replicasets: %w", err)
	}
	for i := range sets.Items {
		rs := sets.Items[i].DeepCopy()
		w.sets[rs.UID] = rs
	}
	pods, err := client.CoreV1().Pods(spec.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return w, fmt.Errorf("list pods: %w", err)
	}
	for i := range pods.Items {
		pod := pods.Items[i].DeepCopy()
		w.pods[pod.UID] = pod
		w.oldPods[pod.UID] = true
	}
	for _, stream := range []struct {
		kind    string
		rv      string
		options metav1.ListOptions
		open    func(context.Context, metav1.ListOptions) (watch.Interface, error)
	}{
		{"deployments", deployments.ResourceVersion, deploymentOptions, client.AppsV1().Deployments(spec.Namespace).Watch},
		{"replicasets", sets.ResourceVersion, metav1.ListOptions{}, client.AppsV1().ReplicaSets(spec.Namespace).Watch},
		{"pods", pods.ResourceVersion, metav1.ListOptions{}, client.CoreV1().Pods(spec.Namespace).Watch},
	} {
		if stream.rv == "" {
			return w, fmt.Errorf("%s LIST omitted resourceVersion", stream.kind)
		}
		stream.options.ResourceVersion = stream.rv
		stream.options.AllowWatchBookmarks = true
		opened, openErr := stream.open(w.ctx, stream.options)
		if openErr != nil {
			return w, fmt.Errorf("establish %s watch: %w", stream.kind, openErr)
		}
		w.streams[stream.kind] = opened
	}
	return w, nil
}

func (w *canaryWatch) start() {
	for kind, stream := range w.streams {
		w.readers.Add(1)
		go func() {
			defer utilruntime.HandleCrash()
			defer w.readers.Done()
			for {
				select {
				case <-w.ctx.Done():
					return
				case event, ok := <-stream.ResultChan():
					if !ok && w.ctx.Err() != nil {
						return
					}
					item := canaryEvent{kind: kind, event: event, received: time.Now()}
					if !ok {
						item.err = fmt.Errorf("watch closed before phase completed")
					}
					select {
					case w.queue <- item:
					default:
						w.mu.Lock()
						w.fail(fmt.Errorf("%s watch event buffer overflow", kind))
						w.mu.Unlock()
						return
					}
					if !ok {
						return
					}
				}
			}
		}()
	}
	w.wg.Add(1)
	go func() {
		defer utilruntime.HandleCrash()
		defer w.wg.Done()
		// Only stop closes the queue, after joining every producer. Cancellation
		// must not discard accepted events, especially errors behind readiness.
		for item := range w.queue {
			w.mu.Lock()
			if item.err != nil {
				w.fail(fmt.Errorf("%s: %w", item.kind, item.err))
			} else {
				w.stats.Events++
				if err := w.apply(item); err != nil {
					w.fail(fmt.Errorf("%s: %w", item.kind, err))
				} else {
					w.evaluate(item.received)
				}
			}
			w.mu.Unlock()
		}
	}()
}

func (w *canaryWatch) stop() {
	w.stopOnce.Do(func() {
		w.cancel()
		for _, stream := range w.streams {
			stream.Stop()
		}
		w.readers.Wait()
		close(w.queue)
		w.wg.Wait()
		close(w.changed)
	})
}

func (w *canaryWatch) diagnostics(out *WatchDiagnostics) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out.Events += w.stats.Events
	out.Gaps += w.stats.Gaps
	out.Errors = append(out.Errors, w.stats.Errors...)
}

func (w *canaryWatch) fail(err error) {
	if w.err == nil {
		w.err = fmt.Errorf("invalid watch measurement: %w", err)
		w.stats.Gaps++
		w.stats.Errors = append(w.stats.Errors, w.err.Error())
	}
	if w.abort != nil {
		w.abort()
	}
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

func (w *canaryWatch) apply(item canaryEvent) error {
	e := item.event
	if e.Type == watch.Error {
		return apierrors.FromObject(e.Object)
	}
	if e.Type == watch.Bookmark {
		return nil
	}
	if e.Type != watch.Added && e.Type != watch.Modified && e.Type != watch.Deleted {
		return fmt.Errorf("unexpected watch event %q", e.Type)
	}
	switch o := e.Object.(type) {
	case *appsv1.Deployment:
		if item.kind != "deployments" {
			break
		}
		if e.Type == watch.Deleted {
			return fmt.Errorf("fixture Deployment deleted")
		}
		w.deployment = o.DeepCopy()
		w.deploymentReceipt = item.received
		return nil
	case *appsv1.ReplicaSet:
		if item.kind != "replicasets" {
			break
		}
		// Retain exact ownership after deletion until its pods disappear.
		owned, err := controlledBy(o.OwnerReferences, "Deployment", w.deployment.Name, w.uid)
		if err != nil {
			return err
		}
		if owned {
			w.knownSets[o.UID] = o.Name
		} else {
			if _, wasOwned := w.knownSets[o.UID]; wasOwned && item.received.After(w.removedReceipt) {
				w.removedReceipt = item.received
			}
			delete(w.knownSets, o.UID)
		}
		w.setReceipts[o.UID] = item.received
		if e.Type == watch.Deleted {
			delete(w.sets, o.UID)
		} else {
			w.sets[o.UID] = o.DeepCopy()
		}
		return nil
	case *corev1.Pod:
		if item.kind != "pods" {
			break
		}
		if e.Type == watch.Deleted {
			w.removedPods = append(w.removedPods, canaryPodRemoval{pod: o.DeepCopy(), received: item.received})
			delete(w.pods, o.UID)
		} else {
			// An ownership change can remove a pod from the counted set too.
			if previous := w.pods[o.UID]; previous != nil && !reflect.DeepEqual(previous.OwnerReferences, o.OwnerReferences) {
				w.removedPods = append(w.removedPods, canaryPodRemoval{pod: previous, received: item.received})
			}
			w.pods[o.UID] = o.DeepCopy()
		}
		w.podReceipts[o.UID] = item.received
		if w.armedAt.IsZero() || !item.received.After(w.armedAt) {
			w.oldPods[o.UID] = true
		}
		return nil
	}
	return fmt.Errorf("unexpected watch object %T", e.Object)
}

func (w *canaryWatch) arm(generation int64, replicas int32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.generation, w.replicas = generation, replicas
	w.armedAt = time.Now()
	w.evaluate(w.armedAt)
}

func (w *canaryWatch) evaluate(at time.Time) {
	if w.err != nil {
		return
	}
	snapshot, _, err := w.snapshot(at)
	if w.completed.IsZero() {
		w.last = snapshot
	}
	if err != nil {
		w.fail(err)
		return
	}
	if w.generation == 0 {
		return
	}
	d := snapshot.Deployment
	// The cache may still hold the pre-write generation during the ACK.
	if d.Generation > w.generation || (d.Generation == w.generation && d.Desired != w.replicas) {
		w.fail(fmt.Errorf("fixture changed during canary phase"))
		return
	}
	if !w.completed.IsZero() || !converged(snapshot, w.generation, w.replicas) {
		return
	}
	if w.replicas == 0 {
		if !w.setsZero() {
			return
		}
	} else {
		pod := snapshot.Pods[0]
		if w.oldPods[pod.UID] || pod.Phase != corev1.PodRunning || !pod.Ready || pod.Deleting {
			return
		}
	}
	w.completed = w.convergenceReceipt(snapshot)
	w.last.ObservedAt = w.completed
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

func (w *canaryWatch) convergenceReceipt(snapshot Snapshot) time.Time {
	// LIST baselines precede arm. Absence may already hold then, but a phase
	// cannot complete before it starts. Never compare RVs across streams.
	at := w.armedAt
	include := func(received time.Time) {
		if received.After(at) {
			at = received
		}
	}
	include(w.deploymentReceipt)
	include(w.removedReceipt)
	for _, pod := range snapshot.Pods {
		include(w.podReceipts[pod.UID])
		for _, owner := range w.pods[pod.UID].OwnerReferences {
			if _, known := w.knownSets[owner.UID]; known {
				include(w.setReceipts[owner.UID])
			}
		}
	}
	if w.replicas == 0 {
		for uid := range w.knownSets {
			include(w.setReceipts[uid])
		}
	}
	// Keep tombstones, including pods deleted before their RS arrived. Their
	// receipts establish absence once that independent stream proves ownership.
	for _, removal := range w.removedPods {
		pod := removal.pod
		for _, owner := range pod.OwnerReferences {
			name, known := w.knownSets[owner.UID]
			if !known {
				continue
			}
			owned, _ := controlledBy(pod.OwnerReferences, "ReplicaSet", name, owner.UID)
			if owned {
				include(removal.received)
				include(w.setReceipts[owner.UID])
			}
		}
	}
	return at
}

func (w *canaryWatch) wait(ctx context.Context) (Snapshot, time.Time, error) {
	for {
		w.mu.Lock()
		snapshot, completed, err := w.last, w.completed, w.err
		w.mu.Unlock()
		if err != nil {
			return snapshot, time.Time{}, err
		}
		if ctx.Err() != nil {
			return snapshot, time.Time{}, ctx.Err()
		}
		if !completed.IsZero() {
			if deadline, ok := ctx.Deadline(); ok && completed.After(deadline) {
				return snapshot, time.Time{}, context.DeadlineExceeded
			}
			return snapshot, completed, nil
		}
		select {
		case <-ctx.Done():
		case <-w.changed:
		}
	}
}

func (w *canaryWatch) setsZero() bool {
	for uid, rs := range w.sets {
		if _, owned := w.knownSets[uid]; owned && ((rs.Spec.Replicas != nil && *rs.Spec.Replicas != 0) || rs.Status.Replicas != 0) {
			return false
		}
	}
	return true
}

func (w *canaryWatch) snapshot(at time.Time) (Snapshot, *appsv1.Deployment, error) {
	d := w.deployment
	s := Snapshot{ObservedAt: at, Deployment: DeploymentStatus{UID: d.UID, Generation: d.Generation, ObservedGeneration: d.Status.ObservedGeneration, Replicas: d.Status.Replicas, Updated: d.Status.UpdatedReplicas, Ready: d.Status.ReadyReplicas, Available: d.Status.AvailableReplicas}}
	if d.Spec.Replicas != nil {
		s.Deployment.Desired = *d.Spec.Replicas
	}
	for _, c := range d.Status.Conditions {
		s.Deployment.Reasons = append(s.Deployment.Reasons, string(c.Type)+"="+string(c.Status)+":"+c.Reason)
	}
	if d.UID != w.uid || d.Spec.Replicas == nil || d.DeletionTimestamp != nil || d.Spec.Paused {
		return s, d, fmt.Errorf("fixture Deployment replaced, deleting, paused, or missing replicas")
	}
	for uid, rs := range w.sets {
		owned, err := controlledBy(rs.OwnerReferences, "Deployment", d.Name, w.uid)
		if err != nil {
			return s, d, err
		}
		if owned {
			if uid == "" {
				return s, d, fmt.Errorf("owned ReplicaSet has no UID")
			}
			w.knownSets[uid] = rs.Name
		} else {
			delete(w.knownSets, uid)
		}
	}
	for _, pod := range w.pods {
		owned := false
		for _, owner := range pod.OwnerReferences {
			name, known := w.knownSets[owner.UID]
			if !known {
				continue
			}
			matches, err := controlledBy(pod.OwnerReferences, "ReplicaSet", name, owner.UID)
			if err != nil {
				return s, d, err
			}
			owned = owned || matches
		}
		if !owned && !(w.conservativeDrain && w.selector.Matches(labels.Set(pod.Labels))) {
			continue
		}
		status := PodStatus{Name: pod.Name, UID: pod.UID, Created: pod.CreationTimestamp.Time, Phase: pod.Status.Phase, Deleting: pod.DeletionTimestamp != nil}
		if pod.Status.Reason != "" {
			status.Reasons = append(status.Reasons, pod.Status.Reason)
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady {
				status.Ready = c.Status == corev1.ConditionTrue
			}
			status.Reasons = append(status.Reasons, string(c.Type)+"="+string(c.Status)+":"+c.Reason)
		}
		for _, containers := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
			for _, container := range containers {
				if container.State.Waiting != nil {
					status.Reasons = append(status.Reasons, container.Name+":"+container.State.Waiting.Reason)
				}
				if container.State.Terminated != nil {
					status.Reasons = append(status.Reasons, container.Name+":"+container.State.Terminated.Reason)
				}
			}
		}
		s.Pods = append(s.Pods, status)
		if (w.generation == 0 || w.replicas != 0) && (pod.UID == "" || !w.selector.Matches(labels.Set(pod.Labels))) {
			return s, d, fmt.Errorf("owned pod has no UID or does not match fixture selector")
		}
	}
	sort.Slice(s.Pods, func(i, j int) bool { return s.Pods[i].Name < s.Pods[j].Name })
	return s, d, nil
}

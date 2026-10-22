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
	"net"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

type turnoverSet struct {
	name string
	hash string
	new  bool
}

type turnoverWork struct {
	record   *TurnoverIntent
	client   kubernetes.Interface
	sets     map[types.UID]turnoverSet // Retain ownership after a ReplicaSet disappears.
	oldPods  map[types.UID]bool
	template *corev1.PodTemplateSpec
	next     time.Time
	retries  int
}

type turnoverEvidenceError struct{ error }

// step performs either one guarded submission or one observation. All client
// calls have individual timeouts; retries are scheduled, never slept inline.
func (w *turnoverWork) step(ctx context.Context, opts TurnoverOptions, end time.Time, emit func(string, *TurnoverIntent, string, error) error) (terminal bool, evidenceErr error) {
	r := w.record
	writeFailed := false
	call := func(operation string, write bool, fn func(context.Context) error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := end.Sub(opts.Now())
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		if write {
			r.Attempts++
			r.AttemptedAt = opts.Now()
		} else {
			r.ReadAttempts++
		}
		if err := emit("attempt", r, operation, nil); err != nil {
			return turnoverEvidenceError{err}
		}
		// Syncing evidence can consume time. Recheck before issuing a request.
		remaining = end.Sub(opts.Now())
		if ctx.Err() != nil || remaining <= 0 {
			return errors.Join(ctx.Err(), context.DeadlineExceeded)
		}
		callCtx, cancel := context.WithTimeout(ctx, min(opts.RequestTimeout, remaining))
		err := fn(callCtx)
		if err == nil {
			err = callCtx.Err()
		}
		cancel()
		if write && err != nil {
			writeFailed = true
		}
		if logErr := emit("response", r, operation, err); logErr != nil {
			return turnoverEvidenceError{errors.Join(err, logErr)}
		}
		return err
	}
	complete, err := func() (bool, error) {
		if w.client == nil {
			client, err := opts.Factory(r.Namespace)
			if err != nil {
				return false, fmt.Errorf("namespace client: %w", err)
			}
			if client == nil {
				return false, fmt.Errorf("namespace client factory returned nil")
			}
			w.client = client
		}
		var deployment *appsv1.Deployment
		if err := call("get deployment", false, func(ctx context.Context) (err error) {
			deployment, err = w.client.AppsV1().Deployments(r.Namespace).Get(ctx, r.Deployment, metav1.GetOptions{})
			return err
		}); err != nil {
			return false, err
		}
		if err := validTurnoverDeployment(deployment, r); err != nil {
			return false, err
		}
		if r.DeploymentUID == "" {
			r.DeploymentUID = deployment.UID
		}
		accepted := !r.AcceptedAt.IsZero()
		if accepted && (deployment.Generation != r.Generation || deployment.Spec.Template.Annotations[RolloutIDAnnotation] != r.RolloutID) {
			return false, fmt.Errorf("deployment changed after acceptance")
		}
		r.Last = Snapshot{ObservedAt: opts.Now(), Deployment: DeploymentStatus{
			UID: deployment.UID, Generation: deployment.Generation, ObservedGeneration: deployment.Status.ObservedGeneration,
			Desired: *deployment.Spec.Replicas, Replicas: deployment.Status.Replicas, Updated: deployment.Status.UpdatedReplicas,
			Ready: deployment.Status.ReadyReplicas, Available: deployment.Status.AvailableReplicas,
		}}
		for _, condition := range deployment.Status.Conditions {
			r.Last.Deployment.Reasons = append(r.Last.Deployment.Reasons, string(condition.Type)+"="+string(condition.Status)+":"+condition.Reason)
		}
		// Namespace-wide lists deliberately include mislabeled owned Pods. Only
		// exact controller UID/name/kind/API chains establish app membership.
		var sets *appsv1.ReplicaSetList
		if err := call("list replicasets", false, func(ctx context.Context) (err error) {
			sets, err = w.client.AppsV1().ReplicaSets(r.Namespace).List(ctx, metav1.ListOptions{})
			return err
		}); err != nil {
			return false, err
		}
		for _, rs := range sets.Items {
			owned, err := controlledBy(rs.OwnerReferences, "Deployment", r.Deployment, r.DeploymentUID)
			if err != nil {
				return false, err
			}
			if !owned {
				delete(w.sets, rs.UID)
				continue
			}
			if rs.UID == "" {
				return false, fmt.Errorf("owned ReplicaSet has no UID")
			}
			current := turnoverSet{name: rs.Name, hash: rs.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey]}
			if accepted && rs.Spec.Template.Annotations[RolloutIDAnnotation] == r.RolloutID {
				template := rs.Spec.Template.DeepCopy()
				delete(template.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
				current.new = current.hash != "" && rs.Labels[appsv1.DefaultDeploymentUniqueLabelKey] == current.hash && equality.Semantic.DeepEqual(template, w.template)
			}
			w.sets[rs.UID] = current
		}
		var pods *corev1.PodList
		if err := call("list pods", false, func(ctx context.Context) (err error) {
			pods, err = w.client.CoreV1().Pods(r.Namespace).List(ctx, metav1.ListOptions{})
			return err
		}); err != nil {
			return false, err
		}
		if w.oldPods == nil {
			w.oldPods = map[types.UID]bool{}
		}
		ready, old := 0, false
		selector, _ := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
		for _, pod := range pods.Items {
			if w.oldPods[pod.UID] {
				old = true // Reparenting an old Pod does not prove its removal.
			}
			owned, fresh := false, false
			for _, ref := range pod.OwnerReferences {
				set, known := w.sets[ref.UID]
				if !known {
					continue
				}
				matches, err := controlledBy(pod.OwnerReferences, "ReplicaSet", set.name, ref.UID)
				if err != nil {
					return false, err
				}
				if matches {
					owned = true
					fresh = set.new && pod.Annotations[RolloutIDAnnotation] == r.RolloutID && pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey] == set.hash
				}
			}
			if !owned {
				continue
			}
			if pod.UID == "" {
				return false, fmt.Errorf("owned Pod has no UID")
			}
			status := PodStatus{Name: pod.Name, UID: pod.UID, Created: pod.CreationTimestamp.Time, Phase: pod.Status.Phase, Deleting: pod.DeletionTimestamp != nil}
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady {
					status.Ready = condition.Status == corev1.ConditionTrue
				}
				status.Reasons = append(status.Reasons, string(condition.Type)+"="+string(condition.Status)+":"+condition.Reason)
			}
			for _, container := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
				if container.State.Waiting != nil {
					status.Reasons = append(status.Reasons, container.Name+":"+container.State.Waiting.Reason)
				}
				if container.State.Terminated != nil {
					status.Reasons = append(status.Reasons, container.Name+":"+container.State.Terminated.Reason)
				}
			}
			r.Last.Pods = append(r.Last.Pods, status)
			if !fresh {
				w.oldPods[pod.UID] = true
				old = true
			}
			if status.Ready && status.Phase == corev1.PodRunning && !status.Deleting && selector.Matches(labels.Set(pod.Labels)) && (!accepted || fresh) {
				ready++
			}
		}
		sort.Slice(r.Last.Pods, func(i, j int) bool { return r.Last.Pods[i].Name < r.Last.Pods[j].Name })
		if accepted {
			return !old && ready == 5 && converged(r.Last, r.Generation, 5), nil
		}
		if ready != 5 || !converged(r.Last, deployment.Generation, 5) {
			return false, fmt.Errorf("seeded Deployment must be converged with five owned Running/Ready Pods")
		}
		updated := deployment.DeepCopy()
		if updated.Spec.Template.Annotations == nil {
			updated.Spec.Template.Annotations = map[string]string{}
		}
		updated.Spec.Template.Annotations[RolloutIDAnnotation] = r.RolloutID
		// Update retains UID and resourceVersion as optimistic concurrency guards.
		// A write is never retried, even if its response is lost or it returns 429.
		if err := call("update deployment", true, func(ctx context.Context) error {
			response, err := w.client.AppsV1().Deployments(r.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
			if response == nil || response.UID != deployment.UID || response.ResourceVersion == "" || response.Generation <= deployment.Generation || response.Spec.Template.Annotations[RolloutIDAnnotation] != r.RolloutID {
				return fmt.Errorf("update response did not confirm intended UID, generation and rollout ID")
			}
			r.Generation, r.AcceptedAt, r.State = response.Generation, opts.Now(), "accepted"
			w.template = response.Spec.Template.DeepCopy()
			delete(w.template.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
			return nil
		}); err != nil {
			return false, err
		}
		if err := emit("accepted", r, "", nil); err != nil {
			return false, turnoverEvidenceError{err}
		}
		return false, nil
	}()
	var logErr turnoverEvidenceError
	if errors.As(err, &logErr) {
		return false, logErr.error
	}
	w.next = opts.Now().Add(opts.PollInterval)
	if err != nil {
		if logErr := emit("error", r, "step", err); logErr != nil {
			return false, errors.Join(err, logErr)
		}
		var networkError net.Error
		transient := apierrors.IsTooManyRequests(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsServiceUnavailable(err) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError)
		if transient && !writeFailed && ctx.Err() == nil && w.retries < 5 {
			delay := time.Second << w.retries
			if seconds, ok := apierrors.SuggestsClientDelay(err); ok {
				delay = max(delay, time.Duration(min(seconds, 30))*time.Second)
			}
			w.retries++
			w.next = opts.Now().Add(min(delay, 30*time.Second))
			return false, nil
		}
		r.State, r.Error, r.FinishedAt = "failed", err.Error(), opts.Now()
		return true, emit("failed", r, "", err)
	}
	w.retries = 0
	if complete {
		r.State, r.CompletedAt, r.FinishedAt = "completed", opts.Now(), opts.Now()
		return true, emit("completed", r, "", nil)
	}
	return false, emit("observed", r, "", nil)
}

func validTurnoverDeployment(d *appsv1.Deployment, r *TurnoverIntent) error {
	if d == nil || d.Name != r.Deployment || d.Namespace != r.Namespace || d.UID == "" || (r.DeploymentUID != "" && d.UID != r.DeploymentUID) || d.ResourceVersion == "" || d.Generation <= 0 || d.DeletionTimestamp != nil || d.Spec.Paused || d.Spec.Replicas == nil || *d.Spec.Replicas != 5 {
		return fmt.Errorf("require original live, unpaused, versioned five-replica Deployment")
	}
	rolling := d.Spec.Strategy.RollingUpdate
	if d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || rolling == nil || rolling.MaxSurge == nil || rolling.MaxUnavailable == nil || *rolling.MaxSurge != intstr.FromInt32(1) || *rolling.MaxUnavailable != intstr.FromInt32(0) {
		return fmt.Errorf("require RollingUpdate maxSurge=1 maxUnavailable=0")
	}
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil || selector.Empty() || !selector.Matches(labels.Set(d.Spec.Template.Labels)) {
		return fmt.Errorf("require nonempty valid Deployment selector matching its template")
	}
	return nil
}

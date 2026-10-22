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

// Package workload provides guest Kubernetes workload primitives, not provisioning.
package workload

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
)

type Spec struct {
	Namespace        string
	Name             string                     // Name of an existing Deployment; no fixture is created or repaired.
	Deadlines        experiment.CanaryDeadlines // Hard bounds: required, positive, Ack <= Ready, capped at 30s/120s/120s.
	Expectations     experiment.CanaryDeadlines // Soft targets: independently optional (zero), nonnegative and <= respective hard bound.
	PreflightTimeout time.Duration              // Zero selects 30s; otherwise must be positive and at most 30s, independent of fitted write Ack.
}

// Stage records elapsed time even on failure. Completed is zero unless successful.
// Attempts counts UpdateScale calls, not retries internal to the supplied client.
type Stage struct {
	Started             time.Time     `json:"started"`
	Completed           time.Time     `json:"completed"`
	Duration            time.Duration `json:"durationNanoseconds"`
	Attempts            int           `json:"attempts"`
	ExpectedDuration    time.Duration `json:"expectedDurationNanoseconds,omitempty"`
	Deadline            time.Duration `json:"deadlineNanoseconds,omitempty"`
	ExpectationExceeded bool          `json:"expectationExceeded"` // May also be true for elapsed time on an incomplete stage.
}

type Result struct {
	// Performance assesses only completed stages with explicit expectations,
	// independently of errors. With none evaluated it is not_evaluated.
	Performance     string            `json:"performance"` // within_expectations, degraded, or not_evaluated.
	Degraded        bool              `json:"degraded"`
	MeasurementMode string            `json:"measurementMode,omitempty"`
	Watch           *WatchDiagnostics `json:"watch,omitempty"`
	Preflight       Stage             `json:"preflight"`
	Ack             Stage             `json:"ack"`
	Ready           Stage             `json:"ready"`
	Cleanup         Stage             `json:"cleanup"`
	DeploymentUID   types.UID         `json:"deploymentUID"`
	Generation      int64             `json:"generation"`
	PodUID          types.UID         `json:"podUID"`
	// Last is retained before cleanup, so a pending/failing pod is not lost.
	Last        Snapshot `json:"last"`
	CleanupLast Snapshot `json:"cleanupLast"`
}

// Snapshot deliberately excludes templates, environment, annotations, messages,
// and full API objects. Reasons include scheduling and container failure codes.
type Snapshot struct {
	ObservedAt time.Time        `json:"observedAt"`
	Deployment DeploymentStatus `json:"deployment"`
	Pods       []PodStatus      `json:"pods"`
}

type DeploymentStatus struct {
	UID                types.UID `json:"uid"`
	Generation         int64     `json:"generation"`
	ObservedGeneration int64     `json:"observedGeneration"`
	Desired            int32     `json:"desired"`
	Replicas           int32     `json:"replicas"`
	Updated            int32     `json:"updated"`
	Ready              int32     `json:"ready"`
	Available          int32     `json:"available"`
	Reasons            []string  `json:"reasons"`
}

type PodStatus struct {
	Name     string          `json:"name"`
	UID      types.UID       `json:"uid"`
	Created  time.Time       `json:"created"` // API timestamp for diagnostics only; not compared to the client clock.
	Phase    corev1.PodPhase `json:"phase"`
	Ready    bool            `json:"ready"`
	Deleting bool            `json:"deleting"`
	Reasons  []string        `json:"reasons"`
}

// RunCanary performs one serial 0 -> 1 -> 0 cycle using only the supplied client
// identity. Callers must prevent concurrent writers/cycles on this Deployment.
// Preflight has its own <=30s budget, independent of fitted write Ack, and is
// measured separately from Ack/Ready/Cleanup. Invalid fixtures are not mutated.
// Once scale-up is attempted, cleanup always runs, including
// after an ambiguous write error or caller cancellation, with its own <=120s bound.
// Writes are attempted once (no application retries); all errors are returned,
// joining cleanup errors rather than hiding the original failure. Only a nil
// error permits treating Ack/Ready/Cleanup durations as a successful CanarySample.
// Expectations only assess performance; crossing one never cancels a phase or
// returns an error. Deadlines remain the hard bounds on actual completion timing.
// The supplied REST client must have Timeout=0: per-request contexts bound writes
// and LISTs, while WATCH bodies must remain readable for the entire phase.
func RunCanary(ctx context.Context, client kubernetes.Interface, spec Spec) (result Result, err error) {
	// Registered first so all timing, watch validation, and cleanup finish before assessment.
	defer func() { result.finalizePerformance() }()
	if client == nil || len(validation.IsDNS1123Label(spec.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(spec.Name)) != 0 {
		return result, fmt.Errorf("client and valid namespace/deployment name are required")
	}
	d := spec.Deadlines
	if d.Ack <= 0 || d.Ack > 30*time.Second || d.Ready < d.Ack || d.Ready > 120*time.Second || d.Cleanup <= 0 || d.Cleanup > 120*time.Second {
		return result, fmt.Errorf("require positive ack <= ready and ack/ready/cleanup within 30s/120s/120s")
	}
	if spec.PreflightTimeout == 0 {
		spec.PreflightTimeout = 30 * time.Second
	}
	if spec.PreflightTimeout < 0 || spec.PreflightTimeout > 30*time.Second {
		return result, fmt.Errorf("preflight timeout must be positive and at most 30s")
	}
	e := spec.Expectations
	if e.Ack < 0 || e.Ack > d.Ack || e.Ready < 0 || e.Ready > d.Ready || e.Cleanup < 0 || e.Cleanup > d.Cleanup {
		return result, fmt.Errorf("expectations must be nonnegative and at most their respective hard deadlines")
	}
	result.Ack.ExpectedDuration, result.Ack.Deadline = e.Ack, d.Ack
	result.Ready.ExpectedDuration, result.Ready.Deadline = e.Ready, d.Ready
	result.Cleanup.ExpectedDuration, result.Cleanup.Deadline = e.Cleanup, d.Cleanup
	result.Preflight.Started = time.Now()
	result.MeasurementMode = "watch"
	result.Watch = &WatchDiagnostics{}
	defer func() {
		if result.Preflight.Completed.IsZero() {
			result.Preflight.Duration = time.Since(result.Preflight.Started)
		}
	}()
	preflight, cancel := context.WithTimeout(ctx, spec.PreflightTimeout)
	defer cancel()
	deployments := client.AppsV1().Deployments(spec.Namespace)
	observation, err := newCanaryWatch(preflight, client, spec, nil)
	if err != nil {
		return result, fmt.Errorf("preflight: %w", err)
	}
	defer func() { observation.stop(); observation.diagnostics(result.Watch) }()
	initial := observation.deployment.DeepCopy()
	selector, err := metav1.LabelSelectorAsSelector(initial.Spec.Selector)
	if err != nil || selector.Empty() || !selector.Matches(labels.Set(initial.Spec.Template.Labels)) {
		return result, fmt.Errorf("fixture requires a nonempty valid selector matching its template")
	}
	if initial.UID == "" || initial.ResourceVersion == "" || initial.Generation <= 0 || initial.Spec.Replicas == nil || initial.DeletionTimestamp != nil || initial.Spec.Paused {
		return result, fmt.Errorf("fixture must be a live, unpaused, versioned Deployment with explicit replicas")
	}
	result.DeploymentUID = initial.UID
	observation.uid, observation.selector = initial.UID, selector
	result.Last, _, err = observation.snapshot(time.Now())
	if err != nil {
		return result, fmt.Errorf("preflight: %w", err)
	}
	if !converged(result.Last, initial.Generation, 0) || !observation.setsZero() {
		return result, fmt.Errorf("fixture must be unchanged, converged at replicas=0, with no owned pods")
	}
	if err := preflight.Err(); err != nil {
		return result, err
	}
	cancel()
	result.Preflight.Completed = time.Now()
	result.Preflight.Duration = result.Preflight.Completed.Sub(result.Preflight.Started)
	observation.start()

	defer func() {
		observation.stop()
		result.Last = observation.last
		if observation.err != nil {
			err = errors.Join(err, observation.err)
			result.Ready.Completed = time.Time{}
			result.Ready.Duration = time.Since(result.Ready.Started)
			result.PodUID = ""
		}
		result.Cleanup.Started = time.Now()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.Cleanup)
		defer cancel()
		cleanupErr := func() error {
			// A failed measured stream is never resynced into a successful timing.
			// Cleanup has independent LIST/WATCH observation and its own budget.
			// Reserve time for the guarded scale-down even if observation setup
			// hangs. Setup overhead remains part of the cleanup measurement.
			setupCtx, setupCancel := context.WithTimeout(cleanupCtx, min(30*time.Second, d.Cleanup/2))
			cleanup, observeErr := newCanaryWatch(setupCtx, client, spec, observation.knownSets)
			setupCancel()
			if cleanup != nil {
				defer func() { cleanup.stop(); cleanup.diagnostics(result.Watch) }()
				cleanup.uid, cleanup.selector = initial.UID, selector
			}
			current, err := deployments.Get(cleanupCtx, spec.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if current.UID != initial.UID || current.ResourceVersion == "" || current.Spec.Replicas == nil {
				return fmt.Errorf("refusing cleanup of replaced or invalid Deployment")
			}
			generation := current.Generation
			if *current.Spec.Replicas != 0 {
				generation++
			}
			if cleanup != nil {
				cleanup.arm(generation, 0)
				cleanup.start()
			}
			result.Cleanup.Attempts++
			_, err = deployments.UpdateScale(cleanupCtx, spec.Name, scale(current, 0), metav1.UpdateOptions{})
			if err != nil || observeErr != nil {
				return errors.Join(err, observeErr)
			}
			result.CleanupLast, result.Cleanup.Completed, err = cleanup.wait(cleanupCtx)
			cleanup.stop()
			result.CleanupLast = cleanup.last
			return errors.Join(err, cleanup.err)
		}()
		result.Cleanup.Duration = time.Since(result.Cleanup.Started)
		if cleanupErr != nil {
			result.Cleanup.Completed = time.Time{}
			err = errors.Join(err, fmt.Errorf("cleanup: %w", cleanupErr))
		} else {
			result.Cleanup.Duration = result.Cleanup.Completed.Sub(result.Cleanup.Started)
		}
	}()

	result.Ready.Started = time.Now()
	result.Ack.Started = result.Ready.Started
	// Scale responses omit Deployment generation. A resourceVersion-guarded
	// 0 -> 1 spec update increments the initial generation exactly once.
	result.Generation = initial.Generation + 1
	observation.arm(result.Generation, 1)
	readyCtx, readyCancel := context.WithDeadline(ctx, result.Ready.Started.Add(d.Ready))
	defer readyCancel()
	defer func() {
		if result.Ready.Completed.IsZero() {
			result.Ready.Duration = time.Since(result.Ready.Started)
		}
	}()
	ackCtx, ackCancel := context.WithDeadline(readyCtx, result.Ack.Started.Add(d.Ack))
	observation.mu.Lock()
	observation.abort = ackCancel
	if observation.err != nil {
		ackCancel()
	}
	observation.mu.Unlock()
	result.Ack.Attempts++
	_, err = deployments.UpdateScale(ackCtx, spec.Name, scale(initial, 1), metav1.UpdateOptions{})
	ackCompleted := time.Now()
	result.Ack.Duration = ackCompleted.Sub(result.Ack.Started)
	err = errors.Join(err, ackCtx.Err())
	observation.mu.Lock()
	err = errors.Join(err, observation.err)
	observation.abort = nil
	observation.mu.Unlock()
	ackCancel()
	if err != nil {
		return result, fmt.Errorf("ack: %w", err)
	}
	result.Ack.Completed = ackCompleted
	result.Last, result.Ready.Completed, err = observation.wait(readyCtx)
	if err != nil {
		return result, fmt.Errorf("ready: %w", err)
	}
	result.Ready.Duration = result.Ready.Completed.Sub(result.Ready.Started)
	result.PodUID = result.Last.Pods[0].UID
	return result, nil
}

func (r *Result) finalizePerformance() {
	r.Performance, r.Degraded = "not_evaluated", false
	for _, stage := range []*Stage{&r.Ack, &r.Ready, &r.Cleanup} {
		stage.ExpectationExceeded = stage.ExpectedDuration > 0 && stage.Duration > stage.ExpectedDuration
		if stage.ExpectedDuration == 0 || stage.Completed.IsZero() {
			continue
		}
		r.Degraded = r.Degraded || stage.ExpectationExceeded
		r.Performance = "within_expectations"
	}
	if r.Degraded {
		r.Performance = "degraded"
	}
}

func scale(deployment *appsv1.Deployment, replicas int32) *autoscalingv1.Scale {
	return &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: deployment.Name, Namespace: deployment.Namespace, UID: deployment.UID, ResourceVersion: deployment.ResourceVersion},
		Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
	}
}

func converged(snapshot Snapshot, generation int64, replicas int32) bool {
	d := snapshot.Deployment
	return d.Generation >= generation && d.ObservedGeneration >= generation && d.Desired == replicas &&
		d.Replicas == replicas && d.Updated == replicas && d.Ready == replicas && d.Available == replicas && len(snapshot.Pods) == int(replicas)
}

func controlledBy(owners []metav1.OwnerReference, kind, name string, uid types.UID) (bool, error) {
	controllers, matches := 0, false
	for _, owner := range owners {
		if owner.Controller != nil && *owner.Controller {
			controllers++
			matches = matches || (owner.APIVersion == "apps/v1" && owner.Kind == kind && owner.Name == name && owner.UID == uid)
		}
	}
	if matches && controllers != 1 {
		return false, fmt.Errorf("ambiguous controller ownership for %s %s", kind, name)
	}
	return matches, nil
}

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
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

type Availability struct {
	Samples       int        `json:"samples"`
	Minimum       int        `json:"minimum"`
	Maximum       int        `json:"maximum"`
	BelowTwoSince *time.Time `json:"belowTwoSince,omitempty"`
	OutageSeconds float64    `json:"outageSeconds"`
}

type StabilitySummary struct {
	Samples          int                     `json:"samples"`
	UnhealthySamples int                     `json:"unhealthySamples"`
	Restarts         int32                   `json:"restarts"`
	OOMs             int                     `json:"ooms"`
	ProbeFailures    int                     `json:"probeFailures"`
	Critical         map[string]Availability `json:"critical"`
	LastObserved     time.Time               `json:"lastObserved"`
}

func (s *StabilitySummary) availability(name string, ready int, now time.Time) error {
	if s.Critical == nil {
		s.Critical = map[string]Availability{}
	}
	a := s.Critical[name]
	if a.Samples == 0 {
		a.Minimum = ready
	}
	a.Samples++
	a.Minimum, a.Maximum = min(a.Minimum, ready), max(a.Maximum, ready)
	if ready < 2 && a.BelowTwoSince == nil {
		at := now
		a.BelowTwoSince = &at
	}
	var err error
	if a.BelowTwoSince != nil {
		elapsed := now.Sub(*a.BelowTwoSince)
		if elapsed >= 30*time.Second {
			err = errors.New("critical endpoint availability below two for 30s")
		}
		if ready >= 2 {
			a.OutageSeconds += elapsed.Seconds()
			a.BelowTwoSince = nil
		}
	}
	s.Critical[name] = a
	return err
}

// The slower profile loop observes ancillary services and node safety only.
// Critical KAS/etcd/livez checks run independently of this loop and capture.
func (g *safety) ancillarySafety(ctx context.Context, clients startup.Clients, spec Spec, namespace string, pods []corev1.Pod, now time.Time) error {
	for _, name := range []string{"openshift-apiserver", "packageserver"} {
		ready, err := endpointReady(ctx, clients.Kubernetes, spec, namespace, name)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("ancillary availability inspection %s incomplete: %w", name, err)
		}
		_ = g.summary.availability(name, ready, now)
	}
	// Observe node safety, without altering scheduling, resources or probes.
	nodes := map[string]bool{}
	for _, pod := range pods {
		if pod.Spec.NodeName != "" {
			nodes[pod.Spec.NodeName] = true
		}
	}
	nodeCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	names := make(chan string, len(nodes))
	for name := range nodes {
		names <- name
	}
	close(names)
	var checks sync.WaitGroup
	for range min(4, len(nodes)) {
		checks.Add(1)
		go func() {
			defer utilruntime.HandleCrash()
			defer checks.Done()
			for name := range names {
				if nodeCtx.Err() != nil {
					return
				}
				node, err := clients.Kubernetes.CoreV1().Nodes().Get(nodeCtx, name, metav1.GetOptions{})
				if err != nil {
					cancel(fmt.Errorf("control-plane node safety %s unavailable: %w", name, err))
					return
				}
				ready := false
				for _, condition := range node.Status.Conditions {
					if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
						ready = true
					}
					if (condition.Type == corev1.NodeMemoryPressure || condition.Type == corev1.NodeDiskPressure || condition.Type == corev1.NodePIDPressure) && condition.Status == corev1.ConditionTrue {
						cancel(fmt.Errorf("unsafe control-plane node pressure: %s", name))
						return
					}
				}
				if !ready {
					cancel(fmt.Errorf("control-plane node not Ready: %s", name))
					return
				}
			}
		}()
	}
	checks.Wait()
	return context.Cause(nodeCtx)
}

// Only distinct Ready Pod UIDs whose live controller chain matches the named
// owner count. Etcd endpoint readiness remains a quorum proxy, not proof.
func endpointReady(ctx context.Context, client kubernetes.Interface, spec Spec, namespace, name string) (int, error) {
	kind := "Deployment"
	var ownerUID types.UID
	if name == "etcd" {
		kind = "StatefulSet"
		owner, err := client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		if owner.DeletionTimestamp == nil {
			ownerUID = owner.UID
		}
	} else {
		owner, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		if owner.DeletionTimestamp == nil {
			ownerUID = owner.UID
		}
	}
	endpointName := ""
	for _, component := range spec.Startup.Components {
		if component.Kind == kind && component.Name == name {
			endpointName = component.Endpoints
		}
	}
	endpoints, err := client.CoreV1().Endpoints(namespace).Get(ctx, endpointName, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	if ownerUID == "" {
		return 0, nil
	}
	parents := map[types.UID]types.UID{}
	if kind == "Deployment" {
		sets, err := client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return 0, err
		}
		for _, set := range sets.Items {
			owner := metav1.GetControllerOf(&set)
			if owner != nil && owner.Kind == kind && owner.Name == name && owner.UID == ownerUID && set.DeletionTimestamp == nil {
				parents[set.UID] = owner.UID
			}
		}
	}
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	readyPods := map[types.UID]string{}
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || pod.UID == "" || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		owned := owner.Kind == kind && owner.Name == name && owner.UID == ownerUID
		if kind == "Deployment" && owner.Kind == "ReplicaSet" {
			owned = parents[owner.UID] == ownerUID
		}
		if owned {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					readyPods[pod.UID] = pod.Name
				}
			}
		}
	}
	seen := map[types.UID]bool{}
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			ref := address.TargetRef
			if ref != nil && ref.Kind == "Pod" && ref.Namespace == namespace && ref.UID != "" && ref.Name != "" && readyPods[ref.UID] == ref.Name {
				seen[ref.UID] = true
			}
		}
	}
	return len(seen), nil
}

// Only deadline leaves are recoverable. A joined permission/identity/watch error
// must not become recoverable merely because another leaf is a deadline.
func canaryDeadline(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !canaryDeadline(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return canaryDeadline(wrapped.Unwrap())
	}
	return err == context.DeadlineExceeded || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err)
}

func recoverCanary(ctx context.Context, client kubernetes.Interface, spec workload.Spec, result workload.Result, err error) (workload.Snapshot, error) {
	if ctx.Err() != nil || !canaryDeadline(err) || result.DeploymentUID == "" || result.Cleanup.Attempts == 0 {
		return workload.Snapshot{}, errors.New("canary failure is not a recoverable measured deadline")
	}
	return workload.WaitCanaryDrained(ctx, client, spec, result.DeploymentUID)
}

func turnoverOnlyFailure(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !turnoverOnlyFailure(child) {
				return false
			}
		}
		return true
	}
	_, ok := err.(*workload.TurnoverFailure)
	return ok
}

func turnoverDrainFailure(result workload.TurnoverResult, err error) bool {
	if !turnoverOnlyFailure(err) {
		return false
	}
	for _, intent := range result.Intents {
		if intent.State == "failed" && intent.Error != "run stopped before completion" {
			return false // Includes authorization, ownership and unsafe write errors.
		}
	}
	return result.Failed > 0
}

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

package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

type Phase string

const (
	PhasePending  Phase = "pending"
	PhaseReady    Phase = "ready"
	PhaseStopping Phase = "stopping"
	PhaseStopped  Phase = "stopped"
	PhaseFailed   Phase = "failed"
	MaxEvents           = 20
)

// SessionStatus is a non-atomic Kubernetes observation, not a scrape-coverage
// report. ServiceURL is an in-cluster address, not a connectivity guarantee.
// Inventory contains only observed or successfully created objects. Missing
// lists expected resources absent after a partial start or external deletion.
// Diagnostic messages and configuration are deliberately excluded.
type SessionStatus struct {
	Version             string              `json:"version"`
	ID                  string              `json:"id"`
	Namespace           string              `json:"namespace"`
	Phase               Phase               `json:"phase"`
	DesiredReplicas     int32               `json:"desiredReplicas"`
	ReadyReplicas       int32               `json:"readyReplicas"`
	ServiceURL          string              `json:"serviceURL,omitempty"`
	Inventory           []ResourceReference `json:"inventory"`
	Missing             []string            `json:"missing,omitempty"`
	Conditions          []Condition         `json:"conditions,omitempty"`
	Pods                []PodStatus         `json:"pods,omitempty"`
	Storage             []StorageStatus     `json:"storage,omitempty"`
	Events              []Event             `json:"events,omitempty"`
	TLSCredentialPolicy string              `json:"tlsCredentialPolicy,omitempty"`
}

type ResourceReference struct {
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Namespace string    `json:"namespace,omitempty"`
	UID       types.UID `json:"uid"`
}

type Condition struct {
	Type   string                 `json:"type"`
	Status corev1.ConditionStatus `json:"status"`
	Reason string                 `json:"reason,omitempty"`
}

type ContainerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	Restarts     int32  `json:"restarts"`
	Reason       string `json:"reason,omitempty"`
	ExitCode     *int32 `json:"exitCode,omitempty"`
	LastReason   string `json:"lastReason,omitempty"`
	LastExitCode *int32 `json:"lastExitCode,omitempty"`
}

type PodStatus struct {
	Name        string            `json:"name"`
	Phase       corev1.PodPhase   `json:"phase"`
	Node        string            `json:"node,omitempty"`
	Terminating bool              `json:"terminating"`
	Conditions  []Condition       `json:"conditions,omitempty"`
	Containers  []ContainerStatus `json:"containers,omitempty"`
}

type StorageStatus struct {
	Name         string                            `json:"name"`
	Phase        corev1.PersistentVolumeClaimPhase `json:"phase"`
	StorageClass string                            `json:"storageClass"`
	Requested    string                            `json:"requested"`
	Capacity     string                            `json:"capacity"`
	VolumeName   string                            `json:"volumeName,omitempty"`
	Conditions   []Condition                       `json:"conditions,omitempty"`
}

// Event contains only structured event metadata, newest first (at most 20).
// Free-form event messages are not safe to include in exported status.
type Event struct {
	Kind   string      `json:"kind"`
	Name   string      `json:"name"`
	Type   string      `json:"type"`
	Reason string      `json:"reason"`
	Count  int32       `json:"count"`
	Time   metav1.Time `json:"time"`
}

func reference(kind string, object metav1.Object) ResourceReference {
	return ResourceReference{Kind: kind, Name: object.GetName(), Namespace: object.GetNamespace(), UID: object.GetUID()}
}

func serviceURL(namespace string) string {
	return "http://prometheus." + namespace + ".svc:9090"
}

func storageStatus(pvc *corev1.PersistentVolumeClaim) StorageStatus {
	s := StorageStatus{Name: pvc.Name, Phase: pvc.Status.Phase, VolumeName: pvc.Spec.VolumeName,
		Requested: pvc.Spec.Resources.Requests.Storage().String(), Capacity: pvc.Status.Capacity.Storage().String()}
	if pvc.Spec.StorageClassName != nil {
		s.StorageClass = *pvc.Spec.StorageClassName
	}
	for _, c := range pvc.Status.Conditions {
		s.Conditions = append(s.Conditions, Condition{Type: string(c.Type), Status: c.Status, Reason: c.Reason})
	}
	return s
}

// Status verifies namespace ownership before reading any session resources.
// Missing resources produce failed status without an API error. Read failures
// return partial status plus an error; callers must not treat it as complete.
func Status(ctx context.Context, client kubernetes.Interface, id string) (SessionStatus, error) {
	s := SessionStatus{Version: "v1", ID: id, Namespace: "hcp-perf-" + id, Phase: PhasePending}
	ns, err := namespaceFor(ctx, client, id)
	if err != nil {
		return s, err
	}
	s.Inventory = append(s.Inventory, reference("Namespace", ns))
	add := func(kind string, object metav1.Object, err error, names ...string) error {
		name := resourceName
		if len(names) > 0 {
			name = names[0]
		}
		if kind == "ClusterRole" || kind == "ClusterRoleBinding" {
			name = s.Namespace + "-prometheus"
		}
		if apierrors.IsNotFound(err) {
			s.Missing = append(s.Missing, kind+"/"+name)
			return nil
		}
		if err != nil {
			return operationError("get "+kind, err)
		}
		if !owned(object, id) || object.GetLabels()[ComponentLabel] != "capture" {
			return fmt.Errorf("%s is not owned by this capture session", kind)
		}
		if kind == "ClusterRole" || kind == "ClusterRoleBinding" {
			owner := metav1.GetControllerOf(object)
			if owner == nil || owner.APIVersion != "v1" || owner.Kind != "Namespace" || owner.Name != ns.Name || owner.UID != ns.UID || ns.UID == "" {
				return fmt.Errorf("%s is not owned by this namespace UID", kind)
			}
		}
		s.Inventory = append(s.Inventory, reference(kind, object))
		return nil
	}
	sa, err := client.CoreV1().ServiceAccounts(s.Namespace).Get(ctx, resourceName, metav1.GetOptions{})
	if err := add("ServiceAccount", sa, err); err != nil {
		return s, err
	}
	cm, err := client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, resourceName, metav1.GetOptions{})
	if err := add("ConfigMap", cm, err); err != nil {
		return s, err
	}
	if cm != nil && err == nil {
		var persisted Spec
		if err := json.Unmarshal([]byte(cm.Data["spec.json"]), &persisted); err != nil {
			return s, errors.New("invalid persisted capture spec")
		}
		mounts, err := validateTLSMounts(s.Namespace, persisted.TLSMounts)
		if err != nil {
			return s, errors.New("invalid persisted TLS mount requirements")
		}
		if len(mounts) > 0 {
			s.TLSCredentialPolicy = tlsCredentialPolicy
		}
		for _, mount := range mounts {
			if mount.Source.Kind == "Secret" {
				object, err := client.CoreV1().Secrets(s.Namespace).Get(ctx, mount.DestinationName, metav1.GetOptions{})
				if err := add("Secret", object, err, mount.DestinationName); err != nil {
					return s, err
				}
			} else {
				object, err := client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, mount.DestinationName, metav1.GetOptions{})
				if err := add("ConfigMap", object, err, mount.DestinationName); err != nil {
					return s, err
				}
			}
		}
	}
	// The marker is created with the namespace, so partial RBAC creation remains
	// discoverable even if Start was interrupted. Disabled sessions need no RBAC reads.
	if ns.Annotations[discoveryAnnotation] == "true" {
		role, err := client.RbacV1().ClusterRoles().Get(ctx, s.Namespace+"-prometheus", metav1.GetOptions{})
		if err := add("ClusterRole", role, err); err != nil {
			return s, err
		}
		binding, err := client.RbacV1().ClusterRoleBindings().Get(ctx, s.Namespace+"-prometheus", metav1.GetOptions{})
		if err := add("ClusterRoleBinding", binding, err); err != nil {
			return s, err
		}
	}
	svc, err := client.CoreV1().Services(s.Namespace).Get(ctx, resourceName, metav1.GetOptions{})
	if err := add("Service", svc, err); err != nil {
		return s, err
	}
	if err == nil {
		s.ServiceURL = serviceURL(s.Namespace)
	}
	sts, err := client.AppsV1().StatefulSets(s.Namespace).Get(ctx, resourceName, metav1.GetOptions{})
	hasStatefulSet := err == nil
	if err := add("StatefulSet", sts, err); err != nil {
		return s, err
	}
	if hasStatefulSet {
		s.DesiredReplicas = 1 // Kubernetes defaults omitted replicas to one.
		if sts.Spec.Replicas != nil {
			s.DesiredReplicas = *sts.Spec.Replicas
		}
		s.ReadyReplicas = sts.Status.ReadyReplicas
		for _, c := range sts.Status.Conditions {
			s.Conditions = append(s.Conditions, Condition{Type: string(c.Type), Status: c.Status, Reason: c.Reason})
		}
	}
	selector := metav1.ListOptions{LabelSelector: klabels.SelectorFromSet(labels(id)).String()}
	pvcs, err := client.CoreV1().PersistentVolumeClaims(s.Namespace).List(ctx, selector)
	if err != nil {
		return s, operationError("list PVCs", err)
	}
	storageFound, storageBound, failed := false, false, false
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if !owned(pvc, id) {
			return s, errors.New("PVC is not owned by this capture session")
		}
		s.Inventory = append(s.Inventory, reference("PersistentVolumeClaim", pvc))
		s.Storage = append(s.Storage, storageStatus(pvc))
		if pvc.Name == resourceName {
			storageFound = true
			storageBound = pvc.Status.Phase == corev1.ClaimBound && pvc.DeletionTimestamp == nil
		}
		failed = failed || pvc.Status.Phase == corev1.ClaimLost
	}
	if !storageFound {
		s.Missing = append(s.Missing, "PersistentVolumeClaim/"+resourceName)
	}
	// Labels can drift while a collector is still running. Inspect all pods,
	// but only consider the expected collector name or current controller UID.
	pods, err := client.CoreV1().Pods(s.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return s, operationError("list pods", err)
	}
	active, ready := 0, 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		controllerMatches := hasStatefulSet && sts.UID != "" && owner != nil && owner.UID == sts.UID
		if pod.Name != resourceName+"-0" && !controllerMatches {
			continue
		}
		s.Inventory = append(s.Inventory, reference("Pod", pod))
		if !controllerMatches || owner.APIVersion != "apps/v1" || owner.Kind != "StatefulSet" || owner.Name != sts.Name || !owned(pod, id) || pod.Labels[ComponentLabel] != "capture" {
			s.Phase = PhaseFailed
			return s, errors.New("capture pod ownership mismatch; cannot determine readiness or stopped state")
		}
		p := PodStatus{Name: pod.Name, Phase: pod.Status.Phase, Node: pod.Spec.NodeName, Terminating: pod.DeletionTimestamp != nil}
		terminal := pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
		if !terminal {
			active++
		}
		for _, c := range pod.Status.Conditions {
			p.Conditions = append(p.Conditions, Condition{Type: string(c.Type), Status: c.Status, Reason: c.Reason})
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue && !p.Terminating && pod.Status.Phase == corev1.PodRunning {
				ready++
			}
		}
		for _, c := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
			container := ContainerStatus{Name: c.Name, Ready: c.Ready, Restarts: c.RestartCount}
			if c.State.Waiting != nil {
				container.Reason = c.State.Waiting.Reason
				switch container.Reason {
				case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "InvalidImageName":
					failed = true
				}
			}
			if c.State.Terminated != nil {
				container.Reason, container.ExitCode = c.State.Terminated.Reason, ptr(c.State.Terminated.ExitCode)
				failed = failed || c.State.Terminated.ExitCode != 0
			}
			if c.LastTerminationState.Terminated != nil {
				container.LastReason = c.LastTerminationState.Terminated.Reason
				container.LastExitCode = ptr(c.LastTerminationState.Terminated.ExitCode)
			}
			p.Containers = append(p.Containers, container)
		}
		failed = failed || pod.Status.Phase == corev1.PodFailed
		s.Pods = append(s.Pods, p)
	}
	// Events are paginated, retaining only the newest bounded window in memory.
	options := metav1.ListOptions{Limit: 100}
	for {
		events, err := client.CoreV1().Events(s.Namespace).List(ctx, options)
		if err != nil {
			return s, operationError("list events", err)
		}
		for _, e := range events.Items {
			timestamp, count := e.LastTimestamp, e.Count
			if timestamp.IsZero() {
				timestamp = metav1.NewTime(e.EventTime.Time)
			}
			if timestamp.IsZero() {
				timestamp = e.CreationTimestamp
			}
			if e.Series != nil {
				timestamp, count = metav1.NewTime(e.Series.LastObservedTime.Time), e.Series.Count
			}
			s.Events = append(s.Events, Event{Kind: e.InvolvedObject.Kind, Name: e.InvolvedObject.Name, Type: e.Type, Reason: e.Reason, Count: count, Time: timestamp})
		}
		sort.SliceStable(s.Events, func(i, j int) bool { return s.Events[i].Time.After(s.Events[j].Time.Time) })
		if len(s.Events) > MaxEvents {
			s.Events = s.Events[:MaxEvents]
		}
		if events.Continue == "" {
			break
		}
		options.Continue = events.Continue
	}
	switch {
	case ns.DeletionTimestamp != nil:
		s.Phase = PhaseStopping
	case hasStatefulSet && s.DesiredReplicas == 0:
		s.Phase = PhaseStopping
		if active == 0 {
			s.Phase = PhaseStopped
		}
	case failed || len(s.Missing) > 0:
		s.Phase = PhaseFailed
	case hasStatefulSet && sts.DeletionTimestamp == nil && s.DesiredReplicas == 1 && s.ReadyReplicas == 1 && active == 1 && ready == 1 && storageBound && sts.Status.ObservedGeneration >= sts.Generation:
		s.Phase = PhaseReady
	}
	return s, nil
}

// WaitReady polls server readiness, not target health, until ctx is done. It
// tolerates pending/failed observations (e.g. transient image pull failures),
// but returns API errors immediately and rejects stopped/stopping sessions.
func WaitReady(ctx context.Context, client kubernetes.Interface, id string) (SessionStatus, error) {
	return waitFor(ctx, client, id, PhaseReady)
}

// WaitStopped polls until desired replicas are zero and no nonterminal capture
// pods controlled by the current StatefulSet remain. Adversaries are excluded.
// It does not issue Stop itself. Supply a context deadline.
func WaitStopped(ctx context.Context, client kubernetes.Interface, id string) (SessionStatus, error) {
	return waitFor(ctx, client, id, PhaseStopped)
}

func waitFor(ctx context.Context, client kubernetes.Interface, id string, phase Phase) (SessionStatus, error) {
	var last SessionStatus
	for {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		s, err := Status(ctx, client, id)
		last = s
		if err != nil || s.Phase == phase {
			return s, err
		}
		if phase == PhaseReady && (s.Phase == PhaseStopped || s.Phase == PhaseStopping) {
			return s, errors.New("capture session is stopping or stopped")
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, ctx.Err()
		case <-timer.C:
		}
	}
}

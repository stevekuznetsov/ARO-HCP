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

package record

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// These are allowlists, not redacted copies of API objects. In particular, pod
// templates, owner metadata, managedFields and newly added API fields cannot
// accidentally reach disk. Commands are omitted as well as args: a shell command
// may itself contain inline credentials. Reason codes, names and images remain
// caller-controlled fields and must not be treated as a secret-proof channel.
type metadata struct {
	UID               types.UID    `json:"uid"`
	Name              string       `json:"name"`
	Namespace         string       `json:"namespace"`
	Generation        int64        `json:"generation"`
	CreationTimestamp metav1.Time  `json:"creationTimestamp"`
	DeletionTimestamp *metav1.Time `json:"deletionTimestamp,omitempty"`
}

func objectMetadata(m metav1.ObjectMeta) metadata {
	return metadata{m.UID, m.Name, m.Namespace, m.Generation, m.CreationTimestamp, m.DeletionTimestamp}
}

type condition struct {
	Type               string                 `json:"type"`
	Status             corev1.ConditionStatus `json:"status"`
	Reason             string                 `json:"reason,omitempty"`
	LastTransitionTime metav1.Time            `json:"lastTransitionTime"`
}

type container struct {
	Name     string              `json:"name"`
	Image    string              `json:"image"`
	Requests corev1.ResourceList `json:"requests,omitempty"`
	Limits   corev1.ResourceList `json:"limits,omitempty"`
}

type containerState struct {
	State      string      `json:"state"`
	Reason     string      `json:"reason,omitempty"`
	ExitCode   int32       `json:"exitCode"`
	StartedAt  metav1.Time `json:"startedAt"`
	FinishedAt metav1.Time `json:"finishedAt"`
}

type containerStatus struct {
	Name                 string         `json:"name"`
	Ready                bool           `json:"ready"`
	RestartCount         int32          `json:"restartCount"`
	State                containerState `json:"state"`
	LastTerminationState containerState `json:"lastTerminationState"`
}

func state(s corev1.ContainerState) containerState {
	switch {
	case s.Waiting != nil:
		return containerState{State: "waiting", Reason: s.Waiting.Reason}
	case s.Running != nil:
		return containerState{State: "running", StartedAt: s.Running.StartedAt}
	case s.Terminated != nil:
		return containerState{State: "terminated", Reason: s.Terminated.Reason, ExitCode: s.Terminated.ExitCode,
			StartedAt: s.Terminated.StartedAt, FinishedAt: s.Terminated.FinishedAt}
	default:
		return containerState{State: "unknown"}
	}
}

func statuses(source []corev1.ContainerStatus) []containerStatus {
	var result []containerStatus
	for _, s := range source {
		result = append(result, containerStatus{s.Name, s.Ready, s.RestartCount, state(s.State), state(s.LastTerminationState)})
	}
	return result
}

type pod struct {
	Metadata                   metadata          `json:"metadata"`
	NodeName                   string            `json:"nodeName"`
	Phase                      corev1.PodPhase   `json:"phase"`
	StartTime                  *metav1.Time      `json:"startTime,omitempty"`
	Containers                 []container       `json:"containers"`
	InitContainers             []container       `json:"initContainers,omitempty"`
	EphemeralContainers        []container       `json:"ephemeralContainers,omitempty"`
	ContainerStatuses          []containerStatus `json:"containerStatuses,omitempty"`
	InitContainerStatuses      []containerStatus `json:"initContainerStatuses,omitempty"`
	EphemeralContainerStatuses []containerStatus `json:"ephemeralContainerStatuses,omitempty"`
	Conditions                 []condition       `json:"conditions,omitempty"`
}

func sanitizePod(p corev1.Pod) pod {
	result := pod{Metadata: objectMetadata(p.ObjectMeta), NodeName: p.Spec.NodeName, Phase: p.Status.Phase, StartTime: p.Status.StartTime,
		ContainerStatuses: statuses(p.Status.ContainerStatuses), InitContainerStatuses: statuses(p.Status.InitContainerStatuses),
		EphemeralContainerStatuses: statuses(p.Status.EphemeralContainerStatuses)}
	for _, c := range p.Spec.Containers {
		result.Containers = append(result.Containers, container{c.Name, c.Image, c.Resources.Requests, c.Resources.Limits})
	}
	for _, c := range p.Spec.InitContainers {
		result.InitContainers = append(result.InitContainers, container{c.Name, c.Image, c.Resources.Requests, c.Resources.Limits})
	}
	for _, c := range p.Spec.EphemeralContainers {
		result.EphemeralContainers = append(result.EphemeralContainers, container{c.Name, c.Image, c.Resources.Requests, c.Resources.Limits})
	}
	for _, c := range p.Status.Conditions {
		result.Conditions = append(result.Conditions, condition{string(c.Type), c.Status, c.Reason, c.LastTransitionTime})
	}
	return result
}

type workload struct {
	Metadata           metadata    `json:"metadata"`
	DesiredReplicas    *int32      `json:"desiredReplicas"`
	ObservedGeneration int64       `json:"observedGeneration"`
	Replicas           int32       `json:"replicas"`
	ReadyReplicas      int32       `json:"readyReplicas"`
	AvailableReplicas  int32       `json:"availableReplicas"`
	UpdatedReplicas    int32       `json:"updatedReplicas"`
	Conditions         []condition `json:"conditions,omitempty"`
}

type objectReference struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Namespace  string    `json:"namespace"`
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
}

type event struct {
	Metadata       metadata         `json:"metadata"`
	Reason         string           `json:"reason"`
	Type           string           `json:"type"`
	Count          int32            `json:"count"`
	InvolvedObject objectReference  `json:"involvedObject"`
	FirstTimestamp metav1.Time      `json:"firstTimestamp"`
	LastTimestamp  metav1.Time      `json:"lastTimestamp"`
	EventTime      metav1.MicroTime `json:"eventTime"`
	SeriesCount    int32            `json:"seriesCount"`
	LastObservedAt metav1.MicroTime `json:"lastObservedAt"`
}

// A page is sanitized and written before requesting another. The typed client
// necessarily decodes a raw page in memory, but no raw object is ever serialized.
func listPage(ctx context.Context, client kubernetes.Interface, resource, namespace string, options metav1.ListOptions) ([]any, string, error) {
	entries := make([]any, 0)
	switch resource {
	case "pods":
		list, err := client.CoreV1().Pods(namespace).List(ctx, options)
		if err != nil {
			return nil, "", err
		}
		for _, p := range list.Items {
			entries = append(entries, sanitizePod(p))
		}
		return entries, list.Continue, nil
	case "events":
		list, err := client.CoreV1().Events(namespace).List(ctx, options)
		if err != nil {
			return nil, "", err
		}
		for _, e := range list.Items {
			ref := e.InvolvedObject
			entry := event{Metadata: objectMetadata(e.ObjectMeta), Reason: e.Reason, Type: e.Type, Count: e.Count,
				InvolvedObject: objectReference{ref.APIVersion, ref.Kind, ref.Namespace, ref.Name, ref.UID},
				FirstTimestamp: e.FirstTimestamp, LastTimestamp: e.LastTimestamp, EventTime: e.EventTime}
			if e.Series != nil {
				entry.SeriesCount, entry.LastObservedAt = e.Series.Count, e.Series.LastObservedTime
			}
			entries = append(entries, entry)
		}
		return entries, list.Continue, nil
	case "deployments":
		list, err := client.AppsV1().Deployments(namespace).List(ctx, options)
		if err != nil {
			return nil, "", err
		}
		for _, d := range list.Items {
			entry := workload{Metadata: objectMetadata(d.ObjectMeta), DesiredReplicas: d.Spec.Replicas, ObservedGeneration: d.Status.ObservedGeneration,
				Replicas: d.Status.Replicas, ReadyReplicas: d.Status.ReadyReplicas, AvailableReplicas: d.Status.AvailableReplicas, UpdatedReplicas: d.Status.UpdatedReplicas}
			for _, c := range d.Status.Conditions {
				entry.Conditions = append(entry.Conditions, condition{string(c.Type), c.Status, c.Reason, c.LastTransitionTime})
			}
			entries = append(entries, entry)
		}
		return entries, list.Continue, nil
	case "statefulsets":
		list, err := client.AppsV1().StatefulSets(namespace).List(ctx, options)
		if err != nil {
			return nil, "", err
		}
		for _, s := range list.Items {
			entry := workload{Metadata: objectMetadata(s.ObjectMeta), DesiredReplicas: s.Spec.Replicas, ObservedGeneration: s.Status.ObservedGeneration,
				Replicas: s.Status.Replicas, ReadyReplicas: s.Status.ReadyReplicas, AvailableReplicas: s.Status.AvailableReplicas, UpdatedReplicas: s.Status.UpdatedReplicas}
			for _, c := range s.Status.Conditions {
				entry.Conditions = append(entry.Conditions, condition{string(c.Type), c.Status, c.Reason, c.LastTransitionTime})
			}
			entries = append(entries, entry)
		}
		return entries, list.Continue, nil
	default:
		return nil, "", fmt.Errorf("unsupported recording resource")
	}
}

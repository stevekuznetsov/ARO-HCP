// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package startup

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestListedControllerConvergencePreservesPredicates(t *testing.T) {
	for _, kind := range []string{"Deployment", "StatefulSet", "DaemonSet"} {
		for _, mode := range []string{"ready", "missing", "deleting", "generation", "replicas", "updated", "ready-replicas", "available-or-current", "unavailable-or-revision", "empty-revision", "lifecycle"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				spec := Spec{Components: []Component{{Kind: kind, Name: "controller", Replicas: 3}}}
				metadata := metav1.ObjectMeta{Name: "controller", Generation: 2}
				if mode == "deleting" {
					now := metav1.Now()
					metadata.DeletionTimestamp = &now
				}
				d := appsv1.Deployment{ObjectMeta: metadata, Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 3, AvailableReplicas: 3}}
				s := appsv1.StatefulSet{ObjectMeta: metadata, Status: appsv1.StatefulSetStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 3, CurrentReplicas: 3, CurrentRevision: "same", UpdateRevision: "same"}}
				ds := appsv1.DaemonSet{ObjectMeta: metadata, Status: appsv1.DaemonSetStatus{ObservedGeneration: 2, CurrentNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberReady: 3, NumberAvailable: 3}}
				switch mode {
				case "generation":
					d.Status.ObservedGeneration, s.Status.ObservedGeneration, ds.Status.ObservedGeneration = 1, 1, 1
				case "replicas":
					d.Status.Replicas, s.Status.Replicas, ds.Status.CurrentNumberScheduled = 4, 4, 4
				case "updated":
					d.Status.UpdatedReplicas, s.Status.UpdatedReplicas, ds.Status.UpdatedNumberScheduled = 2, 2, 2
				case "ready-replicas":
					d.Status.ReadyReplicas, s.Status.ReadyReplicas, ds.Status.NumberReady = 2, 2, 2
				case "available-or-current":
					d.Status.AvailableReplicas, s.Status.CurrentReplicas, ds.Status.NumberAvailable = 2, 2, 2
				case "unavailable-or-revision":
					d.Status.UnavailableReplicas, s.Status.UpdateRevision, ds.Status.NumberMisscheduled = 1, "different", 1
				case "empty-revision":
					if kind != "StatefulSet" {
						return
					}
					s.Status.CurrentRevision, s.Status.UpdateRevision = "", ""
				case "lifecycle":
					spec.Components[0].Lifecycle = &Lifecycle{AllowAbsent: true}
				}
				deployments, sets, daemons := []appsv1.Deployment{d}, []appsv1.StatefulSet{s}, []appsv1.DaemonSet{ds}
				if mode == "missing" || mode == "lifecycle" {
					deployments, sets, daemons = nil, nil, nil
				}
				sample := Sample{}
				err := checkControllerConvergence(spec, &sample, deployments, sets, daemons)
				if mode == "missing" {
					if err == nil {
						t.Fatal("missing controller no longer fails closed")
					}
					return
				}
				if err != nil || (len(sample.Issues) == 0) != (mode == "ready" || mode == "lifecycle") {
					t.Fatalf("convergence predicate changed: %+v %v", sample, err)
				}
			})
		}
	}
}

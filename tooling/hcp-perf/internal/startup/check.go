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
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
)

// SnapshotHistory retains restart/OOM identities between read-only inspections.
// The first inspection includes historical failures, deliberately requiring a
// clean initial gate. It does not turn a late observation into fresh startup.
type SnapshotHistory struct{ history healthHistory }

// CheckSnapshot reuses startup's full inventory, actual/template resource and
// controller readiness checks without starting an observer or querying the RP.
func CheckSnapshot(ctx context.Context, clients Clients, spec Spec, identity Identity, history *SnapshotHistory) (Sample, error) {
	sample := Sample{At: time.Now().UTC(), Identity: identity, Issues: []string{}}
	if identity.HCPUID == "" || history == nil {
		return sample, errors.New("snapshot requires bound identity and history")
	}
	if history.history.restarts == nil {
		history.history = healthHistory{restarts: map[string]int32{}, ooms: map[string]string{}, probeEvents: map[string]int32{}}
	}
	err := inspect(ctx, clients, spec, identity, &sample, &history.history, true)
	sample.Healthy = err == nil && len(sample.Issues) == 0 && sample.NewOOMs == 0 && sample.NewRestarts == 0 && sample.NewProbeKills == 0
	return sample, err
}

// Performance admission needs full current-generation convergence, rather than
// the startup progress monitor's available-replica lower bound.
// Use the same live lists as profile/ownership inspection, not a second GET per
// component. This keeps request cost independent of the number of controllers.
func checkControllerConvergence(spec Spec, sample *Sample, deployments []appsv1.Deployment, sets []appsv1.StatefulSet, daemons []appsv1.DaemonSet) error {
	deploymentByName := map[string]appsv1.Deployment{}
	for _, d := range deployments {
		deploymentByName[d.Name] = d
	}
	setByName := map[string]appsv1.StatefulSet{}
	for _, s := range sets {
		setByName[s.Name] = s
	}
	daemonByName := map[string]appsv1.DaemonSet{}
	for _, d := range daemons {
		daemonByName[d.Name] = d
	}
	for _, component := range spec.Components {
		if component.Lifecycle != nil {
			continue
		}
		n := component.Replicas
		ready := true
		found := true
		switch component.Kind {
		case "Deployment":
			var d appsv1.Deployment
			d, found = deploymentByName[component.Name]
			ready = found && d.DeletionTimestamp == nil && d.Status.ObservedGeneration >= d.Generation && d.Status.Replicas == n && d.Status.UpdatedReplicas == n && d.Status.ReadyReplicas == n && d.Status.AvailableReplicas == n && d.Status.UnavailableReplicas == 0
		case "StatefulSet":
			var s appsv1.StatefulSet
			s, found = setByName[component.Name]
			ready = found && s.DeletionTimestamp == nil && s.Status.ObservedGeneration >= s.Generation && s.Status.Replicas == n && s.Status.ReadyReplicas == n && s.Status.CurrentReplicas == n && s.Status.UpdatedReplicas == n && s.Status.CurrentRevision != "" && s.Status.CurrentRevision == s.Status.UpdateRevision
		case "DaemonSet":
			var d appsv1.DaemonSet
			d, found = daemonByName[component.Name]
			ready = found && d.DeletionTimestamp == nil && d.Status.ObservedGeneration >= d.Generation && d.Status.CurrentNumberScheduled == n && d.Status.UpdatedNumberScheduled == n && d.Status.NumberReady == n && d.Status.NumberAvailable == n && d.Status.NumberMisscheduled == 0
		}
		if !found {
			return fmt.Errorf("controller missing from live inventory: %s/%s", component.Kind, component.Name)
		}
		if !ready {
			sample.Issues = append(sample.Issues, "controller not converged: "+component.Kind+"/"+component.Name)
		}
	}
	return nil
}

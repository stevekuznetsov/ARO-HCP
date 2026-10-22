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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/record"
)

// Discover matches the ARM-ID annotation, then the exact HCP annotation referring
// to HC namespace/name. HCP clusterID is supplemental, not a discovery fallback.
func Discover(ctx context.Context, clients Clients, resourceID string, previous Identity) (Identity, error) {
	identity := Identity{ResourceID: resourceID}
	clusters, err := clients.Dynamic.Resource(HostedClusters).List(ctx, metav1.ListOptions{})
	if err != nil {
		return identity, fmt.Errorf("HostedCluster discovery request failed: %w", err)
	}
	for _, cluster := range clusters.Items {
		if !strings.EqualFold(cluster.GetAnnotations()[ResourceIDAnnotation], resourceID) {
			continue
		}
		if identity.HCUID != "" {
			return identity, errors.New("ambiguous HostedCluster ARM identity")
		}
		identity.HCNamespace, identity.HCName, identity.HCUID = cluster.GetNamespace(), cluster.GetName(), string(cluster.GetUID())
		identity.ClusterID, _, _ = unstructured.NestedString(cluster.Object, "spec", "clusterID")
		identity.CSID = cluster.GetLabels()["api.openshift.com/id"]
		if identity.HCUID == "" || identity.ClusterID == "" {
			return identity, errors.New("HostedCluster lacks immutable identity")
		}
	}
	if identity.HCUID == "" {
		if previous.HCUID != "" {
			return previous, errors.New("bound HostedCluster disappeared")
		}
		return identity, nil
	}
	planes, err := clients.Dynamic.Resource(HostedControlPlanes).List(ctx, metav1.ListOptions{})
	if err != nil {
		return identity, fmt.Errorf("HostedControlPlane discovery request failed: %w", err)
	}
	for _, plane := range planes.Items {
		if plane.GetAnnotations()[HostedClusterAnnotation] != identity.HCNamespace+"/"+identity.HCName {
			continue
		}
		if identity.HCPUID != "" {
			return identity, errors.New("ambiguous HostedControlPlane HostedCluster reference")
		}
		clusterID, _, _ := unstructured.NestedString(plane.Object, "spec", "clusterID")
		if clusterID != "" && clusterID != identity.ClusterID {
			return identity, errors.New("HostedControlPlane supplemental clusterID mismatch")
		}
		if csid := plane.GetLabels()["api.openshift.com/id"]; csid != "" && identity.CSID != "" && csid != identity.CSID {
			return identity, errors.New("HostedControlPlane CS identity mismatch")
		}
		if armID := plane.GetAnnotations()[ResourceIDAnnotation]; armID != "" && !strings.EqualFold(armID, resourceID) {
			return identity, errors.New("HostedControlPlane ARM identity mismatch")
		}
		identity.HCPNamespace, identity.HCPName, identity.HCPUID = plane.GetNamespace(), plane.GetName(), string(plane.GetUID())
		namespace, err := clients.Kubernetes.CoreV1().Namespaces().Get(ctx, identity.HCPNamespace, metav1.GetOptions{})
		if err != nil {
			return identity, fmt.Errorf("control plane namespace identity request failed: %w", err)
		}
		identity.NamespaceUID = string(namespace.UID)
		if identity.HCPUID == "" || identity.NamespaceUID == "" {
			return identity, errors.New("control plane lacks immutable identity")
		}
	}
	for _, pair := range [][2]string{{previous.HCUID, identity.HCUID}, {previous.ClusterID, identity.ClusterID}, {previous.CSID, identity.CSID}, {previous.HCPUID, identity.HCPUID}, {previous.NamespaceUID, identity.NamespaceUID}} {
		if pair[0] != "" && pair[0] != pair[1] {
			return previous, errors.New("bound startup identity changed or disappeared")
		}
	}
	return identity, nil
}

type healthHistory struct {
	restarts      map[string]int32
	ooms          map[string]string
	healthySince  time.Time
	violated      bool
	probeEvents   map[string]int32
	probeDegraded bool
}

// Observe keeps collecting after restarts, OOMs, profile drift and API errors.
// The final sample separates current sustained health from fresh qualification.
// ready is called only after the initial discovery baseline is durably saved.
func Observe(ctx context.Context, clients Clients, spec Spec, dir string, ready func() error) (Sample, error) {
	return observe(ctx, clients, spec, dir, ready, nil)
}

// ObserveContinuation starts a new, explicitly interrupted measurement against
// an already verified identity. It can report current health, never fresh startup
// qualification. The original attempt's evidence is not modified or spliced in.
func ObserveContinuation(ctx context.Context, clients Clients, spec Spec, dir string, identity Identity, ready func() error) (Sample, error) {
	if identity.HCUID == "" || !strings.EqualFold(identity.ResourceID, spec.ResourceID) {
		return Sample{}, errors.New("observation continuation requires a bound HostedCluster identity")
	}
	return observe(ctx, clients, spec, dir, ready, &identity)
}

func observe(ctx context.Context, clients Clients, spec Spec, dir string, ready func() error, continuation *Identity) (Sample, error) {
	if err := spec.Validate(); err != nil {
		return Sample{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, spec.MaxDuration)
	defer cancel()
	historyFile, err := os.OpenFile(filepath.Join(dir, "history.ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Sample{}, err
	}
	defer historyFile.Close()
	violations, err := os.OpenFile(filepath.Join(dir, "violations.ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Sample{}, err
	}
	defer violations.Close()
	history := healthHistory{restarts: map[string]int32{}, ooms: map[string]string{}, probeEvents: map[string]int32{}}
	var identity Identity
	if continuation != nil {
		identity = *continuation
		history.violated = true
	}
	var sample Sample
	var recorderDone chan error
	defer func() {
		cancel()
		if recorderDone != nil {
			<-recorderDone
		}
	}()
	ticker := time.NewTicker(spec.PollInterval)
	defer ticker.Stop()
	armed := false
	var recorderFailure bool
	var lastSuccessfulPoll *time.Time
	for {
		if recorderDone != nil {
			select {
			case recordErr := <-recorderDone:
				recorderFailure = recordErr != nil
				recorderDone = nil
			default:
			}
		}
		next, discoverErr := Discover(ctx, clients, spec.ResourceID, identity)
		sample = Sample{At: time.Now().UTC(), Identity: identity, Issues: []string{}, ProbeFailurePolicy: spec.EffectiveProbeFailurePolicy()}
		if discoverErr != nil {
			sample.Issues = append(sample.Issues, discoverErr.Error())
		} else {
			identity, sample.Identity = next, next
			if identity.HCPUID != "" {
				if recorderDone == nil && !recorderFailure {
					recorderDone = make(chan error, 1)
					go func(namespace string) {
						defer utilruntime.HandleCrash()
						result, err := record.Record(ctx, clients.Kubernetes, record.Spec{Version: "v1", Namespaces: []string{namespace}, IntervalSeconds: int64(spec.PollInterval / time.Second), DurationSeconds: int64(spec.MaxDuration / time.Second), OutputDir: filepath.Join(dir, "record")})
						if result.APIErrors > 0 || result.IncompleteStreams > 0 {
							err = errors.New("snapshot recorder has incomplete streams or API failures")
						}
						recorderDone <- err
					}(identity.HCPNamespace)
				}
				if err := inspect(ctx, clients, spec, identity, &sample, &history, false); err != nil {
					sample.Issues = append(sample.Issues, "Kubernetes health inspection failed")
				} else {
					at := time.Now().UTC()
					lastSuccessfulPoll = &at
				}
			} else {
				sample.Issues = append(sample.Issues, "waiting for exact control plane identity")
			}
		}
		if recorderFailure {
			sample.Issues = append(sample.Issues, "snapshot recorder failed")
			history.violated = true
		}
		if discoverErr != nil {
			history.violated = true
		}
		qualify(&sample, &history, spec.HealthyWindow, identity.HCPUID != "")
		sample.At = time.Now().UTC()
		sample.LastSuccessfulPoll = lastSuccessfulPoll
		if !history.healthySince.IsZero() {
			since := history.healthySince
			sample.HealthySince = &since
		}
		if err := json.NewEncoder(historyFile).Encode(sample); err != nil {
			return sample, err
		}
		if err := historyFile.Sync(); err != nil {
			return sample, err
		}
		if sample.NewOOMs > 0 || sample.NewRestarts > 0 || sample.NewProbeFailures > 0 || sample.NewProbeKills > 0 || (identity.HCPUID != "" && len(sample.Issues) > 0) {
			if err := json.NewEncoder(violations).Encode(sample); err != nil {
				return sample, err
			}
			if err := violations.Sync(); err != nil {
				return sample, err
			}
		}
		if err := publish(dir, "identity.json", identity); err != nil {
			return sample, err
		}
		if err := publish(dir, "startup-status.json", sample); err != nil {
			return sample, err
		}
		if !armed {
			if discoverErr != nil {
				return sample, discoverErr
			}
			if continuation == nil && identity.HCUID != "" {
				return sample, errors.New("startup must be armed before the target HostedCluster exists")
			}
			candidate, err := clients.Dynamic.Resource(SizingConfigurations).Get(ctx, spec.CandidateName, metav1.GetOptions{})
			if err != nil {
				return sample, errors.New("candidate preflight request failed")
			}
			expected, err := DecodeCandidateSpec(spec.CandidateSpec)
			if err != nil || !candidateSpecEqual(candidate.Object["spec"], expected) {
				return sample, errors.New("candidate preflight spec mismatch")
			}
			if err := ready(); err != nil {
				return sample, err
			}
			armed = true
		}
		if availabilityComplete(spec, sample) {
			if err := ctx.Err(); err != nil {
				return sample, err
			}
			cancel()
			if recorderDone != nil {
				recordErr := <-recorderDone
				recorderDone = nil
				if recordErr != nil && !errors.Is(recordErr, context.Canceled) {
					return sample, errors.New("startup snapshot recording incomplete")
				}
			}
			return sample, nil
		}
		select {
		case <-ctx.Done():
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return sample, ctx.Err()
			}
			if recorderDone != nil {
				recordErr := <-recorderDone
				recorderDone = nil
				if recordErr != nil && !errors.Is(recordErr, context.DeadlineExceeded) && !errors.Is(recordErr, context.Canceled) {
					return sample, errors.New("startup snapshot recording incomplete")
				}
			}
			if !sample.HealthyWindow {
				return sample, errors.New("startup ended without sustained current health")
			}
			return sample, nil
		case <-ticker.C:
		}
	}
}

func availabilityComplete(spec Spec, sample Sample) bool {
	return spec.EffectiveProbeFailurePolicy() == Availability && sample.Healthy && sample.HealthyWindow && sample.Coverage != nil && sample.Coverage.Complete && len(sample.Issues) == 0 && sample.NewRestarts == 0 && sample.NewOOMs == 0 && sample.NewProbeKills == 0
}

func qualify(sample *Sample, history *healthHistory, window time.Duration, discovered bool) {
	sample.Healthy = discovered && len(sample.Issues) == 0 && sample.NewOOMs == 0 && sample.NewRestarts == 0 && sample.NewProbeKills == 0
	if !sample.Healthy {
		history.healthySince = time.Time{}
		// Missing readiness during initial bootstrap is expected; restarts, OOMs
		// and profile/inventory violations permanently disqualify a fresh run.
		if sample.NewOOMs > 0 || sample.NewRestarts > 0 || sample.NewProbeKills > 0 {
			history.violated = true
		}
	} else if history.healthySince.IsZero() {
		history.healthySince = sample.At
	}
	sample.HealthyWindow = sample.Healthy && sample.At.Sub(history.healthySince) >= window
	sample.FreshQualified = sample.HealthyWindow && !history.violated
}

func inspect(ctx context.Context, clients Clients, spec Spec, identity Identity, sample *Sample, history *healthHistory, requireConvergence bool) (err error) {
	stage := "candidate"
	defer func() {
		err = errors.Join(err, ctx.Err())
		if err != nil {
			err = fmt.Errorf("%s inspection: %w", stage, err)
		}
	}()
	sample.ProbeFailurePolicy = spec.EffectiveProbeFailurePolicy()
	sample.Coverage = &Coverage{RequiredWorkloads: len(spec.Components)}
	for _, component := range spec.Components {
		sample.Coverage.RequiredPods += component.Replicas
		sample.Coverage.RequiredEndpoints += component.ReadyEndpoints
	}
	config, err := clients.Dynamic.Resource(SizingConfigurations).Get(ctx, spec.CandidateName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	desired, err := DecodeCandidateSpec(spec.CandidateSpec)
	if err != nil {
		return err
	}
	if !candidateSpecEqual(config.Object["spec"], desired) {
		sample.Issues = append(sample.Issues, "candidate spec drift")
		history.violated = true
	}
	stage = "pods"
	pods, err := clients.Kubernetes.CoreV1().Pods(identity.HCPNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	workloads := map[string]corev1.PodTemplateSpec{}
	workloadUIDs := map[string]types.UID{}
	selectors := map[string]labels.Selector{}
	readyReplicas := map[string]int32{}
	desiredReplicas := map[string]int32{}
	stage = "deployments"
	deployments, err := clients.Kubernetes.AppsV1().Deployments(identity.HCPNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, deployment := range deployments.Items {
		key := "Deployment/" + deployment.Name
		workloads[key] = deployment.Spec.Template
		workloadUIDs[key] = deployment.UID
		selectors[key], err = metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
		if err != nil {
			return err
		}
		if deployment.Spec.Replicas != nil {
			desiredReplicas[key] = *deployment.Spec.Replicas
		}
		if deployment.Status.ObservedGeneration >= deployment.Generation {
			readyReplicas[key] = deployment.Status.AvailableReplicas
		}
	}
	stage = "statefulsets"
	sets, err := clients.Kubernetes.AppsV1().StatefulSets(identity.HCPNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, set := range sets.Items {
		key := "StatefulSet/" + set.Name
		workloads[key] = set.Spec.Template
		workloadUIDs[key] = set.UID
		selectors[key], err = metav1.LabelSelectorAsSelector(set.Spec.Selector)
		if err != nil {
			return err
		}
		if set.Spec.Replicas != nil {
			desiredReplicas[key] = *set.Spec.Replicas
		}
		if set.Status.ObservedGeneration >= set.Generation {
			readyReplicas[key] = set.Status.ReadyReplicas
		}
	}
	stage = "daemonsets"
	daemons, err := clients.Kubernetes.AppsV1().DaemonSets(identity.HCPNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, daemon := range daemons.Items {
		key := "DaemonSet/" + daemon.Name
		workloads[key] = daemon.Spec.Template
		workloadUIDs[key] = daemon.UID
		selectors[key], err = metav1.LabelSelectorAsSelector(daemon.Spec.Selector)
		if err != nil {
			return err
		}
		desiredReplicas[key] = daemon.Status.DesiredNumberScheduled
		if daemon.Status.ObservedGeneration >= daemon.Generation {
			readyReplicas[key] = daemon.Status.NumberReady
		}
	}
	expected := map[string]bool{}
	stage = "replicasets"
	replicaSets, err := clients.Kubernetes.AppsV1().ReplicaSets(identity.HCPNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	parents := map[types.UID]types.UID{}
	obsoleteReplicaSets := map[types.UID]bool{}
	for _, replicaSet := range replicaSets.Items {
		for _, owner := range replicaSet.OwnerReferences {
			if owner.Controller != nil && *owner.Controller && owner.Kind == "Deployment" {
				parents[replicaSet.UID] = owner.UID
				key := "Deployment/" + owner.Name
				if owner.UID != "" && owner.UID == workloadUIDs[key] && replicaSet.Spec.Replicas != nil && *replicaSet.Spec.Replicas == 0 {
					// Like Deployment revision matching, ignore only the generated hash label.
					previous, template := replicaSet.Spec.Template.DeepCopy(), workloads[key]
					current := template.DeepCopy()
					delete(previous.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
					delete(current.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
					obsoleteReplicaSets[replicaSet.UID] = !equality.Semantic.DeepEqual(previous, current)
				}
			}
		}
	}
	matchedPods := map[string]bool{}
	stage = "batch workloads"
	if err := inspectBatch(ctx, clients, spec.Components, identity.HCPNamespace, pods.Items, matchedPods, sample, history); err != nil {
		return err
	}
	for _, component := range spec.Components {
		if component.Kind == "Job" || component.Kind == "CronJob" || component.Kind == "Pod" {
			continue
		}
		key := component.Kind + "/" + component.Name
		expected[key] = true
		template, found := workloads[key]
		if !found {
			if component.Lifecycle != nil && component.Lifecycle.AllowAbsent {
				continue
			}
			sample.Issues = append(sample.Issues, "missing "+key)
			continue
		}
		sample.Coverage.ObservedWorkloads++
		if desiredReplicas[key] != component.Replicas || readyReplicas[key] < component.Replicas {
			sample.Issues = append(sample.Issues, "replicas not ready: "+key)
		}
		if !matchesProfile(template.Spec, component) {
			sample.Issues = append(sample.Issues, "template resources mismatch: "+key)
			history.violated = true
		}
		var ready int32
		for _, pod := range pods.Items {
			if !selectors[key].Matches(labels.Set(pod.Labels)) {
				continue
			}
			owned := false
			obsolete := false
			for _, owner := range pod.OwnerReferences {
				if owner.Controller == nil || !*owner.Controller {
					continue
				}
				uid := owner.UID
				if owner.Kind == "ReplicaSet" {
					uid = parents[uid]
					obsolete = obsolete || (component.Kind == "Deployment" && uid != "" && uid == workloadUIDs[key] && obsoleteReplicaSets[owner.UID])
				}
				owned = owned || (uid != "" && uid == workloadUIDs[key])
			}
			if !owned {
				continue
			}
			matchedPods[string(pod.UID)] = true
			terminal := pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
			if terminal && obsolete && component.Lifecycle == nil {
				// Retain inventory and failure history below, but never enforce today's
				// serving profile or count readiness on an obsolete terminal record.
				continue
			}
			if terminal && component.Kind == "Deployment" && component.Lifecycle == nil {
				sample.Issues = append(sample.Issues, "terminal pod in current deployment: "+pod.Name)
			}
			if component.Lifecycle != nil && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				sample.Issues = append(sample.Issues, "disabled workload has active pod: "+pod.Name)
				history.violated = true
			}
			if !matchesProfile(pod.Spec, component) {
				sample.Issues = append(sample.Issues, "observed pod resources mismatch: "+pod.Name)
				history.violated = true
			}
			if !terminal && pod.DeletionTimestamp == nil {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						ready++
					}
				}
			}
		}
		if ready < component.Replicas {
			sample.Issues = append(sample.Issues, "observed pods not ready: "+key)
		}
		sample.Coverage.ReadyPods += ready
		if component.Endpoints != "" {
			stage = "endpoints/" + component.Endpoints
			endpoints, err := clients.Kubernetes.CoreV1().Endpoints(identity.HCPNamespace).Get(ctx, component.Endpoints, metav1.GetOptions{})
			if err != nil {
				if requireConvergence {
					return err
				}
				sample.Issues = append(sample.Issues, "endpoint inspection failed: "+key)
			} else {
				count := 0
				for _, subset := range endpoints.Subsets {
					count += len(subset.Addresses)
				}
				sample.Coverage.ReadyEndpoints += count
				if count < component.ReadyEndpoints {
					sample.Issues = append(sample.Issues, "endpoints not ready: "+key)
				}
			}
		}
	}
	for key := range workloads {
		if !expected[key] {
			sample.Issues = append(sample.Issues, "undeclared workload: "+key)
			history.violated = true
		}
	}
	for _, pod := range pods.Items {
		if !matchedPods[string(pod.UID)] {
			sample.Issues = append(sample.Issues, "undeclared pod: "+pod.Name)
			history.violated = true
		}
		for _, statuses := range [][]corev1.ContainerStatus{pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses} {
			for _, status := range statuses {
				key := string(pod.UID) + "/" + status.Name
				if status.RestartCount > history.restarts[key] {
					sample.NewRestarts += status.RestartCount - history.restarts[key]
				}
				history.restarts[key] = status.RestartCount
				for _, terminated := range []*corev1.ContainerStateTerminated{status.State.Terminated, status.LastTerminationState.Terminated} {
					if terminated != nil && terminated.Reason == "OOMKilled" {
						stamp := key + "/" + terminated.FinishedAt.String()
						if history.ooms[stamp] == "" {
							sample.NewOOMs++
							history.ooms[stamp] = "seen"
						}
					}
				}
				if status.State.Waiting != nil && status.State.Waiting.Reason == "CrashLoopBackOff" {
					sample.Issues = append(sample.Issues, "container crash loop: "+pod.Name+"/"+status.Name)
				}
			}
		}
	}
	stage = "events"
	events, err := clients.Kubernetes.CoreV1().Events(identity.HCPNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, event := range events.Items {
		if matchedPods[string(event.InvolvedObject.UID)] {
			observeProbeEvent(event, spec.EffectiveProbeFailurePolicy(), sample, history)
		}
	}
	sample.ProbeQuality = "clean"
	if history.probeDegraded {
		sample.ProbeQuality = "degraded"
	}
	sample.Coverage.Complete = true
	if requireConvergence {
		stage = "controller convergence"
		return checkControllerConvergence(spec, sample, deployments.Items, sets.Items, daemons.Items)
	}
	return nil
}

func observeProbeEvent(event corev1.Event, policy ProbeFailurePolicy, sample *Sample, history *healthHistory) {
	// Event messages are used only to classify kubelet probe-triggered Killing,
	// never persisted. An Unhealthy attempt is not evidence of a restart.
	message := strings.ToLower(event.Message)
	probeKill := event.Reason == "Killing" && (strings.Contains(message, "failed liveness probe") || strings.Contains(message, "failed startup probe"))
	if event.Reason != "Unhealthy" && !probeKill {
		return
	}
	key := string(event.UID) + "/" + event.Reason
	count := max(event.Count, 1)
	if event.Series != nil {
		count = max(count, event.Series.Count)
	}
	delta := count - history.probeEvents[key]
	if delta <= 0 {
		return
	}
	history.probeEvents[key] = count
	history.probeDegraded = true
	if probeKill {
		sample.NewProbeKills += int(delta)
		sample.Issues = append(sample.Issues, "new probe-triggered container kill")
		history.violated = true
		return
	}
	sample.NewProbeFailures += int(delta)
	if policy == Availability {
		sample.Warnings = append(sample.Warnings, "new probe failure event")
	} else {
		sample.Issues = append(sample.Issues, "new probe failure event")
		history.violated = true
	}
}

func matchesProfile(spec corev1.PodSpec, component Component) bool {
	if len(spec.EphemeralContainers) != 0 {
		return false
	}
	for _, group := range []struct {
		actual   []corev1.Container
		expected []Container
	}{{spec.Containers, component.Containers}, {spec.InitContainers, component.InitContainers}} {
		if len(group.actual) != len(group.expected) {
			return false
		}
		for _, expected := range group.expected {
			found := false
			for _, actual := range group.actual {
				if actual.Name != expected.Name {
					continue
				}
				found = true
				if expected.Image != "" && actual.Image != expected.Image {
					return false
				}
				if expected.Env != nil {
					observedEnv := map[string]string{}
					for _, variable := range actual.Env {
						if variable.Name != "GOMEMLIMIT" && variable.Name != "GOMAXPROCS" {
							continue
						}
						if variable.ValueFrom != nil {
							return false
						}
						if _, exists := observedEnv[variable.Name]; exists {
							return false
						}
						observedEnv[variable.Name] = variable.Value
					}
					if len(observedEnv) != len(*expected.Env) {
						return false
					}
					for _, variable := range *expected.Env {
						value, exists := observedEnv[variable.Name]
						if !exists || value != variable.Value {
							return false
						}
					}
				}
				for _, pair := range [][2]corev1.ResourceList{{actual.Resources.Requests, expected.Resources.Requests}, {actual.Resources.Limits, expected.Resources.Limits}} {
					if len(pair[0]) != len(pair[1]) {
						return false
					}
					for name, quantity := range pair[1] {
						observed, exists := pair[0][name]
						if !exists || quantity.Cmp(observed) != 0 {
							return false
						}
					}
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func publish(dir, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".tmp"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(dir, name+".tmp"), filepath.Join(dir, name)); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open startup directory: %w", err)
	}
	return errors.Join(directory.Sync(), directory.Close())
}

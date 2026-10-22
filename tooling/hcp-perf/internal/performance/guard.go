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
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	podresource "k8s.io/component-helpers/resource"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/record"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

func captureReady(ctx context.Context, client *http.Client, capture Capture) error {
	for _, source := range capture.Sources {
		selector := "up{job=" + strconv.Quote(source.Job) + "}"
		up, err := metrics.Instant(ctx, client, capture.Endpoint, selector)
		if err != nil || len(up) != source.Replicas {
			return errors.New("capture up coverage incomplete")
		}
		stamps, err := metrics.Instant(ctx, client, capture.Endpoint, "timestamp("+selector+")")
		if err != nil || len(stamps) != len(up) {
			return errors.New("capture source timestamps incomplete")
		}
		seen := map[string]bool{}
		for _, point := range up {
			delete(point.Labels, "__name__")
			key := labels.Set(point.Labels).String()
			if point.Labels["job"] != source.Job || point.Value != 1 || seen[key] {
				return errors.New("capture replica down or duplicated")
			}
			seen[key] = true
		}
		for _, point := range stamps {
			delete(point.Labels, "__name__")
			key := labels.Set(point.Labels).String()
			age := time.Since(time.UnixMilli(int64(point.Value * 1000)))
			if !seen[key] || age < -5*time.Second || age > capture.maxAge(source.Job) {
				return errors.New("capture source stale or mismatched")
			}
			delete(seen, key)
		}
	}
	return nil
}

func recorderReady(dir string, age time.Duration) error {
	health, err := record.Health(dir)
	if err != nil || health.Checkpoint == nil || health.Final != nil {
		return errors.New("recorder has no active checkpoint")
	}
	result := health.Checkpoint.Result
	if len(result.Streams) != len(health.Manifest.Spec.Namespaces)*4 {
		return errors.New("recorder stream inventory incomplete")
	}
	for _, stream := range result.Streams {
		if stream.LastSuccess == nil || time.Since(*stream.LastSuccess) > age {
			return errors.New("recorder stream lacks fresh successful snapshot")
		}
	}
	return nil
}

type nodeCapacity struct {
	Name              string
	UID               string
	Pods, CPU, Memory int64
}

func guestNodesSafe(nodes []corev1.Node, spec Spec) error {
	if err := spec.validateGuestNodes(); err != nil {
		return err
	}
	if len(nodes) != len(spec.ExpectedGuestNodes) {
		return errors.New("guest node inventory count changed")
	}
	expected := map[string]string{}
	for _, node := range spec.ExpectedGuestNodes {
		expected[node.Name] = node.UID
	}
	for _, node := range nodes {
		if uid, ok := expected[node.Name]; !ok || uid != string(node.UID) {
			return fmt.Errorf("guest node identity mismatch: %s", node.Name)
		}
		delete(expected, node.Name)
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				ready = condition.Status == corev1.ConditionTrue
			}
			if (condition.Type == corev1.NodeMemoryPressure || condition.Type == corev1.NodeDiskPressure || condition.Type == corev1.NodePIDPressure) && condition.Status != corev1.ConditionFalse {
				return fmt.Errorf("guest node pressure: %s", node.Name)
			}
		}
		if !ready || node.Spec.Unschedulable || node.DeletionTimestamp != nil || node.Labels[corev1.LabelOSStable] != "linux" {
			return fmt.Errorf("guest node not Ready schedulable Linux: %s", node.Name)
		}
		for _, taint := range node.Spec.Taints {
			if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
				return fmt.Errorf("guest node scheduling taint: %s", node.Name)
			}
		}
	}
	return nil
}

// capacity reserves all existing nonterminal Pods (including terminating Pods),
// unseeded replicas, rollout headroom, and a canary Pod. Each
// eligible node must admit its balanced share; this is conservative admission,
// not a guarantee about the scheduler's future placement.
func capacity(nodes []corev1.Node, pods []corev1.Pod, spec Spec) ([]nodeCapacity, error) {
	if err := guestNodesSafe(nodes, spec); err != nil {
		return nil, err
	}
	free := map[string]*nodeCapacity{}
	for _, node := range nodes {
		free[node.Name] = &nodeCapacity{Name: node.Name, UID: string(node.UID), Pods: node.Status.Allocatable.Pods().Value(), CPU: node.Status.Allocatable.Cpu().MilliValue(), Memory: node.Status.Allocatable.Memory().Value()}
	}
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if pod.Spec.NodeName == "" {
			return nil, errors.New("guest has unassigned platform pods")
		}
		if node := free[pod.Spec.NodeName]; node != nil {
			requests := podresource.PodRequests(&pod, podresource.PodResourcesOptions{})
			node.Pods--
			node.CPU -= requests.Cpu().MilliValue()
			node.Memory -= requests.Memory().Value()
		} else {
			return nil, errors.New("guest pod assigned to unknown node")
		}
	}
	cpuQuantity := resource.MustParse(spec.Seed.PerPodCPU)
	memoryQuantity := resource.MustParse(spec.Seed.PerPodMemory)
	cpu := cpuQuantity.MilliValue()
	memory := memoryQuantity.Value()
	// Canary setup's fixed requests are 5m/8Mi; reserve the larger of these
	// and the application requests for every admitted slot.
	cpu = max(cpu, 5)
	memory = max(memory, 8<<20)
	population := spec.Seed.Applications * 5
	if spec.ExistingPopulation != nil && !spec.RestorePopulation {
		population = 0 // Live Pods above already charge the retained population.
	}
	// Turnover enforces maxSurge=1/maxUnavailable=0, but terminating old Pods
	// can outlive replacements. The population charge already covers five old
	// replicas; reserve five replacements plus two conservative headroom slots per
	// outstanding intent, for fresh AND reused fixtures. The two slots are policy
	// margin, not an additional maxSurge allowance on top of five replacements.
	// This is not another steady population charge or a bound on arbitrary churn.
	required := population + 7*min(int64(spec.MaxOutstanding), spec.Seed.Applications) + 1
	share := (required + int64(len(free)) - 1) / int64(len(free))
	result := []nodeCapacity{}
	for _, node := range free {
		if node.Pods < share+2 || node.CPU < share*cpu || node.Memory < share*memory {
			return nil, fmt.Errorf("guest per-node capacity insufficient: %s", node.Name)
		}
		result = append(result, *node)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

type restartEvent struct {
	at    time.Time
	owner string
	count int32
}
type safety struct {
	sync.Mutex
	history      startup.SnapshotHistory
	restarts     map[string]int32
	events       []restartEvent
	unreadySince time.Time
	observing    bool
	summary      StabilitySummary
	livez        func(context.Context) error
}

func (g *safety) check(ctx context.Context, clients startup.Clients, spec Spec, identity startup.Identity, initial bool) (startup.Sample, error) {
	g.Lock()
	defer g.Unlock()
	next, err := startup.Discover(ctx, clients, spec.Startup.ResourceID, identity)
	if err != nil {
		return startup.Sample{}, fmt.Errorf("performance identity inspection: %w", err)
	}
	if next != identity {
		return startup.Sample{}, errors.New("performance identity changed or unavailable")
	}
	sample, err := startup.CheckSnapshot(ctx, clients, spec.Startup, identity, &g.history)
	if err != nil {
		return sample, fmt.Errorf("control-plane inspection incomplete: %w", err)
	}
	if initial {
		// Baseline historical counters, then require a fresh converged snapshot.
		// New failures between these reads still fail admission.
		sample, err = startup.CheckSnapshot(ctx, clients, spec.Startup, identity, &g.history)
		if err != nil {
			return sample, fmt.Errorf("control-plane baseline inspection incomplete: %w", err)
		}
	}
	if initial && !sample.Healthy {
		return sample, errors.New("initial control-plane gate is not clean and controller-ready")
	}
	if sample.NewOOMs > 0 && !g.observing {
		return sample, errors.New("new control-plane OOM")
	}
	for _, issue := range sample.Issues {
		if strings.Contains(issue, "mismatch") || strings.Contains(issue, "drift") || strings.HasPrefix(issue, "undeclared") {
			return sample, errors.New("frozen control-plane profile violated")
		}
	}
	g.summary.Samples++
	g.summary.LastObserved = sample.At
	g.summary.Restarts += sample.NewRestarts
	g.summary.OOMs += sample.NewOOMs
	g.summary.ProbeFailures += sample.NewProbeFailures
	if !sample.Healthy {
		g.summary.UnhealthySamples++
	}
	pods, err := clients.Kubernetes.CoreV1().Pods(identity.HCPNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return sample, fmt.Errorf("restart inspection failed: %w", err)
	}
	sets, err := clients.Kubernetes.AppsV1().ReplicaSets(identity.HCPNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return sample, fmt.Errorf("restart owner inspection failed: %w", err)
	}
	parents := map[types.UID]string{}
	for _, set := range sets.Items {
		for _, owner := range set.OwnerReferences {
			if owner.Controller != nil && *owner.Controller {
				parents[set.UID] = owner.Kind + "/" + owner.Name
			}
		}
	}
	if g.restarts == nil {
		g.restarts = map[string]int32{}
	}
	now := sample.At
	for _, pod := range pods.Items {
		ownerName := "Pod/" + pod.Name
		for _, owner := range pod.OwnerReferences {
			if owner.Controller != nil && *owner.Controller {
				ownerName = owner.Kind + "/" + owner.Name
				if owner.Kind == "ReplicaSet" {
					if parent, ok := parents[owner.UID]; ok {
						ownerName = parent
					} else {
						return sample, errors.New("restart owner unresolved")
					}
				}
			}
		}
		for _, group := range [][]corev1.ContainerStatus{pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses} {
			for _, status := range group {
				key := string(pod.UID) + "/" + status.Name
				if delta := status.RestartCount - g.restarts[key]; delta > 0 && !initial {
					g.events = append(g.events, restartEvent{now, ownerName, delta})
				}
				g.restarts[key] = status.RestartCount
			}
		}
	}
	counts := map[string]int32{}
	retained := g.events[:0]
	for _, event := range g.events {
		if now.Sub(event.at) <= 120*time.Second {
			retained = append(retained, event)
			counts[event.owner] += event.count
		}
	}
	g.events = retained
	if g.observing {
		return sample, g.ancillarySafety(ctx, clients, spec, identity.HCPNamespace, pods.Items, now)
	}
	for _, count := range counts {
		if count >= 3 {
			return sample, errors.New("three owner restarts within 120s")
		}
	}
	if sample.Healthy {
		g.unreadySince = time.Time{}
	} else if g.unreadySince.IsZero() {
		g.unreadySince = now
	}
	if !g.unreadySince.IsZero() && now.Sub(g.unreadySince) >= 2*time.Minute {
		return sample, errors.New("control-plane unhealthy for two minutes")
	}
	return sample, nil
}

// Existing registered CPU workers must already be suspended and drained. Do not
// adopt or stop another owner's run to manufacture a quiet baseline.
func noCompetitors(ctx context.Context, client kubernetes.Interface) error {
	jobs, err := client.BatchV1().Jobs("").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=hcp-perf"})
	if err != nil {
		return errors.New("competitor inventory unavailable")
	}
	for _, job := range jobs.Items {
		for _, container := range job.Spec.Template.Spec.Containers {
			if container.Name == "cpu-burn" && (job.Spec.Suspend == nil || !*job.Spec.Suspend || job.Status.Active != 0) {
				return errors.New("existing competitor not suspended")
			}
		}
	}
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=hcp-perf"})
	if err != nil {
		return errors.New("competitor pod inventory unavailable")
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			if container.Name == "cpu-burn" && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				return errors.New("existing competitor load remains")
			}
		}
	}
	return nil
}

func guestCapacity(ctx context.Context, client kubernetes.Interface, spec Spec) ([]nodeCapacity, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errors.New("guest node listing failed")
	}
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errors.New("guest capacity listing failed")
	}
	return capacity(nodes.Items, pods.Items, spec)
}

// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

type inventoryObject struct {
	Kind     string            `json:"kind"`
	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int32                 `json:"replicas"`
		Selector *metav1.LabelSelector  `json:"selector"`
		Template corev1.PodTemplateSpec `json:"template"`
		corev1.PodSpec
	} `json:"spec"`
	Status corev1.PodStatus `json:"status"`
}

type resourceComponent struct {
	object           inventoryObject
	containers       []corev1.Container
	usage            ComponentUsage
	scope            string
	selectorFallback bool
}

type podIdentity struct {
	object    inventoryObject
	component string
	observed  []float64
	runtimes  []runtimeObservation
}

type recordedContainerStatus struct {
	Name        string `json:"name"`
	ContainerID string `json:"containerID"`
	State       struct {
		State     string    `json:"state"`
		StartedAt time.Time `json:"startedAt"`
	} `json:"state"`
}

type runtimeObservation struct {
	at       float64
	statuses []recordedContainerStatus
}

type promSeries struct {
	Metric map[string]string    `json:"metric"`
	Values [][2]json.RawMessage `json:"values"`
}
type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string       `json:"resultType"`
		Result     []promSeries `json:"result"`
	} `json:"data"`
}
type sample struct{ evaluation, value float64 }
type interval struct{ start, end, value float64 }

func steadyContainers(spec corev1.PodSpec) []corev1.Container {
	out := append([]corev1.Container(nil), spec.Containers...)
	for _, c := range spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			out = append(out, c)
		}
	}
	return out
}

func resourceReferences(containers []corev1.Container, replicas int) ComponentUsage {
	u := ComponentUsage{Replicas: replicas, Containers: len(containers) * replicas}
	cpu, memory, cpuLimit, memoryLimit, goTarget := 0.0, 0.0, 0.0, 0.0, 0.0
	allCPU, allMemory, goCount := true, true, 0
	for _, c := range containers {
		cpu += c.Resources.Requests.Cpu().AsApproximateFloat64()
		memory += float64(c.Resources.Requests.Memory().Value()) / (1 << 20)
		if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			cpuLimit += q.AsApproximateFloat64()
		} else {
			allCPU = false
		}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			memoryLimit += float64(q.Value()) / (1 << 20)
		} else {
			allMemory = false
		}
		for _, env := range c.Env {
			if env.Name != "GOMEMLIMIT" || env.Value == "" || env.Value == "off" {
				continue
			}
			text := strings.TrimSuffix(env.Value, "B")
			q, err := resource.ParseQuantity(text)
			if err == nil && q.Sign() > 0 {
				goTarget += float64(q.Value()) / (1 << 20)
				goCount++
			}
		}
	}
	cpu *= float64(replicas)
	memory *= float64(replicas)
	cpuLimit *= float64(replicas)
	memoryLimit *= float64(replicas)
	goTarget *= float64(replicas)
	u.CPURequest, u.MemoryRequest = &cpu, &memory
	if allCPU {
		u.CPULimit = &cpuLimit
	}
	if allMemory {
		u.MemoryLimit = &memoryLimit
	}
	if goCount > 0 {
		u.GoTarget = &goTarget
	}
	if goCount != len(containers) {
		u.Notes = append(u.Notes, fmt.Sprintf("Partial Go target: explicit GOMEMLIMIT on %d/%d containers per replica; absent settings are not zero targets.", goCount, len(containers)))
	}
	if !allCPU || !allMemory {
		u.Notes = append(u.Notes, "At least one container has no finite limit; no aggregate full-component cap is claimed.")
	}
	return u
}

func matchingComponent(pod inventoryObject, components map[string]*resourceComponent, objects map[string]inventoryObject) (string, bool) {
	owner := metav1.GetControllerOf(&pod.Metadata)
	if owner != nil {
		if object, ok := objects[string(owner.UID)]; ok && object.Kind == owner.Kind && object.Metadata.Name == owner.Name {
			if object.Kind == "ReplicaSet" {
				parent := metav1.GetControllerOf(&object.Metadata)
				if parent != nil {
					if deployment, ok := objects[string(parent.UID)]; ok && deployment.Kind == "Deployment" && deployment.Metadata.Name == parent.Name {
						if _, ok := components[deployment.Metadata.Name]; ok {
							return deployment.Metadata.Name, false
						}
					}
				}
			} else if object.Kind == "StatefulSet" {
				if _, ok := components[object.Metadata.Name]; ok {
					return object.Metadata.Name, false
				}
			}
		}
	}
	// Missing ReplicaSet snapshots cannot be replaced by stripping Pod names.
	// A unique Deployment selector is a weaker, explicitly labeled association.
	if owner == nil || owner.Kind != "ReplicaSet" {
		return "", false
	}
	match := ""
	for name, component := range components {
		if component.object.Kind != "Deployment" || component.object.Spec.Selector == nil {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(component.object.Spec.Selector)
		if err == nil && !selector.Empty() && selector.Matches(labels.Set(pod.Metadata.Labels)) {
			if match != "" {
				return "", false
			}
			match = name
		}
	}
	return match, match != ""
}

func equalContainers(a, b []corev1.Container) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		found := false
		for _, y := range b {
			if x.Name != y.Name || x.Image != y.Image {
				continue
			}
			found = true
			for _, key := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
				for _, pair := range [][2]corev1.ResourceList{{x.Resources.Requests, y.Resources.Requests}, {x.Resources.Limits, y.Resources.Limits}} {
					xq, xok := pair[0][key]
					yq, yok := pair[1][key]
					if xok != yok || xq.Cmp(yq) != 0 {
						return false
					}
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func exportResources(reader *sourceReader, in RunInput, run *Run, start, end time.Time) ([]Component, error) {
	refs := in.Sources
	read := func(ref InputRef) ([]byte, error) {
		b, err := reader.read(in.Directory, ref)
		if err == nil {
			run.Sources = append(run.Sources, ref.ID)
		}
		return b, err
	}
	var objects = map[string]inventoryObject{}
	for _, ref := range refs.Inventories {
		b, err := read(ref)
		if err != nil {
			return nil, err
		}
		var inventory struct {
			Items []inventoryObject `json:"items"`
		}
		if err := json.Unmarshal(b, &inventory); err != nil {
			return nil, errors.New("invalid inventory")
		}
		for _, object := range inventory.Items {
			if object.Metadata.Namespace == in.HCPNamespace && object.Metadata.UID != "" {
				objects[string(object.Metadata.UID)] = object
			}
		}
	}
	// The frozen startup contract supplies role and expected images/resources;
	// inventories supply selectors and UIDs, never a name-prefix ownership guess.
	b, err := reader.read(in.Directory, refs.Spec)
	if err != nil {
		return nil, err
	}
	var spec struct {
		Startup struct {
			Components []struct {
				Name       string             `json:"name"`
				Kind       string             `json:"kind"`
				Scope      string             `json:"scope"`
				Replicas   int                `json:"replicas"`
				Containers []corev1.Container `json:"containers"`
			} `json:"components"`
		} `json:"startup"`
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, err
	}
	components := map[string]*resourceComponent{}
	for _, object := range objects {
		if object.Kind != "Deployment" && object.Kind != "StatefulSet" {
			continue
		}
		if object.Metadata.Name == "kube-state-metrics-hcp" || object.Spec.Replicas == nil || *object.Spec.Replicas == 0 {
			continue
		}
		containers := steadyContainers(object.Spec.Template.Spec)
		role := "downstream"
		for _, expected := range spec.Startup.Components {
			if expected.Name != object.Metadata.Name || expected.Kind != object.Kind {
				continue
			}
			if expected.Scope == "controlled" {
				role = "controlled"
			}
			// Native sidecar init containers are additional steady-state consumers.
			if !equalContainers(expected.Containers, object.Spec.Template.Spec.Containers) || expected.Replicas != int(*object.Spec.Replicas) {
				return nil, fmt.Errorf("inventory differs from frozen component %s", expected.Name)
			}
		}
		u := resourceReferences(containers, int(*object.Spec.Replicas))
		u.ComponentID = object.Metadata.Name
		components[object.Metadata.Name] = &resourceComponent{object: object, containers: containers, usage: u, scope: role}
	}
	if len(components) == 0 {
		return nil, errors.New("no declared HCP workload inventory")
	}
	for _, expected := range spec.Startup.Components {
		if (expected.Kind == "Deployment" || expected.Kind == "StatefulSet") && expected.Replicas > 0 && expected.Name != "kube-state-metrics-hcp" {
			if _, ok := components[expected.Name]; !ok {
				return nil, fmt.Errorf("missing expected workload inventory for %s", expected.Name)
			}
		}
	}
	componentNames := make([]string, 0, len(components))
	for name := range components {
		componentNames = append(componentNames, name)
	}
	sort.Strings(componentNames)
	pods := map[string]*podIdentity{}
	for _, object := range objects {
		if object.Kind != "Pod" {
			continue
		}
		component, fallback := matchingComponent(object, components, objects)
		if component == "" || !equalContainers(steadyContainers(object.Spec.PodSpec), components[component].containers) {
			continue
		}
		components[component].selectorFallback = components[component].selectorFallback || fallback
		pods[string(object.Metadata.UID)] = &podIdentity{object: object, component: component}
	}
	pendingPods := map[int64]map[string]runtimeObservation{}
	observePod := func(raw json.RawMessage) error {
		var page struct {
			Poll      int64     `json:"poll"`
			At        time.Time `json:"atTime"`
			Namespace string    `json:"namespace"`
			Type      string    `json:"type"`
			Entries   []struct {
				Metadata     metav1.ObjectMeta         `json:"metadata"`
				Statuses     []recordedContainerStatus `json:"containerStatuses"`
				InitStatuses []recordedContainerStatus `json:"initContainerStatuses"`
				Containers   []struct {
					Name     string              `json:"name"`
					Image    string              `json:"image"`
					Requests corev1.ResourceList `json:"requests"`
					Limits   corev1.ResourceList `json:"limits"`
				} `json:"containers"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		if page.Namespace != in.HCPNamespace {
			return nil
		}
		if page.Type == "complete" {
			for uid, observation := range pendingPods[page.Poll] {
				pods[uid].observed = append(pods[uid].observed, observation.at)
				pods[uid].runtimes = append(pods[uid].runtimes, observation)
			}
			delete(pendingPods, page.Poll)
			return nil
		}
		if page.Type != "page" {
			delete(pendingPods, page.Poll)
			return nil
		}
		if pendingPods[page.Poll] == nil {
			pendingPods[page.Poll] = map[string]runtimeObservation{}
		}
		for _, p := range page.Entries {
			identity := pods[string(p.Metadata.UID)]
			if identity == nil || identity.object.Metadata.Name != p.Metadata.Name {
				continue
			}
			containers := make([]corev1.Container, 0, len(p.Containers))
			for _, c := range p.Containers {
				containers = append(containers, corev1.Container{Name: c.Name, Image: c.Image, Resources: corev1.ResourceRequirements{Requests: c.Requests, Limits: c.Limits}})
			}
			if equalContainers(containers, identity.object.Spec.Containers) {
				pendingPods[page.Poll][string(p.Metadata.UID)] = runtimeObservation{at: float64(page.At.UnixNano()) / 1e9, statuses: append(p.Statuses, p.InitStatuses...)}
			}
		}
		return nil
	}
	podRefs := append([]InputRef(nil), refs.PodChunks...)
	if refs.Pods.Path != "" {
		podRefs = append(podRefs, refs.Pods)
	}
	if len(podRefs) == 0 || len(podRefs) > 64 {
		return nil, errors.New("require bounded declared Pod recorder inputs")
	}
	for _, ref := range podRefs {
		b, err = read(ref)
		if err != nil {
			return nil, err
		}
		if err := eachJSON(b, observePod); err != nil {
			return nil, err
		}
	}
	for _, p := range pods {
		sort.Float64s(p.observed)
		sort.Slice(p.runtimes, func(i, j int) bool { return p.runtimes[i].at < p.runtimes[j].at })
	}
	// Workload recorder inputs are declared and verified even though selectors
	// are sourced from full inventories rather than its sanitized status records.
	for _, ref := range []InputRef{refs.Deployments, refs.StatefulSets} {
		if _, err := read(ref); err != nil {
			return nil, err
		}
	}
	b, err = read(refs.MetricsManifest)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Start   time.Time     `json:"start"`
		Step    time.Duration `json:"stepNanoseconds"`
		Queries []struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
			Status string `json:"status"`
			File   string `json:"file"`
		} `json:"queries"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		return nil, err
	}
	step := manifest.Step.Seconds()
	if step <= 0 || step > 60 {
		return nil, errors.New("invalid metric export step")
	}
	from, to := float64(start.UnixNano())/1e9, float64(end.UnixNano())/1e9
	origin := float64(manifest.Start.UnixMilli()) / 1000
	grid := []float64{}
	for at := origin + math.Ceil((from-origin)/step)*step; at <= to; at += step {
		grid = append(grid, at)
	}
	if len(grid) > 100000 {
		return nil, errors.New("resource window too large")
	}
	type metricInput struct {
		name, metric string
		values       InputRef
		timestamps   *InputRef
		cpu          bool
	}
	inputs := []metricInput{{"cpu", "container_cpu_usage_seconds_total", refs.CPU, &refs.CPUTime, true}, {"memory", "container_memory_usage_bytes", refs.Memory, &refs.MemoryTime, false}, {"working-set", "container_memory_working_set_bytes", refs.Memory, refs.WorkingSetTime, false}, {"rss", "container_memory_rss", refs.Memory, refs.RSSTime, false}}
	var whole []MetricDistribution
	for _, input := range inputs {
		if input.timestamps == nil {
			whole = append(whole, MetricDistribution{ID: input.name, Label: input.name, Unit: "MiB", Excluded: len(grid), Note: "Unavailable: no independently timestamped source companion; no freshness claim."})
			continue
		}
		// Failed native range queries have no response file. Require both named
		// failures in the hash-verified manifest, never fabricate empty matrices.
		if input.values == (InputRef{}) && *input.timestamps == (InputRef{}) {
			query := "container-" + input.name
			for _, name := range []string{query, query + "-time"} {
				matches := 0
				for _, q := range manifest.Queries {
					if q.Name == name {
						if q.Status != "error" || q.SHA256 != "" || q.File != "" {
							return nil, fmt.Errorf("missing metric %s requires a failed query without a response", name)
						}
						matches++
					}
				}
				if matches != 1 {
					return nil, fmt.Errorf("missing metric %s requires exactly one failed query", name)
				}
			}
			note := "Unavailable: native range query failed without a response; no zero substitution or capture completeness claim."
			unit := "MiB"
			if input.cpu {
				unit = "cores"
			}
			whole = append(whole, MetricDistribution{ID: input.name, Label: "Whole HCP " + input.name, Unit: unit, Excluded: len(grid), Note: note})
			for _, component := range components {
				component.usage.Notes = append(component.usage.Notes, input.name+": "+note)
			}
			continue
		}
		for _, ref := range []InputRef{input.values, *input.timestamps} {
			matched := false
			for _, q := range manifest.Queries {
				if q.SHA256 == ref.SHA256 {
					matched = true
				}
			}
			if !matched {
				return nil, fmt.Errorf("metric source %s absent from declared metric manifest", ref.ID)
			}
		}
		values, err := decodeSource[promResponse](reader, in.Directory, input.values)
		if err != nil {
			return nil, err
		}
		times, err := decodeSource[promResponse](reader, in.Directory, *input.timestamps)
		if err != nil {
			return nil, err
		}
		run.Sources = append(run.Sources, input.values.ID, input.timestamps.ID)
		if values.Status != "success" || values.Data.ResultType != "matrix" || times.Status != "success" || times.Data.ResultType != "matrix" {
			return nil, errors.New("invalid Prometheus matrix response")
		}
		stamps := map[string][]sample{}
		for _, series := range times.Data.Result {
			if series.Metric["namespace"] != in.HCPNamespace {
				continue
			}
			x, err := parseSamples(series.Values)
			if err != nil {
				return nil, err
			}
			stamps[seriesKey(series.Metric)] = x
		}
		// Each grid cell contains distinct Pod UID/container identities. Duplicate
		// runtime series invalidate the cell instead of double-counting a restart.
		cells := map[string][]map[string]float64{}
		for name := range components {
			cells[name] = make([]map[string]float64, len(grid))
		}
		for _, series := range values.Data.Result {
			m := series.Metric
			if m["namespace"] != in.HCPNamespace || m["__name__"] != input.metric || (input.cpu && m["cpu"] != "total" && m["cpu"] != "") {
				continue
			}
			identity := metricPod(m, pods)
			if identity == nil {
				continue
			}
			x, err := parseSamples(series.Values)
			if err != nil {
				return nil, err
			}
			paired := stamps[seriesKey(m)]
			var intervals []interval
			if input.cpu {
				intervals = cpuIntervals(x, paired, from, to)
			}
			for i, at := range grid {
				if !observedNear(identity.observed, at) {
					continue
				}
				intervalStart := at
				if input.cpu {
					intervalStart = at - step
				}
				if !runtimeMatches(identity, m, intervalStart, at) {
					continue
				}
				value, ok := 0.0, false
				if input.cpu {
					value, ok = integrated(intervals, at-step, at)
				} else {
					value, ok = freshGauge(x, paired, at, from, to)
				}
				if !ok {
					continue
				}
				if !input.cpu {
					value /= 1 << 20
				}
				cell := cells[identity.component][i]
				if cell == nil {
					cell = map[string]float64{}
					cells[identity.component][i] = cell
				}
				key := string(identity.object.Metadata.UID) + "/" + m["container"]
				if _, duplicate := cell[key]; duplicate {
					cell[key] = math.NaN()
				} else {
					cell[key] = value
				}
			}
		}
		all := make([]float64, 0, len(grid))
		totalContainers := 0
		for _, component := range components {
			totalContainers += component.usage.Containers
		}
		for _, name := range componentNames {
			component := components[name]
			var sums []float64
			for _, cell := range cells[name] {
				if sum, ok := componentSum(cell, component); ok {
					sums = append(sums, sum)
				}
			}
			d := distribution(sums, len(grid))
			switch input.name {
			case "cpu":
				component.usage.CPU = d
			case "memory":
				component.usage.Memory = d
			case "working-set":
				component.usage.WorkingSet = d
			case "rss":
				component.usage.RSS = d
			}
			component.usage.Notes = append(component.usage.Notes, fmt.Sprintf("%s: %d/%d complete grid opportunities; %d missing/excluded.", input.name, len(sums), len(grid), len(grid)-len(sums)))
		}
		for i := range grid {
			sum, complete := 0.0, true
			for _, name := range componentNames {
				c := components[name]
				v, ok := componentSum(cells[name][i], c)
				sum += v
				complete = complete && ok
			}
			if complete {
				all = append(all, sum)
			}
		}
		unit := "MiB"
		if input.cpu {
			unit = "cores"
		}
		whole = append(whole, MetricDistribution{ID: input.name, Label: "Whole HCP " + input.name, Unit: unit, Distribution: distribution(all, len(grid)), Excluded: len(grid) - len(all), Note: fmt.Sprintf("Complete aligned totals require all %d steady containers across %d workloads; no sum of percentiles. %d/%d grid opportunities covered.", totalContainers, len(components), len(all), len(grid))})
	}
	run.WholeHCP = whole
	var output []Component
	for _, name := range componentNames {
		c := components[name]
		method := "Pod UID -> controller owner UID chain from full inventory."
		if c.selectorFallback {
			method = "Exact Pod UID/cgroup identity with unique Deployment selector and matching container images/resources; ReplicaSet owner chain not fully captured."
		}
		if c.usage.CPU == nil || c.usage.Memory == nil {
			run.Notes = append(run.Notes, "Incomplete resource attribution/coverage for "+name+"; absent distributions and whole-HCP totals are not zero utilization.")
			c.usage.Notes = append(c.usage.Notes, "Runtime attribution requires an exact container ID plus consistent recorder start times. A recorder-observed replacement without a saved replacement container ID cannot inherit the older inventory ID.")
		}
		c.usage.Notes = append(c.usage.Notes, method, "Recorder confirms Pod UID/container resources near each grid observation; historical runtime identities not represented by the inventory are excluded.", "RSS/working-set require their own source timestamps; absent companions are unavailable.")
		run.Resources = append(run.Resources, c.usage)
		label := name
		if alias := map[string]string{"kube-apiserver": "KAS", "openshift-apiserver": "OAS", "kube-controller-manager": "KCM", "control-plane-operator": "CPO"}[name]; alias != "" {
			label = alias + " (" + name + ")"
		}
		output = append(output, Component{ID: name, Name: label, Scope: c.scope})
	}
	sort.Slice(run.Resources, func(i, j int) bool { return run.Resources[i].ComponentID < run.Resources[j].ComponentID })
	run.Notes = append(run.Notes, "Resource inventory excludes zero-replica workloads, completed Jobs and non-restartable init containers; native restartable init sidecars are included. Kube-state-metrics instrumentation is excluded. Unmapped lifecycle identities remain missing.")
	// Keep citations unique without exposing any source path.
	sort.Strings(run.Sources)
	unique := run.Sources[:0]
	for _, id := range run.Sources {
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	run.Sources = unique
	return output, nil
}

func seriesKey(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "__name__" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
		b.WriteByte(0)
	}
	return b.String()
}

func parseSamples(raw [][2]json.RawMessage) ([]sample, error) {
	out := make([]sample, 0, len(raw))
	for _, pair := range raw {
		var at float64
		var text string
		if json.Unmarshal(pair[0], &at) != nil || json.Unmarshal(pair[1], &text) != nil {
			return nil, errors.New("invalid metric sample")
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, errors.New("invalid metric number")
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		if len(out) > 0 && at <= out[len(out)-1].evaluation {
			return nil, errors.New("unordered metric evaluations")
		}
		out = append(out, sample{at, value})
	}
	return out, nil
}

func metricPod(m map[string]string, pods map[string]*podIdentity) *podIdentity {
	for uid, p := range pods {
		if p.object.Metadata.Name != m["pod"] || !strings.Contains(strings.ReplaceAll(m["id"], "_", "-"), "pod"+uid) {
			continue
		}
		known := false
		for _, c := range steadyContainers(p.object.Spec.PodSpec) {
			if c.Name == m["container"] && c.Image == m["image"] {
				known = true
			}
		}
		if !known {
			return nil
		}
		for _, observation := range p.runtimes {
			for _, status := range observation.statuses {
				if status.Name == m["container"] && runtimeID(status.ContainerID) == m["name"] && m["name"] != "" {
					return p
				}
			}
		}
		for _, status := range append(append([]corev1.ContainerStatus(nil), p.object.Status.ContainerStatuses...), p.object.Status.InitContainerStatuses...) {
			if status.Name == m["container"] && status.ContainerID != "" {
				parts := strings.SplitN(status.ContainerID, "://", 2)
				if len(parts) != 2 || m["name"] != parts[1] {
					return nil
				}
			}
		}
		return p
	}
	return nil
}

func runtimeID(id string) string {
	parts := strings.SplitN(id, "://", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

// A full inventory is only an identity baseline. Select the latest completed
// recorder snapshot at or before the interval, never a future replacement.
func runtimeMatches(p *podIdentity, metric map[string]string, from, to float64) bool {
	i := sort.Search(len(p.runtimes), func(i int) bool { return p.runtimes[i].at > from }) - 1
	if i < 0 || from-p.runtimes[i].at > 60 {
		return false
	}
	var started time.Time
	for ; i < len(p.runtimes) && p.runtimes[i].at <= to; i++ {
		found := false
		for _, status := range p.runtimes[i].statuses {
			if status.Name != metric["container"] {
				continue
			}
			if status.State.State != "running" || status.State.StartedAt.IsZero() || float64(status.State.StartedAt.UnixNano())/1e9 > from {
				return false
			}
			if !started.IsZero() && !started.Equal(status.State.StartedAt) {
				return false
			}
			started = status.State.StartedAt
			id := runtimeID(status.ContainerID)
			if id == "" {
				for _, baseline := range append(append([]corev1.ContainerStatus(nil), p.object.Status.ContainerStatuses...), p.object.Status.InitContainerStatuses...) {
					if baseline.Name == status.Name && baseline.State.Running != nil && baseline.State.Running.StartedAt.Time.Equal(started) {
						id = runtimeID(baseline.ContainerID)
					}
				}
			}
			if id == "" || id != metric["name"] {
				return false
			}
			found = true
		}
		if !found {
			return false
		}
	}
	return true
}

func observedNear(observed []float64, at float64) bool {
	i := sort.SearchFloat64s(observed, at)
	return (i < len(observed) && observed[i]-at <= 60) || (i > 0 && at-observed[i-1] <= 60)
}

func cpuIntervals(values, times []sample, from, to float64) []interval {
	stamps := map[float64]float64{}
	for _, s := range times {
		stamps[s.evaluation] = s.value
	}
	var out []interval
	var previous sample
	have := false
	for _, s := range values {
		at, ok := stamps[s.evaluation]
		if !ok || at <= 0 || at > s.evaluation || s.evaluation-at > 45 || at < from || at > to || s.value < 0 {
			continue
		}
		if have && at == previous.evaluation {
			continue
		}
		if have && at > previous.evaluation && at-previous.evaluation <= 45 && s.value >= previous.value {
			out = append(out, interval{previous.evaluation, at, (s.value - previous.value) / (at - previous.evaluation)})
		}
		previous = sample{at, s.value}
		have = true
	}
	return out
}

func integrated(intervals []interval, from, to float64) (float64, bool) {
	if to <= from {
		return 0, false
	}
	covered, total := from, 0.0
	for _, v := range intervals {
		if v.end <= covered {
			continue
		}
		if v.start > covered+1e-6 {
			return 0, false
		}
		stop := math.Min(to, v.end)
		total += (stop - covered) * v.value
		covered = stop
		if covered >= to-1e-6 {
			return total / (to - from), true
		}
	}
	return 0, false
}

func freshGauge(values, times []sample, at, from, to float64) (float64, bool) {
	i := sort.Search(len(values), func(i int) bool { return values[i].evaluation >= at-1e-6 })
	j := sort.Search(len(times), func(i int) bool { return times[i].evaluation >= at-1e-6 })
	if i == len(values) || j == len(times) || math.Abs(values[i].evaluation-at) > 1e-5 || math.Abs(times[j].evaluation-at) > 1e-5 {
		return 0, false
	}
	source := times[j].value
	if source <= 0 || source < from || source > to || source > at || at-source > 45 || values[i].value < 0 {
		return 0, false
	}
	return values[i].value, true
}

func completeSum(cell map[string]float64, expected int) (float64, bool) {
	if expected == 0 || len(cell) != expected {
		return 0, false
	}
	sum := 0.0
	keys := make([]string, 0, len(cell))
	for key := range cell {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := cell[key]
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		sum += v
	}
	return sum, true
}

func componentSum(cell map[string]float64, component *resourceComponent) (float64, bool) {
	counts := map[string]int{}
	podCounts := map[string]int{}
	for key := range cell {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 {
			return 0, false
		}
		counts[parts[1]]++
		podCounts[parts[0]]++
	}
	if len(podCounts) != component.usage.Replicas {
		return 0, false
	}
	for _, count := range podCounts {
		if count != len(component.containers) {
			return 0, false
		}
	}
	for _, container := range component.containers {
		if counts[container.Name] != component.usage.Replicas {
			return 0, false
		}
	}
	return completeSum(cell, component.usage.Containers)
}

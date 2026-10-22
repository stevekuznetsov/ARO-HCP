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

package adversary

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	podresource "k8s.io/component-helpers/resource"
)

const (
	managedByLabel    = "app.kubernetes.io/managed-by"
	runIDLabel        = "hcp-perf.azure.com/run-id"
	digestAnnotation  = "hcp-perf.azure.com/config-digest"
	stoppedAnnotation = "hcp-perf.azure.com/stopped"
	workerRoleLabel   = "aro-hcp.azure.com/role"
	maxJobEvents      = 20
)

var pinnedImage = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)

// RunSpec contains execution bounds, never a submitted node snapshot.
// For Start, Namespace must exist with app.kubernetes.io/managed-by=hcp-perf.
// An omitted reserve fills 90% of each node's free CPU request headroom by
// default. Memory defaults (32/128 MiB) are policy knobs, not discovered process
// memory requirements. Actual CPU saturation must be verified separately.
type RunSpec struct {
	Version         string `json:"version"`
	ID              string `json:"id"`
	Namespace       string `json:"namespace"`
	Image           string `json:"image"`
	TargetNamespace string `json:"targetNamespace"`
	ReserveCPUMilli *int64 `json:"reserveCPUMilli,omitempty"`
	CPUFillPercent  int32  `json:"cpuFillPercent,omitempty"`
	MemoryRequestMi int64  `json:"memoryRequestMi,omitempty"`
	MemoryLimitMi   int64  `json:"memoryLimitMi,omitempty"`
	DurationSeconds int64  `json:"durationSeconds"`
}

// RunStatus reports submission and Kubernetes observations, not CPU usage or
// measured contention. Missing expected Jobs never count as successful execution.
type RunStatus struct {
	RegistrationUID types.UID   `json:"registrationUID,omitempty"`
	Spec            RunSpec     `json:"spec"`
	Snapshot        Spec        `json:"snapshot"`
	Plan            Result      `json:"plan"`
	Registration    string      `json:"registration"`
	Phase           string      `json:"phase"`
	Validity        string      `json:"validity"`
	Jobs            []JobStatus `json:"jobs"`
	StopRequested   bool        `json:"stopRequested"`
	// Missing Jobs leave submission unresolved, even after an interrupted Start.
	SubmissionIncomplete bool              `json:"submissionIncomplete"`
	NonterminalPods      int               `json:"nonterminalPods"`
	PlacementValid       bool              `json:"placementValid"`
	PlacementValidity    string            `json:"placementValidity"`
	Drift                []string          `json:"drift,omitempty"`
	ObservedTargetPods   map[string]string `json:"observedTargetPods,omitempty"`
}

type JobStatus struct {
	Deleting   bool                   `json:"deleting,omitempty"`
	Name       string                 `json:"name"`
	Node       string                 `json:"node"`
	UID        types.UID              `json:"uid,omitempty"`
	Phase      string                 `json:"phase"`
	Conditions []batchv1.JobCondition `json:"conditions,omitempty"`
	Pods       []PodStatus            `json:"pods,omitempty"`
	// Events include admission FailedCreate diagnostics when no pod exists.
	Events          []corev1.Event `json:"events,omitempty"`
	EventsTruncated bool           `json:"eventsTruncated,omitempty"`
}

// Container states include waiting/termination reasons, messages, and exit codes.
type PodStatus struct {
	UID            types.UID                `json:"uid,omitempty"`
	Name           string                   `json:"name"`
	Node           string                   `json:"node"`
	Phase          corev1.PodPhase          `json:"phase"`
	Reason         string                   `json:"reason,omitempty"`
	Message        string                   `json:"message,omitempty"`
	Ready          bool                     `json:"ready"`
	Deleting       bool                     `json:"deleting"`
	Conditions     []corev1.PodCondition    `json:"conditions,omitempty"`
	Containers     []corev1.ContainerStatus `json:"containers,omitempty"`
	InitContainers []corev1.ContainerStatus `json:"initContainers,omitempty"`
}

type registration struct {
	Spec     RunSpec `json:"spec"`
	Snapshot Spec    `json:"snapshot"`
	Plan     Result  `json:"plan"`
}

func validateRun(spec RunSpec) error {
	if spec.Version != "v1" {
		return fmt.Errorf("version must be v1")
	}
	// Leave room for a node hash and the Job controller's pod-name suffix.
	if len(spec.ID) > 40 || len(validation.IsDNS1123Label(spec.ID)) != 0 {
		return fmt.Errorf("id must be a DNS label of at most 40 characters")
	}
	if len(validation.IsDNS1123Label(spec.Namespace)) != 0 || len(validation.IsDNS1123Label(spec.TargetNamespace)) != 0 || spec.Namespace == spec.TargetNamespace {
		return fmt.Errorf("namespace and targetNamespace must be distinct, explicit DNS labels")
	}
	if !pinnedImage.MatchString(spec.Image) {
		return fmt.Errorf("image must be pinned with @sha256: followed by 64 lowercase hex characters")
	}
	if spec.DurationSeconds < 1 || spec.DurationSeconds > 86400 || (spec.ReserveCPUMilli != nil && *spec.ReserveCPUMilli < 0) {
		return fmt.Errorf("durationSeconds must be 1..86400 and reserveCPUMilli must be nonnegative")
	}
	if spec.CPUFillPercent < 0 || spec.CPUFillPercent > 100 {
		return fmt.Errorf("cpuFillPercent must be 1..100 when specified")
	}
	if spec.ReserveCPUMilli != nil && spec.CPUFillPercent != 0 {
		return fmt.Errorf("reserveCPUMilli and cpuFillPercent are mutually exclusive")
	}
	if spec.MemoryRequestMi <= 0 || spec.MemoryLimitMi < spec.MemoryRequestMi || spec.MemoryLimitMi > math.MaxInt64/(1024*1024) {
		return fmt.Errorf("memory request/limit must be positive, ordered, and representable in bytes")
	}
	return nil
}

// Validate checks explicit execution bounds without discovery or API calls.
func (spec RunSpec) Validate() error { return validateRun(spec) }

func ownedNamespace(ctx context.Context, client kubernetes.Interface, namespace string) error {
	if len(validation.IsDNS1123Label(namespace)) != 0 {
		return fmt.Errorf("invalid namespace %q", namespace)
	}
	ns, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if ns.Labels[managedByLabel] != "hcp-perf" || ns.DeletionTimestamp != nil {
		return fmt.Errorf("namespace %q must be live and managed by hcp-perf", namespace)
	}
	return nil
}

// Discover reads live placement and scheduler requests without creating or
// modifying any resources. Its result is a point-in-time plan, not a reservation
// or proof of saturation. The execution namespace need not exist for discovery.
func Discover(ctx context.Context, client kubernetes.Interface, spec RunSpec) (RunStatus, error) {
	if spec.MemoryRequestMi == 0 {
		spec.MemoryRequestMi = 32
	}
	if spec.MemoryLimitMi == 0 {
		spec.MemoryLimitMi = 128
	}
	if spec.ReserveCPUMilli == nil && spec.CPUFillPercent == 0 {
		spec.CPUFillPercent = 90
	}
	if err := validateRun(spec); err != nil {
		return RunStatus{}, err
	}
	pods, err := client.CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return RunStatus{}, fmt.Errorf("discover pods: %w", err)
	}
	targets := map[string]bool{}
	committed := map[string]int64{}
	memory := map[string]int64{}
	podCounts := map[string]int64{}
	targetPods := map[string]string{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if pod.Namespace == spec.TargetNamespace {
			if pod.Spec.NodeName == "" {
				return RunStatus{}, fmt.Errorf("target pod %s/%s is unscheduled", pod.Namespace, pod.Name)
			}
			if pod.UID == "" {
				return RunStatus{}, fmt.Errorf("target pod %s/%s has no UID", pod.Namespace, pod.Name)
			}
			targets[pod.Spec.NodeName] = true
			targetPods[string(pod.UID)] = pod.Spec.NodeName
		}
		if pod.Spec.NodeName == "" {
			continue
		}
		// Status resources are resize allocations, not observed CPU usage.
		requests := podresource.PodRequests(pod, podresource.PodResourcesOptions{UseStatusResources: true})
		cpu := requests.Cpu().MilliValue()
		if cpu < 0 || committed[pod.Spec.NodeName] > math.MaxInt64-cpu {
			return RunStatus{}, fmt.Errorf("invalid or overflowing CPU commitments on %q", pod.Spec.NodeName)
		}
		committed[pod.Spec.NodeName] += cpu
		bytes := requests.Memory().Value()
		if bytes < 0 || memory[pod.Spec.NodeName] > math.MaxInt64-bytes {
			return RunStatus{}, fmt.Errorf("invalid or overflowing memory commitments on %q", pod.Spec.NodeName)
		}
		memory[pod.Spec.NodeName] += bytes
		podCounts[pod.Spec.NodeName]++
	}
	if len(targets) == 0 {
		return RunStatus{}, fmt.Errorf("target namespace %q has no live pods", spec.TargetNamespace)
	}
	snapshot := Spec{Version: spec.Version, CollectedAt: time.Now().UTC(), DurationSeconds: spec.DurationSeconds, MemoryRequestMi: spec.MemoryRequestMi, MemoryLimitMi: spec.MemoryLimitMi, CPUFillPercent: spec.CPUFillPercent, TargetPods: targetPods}
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		node, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return RunStatus{}, fmt.Errorf("discover node %q: %w", name, err)
		}
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				ready = condition.Status == corev1.ConditionTrue
			}
			if (condition.Type == corev1.NodeMemoryPressure || condition.Type == corev1.NodeDiskPressure || condition.Type == corev1.NodePIDPressure) && condition.Status == corev1.ConditionTrue {
				return RunStatus{}, fmt.Errorf("node %q has %s", name, condition.Type)
			}
		}
		if !ready || node.Spec.Unschedulable || node.DeletionTimestamp != nil {
			return RunStatus{}, fmt.Errorf("node %q must be Ready and schedulable", name)
		}
		if node.UID == "" {
			return RunStatus{}, fmt.Errorf("node %q has no UID", name)
		}
		// Competitors deliberately have no tolerations. Do not bypass taints.
		for _, taint := range node.Spec.Taints {
			if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
				return RunStatus{}, fmt.Errorf("node %q has untolerated taint %s=%s:%s", name, taint.Key, taint.Value, taint.Effect)
			}
		}
		allocatable := node.Status.Allocatable.Cpu().MilliValue()
		if allocatable <= 0 {
			return RunStatus{}, fmt.Errorf("node %q: allocatable CPU must be known and positive", name)
		}
		available := allocatable - committed[name]
		reserve := int64(0)
		if spec.ReserveCPUMilli != nil {
			reserve = *spec.ReserveCPUMilli
		} else if available > 0 {
			// ceil(headroom * (100-fill) / 100), without overflowing int64.
			unfilled := int64(100 - spec.CPUFillPercent)
			reserve = available/100*unfilled + (available%100*unfilled+99)/100
		}
		allocatableMemory := node.Status.Allocatable.Memory().Value()
		if allocatableMemory <= 0 || memory[name] > allocatableMemory || allocatableMemory-memory[name] < spec.MemoryRequestMi*1024*1024 {
			return RunStatus{}, fmt.Errorf("node %q: insufficient or unknown memory request headroom", name)
		}
		allocatablePods := node.Status.Allocatable.Pods().Value()
		if allocatablePods <= podCounts[name] {
			return RunStatus{}, fmt.Errorf("node %q: insufficient or unknown allocatable pod slots", name)
		}
		snapshot.Nodes = append(snapshot.Nodes, Node{
			Name: name, Pool: node.Labels["kubernetes.azure.com/agentpool"], Role: node.Labels[workerRoleLabel], HostsTarget: true,
			CapacityCPUMilli: node.Status.Capacity.Cpu().MilliValue(), AllocatableCPUMilli: node.Status.Allocatable.Cpu().MilliValue(),
			CommittedCPUMilli: committed[name], ReserveCPUMilli: reserve,
			UID: string(node.UID), AllocatableMemoryBytes: allocatableMemory, CommittedMemoryBytes: memory[name],
			AllocatablePods: allocatablePods, CommittedPods: podCounts[name],
		})
	}
	plan, err := Plan(snapshot)
	if err != nil {
		return RunStatus{}, err
	}
	for _, node := range plan.Nodes {
		if node.Workers > 1024 {
			return RunStatus{}, fmt.Errorf("node %q needs %d workers, exceeding burner bound 1024", node.Name, node.Workers)
		}
	}
	record := registration{Spec: spec, Snapshot: snapshot, Plan: plan}
	status := record.status("")
	status.Phase, status.SubmissionIncomplete = "discovered", false
	status.PlacementValid, status.PlacementValidity = true, "discovery-snapshot-only"
	status.Jobs = nil
	return status, nil
}

// Start uses the same discovery as preview, then registers the immutable plan
// before creating Jobs. A collision never creates more Jobs. On partial creation
// failure the registration and created Jobs are retained for inspection or Stop.
func Start(ctx context.Context, client kubernetes.Interface, spec RunSpec) (RunStatus, error) {
	discovered, err := Discover(ctx, client, spec)
	if err != nil {
		return RunStatus{}, err
	}
	if err := ownedNamespace(ctx, client, spec.Namespace); err != nil {
		return RunStatus{}, err
	}
	spec, plan := discovered.Spec, discovered.Plan
	record := registration{Spec: spec, Snapshot: discovered.Snapshot, Plan: plan}
	data, err := json.Marshal(record)
	if err != nil {
		return RunStatus{}, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	immutable := true
	cm, err := client.CoreV1().ConfigMaps(spec.Namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "adversaries-" + spec.ID, Namespace: spec.Namespace, Labels: runLabels(spec.ID), Annotations: map[string]string{digestAnnotation: digest}},
		Immutable:  &immutable, Data: map[string]string{"run.json": string(data)},
	}, metav1.CreateOptions{})
	if err != nil {
		return RunStatus{}, fmt.Errorf("register run (IDs are create-only): %w", err)
	}
	status := record.status(cm.Name)
	status.RegistrationUID = cm.UID
	status.Phase = "submitted"
	for i := 0; i <= len(plan.Nodes); i++ {
		// This check cannot fence an in-flight create. Stop therefore never
		// acknowledges stopped while any expected Job is missing. The final
		// check also suspends a Job whose create raced with the stop intent.
		current, _, err := loadRegistration(ctx, client, spec.Namespace, spec.ID)
		if err != nil {
			status.Phase = "partial"
			return status, err
		}
		if current.UID != cm.UID || current.Annotations[digestAnnotation] != digest {
			status.Phase = "partial"
			return status, fmt.Errorf("registration replaced during submission")
		}
		if current.Annotations[stoppedAnnotation] == "true" {
			stopped, err := Stop(ctx, client, spec.Namespace, spec.ID)
			return stopped, errors.Join(fmt.Errorf("submission interrupted by stop request"), err)
		}
		if i == len(plan.Nodes) {
			break
		}
		node := plan.Nodes[i]
		job := desiredJob(cm, record, node)
		created, err := client.BatchV1().Jobs(spec.Namespace).Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
			status.Phase = "partial"
			status.Jobs[i].Phase = "creation-failed"
			return status, fmt.Errorf("create job %q (registration and previous Jobs retained): %w", job.Name, err)
		}
		status.Jobs[i].UID, status.Jobs[i].Phase = created.UID, "submitted"
		status.SubmissionIncomplete = i+1 != len(plan.Nodes)
	}
	return status, nil
}

func runLabels(id string) map[string]string {
	return map[string]string{managedByLabel: "hcp-perf", runIDLabel: id}
}

func jobName(id, node string) string {
	digest := sha256.Sum256([]byte(node))
	return fmt.Sprintf("%s-%x", id, digest[:8])
}

func (r registration) status(name string) RunStatus {
	s := RunStatus{Spec: r.Spec, Snapshot: r.Snapshot, Plan: r.Plan, Registration: name, SubmissionIncomplete: true, Validity: "kubernetes-observations-only; CPU contention is not measured; memory headroom is request-based, not RSS"}
	s.PlacementValidity = "unverified-legacy"
	if len(r.Snapshot.TargetPods) != 0 {
		s.PlacementValidity = "not-checked"
	}
	for _, node := range r.Plan.Nodes {
		s.Jobs = append(s.Jobs, JobStatus{Name: jobName(r.Spec.ID, node.Name), Node: node.Name, Phase: "missing"})
	}
	return s
}

func desiredJob(cm *corev1.ConfigMap, r registration, node NodePlan) *batchv1.Job {
	zero, one := int32(0), int32(1)
	deadline, uid := node.DeadlineSeconds+60, int64(65532)
	no, yes := false, true
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName(r.Spec.ID, node.Name), Namespace: r.Spec.Namespace, Labels: runLabels(r.Spec.ID),
			Annotations:     map[string]string{digestAnnotation: cm.Annotations[digestAnnotation]},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: cm.Name, UID: cm.UID, Controller: &yes}},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &zero, Completions: &one, Parallelism: &one, ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: runLabels(r.Spec.ID), Annotations: map[string]string{digestAnnotation: cm.Annotations[digestAnnotation]}},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: &no,
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchFields:      []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node.Name}}},
						MatchExpressions: []corev1.NodeSelectorRequirement{{Key: workerRoleLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{"worker"}}},
					}}}}},
					Containers: []corev1.Container{{
						Name: "cpu-burn", Image: r.Spec.Image, Command: []string{"/hcp-perf"},
						Args: []string{"cpu", "burn", "--workers", strconv.FormatInt(node.Workers, 10), "--duration", strconv.FormatInt(node.DeadlineSeconds, 10) + "s"},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(node.CPURequestMilli, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(node.MemoryRequestMi*1024*1024, resource.BinarySI)},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(node.MemoryLimitMi*1024*1024, resource.BinarySI)},
						},
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					}},
				},
			},
		},
	}
}

func loadRegistration(ctx context.Context, client kubernetes.Interface, namespace, id string) (*corev1.ConfigMap, registration, error) {
	var r registration
	if len(id) > 40 || len(validation.IsDNS1123Label(id)) != 0 {
		return nil, r, fmt.Errorf("invalid run ID")
	}
	if err := ownedNamespace(ctx, client, namespace); err != nil {
		return nil, r, err
	}
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, "adversaries-"+id, metav1.GetOptions{})
	if err != nil {
		return nil, r, err
	}
	if cm.Labels[managedByLabel] != "hcp-perf" || cm.Labels[runIDLabel] != id || cm.DeletionTimestamp != nil || cm.Immutable == nil || !*cm.Immutable {
		return nil, r, fmt.Errorf("registration %q is not owned by this run", cm.Name)
	}
	data := []byte(cm.Data["run.json"])
	if fmt.Sprintf("%x", sha256.Sum256(data)) != cm.Annotations[digestAnnotation] {
		return nil, r, fmt.Errorf("registration digest mismatch")
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, r, err
	}
	if err := validateRun(r.Spec); err != nil {
		return nil, r, err
	}
	plan, err := Plan(r.Snapshot)
	if err != nil || !reflect.DeepEqual(plan, r.Plan) || r.Spec.ID != id || r.Spec.Namespace != namespace {
		return nil, r, fmt.Errorf("registration identity or resolved plan mismatch")
	}
	return cm, r, nil
}

func ownedJob(job *batchv1.Job, cm *corev1.ConfigMap, r registration, node NodePlan) error {
	owner := metav1.GetControllerOf(job)
	if job.Namespace != r.Spec.Namespace || job.Name != jobName(r.Spec.ID, node.Name) || job.Labels[managedByLabel] != "hcp-perf" || job.Labels[runIDLabel] != r.Spec.ID || job.Annotations[digestAnnotation] != cm.Annotations[digestAnnotation] || owner == nil || owner.APIVersion != "v1" || owner.Kind != "ConfigMap" || owner.Name != cm.Name || owner.UID != cm.UID {
		return fmt.Errorf("refusing unowned or replaced job %q", job.Name)
	}
	return nil
}

// observePlacement never retunes the registered plan. Read failures are reported
// as unverified observations rather than preventing Stop from checking cleanup.
func (s *RunStatus) observePlacement(ctx context.Context, client kubernetes.Interface) {
	if len(s.Snapshot.TargetPods) == 0 {
		s.PlacementValidity = "unverified-legacy"
		return
	}
	for _, node := range s.Snapshot.Nodes {
		if node.UID == "" {
			s.PlacementValidity = "unverified-legacy"
			return
		}
	}
	s.PlacementValidity = "valid"
	pods, err := client.CoreV1().Pods(s.Spec.TargetNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		s.PlacementValidity = "unverified"
		s.Drift = append(s.Drift, fmt.Sprintf("cannot observe target pods: %v", err))
	} else {
		s.ObservedTargetPods = map[string]string{}
		nodes := map[string]bool{}
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			s.ObservedTargetPods[string(pod.UID)] = pod.Spec.NodeName
			if pod.Spec.NodeName == "" {
				s.Drift = append(s.Drift, fmt.Sprintf("target pod %s/%s is unscheduled", pod.Namespace, pod.Name))
			} else {
				nodes[pod.Spec.NodeName] = true
			}
			if previous, ok := s.Snapshot.TargetPods[string(pod.UID)]; !ok {
				s.Drift = append(s.Drift, fmt.Sprintf("new target pod %s/%s (UID %s) on node %q", pod.Namespace, pod.Name, pod.UID, pod.Spec.NodeName))
			} else if previous != pod.Spec.NodeName {
				s.Drift = append(s.Drift, fmt.Sprintf("target pod UID %s moved from %q to %q", pod.UID, previous, pod.Spec.NodeName))
			}
		}
		for uid, node := range s.Snapshot.TargetPods {
			if _, ok := s.ObservedTargetPods[uid]; !ok {
				s.Drift = append(s.Drift, fmt.Sprintf("target pod UID %s on %q is no longer live", uid, node))
			}
		}
		for _, node := range s.Snapshot.Nodes {
			if !nodes[node.Name] {
				s.Drift = append(s.Drift, fmt.Sprintf("node %q no longer hosts live target pods", node.Name))
			}
			delete(nodes, node.Name)
		}
		for name := range nodes {
			s.Drift = append(s.Drift, fmt.Sprintf("target now occupies unplanned node %q", name))
		}
	}
	for _, expected := range s.Snapshot.Nodes {
		node, err := client.CoreV1().Nodes().Get(ctx, expected.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			s.Drift = append(s.Drift, fmt.Sprintf("node %q was removed", expected.Name))
		case err != nil:
			s.PlacementValidity = "unverified"
			s.Drift = append(s.Drift, fmt.Sprintf("cannot observe node %q: %v", expected.Name, err))
		case string(node.UID) != expected.UID:
			s.Drift = append(s.Drift, fmt.Sprintf("node %q UID replaced: %s -> %s", expected.Name, expected.UID, node.UID))
		case node.Labels[workerRoleLabel] != "worker":
			s.Drift = append(s.Drift, fmt.Sprintf("node %q is no longer a worker", expected.Name))
		}
	}
	sort.Strings(s.Drift)
	s.PlacementValid = len(s.Drift) == 0
	if !s.PlacementValid && s.PlacementValidity == "valid" {
		s.PlacementValidity = "drifted"
	}
}

// Status reads the registered plan and every expected Job, including missing Jobs.
// Placement drift is reported independently of execution Phase and never mutates
// the run. Legacy registrations without placement identities remain unverified.
func Status(ctx context.Context, client kubernetes.Interface, namespace, id string) (RunStatus, error) {
	cm, r, err := loadRegistration(ctx, client, namespace, id)
	if err != nil {
		return RunStatus{}, err
	}
	status := r.status(cm.Name)
	status.RegistrationUID = cm.UID
	status.Phase = "pending"
	status.observePlacement(ctx, client)
	status.StopRequested = cm.Annotations[stoppedAnnotation] == "true"
	// Do not trust selector labels to prove that all pods are gone. Include
	// pods whose controller names match the inventory even if labels changed.
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return status, err
	}
	podsByJob := map[string][]corev1.Pod{}
	expected := map[string]bool{}
	for _, js := range status.Jobs {
		expected[js.Name] = true
	}
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		matchesRun := pod.Labels[runIDLabel] == id
		matchesJob := owner != nil && owner.Kind == "Job" && expected[owner.Name]
		if !matchesRun && !matchesJob {
			continue
		}
		if owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "Job" || !expected[owner.Name] || pod.Labels[managedByLabel] != "hcp-perf" || !matchesRun || pod.Annotations[digestAnnotation] != cm.Annotations[digestAnnotation] {
			return status, fmt.Errorf("refusing unowned pod %q", pod.Name)
		}
		podsByJob[owner.Name] = append(podsByJob[owner.Name], pod)
	}
	allComplete, allSuspended, missing, failed, running := true, true, false, false, false
	for i, node := range r.Plan.Nodes {
		js := &status.Jobs[i]
		job, err := client.BatchV1().Jobs(namespace).Get(ctx, js.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if len(podsByJob[js.Name]) != 0 {
				return status, fmt.Errorf("missing job %q still has pods; cannot verify their ownership", js.Name)
			}
			allComplete, missing = false, true
			continue
		}
		if err != nil {
			return status, err
		}
		if err := ownedJob(job, cm, r, node); err != nil {
			return status, err
		}
		js.UID, js.Conditions, js.Phase = job.UID, job.Status.Conditions, "pending"
		js.Deleting = job.DeletionTimestamp != nil
		events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("involvedObject.uid", string(job.UID)).String(), Limit: maxJobEvents,
		})
		if err != nil {
			return status, fmt.Errorf("list events for job %q: %w", job.Name, err)
		}
		js.EventsTruncated = events.Continue != ""
		for _, event := range events.Items {
			ref := event.InvolvedObject
			if ref.UID != job.UID || ref.Name != job.Name || ref.Namespace != namespace || ref.Kind != "Job" || ref.APIVersion != "batch/v1" {
				continue
			}
			if len(js.Events) == maxJobEvents {
				js.EventsTruncated = true
				break
			}
			js.Events = append(js.Events, event)
		}
		suspended := job.Spec.Suspend != nil && *job.Spec.Suspend
		allSuspended = allSuspended && suspended
		complete := false
		for _, condition := range job.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}
			if condition.Type == batchv1.JobComplete {
				complete, js.Phase = true, "completed"
			}
			if condition.Type == batchv1.JobFailed {
				failed, js.Phase = true, "failed"
			}
		}
		allComplete = allComplete && complete
		for _, pod := range podsByJob[job.Name] {
			owner := metav1.GetControllerOf(&pod)
			if owner == nil || owner.Kind != "Job" || owner.APIVersion != "batch/v1" || owner.Name != job.Name || owner.UID != job.UID || pod.Labels[managedByLabel] != "hcp-perf" || pod.Labels[runIDLabel] != id || pod.Annotations[digestAnnotation] != cm.Annotations[digestAnnotation] {
				return status, fmt.Errorf("refusing unowned pod %q", pod.Name)
			}
			ps := PodStatus{UID: pod.UID, Name: pod.Name, Node: pod.Spec.NodeName, Phase: pod.Status.Phase, Reason: pod.Status.Reason, Message: pod.Status.Message, Deleting: pod.DeletionTimestamp != nil, Conditions: pod.Status.Conditions, Containers: pod.Status.ContainerStatuses, InitContainers: pod.Status.InitContainerStatuses}
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					ps.Ready = true
				}
			}
			if pod.Status.Phase == corev1.PodRunning && !ps.Deleting && js.Phase == "pending" {
				js.Phase, running = "running", true
			}
			js.Pods = append(js.Pods, ps)
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				status.NonterminalPods++
			}
		}
		sort.Slice(js.Pods, func(i, j int) bool { return js.Pods[i].Name < js.Pods[j].Name })
		if suspended {
			js.Phase = "suspended"
		}
	}
	status.SubmissionIncomplete = missing
	switch {
	case status.StopRequested:
		status.Phase = "stopping"
		if !missing && allSuspended && status.NonterminalPods == 0 {
			status.Phase = "stopped"
		}
	case missing:
		status.Phase = "incomplete"
	case failed:
		status.Phase = "failed"
	case allComplete:
		status.Phase = "completed"
	case running:
		status.Phase = "running"
	}
	return status, nil
}

// Stop retains the registration and Jobs. Suspending a Job terminates its active
// pods and prevents replacements. Status requires every expected Job suspended
// and no nonterminal pods before reporting stopped. Missing Jobs remain unresolved
// even if submission was interrupted permanently; terminal pods retain diagnostics.
// UID and resourceVersion tests prevent patching a replaced or concurrently
// relabeled object. Each owned Job is attempted even when another fails. Optional
// identity pins registration and previously observed Job UIDs across Stop calls.
// Conflicts/transient failures retry three times with fresh reads, never new UIDs.
func Stop(ctx context.Context, client kubernetes.Interface, namespace, id string, identity ...*RunStatus) (RunStatus, error) {
	cm, r, err := loadRegistration(ctx, client, namespace, id)
	if err != nil {
		return RunStatus{}, err
	}
	status := r.status(cm.Name)
	status.RegistrationUID = cm.UID
	status.Phase = "stopping"
	if len(identity) > 1 {
		return status, errors.New("at most one stop identity is allowed")
	}
	bound := &status
	if len(identity) == 1 && identity[0] != nil {
		bound = identity[0]
		if bound.RegistrationUID == "" || bound.RegistrationUID != cm.UID || !reflect.DeepEqual(bound.Plan.Nodes, r.Plan.Nodes) {
			return status, errors.New("stop registration identity changed")
		}
	}
	// Persist intent before reading Jobs, so Start stops submitting as soon as
	// it observes this marker. Even if this fails, suspend every owned worker;
	// failed intent persistence still prevents claiming verified cleanup.
	intentErr := retryStop(ctx, func(requestCtx context.Context) error {
		current, _, err := loadRegistration(requestCtx, client, namespace, id)
		if err != nil {
			return err
		}
		if current.UID != cm.UID || current.Annotations[digestAnnotation] != cm.Annotations[digestAnnotation] {
			return errors.New("registration replaced during stop")
		}
		if current.Annotations[stoppedAnnotation] == "true" {
			return nil
		}
		current.Annotations[stoppedAnnotation] = "true"
		patch, err := guardedPatch(current.UID, current.ResourceVersion, "/metadata/annotations", current.Annotations)
		if err != nil {
			return err
		}
		_, err = client.CoreV1().ConfigMaps(namespace).Patch(requestCtx, cm.Name, types.JSONPatchType, patch, metav1.PatchOptions{})
		return err
	})
	errs := []error{intentErr}
	for i, node := range r.Plan.Nodes {
		name := jobName(id, node.Name)
		var pinned types.UID
		for _, job := range bound.Jobs {
			if job.Name == name {
				pinned = job.UID
			}
		}
		jobErr := retryStop(ctx, func(requestCtx context.Context) error {
			job, err := client.BatchV1().Jobs(namespace).Get(requestCtx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil // Status retains missing submission as unresolved.
			}
			if err != nil {
				return err
			}
			if err := ownedJob(job, cm, r, node); err != nil {
				return err
			}
			if job.UID == "" || pinned != "" && pinned != job.UID {
				return errors.New("job UID changed during stop")
			}
			pinned = job.UID
			status.Jobs[i].UID = pinned
			for j := range bound.Jobs {
				if bound.Jobs[j].Name == name {
					bound.Jobs[j].UID = pinned
				}
			}
			if job.Spec.Suspend != nil && *job.Spec.Suspend {
				return nil
			}
			patch, err := guardedPatch(pinned, job.ResourceVersion, "/spec/suspend", true)
			if err != nil {
				return err
			}
			_, err = client.BatchV1().Jobs(namespace).Patch(requestCtx, job.Name, types.JSONPatchType, patch, metav1.PatchOptions{})
			return err
		})
		if jobErr != nil {
			errs = append(errs, fmt.Errorf("suspend job %q: %w", name, jobErr))
		}
	}
	observed, statusErr := Status(ctx, client, namespace, id)
	errs = append(errs, statusErr)
	if statusErr == nil {
		if observed.RegistrationUID != cm.UID {
			errs = append(errs, errors.New("registration replaced during stop verification"))
		}
		for i, job := range observed.Jobs {
			if status.Jobs[i].UID != "" && job.UID != status.Jobs[i].UID {
				errs = append(errs, errors.New("job replaced during stop verification"))
			}
		}
		status = observed
	}
	return status, errors.Join(errs...)
}

func retryStop(ctx context.Context, attempt func(context.Context) error) error {
	var last error
	for i := 0; i < 3; i++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(last, err)
		}
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		last = attempt(requestCtx)
		cancel()
		var network net.Error
		retryable := apierrors.IsConflict(last) || apierrors.IsTimeout(last) || apierrors.IsServerTimeout(last) || apierrors.IsTooManyRequests(last) || apierrors.IsServiceUnavailable(last) || apierrors.IsInternalError(last) || errors.Is(last, context.DeadlineExceeded) || errors.As(last, &network)
		if last == nil || !retryable || i == 2 {
			return last
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(last, ctx.Err())
		case <-timer.C:
		}
	}
	return last
}

func guardedPatch(uid types.UID, version, path string, value any) ([]byte, error) {
	return json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": uid},
		{"op": "test", "path": "/metadata/resourceVersion", "value": version},
		{"op": "add", "path": path, "value": value},
	})
}

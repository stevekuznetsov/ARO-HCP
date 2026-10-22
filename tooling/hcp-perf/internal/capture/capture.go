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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"

	"sigs.k8s.io/yaml"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/scrape"
)

const (
	ManagedByLabel      = "app.kubernetes.io/managed-by"
	SessionIDLabel      = "hcp-perf.azure.com/session-id"
	ComponentLabel      = "hcp-perf.azure.com/component"
	discoveryAnnotation = "hcp-perf.azure.com/kubernetes-discovery"
	resourceName        = "prometheus"
	MaxStorageGi        = 4096
)

// Spec is the explicit v1 session input. There are no resource defaults.
// StorageGi is 1..4096, RetentionHours is 1..168. CPURequestMilli,
// MemoryRequestMi and MemoryLimitMi must be positive; memory limit >= request.
// ID is a lowercase DNS label of at most 40 characters. Image must be pinned
// with @sha256:<64 lowercase hexadecimal digits>.
type Spec struct {
	Version         string `json:"version"`
	ID              string `json:"id"`
	Image           string `json:"image"`
	StorageClass    string `json:"storageClass"`
	StorageGi       int64  `json:"storageGi"`
	RetentionHours  int64  `json:"retentionHours"`
	CPURequestMilli int64  `json:"cpuRequestMilli"`
	MemoryRequestMi int64  `json:"memoryRequestMi"`
	MemoryLimitMi   int64  `json:"memoryLimitMi"`
	// KubernetesDiscovery is required in JSON input (no omitempty). It opts
	// into a mounted token and cluster-wide read RBAC; false grants neither.
	KubernetesDiscovery bool `json:"kubernetesDiscovery"`
	// TLSMounts accepts Bundle.TLSMounts (TLS and HTTP credential files) and
	// snapshots only selected keys. The field name is persisted session format.
	TLSMounts []scrape.TLSMountRequirement `json:"tlsMounts,omitempty"`
}

var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)

// Validate checks the spec without making Kubernetes calls.
func (s Spec) Validate() error {
	if s.Version != "v1" {
		return errors.New("capture version must be v1")
	}
	if err := validateID(s.ID); err != nil {
		return err
	}
	if !imagePattern.MatchString(s.Image) || strings.Contains(s.Image, "://") {
		return errors.New("image must be a registry image pinned by a lowercase sha256 digest")
	}
	if s.StorageClass == "" || len(validation.IsDNS1123Subdomain(s.StorageClass)) != 0 {
		return errors.New("storageClass must explicitly name a valid StorageClass")
	}
	if s.StorageGi < 1 || s.StorageGi > MaxStorageGi {
		return fmt.Errorf("storageGi must be between 1 and %d", MaxStorageGi)
	}
	if s.RetentionHours < 1 || s.RetentionHours > 168 {
		return errors.New("retentionHours must be between 1 and 168")
	}
	// Bound quantities before conversion to bytes to prevent int64 overflow.
	const maxMi = (1<<63 - 1) / (1024 * 1024)
	if s.CPURequestMilli < 1 || s.MemoryRequestMi < 1 || s.MemoryLimitMi < s.MemoryRequestMi || s.MemoryLimitMi > maxMi {
		return errors.New("CPU and memory must be positive representable quantities, with memory limit >= request")
	}
	_, err := validateTLSMounts("hcp-perf-"+s.ID, s.TLSMounts)
	return err
}

func validateID(id string) error {
	if len(id) > 40 || len(validation.IsDNS1123Label(id)) != 0 {
		return errors.New("capture ID must be a lowercase DNS label of 1..40 characters")
	}
	return nil
}

func validateConfig(config string) error {
	if len(config) == 0 || len(config) > 900*1024 {
		return errors.New("Prometheus configuration must contain 1..921600 bytes")
	}
	reader := yamlutil.NewYAMLReader(bufio.NewReader(strings.NewReader(config)))
	document, err := reader.Read()
	if err != nil {
		return errors.New("Prometheus configuration must be one YAML mapping")
	}
	if _, err := reader.Read(); err != io.EOF {
		return errors.New("Prometheus configuration must contain exactly one YAML document")
	}
	var configuration map[string]any
	if err := yaml.UnmarshalStrict(document, &configuration); err != nil {
		// YAML errors can echo secret-bearing input. Do not return the parser error.
		return errors.New("Prometheus configuration must be a valid YAML mapping without duplicate keys")
	}
	scrapes, ok := configuration["scrape_configs"].([]any)
	if !ok || len(scrapes) == 0 {
		return errors.New("Prometheus configuration requires nonempty scrape_configs")
	}
	names := map[string]bool{}
	for _, scrape := range scrapes {
		job, ok := scrape.(map[string]any)
		if !ok {
			return errors.New("each scrape_config must be a mapping")
		}
		name, ok := job["job_name"].(string)
		if !ok || strings.TrimSpace(name) == "" || names[name] {
			return errors.New("scrape_configs require unique nonempty job_name strings")
		}
		names[name] = true
	}
	return nil
}

func labels(id string) map[string]string {
	return map[string]string{ManagedByLabel: "hcp-perf", SessionIDLabel: id, ComponentLabel: "capture"}
}

func owned(object metav1.Object, id string) bool {
	return object.GetLabels()[ManagedByLabel] == "hcp-perf" && object.GetLabels()[SessionIDLabel] == id
}

func objectMeta(namespace, id string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: resourceName, Namespace: namespace, Labels: labels(id)}
}

// Start validates before any API calls, reads selected TLS source keys if any,
// then creates a new dedicated namespace and its resources. It does not wait for
// readiness or undo partial creation. Source read failures create no resources.
// Returned inventory records confirmed creates only. Even an identical existing
// session is an error; use Status to inspect it, not Start to reconcile it.
func Start(ctx context.Context, client kubernetes.Interface, spec Spec, config string) (SessionStatus, error) {
	s := SessionStatus{Version: "v1", ID: spec.ID, Namespace: "hcp-perf-" + spec.ID, Phase: PhasePending}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if err := spec.Validate(); err != nil {
		return s, err
	}
	if err := validateConfig(config); err != nil {
		return s, err
	}
	// Canonicalize and deduplicate references before reading any source objects.
	spec.TLSMounts, _ = validateTLSMounts(s.Namespace, spec.TLSMounts)
	snapshots, err := readTLS(ctx, client, spec.TLSMounts)
	if err != nil {
		return s, err
	}
	if len(spec.TLSMounts) > 0 {
		s.TLSCredentialPolicy = tlsCredentialPolicy
	}
	nsLabels := labels(spec.ID)
	nsLabels["network.openshift.io/policy-group"] = "monitoring"
	namespace, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: s.Namespace, Labels: nsLabels,
			Annotations: map[string]string{discoveryAnnotation: fmt.Sprint(spec.KubernetesDiscovery)}},
	}, metav1.CreateOptions{})
	if err != nil {
		return s, operationError("create namespace (existing namespaces are never adopted)", err)
	}
	s.Inventory = append(s.Inventory, reference("Namespace", namespace))
	// Keep an inventory without issuing further requests after a failed create,
	// including when the caller's context has already expired.
	record := func(kind string, object metav1.Object, err error) error {
		if err != nil {
			s.Phase = PhaseFailed
			return operationError("create "+kind+"; resources retained", err)
		}
		s.Inventory = append(s.Inventory, reference(kind, object))
		return nil
	}
	sa, err := client.CoreV1().ServiceAccounts(s.Namespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: objectMeta(s.Namespace, spec.ID), AutomountServiceAccountToken: ptr(spec.KubernetesDiscovery),
	}, metav1.CreateOptions{})
	if err := record("ServiceAccount", sa, err); err != nil {
		return s, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return s, err
	}
	cm, err := client.CoreV1().ConfigMaps(s.Namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: objectMeta(s.Namespace, spec.ID), Immutable: ptr(true),
		Data: map[string]string{"spec.json": string(specJSON), "prometheus.yml": config},
	}, metav1.CreateOptions{})
	if err := record("ConfigMap", cm, err); err != nil {
		return s, err
	}
	for i, mount := range spec.TLSMounts {
		metadata := objectMeta(s.Namespace, spec.ID)
		metadata.Name = mount.DestinationName
		if mount.Source.Kind == "Secret" {
			secret, err := client.CoreV1().Secrets(s.Namespace).Create(ctx, &corev1.Secret{
				ObjectMeta: metadata, Type: corev1.SecretTypeOpaque, Immutable: ptr(true),
				Data: map[string][]byte{mount.Source.Key: snapshots[i]},
			}, metav1.CreateOptions{})
			if err := record("Secret", secret, err); err != nil {
				return s, err
			}
		} else {
			ca, err := client.CoreV1().ConfigMaps(s.Namespace).Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metadata, Immutable: ptr(true), BinaryData: map[string][]byte{mount.Source.Key: snapshots[i]},
			}, metav1.CreateOptions{})
			if err := record("ConfigMap", ca, err); err != nil {
				return s, err
			}
		}
	}
	if spec.KubernetesDiscovery {
		metadata := metav1.ObjectMeta{
			Name: s.Namespace + "-prometheus", Labels: labels(spec.ID),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(namespace, corev1.SchemeGroupVersion.WithKind("Namespace"))},
		}
		role, err := client.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
			ObjectMeta: metadata,
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"nodes", "services", "endpoints", "pods"}, Verbs: []string{"get", "list", "watch"}},
				{APIGroups: []string{""}, Resources: []string{"nodes/metrics"}, Verbs: []string{"get"}},
				{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}},
				{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"get", "list", "watch"}},
			},
		}, metav1.CreateOptions{})
		if err := record("ClusterRole", role, err); err != nil {
			return s, err
		}
		binding, err := client.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metadata,
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: metadata.Name},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: resourceName, Namespace: s.Namespace}},
		}, metav1.CreateOptions{})
		if err := record("ClusterRoleBinding", binding, err); err != nil {
			return s, err
		}
	}
	pvc, err := client.CoreV1().PersistentVolumeClaims(s.Namespace).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: objectMeta(s.Namespace, spec.ID),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: &spec.StorageClass,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewQuantity(spec.StorageGi*1024*1024*1024, resource.BinarySI),
			}},
		},
	}, metav1.CreateOptions{})
	if err := record("PersistentVolumeClaim", pvc, err); err != nil {
		return s, err
	}
	s.Storage = append(s.Storage, storageStatus(pvc))
	service, err := client.CoreV1().Services(s.Namespace).Create(ctx, &corev1.Service{
		ObjectMeta: objectMeta(s.Namespace, spec.ID),
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, Selector: labels(spec.ID),
			Ports: []corev1.ServicePort{{Name: "http", Port: 9090, TargetPort: intstr.FromInt32(9090)}},
		},
	}, metav1.CreateOptions{})
	if err := record("Service", service, err); err != nil {
		return s, err
	}
	s.ServiceURL = serviceURL(s.Namespace)
	sts, err := client.AppsV1().StatefulSets(s.Namespace).Create(ctx, statefulSet(spec, s.Namespace), metav1.CreateOptions{})
	if err := record("StatefulSet", sts, err); err != nil {
		return s, err
	}
	s.DesiredReplicas = 1
	return s, nil
}

func statefulSet(spec Spec, namespace string) *appsv1.StatefulSet {
	probe := func(path string, period, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(9090)}},
			PeriodSeconds: period, TimeoutSeconds: 3, FailureThreshold: failures,
		}
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: objectMeta(namespace, spec.ID),
		Spec: appsv1.StatefulSetSpec{
			ServiceName: resourceName, Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels(spec.ID)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(spec.ID)},
				Spec: corev1.PodSpec{
					ServiceAccountName: resourceName, AutomountServiceAccountToken: ptr(spec.KubernetesDiscovery),
					TerminationGracePeriodSeconds: ptr(int64(120)),
					NodeSelector:                  map[string]string{"aro-hcp.azure.com/role": "infra"},
					Tolerations:                   []corev1.Toleration{{Key: "infra", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(1000)), RunAsGroup: ptr(int64(2000)), FSGroup: ptr(int64(2000)),
						FSGroupChangePolicy: ptr(corev1.FSGroupChangeOnRootMismatch),
						SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name: resourceName, Image: spec.Image, ImagePullPolicy: corev1.PullIfNotPresent,
						Args: []string{
							"--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus",
							fmt.Sprintf("--storage.tsdb.retention.time=%dh", spec.RetentionHours),
							"--web.listen-address=:9090",
						},
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 9090}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    *resource.NewMilliQuantity(spec.CPURequestMilli, resource.DecimalSI),
								corev1.ResourceMemory: *resource.NewQuantity(spec.MemoryRequestMi*1024*1024, resource.BinarySI),
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(spec.MemoryLimitMi*1024*1024, resource.BinarySI)},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						ReadinessProbe: probe("/-/ready", 5, 3), LivenessProbe: probe("/-/healthy", 10, 3),
						StartupProbe: probe("/-/ready", 10, 180),
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: "/etc/prometheus", ReadOnly: true},
							{Name: "data", MountPath: "/prometheus"}, {Name: "tmp", MountPath: "/tmp"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: resourceName},
							Items:                []corev1.KeyToPath{{Key: "prometheus.yml", Path: "prometheus.yml"}},
						}}},
						{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: resourceName}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(64*1024*1024, resource.BinarySI)}}},
					},
				},
			},
		},
	}
	for _, mount := range spec.TLSMounts {
		sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes, *mount.Volume.DeepCopy())
		sts.Spec.Template.Spec.Containers[0].VolumeMounts = append(sts.Spec.Template.Spec.Containers[0].VolumeMounts, *mount.VolumeMount.DeepCopy())
	}
	return sts
}

func ptr[T any](value T) *T { return &value }

// operationError omits server messages and reason strings, which may echo config.
// Only fixed classifications and context cancellation escape this boundary.
func operationError(operation string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", operation, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, context.DeadlineExceeded)
	}
	classification := "Unknown"
	switch {
	case apierrors.IsNotFound(err):
		classification = "NotFound"
	case apierrors.IsForbidden(err):
		classification = "Forbidden"
	case apierrors.IsUnauthorized(err):
		classification = "Unauthorized"
	case apierrors.IsAlreadyExists(err):
		classification = "AlreadyExists"
	case apierrors.IsConflict(err):
		classification = "Conflict"
	case apierrors.IsInvalid(err):
		classification = "Invalid"
	case apierrors.IsBadRequest(err):
		classification = "BadRequest"
	case apierrors.IsTooManyRequests(err):
		classification = "TooManyRequests"
	case apierrors.IsTimeout(err):
		classification = "Timeout"
	case apierrors.IsServerTimeout(err):
		classification = "ServerTimeout"
	case apierrors.IsServiceUnavailable(err):
		classification = "ServiceUnavailable"
	case apierrors.IsInternalError(err):
		classification = "InternalError"
	case apierrors.IsGone(err) || apierrors.IsResourceExpired(err):
		classification = "Gone"
	}
	return fmt.Errorf("%s: Kubernetes API request failed (%s)", operation, classification)
}

func namespaceFor(ctx context.Context, client kubernetes.Interface, id string) (*corev1.Namespace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateID(id); err != nil {
		return nil, err
	}
	ns, err := client.CoreV1().Namespaces().Get(ctx, "hcp-perf-"+id, metav1.GetOptions{})
	if err != nil {
		return nil, operationError("get namespace", err)
	}
	if !owned(ns, id) {
		return nil, errors.New("namespace is not owned by this capture session")
	}
	return ns, nil
}

// Stop requests zero replicas without deleting the PVC or other evidence.
// Only namespace and StatefulSet ownership reads precede the guarded mutation.
// Diagnostics run afterward; an observation error returns stopping status with
// zero desired replicas and the confirmed namespace/StatefulSet inventory.
// Callers may use WaitStopped to await shutdown.
// Conflicts are returned, not retried against a potentially replaced object.
func Stop(ctx context.Context, client kubernetes.Interface, id string) (SessionStatus, error) {
	s := SessionStatus{Version: "v1", ID: id, Namespace: "hcp-perf-" + id, Phase: PhasePending}
	ns, err := namespaceFor(ctx, client, id)
	if err != nil {
		return s, err
	}
	s.Inventory = append(s.Inventory, reference("Namespace", ns))
	sts, err := client.AppsV1().StatefulSets(s.Namespace).Get(ctx, resourceName, metav1.GetOptions{})
	if err != nil {
		return s, operationError("get StatefulSet", err)
	}
	if !owned(sts, id) || sts.Labels[ComponentLabel] != "capture" || sts.DeletionTimestamp != nil {
		return s, errors.New("StatefulSet is not owned by this capture session or is being deleted")
	}
	s.Inventory = append(s.Inventory, reference("StatefulSet", sts))
	s.DesiredReplicas = 1
	if sts.Spec.Replicas != nil {
		s.DesiredReplicas = *sts.Spec.Replicas
	}
	s.ReadyReplicas = sts.Status.ReadyReplicas
	// JSON Patch tests bind the mutation to the exact object version inspected.
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": sts.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": sts.ResourceVersion},
		{"op": "add", "path": "/spec/replicas", "value": 0},
	})
	if err != nil {
		return s, err
	}
	if _, err := client.AppsV1().StatefulSets(s.Namespace).Patch(ctx, resourceName, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		return s, operationError("scale StatefulSet to zero", err)
	}
	s.DesiredReplicas, s.Phase = 0, PhaseStopping
	observed, err := Status(ctx, client, id)
	if err != nil {
		return s, err
	}
	return observed, nil
}

// Cleanup requires explicit consent to destroy evidence and submits a namespace
// deletion guarded by UID and resourceVersion. It never removes finalizers and
// does not wait for deletion to complete. Capture must have zero desired replicas
// and the entire namespace must have no nonterminal pods, including adversaries.
// Every Job must be suspended or have Complete=True or Failed=True.
// Callers must quiesce workload producers before this non-atomic safety check.
func Cleanup(ctx context.Context, client kubernetes.Interface, id string, deleteEvidence bool) error {
	if !deleteEvidence {
		return errors.New("cleanup requires deleteEvidence=true; namespace deletion destroys the capture PVC")
	}
	ns, err := namespaceFor(ctx, client, id)
	if err != nil {
		return err
	}
	sts, err := client.AppsV1().StatefulSets(ns.Name).Get(ctx, resourceName, metav1.GetOptions{})
	if err != nil {
		return operationError("cleanup requires a stopped capture StatefulSet", err)
	}
	if !owned(sts, id) || sts.Labels[ComponentLabel] != "capture" || sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 || sts.DeletionTimestamp != nil {
		return errors.New("cleanup requires an owned, stopped capture StatefulSet")
	}
	// Do not use the capture selector: deleting the namespace also destroys
	// adversary Jobs and any other workloads or evidence sharing it.
	jobs, err := client.BatchV1().Jobs(ns.Name).List(ctx, metav1.ListOptions{})
	if err != nil {
		return operationError("list all Jobs before cleanup", err)
	}
	for _, job := range jobs.Items {
		quiescent := job.Spec.Suspend != nil && *job.Spec.Suspend
		for _, condition := range job.Status.Conditions {
			if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) && condition.Status == corev1.ConditionTrue {
				quiescent = true
			}
		}
		if !quiescent {
			return errors.New("cleanup refused: namespace contains Jobs that are neither terminal nor suspended")
		}
	}
	pods, err := client.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{})
	if err != nil {
		return operationError("list all pods before cleanup", err)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			return errors.New("cleanup refused: namespace still contains nonterminal pods")
		}
	}
	err = client.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &ns.UID, ResourceVersion: &ns.ResourceVersion},
	})
	if err != nil {
		return operationError("delete namespace", err)
	}
	return nil
}

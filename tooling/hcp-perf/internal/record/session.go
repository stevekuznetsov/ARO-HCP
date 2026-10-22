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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	managedByLabel   = "app.kubernetes.io/managed-by"
	RecordIDLabel    = "hcp-perf.azure.com/record-id"
	componentLabel   = "hcp-perf.azure.com/component"
	SessionOutputDir = "/data/observations"
)

// SessionSpec launches Record in an existing, live namespace managed by hcp-perf.
// ID is a DNS label of at most 40 characters, independent of the capture ID.
// All resource quantities, the digest-pinned image and StorageClass are explicit.
// Target namespaces need not exist yet. OutputDir is fixed, not caller supplied.
//
// SECURITY: the worker receives cluster-wide get/list of full Pods, Events,
// Deployments and StatefulSets, not metadata-only responses. This can expose
// credentials in pod env, commands and event messages across ALL namespaces.
// Namespaces limits recording, NOT authorization. Watch output is allowlisted;
// opt-in raw logs are NOT sanitized and can contain secrets. EnableWatch grants
// watch and EnableLogs grants pods/log get across ALL namespaces. No Secrets,
// nodes or write permissions are granted. Cluster RBAC observes future namespaces
// without writing Roles into HCP namespaces or running a privileged reconciler.
type SessionSpec struct {
	Version                  string   `json:"version"`
	ID                       string   `json:"id"`
	Namespace                string   `json:"namespace"`
	Image                    string   `json:"image"`
	StorageClass             string   `json:"storageClass"`
	StorageGi                int64    `json:"storageGi"`
	CPURequestMilli          int64    `json:"cpuRequestMilli"`
	MemoryRequestMi          int64    `json:"memoryRequestMi"`
	MemoryLimitMi            int64    `json:"memoryLimitMi"`
	Namespaces               []string `json:"namespaces"`
	IntervalSeconds          int64    `json:"intervalSeconds"`
	DurationSeconds          int64    `json:"durationSeconds"`
	EnableWatch              bool     `json:"enableWatch,omitempty"`
	EnableLogs               bool     `json:"enableLogs,omitempty"`
	RawLogs                  bool     `json:"rawLogs,omitempty"`
	MaxStreamBytes           int64    `json:"maxStreamBytes,omitempty"`
	MaxObservationBytes      int64    `json:"maxObservationBytes,omitempty"`
	MaxConcurrentLogs        int      `json:"maxConcurrentLogs,omitempty"`
	LogRequestQPS            int64    `json:"logRequestQPS,omitempty"`
	LogRequestBurst          int64    `json:"logRequestBurst,omitempty"`
	StreamSyncIntervalMillis int64    `json:"streamSyncIntervalMillis,omitempty"`
}

func (s SessionSpec) recorderSpec() Spec {
	return Spec{Version: s.Version, Namespaces: s.Namespaces, IntervalSeconds: s.IntervalSeconds, DurationSeconds: s.DurationSeconds, OutputDir: SessionOutputDir,
		EnableWatch: s.EnableWatch, EnableLogs: s.EnableLogs, RawLogs: s.RawLogs, MaxStreamBytes: s.MaxStreamBytes,
		MaxObservationBytes: s.MaxObservationBytes, MaxConcurrentLogs: s.MaxConcurrentLogs,
		LogRequestQPS: s.LogRequestQPS, LogRequestBurst: s.LogRequestBurst, StreamSyncIntervalMillis: s.StreamSyncIntervalMillis}
}

var sessionImage = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)

func validateSessionIdentity(namespace, id string) error {
	if len(id) > 40 || len(validation.IsDNS1123Label(id)) != 0 || len(validation.IsDNS1123Label(namespace)) != 0 {
		return errors.New("namespace must be an explicit DNS label and id a DNS label of 1..40 characters")
	}
	return nil
}

func (s SessionSpec) Validate() error {
	if err := validateSessionIdentity(s.Namespace, s.ID); err != nil {
		return err
	}
	if err := s.recorderSpec().Validate(); err != nil {
		return err
	}
	if !sessionImage.MatchString(s.Image) || strings.Contains(s.Image, "://") {
		return errors.New("image must be pinned by a lowercase sha256 digest")
	}
	if s.StorageClass == "" || len(validation.IsDNS1123Subdomain(s.StorageClass)) != 0 || s.StorageGi < 1 || s.StorageGi > 4096 {
		return errors.New("storageClass must be explicit and valid; storageGi must be 1..4096")
	}
	if s.CPURequestMilli < 1 || s.MemoryRequestMi < 1 || s.MemoryLimitMi < s.MemoryRequestMi || s.MemoryLimitMi > (1<<63-1)/(1024*1024) {
		return errors.New("CPU and memory must be positive representable quantities, with memory limit >= request")
	}
	return nil
}

// SessionStatus is a non-atomic Kubernetes observation, NOT recording progress or
// a data-integrity guarantee. No logs, exec, checkpoint contents or API messages
// are returned. Inventory includes only confirmed creates/owned observations,
// including the dedicated PVC even after the Job or its pods have disappeared.
// An error means the observation is partial; never infer inactivity from it.
type SessionStatus struct {
	Version         string              `json:"version"`
	ID              string              `json:"id"`
	Namespace       string              `json:"namespace"`
	Phase           string              `json:"phase"` // partial, pending, running, completed, failed, stopping, stopped
	OutputDir       string              `json:"outputDir"`
	Inventory       []ResourceReference `json:"inventory"`
	Missing         []string            `json:"missing,omitempty"`
	Storage         *SessionStorage     `json:"storage,omitempty"`
	Conditions      []SessionCondition  `json:"conditions,omitempty"`
	Pods            []SessionPod        `json:"pods,omitempty"`
	NonterminalPods int                 `json:"nonterminalPods"`
	StopRequested   bool                `json:"stopRequested"`
	// ExportHelper is read-only compute, never included in writer pod counts.
	ExportHelper *SessionPod `json:"exportHelper,omitempty"`
}

type ResourceReference struct {
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Namespace string    `json:"namespace,omitempty"`
	UID       types.UID `json:"uid"`
}

type SessionStorage struct {
	Name       string                            `json:"name"`
	UID        types.UID                         `json:"uid"`
	Phase      corev1.PersistentVolumeClaimPhase `json:"phase"`
	VolumeName string                            `json:"volumeName,omitempty"`
}

type SessionCondition struct {
	Type   batchv1.JobConditionType `json:"type"`
	Status corev1.ConditionStatus   `json:"status"`
}

type SessionPod struct {
	Name        string          `json:"name"`
	UID         types.UID       `json:"uid"`
	Phase       corev1.PodPhase `json:"phase"`
	Terminating bool            `json:"terminating"`
	Ready       bool            `json:"ready"`
	StartTime   *metav1.Time    `json:"startTime,omitempty"`
}

func sessionStatus(namespace, id string) SessionStatus {
	return SessionStatus{Version: "v1", Namespace: namespace, ID: id, Phase: "partial", OutputDir: SessionOutputDir}
}

func (s *SessionStatus) add(kind string, object metav1.Object) {
	s.Inventory = append(s.Inventory, ResourceReference{Kind: kind, Name: object.GetName(), Namespace: object.GetNamespace(), UID: object.GetUID()})
}

func sessionLabels(id string) map[string]string {
	return map[string]string{managedByLabel: "hcp-perf", RecordIDLabel: id, componentLabel: "record"}
}

func sessionRBACName(ns *corev1.Namespace, id string) string {
	digest := sha256.Sum256([]byte(ns.Name + "/" + string(ns.UID) + "/" + id))
	return fmt.Sprintf("hcp-perf-record-%x", digest[:16])
}

func sessionOwned(object metav1.Object, id string, owner metav1.Object, kind, apiVersion string) bool {
	ref := metav1.GetControllerOf(object)
	return object.GetLabels()[managedByLabel] == "hcp-perf" && object.GetLabels()[RecordIDLabel] == id && object.GetLabels()[componentLabel] == "record" &&
		owner.GetUID() != "" && ref != nil && ref.UID == owner.GetUID() && ref.Name == owner.GetName() && ref.Kind == kind && ref.APIVersion == apiVersion
}

func sessionNamespace(ctx context.Context, client kubernetes.Interface, namespace, id string) (*corev1.Namespace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSessionIdentity(namespace, id); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("Kubernetes client is required")
	}
	ns, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return nil, sessionError("get namespace", err)
	}
	if ns.Labels[managedByLabel] != "hcp-perf" || ns.DeletionTimestamp != nil || ns.UID == "" {
		return nil, errors.New("namespace must exist, be live and managed by hcp-perf with a UID")
	}
	return ns, nil
}

// Start is create-only: the immutable registration is created first and the Job
// last. Collisions are never adopted, updated or deleted. Partial resources are
// retained and returned without further API calls after a failed create. The PVC
// is never owned by the Job, shared with Prometheus, or automatically cleaned up.
func Start(ctx context.Context, client kubernetes.Interface, spec SessionSpec) (SessionStatus, error) {
	s := sessionStatus(spec.Namespace, spec.ID)
	if err := spec.Validate(); err != nil {
		return s, err
	}
	resolved := spec.recorderSpec().withDefaults()
	spec.LogRequestQPS, spec.LogRequestBurst = resolved.LogRequestQPS, resolved.LogRequestBurst
	spec.StreamSyncIntervalMillis = resolved.StreamSyncIntervalMillis
	workerJSON, err := json.Marshal(spec.recorderSpec())
	if err != nil {
		return s, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return s, err
	}
	if len(workerJSON)+len(specJSON) > 900*1024 {
		return s, errors.New("session configuration exceeds 900 KiB")
	}
	ns, err := sessionNamespace(ctx, client, spec.Namespace, spec.ID)
	if err != nil {
		return s, err
	}
	s.add("Namespace", ns)
	meta := metav1.ObjectMeta{Name: "record-" + spec.ID, Namespace: ns.Name, Labels: sessionLabels(spec.ID),
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ns, corev1.SchemeGroupVersion.WithKind("Namespace"))}}
	created := func(kind string, object metav1.Object, err error) error {
		if err != nil {
			return sessionError("create "+kind+" (create-only; resources retained)", err)
		}
		s.add(kind, object)
		return nil
	}
	cm, err := client.CoreV1().ConfigMaps(ns.Name).Create(ctx, &corev1.ConfigMap{ObjectMeta: meta, Immutable: sessionPtr(true),
		Data: map[string]string{"session.json": string(specJSON), "spec.json": string(workerJSON)}}, metav1.CreateOptions{})
	if err := created("ConfigMap", cm, err); err != nil {
		return s, err
	}
	meta.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(cm, corev1.SchemeGroupVersion.WithKind("ConfigMap"))}
	sa, err := client.CoreV1().ServiceAccounts(ns.Name).Create(ctx, &corev1.ServiceAccount{ObjectMeta: meta, AutomountServiceAccountToken: sessionPtr(false)}, metav1.CreateOptions{})
	if err := created("ServiceAccount", sa, err); err != nil {
		return s, err
	}
	rbacMeta := metav1.ObjectMeta{Name: sessionRBACName(ns, spec.ID), Labels: sessionLabels(spec.ID),
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ns, corev1.SchemeGroupVersion.WithKind("Namespace"))}}
	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods", "events"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments", "statefulsets"}, Verbs: []string{"get", "list"}},
	}
	if spec.EnableWatch {
		for i := range rules {
			rules[i].Verbs = append(rules[i].Verbs, "watch")
		}
	}
	if spec.EnableLogs {
		rules = append(rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}})
	}
	role, err := client.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: rbacMeta, Rules: rules}, metav1.CreateOptions{})
	if err := created("ClusterRole", role, err); err != nil {
		return s, err
	}
	binding, err := client.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: rbacMeta,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa.Name, Namespace: ns.Name}}}, metav1.CreateOptions{})
	if err := created("ClusterRoleBinding", binding, err); err != nil {
		return s, err
	}
	pvc, err := client.CoreV1().PersistentVolumeClaims(ns.Name).Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: meta,
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: &spec.StorageClass,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(spec.StorageGi*1024*1024*1024, resource.BinarySI)}}}}, metav1.CreateOptions{})
	if err := created("PersistentVolumeClaim", pvc, err); err != nil {
		return s, err
	}
	s.Storage = &SessionStorage{Name: pvc.Name, UID: pvc.UID, Phase: pvc.Status.Phase, VolumeName: pvc.Spec.VolumeName}
	job, err := client.BatchV1().Jobs(ns.Name).Create(ctx, &batchv1.Job{ObjectMeta: meta, Spec: batchv1.JobSpec{
		BackoffLimit: sessionPtr(int32(0)), Completions: sessionPtr(int32(1)), Parallelism: sessionPtr(int32(1)), ActiveDeadlineSeconds: sessionPtr(spec.DurationSeconds + 120),
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: sessionLabels(spec.ID)}, Spec: corev1.PodSpec{
			ServiceAccountName: sa.Name, AutomountServiceAccountToken: sessionPtr(false), RestartPolicy: corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: sessionPtr(int64(120)), EnableServiceLinks: sessionPtr(false),
			NodeSelector: map[string]string{"aro-hcp.azure.com/role": "infra"},
			Tolerations:  []corev1.Toleration{{Key: "infra", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}},
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: sessionPtr(true), RunAsUser: sessionPtr(int64(1000)), RunAsGroup: sessionPtr(int64(2000)), FSGroup: sessionPtr(int64(2000)),
				FSGroupChangePolicy: sessionPtr(corev1.FSGroupChangeOnRootMismatch), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: "record", Image: spec.Image, Command: []string{"/hcp-perf"}, Args: []string{"record", "worker", "--input", "/config/spec.json"},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/hcp-perf", "record", "health", "--input", SessionOutputDir, "--require-ready"}}},
					PeriodSeconds: 2, FailureThreshold: 3,
				},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: *resource.NewMilliQuantity(spec.CPURequestMilli, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(spec.MemoryRequestMi*1024*1024, resource.BinarySI)},
					Limits: corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(spec.MemoryLimitMi*1024*1024, resource.BinarySI)}},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: sessionPtr(false), ReadOnlyRootFilesystem: sessionPtr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}, {Name: "config", MountPath: "/config", ReadOnly: true},
					{Name: "api-access", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name}}},
				{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cm.Name}, Items: []corev1.KeyToPath{{Key: "spec.json", Path: "spec.json"}}}}},
				{Name: "api-access", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: sessionPtr(int64(3600))}},
					{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
					{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
				}}}},
			},
		}},
	}}, metav1.CreateOptions{})
	if err := created("Job", job, err); err != nil {
		return s, err
	}
	s.Phase = "pending"
	return s, nil
}

func loadSession(ctx context.Context, client kubernetes.Interface, s *SessionStatus) (*corev1.Namespace, *corev1.ConfigMap, error) {
	ns, err := sessionNamespace(ctx, client, s.Namespace, s.ID)
	if err != nil {
		return nil, nil, err
	}
	s.add("Namespace", ns)
	cm, err := client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, "record-"+s.ID, metav1.GetOptions{})
	if err != nil {
		return ns, nil, sessionError("get session registration", err)
	}
	if !sessionOwned(cm, s.ID, ns, "Namespace", "v1") || cm.Immutable == nil || !*cm.Immutable || cm.DeletionTimestamp != nil || cm.UID == "" {
		return ns, nil, errors.New("registration is not live, immutable and owned by this namespace/session")
	}
	var spec SessionSpec
	if err := json.Unmarshal([]byte(cm.Data["session.json"]), &spec); err != nil {
		return ns, nil, errors.New("invalid session registration")
	}
	workerJSON, err := json.Marshal(spec.recorderSpec())
	if err != nil || spec.Validate() != nil || spec.ID != s.ID || spec.Namespace != s.Namespace || string(workerJSON) != cm.Data["spec.json"] {
		return ns, nil, errors.New("session registration identity or worker spec mismatch")
	}
	s.add("ConfigMap", cm)
	return ns, cm, nil
}

// Status retains evidence inventory when resources are missing. Terminal Job
// conditions, rather than a missing/running pod, establish completion/failure.
// Stopped additionally requires controller acknowledgement and no active pods.
func Status(ctx context.Context, client kubernetes.Interface, namespace, id string) (SessionStatus, error) {
	s := sessionStatus(namespace, id)
	ns, cm, err := loadSession(ctx, client, &s)
	if err != nil {
		return s, err
	}
	add := func(kind, name string, object metav1.Object, err error, cluster bool) error {
		if apierrors.IsNotFound(err) {
			s.Missing = append(s.Missing, kind+"/"+name)
			return nil
		}
		if err != nil {
			return sessionError("get "+kind, err)
		}
		owner, ownerKind := metav1.Object(cm), "ConfigMap"
		if cluster {
			owner, ownerKind = ns, "Namespace"
		}
		if !sessionOwned(object, id, owner, ownerKind, "v1") {
			return fmt.Errorf("%s is not owned by this session", kind)
		}
		s.add(kind, object)
		if object.GetDeletionTimestamp() != nil {
			return fmt.Errorf("%s is being deleted", kind)
		}
		return nil
	}
	// Inspect evidence before compute/RBAC so missing Jobs cannot hide the PVC.
	pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if err := add("PersistentVolumeClaim", cm.Name, pvc, err, false); err != nil {
		return s, err
	}
	if err == nil {
		s.Storage = &SessionStorage{Name: pvc.Name, UID: pvc.UID, Phase: pvc.Status.Phase, VolumeName: pvc.Spec.VolumeName}
	}
	sa, err := client.CoreV1().ServiceAccounts(namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if err := add("ServiceAccount", cm.Name, sa, err, false); err != nil {
		return s, err
	}
	rbacName := sessionRBACName(ns, id)
	role, err := client.RbacV1().ClusterRoles().Get(ctx, rbacName, metav1.GetOptions{})
	if err := add("ClusterRole", rbacName, role, err, true); err != nil {
		return s, err
	}
	binding, err := client.RbacV1().ClusterRoleBindings().Get(ctx, rbacName, metav1.GetOptions{})
	if err := add("ClusterRoleBinding", rbacName, binding, err, true); err != nil {
		return s, err
	}
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if err := add("Job", cm.Name, job, err, false); err != nil {
		return s, err
	}
	hasJob := err == nil
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return s, sessionError("list session pods", err)
	}
	ready := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if exportHelperOwned(pod, cm, id) {
			s.add("Pod", pod)
			s.ExportHelper = &SessionPod{Name: pod.Name, UID: pod.UID, Phase: pod.Status.Phase, Terminating: pod.DeletionTimestamp != nil}
			continue
		}
		owner := metav1.GetControllerOf(pod)
		// A Job controller can release a pod after label drift. Its immutable
		// storage/account association and generated name still identify a
		// candidate, but never establish ownership or prove it is inactive.
		candidate := pod.Name == "record-export-"+id || pod.Labels[RecordExportIDLabel] == id || pod.Labels[RecordIDLabel] == id || (owner != nil && owner.Kind == "Job" && owner.Name == cm.Name) ||
			pod.Spec.ServiceAccountName == cm.Name || strings.HasPrefix(pod.Name, cm.Name+"-") || strings.HasPrefix(pod.GenerateName, cm.Name+"-")
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == cm.Name {
				candidate = true
			}
		}
		if !candidate {
			continue
		}
		if !hasJob || !sessionOwned(pod, id, job, "Job", "batch/v1") {
			return s, errors.New("pod ownership mismatch; inactivity cannot be determined")
		}
		s.add("Pod", pod)
		observed := SessionPod{Name: pod.Name, UID: pod.UID, Phase: pod.Status.Phase, Terminating: pod.DeletionTimestamp != nil, StartTime: pod.Status.StartTime}
		for _, container := range pod.Status.ContainerStatuses {
			if container.Name == "record" && container.Ready {
				observed.Ready = true
			}
		}
		// Job activity includes scheduling and image pulls, not recorder startup.
		if observed.Phase == corev1.PodRunning && observed.Ready && !observed.Terminating && !observed.StartTime.IsZero() {
			ready = true
		}
		s.Pods = append(s.Pods, observed)
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			s.NonterminalPods++
		}
	}
	if len(s.Missing) != 0 {
		return s, errors.New("session resources missing; submission or evidence is incomplete")
	}
	s.StopRequested = job.Spec.Suspend != nil && *job.Spec.Suspend
	complete, failed, suspended := false, false, false
	for _, c := range job.Status.Conditions {
		s.Conditions = append(s.Conditions, SessionCondition{Type: c.Type, Status: c.Status})
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			complete = true
		case batchv1.JobFailed:
			failed = true
		case batchv1.JobSuspended:
			suspended = true
		}
	}
	s.Phase = "pending"
	switch {
	case failed || pvc.Status.Phase == corev1.ClaimLost:
		s.Phase = "failed"
	case complete:
		s.Phase = "completed"
	case s.StopRequested:
		s.Phase = "stopping"
		if suspended && job.Status.Active == 0 && s.NonterminalPods == 0 {
			s.Phase = "stopped"
		}
	case ready:
		s.Phase = "running"
	}
	return s, nil
}

// Stop suspends only the owned Job, guarded by UID and resourceVersion tests.
// It retains the PVC, registration, RBAC and Job. Missing Jobs return a partial
// error, never a false stopped acknowledgement (Start may still be submitting).
// It does not revoke RBAC or delete the namespace. Namespace cleanup must first
// quiesce all producers and verify ALL Jobs inactive, not just capture pods.
func Stop(ctx context.Context, client kubernetes.Interface, namespace, id string) (SessionStatus, error) {
	s := sessionStatus(namespace, id)
	_, cm, err := loadSession(ctx, client, &s)
	if err != nil {
		return s, err
	}
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if err != nil {
		return s, sessionError("get Job; stop unresolved", err)
	}
	if !sessionOwned(job, id, cm, "ConfigMap", "v1") || job.DeletionTimestamp != nil || job.UID == "" || job.ResourceVersion == "" {
		return s, errors.New("Job is not live and owned by this session with UID/resourceVersion")
	}
	s.add("Job", job)
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": job.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": job.ResourceVersion},
		{"op": "add", "path": "/spec/suspend", "value": true},
	})
	if err != nil {
		return s, err
	}
	if _, err := client.BatchV1().Jobs(namespace).Patch(ctx, job.Name, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		return s, sessionError("suspend Job", err)
	}
	s.Phase, s.StopRequested = "stopping", true
	observed, err := Status(ctx, client, namespace, id)
	if err != nil {
		if len(observed.Inventory) < len(s.Inventory) {
			return s, err
		}
		observed.StopRequested = true
		return observed, err
	}
	return observed, nil
}

func sessionPtr[T any](value T) *T { return &value }

func sessionError(operation string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", operation, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, context.DeadlineExceeded)
	}
	return fmt.Errorf("%s: Kubernetes API request failed (%s)", operation, classifyError(err).Reason)
}

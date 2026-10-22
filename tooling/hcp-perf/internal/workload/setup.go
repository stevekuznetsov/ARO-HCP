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

package workload

import (
	"context"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	flowcontrolv1 "k8s.io/api/flowcontrol/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
)

const (
	CanaryName = "canary"
	// CanaryImage is the pause image pinned in examples/seed.json. Its default
	// process needs no arguments, writable volumes, capabilities, or fixed UID.
	CanaryImage = "registry.k8s.io/pause@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a"
	// CanaryMirror is the verified pause v3.9 import for OCP-only guest pull secrets.
	CanaryMirror   = "arohcpocpdev.azurecr.io/stevekuznetsov/pause@sha256:7031c1b283388d2c2e09b57badb803c05ebed362dc88d84b480cc47f72a21097"
	setupManagedBy = "app.kubernetes.io/managed-by"
	// Seed's on-cluster contract; keep the workload primitive independent of
	// seed execution. Tests check these against the seed package constants.
	setupRunIDLabel        = "hcp-perf.azure.com/run-id"
	setupBenchmarkIdentity = "benchmark"
)

// SetupSpec identifies an already completed seed and its identities. Image must
// be the reviewed pause pin; arbitrary application commands are not supported.
type SetupSpec struct {
	Version         string            `json:"version"`
	ID              string            `json:"id"`
	NamespacePrefix string            `json:"namespacePrefix"`
	GuestNodes      int64             `json:"guestNodes"`
	Applications    int64             `json:"applications"`
	Image           string            `json:"image"`
	NodeSelector    map[string]string `json:"nodeSelector,omitempty"`
	Admission       *AdmissionPolicy  `json:"admission,omitempty"`
}

// AdmissionPolicy configures only run-owned Limited priority levels. Lending is
// always zero; borrowing retains the API default. Omission defaults the entire
// policy, not individual fields, so explicit zero shares are preserved.
type AdmissionPolicy struct {
	Benchmark AdmissionProfile `json:"benchmark"`
	Canary    AdmissionProfile `json:"canary"`
}

type AdmissionProfile struct {
	NominalConcurrencyShares int32 `json:"nominalConcurrencyShares"`
	// A nil Queue means Reject. The canary must have a bounded queue.
	Queue *QueuingConfig `json:"queue,omitempty"`
}

// QueuingConfig keeps shuffle sharding fixed at 8 queues and a hand size of 2.
type QueuingConfig struct {
	QueueLengthLimit int32 `json:"queueLengthLimit"`
}

func (s SetupSpec) admissionPolicy() AdmissionPolicy {
	if s.Admission == nil {
		return AdmissionPolicy{
			Benchmark: AdmissionProfile{NominalConcurrencyShares: 50},
			Canary:    AdmissionProfile{NominalConcurrencyShares: 5, Queue: &QueuingConfig{QueueLengthLimit: 10}},
		}
	}
	policy := *s.Admission
	// Results and rendered objects must not alias caller-owned configuration.
	if policy.Benchmark.Queue != nil {
		policy.Benchmark.Queue = ptr.To(*policy.Benchmark.Queue)
	}
	if policy.Canary.Queue != nil {
		policy.Canary.Queue = ptr.To(*policy.Canary.Queue)
	}
	return policy
}

func (s SetupSpec) Validate() error {
	if s.Version != "v1" || s.GuestNodes < 1 || s.GuestNodes > 100 {
		return fmt.Errorf("require version v1 and guestNodes in [1,100] for the initial APF profile")
	}
	if s.Applications < 0 || s.Applications > experiment.MaxApplications {
		return fmt.Errorf("require applications in [0,40000]")
	}
	if len(validation.IsDNS1123Label(s.ID)) != 0 || len(validation.IsDNS1123Label(s.CanaryNamespace())) != 0 {
		return fmt.Errorf("id must be a DNS label with room for hcp-perf-canary-")
	}
	if len(validation.IsDNS1123Label(s.NamespacePrefix)) != 0 || len(validation.IsDNS1123Label(s.NamespacePrefix+"-1999")) != 0 {
		return fmt.Errorf("namespacePrefix must be a DNS label with room for -1999")
	}
	if s.Image != CanaryImage && s.Image != CanaryMirror {
		return fmt.Errorf("image must match a reviewed pause digest: CanaryImage or CanaryMirror")
	}
	for key, value := range s.NodeSelector {
		if key == "kubernetes.io/hostname" || len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 || (key == corev1.LabelOSStable && value != "linux") {
			return fmt.Errorf("nodeSelector must contain valid labels, select Linux, and not pin a hostname")
		}
	}
	for n := int64(0); n < (s.Applications+19)/20; n++ {
		if fmt.Sprintf("%s-%d", s.NamespacePrefix, n) == s.CanaryNamespace() {
			return fmt.Errorf("canary namespace must be separate from seed namespaces")
		}
	}
	policy := s.admissionPolicy()
	var errs field.ErrorList
	path := field.NewPath("admission")
	for _, profile := range []struct {
		name string
		AdmissionProfile
	}{{"benchmark", policy.Benchmark}, {"canary", policy.Canary}} {
		// flowcontrol/v1 permits zero shares, but requires positive queue lengths.
		// int32 provides the schema upper bound for both tunable fields.
		if profile.NominalConcurrencyShares < 0 {
			errs = append(errs, field.Invalid(path.Child(profile.name, "nominalConcurrencyShares"), profile.NominalConcurrencyShares, "must be non-negative"))
		}
		if profile.Queue != nil && profile.Queue.QueueLengthLimit <= 0 {
			errs = append(errs, field.Invalid(path.Child(profile.name, "queue", "queueLengthLimit"), profile.Queue.QueueLengthLimit, "must be positive"))
		}
	}
	if int64(policy.Benchmark.NominalConcurrencyShares)+int64(policy.Canary.NominalConcurrencyShares) <= 0 {
		errs = append(errs, field.Invalid(path, policy, "sum of nominal concurrency shares must be positive"))
	}
	if policy.Canary.Queue == nil {
		errs = append(errs, field.Required(path.Child("canary", "queue"), "canary must use a bounded queue"))
	}
	return errs.ToAggregate()
}

func (s SetupSpec) CanaryNamespace() string { return "hcp-perf-canary-" + s.ID }

// SetupObjects renders the native create-only contract without cluster access.
// Every call returns fresh objects; callers must not infer server defaults here.
func SetupObjects(s SetupSpec) ([]runtime.Object, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return setupObjects(s), nil
}

type SetupResource struct {
	APIVersion      string    `json:"apiVersion"`
	Kind            string    `json:"kind"`
	Namespace       string    `json:"namespace,omitempty"`
	Name            string    `json:"name"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
}

// Created contains only acknowledged creations, never planned or adopted
// objects. An ambiguous failed create may still exist on the server. No rollback
// or retry is performed. Completed means created, not APF-observed or warmed up.
type SetupResult struct {
	Version                string `json:"version"`
	ID                     string `json:"id"`
	State                  string `json:"state"`
	CanaryNamespace        string `json:"canaryNamespace"`
	CanaryDeployment       string `json:"canaryDeployment"`
	BenchmarkPriorityLevel string `json:"benchmarkPriorityLevel"`
	CanaryPriorityLevel    string `json:"canaryPriorityLevel"`
	// Admission is the effective requested policy, not observed APF convergence.
	Admission AdmissionPolicy `json:"admission"`
	Created   []SetupResource `json:"created"`
	Warnings  []string        `json:"warnings"`
}

// setupObjects is shared by create-only setup and credential ownership checks.
func setupObjects(s SetupSpec) []runtime.Object {
	metadata := func(name, ns string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{setupManagedBy: "hcp-perf", setupRunIDLabel: s.ID}}
	}
	ns := s.CanaryNamespace()
	selector := map[string]string{setupRunIDLabel: s.ID, "app.kubernetes.io/name": CanaryName}
	nodeSelector := map[string]string{corev1.LabelOSStable: "linux"}
	for k, v := range s.NodeSelector {
		nodeSelector[k] = v
	}
	objects := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metadata(ns, "")},
		&corev1.ServiceAccount{ObjectMeta: metadata(CanaryName, ns), AutomountServiceAccountToken: ptr.To(false)},
		&rbacv1.Role{ObjectMeta: metadata(CanaryName, ns), Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, ResourceNames: []string{CanaryName}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments/scale"}, ResourceNames: []string{CanaryName}, Verbs: []string{"update"}},
			{APIGroups: []string{"apps"}, Resources: []string{"replicasets"}, Verbs: []string{"list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list", "watch"}},
		}},
		&rbacv1.RoleBinding{ObjectMeta: metadata(CanaryName, ns), RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: CanaryName}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: ns, Name: CanaryName}}},
		&appsv1.Deployment{ObjectMeta: metadata(CanaryName, ns), Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(0)), RevisionHistoryLimit: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: ptr.To(intstr.FromInt32(1)), MaxUnavailable: ptr.To(intstr.FromInt32(0))}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: selector}, Spec: corev1.PodSpec{
				ServiceAccountName: CanaryName, AutomountServiceAccountToken: ptr.To(false), NodeSelector: nodeSelector,
				SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Containers: []corev1.Container{{Name: "app", Image: s.Image, ImagePullPolicy: corev1.PullIfNotPresent,
					// SCC supplies the namespace UID. Pause needs no fixed UID or probe;
					// readiness measures scheduling and a running, Ready container.
					SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m"), corev1.ResourceMemory: resource.MustParse("8Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")}},
				}},
			}},
		}},
	}
	policy := s.admissionPolicy()
	for _, profile := range []struct {
		name string
		AdmissionProfile
	}{{"benchmark", policy.Benchmark}, {"canary", policy.Canary}} {
		response := flowcontrolv1.LimitResponse{Type: flowcontrolv1.LimitResponseTypeReject}
		if profile.Queue != nil {
			response.Type = flowcontrolv1.LimitResponseTypeQueue
			response.Queuing = &flowcontrolv1.QueuingConfiguration{Queues: 8, HandSize: 2, QueueLengthLimit: profile.Queue.QueueLengthLimit}
		}
		objects = append(objects, &flowcontrolv1.PriorityLevelConfiguration{ObjectMeta: metadata("hcp-perf-"+profile.name+"-"+s.ID, ""), Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
			Type:    flowcontrolv1.PriorityLevelEnablementLimited,
			Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{NominalConcurrencyShares: ptr.To(profile.NominalConcurrencyShares), LendablePercent: ptr.To(int32(0)), LimitResponse: response},
		}})
	}
	// One exact subject and namespace per schema avoids the APF subject limit
	// without broadening to every service account in a namespace or cluster.
	for n := int64(-1); n < (s.Applications+19)/20; n++ {
		namespace, identity, profile, precedence := ns, CanaryName, "canary", int32(200)
		name := "hcp-perf-canary-" + s.ID
		if n >= 0 {
			namespace, identity, profile, precedence = fmt.Sprintf("%s-%d", s.NamespacePrefix, n), setupBenchmarkIdentity, "benchmark", 300
			name = fmt.Sprintf("hcp-perf-benchmark-%s-%d", s.ID, n)
		}
		objects = append(objects, &flowcontrolv1.FlowSchema{ObjectMeta: metadata(name, ""), Spec: flowcontrolv1.FlowSchemaSpec{
			PriorityLevelConfiguration: flowcontrolv1.PriorityLevelConfigurationReference{Name: "hcp-perf-" + profile + "-" + s.ID},
			MatchingPrecedence:         precedence, DistinguisherMethod: &flowcontrolv1.FlowDistinguisherMethod{Type: flowcontrolv1.FlowDistinguisherMethodByUserType},
			Rules: []flowcontrolv1.PolicyRulesWithSubjects{{
				Subjects: []flowcontrolv1.Subject{{Kind: flowcontrolv1.SubjectKindServiceAccount, ServiceAccount: &flowcontrolv1.ServiceAccountSubject{Namespace: namespace, Name: identity}}},
				ResourceRules: []flowcontrolv1.ResourcePolicyRule{
					{Verbs: []string{"get", "list", "watch", "patch", "update"}, APIGroups: []string{"apps"}, Resources: []string{"deployments", "deployments/scale", "replicasets"}, Namespaces: []string{namespace}},
					{Verbs: []string{"get", "list", "watch"}, APIGroups: []string{""}, Resources: []string{"pods"}, Namespaces: []string{namespace}},
				},
			}},
		}})
	}
	return objects
}

// Configure uses only the supplied guest-admin client. All seed ownership and
// name collisions are checked before creating anything. Existing resources are
// never updated, adopted or deleted, including Kubernetes' system APF objects.
func Configure(ctx context.Context, client kubernetes.Interface, spec SetupSpec) (result SetupResult, err error) {
	result = SetupResult{Version: "v1", ID: spec.ID, State: "failed", CanaryNamespace: spec.CanaryNamespace(), CanaryDeployment: CanaryName,
		BenchmarkPriorityLevel: "hcp-perf-benchmark-" + spec.ID, CanaryPriorityLevel: "hcp-perf-canary-" + spec.ID, Created: []SetupResource{},
		Admission: spec.admissionPolicy(),
		Warnings: []string{
			"APF policy is recorded in admission, lending=0; shares are not fixed seats. System APF is unchanged. Classification and controller convergence are not verified.",
			"No image pre-pull or preloader is performed. Complete image pulls and warmup on eligible nodes outside the scored window. Pause readiness measures a running process, not an application endpoint.",
			"Resources are preserved on success and failure; no adoption, rollback, or automatic cleanup. Failed creates may have reached the server; inspect before retrying.",
		}}
	if err := spec.Validate(); err != nil {
		return result, err
	}
	if client == nil {
		return result, fmt.Errorf("guest admin client is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for n := int64(0); n < (spec.Applications+19)/20; n++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ns := fmt.Sprintf("%s-%d", spec.NamespacePrefix, n)
		namespace, err := client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err != nil {
			return result, fmt.Errorf("read seed namespace %s: %w", ns, err)
		}
		if namespace.DeletionTimestamp != nil || namespace.Labels[setupRunIDLabel] != spec.ID || namespace.Labels["kube-burner-uuid"] != spec.ID || namespace.Labels["kube-burner-job"] != spec.NamespacePrefix {
			return result, fmt.Errorf("namespace %s is not owned by this seed", ns)
		}
		sa, err := client.CoreV1().ServiceAccounts(ns).Get(ctx, setupBenchmarkIdentity, metav1.GetOptions{})
		if err != nil {
			return result, fmt.Errorf("read benchmark identity in %s: %w", ns, err)
		}
		if sa.DeletionTimestamp != nil || sa.Labels[setupRunIDLabel] != spec.ID || sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
			return result, fmt.Errorf("service account in %s is not a seed benchmark identity", ns)
		}
	}
	objects := setupObjects(spec)
	// The same typed operations are used in both passes; no discovery, dynamic
	// clients, server-side apply, or update-on-AlreadyExists behavior is needed.
	for _, create := range []bool{false, true} {
		for _, object := range objects {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			m, _ := meta.Accessor(object)
			var actual runtime.Object
			var err error
			var apiVersion, kind string
			switch o := object.(type) {
			case *corev1.Namespace:
				apiVersion, kind = "v1", "Namespace"
				if create {
					actual, err = client.CoreV1().Namespaces().Create(ctx, o, metav1.CreateOptions{})
				} else {
					actual, err = client.CoreV1().Namespaces().Get(ctx, o.Name, metav1.GetOptions{})
				}
			case *corev1.ServiceAccount:
				apiVersion, kind = "v1", "ServiceAccount"
				if create {
					actual, err = client.CoreV1().ServiceAccounts(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
				} else {
					actual, err = client.CoreV1().ServiceAccounts(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
				}
			case *rbacv1.Role:
				apiVersion, kind = rbacv1.SchemeGroupVersion.String(), "Role"
				if create {
					actual, err = client.RbacV1().Roles(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
				} else {
					actual, err = client.RbacV1().Roles(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
				}
			case *rbacv1.RoleBinding:
				apiVersion, kind = rbacv1.SchemeGroupVersion.String(), "RoleBinding"
				if create {
					actual, err = client.RbacV1().RoleBindings(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
				} else {
					actual, err = client.RbacV1().RoleBindings(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
				}
			case *appsv1.Deployment:
				apiVersion, kind = appsv1.SchemeGroupVersion.String(), "Deployment"
				if create {
					actual, err = client.AppsV1().Deployments(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
				} else {
					actual, err = client.AppsV1().Deployments(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
				}
			case *flowcontrolv1.PriorityLevelConfiguration:
				apiVersion, kind = flowcontrolv1.SchemeGroupVersion.String(), "PriorityLevelConfiguration"
				if create {
					actual, err = client.FlowcontrolV1().PriorityLevelConfigurations().Create(ctx, o, metav1.CreateOptions{})
				} else {
					actual, err = client.FlowcontrolV1().PriorityLevelConfigurations().Get(ctx, o.Name, metav1.GetOptions{})
				}
			case *flowcontrolv1.FlowSchema:
				apiVersion, kind = flowcontrolv1.SchemeGroupVersion.String(), "FlowSchema"
				if create {
					actual, err = client.FlowcontrolV1().FlowSchemas().Create(ctx, o, metav1.CreateOptions{})
				} else {
					actual, err = client.FlowcontrolV1().FlowSchemas().Get(ctx, o.Name, metav1.GetOptions{})
				}
			}
			if !create {
				if apierrors.IsNotFound(err) {
					continue
				}
				if err == nil {
					return result, fmt.Errorf("refuse existing %s %s/%s", kind, m.GetNamespace(), m.GetName())
				}
				return result, fmt.Errorf("preflight %s %s/%s: %w", kind, m.GetNamespace(), m.GetName(), err)
			}
			if err != nil {
				return result, fmt.Errorf("create %s %s/%s: %w", kind, m.GetNamespace(), m.GetName(), err)
			}
			created, _ := meta.Accessor(actual)
			result.Created = append(result.Created, SetupResource{APIVersion: apiVersion, Kind: kind, Namespace: created.GetNamespace(), Name: created.GetName(), UID: created.GetUID(), ResourceVersion: created.GetResourceVersion()})
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.State = "completed"
	return result, nil
}

// VerifyCanaryIdentity gates separate TokenRequests on owned, exact local RBAC.
// It cannot prove absence of additional external role bindings or APF overrides.
func VerifyCanaryIdentity(ctx context.Context, client kubernetes.Interface, spec SetupSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("guest admin client is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ns := spec.CanaryNamespace()
	namespace, err := client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return err
	}
	sa, err := client.CoreV1().ServiceAccounts(ns).Get(ctx, CanaryName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	role, err := client.RbacV1().Roles(ns).Get(ctx, CanaryName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	binding, err := client.RbacV1().RoleBindings(ns).Get(ctx, CanaryName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	deployment, err := client.AppsV1().Deployments(ns).Get(ctx, CanaryName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for _, object := range []metav1.Object{namespace, sa, role, binding, deployment} {
		if object.GetDeletionTimestamp() != nil || object.GetLabels()[setupManagedBy] != "hcp-perf" || object.GetLabels()[setupRunIDLabel] != spec.ID {
			return fmt.Errorf("canary resource is not active and owned by this run")
		}
	}
	expected := setupObjects(spec)
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken || !reflect.DeepEqual(role.Rules, expected[2].(*rbacv1.Role).Rules) || binding.RoleRef != expected[3].(*rbacv1.RoleBinding).RoleRef || !reflect.DeepEqual(binding.Subjects, expected[3].(*rbacv1.RoleBinding).Subjects) {
		return fmt.Errorf("canary identity must have the exact setup role and binding")
	}
	return ctx.Err()
}

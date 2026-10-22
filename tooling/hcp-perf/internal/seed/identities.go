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

package seed

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
)

// IdentityObjects is the exact native benchmark SA/RBAC creation contract.
func IdentityObjects(spec Spec, n int64) ([]runtime.Object, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if n < 0 || n >= (spec.Applications+19)/20 {
		return nil, fmt.Errorf("invalid seed identity index")
	}
	ns := fmt.Sprintf("%s-%d", spec.NamespacePrefix, n)
	metadata := metav1.ObjectMeta{Name: IdentityName, Namespace: ns, Labels: map[string]string{RunIDLabel: spec.ID}}
	automount := false
	names := make([]string, min(20, spec.Applications-n*20))
	for app := range names {
		names[app] = fmt.Sprintf("app-%06d", n*20+int64(app))
	}
	return []runtime.Object{
		&corev1.ServiceAccount{ObjectMeta: metadata, AutomountServiceAccountToken: &automount},
		&rbacv1.Role{ObjectMeta: metadata, Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, ResourceNames: names, Verbs: []string{"get", "update"}},
			{APIGroups: []string{"apps"}, Resources: []string{"replicasets"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
		}},
		&rbacv1.RoleBinding{ObjectMeta: metadata, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: IdentityName, Namespace: ns}}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: IdentityName}},
	}, nil
}

// ConfigureIdentities is a separate, create-only post-seed step. It grants each
// namespace's benchmark SA only turnover access: get/update its up to 20 Deployments
// and observe Pods/ReplicaSets. It never adopts existing RBAC, requests tokens,
// stores credentials, grants Secret access, or configures an APF exemption.
// Errors leave partial creations in place and must not be blindly retried.
func ConfigureIdentities(ctx context.Context, client kubernetes.Interface, spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if spec.Applications == 0 {
		return ctx.Err()
	}
	if client == nil {
		return fmt.Errorf("client is required")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
	defer cancel()
	for n := int64(0); n < (spec.Applications+19)/20; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		ns := fmt.Sprintf("%s-%d", spec.NamespacePrefix, n)
		namespace, err := client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get seed namespace %s: %w", ns, err)
		}
		if namespace.DeletionTimestamp != nil || namespace.Labels[RunIDLabel] != spec.ID || namespace.Labels["kube-burner-uuid"] != spec.ID || namespace.Labels["kube-burner-job"] != spec.NamespacePrefix {
			return fmt.Errorf("namespace %s is not an active namespace owned by this seed", ns)
		}
		objects, err := IdentityObjects(spec, n)
		if err != nil {
			return err
		}
		if _, err := client.CoreV1().ServiceAccounts(ns).Create(ctx, objects[0].(*corev1.ServiceAccount), metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create serviceaccount %s/%s: %w", ns, IdentityName, err)
		}
		if _, err := client.RbacV1().Roles(ns).Create(ctx, objects[1].(*rbacv1.Role), metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create role %s/%s: %w", ns, IdentityName, err)
		}
		if _, err := client.RbacV1().RoleBindings(ns).Create(ctx, objects[2].(*rbacv1.RoleBinding), metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create rolebinding %s/%s: %w", ns, IdentityName, err)
		}
	}
	return ctx.Err()
}

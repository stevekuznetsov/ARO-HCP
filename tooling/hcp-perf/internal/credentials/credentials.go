// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package credentials

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
)

// MintCredentials returns static, scoped authentication only. The caller must
// verify the exact identity before issuance and reserve private evidence before
// this mutating API call. Neither errors nor expiry evidence contain the token.
func MintCredentials(ctx context.Context, client kubernetes.Interface, admin *rest.Config, namespace, account string, expiration time.Duration) (*rest.Config, time.Time, error) {
	if client == nil || admin == nil || expiration < 600*time.Second || expiration > 24*time.Hour || expiration%time.Second != 0 {
		return nil, time.Time{}, errors.New("require client, config and whole-second expiration in [600s,24h]")
	}
	ca := admin.CAData
	if len(ca) == 0 && admin.CAFile != "" {
		var err error
		ca, err = os.ReadFile(admin.CAFile)
		if err != nil {
			return nil, time.Time{}, errors.New("read guest CA")
		}
	}
	seconds := int64(expiration / time.Second)
	token, err := client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, account, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{}, ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
	if err != nil {
		return nil, time.Time{}, errors.Join(errors.New("request scoped token failed"), ctx.Err())
	}
	if token.Status.Token == "" {
		return nil, time.Time{}, fmt.Errorf("token for %s: empty token", namespace)
	}
	expires := token.Status.ExpirationTimestamp.Time
	if expires.IsZero() {
		return nil, expires, fmt.Errorf("token for %s: zero expiration", namespace)
	}
	if !expires.After(time.Now()) {
		return nil, expires, fmt.Errorf("token for %s: expired at receipt", namespace)
	}
	return &rest.Config{Host: admin.Host, TLSClientConfig: rest.TLSClientConfig{CAData: append([]byte(nil), ca...), ServerName: admin.ServerName, Insecure: admin.Insecure}, BearerToken: token.Status.Token, UserAgent: "hcp-perf/v1"}, expires, nil
}

// VerifySeedIdentity refuses adoption of a same-named account or broadened role.
func VerifySeedIdentity(ctx context.Context, client kubernetes.Interface, spec seed.Spec, n int64) error {
	if client == nil || n < 0 || n >= (spec.Applications+19)/20 {
		return errors.New("invalid seed identity index")
	}
	ns := fmt.Sprintf("%s-%d", spec.NamespacePrefix, n)
	namespace, err := client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if namespace.DeletionTimestamp != nil || namespace.Labels[seed.RunIDLabel] != spec.ID || namespace.Labels["kube-burner-uuid"] != spec.ID || namespace.Labels["kube-burner-job"] != spec.NamespacePrefix {
		return errors.New("namespace is not owned by seed")
	}
	sa, err := client.CoreV1().ServiceAccounts(ns).Get(ctx, seed.IdentityName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if sa.DeletionTimestamp != nil || sa.Labels[seed.RunIDLabel] != spec.ID || sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		return errors.New("service account is not a seed identity")
	}
	role, err := client.RbacV1().Roles(ns).Get(ctx, seed.IdentityName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	names := make([]string, min(20, spec.Applications-n*20))
	for app := range names {
		names[app] = fmt.Sprintf("app-%06d", n*20+int64(app))
	}
	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, ResourceNames: names, Verbs: []string{"get", "update"}},
		{APIGroups: []string{"apps"}, Resources: []string{"replicasets"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
	}
	if role.DeletionTimestamp != nil || role.Labels[seed.RunIDLabel] != spec.ID || !reflect.DeepEqual(role.Rules, rules) {
		return errors.New("role is not the exact seed role")
	}
	binding, err := client.RbacV1().RoleBindings(ns).Get(ctx, seed.IdentityName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if binding.DeletionTimestamp != nil || binding.Labels[seed.RunIDLabel] != spec.ID || binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: seed.IdentityName}) || !reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: seed.IdentityName, Namespace: ns}}) {
		return errors.New("binding is not the exact seed binding")
	}
	return nil
}

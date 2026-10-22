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
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func namespaces(s Spec) []runtime.Object {
	var objects []runtime.Object
	for i := int64(0); i < (s.Applications+19)/20; i++ {
		objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", s.NamespacePrefix, i), Labels: map[string]string{RunIDLabel: s.ID, "kube-burner-uuid": s.ID, "kube-burner-job": s.NamespacePrefix}}})
	}
	return objects
}

func TestConfigureIdentities(t *testing.T) {
	for _, apps := range []int64{0, 12, 21, 40, 120} {
		t.Run(fmt.Sprint(apps), func(t *testing.T) {
			spec := example(t)
			spec.GuestNodes = 3
			spec.Applications = apps
			client := fake.NewClientset(namespaces(spec)...)
			ctx := context.Background()
			if err := ConfigureIdentities(ctx, client, spec); err != nil {
				t.Fatal(err)
			}
			if len(client.Actions()) != int((apps+19)/20)*4 {
				t.Fatal("unexpected number of identity operations", client.Actions())
			}
			if apps == 0 {
				if err := ConfigureIdentities(ctx, nil, spec); err != nil {
					t.Fatal("empty identities require no client", err)
				}
				return
			}
			for n := int64(0); n < (apps+19)/20; n++ {
				ns := fmt.Sprintf("%s-%d", spec.NamespacePrefix, n)
				sa, err := client.CoreV1().ServiceAccounts(ns).Get(ctx, IdentityName, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if *sa.AutomountServiceAccountToken || len(sa.Secrets) != 0 || sa.Labels[RunIDLabel] != spec.ID {
					t.Fatal(sa)
				}
				role, err := client.RbacV1().Roles(ns).Get(ctx, IdentityName, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				count := min(20, apps-n*20)
				if len(role.Rules) != 3 || int64(len(role.Rules[0].ResourceNames)) != count {
					t.Fatal(role)
				}
				for app, name := range role.Rules[0].ResourceNames {
					if name != fmt.Sprintf("app-%06d", n*20+int64(app)) {
						t.Fatal("incorrect global application name", name)
					}
				}
				binding, err := client.RbacV1().RoleBindings(ns).Get(ctx, IdentityName, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if len(binding.Subjects) != 1 || binding.Subjects[0].Namespace != ns || binding.Subjects[0].Name != IdentityName || binding.RoleRef.Kind != "Role" {
					t.Fatal(binding)
				}
			}
			for _, action := range client.Actions() {
				if action.GetSubresource() != "" || (action.GetVerb() != "get" && action.GetVerb() != "create") {
					t.Fatal("unexpected mutation/token request", action)
				}
			}
			if err := ConfigureIdentities(ctx, client, spec); !apierrors.IsAlreadyExists(err) {
				t.Fatal("existing identity adopted", err)
			}
		})
	}
}

func TestIdentityCollisions(t *testing.T) {
	spec := example(t)
	spec.Applications = 40
	ns := spec.NamespacePrefix + "-0"
	for _, object := range []runtime.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: IdentityName, Namespace: ns}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: IdentityName, Namespace: ns}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: IdentityName, Namespace: ns}},
	} {
		client := fake.NewClientset(append(namespaces(spec), object)...)
		if err := ConfigureIdentities(context.Background(), client, spec); !apierrors.IsAlreadyExists(err) {
			t.Fatal("collision not rejected", err)
		}
		for _, action := range client.Actions() {
			if action.GetVerb() != "get" && action.GetVerb() != "create" {
				t.Fatal("collision caused repair/rollback", action)
			}
		}
	}
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	if err := ConfigureIdentities(context.Background(), client, spec); err == nil {
		t.Fatal("foreign namespace accepted")
	}
	if len(client.Actions()) != 1 {
		t.Fatal("mutated foreign namespace")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ConfigureIdentities(ctx, client, spec); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	spec.GuestNodes = 0
	spec.Applications = 0
	client = fake.NewClientset()
	if err := ConfigureIdentities(context.Background(), client, spec); err != nil || len(client.Actions()) != 0 {
		t.Fatal("empty identities should do nothing", err)
	}
}

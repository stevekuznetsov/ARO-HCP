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

// Operator-only personal test helper. Credentials remain in memory; output is
// limited to identity references, authorization decisions and token expiration.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "personal management kubeconfig")
	hcpNamespace := flag.String("hcp-namespace", "", "authorized HCP namespace")
	server := flag.String("server", "", "HTTPS guest KAS URL, including a loopback port-forward")
	provision := flag.Bool("provision", false, "create a dedicated guest metrics observer and management token Secret")
	flag.Parse()
	if err := run(*kubeconfig, *hcpNamespace, *server, *provision); err != nil {
		// Never print Kubernetes/transport errors that could echo credentials.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(kubeconfig, namespace, server string, provision bool) error {
	if kubeconfig == "" || namespace == "" || server == "" {
		return fmt.Errorf("kubeconfig, hcp-namespace and server are required")
	}
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return fmt.Errorf("server must be a bare HTTPS origin without credentials, path or query")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("load management kubeconfig failed")
	}
	config.Timeout = 20 * time.Second
	management, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("management client failed")
	}
	secret, err := management.CoreV1().Secrets(namespace).Get(ctx, "admin-kubeconfig", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read admin-kubeconfig reference failed")
	}
	guestConfig, err := clientcmd.RESTConfigFromKubeConfig(secret.Data["kubeconfig"])
	if err != nil {
		return fmt.Errorf("parse admin-kubeconfig failed")
	}
	guestConfig.Host, guestConfig.ServerName, guestConfig.Timeout = server, "kube-apiserver", 20*time.Second
	if guestConfig.Insecure || len(guestConfig.CAData) == 0 {
		return fmt.Errorf("guest kubeconfig must contain verified CA trust")
	}
	if guestConfig.ExecProvider != nil || guestConfig.AuthProvider != nil || guestConfig.CertFile != "" || guestConfig.KeyFile != "" || guestConfig.BearerTokenFile != "" {
		return fmt.Errorf("guest admin kubeconfig must use inline credentials, not plugins or local files")
	}
	guest, err := kubernetes.NewForConfig(guestConfig)
	if err != nil {
		return fmt.Errorf("guest client failed")
	}
	binding, err := guest.RbacV1().ClusterRoleBindings().Get(ctx, "hypershift-metrics-client", metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read guest metrics binding failed")
	}
	result := map[string]any{"nativeMetricsBindingPresent": err == nil}
	if err == nil {
		result["nativeMetricsBinding"] = map[string]any{"roleRef": binding.RoleRef, "subjects": binding.Subjects}
	}
	metrics, err := management.CoreV1().Secrets(namespace).Get(ctx, "metrics-client", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read metrics-client reference failed")
	}
	observerConfig := &rest.Config{Host: server, Timeout: 20 * time.Second, TLSClientConfig: rest.TLSClientConfig{CAData: guestConfig.CAData, ServerName: "kube-apiserver", CertData: metrics.Data["tls.crt"], KeyData: metrics.Data["tls.key"]}}
	httpClient, err := rest.HTTPClientFor(observerConfig)
	if err != nil {
		return fmt.Errorf("metrics-client TLS setup failed")
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server+"/metrics", nil)
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("metrics-client GET metrics failed")
	}
	result["nativeMetricsHTTPStatus"] = response.StatusCode
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if !provision {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	const name = "hcp-perf-observer"
	labels := map[string]string{"app.kubernetes.io/managed-by": "hcp-perf", "hcp-perf.azure.com/purpose": "metrics-observer"}
	// Create-only: never adopt, expand or mutate an existing identity or binding.
	if _, err := guest.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create observer namespace failed; existing resources are never adopted")
	}
	automount := false
	if _, err := guest.CoreV1().ServiceAccounts(name).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, AutomountServiceAccountToken: &automount}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create observer service account failed; partial resources retained")
	}
	if _, err := guest.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Rules: []rbacv1.PolicyRule{{NonResourceURLs: []string{"/metrics", "/metrics/resources"}, Verbs: []string{"get"}}}}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create observer role failed; partial resources retained")
	}
	if _, err := guest.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: name, Name: name}}}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create observer binding failed; partial resources retained")
	}
	expiration := int64(12 * 60 * 60)
	token, err := guest.CoreV1().ServiceAccounts(name).CreateToken(ctx, name, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiration}}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("mint observer token failed; partial resources retained")
	}
	observerConfig.CertData, observerConfig.KeyData = nil, nil
	observerConfig.BearerToken = token.Status.Token
	observer, err := kubernetes.NewForConfig(observerConfig)
	if err != nil {
		return fmt.Errorf("observer client failed")
	}
	for _, check := range []struct {
		name    string
		spec    authorizationv1.SelfSubjectAccessReviewSpec
		allowed bool
	}{
		{"metrics", authorizationv1.SelfSubjectAccessReviewSpec{NonResourceAttributes: &authorizationv1.NonResourceAttributes{Verb: "get", Path: "/metrics"}}, true},
		{"metricsResources", authorizationv1.SelfSubjectAccessReviewSpec{NonResourceAttributes: &authorizationv1.NonResourceAttributes{Verb: "get", Path: "/metrics/resources"}}, true},
		{"listSecrets", authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: "list", Resource: "secrets"}}, false},
		{"createPods", authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: "create", Resource: "pods", Namespace: name}}, false},
	} {
		decision, err := observer.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: check.spec}, metav1.CreateOptions{})
		if err != nil || decision.Status.Allowed != check.allowed {
			return fmt.Errorf("observer scope verification failed for %s; token not persisted", check.name)
		}
		result[check.name+"Allowed"] = decision.Status.Allowed
	}
	httpClient, err = rest.HTTPClientFor(observerConfig)
	if err != nil {
		return fmt.Errorf("observer TLS setup failed")
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, server+"/metrics", nil)
	response, err = httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("observer metrics request failed")
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("observer metrics authorization failed with HTTP %d", response.StatusCode)
	}
	immutable := true
	if _, err := management.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-token", Labels: labels}, Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"token": []byte(token.Status.Token)}}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create management observer token Secret failed; partial resources retained")
	}
	result["observerMetricsHTTPStatus"] = response.StatusCode
	result["tokenSecret"] = map[string]string{"kind": "Secret", "namespace": namespace, "name": name + "-token", "key": "token"}
	result["tokenExpiresAt"] = token.Status.ExpirationTimestamp
	return json.NewEncoder(os.Stdout).Encode(result)
}

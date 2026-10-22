// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
)

func TestAPILoadCLI(t *testing.T) {
	t.Setenv("KUBECONFIG", "/must-not-use-ambient")
	t.Setenv("HCP_PERF_BEARER_TOKEN", "must-not-use-ambient")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer namespace-token" || r.URL.Path != "/apis/apps/v1/namespaces/bench-0/deployments/app-000000" {
			t.Errorf("wrong request identity/path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, "{}")
	}))
	defer server.Close()
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: guest
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: benchmark
  user:
    token: namespace-token
contexts:
- name: benchmark
  context:
    cluster: guest
    user: benchmark
    namespace: bench-0
current-context: benchmark
`, server.URL, base64.StdEncoding.EncodeToString(ca))
	if err := os.WriteFile(filepath.Join(dir, "bench-0.kubeconfig"), []byte(kubeconfig), 0600); err != nil {
		t.Fatal(err)
	}
	spec := apiload.Spec{Version: "v1", DurationSeconds: 1, RatePerSecond: 2, MaxInFlight: 1, RequestTimeoutSeconds: 1,
		Namespaces: []string{"bench-0"}, Targets: []apiload.Target{{Namespace: "bench-0", APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Name: "app-000000", Weight: 1}}}
	data, _ := json.Marshal(spec)
	input := filepath.Join(dir, "input.json")
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"api-load", "run", "--input", input, "--credentials-dir", dir, "--output", filepath.Join(dir, "output")}
	if err := runAPILoad(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	var result apiload.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Offered != 2 || result.Successful == 0 {
		t.Fatalf("result: %s %v", out.String(), err)
	}
	if strings.Contains(out.String(), "token") || strings.Contains(out.String(), server.URL) {
		t.Fatal("credential data in output")
	}
	for _, args := range [][]string{nil, {"api-load"}, {"api-load", "plan"}, {"api-load", "run"}, append(append([]string{}, args...), "extra"), append(append([]string{}, args...), "--kubeconfig", "admin")} {
		out.Reset()
		if err := runAPILoad(context.Background(), args, &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid CLI accepted", args, err)
		}
	}
}

func TestAPILoadStrictInput(t *testing.T) {
	data, err := os.ReadFile("examples/api-load.json")
	if err != nil {
		t.Fatal(err)
	}
	var example apiload.Spec
	if err := decodeFile("examples/api-load.json", &example); err != nil {
		t.Fatal(err)
	}
	if _, err := example.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"unknown":    strings.Replace(string(data), `"seed": 42`, `"seed": 42, "qps": 5`, 1),
		"duplicate":  strings.Replace(string(data), `"seed": 42`, `"seed": 42, "seed": 5`, 1),
		"fractional": strings.Replace(string(data), `"ratePerSecond": 640`, `"ratePerSecond": 0.5`, 1),
		"case":       strings.Replace(string(data), `"version"`, `"Version"`, 1),
		"missing":    strings.Replace(string(data), `"maxInFlight": 128,`, "", 1),
		"null":       "null", "trailing": string(data) + "{}",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			input, output := filepath.Join(dir, "input.json"), filepath.Join(dir, "output")
			if err := os.WriteFile(input, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := runAPILoad(context.Background(), []string{"api-load", "run", "--input", input, "--credentials-dir", dir, "--output", output}, &out); err == nil || out.Len() != 0 {
				t.Fatal("invalid input accepted", err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("invalid input created output", err)
			}
		})
	}
}

func TestAPILoadCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KUBECONFIG", filepath.Join(dir, "ambient"))
	for _, ns := range []string{"../outside", "", "missing"} {
		if _, err := apiLoadNamespaceConfig(dir, ns); err == nil {
			t.Fatal("invalid credentials accepted", ns)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "bench-0.kubeconfig"), []byte("SECRET-CREDENTIAL: [malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := apiLoadNamespaceConfig(dir, "bench-0"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("parser payload leaked", err)
	}
}

func TestAPILoadExplicitCredentialsOnly(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	valid := clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"guest": {Server: server.URL, CertificateAuthorityData: ca}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"benchmark": {Token: "explicit-token"}},
		Contexts:       map[string]*clientcmdapi.Context{"benchmark": {Cluster: "guest", AuthInfo: "benchmark", Namespace: "bench-0"}},
		CurrentContext: "benchmark",
	}
	ambient := valid.DeepCopy()
	ambient.AuthInfos["benchmark"].Token = "ambient-token"
	data, err := clientcmd.Write(*ambient)
	if err != nil {
		t.Fatal(err)
	}
	ambientPath := filepath.Join(dir, "ambient.kubeconfig")
	if err := os.WriteFile(ambientPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", ambientPath)
	t.Setenv("KUBERNETES_MASTER", server.URL)
	t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", strings.Split(strings.TrimPrefix(server.URL, "https://"), ":")[1])
	if err := os.WriteFile(filepath.Join(dir, "bench-0.kubeconfig"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := apiLoadNamespaceConfig(dir, "bench-0"); err == nil {
		t.Fatal("empty file fell back to ambient configuration")
	}
	marker := filepath.Join(dir, "exec-was-spawned")
	plugin := &clientcmdapi.ExecConfig{Command: "/bin/sh", Args: []string{"-c", `touch "$1"`, "plugin", marker}, APIVersion: "client.authentication.k8s.io/v1", InteractiveMode: clientcmdapi.NeverExecInteractiveMode}
	for name, mutate := range map[string]func(*clientcmdapi.Config){
		"empty":                func(c *clientcmdapi.Config) { *c = clientcmdapi.Config{} },
		"no current context":   func(c *clientcmdapi.Config) { c.CurrentContext = "" },
		"missing context":      func(c *clientcmdapi.Config) { c.Contexts = nil },
		"no cluster reference": func(c *clientcmdapi.Config) { c.Contexts["benchmark"].Cluster = "" },
		"missing cluster":      func(c *clientcmdapi.Config) { c.Clusters = nil },
		"empty server":         func(c *clientcmdapi.Config) { c.Clusters["guest"].Server = "" },
		"namespace absent":     func(c *clientcmdapi.Config) { c.Contexts["benchmark"].Namespace = "" },
		"namespace mismatch":   func(c *clientcmdapi.Config) { c.Contexts["benchmark"].Namespace = "other" },
		"no user reference":    func(c *clientcmdapi.Config) { c.Contexts["benchmark"].AuthInfo = "" },
		"missing user":         func(c *clientcmdapi.Config) { c.AuthInfos = nil },
		"no token":             func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].Token = "" },
		"blank token":          func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].Token = " \n" },
		"token file":           func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].TokenFile = "must-not-read" },
		"certificate file":     func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].ClientCertificate = "must-not-read" },
		"certificate data":     func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].ClientCertificateData = []byte("secret-cert") },
		"key file":             func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].ClientKey = "must-not-read" },
		"key data":             func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].ClientKeyData = []byte("secret-key") },
		"username":             func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].Username = "admin" },
		"password":             func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].Password = "secret-password" },
		"impersonate":          func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].Impersonate = "admin" },
		"impersonate UID":      func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].ImpersonateUID = "admin" },
		"impersonate groups":   func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].ImpersonateGroups = []string{"system:masters"} },
		"impersonate extra": func(c *clientcmdapi.Config) {
			c.AuthInfos["benchmark"].ImpersonateUserExtra = map[string][]string{"key": {"value"}}
		},
		"exec with token": func(c *clientcmdapi.Config) { c.AuthInfos["benchmark"].Exec = plugin },
		"exec only": func(c *clientcmdapi.Config) {
			c.AuthInfos["benchmark"].Token = ""
			c.AuthInfos["benchmark"].Exec = plugin
		},
		"unused exec": func(c *clientcmdapi.Config) { c.AuthInfos["unused"] = &clientcmdapi.AuthInfo{Exec: plugin} },
		"auth provider": func(c *clientcmdapi.Config) {
			c.AuthInfos["benchmark"].AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "oidc"}
		},
		"insecure": func(c *clientcmdapi.Config) { c.Clusters["guest"].InsecureSkipTLSVerify = true },
		"HTTP": func(c *clientcmdapi.Config) {
			c.Clusters["guest"].Server = strings.Replace(server.URL, "https:", "http:", 1)
		},
		"query": func(c *clientcmdapi.Config) { c.Clusters["guest"].Server += "/prefix?secret=value" },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid.DeepCopy()
			mutate(c)
			data, err := clientcmd.Write(*c)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "bench-0.kubeconfig"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := apiLoadNamespaceConfig(dir, "bench-0"); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("invalid credentials accepted or leaked", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("credential rejection contacted the API server")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("exec plugin was spawned", err)
	}
	data, err = clientcmd.Write(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bench-0.kubeconfig"), data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := apiLoadNamespaceConfig(dir, "bench-0")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != server.URL || c.BearerToken != "explicit-token" || c.BearerTokenFile != "" || c.ExecProvider != nil || c.AuthProvider != nil || c.Insecure || !bytes.Equal(c.CAData, ca) {
		t.Fatal("explicit credentials or TLS settings were not preserved")
	}
	valid.Clusters["guest"].CertificateAuthorityData = nil
	valid.Clusters["guest"].CertificateAuthority = "ca.crt"
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0600); err != nil {
		t.Fatal(err)
	}
	data, err = clientcmd.Write(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bench-0.kubeconfig"), data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err = apiLoadNamespaceConfig(dir, "bench-0")
	if err != nil {
		t.Fatal(err)
	}
	if c.CAFile != filepath.Join(dir, "ca.crt") {
		t.Fatal("CA path was not resolved relative to the explicit kubeconfig")
	}
	client, err := rest.HTTPClientFor(c)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal("relative CA did not verify local TLS server", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

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

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
)

func seedCLISpec() seed.Spec {
	return seed.Spec{Version: "v1", ID: "test-seed", NamespacePrefix: "seed-app", GuestNodes: 1, Applications: 40, Image: "example.com/image@sha256:" + strings.Repeat("a", 64), PerPodCPU: "10m", PerPodMemory: "10Mi", TimeoutSeconds: 60, Binary: seed.Binary{Path: "/not-installed/kube-burner", ExpectedSHA256: strings.Repeat("b", 64)}}
}

func TestSeedCLIRenderAndRun(t *testing.T) {
	spec := seedCLISpec()
	input := writeCLIJSON(t, spec)
	output := filepath.Join(t.TempDir(), "render")
	if err := runSeed(context.Background(), []string{"seed", "render", "--input", input, "--output", output}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, "bundle.json")); err != nil {
		t.Fatal(err)
	}
	if err := runSeed(context.Background(), []string{"seed", "render", "--input", input, "--output", output}, io.Discard); err == nil {
		t.Fatal("render accepted existing output")
	}
	const script = "#!/bin/sh\nprintf 'private-child-token'\nexit 0\n"
	binary := filepath.Join(t.TempDir(), "burner")
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	spec.Binary.ExpectedSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(script)))
	input = writeCLIJSON(t, spec)
	kubeconfig := writeCLIJSON(t, map[string]string{"private": "admin-token"})
	output = filepath.Join(t.TempDir(), "run")
	var out bytes.Buffer
	if err := runSeed(context.Background(), []string{"seed", "run", "--input", input, "--output", output, "--kubeconfig", kubeconfig, "--binary", binary}, &out); err != nil {
		t.Fatal(err)
	}
	var result seed.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.Invoked || result.State != "completed" || result.BinarySHA256 != spec.Binary.ExpectedSHA256 {
		t.Fatalf("bad run result: %s (%v)", &out, err)
	}
	if strings.Contains(out.String(), "private-child-token") || strings.Contains(out.String(), "admin-token") {
		t.Fatal("run leaked credentials or subprocess output")
	}
	var bundle seed.Bundle
	data, _ := os.ReadFile(filepath.Join(output, "bundle.json"))
	if err := json.Unmarshal(data, &bundle); err != nil || bundle.Spec.Binary.Path != binary || bundle.Spec.Binary.ExpectedSHA256 != spec.Binary.ExpectedSHA256 {
		t.Fatal("binary override lost its pin or was not persisted")
	}
	spec.Binary.ExpectedSHA256 = strings.Repeat("c", 64)
	if err := runSeed(context.Background(), []string{"seed", "run", "--input", writeCLIJSON(t, spec), "--output", filepath.Join(t.TempDir(), "mismatch"), "--kubeconfig", kubeconfig, "--binary", binary}, io.Discard); err == nil {
		t.Fatal("binary override bypassed hash validation")
	}
}

func TestSeedCLIValidation(t *testing.T) {
	input := writeCLIJSON(t, seedCLISpec())
	for _, args := range [][]string{
		{}, {"seed"}, {"seed", "unknown"}, {"seed", "render"},
		{"seed", "render", "--input", input, "--output", "unused", "--kubeconfig", "no"},
		{"seed", "render", "--input", input, "--output", "unused", "--timeout", "1s"},
		{"seed", "run", "--input", input, "--output", "unused"},
		{"seed", "identities", "--input", input},
		{"seed", "credentials", "--input", input, "--output", "unused", "--kubeconfig", "no", "--expiration", "25h"},
		{"seed", "credentials", "--input", input, "--output", "unused", "--kubeconfig", "no", "--expiration", "0"},
		{"seed", "credentials", "--input", input, "--output", "unused", "--kubeconfig", "no", "--expiration", "1.5s"},
		{"seed", "identities", "--input", input, "--kubeconfig", "no", "--timeout", "0"},
		{"seed", "identities", "--input", input, "--kubeconfig", "no", "--timeout", "25h"},
		{"seed", "run", "--input", input, "--output", "unused", "--kubeconfig", "no", "--binary="},
	} {
		if err := runSeed(context.Background(), args, io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	spec := seedCLISpec()
	spec.GuestNodes = 1001
	if err := runSeed(context.Background(), []string{"seed", "render", "--input", writeCLIJSON(t, spec), "--output", filepath.Join(t.TempDir(), "output")}, io.Discard); err == nil {
		t.Fatal("accepted more than 2000 namespaces")
	}
	data, err := json.Marshal(seedCLISpec())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "applications")
	err = runSeed(context.Background(), []string{"seed", "render", "--input", writeCLIJSON(t, fields), "--output", filepath.Join(t.TempDir(), "missing-apps")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "$.applications: required field is missing") {
		t.Fatal("applications must be explicit", err)
	}
}

func TestSeedCLIEmpty(t *testing.T) {
	for _, command := range []string{"render", "run", "identities", "credentials"} {
		for _, kubeconfig := range []string{"", "/not-present/admin.kubeconfig"} {
			if command == "render" && kubeconfig != "" {
				continue
			}
			t.Run(command+"/"+kubeconfig, func(t *testing.T) {
				spec := seedCLISpec()
				spec.GuestNodes = 3
				spec.Applications = 0
				output := filepath.Join(t.TempDir(), "empty")
				args := []string{"seed", command, "--input", writeCLIJSON(t, spec)}
				if command != "identities" {
					args = append(args, "--output", output)
				}
				if kubeconfig != "" {
					args = append(args, "--kubeconfig", kubeconfig)
				}
				var out bytes.Buffer
				if err := runSeed(context.Background(), args, &out); err != nil {
					t.Fatal("empty seed must not load admin credentials or executable", err)
				}
				switch command {
				case "render":
					var bundle seed.Bundle
					if err := json.Unmarshal(out.Bytes(), &bundle); err != nil || bundle.Applications != 0 || bundle.Namespaces != 0 || bundle.Pods != 0 || len(bundle.Templates) != 0 || !strings.Contains(bundle.Config, "jobs: []") {
						t.Fatal("invalid empty bundle", out.String(), err)
					}
				case "run":
					var result seed.Result
					if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Invoked || result.State != "empty" {
						t.Fatal("invalid empty run", out.String(), err)
					}
				case "identities":
					var result map[string]any
					if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["namespaces"] != float64(0) || result["state"] != "completed" {
						t.Fatal("invalid empty identities", out.String(), err)
					}
				case "credentials":
					var result seedCredentialResult
					if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Files == nil || len(result.Files) != 0 {
						t.Fatal("empty credentials must have explicit files: []", out.String(), err)
					}
					data, err := os.ReadFile(filepath.Join(output, "credentials.json"))
					if err != nil || !bytes.Contains(data, []byte(`"files": []`)) {
						t.Fatal("empty credential metadata missing", string(data), err)
					}
					entries, err := os.ReadDir(output)
					if err != nil || len(entries) != 1 {
						t.Fatal("empty credentials should write only metadata", entries, err)
					}
				}
			})
		}
	}
}

func TestSeedCLIIdentitiesAndCredentials(t *testing.T) {
	for _, mode := range []string{"success", "minimum", "maximum", "server-issued", "apps12", "apps21", "apps120", "missing-identity", "wrong-namespace", "broad-role", "wrong-binding", "empty", "zero-expiry", "expired", "expired-at-return", "overlong", "token-error", "partial"} {
		t.Run(mode, func(t *testing.T) {
			spec := seedCLISpec()
			spec.GuestNodes = 3
			switch mode {
			case "apps12":
				spec.Applications = 12
			case "apps21":
				spec.Applications = 21
			case "apps120":
				spec.Applications = 120
			}
			wantNamespaces := int((spec.Applications + 19) / 20)
			success := mode == "success" || mode == "minimum" || mode == "maximum" || mode == "server-issued" || mode == "overlong" || strings.HasPrefix(mode, "apps")
			expiration := 6 * time.Hour
			if mode == "minimum" {
				expiration = 600 * time.Second
			} else if mode == "maximum" {
				expiration = 24 * time.Hour
			}
			objects := map[string]json.RawMessage{}
			tokens := 0
			expiries := map[string]time.Time{}
			validUntil := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
			if mode == "expired" {
				validUntil = time.Now().Add(-time.Hour)
			} else if mode == "overlong" {
				validUntil = time.Now().Add(25 * time.Hour)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") != "Bearer private-admin-token" {
					t.Error("request did not use explicit admin config")
				}
				if strings.Contains(r.URL.Path, "flowcontrol") || strings.Contains(r.URL.Path, "clusterrole") {
					t.Errorf("unexpected cluster-wide RBAC or APF request: %s", r.URL.Path)
				}
				if strings.HasSuffix(r.URL.Path, "/token") {
					tokens++
					var request authenticationv1.TokenRequest
					data, _ := io.ReadAll(r.Body)
					if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(data, nil, &request); err != nil {
						t.Error(err)
					}
					if len(request.Spec.Audiences) != 0 || request.Spec.ExpirationSeconds == nil || *request.Spec.ExpirationSeconds != int64(expiration/time.Second) {
						t.Errorf("unexpected token request: %+v", request.Spec)
					}
					if mode == "token-error" || (mode == "partial" && tokens == 2) {
						w.WriteHeader(http.StatusForbidden)
						fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"private-issued-token","reason":"Forbidden","code":403}`)
						return
					}
					token := "private-issued-token"
					switch mode {
					case "empty":
						token = ""
					case "zero-expiry":
						validUntil = time.Time{}
					case "expired-at-return":
						validUntil = time.Now()
					case "server-issued", "minimum", "maximum":
						validUntil = time.Now().Add(expiration + 250*time.Millisecond)
					}
					expiries[fmt.Sprintf("%s-%d", spec.NamespacePrefix, tokens-1)] = validUntil
					// Preserve subsecond server expiry; metav1.Time's JSON encoder truncates it.
					_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "TokenRequest", "status": map[string]any{"token": token, "expirationTimestamp": validUntil.UTC().Format(time.RFC3339Nano)}})
					return
				}
				if r.Method == http.MethodPost {
					data, _ := io.ReadAll(r.Body)
					if r.Header.Get("Content-Type") == "application/vnd.kubernetes.protobuf" {
						object, _, err := scheme.Codecs.UniversalDeserializer().Decode(data, nil, nil)
						if err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						data, _ = json.Marshal(object)
					}
					var metadata struct {
						Metadata metav1.ObjectMeta `json:"metadata"`
					}
					_ = json.Unmarshal(data, &metadata)
					objects[r.URL.Path+"/"+metadata.Metadata.Name] = data
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write(data)
					return
				}
				if data, ok := objects[r.URL.Path]; ok {
					_, _ = w.Write(data)
				} else {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
				}
			}))
			defer server.Close()
			for n := 0; n < wantNamespaces; n++ {
				ns := fmt.Sprintf("%s-%d", spec.NamespacePrefix, n)
				labels := map[string]string{seed.RunIDLabel: spec.ID, "kube-burner-uuid": spec.ID, "kube-burner-job": spec.NamespacePrefix}
				objects["/api/v1/namespaces/"+ns], _ = json.Marshal(corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: labels}})
			}
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			config := clientcmdapi.NewConfig()
			config.Clusters["guest"] = &clientcmdapi.Cluster{Server: server.URL, CertificateAuthorityData: ca, TLSServerName: "example.com"}
			config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "private-admin-token"}
			config.Contexts["admin"] = &clientcmdapi.Context{Cluster: "guest", AuthInfo: "admin"}
			config.CurrentContext = "admin"
			data, err := clientcmd.Write(*config)
			if err != nil {
				t.Fatal(err)
			}
			kubeconfig := filepath.Join(t.TempDir(), "admin.kubeconfig")
			if err := os.WriteFile(kubeconfig, data, 0600); err != nil {
				t.Fatal(err)
			}
			input := writeCLIJSON(t, spec)
			if mode != "missing-identity" {
				if err := runSeed(context.Background(), []string{"seed", "identities", "--input", input, "--kubeconfig", kubeconfig}, io.Discard); err != nil {
					t.Fatal(err)
				}
			}
			if tokens != 0 {
				t.Fatal("identity setup minted tokens")
			}
			ns := spec.NamespacePrefix + "-0"
			switch mode {
			case "wrong-namespace":
				objects["/api/v1/namespaces/"+ns], _ = json.Marshal(corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
			case "broad-role":
				path := "/apis/rbac.authorization.k8s.io/v1/namespaces/" + ns + "/roles/benchmark"
				var role rbacv1.Role
				_ = json.Unmarshal(objects[path], &role)
				role.Rules[0].Verbs = []string{"*"}
				objects[path], _ = json.Marshal(role)
			case "wrong-binding":
				path := "/apis/rbac.authorization.k8s.io/v1/namespaces/" + ns + "/rolebindings/benchmark"
				var binding rbacv1.RoleBinding
				_ = json.Unmarshal(objects[path], &binding)
				binding.RoleRef.Kind = "ClusterRole"
				objects[path], _ = json.Marshal(binding)
			}
			output := filepath.Join(t.TempDir(), "credentials")
			var out bytes.Buffer
			args := []string{"seed", "credentials", "--input", input, "--kubeconfig", kubeconfig, "--output", output, "--expiration", expiration.String()}
			err = runSeed(context.Background(), args, &out)
			if (err == nil) != success {
				t.Fatalf("unexpected outcome: %v", err)
			}
			if strings.Contains(out.String(), "private-") || (err != nil && strings.Contains(err.Error(), "private-")) {
				t.Fatal("credentials leaked into stdout/error")
			}
			if !success {
				wantError := map[string]string{"empty": "empty token", "zero-expiry": "zero expiration", "expired": "expired at receipt", "expired-at-return": "expired at receipt"}[mode]
				if wantError != "" {
					if !strings.Contains(err.Error(), wantError) || !strings.Contains(err.Error(), ns) {
						t.Fatalf("missing safe diagnostic: %v", err)
					}
					entries, readErr := os.ReadDir(output)
					metadata, metadataErr := os.ReadFile(filepath.Join(output, "credentials.json"))
					if readErr != nil || metadataErr != nil || len(entries) != 1 || bytes.Contains(metadata, []byte("private-")) {
						t.Fatal("invalid token persisted credential material")
					}
				}
				if mode == "partial" {
					var result seedCredentialResult
					data, readErr := os.ReadFile(filepath.Join(output, "credentials.json"))
					if readErr != nil || json.Unmarshal(data, &result) != nil || len(result.Files) != 1 {
						t.Fatal("failed to retain partial credential metadata")
					}
				}
				return
			}
			var result seedCredentialResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Files) != wantNamespaces || tokens != wantNamespaces {
				t.Fatalf("invalid credential result: %s (%v)", &out, err)
			}
			for _, file := range result.Files {
				if !file.ValidUntil.Equal(expiries[file.Namespace]) || filepath.Base(file.Path) != file.Namespace+".kubeconfig" {
					t.Fatalf("bad credential metadata: %+v", file)
				}
				guest, err := clientcmd.LoadFromFile(file.Path)
				if err != nil {
					t.Fatal(err)
				}
				auth := guest.AuthInfos["benchmark"]
				if auth == nil {
					t.Fatal("generated kubeconfig has no benchmark authentication")
				}
				auth.LocationOfOrigin = ""
				if len(auth.Extensions) == 0 {
					auth.Extensions = nil
				}
				if len(guest.AuthInfos) != 1 || !reflect.DeepEqual(auth, &clientcmdapi.AuthInfo{Token: "private-issued-token"}) {
					t.Fatal("generated kubeconfig contains unexpected authentication")
				}
				cluster := guest.Clusters["guest"]
				if cluster.Server != server.URL || !bytes.Equal(cluster.CertificateAuthorityData, ca) || cluster.TLSServerName != "example.com" || guest.Contexts[guest.CurrentContext].Namespace != file.Namespace {
					t.Fatal("generated kubeconfig lost cluster TLS or namespace")
				}
				info, _ := os.Stat(file.Path)
				if info.Mode().Perm() != 0600 {
					t.Fatal("credential file is not private")
				}
			}
			metadata, _ := os.ReadFile(filepath.Join(output, "credentials.json"))
			var persisted seedCredentialResult
			if err := json.Unmarshal(metadata, &persisted); err != nil || !reflect.DeepEqual(persisted, result) {
				t.Fatal("persisted credential metadata differs from result", err)
			}
			if bytes.Contains(metadata, []byte("private-")) {
				t.Fatal("metadata contains credentials")
			}
			if err := runSeed(context.Background(), args, io.Discard); err == nil || tokens != wantNamespaces {
				t.Fatal("existing output was reused")
			}
		})
	}
}

func TestSeedCredentialsExpirationValidation(t *testing.T) {
	for _, expiration := range []time.Duration{-time.Second, 0, time.Second, 599 * time.Second, 600*time.Second + time.Millisecond, 24*time.Hour + time.Second} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/direct=%t", expiration, direct), func(t *testing.T) {
				output := filepath.Join(t.TempDir(), "credentials")
				var err error
				if direct {
					_, err = seedCredentials(t.Context(), seedCLISpec(), "/not-present/admin.kubeconfig", output, expiration)
				} else {
					err = runSeed(t.Context(), []string{"seed", "credentials", "--input", writeCLIJSON(t, seedCLISpec()), "--kubeconfig", "/not-present/admin.kubeconfig", "--output", output, "--expiration", expiration.String()}, io.Discard)
				}
				if err == nil || !strings.Contains(err.Error(), "expiration must be whole seconds in [600s,24h]") {
					t.Fatalf("invalid expiration passed validation: %v", err)
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatal("invalid expiration created output before validation")
				}
			})
		}
	}
}

func TestSeedCLICancellationAndExclusiveFiles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output := filepath.Join(t.TempDir(), "output")
	err := runSeed(ctx, []string{"seed", "run", "--input", writeCLIJSON(t, seedCLISpec()), "--kubeconfig", "unused", "--output", output}, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("canceled run wrote artifacts")
	}
	path := filepath.Join(t.TempDir(), "credential")
	if err := writeSeedPrivate(path, []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := writeSeedPrivate(path, []byte("replacement")); err == nil {
		t.Fatal("credential writer overwrote existing file")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Fatal("existing credential changed")
	}
}

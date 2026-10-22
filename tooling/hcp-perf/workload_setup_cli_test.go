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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func workloadSetupCLISpec() workload.SetupSpec {
	return workload.SetupSpec{Version: "v1", ID: "test-run", NamespacePrefix: "seed-test", GuestNodes: 3, Applications: 21, Image: workload.CanaryImage}
}

func workloadSetupCLIClient(t *testing.T, spec workload.SetupSpec) *fake.Clientset {
	t.Helper()
	client := fake.NewSimpleClientset()
	for n := int64(0); n < (spec.Applications+19)/20; n++ {
		ns := fmt.Sprintf("%s-%d", spec.NamespacePrefix, n)
		for _, obj := range []runtime.Object{
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{seed.RunIDLabel: spec.ID, "kube-burner-uuid": spec.ID, "kube-burner-job": spec.NamespacePrefix}}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: seed.IdentityName, Namespace: ns, Labels: map[string]string{seed.RunIDLabel: spec.ID}}, AutomountServiceAccountToken: ptr.To(false)},
		} {
			if err := client.Tracker().Add(obj); err != nil {
				t.Fatal(err)
			}
		}
	}
	return client
}

func TestWorkloadSetupCLIValidation(t *testing.T) {
	input := writeCLIJSON(t, workloadSetupCLISpec())
	base := []string{"workload", "setup", "--input", input, "--kubeconfig", "explicit"}
	for _, args := range [][]string{
		{}, {"workload"}, {"workload", "other"}, {"seed", "setup"}, {"workload", "setup"},
		{"workload", "setup", "--input", input}, {"workload", "setup", "--kubeconfig", "explicit"},
		append(append([]string{}, base...), "extra"), append(append([]string{}, base...), "--timeout", "0"),
		append(append([]string{}, base...), "--timeout", "11m"), append(append([]string{}, base...), "--output", "unused"),
		{"workload", "credentials", "--input", input, "--kubeconfig", "explicit"},
		{"workload", "credentials", "--input", input, "--kubeconfig", "explicit", "--output", "unused", "--expiration", "25h"},
		{"workload", "credentials", "--input", input, "--kubeconfig", "explicit", "--output", "unused", "--expiration", "1.5s"},
		{"workload", "setup", "--input", writeCLIJSON(t, map[string]any{"version": "v1", "unknown": true}), "--kubeconfig", "explicit"},
	} {
		err := runWorkloadSetupWithClient(context.Background(), args, io.Discard, func(string) (kubernetes.Interface, *rest.Config, error) {
			t.Fatal("invalid flags reached client")
			return nil, nil, nil
		})
		if err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	t.Setenv("KUBECONFIG", "ambient-must-not-be-used")
	path := filepath.Join(t.TempDir(), "bad-config")
	if err := os.WriteFile(path, []byte("private-admin-token: ["), 0600); err != nil {
		t.Fatal(err)
	}
	err := runWorkloadSetup(context.Background(), []string{"workload", "setup", "--input", input, "--kubeconfig", path}, io.Discard)
	if err == nil || strings.Contains(err.Error(), "private-admin-token") {
		t.Fatalf("bad credential error: %v", err)
	}
}

func TestWorkloadSetupCLIInventory(t *testing.T) {
	for _, mode := range []string{"success", "failure", "zero-apps"} {
		t.Run(mode, func(t *testing.T) {
			fail := mode == "failure"
			spec := workloadSetupCLISpec()
			if mode == "zero-apps" {
				spec.Applications = 0
			}
			client := workloadSetupCLIClient(t, spec)
			if fail {
				client.PrependReactor("create", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("private-server-token")
				})
			}
			var out bytes.Buffer
			err := runWorkloadSetupWithClient(context.Background(), []string{"workload", "setup", "--input", writeCLIJSON(t, spec), "--kubeconfig", "explicit"}, &out, func(path string) (kubernetes.Interface, *rest.Config, error) {
				if path != "explicit" {
					t.Fatal("wrong config path")
				}
				return client, nil, nil
			})
			if (err != nil) != fail {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Contains(out.String(), "private-") || (err != nil && strings.Contains(err.Error(), "private-")) {
				t.Fatal("server error leaked")
			}
			var result workload.SetupResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			want, state := 8+int((spec.Applications+19)/20), "completed"
			if fail {
				want, state = 4, "failed"
			}
			if len(result.Created) != want || result.State != state || len(result.Warnings) == 0 {
				t.Fatalf("lost inventory: %+v", result)
			}
			for _, action := range client.Actions() {
				if action.GetSubresource() == "token" {
					t.Fatal("setup minted token")
				}
			}
		})
	}
}

func TestWorkloadCanaryCredentialsCLI(t *testing.T) {
	for _, mode := range []string{"success", "minimum", "maximum", "server-issued", "zero-apps", "missing", "drift", "token-error", "empty", "zero-expiry", "expired", "expired-at-return", "overlong", "existing", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			success := mode == "success" || mode == "minimum" || mode == "maximum" || mode == "server-issued" || mode == "zero-apps" || mode == "overlong"
			expiration := 6 * time.Hour
			if mode == "minimum" {
				expiration = 600 * time.Second
			} else if mode == "maximum" {
				expiration = 24 * time.Hour
			}
			spec := workloadSetupCLISpec()
			if mode == "zero-apps" {
				spec.Applications = 0
			}
			client := workloadSetupCLIClient(t, spec)
			if mode != "missing" {
				if _, err := workload.Configure(context.Background(), client, spec); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "drift" {
				role, _ := client.RbacV1().Roles(spec.CanaryNamespace()).Get(context.Background(), workload.CanaryName, metav1.GetOptions{})
				role.Rules[0].Verbs = []string{"*"}
				_, _ = client.RbacV1().Roles(role.Namespace).Update(context.Background(), role, metav1.UpdateOptions{})
			}
			client.ClearActions()
			tokens := 0
			validUntil := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
			client.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "token" {
					t.Fatal("unexpected serviceaccount create")
				}
				tokens++
				if action.GetNamespace() != spec.CanaryNamespace() || action.(ktesting.CreateActionImpl).Name != workload.CanaryName {
					t.Fatal("wrong token identity")
				}
				request := action.(ktesting.CreateAction).GetObject().(*authenticationv1.TokenRequest)
				if request.Spec.ExpirationSeconds == nil || *request.Spec.ExpirationSeconds != int64(expiration/time.Second) || len(request.Spec.Audiences) != 0 {
					t.Fatalf("bad token request: %+v", request.Spec)
				}
				if mode == "token-error" {
					return true, nil, errors.New("private-issued-token")
				}
				token := "private-issued-token"
				switch mode {
				case "empty":
					token = ""
				case "zero-expiry":
					validUntil = time.Time{}
				case "expired":
					validUntil = time.Now().Add(-time.Hour)
				case "expired-at-return":
					validUntil = time.Now()
				case "server-issued", "minimum", "maximum":
					validUntil = time.Now().Add(expiration + 250*time.Millisecond)
				case "overlong":
					validUntil = time.Now().Add(25 * time.Hour)
				}
				return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: token, ExpirationTimestamp: metav1.NewTime(validUntil)}}, nil
			})
			output := filepath.Join(t.TempDir(), "canary.kubeconfig")
			if mode == "existing" {
				if err := os.WriteFile(output, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "symlink" {
				if err := os.Symlink(filepath.Join(t.TempDir(), "not-created"), output); err != nil {
					t.Fatal(err)
				}
			}
			config := &rest.Config{Host: "https://guest.invalid", BearerToken: "private-admin-token", Username: "admin", Password: "private-password", ExecProvider: &clientcmdapi.ExecConfig{Command: "never-copy"}, TLSClientConfig: rest.TLSClientConfig{CAData: []byte("guest-ca"), CertData: []byte("private-admin-cert"), KeyData: []byte("private-admin-key"), ServerName: "guest-name"}}
			var out bytes.Buffer
			err := runWorkloadSetupWithClient(context.Background(), []string{"workload", "credentials", "--input", writeCLIJSON(t, spec), "--kubeconfig", "explicit", "--output", output, "--expiration", expiration.String()}, &out, func(string) (kubernetes.Interface, *rest.Config, error) { return client, config, nil })
			if (err == nil) != success {
				t.Fatalf("unexpected result %s: %v", mode, err)
			}
			if strings.Contains(out.String(), "private-") || (err != nil && strings.Contains(err.Error(), "private-")) {
				t.Fatal("leaked credentials")
			}
			if mode == "missing" || mode == "drift" || mode == "existing" || mode == "symlink" {
				if tokens != 0 {
					t.Fatal("minted token before preflight")
				}
			}
			if mode == "existing" {
				data, _ := os.ReadFile(output)
				if string(data) != "preserve" {
					t.Fatal("overwrote existing output")
				}
			}
			if !success {
				wantError := map[string]string{"empty": "empty token", "zero-expiry": "zero expiration", "expired": "expired at receipt", "expired-at-return": "expired at receipt"}[mode]
				if wantError != "" {
					if !strings.Contains(err.Error(), wantError) || !strings.Contains(err.Error(), spec.CanaryNamespace()) {
						t.Fatalf("missing safe diagnostic: %v", err)
					}
					data, readErr := os.ReadFile(output)
					if readErr != nil || len(data) != 0 {
						t.Fatal("invalid token persisted credential material", readErr)
					}
				}
				return
			}
			var result seedCredentialFile
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Path != output || result.Namespace != spec.CanaryNamespace() || !result.ValidUntil.Equal(validUntil) {
				t.Fatalf("bad credential metadata: %s", &out)
			}
			stat, err := os.Stat(output)
			if err != nil || stat.Mode().Perm() != 0600 {
				t.Fatal("credential file is not private")
			}
			guest, err := clientcmd.LoadFromFile(output)
			if err != nil {
				t.Fatal(err)
			}
			auth := guest.AuthInfos[workload.CanaryName]
			if len(guest.AuthInfos) != 1 || auth == nil || auth.Token != "private-issued-token" {
				t.Fatal("copied admin auth")
			}
			auth.Token, auth.LocationOfOrigin = "", ""
			if len(auth.Extensions) == 0 {
				auth.Extensions = nil
			}
			if !reflect.DeepEqual(auth, &clientcmdapi.AuthInfo{}) {
				t.Fatal("copied admin auth fields")
			}
			cluster := guest.Clusters["guest"]
			if cluster.Server != config.Host || cluster.TLSServerName != config.ServerName || !bytes.Equal(cluster.CertificateAuthorityData, config.CAData) || guest.Contexts[guest.CurrentContext].Namespace != spec.CanaryNamespace() {
				t.Fatal("lost guest transport or namespace")
			}
			if tokens != 1 {
				t.Fatal("wrong token count")
			}
		})
	}
}

func TestWorkloadCanaryCredentialsExpirationValidation(t *testing.T) {
	for _, expiration := range []time.Duration{-time.Second, 0, time.Second, 599 * time.Second, 600*time.Second + time.Millisecond, 24*time.Hour + time.Second} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/direct=%t", expiration, direct), func(t *testing.T) {
				client := fake.NewSimpleClientset()
				output := filepath.Join(t.TempDir(), "canary.kubeconfig")
				var err error
				if direct {
					_, err = workloadCanaryCredentials(t.Context(), client, &rest.Config{}, workloadSetupCLISpec(), output, expiration)
				} else {
					err = runWorkloadSetupWithClient(t.Context(), []string{"workload", "credentials", "--input", writeCLIJSON(t, workloadSetupCLISpec()), "--kubeconfig", "explicit", "--output", output, "--expiration", expiration.String()}, io.Discard, func(string) (kubernetes.Interface, *rest.Config, error) {
						t.Fatal("invalid expiration reached client factory")
						return nil, nil, nil
					})
				}
				if err == nil || !strings.Contains(err.Error(), "[600s,24h]") || len(client.Actions()) != 0 {
					t.Fatalf("invalid expiration passed validation: %v", err)
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatal("invalid expiration created output before validation")
				}
			})
		}
	}
}

func TestWorkloadSetupCLIRequiresApplications(t *testing.T) {
	for _, command := range []string{"setup", "credentials"} {
		for _, value := range []any{"missing", nil, int64(-1), int64(40001)} {
			t.Run(fmt.Sprintf("%s/%v", command, value), func(t *testing.T) {
				data, err := json.Marshal(workloadSetupCLISpec())
				if err != nil {
					t.Fatal(err)
				}
				var input map[string]any
				if err := json.Unmarshal(data, &input); err != nil {
					t.Fatal(err)
				}
				input["applications"] = value
				if value == "missing" {
					delete(input, "applications")
				}
				args := []string{"workload", command, "--input", writeCLIJSON(t, input), "--kubeconfig", "explicit"}
				if command == "credentials" {
					args = append(args, "--output", filepath.Join(t.TempDir(), "canary.kubeconfig"))
				}
				err = runWorkloadSetupWithClient(t.Context(), args, io.Discard, func(string) (kubernetes.Interface, *rest.Config, error) {
					t.Fatal("invalid applications reached admin client")
					return nil, nil, nil
				})
				if err == nil || !strings.Contains(err.Error(), "applications") {
					t.Fatalf("invalid applications accepted: %v", err)
				}
			})
		}
	}
}

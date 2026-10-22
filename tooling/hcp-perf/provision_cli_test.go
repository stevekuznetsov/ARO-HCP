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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/provision"
)

func rpCLIConfig(endpoint string) provision.Config {
	return provision.Config{Version: "v1", Endpoint: endpoint, SubscriptionID: "11111111-1111-4111-8111-111111111111", TenantID: "22222222-2222-4222-8222-222222222222", ObjectID: "33333333-3333-4333-8333-333333333333", Principal: "test@example.com", APIVersion: "2025-12-23-preview"}
}

func writeCLIJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRPCLICommands(t *testing.T) {
	for _, command := range []string{"create-cluster", "create-nodepool", "get", "wait", "delete", "credentials"} {
		t.Run(command, func(t *testing.T) {
			const credential = "private-admin-kubeconfig-token"
			config := rpCLIConfig("")
			ref := provision.Resource{ResourceGroup: "test-rg", Cluster: "test-cluster"}
			if command == "create-nodepool" {
				ref.NodePool = "workers"
			}
			payload := json.RawMessage(`{"location":"eastus","properties":{"arbitrary":[{"precision":9007199254740993}]}}`)
			operation := "/subscriptions/" + config.SubscriptionID + "/providers/Microsoft.RedHatOpenShift/locations/eastus/hcpOperationStatuses/test"
			var calls []string
			created := false
			polls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				if r.Header.Get("Authorization") != "" || r.Header.Get("X-Ms-Home-Tenant-Id") != config.TenantID {
					t.Error("unexpected RP authentication headers")
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/subscriptions/"+config.SubscriptionID:
					fmt.Fprintf(w, `{"state":"Registered","properties":{"tenantId":%q}}`, config.TenantID)
				case r.URL.Path == operation:
					polls++
					w.Header().Set("Retry-After", "0")
					if command == "wait" && polls == 1 {
						fmt.Fprint(w, `{"status":"Provisioning"}`)
					} else {
						fmt.Fprint(w, `{"status":"Succeeded"}`)
					}
				case r.Method == http.MethodPut:
					data, _ := io.ReadAll(r.Body)
					if !bytes.Equal(data, payload) {
						t.Errorf("payload changed: %s", data)
					}
					created = true
					w.Header().Set("Azure-AsyncOperation", operation)
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusAccepted)
					fmt.Fprint(w, `{"properties":{"provisioningState":"Creating"}}`)
				case r.Method == http.MethodDelete:
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/requestAdminCredential"):
					_ = json.NewEncoder(w).Encode(map[string]string{"kubeconfig": credential})
				case strings.HasPrefix(command, "create-") && !created:
					w.WriteHeader(http.StatusNotFound)
				default:
					fmt.Fprint(w, `{"properties":{"provisioningState":"Succeeded"}}`)
				}
			}))
			defer server.Close()
			config.Endpoint = server.URL
			output := filepath.Join(t.TempDir(), "artifacts")
			args := []string{"rp", command, "--input", writeCLIJSON(t, config), "--resource", writeCLIJSON(t, ref), "--output", output}
			credentialPath := filepath.Join(t.TempDir(), "admin.kubeconfig")
			switch command {
			case "wait":
				args = []string{"rp", "wait", "--input", writeCLIJSON(t, config), "--resource", writeCLIJSON(t, ref), "--operation-url", server.URL + operation, "--outputdir", output}
			case "create-cluster", "create-nodepool":
				args = append(args, "--payload", writeCLIJSON(t, payload))
			case "delete":
				args = append(args, "--confirm-delete")
			case "credentials":
				args = append(args, "--credentials-output", credentialPath)
			}
			var out bytes.Buffer
			if err := runRP(context.Background(), args, &out); err != nil {
				t.Fatal(err)
			}
			var result provision.Result
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.FinalState != "Succeeded" {
				t.Fatalf("bad result: %s (%v)", &out, err)
			}
			if strings.HasPrefix(command, "create-") && (len(calls) != 5 || result.OperationURL == "") {
				t.Fatalf("did not preflight and poll: %v", calls)
			}
			if command == "wait" {
				if len(calls) != 3 || result.OperationURL == "" || result.Method != "GET" {
					t.Fatalf("wait did not poll then inspect: %v", calls)
				}
				for _, call := range calls {
					if !strings.HasPrefix(call, "GET ") {
						t.Fatalf("wait resubmitted mutation: %s", call)
					}
				}
			}
			if command == "credentials" {
				data, err := os.ReadFile(credentialPath)
				if err != nil || string(data) != credential {
					t.Fatalf("missing private credential: %v", err)
				}
				info, _ := os.Stat(credentialPath)
				if info.Mode().Perm() != 0600 || strings.Contains(out.String(), credential) {
					t.Fatal("credential was not kept private")
				}
				entries, _ := os.ReadDir(output)
				for _, entry := range entries {
					data, _ := os.ReadFile(filepath.Join(output, entry.Name()))
					if bytes.Contains(data, []byte(credential)) {
						t.Errorf("credential leaked into %s", entry.Name())
					}
				}
			}
			if err := runRP(context.Background(), args, io.Discard); err == nil {
				t.Fatal("accepted existing output directory")
			}
		})
	}
}

func TestRPCLITimeoutRetainsOperation(t *testing.T) {
	config := rpCLIConfig("")
	operation := "/subscriptions/" + config.SubscriptionID + "/providers/Microsoft.RedHatOpenShift/locations/eastus/hcpOperationStatuses/test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Azure-AsyncOperation", operation)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"status":"Running"}`)
	}))
	defer server.Close()
	config.Endpoint = server.URL
	var out bytes.Buffer
	err := runRP(context.Background(), []string{"rp", "delete", "--confirm-delete", "--input", writeCLIJSON(t, config), "--resource", writeCLIJSON(t, provision.Resource{ResourceGroup: "rg", Cluster: "cluster"}), "--output", filepath.Join(t.TempDir(), "output"), "--timeout", (100 * time.Millisecond).String()}, &out)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	var result provision.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.OperationURL == "" || result.FinalState != "" {
		t.Fatalf("lost partial result: %s (%v)", &out, err)
	}
}

func TestRPCLIValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid CLI reached server: %s", r.URL)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	input := writeCLIJSON(t, rpCLIConfig(server.URL))
	resource := writeCLIJSON(t, provision.Resource{ResourceGroup: "test-rg", Cluster: "cluster"})
	for _, suffix := range [][]string{
		{"get", "--payload", "anything"}, {"delete"}, {"delete", "--confirm-delete=false"},
		{"get", "--timeout", "0"}, {"get", "--timeout", "25h"}, {"get", "extra"},
		{"credentials"}, {"create-cluster"}, {"unknown"},
		{"wait"}, {"wait", "--operation-url", "https://evil.example/operation"}, {"wait", "--operation-url", "anything", "--payload", "anything"},
	} {
		args := append([]string{"rp", suffix[0], "--input", input, "--resource", resource, "--output", filepath.Join(t.TempDir(), "output")}, suffix[1:]...)
		if err := runRP(context.Background(), args, io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	for _, raw := range []string{`[]`, `null`, `1`, `"text"`, `{} {}`, `{"a":1,"a":2}`, `{"a":1,"A":2}`, `{"nested":{"x":1,"x":2}}`} {
		path := filepath.Join(t.TempDir(), "payload.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := runRP(context.Background(), []string{"rp", "create-cluster", "--input", input, "--resource", resource, "--payload", path, "--output", filepath.Join(t.TempDir(), "output")}, io.Discard); err == nil {
			t.Errorf("accepted payload %s", raw)
		}
	}
	for _, ref := range []any{map[string]string{"resourceGroup": "rg"}, map[string]string{"resourceGroup": "rg", "cluster": "cluster", "unknown": "x"}, map[string]string{"resourceGroup": "rg", "cluster": "cluster", "Cluster": "alias"}} {
		if err := runRP(context.Background(), []string{"rp", "get", "--input", input, "--resource", writeCLIJSON(t, ref), "--output", filepath.Join(t.TempDir(), "output")}, io.Discard); err == nil {
			t.Errorf("accepted resource %v", ref)
		}
	}
}

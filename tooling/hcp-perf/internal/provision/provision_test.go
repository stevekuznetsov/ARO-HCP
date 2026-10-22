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

package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const subID = "00000000-0000-0000-0000-000000000001"
const tenantID = "00000000-0000-0000-0000-000000000002"
const statusPath = "/subscriptions/" + subID + "/providers/Microsoft.RedHatOpenShift/locations/eastus/hcpOperationStatuses/op-123"
const resultPath = "/subscriptions/" + subID + "/providers/Microsoft.RedHatOpenShift/locations/eastus/hcpOperationResults/op-123"
const succeeded = `{"properties":{"provisioningState":"Succeeded"}}`
const registered = `{"state":"Registered","properties":{"tenantId":"` + tenantID + `"}}`

var ref = Resource{ResourceGroup: "customer-rg", Cluster: "perf-cluster"}

func config(endpoint string) Config {
	return Config{Version: "v1", Endpoint: endpoint, SubscriptionID: subID, TenantID: tenantID, ObjectID: "00000000-0000-0000-0000-000000000003", Principal: "personal-user", APIVersion: "2025-12-23-preview"}
}

func newClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c, err := New(config(s.URL), s.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func output(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "operation")
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func saved(t *testing.T, result Result) {
	t.Helper()
	var disk Result
	if err := json.Unmarshal(read(t, filepath.Join(result.OutputDir, "operation.json")), &disk); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, disk) {
		t.Fatalf("disk result differs: %+v != %+v", disk, result)
	}
}

func TestCreateHeadersAndLRO(t *testing.T) {
	for _, version := range []string{"2024-06-10-preview", "2025-12-23-preview"} {
		for _, nodePool := range []string{"", "workers"} {
			t.Run(version+"/"+nodePool, func(t *testing.T) {
				target := ref
				target.NodePool = nodePool
				payload := json.RawMessage("{\n  \"properties\": {\"existingCustomerNetwork\": \"/user/supplied\", \"replicas\": 9007199254740993}\n}")
				var paths []string
				var ids = map[string]bool{}
				created, polls := false, 0
				dir := output(t)
				c, server := newClient(t, func(w http.ResponseWriter, r *http.Request) {
					paths = append(paths, r.Method+" "+r.URL.Path)
					w.Header().Set("Retry-After", "0")
					for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Ms-Client-Principal"} {
						if r.Header.Get(header) != "" {
							t.Errorf("unexpected privileged header %s", header)
						}
					}
					for _, header := range []string{"X-Ms-Client-Request-Id", "X-Ms-Correlation-Request-Id"} {
						id := r.Header.Get(header)
						if !uuidPattern.MatchString(id) || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) || ids[id] {
							t.Errorf("invalid or reused request UUID: %q", id)
						}
						ids[id] = true
					}
					if r.Header.Get("X-Ms-Home-Tenant-Id") != tenantID || r.Header.Get("X-Ms-Client-Object-Id") != config("").ObjectID || r.Header.Get("X-Ms-Client-Principal-Name") != "personal-user" {
						t.Error("missing caller identity headers")
					}
					referer, err := url.Parse(r.Referer())
					if err != nil || referer.Host != r.Host || referer.Path != r.URL.Path || referer.RawQuery != r.URL.RawQuery {
						t.Error("referer must describe the direct request origin and URL")
					}
					if r.URL.Path == "/subscriptions/"+subID {
						if r.Method != "GET" || r.URL.Query().Get("api-version") != "2.0" {
							t.Error("registration must only be read with system API version")
						}
						fmt.Fprint(w, registered)
						return
					}
					if r.URL.Query().Get("api-version") != version {
						t.Error("incorrect resource API version")
					}
					switch {
					case r.Method == "PUT":
						body, _ := io.ReadAll(r.Body)
						if string(body) != string(payload) {
							t.Error("payload was modified")
						}
						var system map[string]string
						if json.Unmarshal([]byte(r.Header.Get("X-Ms-Arm-Resource-System-Data")), &system) != nil || system["createdBy"] != "personal-user" || system["createdByType"] != "Application" {
							t.Error("invalid system data")
						}
						if _, err := time.Parse(time.RFC3339, system["createdAt"]); err != nil {
							t.Error(err)
						}
						if r.Header.Get("X-Ms-Identity-Url") != "https://dummyhost.identity.azure.net" || r.Header.Get("Content-Type") != "application/json" {
							t.Error("missing PUT headers")
						}
						created = true
						w.Header().Set("Azure-AsyncOperation", statusPath+"?api-version="+version)
						w.Header().Set("Location", resultPath)
						w.WriteHeader(201)
						fmt.Fprint(w, `{"properties":{"provisioningState":"Creating"}}`)
					case r.URL.Path == statusPath:
						var disk Result
						if json.Unmarshal(read(t, filepath.Join(dir, "operation.json")), &disk) != nil || disk.OperationURL == "" {
							t.Error("operation URL was not persisted before polling")
						}
						polls++
						if polls == 1 {
							w.WriteHeader(202)
							fmt.Fprint(w, `{"status":"Provisioning"}`)
						} else {
							fmt.Fprint(w, `{"status":"Succeeded"}`)
						}
					case r.URL.Path == resultPath:
						t.Error("Azure-AsyncOperation must take priority over Location")
					case !created:
						w.WriteHeader(404)
					default:
						fmt.Fprint(w, succeeded)
					}
				})
				c.config.APIVersion = version
				jar, _ := cookiejar.New(nil)
				u, _ := url.Parse(server.URL)
				jar.SetCookies(u, []*http.Cookie{{Name: "secret", Value: "token"}})
				supplied := *server.Client()
				supplied.Jar = jar
				cfg := config(server.URL)
				cfg.APIVersion = version
				c, err := New(cfg, &supplied)
				if err != nil {
					t.Fatal(err)
				}
				var result Result
				if nodePool == "" {
					result, err = c.CreateCluster(t.Context(), target, payload, dir)
				} else {
					result, err = c.CreateNodePool(t.Context(), target, payload, dir)
				}
				if err != nil {
					t.Fatal(err)
				}
				if result.FinalState != "Succeeded" || result.ErrorCode != "" || polls != 2 || len(paths) != 6 {
					t.Fatalf("unexpected completion: %+v, requests %v", result, paths)
				}
				if string(read(t, filepath.Join(dir, "payload.json"))) != string(payload) {
					t.Error("raw input artifact changed")
				}
				saved(t, result)
				entries, _ := os.ReadDir(dir)
				for _, entry := range entries {
					info, _ := entry.Info()
					if info.Mode().Perm() != 0600 {
						t.Errorf("artifact %s is not private", entry.Name())
					}
				}
				info, _ := os.Stat(dir)
				if info.Mode().Perm() != 0700 {
					t.Error("directory is not private")
				}
				lines := strings.Split(strings.TrimSpace(string(read(t, filepath.Join(dir, "status.ndjson")))), "\n")
				if len(lines) != 12 {
					t.Fatalf("expected request and response events, got %d", len(lines))
				}
			})
		}
	}
}

func TestValidation(t *testing.T) {
	for _, endpoint := range []string{"", "http://example.com", "ftp://localhost", "http://localhost.evil", "http://127.0.0.1.evil", "https://user:secret@example.com", "https://example.com/path", "https://example.com?token=secret", "https://example.com#", "https://example.com/%2f"} {
		if _, err := New(config(endpoint), nil); err == nil {
			t.Errorf("accepted endpoint %q", endpoint)
		}
	}
	for _, endpoint := range []string{"http://localhost:8443", "http://127.0.0.1:1234", "http://[::1]:8443", "https://personal.example.com"} {
		if _, err := New(config(endpoint), nil); err != nil {
			t.Errorf("rejected endpoint %q: %v", endpoint, err)
		}
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.Version = "" }, func(c *Config) { c.APIVersion = "2026-09-01-preview" }, func(c *Config) { c.SubscriptionID = "../other" }, func(c *Config) { c.TenantID = "" }, func(c *Config) { c.ObjectID = "bad" }, func(c *Config) { c.Principal = "secret\r\nHeader: value" }} {
		cfg := config("http://localhost:8443")
		mutate(&cfg)
		if _, err := New(cfg, nil); err == nil {
			t.Errorf("accepted invalid config %+v", cfg)
		}
	}
	c, _ := New(config("http://localhost:8443"), nil)
	for _, name := range []string{"..", ".", "a/b", "a\\b", "a?b", "a#b", "%2e%2e", "a:b", "a;b", "a@b", "a\nb", ""} {
		for field := range 3 {
			r := ref
			switch field {
			case 0:
				r.ResourceGroup = name
			case 1:
				r.Cluster = name
			case 2:
				r.NodePool = name
			}
			if field == 2 && name == "" {
				continue
			}
			if _, err := c.resourceID(r); err == nil {
				t.Errorf("accepted invalid resource name %q in field %d", name, field)
			}
		}
	}
	for _, r := range []Resource{{"rg.", "cluster", ""}, {"rg", strings.Repeat("a", 55), ""}, {"rg", "cluster", strings.Repeat("a", 16)}, {"rg", "1cluster", ""}, {"rg", "cluster-", ""}} {
		if _, err := c.resourceID(r); err == nil {
			t.Errorf("accepted invalid resource %+v", r)
		}
	}
	for _, payload := range []string{"null", "[]", `{} {}`, `{"properties":{"a":1,"a":2}}`, `{"properties":[{"a":1,"A":2}]}`, `{"apiVersion":"wrong"}`, `{"a":{},"a":[]}`} {
		if err := validatePayload([]byte(payload), c.config.APIVersion); err == nil {
			t.Errorf("accepted payload %s", payload)
		}
	}
	for _, payload := range []string{`{}`, `{"properties":{"a":null,"b":[1,{},[],{"a":3}]},"tags":{"a":"b"}}`} {
		if err := validatePayload([]byte(payload), c.config.APIVersion); err != nil {
			t.Errorf("rejected raw payload: %v", err)
		}
	}
}

func TestUnsafeOperationsAndRedirects(t *testing.T) {
	var externalCalls atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { externalCalls.Add(1) }))
	defer evil.Close()
	for _, location := range []string{evil.URL + statusPath, "https://localhost" + statusPath, "//evil.example" + statusPath, "/subscriptions/other/providers/Microsoft.RedHatOpenShift/locations/eastus/hcpOperationStatuses/op", strings.Replace(statusPath, "hcpOperationStatuses", "requestAdminCredential", 1), statusPath + "/extra", statusPath + "/../op", statusPath + "?token=secret", statusPath + "#secret", strings.Replace(statusPath, "op-123", "%2e%2e", 1), statusPath + "?api-version=wrong", statusPath + "?api-version=2025-12-23-preview&api-version=2025-12-23-preview", "file:///etc/passwd"} {
		t.Run(location, func(t *testing.T) {
			var calls int
			c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Azure-AsyncOperation", location)
				w.WriteHeader(202)
			})
			result, err := c.Delete(t.Context(), ref, output(t), true)
			if err == nil || calls != 1 || result.FinalState != "" {
				t.Fatalf("unsafe URL followed: %+v %v, calls %d", result, err, calls)
			}
		})
	}
	for _, code := range []int{301, 302, 303, 307, 308} {
		c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, evil.URL+statusPath, code) })
		result, err := c.Inspect(t.Context(), ref, output(t))
		if err == nil || result.HTTPStatus != code {
			t.Errorf("redirect was not rejected: %+v %v", result, err)
		}
	}
	if externalCalls.Load() != 0 {
		t.Fatal("sent requests to a foreign origin")
	}
}

func TestFreshnessRegistrationAndSynchronousCompletion(t *testing.T) {
	for _, test := range []struct {
		name, subscription   string
		getCode, putCode     int
		putBody              string
		wantPUT, wantSuccess bool
	}{
		{"existing", registered, 200, 201, succeeded, false, false},
		{"forbidden", registered, 403, 201, succeeded, false, false},
		{"unregistered", `{"state":"Unregistered"}`, 404, 201, succeeded, false, false},
		{"wrong tenant", strings.Replace(registered, tenantID, subID, 1), 404, 201, succeeded, false, false},
		{"success", registered, 404, 200, succeeded, true, true},
		{"created", registered, 404, 201, succeeded, true, true},
		{"still creating", registered, 404, 200, `{"properties":{"provisioningState":"Creating"}}`, true, false},
		{"no state", registered, 404, 200, `{}`, true, false},
		{"not a resource", registered, 404, 200, `{"status":"Succeeded"}`, true, false},
		{"accepted without URL", registered, 404, 202, `{}`, true, false},
		{"race conflict", registered, 404, 409, `{"error":{"code":"Conflict","message":"private-message"}}`, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			put := false
			c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/subscriptions/"+subID:
					fmt.Fprint(w, test.subscription)
				case r.Method == "GET":
					w.WriteHeader(test.getCode)
				case r.Method == "PUT":
					put = true
					w.WriteHeader(test.putCode)
					fmt.Fprint(w, test.putBody)
				default:
					t.Error("unexpected mutation")
				}
			})
			result, err := c.CreateCluster(t.Context(), ref, json.RawMessage(`{}`), output(t))
			if (err == nil) != test.wantSuccess || put != test.wantPUT {
				t.Fatalf("result %+v, error %v, PUT %t", result, err, put)
			}
			if err != nil && strings.Contains(err.Error(), "private-message") {
				t.Error("ARM message leaked")
			}
			if (result.FinalState == "Succeeded") != test.wantSuccess {
				t.Error("incorrect final state")
			}
			saved(t, result)
		})
	}
}

func TestFailedCanceledAndCanceledPolling(t *testing.T) {
	for _, state := range []string{"Failed", "Canceled"} {
		t.Run(state, func(t *testing.T) {
			c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "0")
				if r.Method == "DELETE" {
					w.Header().Set("Azure-AsyncOperation", statusPath)
					w.WriteHeader(202)
					return
				}
				fmt.Fprintf(w, `{"status":%q,"error":{"code":"OperationFailed","message":"private-error-detail"}}`, state)
			})
			result, err := c.Delete(t.Context(), ref, output(t), true)
			if err == nil || result.FinalState != state || result.ErrorCode != "OperationFailed" || strings.Contains(err.Error(), "private-error-detail") {
				t.Fatalf("unexpected failure: %+v %v", result, err)
			}
			if !strings.Contains(string(read(t, filepath.Join(result.OutputDir, "response-0001.json"))), "private-error-detail") {
				t.Error("private error artifact missing")
			}
			saved(t, result)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		if r.Method == "DELETE" {
			w.Header().Set("Azure-AsyncOperation", statusPath)
			w.WriteHeader(202)
			return
		}
		cancel()
		fmt.Fprint(w, `{"status":"Running"}`)
	})
	result, err := c.Delete(ctx, ref, output(t), true)
	if !errors.Is(err, context.Canceled) || result.OperationURL == "" || result.FinalState != "" {
		t.Fatalf("lost partial result: %+v %v", result, err)
	}
	saved(t, result)
}

func TestCredentialsStayPrivate(t *testing.T) {
	for _, version := range []string{"2024-06-10-preview", "2025-12-23-preview"} {
		for _, async := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/async=%t", version, async), func(t *testing.T) {
				const kubeconfig = "apiVersion: v1\nusers:\n- token: super-secret-token\n"
				var posts int
				var methods []string
				c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
					methods = append(methods, r.Method+" "+r.URL.Path)
					w.Header().Set("Retry-After", "0")
					if r.URL.Query().Get("api-version") != version {
						t.Error("incorrect credential API version")
					}
					if r.Method == "POST" {
						posts++
						body, _ := io.ReadAll(r.Body)
						if len(body) != 0 || r.ContentLength != 0 || !strings.HasSuffix(r.URL.Path, "/requestAdminCredential") {
							t.Error("credential request must be a bodyless exact action")
						}
						if async {
							w.Header().Set("Azure-AsyncOperation", statusPath)
							w.Header().Set("Location", resultPath)
							w.WriteHeader(202)
							return
						}
					} else if r.URL.Path == statusPath {
						fmt.Fprint(w, `{"status":"Succeeded","ignored":"super-secret-token"}`)
						return
					} else if r.URL.Path != resultPath {
						t.Error("unexpected GET")
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"kubeconfig": kubeconfig})
				})
				c.config.APIVersion = version
				path := filepath.Join(t.TempDir(), "admin.kubeconfig")
				result, err := c.RequestAdminCredential(t.Context(), ref, output(t), path)
				if err != nil {
					t.Fatal(err)
				}
				if string(read(t, path)) != kubeconfig || result.CredentialFile != path || result.FinalState != "Succeeded" {
					t.Fatal("credential output was not decoded literally")
				}
				info, _ := os.Stat(path)
				if info.Mode().Perm() != 0600 {
					t.Error("credential permissions are not 0600")
				}
				data, _ := json.Marshal(result)
				if strings.Contains(string(data), "super-secret") || strings.Contains(fmt.Sprintf("%+v", result), "super-secret") {
					t.Error("result exposes credential contents")
				}
				entries, _ := os.ReadDir(result.OutputDir)
				for _, entry := range entries {
					if strings.Contains(string(read(t, filepath.Join(result.OutputDir, entry.Name()))), "super-secret") {
						t.Errorf("credential leaked into %s", entry.Name())
					}
					if strings.HasPrefix(entry.Name(), "response-") {
						t.Error("credential response body archived")
					}
				}
				saved(t, result)
				if _, err := c.RequestAdminCredential(t.Context(), ref, output(t), path); err == nil || posts != 1 {
					t.Error("credential collision overwrote output or issued a POST")
				}
				if string(read(t, path)) != kubeconfig {
					t.Error("existing credential was modified")
				}
				if async && len(methods) != 3 {
					t.Fatalf("unexpected request flow: %v", methods)
				}
			})
		}
	}
}

func TestCredentialErrorAndInspectDoNotReturnBodies(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(400)
		}
		fmt.Fprint(w, `{"kubeconfig":"secret-credential","error":{"code":"BadRequest","message":"secret-credential"}}`)
	})
	result, err := c.RequestAdminCredential(t.Context(), ref, output(t), filepath.Join(t.TempDir(), "config"))
	if err == nil || strings.Contains(err.Error(), "secret-credential") || result.CredentialFile != "" {
		t.Fatalf("credential error leaked: %+v %v", result, err)
	}
	entries, _ := os.ReadDir(result.OutputDir)
	for _, entry := range entries {
		if strings.Contains(string(read(t, filepath.Join(result.OutputDir, entry.Name()))), "secret-credential") {
			t.Error("credential error body archived")
		}
	}
	result, err = c.Inspect(t.Context(), ref, output(t))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "secret-credential") {
		t.Error("GET body leaked into result")
	}
}

func TestDeleteAndOutputSafety(t *testing.T) {
	for _, async := range []bool{false, true} {
		var calls int
		c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method == "DELETE" && async {
				w.Header().Set("Location", resultPath)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(202)
				return
			}
			w.WriteHeader(204)
		})
		if _, err := c.Delete(t.Context(), ref, output(t), false); err == nil || calls != 0 {
			t.Error("deletion without consent")
		}
		if _, err := c.Delete(t.Context(), Resource{ResourceGroup: "customer-rg"}, output(t), true); err == nil || calls != 0 {
			t.Error("resource group deletion allowed")
		}
		dir := t.TempDir()
		if _, err := c.Delete(t.Context(), ref, dir, true); err == nil || calls != 0 {
			t.Error("existing directory accepted")
		}
		r := ref
		r.NodePool = "workers"
		result, err := c.Delete(t.Context(), r, output(t), true)
		if err != nil || result.FinalState != "Succeeded" || !strings.HasSuffix(result.ResourceID, "/nodePools/workers") {
			t.Fatalf("delete completion: %+v %v", result, err)
		}
		saved(t, result)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientCloneAndDeadlines(t *testing.T) {
	client := &http.Client{Timeout: time.Hour, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	client.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 30*time.Second {
			t.Error("request lacks bounded deadline")
		}
		return nil, errors.New("secret transport detail")
	})
	c, err := New(config("http://localhost:8443"), client)
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout != time.Hour || client.CheckRedirect(nil, nil) != nil || c.http.Timeout != 30*time.Second || c.http.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Error("supplied client mutated or redirect policy missing")
	}
	_, err = c.Inspect(t.Context(), ref, output(t))
	if err == nil || strings.Contains(err.Error(), "secret transport detail") {
		t.Fatal("transport details leaked")
	}
	client.Timeout = time.Millisecond
	c, _ = New(config("http://localhost:8443"), client)
	if c.http.Timeout != time.Millisecond {
		t.Error("short supplied timeout ignored")
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Duration
	}{{"0", 0}, {"2", 2 * time.Second}, {"99999999999", 30 * time.Second}, {"-1", 10 * time.Second}, {"", 10 * time.Second}, {"invalid", 10 * time.Second}, {now.Add(time.Hour).Format(http.TimeFormat), 30 * time.Second}, {now.Add(-time.Hour).Format(http.TimeFormat), 0}} {
		if got := retryAfter(test.value, now); got != test.want {
			t.Errorf("Retry-After %q: %v != %v", test.value, got, test.want)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sleep(ctx, 30*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatal("sleep ignored cancellation")
	}
}

func TestTLSAndBodyTimeoutPreserveOperation(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Azure-AsyncOperation", statusPath)
		w.WriteHeader(202)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	supplied := s.Client()
	supplied.Timeout = 50 * time.Millisecond
	c, err := New(config(s.URL), supplied)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Delete(t.Context(), ref, output(t), true)
	if !errors.Is(err, context.DeadlineExceeded) || result.OperationURL == "" || result.HTTPStatus != 202 || result.FinalState != "" {
		t.Fatalf("accepted request metadata lost on body timeout: %+v %v", result, err)
	}
	saved(t, result)
}

func TestCredentialLocationOnlyAndPendingResult(t *testing.T) {
	polls := 0
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		if r.Method == "POST" {
			w.Header().Set("Location", resultPath)
			w.WriteHeader(202)
			return
		}
		if r.URL.Path != resultPath {
			t.Error("polled outside result route")
		}
		polls++
		if polls == 1 {
			w.WriteHeader(202)
			return
		}
		fmt.Fprint(w, `{"kubeconfig":"literal-not-base64"}`)
	})
	path := filepath.Join(t.TempDir(), "config")
	result, err := c.RequestAdminCredential(t.Context(), ref, output(t), path)
	if err != nil || result.FinalState != "Succeeded" || polls != 2 || string(read(t, path)) != "literal-not-base64" {
		t.Fatalf("Location-only credentials failed: %+v %v", result, err)
	}
}

func TestCredentialSymlinkCollision(t *testing.T) {
	c, _ := New(config("http://localhost:8443"), nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	target := filepath.Join(dir, "untouched")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RequestAdminCredential(t.Context(), ref, output(t), path); err == nil {
		t.Fatal("accepted credential output symlink")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("followed credential symlink")
	}
}

func TestRPProvisioningStates(t *testing.T) {
	// All values from coreapi.ListProvisioningStates, including ExternalAuth's
	// AwaitingSecret, which can appear on the shared operation status route.
	for _, state := range []string{"Accepted", "Deleting", "Provisioning", "Updating", "AwaitingSecret", "Succeeded", "Failed", "Canceled"} {
		for _, body := range []string{fmt.Sprintf(`{"status":%q}`, state), fmt.Sprintf(`{"properties":{"provisioningState":%q}}`, state)} {
			if got := responseState([]byte(body)); got != state {
				t.Errorf("state %s decoded as %s", state, got)
			}
		}
	}
}

func TestResumeReadOnly(t *testing.T) {
	for _, nodePool := range []string{"", "workers"} {
		for _, absent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/absent=%t", nodePool, absent), func(t *testing.T) {
				states := []string{"Accepted", "Provisioning", "Updating", "Deleting", "AwaitingSecret", "Succeeded"}
				polls, gets := 0, 0
				dir := output(t)
				r := ref
				r.NodePool = nodePool
				resourceSuffix := "/hcpOpenShiftClusters/" + r.Cluster
				if nodePool != "" {
					resourceSuffix += "/nodePools/" + nodePool
				}
				c, _ := newClient(t, func(w http.ResponseWriter, req *http.Request) {
					if req.Method != "GET" {
						t.Fatalf("resume sent mutation %s", req.Method)
					}
					body, _ := io.ReadAll(req.Body)
					if len(body) != 0 {
						t.Error("resume sent a payload")
					}
					w.Header().Set("Retry-After", "0")
					if req.URL.Path == statusPath {
						var disk Result
						if json.Unmarshal(read(t, filepath.Join(dir, "operation.json")), &disk) != nil || disk.OperationURL == "" {
							t.Error("operation URL not saved before polling")
						}
						fmt.Fprintf(w, `{"status":%q,"kubeconfig":"do-not-archive-poll-body"}`, states[polls])
						polls++
						return
					}
					gets++
					if !strings.HasSuffix(req.URL.Path, resourceSuffix) {
						t.Errorf("unexpected resource GET %s", req.URL.Path)
					}
					if absent {
						w.WriteHeader(404)
						return
					}
					fmt.Fprint(w, succeeded)
				})
				result, err := c.Resume(t.Context(), r, statusPath, dir)
				if err != nil || result.FinalState != "Succeeded" || result.Method != "GET" || polls != len(states) || gets != 1 {
					t.Fatalf("resume: %+v %v polls=%d gets=%d", result, err, polls, gets)
				}
				if absent && result.HTTPStatus != 404 {
					t.Error("successful absence not recorded")
				}
				saved(t, result)
				entries, _ := os.ReadDir(dir)
				for _, entry := range entries {
					if entry.Name() == "payload.json" || strings.Contains(string(read(t, filepath.Join(dir, entry.Name()))), "do-not-archive-poll-body") {
						t.Errorf("unexpected content in %s", entry.Name())
					}
				}
			})
		}
	}
}

func TestResumeErrorsAndCancellation(t *testing.T) {
	for _, state := range []string{"Failed", "Canceled", "NeverHeardOfThis", "Provisioning"} {
		t.Run(state, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != statusPath {
					t.Errorf("unexpected resume request %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprintf(w, `{"status":%q,"error":{"code":"OperationFailed"}}`, state)
				w.(http.Flusher).Flush()
				if state == "Provisioning" {
					cancel()
				}
			})
			result, err := c.Resume(ctx, ref, statusPath, output(t))
			if err == nil || result.OperationURL == "" {
				t.Fatalf("missing partial result or error: %+v %v", result, err)
			}
			if state == "Provisioning" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if state == "NeverHeardOfThis" && (!strings.Contains(err.Error(), "unrecognized RP provisioning state") || result.FinalState != "") {
				t.Fatalf("unknown state handling: %+v %v", result, err)
			}
			if (state == "Failed" || state == "Canceled") && result.FinalState != state {
				t.Error("terminal state lost")
			}
			saved(t, result)
		})
	}
}

func TestResumeURLSafety(t *testing.T) {
	var calls atomic.Int32
	c, s := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/credential-secret", http.StatusTemporaryRedirect)
	})
	for _, target := range []string{"", resultPath, "https://evil.example" + statusPath, strings.Replace(statusPath, subID, tenantID, 1), statusPath + "/../op", statusPath + "?token=secret", "/requestAdminCredential"} {
		if _, err := c.Resume(t.Context(), ref, target, output(t)); err == nil {
			t.Errorf("accepted unsafe resume URL %s", target)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unsafe resume sent a request")
	}
	result, err := c.Resume(t.Context(), ref, s.URL+statusPath, output(t))
	if err == nil || result.HTTPStatus != 307 || calls.Load() != 1 {
		t.Fatalf("resume followed redirect: %+v %v", result, err)
	}
}

func TestResumeRequiresFinalResourceSuccess(t *testing.T) {
	for _, body := range []string{`{}`, `{"status":"Succeeded"}`, `{"properties":{"provisioningState":"Provisioning"}}`, `{"properties":{"provisioningState":"Failed"}}`} {
		c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == statusPath {
				fmt.Fprint(w, `{"status":"Succeeded"}`)
				return
			}
			fmt.Fprint(w, body)
		})
		result, err := c.Resume(t.Context(), ref, statusPath, output(t))
		if err == nil || result.FinalState == "Succeeded" {
			t.Fatalf("claimed success without successful resource: %+v %v", result, err)
		}
		saved(t, result)
	}
}

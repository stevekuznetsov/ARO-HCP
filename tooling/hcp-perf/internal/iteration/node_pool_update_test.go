// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iteration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func scaleOutSpec(endpoint string) Spec {
	spec := testSpec()
	a := &spec.Actions[0]
	a.ID, a.Kind, a.Recovery = "scale", RPUpdateNodePool, RemoteMutation
	a.RP.Config.Endpoint, a.RP.Resource.NodePool = endpoint, "workers"
	replicas := 4
	a.RP.Replicas = &replicas
	a.RP.ExpectedResource = json.RawMessage(`{"id":"/subscriptions/` + a.RP.Config.SubscriptionID + `/resourceGroups/group/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/cluster/nodePools/workers","location":"eastus","properties":{"provisioningState":"Succeeded","replicas":2,"platform":{"vmSize":"Standard_D4s_v5","subnetId":"/original/subnet"},"version":{"id":"4.21"}}}`)
	return spec
}

func TestNodePoolUpdateTypedValidation(t *testing.T) {
	for name, change := range map[string]func(*Action){
		"missing-replicas":    func(a *Action) { a.RP.Replicas = nil },
		"arbitrary-body":      func(a *Action) { a.RP.Payload = json.RawMessage(`{"properties":{"replicas":4}}`) },
		"missing-snapshot":    func(a *Action) { a.RP.ExpectedResource = nil },
		"wrong-target":        func(a *Action) { a.RP.Resource.NodePool = "other" },
		"wrong-recovery":      func(a *Action) { a.Recovery = ReadOnly },
		"irrelevant-replicas": func(a *Action) { a.Kind = RPInspect; a.RP.ExpectedResource = nil },
		"operation-input":     func(a *Action) { a.RP.OperationURL = "https://other.invalid" },
		"image-guard":         func(a *Action) { a.RP.Image = "image" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := scaleOutSpec("http://localhost")
			if err := spec.Validate(); err != nil {
				t.Fatal(err)
			}
			change(&spec.Actions[0])
			if err := spec.Validate(); err == nil {
				t.Fatal("unsafe typed action accepted")
			}
		})
	}
}

func TestNodePoolUpdateExecutionAndResume(t *testing.T) {
	for _, scenario := range []string{"initial", "resume", "wrong-final-count", "wrong-receipt", "lost-receipt"} {
		t.Run(scenario, func(t *testing.T) {
			var patches, calls atomic.Int32
			expected := string(scaleOutSpec("http://localhost").Actions[0].RP.ExpectedResource)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "0")
				if r.Method == http.MethodPatch {
					patches.Add(1)
					body, _ := io.ReadAll(r.Body)
					if scenario != "initial" || string(body) != `{"properties":{"replicas":4}}` {
						t.Error("unsafe PATCH")
					}
					w.Header().Set("Azure-AsyncOperation", "/subscriptions/11111111-1111-1111-1111-111111111111/providers/Microsoft.RedHatOpenShift/locations/test/hcpOperationStatuses/scale")
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if r.Method != http.MethodGet {
					t.Error("unexpected mutation")
					w.WriteHeader(500)
					return
				}
				if strings.Contains(r.URL.Path, "hcpOperationStatuses") {
					fmt.Fprint(w, `{"status":"Succeeded"}`)
					return
				}
				body := expected
				if scenario != "wrong-final-count" && (scenario != "initial" || patches.Load() > 0) {
					body = strings.Replace(body, `"replicas":2`, `"replicas":4`, 1)
				}
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			c := testController(t, nil)
			spec := scaleOutSpec(server.URL)
			token := submit(t, c, spec)
			if scenario != "initial" {
				if scenario != "lost-receipt" {
					receipt := acceptedReceipt(t, spec.Actions[0])
					if receipt.Method != http.MethodPatch {
						t.Fatal("receipt method not PATCH")
					}
					if scenario == "wrong-receipt" {
						receipt.Method = http.MethodPut
					}
					saveReceipt(t, c, token, receipt)
				}
				loseAttempt(t, c, 0)
				if err := c.Reconcile(t.Context(), "run"); err != nil {
					t.Fatal(err)
				}
				run := status(t, c)
				if scenario == "lost-receipt" || scenario == "wrong-receipt" {
					if run.State != Blocked || len(run.Actions[0].Attempts) != 1 || calls.Load() != 0 {
						t.Fatalf("unsafe receipt recovery: %+v", run)
					}
					if err := c.ReconcileOperation(t.Context(), "run", "scale", time.Second); err == nil {
						t.Fatal("manual reconcile accepted missing/invalid receipt")
					}
					return
				}
				token = run.Actions[0].Attempts[1].Token
			}
			if err := c.Work(t.Context(), token); err != nil {
				t.Fatal(err)
			}
			run := status(t, c)
			if scenario == "wrong-final-count" {
				if run.State != Blocked {
					t.Fatalf("operation success hid wrong replicas: %+v", run)
				}
			} else if run.State != Succeeded {
				t.Fatalf("scale failed: %+v", run)
			}
			wantPatches := int32(0)
			if scenario == "initial" {
				wantPatches = 1
			}
			if patches.Load() != wantPatches {
				t.Fatal("mutation replayed")
			}
		})
	}
}

func TestNodePoolUpdatePerActionManualRecovery(t *testing.T) {
	var mutations atomic.Int32
	expected := string(scaleOutSpec("http://localhost").Actions[0].RP.ExpectedResource)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations.Add(1)
			w.WriteHeader(500)
			return
		}
		if strings.Contains(r.URL.Path, "hcpOperationStatuses") {
			fmt.Fprint(w, `{"status":"Succeeded"}`)
			return
		}
		fmt.Fprint(w, strings.Replace(expected, `"replicas":2`, `"replicas":4`, 1))
	}))
	defer server.Close()
	c := testController(t, nil)
	spec := scaleOutSpec(server.URL)
	spec.Actions[0].MaxAttempts = 1
	sibling := testSpec().Actions[0]
	sibling.ID = "failed-sibling"
	spec.Actions = append(spec.Actions, sibling)
	token := submit(t, c, spec)
	saveReceipt(t, c, token, acceptedReceipt(t, spec.Actions[0]))
	loseAttempt(t, c, 0)
	if err := c.update(t.Context(), "run", func(run *Run) error {
		run.Actions[1].State = Failed
		run.Actions[1].Attempts[0].State = Failed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOperation(t.Context(), "run", "scale", time.Second); err != nil {
		t.Fatal(err)
	}
	run := status(t, c)
	if run.Actions[0].State != Succeeded || run.Actions[1].State != Failed || mutations.Load() != 0 || len(run.Actions[0].Attempts) != 2 {
		t.Fatalf("per-action recovery failed: %+v", run)
	}
}

func TestNodePoolUpdateTimeoutBindingAndNoReplay(t *testing.T) {
	for _, receiptPresent := range []bool{true, false} {
		t.Run(fmt.Sprintf("accepted-receipt=%t", receiptPresent), func(t *testing.T) {
			var patches, polls atomic.Int32
			expected := string(scaleOutSpec("http://localhost").Actions[0].RP.ExpectedResource)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					patches.Add(1)
					if receiptPresent {
						w.Header().Set("Azure-AsyncOperation", "/subscriptions/11111111-1111-1111-1111-111111111111/providers/Microsoft.RedHatOpenShift/locations/test/hcpOperationStatuses/scale")
					}
					w.WriteHeader(http.StatusAccepted)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				if r.Method != http.MethodGet {
					t.Error("unexpected mutation")
					w.WriteHeader(500)
					return
				}
				if strings.Contains(r.URL.Path, "hcpOperationStatuses") {
					polls.Add(1)
					fmt.Fprint(w, `{"status":"Succeeded"}`)
					return
				}
				body := expected
				if patches.Load() > 0 {
					body = strings.Replace(body, `"replicas":2`, `"replicas":4`, 1)
				}
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			c := testController(t, nil)
			client := *server.Client()
			client.Timeout = 100 * time.Millisecond
			c.options.Executor = libraryExecutor{client: &client}
			spec := scaleOutSpec(server.URL)
			sibling := testSpec().Actions[0]
			sibling.ID = "failed-sibling"
			spec.Actions = append(spec.Actions, sibling)
			token := submit(t, c, spec)
			if err := c.Work(t.Context(), token); err != nil {
				t.Fatal(err)
			}
			run := status(t, c)
			if run.Actions[0].State != Blocked || (run.Actions[0].Binding.OperationURL != "") != receiptPresent {
				t.Fatalf("timeout lost binding/uncertainty: %+v", run)
			}
			if err := c.update(t.Context(), "run", func(run *Run) error {
				run.Actions[1].State, run.Actions[1].Attempts[0].State = Failed, Failed
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := c.Reconcile(t.Context(), "run"); err != nil {
				t.Fatal(err)
			}
			run = status(t, c)
			if receiptPresent {
				if len(run.Actions[0].Attempts) != 2 || run.Actions[0].Attempts[1].ResumeURL == "" {
					t.Fatalf("accepted action did not recover independently of sibling: %+v", run)
				}
				if err := c.Work(t.Context(), run.Actions[0].Attempts[1].Token); err != nil {
					t.Fatal(err)
				}
				if status(t, c).Actions[0].State != Succeeded || polls.Load() != 1 {
					t.Fatal("GET-only accepted recovery failed")
				}
			} else if len(run.Actions[0].Attempts) != 1 || run.Actions[0].State != Blocked || polls.Load() != 0 {
				t.Fatalf("uncertain mutation replayed: %+v", run)
			}
			if patches.Load() != 1 {
				t.Fatal("PATCH replayed after timeout")
			}
		})
	}
}

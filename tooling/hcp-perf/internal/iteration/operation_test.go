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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExplicitOperationReconciliation(t *testing.T) {
	for _, scenario := range []string{"failed", "canceled", "resource-only-failed", "interrupted", "wrong-receipt"} {
		t.Run(scenario, func(t *testing.T) {
			var calls, mutations atomic.Int32
			var complete atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet {
					mutations.Add(1)
					w.WriteHeader(500)
					return
				}
				if strings.Contains(r.URL.Path, "hcpOperationStatuses") {
					if scenario == "interrupted" && !complete.Load() {
						<-r.Context().Done()
						return
					}
					state := "Failed"
					if scenario == "canceled" {
						state = "Canceled"
					}
					if scenario == "resource-only-failed" {
						state = "Succeeded"
					}
					fmt.Fprintf(w, `{"status":%q}`, state)
					return
				}
				fmt.Fprint(w, `{"properties":{"provisioningState":"Failed"}}`)
			}))
			defer server.Close()
			c := testController(t, nil)
			spec := testSpec()
			spec.Actions[0].Kind, spec.Actions[0].Recovery, spec.Actions[0].MaxAttempts = RPCreateCluster, RemoteMutation, 1
			spec.Actions[0].RP.Config.Endpoint, spec.Actions[0].RP.Payload = server.URL, json.RawMessage(`{}`)
			token := submit(t, c, spec)
			receipt := acceptedReceipt(t, spec.Actions[0])
			if scenario == "wrong-receipt" {
				receipt.ResourceID += "-wrong"
			}
			saveReceipt(t, c, token, receipt)
			loseAttempt(t, c, 0)
			if err := c.Reconcile(t.Context(), "run"); err != nil {
				t.Fatal(err)
			}
			if err := c.Stop(t.Context(), "run"); err != nil {
				t.Fatal(err)
			}
			err := c.ReconcileOperation(t.Context(), "run", spec.Actions[0].ID, 100*time.Millisecond)
			run := status(t, c)
			switch scenario {
			case "failed", "canceled":
				if err != nil || run.State != Failed || len(run.Actions[0].Attempts) != 2 {
					t.Fatalf("terminal operation not resolved after maxAttempts=1: %+v %v", run, err)
				}
			case "resource-only-failed":
				if err == nil || run.State != Blocked {
					t.Fatal("resource provisioning state falsely proved failed operation")
				}
			case "wrong-receipt":
				if err == nil || calls.Load() != 0 || !run.Actions[0].ReceiptInvalid {
					t.Fatal("invalid receipt reached HTTP")
				}
			case "interrupted":
				if err == nil || run.State != Blocked {
					t.Fatal("interrupted observation lost uncertainty")
				}
				complete.Store(true)
				if err := c.ReconcileOperation(t.Context(), "run", spec.Actions[0].ID, time.Second); err != nil {
					t.Fatal(err)
				}
				if run := status(t, c); run.State != Failed || len(run.Actions[0].Attempts) != 3 {
					t.Fatal("explicit second observation did not resolve terminal operation")
				}
			}
			if mutations.Load() != 0 {
				t.Fatal("operation check replayed mutation")
			}
		})
	}
}

func TestConcurrentOperationReconciliationFenced(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "hcpOperationStatuses") {
			close(started)
			<-release
			fmt.Fprint(w, `{"status":"Failed"}`)
		} else {
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := testController(t, nil)
	spec := deleteSpec(server.URL)
	token := submit(t, c, spec)
	saveReceipt(t, c, token, acceptedReceipt(t, spec.Actions[0]))
	loseAttempt(t, c, 0)
	done := make(chan error, 1)
	go func() { done <- c.ReconcileOperation(t.Context(), "run", spec.Actions[0].ID, time.Second) }()
	<-started
	if err := c.ReconcileOperation(t.Context(), "run", spec.Actions[0].ID, time.Second); err == nil {
		t.Error("concurrent check acquired active attempt")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

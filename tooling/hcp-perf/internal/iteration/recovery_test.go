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
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/provision"
)

func deleteSpec(endpoint string) Spec {
	spec := testSpec()
	spec.Actions[0].Kind, spec.Actions[0].Recovery = RPDelete, RemoteMutation
	spec.Actions[0].RP.ConfirmDelete = true
	spec.Actions[0].RP.Config.Endpoint = endpoint
	return spec
}

func acceptedReceipt(t *testing.T, action Action) provision.Result {
	t.Helper()
	client, err := provision.New(action.RP.Config, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := client.ValidateActionInputs(action.RP.Resource, action.requestMethod(), action.RP.Payload, "")
	if err != nil {
		t.Fatal(err)
	}
	operationPath := "/subscriptions/" + action.RP.Config.SubscriptionID + "/providers/Microsoft.RedHatOpenShift/locations/test/hcpOperationStatuses/" + action.ID
	operation, err := client.ValidateActionInputs(action.RP.Resource, http.MethodGet, nil, operationPath)
	if err != nil {
		t.Fatal(err)
	}
	receipt.OperationURL, receipt.HTTPStatus = operation.OperationURL, http.StatusAccepted
	return receipt
}

func saveReceipt(t *testing.T, c *Controller, token string, receipt provision.Result) {
	t.Helper()
	dir := filepath.Join(c.attemptDir("run", token), "rp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "operation.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func loseAttempt(t *testing.T, c *Controller, index int) {
	t.Helper()
	if err := c.update(t.Context(), "run", func(run *Run) error {
		status := &run.Actions[index]
		status.State = Running
		status.Attempts[len(status.Attempts)-1].State = Running
		summarize(run)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptedRecoveriesIgnoreSiblingFailures(t *testing.T) {
	var mutations, polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations.Add(1)
			w.WriteHeader(500)
			return
		}
		if strings.Contains(r.URL.Path, "hcpOperationStatuses") {
			polls.Add(1)
			fmt.Fprint(w, `{"status":"Succeeded"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	c := testController(t, nil)
	spec := deleteSpec(server.URL)
	second := deleteSpec(server.URL).Actions[0]
	second.ID, second.RP.Resource.Cluster = "second", "other"
	sibling := testSpec().Actions[0]
	sibling.ID = "failed-read"
	spec.Actions = append(spec.Actions, second, sibling)
	submit(t, c, spec)
	run := status(t, c)
	for i := range 2 {
		saveReceipt(t, c, run.Actions[i].Attempts[0].Token, acceptedReceipt(t, spec.Actions[i]))
		loseAttempt(t, c, i)
	}
	if err := c.update(t.Context(), "run", func(run *Run) error {
		run.Actions[2].State, run.Actions[2].Attempts[0].State = Failed, Failed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	run = status(t, c)
	for i := range 2 {
		if len(run.Actions[i].Attempts) != 2 {
			t.Fatalf("accepted action %d not recovered with failed siblings: %+v", i, run)
		}
		if err := c.Work(t.Context(), run.Actions[i].Attempts[1].Token); err != nil {
			t.Fatal(err)
		}
	}
	run = status(t, c)
	if run.Actions[0].State != Succeeded || run.Actions[1].State != Succeeded || polls.Load() != 2 || mutations.Load() != 0 {
		t.Fatalf("recovery did not safely complete: %+v polls=%d mutations=%d", run, polls.Load(), mutations.Load())
	}
}

func TestClaimRechecksInitialEligibility(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovery=%t", recovery), func(t *testing.T) {
			var calls atomic.Int32
			c := testController(t, executorFunc(func(context.Context, Action, string, string) error {
				calls.Add(1)
				return nil
			}))
			spec := deleteSpec("http://localhost")
			sibling := testSpec().Actions[0]
			sibling.ID = "sibling"
			spec.Actions = append(spec.Actions, sibling)
			token := submit(t, c, spec)
			if recovery {
				saveReceipt(t, c, token, acceptedReceipt(t, spec.Actions[0]))
				loseAttempt(t, c, 0)
				if err := c.Reconcile(t.Context(), "run"); err != nil {
					t.Fatal(err)
				}
				token = status(t, c).Actions[0].Attempts[1].Token
			}
			if err := c.update(t.Context(), "run", func(run *Run) error {
				run.Actions[1].State, run.Actions[1].Attempts[0].State = Failed, Failed
				summarize(run)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Submit(t.Context(), "too-early", spec); err == nil {
				t.Fatal("claim released with a dispatched attempt")
			}
			if err := c.Work(t.Context(), token); err != nil {
				t.Fatal(err)
			}
			if recovery && calls.Load() != 1 {
				t.Fatal("accepted recovery suppressed at claim")
			}
			if !recovery && (calls.Load() != 0 || status(t, c).Actions[0].State != Stopped) {
				t.Fatal("initial mutation executed despite failed sibling")
			}
		})
	}
}

func TestInvalidReceiptsBlockBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(500)
	}))
	defer server.Close()
	for name, corrupt := range map[string]func(*provision.Result){
		"version":  func(r *provision.Result) { r.Version = "v2" },
		"resource": func(r *provision.Result) { r.ResourceID += "-other" },
		"request":  func(r *provision.Result) { r.RequestURL += "&extra=value" },
		"method":   func(r *provision.Result) { r.Method = http.MethodPut },
		"credential-route": func(r *provision.Result) {
			r.OperationURL = strings.Replace(r.OperationURL, "hcpOperationStatuses", "requestAdminCredential", 1)
		},
		"other-origin":    func(r *provision.Result) { r.OperationURL = "https://other.invalid/operation" },
		"changed-binding": func(r *provision.Result) { r.OperationURL = strings.Replace(r.OperationURL, "/inspect?", "/other?", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			c := testController(t, nil)
			spec := deleteSpec(server.URL)
			token := submit(t, c, spec)
			receipt := acceptedReceipt(t, spec.Actions[0])
			saveReceipt(t, c, token, receipt)
			loseAttempt(t, c, 0)
			if err := c.Reconcile(t.Context(), "run"); err != nil {
				t.Fatal(err)
			}
			resume := status(t, c).Actions[0].Attempts[1].Token
			corrupt(&receipt)
			saveReceipt(t, c, token, receipt)
			if err := c.Work(t.Context(), resume); err != nil {
				t.Fatal(err)
			}
			if err := c.Reconcile(t.Context(), "run"); err != nil {
				t.Fatal(err)
			}
			run := status(t, c)
			if run.State != Blocked || !run.Actions[0].ReceiptInvalid || len(run.Actions[0].Attempts) != 2 {
				t.Fatalf("invalid receipt not durably fenced: %+v", run)
			}
			if _, err := c.Submit(t.Context(), "other", spec); err == nil {
				t.Fatal("invalid receipt released environment claim")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid receipt caused %d HTTP requests", requests.Load())
	}
}

func TestMalformedReceiptCompletionAndServeIsolation(t *testing.T) {
	for _, workerCompletes := range []bool{false, true} {
		t.Run(fmt.Sprintf("workerCompletes=%t", workerCompletes), func(t *testing.T) {
			executor := executorFunc(func(_ context.Context, _ Action, dir, _ string) error {
				if err := os.Mkdir(filepath.Join(dir, "rp"), 0700); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "rp", "operation.json"), []byte("{broken"), 0600)
			})
			c := testController(t, executor)
			spec := deleteSpec("http://localhost")
			token := submit(t, c, spec)
			if workerCompletes {
				if err := c.Work(t.Context(), token); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := executor.Execute(t.Context(), spec.Actions[0], c.attemptDir("run", token), ""); err != nil {
					t.Fatal(err)
				}
				loseAttempt(t, c, 0)
			}
			healthy := testSpec()
			healthy.Environment.ID = "healthy"
			if _, err := c.Submit(t.Context(), "healthy", healthy); err != nil {
				t.Fatal(err)
			}
			var dispatched atomic.Int32
			c.options.ProcessFactory = func(string, string, string) error { dispatched.Add(1); return nil }
			serveUntil(t, c, func() bool {
				return dispatched.Load() > 0 && status(t, c).State == Blocked
			})
			run := status(t, c)
			if dispatched.Load() == 0 || run.State != Blocked || run.Actions[0].Attempts[0].State != Blocked {
				t.Fatalf("malformed receipt not isolated: %+v dispatches=%d", run, dispatched.Load())
			}
		})
	}
}

func TestSubmissionUsesProvisionValidationBeforeArtifacts(t *testing.T) {
	for name, change := range map[string]func(*RPParameters){
		"cluster-path":      func(p *RPParameters) { p.Resource.Cluster = "../other" },
		"group-path":        func(p *RPParameters) { p.Resource.ResourceGroup = "group/other" },
		"nodepool-wildcard": func(p *RPParameters) { p.Resource.NodePool = "*" },
		"nodepool-length":   func(p *RPParameters) { p.Resource.NodePool = strings.Repeat("a", 16) },
		"duplicate":         func(p *RPParameters) { p.Payload = json.RawMessage(`{"x":1,"x":2}`) },
		"case-alias":        func(p *RPParameters) { p.Payload = json.RawMessage(`{"properties":{"x":1,"X":2}}`) },
		"api-version":       func(p *RPParameters) { p.Payload = json.RawMessage(`{"apiVersion":"wrong"}`) },
		"size":              func(p *RPParameters) { p.Payload = json.RawMessage(`{"x":"` + strings.Repeat("a", 16<<20) + `"}`) },
	} {
		t.Run(name, func(t *testing.T) {
			c := testController(t, nil)
			spec := deleteSpec("http://localhost")
			spec.Actions[0].Kind = RPCreateNodePool
			spec.Actions[0].RP.Resource.NodePool = "pool"
			spec.Actions[0].RP.Payload = json.RawMessage(`{}`)
			change(spec.Actions[0].RP)
			if _, err := c.Submit(t.Context(), "invalid", spec); err == nil {
				t.Fatal("invalid RP action accepted")
			}
			if _, err := os.Stat(filepath.Join(c.root, "invalid")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid input reserved an output directory: %v", err)
			}
			var claims int
			if err := c.db.QueryRow("SELECT count(*) FROM claims").Scan(&claims); err != nil || claims != 0 {
				t.Fatalf("invalid input claimed environment: count=%d err=%v", claims, err)
			}
		})
	}
}

func TestDatabaseVersionEnforcement(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", filepath.Join(root, "iteration.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(fmt.Sprintf("CREATE TABLE unknown (value TEXT); PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			if c, err := Open(root); err == nil {
				c.Close()
				t.Fatal("unknown schema accepted")
			}
			var tables, actual int
			if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table'").Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("PRAGMA user_version").Scan(&actual); err != nil {
				t.Fatal(err)
			}
			if tables != 1 || actual != version {
				t.Fatal("unsupported database was modified")
			}
		})
	}
	c := testController(t, nil)
	var version int
	if err := c.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("new schema version not persisted: version=%d err=%v", version, err)
	}
	other, err := Open(c.root)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
}

func TestRecoveryReceiptCannotChangeOperation(t *testing.T) {
	c := testController(t, nil)
	spec := deleteSpec("http://localhost")
	token := submit(t, c, spec)
	receipt := acceptedReceipt(t, spec.Actions[0])
	saveReceipt(t, c, token, receipt)
	loseAttempt(t, c, 0)
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	run := status(t, c)
	resumeToken := run.Actions[0].Attempts[1].Token
	receipt.Method = http.MethodGet
	receipt.OperationURL = strings.Replace(receipt.OperationURL, "/inspect?", "/other?", 1)
	saveReceipt(t, c, resumeToken, receipt)
	loseAttempt(t, c, 0)
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	run = status(t, c)
	if !run.Actions[0].ReceiptInvalid || run.State != Blocked || len(run.Actions[0].Attempts) != 2 {
		t.Fatalf("recovery receipt replaced operation binding: %+v", run)
	}
}

// Wait for reconciliation outcomes, not a fixed number of milliseconds of disk I/O.
func serveUntil(t *testing.T, c *Controller, ready func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx, 10*time.Millisecond) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Serve stopped before isolation was observed: %v", err)
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for run isolation and healthy dispatch")
		case <-ticker.C:
		}
	}
}

func TestServeContinuesAfterPerRunError(t *testing.T) {
	c := testController(t, nil)
	if _, err := c.Submit(t.Context(), "run", testSpec()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.root, "run", "spec.json")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	healthy := testSpec()
	healthy.Environment.ID = "healthy"
	if _, err := c.Submit(t.Context(), "healthy", healthy); err != nil {
		t.Fatal(err)
	}
	var dispatches atomic.Int32
	c.options.ProcessFactory = func(string, string, string) error {
		dispatches.Add(1)
		return nil
	}
	serveUntil(t, c, func() bool {
		return dispatches.Load() > 0 && status(t, c).State == Blocked
	})
	if dispatches.Load() == 0 || status(t, c).State != Blocked {
		t.Fatal("damaged run was not isolated from healthy dispatch")
	}
}

func TestReadOnlyRecoveryIgnoresSiblingFailure(t *testing.T) {
	var calls int
	c := testController(t, executorFunc(func(context.Context, Action, string, string) error {
		calls++
		return nil
	}))
	spec := testSpec()
	sibling := testSpec().Actions[0]
	sibling.ID = "sibling"
	spec.Actions = append(spec.Actions, sibling)
	submit(t, c, spec)
	loseAttempt(t, c, 0)
	if err := c.update(t.Context(), "run", func(run *Run) error {
		run.Actions[1].State, run.Actions[1].Attempts[0].State = Failed, Failed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	run := status(t, c)
	if len(run.Actions[0].Attempts) != 2 {
		t.Fatal("failed sibling prevented read-only recovery")
	}
	if err := c.Work(t.Context(), run.Actions[0].Attempts[1].Token); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || status(t, c).Actions[0].State != Succeeded {
		t.Fatal("read-only recovery failed claim eligibility")
	}
}

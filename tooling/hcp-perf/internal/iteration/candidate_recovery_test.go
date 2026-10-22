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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

func TestBlockedCandidateReadOnlyRecovery(t *testing.T) {
	for _, scenario := range []string{"unchanged", "changed-rv", "changed-uid", "changed-spec", "active-lock", "live-process", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				requests++
				if r.Method != http.MethodGet {
					t.Errorf("recovery issued mutation %s", r.Method)
					w.WriteHeader(500)
					return
				}
				if scenario == "unavailable" {
					w.WriteHeader(503)
					return
				}
				uid, rv, limit := "csc-uid", "7", 5
				if scenario == "changed-rv" {
					rv = "8"
				}
				if scenario == "changed-uid" {
					uid = "new-uid"
				}
				if scenario == "changed-spec" {
					limit = 6
				}
				fmt.Fprintf(w, `{"apiVersion":"scheduling.hypershift.openshift.io/v1alpha1","kind":"ClusterSizingConfiguration","metadata":{"name":"cluster","uid":%q,"resourceVersion":%q},"spec":{"limit":%d}}`, uid, rv, limit)
			}))
			defer server.Close()
			kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
			data := fmt.Sprintf(`{"apiVersion":"v1","kind":"Config","current-context":"test","clusters":[{"name":"cluster","cluster":{"server":%q}}],"contexts":[{"name":"test","context":{"cluster":"cluster","user":"user"}}],"users":[{"name":"user","user":{}}]}`, server.URL)
			if err := os.WriteFile(kubeconfig, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			c := testController(t, nil)
			spec := testSpec()
			spec.Actions = []Action{{ID: "candidate", Kind: CandidateApply, Recovery: GuardedMutation, Timeout: time.Minute, MaxAttempts: 1, Candidate: &startup.Candidate{Kubeconfig: kubeconfig, Name: "cluster", UID: "csc-uid", ResourceVersion: "7", ExpectedSpec: json.RawMessage(`{"limit":5}`), DesiredSpec: json.RawMessage(`{"limit":6}`)}}}
			token := submit(t, c, spec)
			if err := c.update(t.Context(), "run", func(run *Run) error {
				run.Actions[0].State, run.Actions[0].Attempts[0].State, run.State = Blocked, Blocked, Blocked
				if scenario == "live-process" {
					run.Actions[0].Attempts[0].Process, _ = processIdentity(os.Getpid())
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if scenario == "active-lock" {
				lock, err := acquireLock(c.attemptDir("run", token) + ".lock")
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			if err := c.Stop(t.Context(), "run"); err != nil {
				t.Fatal(err)
			}
			if err := c.Reconcile(t.Context(), "run"); err != nil {
				t.Fatal(err)
			}
			run := status(t, c)
			if scenario == "unchanged" {
				verification := run.Actions[0].CandidateVerification
				if run.State != Failed || verification == nil || verification.AttemptToken != token || verification.SpecSHA256 != run.SpecSHA256 || verification.ResourceVersion != "7" {
					t.Fatalf("unchanged recovery not journaled: %+v", run)
				}
				if _, err := c.Submit(t.Context(), "fresh", spec); err != nil {
					t.Fatalf("verified failed run retained claim: %v", err)
				}
			} else {
				if run.State != Blocked || run.Actions[0].CandidateVerification != nil {
					t.Fatalf("uncertain candidate cleared: %+v", run)
				}
				if _, err := c.Submit(t.Context(), "fresh", spec); err == nil {
					t.Fatal("uncertain candidate released claim")
				}
			}
			if (scenario == "active-lock" || scenario == "live-process") && requests != 0 {
				t.Fatal("queried live candidate worker")
			}
			if len(run.Actions[0].Attempts) != 1 {
				t.Fatal("recovery replayed candidate")
			}
		})
	}
}

func TestCandidateNoMutationWorkerDisposition(t *testing.T) {
	c := testController(t, executorFunc(func(context.Context, Action, string, string) error {
		return fmt.Errorf("preflight: %w", &startup.NoMutationError{Err: errors.New("precondition mismatch")})
	}))
	spec := testSpec()
	spec.Actions = []Action{{ID: "candidate", Kind: CandidateApply, Recovery: GuardedMutation, Timeout: time.Minute, MaxAttempts: 1, Candidate: &startup.Candidate{Kubeconfig: "/external/kubeconfig", Name: "cluster", UID: "uid", ResourceVersion: "7", ExpectedSpec: json.RawMessage(`{"limit":5}`), DesiredSpec: json.RawMessage(`{"limit":6}`)}}}
	token := submit(t, c, spec)
	if err := c.Work(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if status(t, c).State != Failed {
		t.Fatal("known pre-dispatch error remained uncertain")
	}
}

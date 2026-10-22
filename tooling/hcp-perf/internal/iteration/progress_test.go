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
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

func TestStartupProgressDurableSnapshot(t *testing.T) {
	for _, scenario := range []string{"current", "legacy-identity-error", "stale", "future", "missing", "malformed", "oversized", "symlink", "fifo", "wrong-resource", "secret-error"} {
		t.Run(scenario, func(t *testing.T) {
			c := testController(t, nil)
			spec := startupIterationSpec()
			if _, err := c.Submit(t.Context(), "run", spec); err != nil {
				t.Fatal(err)
			}
			const token = "00000000000000000000000000000001"
			if err := os.Mkdir(c.attemptDir("run", token), 0700); err != nil {
				t.Fatal(err)
			}
			identity, err := processIdentity(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.update(t.Context(), "run", func(run *Run) error {
				run.Actions[2].State = Running
				run.Actions[2].Attempts = []Attempt{{Token: token, State: Running, Process: identity}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			sample := startup.Sample{At: now, Identity: startup.Identity{ResourceID: spec.Actions[2].Startup.ResourceID, HCPUID: "hcp"}, Issues: []string{"observed pods not ready: Deployment/kube-apiserver"}, Coverage: &startup.Coverage{ReadyPods: 2, RequiredPods: 12, ReadyEndpoints: 1, RequiredEndpoints: 12, Complete: true}, LastSuccessfulPoll: &now, NewOOMs: 1, NewProbeFailures: 2}
			path := filepath.Join(c.attemptDir("run", token), "startup-status.json")
			switch scenario {
			case "legacy-identity-error":
				sample.Coverage = nil
				sample.Issues = []string{"HostedCluster CS identity mismatch"}
			case "stale":
				sample.At = now.Add(-5 * time.Minute)
			case "future":
				sample.At = now.Add(time.Hour)
			case "wrong-resource":
				sample.Identity.ResourceID = "other"
			case "secret-error":
				sample.Issues = []string{"GET https://secret.invalid?token=TOPSECRET", "observed pods not ready: TOPSECRET=https://secret.invalid"}
			}
			data, err := json.Marshal(sample)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing":
			case "malformed":
				if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(path, []byte(strings.Repeat(" ", maxProgressBytes+1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("/external/credential", path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			run := status(t, c)
			progress := run.Actions[2].StartupProgress
			if progress == nil || !run.Actions[2].WorkerIdentityVerified {
				t.Fatal("running observer diagnostics missing")
			}
			switch scenario {
			case "current":
				if progress.Stale || progress.Coverage == nil || progress.Coverage.ReadyPods != 2 || progress.LastError != "pods-not-ready" || len(progress.UnhealthyWorkloads) != 1 {
					t.Fatalf("bad progress: %+v", progress)
				}
			case "legacy-identity-error":
				if progress.LastError != "cs-identity-mismatch" || progress.Coverage != nil {
					t.Fatalf("legacy evidence hidden or invented: %+v", progress)
				}
			case "stale", "future":
				if !progress.Stale || progress.Phase != "stale" {
					t.Fatal("old/future snapshot treated as live")
				}
			case "secret-error":
				encoded, _ := json.Marshal(progress)
				if strings.Contains(string(encoded), "TOPSECRET") || strings.Contains(string(encoded), "secret.invalid") {
					t.Fatal("raw error leaked into progress")
				}
			default:
				if progress.SnapshotState == "available" || !progress.Stale {
					t.Fatalf("invalid snapshot accepted: %+v", progress)
				}
			}
			// A malformed busy-worker snapshot must not terminate reconciliation.
			lock, err := acquireLock(c.attemptDir("run", token) + ".lock")
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Reconcile(t.Context(), "run"); err != nil {
				lock.Close()
				t.Fatal(err)
			}
			lock.Close()
			run = status(t, c)
			if run.LastReconciledAt == nil || run.ReconcileState != "completed" || !run.ControllerIdentityVerified {
				t.Fatal("reconcile heartbeat missing")
			}
			list, err := c.List(t.Context())
			if err != nil || len(list) != 1 || list[0].Actions[2].StartupProgress == nil {
				t.Fatalf("list omitted progress: %v", err)
			}
		})
	}
}

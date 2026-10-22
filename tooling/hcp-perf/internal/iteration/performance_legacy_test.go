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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/performance"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func legacyPerformanceSpec() Spec {
	s := testSpec()
	p := performance.Spec{Version: "v1", Startup: *startupIterationSpec().Actions[2].Startup, GuestKubeconfig: "/external/guest", Seed: seed.Spec{Version: "v1", ID: "fresh", NamespacePrefix: "fresh", GuestNodes: 3, Applications: 0, Image: workload.CanaryImage, PerPodCPU: "5m", PerPodMemory: "8Mi", TimeoutSeconds: 300, Binary: seed.Binary{Path: "/external/burner", ExpectedSHA256: strings.Repeat("a", 64)}}, MeasurementSeconds: 300, SetupSeconds: 1800, RecoverySeconds: 30, CanaryCadenceSeconds: 30, MaxOutstanding: 1, Capture: performance.Capture{Endpoint: "http://127.0.0.1:1", Reference: "external", Sources: []performance.Source{{Job: "kas", Replicas: 3}}, Queries: []metrics.Query{{Name: "up", Expr: "up"}}}}
	s.Actions = []Action{{ID: "performance", Kind: PerformanceRun, Recovery: Measurement, Timeout: 2 * time.Hour, MaxAttempts: 1, Performance: &p}}
	return s
}

func TestLegacyPerformanceJournalAcrossControllerUpgrade(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		for _, state := range []State{Prepared, Running, Blocked, Succeeded, Failed, Stopped} {
			t.Run(fmt.Sprintf("cleanup=%t/%s", cleanup, state), func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "journal")
				c, err := Open(root)
				if err != nil {
					t.Fatal(err)
				}
				spec := legacyPerformanceSpec()
				if cleanup {
					spec.Actions[0] = Action{ID: "cleanup", Kind: PerformanceCleanup, Recovery: GuardedMutation, Timeout: time.Hour, MaxAttempts: 1, PerformanceCleanup: &performance.CleanupSpec{Source: *spec.Actions[0].Performance, Directory: "/external/case", OutcomeSHA256: strings.Repeat("a", 64), Objects: []performance.CleanupObject{{Kind: "Namespace", Name: "hcp-perf-canary-fresh", UID: "ns"}, {Kind: "FlowSchema", Name: "hcp-perf-canary-fresh", UID: "fs"}, {Kind: "PriorityLevelConfiguration", Name: "hcp-perf-benchmark-fresh", UID: "pl1"}, {Kind: "PriorityLevelConfiguration", Name: "hcp-perf-canary-fresh", UID: "pl2"}}}}
				}
				if err := spec.Validate(); err != nil {
					t.Fatal(err)
				}
				if cleanup {
					// A new cleanup may reference an unchanged historical source;
					// it is not a new legacy measurement.
					cleanupController := testController(t, nil)
					if _, err := cleanupController.Submit(t.Context(), "cleanup-old-source", spec); err != nil {
						t.Fatal("cleanup of historical source rejected", err)
					}
				}
				data, err := json.Marshal(spec)
				if err != nil {
					t.Fatal(err)
				}
				// Emulate the old writer's bytes, not a new submission or migration.
				data = bytes.ReplaceAll(data, []byte(`"expectedGuestNodes":null,`), nil)
				dir := filepath.Join(root, "old")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				worker := []byte("immutable historical worker stand-in; never executed")
				for name, raw := range map[string][]byte{"spec.json": data, "worker": worker} {
					if err := writePrivate(filepath.Join(dir, name), raw, 0500); err != nil {
						t.Fatal(err)
					}
				}
				run := Run{ID: "old", Environment: spec.Environment, State: state, SpecSHA256: fmt.Sprintf("%x", sha256.Sum256(data)), BinarySHA256: fmt.Sprintf("%x", sha256.Sum256(worker)), Actions: []ActionStatus{{ID: spec.Actions[0].ID, State: state}}}
				if state != Prepared {
					run.Actions[0].Attempts = []Attempt{{Token: "old-attempt", State: state}}
				}
				raw, _ := json.Marshal(run)
				if _, err := c.db.Exec("INSERT INTO runs VALUES (?,?)", run.ID, raw); err != nil {
					t.Fatal(err)
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				c, err = Open(root)
				if err != nil {
					t.Fatal("upgraded controller Open", err)
				}
				defer c.Close()
				dispatches := 0
				c.options.ProcessFactory = func(binary, gotRoot, token string) error {
					dispatches++
					if binary != filepath.Join(dir, "worker") || gotRoot != root || token == "" {
						t.Fatal("historical worker was substituted")
					}
					return verifyBinary(binary, run.BinarySHA256)
				}
				if state == Running {
					lock, err := acquireLock(filepath.Join(dir, "old-attempt.lock"))
					if err != nil {
						t.Fatal(err)
					}
					defer lock.Close()
				}
				if _, err := c.Status(t.Context(), run.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := c.List(t.Context()); err != nil {
					t.Fatal(err)
				}
				loaded, err := c.loadSpec(run)
				if err != nil {
					t.Fatal("historical spec rejected", err)
				}
				if !cleanup && loaded.Actions[0].Performance.ExpectedGuestNodes != nil {
					t.Fatal("invented node identities")
				}
				if err := c.Reconcile(t.Context(), run.ID); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				if err := c.Serve(ctx, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("serve stopped on old journal", err)
				}
				after, err := c.Status(t.Context(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.SpecSHA256 != run.SpecSHA256 || after.BinarySHA256 != run.BinarySHA256 {
					t.Fatal("historical hashes changed")
				}
				if state == Prepared {
					if dispatches == 0 || len(after.Actions[0].Attempts) != 1 {
						t.Fatal("original worker not dispatched exactly one attempt")
					}
					// A promoted executable cannot claim an old binary's attempt.
					if err := c.Work(t.Context(), after.Actions[0].Attempts[0].Token); err == nil || !strings.Contains(err.Error(), "verify executing worker") {
						t.Fatalf("old-worker hash fence lost: %v", err)
					}
				} else if dispatches != 0 || after.State != state || len(after.Actions[0].Attempts) != 1 {
					t.Fatalf("historical measurement replayed or state changed: %+v dispatches=%d", after, dispatches)
				}
				for name, before := range map[string][]byte{"spec.json": data, "worker": worker} {
					got, err := os.ReadFile(filepath.Join(dir, name))
					if err != nil || !bytes.Equal(got, before) {
						t.Fatal("immutable artifact changed", name, err)
					}
				}
				if state == Prepared && !cleanup {
					if err := os.Chmod(filepath.Join(dir, "worker"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "worker"), []byte("substituted worker"), 0700); err != nil {
						t.Fatal(err)
					}
					beforeDispatches := dispatches
					if err := c.Reconcile(t.Context(), run.ID); err == nil || !strings.Contains(err.Error(), "frozen worker SHA-256 mismatch") || dispatches != beforeDispatches {
						t.Fatalf("legacy dispatch bypassed binary hash: %v", err)
					}
					if err := os.Chmod(filepath.Join(dir, "spec.json"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "spec.json"), append(data, '\n'), 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := c.loadSpec(run); err == nil || !strings.Contains(err.Error(), "frozen spec SHA-256 mismatch") {
						t.Fatalf("legacy decode bypassed spec hash: %v", err)
					}
				}
			})
		}
	}
}

func TestNewPerformanceSubmissionRejectsLegacyNodeContract(t *testing.T) {
	c := testController(t, nil)
	spec := legacyPerformanceSpec()
	if _, err := c.Submit(t.Context(), "legacy", spec); err == nil {
		t.Fatal("new legacy measurement admitted")
	}
	if _, err := os.Stat(filepath.Join(c.root, "legacy")); !os.IsNotExist(err) {
		t.Fatal("rejected submission created artifacts")
	}
	for i := range 3 {
		spec.Actions[0].Performance.ExpectedGuestNodes = append(spec.Actions[0].Performance.ExpectedGuestNodes, performance.GuestNodeIdentity{Name: fmt.Sprintf("node-%d", i), UID: fmt.Sprintf("uid-%d", i)})
	}
	if _, err := c.Submit(t.Context(), "explicit", spec); err != nil {
		t.Fatal("explicit measurement rejected", err)
	}
}

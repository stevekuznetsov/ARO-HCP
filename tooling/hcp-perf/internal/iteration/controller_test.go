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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/provision"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

type executorFunc func(context.Context, Action, string, string) error

func (f executorFunc) Execute(ctx context.Context, a Action, dir, resume string) error {
	return f(ctx, a, dir, resume)
}

func testSpec() Spec {
	return Spec{Version: Version, Environment: Environment{ID: "environment", Incarnation: "first"}, Actions: []Action{{
		ID: "inspect", Kind: RPInspect, Recovery: ReadOnly, Timeout: time.Minute, MaxAttempts: 3,
		RP: &RPParameters{Config: provision.Config{Version: "v1", Endpoint: "http://localhost", APIVersion: "2025-12-23-preview", SubscriptionID: "11111111-1111-1111-1111-111111111111", TenantID: "22222222-2222-2222-2222-222222222222", ObjectID: "33333333-3333-3333-3333-333333333333", Principal: "tester"}, Resource: provision.Resource{ResourceGroup: "group", Cluster: "cluster"}},
	}}}
}

func testController(t *testing.T, executor Executor) *Controller {
	t.Helper()
	c, err := NewController(filepath.Join(t.TempDir(), "private"), Options{Executor: executor, ProcessFactory: func(string, string, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func submit(t *testing.T, c *Controller, spec Spec) string {
	t.Helper()
	if _, err := c.Submit(t.Context(), "run", spec); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	return status(t, c).Actions[0].Attempts[0].Token
}

func status(t *testing.T, c *Controller) Run {
	t.Helper()
	run, err := c.Status(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestDispatchBeforeClaimAndDuplicateWorkers(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c := testController(t, executorFunc(func(ctx context.Context, _ Action, _, _ string) error {
		calls.Add(1)
		close(started)
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	token := submit(t, c, testSpec())
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if got := status(t, c).Actions[0].Attempts; len(got) != 1 || got[0].Token != token {
		t.Fatalf("crash before claim changed attempt: %+v", got)
	}
	workerDone := make(chan error, 1)
	go func() { workerDone <- c.Work(t.Context(), token) }()
	<-started
	if err := c.Work(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if got := status(t, c); got.Actions[0].State != Running || len(got.Actions[0].Attempts) != 1 {
		t.Fatalf("live worker not adopted: %+v", got)
	}
	close(finish)
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	if err := c.Work(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || status(t, c).State != Succeeded {
		t.Fatal("duplicate worker executed or completion lost")
	}
}

func TestLostAttemptRecovery(t *testing.T) {
	for _, recovery := range []Recovery{ReadOnly, RemoteMutation, Measurement} {
		t.Run(string(recovery), func(t *testing.T) {
			spec := testSpec()
			switch recovery {
			case RemoteMutation:
				spec.Actions[0].Kind, spec.Actions[0].Recovery, spec.Actions[0].RP.ConfirmDelete = RPDelete, recovery, true
			case Measurement:
				spec.Actions[0] = metricsAction("http://localhost")
			}
			c := testController(t, nil)
			token := submit(t, c, spec)
			if err := c.update(t.Context(), "run", func(run *Run) error {
				run.Actions[0].State, run.Actions[0].Attempts[0].State = Running, Running
				run.Actions[0].Attempts[0].Process = ProcessIdentity{PID: os.Getpid(), BootID: "previous-boot", StartTicks: "1"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := c.Reconcile(t.Context(), "run"); err != nil {
				t.Fatal(err)
			}
			run := status(t, c)
			if recovery == ReadOnly {
				if len(run.Actions[0].Attempts) != 2 || run.Actions[0].Attempts[1].Token == token {
					t.Fatalf("read-only retry missing: %+v", run)
				}
			} else {
				if run.State != Blocked || len(run.Actions[0].Attempts) != 1 {
					t.Fatalf("uncertain action replayed: %+v", run)
				}
				if err := c.Stop(t.Context(), "run"); err != nil {
					t.Fatal(err)
				}
				if err := c.Reconcile(t.Context(), "run"); err != nil {
					t.Fatal(err)
				}
				if status(t, c).State != Blocked {
					t.Fatal("stop erased remote/measurement uncertainty")
				}
				spec.Environment.Incarnation = "second"
				if _, err := c.Submit(t.Context(), "other", spec); err == nil {
					t.Fatal("blocked run released environment claim")
				}
			}
		})
	}
}

func TestStopCancelsWorkerWithoutController(t *testing.T) {
	started := make(chan struct{})
	c := testController(t, executorFunc(func(ctx context.Context, _ Action, _, _ string) error { close(started); <-ctx.Done(); return ctx.Err() }))
	token := submit(t, c, testSpec())
	done := make(chan error, 1)
	go func() { done <- c.Work(t.Context(), token) }()
	<-started
	other, err := Open(c.root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Stop(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not observe persisted stop")
	}
	if status(t, c).State != Stopped {
		t.Fatal("read-only worker did not stop")
	}
}

func TestConcurrentEnvironmentClaim(t *testing.T) {
	c := testController(t, nil)
	other, err := Open(c.root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var successes atomic.Int32
	var group sync.WaitGroup
	for i, controller := range []*Controller{c, other} {
		group.Go(func() {
			if _, err := controller.Submit(t.Context(), fmt.Sprintf("run%d", i), testSpec()); err == nil {
				successes.Add(1)
			}
		})
	}
	group.Wait()
	if successes.Load() != 1 {
		t.Fatalf("concurrent claims succeeded %d times", successes.Load())
	}
}

func TestProcessIdentityFence(t *testing.T) {
	identity, err := processIdentity(os.Getpid())
	if err != nil || !sameProcess(identity) {
		t.Fatalf("identify current process: %v", err)
	}
	identity.StartTicks += "0"
	if sameProcess(identity) {
		t.Fatal("PID reuse passed start-tick fence")
	}
	identity, _ = processIdentity(os.Getpid())
	identity.BootID = "other"
	if sameProcess(identity) {
		t.Fatal("PID reuse passed boot fence")
	}
}

func TestAcceptedMutationResumeAndVerify(t *testing.T) {
	for _, kind := range []Kind{RPCreateCluster, RPDelete} {
		for _, desired := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/desired=%t", kind, desired), func(t *testing.T) {
				var mutations atomic.Int32
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
					exists := (kind == RPCreateCluster) == desired
					if !exists {
						w.WriteHeader(404)
						fmt.Fprint(w, `{"error":{"code":"NotFound"}}`)
						return
					}
					fmt.Fprint(w, `{"properties":{"provisioningState":"Succeeded"}}`)
				}))
				defer server.Close()
				c := testController(t, nil)
				spec := testSpec()
				action := &spec.Actions[0]
				action.Kind, action.Recovery = kind, RemoteMutation
				action.RP.Config.Endpoint, action.RP.ConfirmDelete = server.URL, true
				if kind == RPCreateCluster {
					action.RP.Payload = json.RawMessage(`{}`)
				}
				token := submit(t, c, spec)
				operationURL := server.URL + "/subscriptions/" + action.RP.Config.SubscriptionID + "/providers/Microsoft.RedHatOpenShift/locations/test/hcpOperationStatuses/op"
				client, err := provision.New(action.RP.Config, nil)
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := client.ValidateActionInputs(action.RP.Resource, action.requestMethod(), action.RP.Payload, "")
				if err != nil {
					t.Fatal(err)
				}
				operation, err := client.ValidateActionInputs(action.RP.Resource, http.MethodGet, nil, operationURL)
				if err != nil {
					t.Fatal(err)
				}
				operationURL = operation.OperationURL
				receipt.OperationURL = operationURL
				rpDir := filepath.Join(c.attemptDir("run", token), "rp")
				if err := os.Mkdir(rpDir, 0700); err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal(receipt)
				if err := writePrivate(filepath.Join(rpDir, "operation.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := c.update(t.Context(), "run", func(run *Run) error {
					run.Actions[0].State, run.Actions[0].Attempts[0].State = Running, Running
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := c.Reconcile(t.Context(), "run"); err != nil {
					t.Fatal(err)
				}
				run := status(t, c)
				if len(run.Actions[0].Attempts) != 2 || run.Actions[0].Binding.OperationURL != operationURL {
					t.Fatalf("receipt not adopted: %+v", run)
				}
				if err := c.Work(t.Context(), run.Actions[0].Attempts[1].Token); err != nil {
					t.Fatal(err)
				}
				expected := Blocked
				if desired {
					expected = Succeeded
				}
				if got := status(t, c).State; got != expected {
					t.Fatalf("state=%s expected=%s", got, expected)
				}
				if mutations.Load() != 0 {
					t.Fatal("accepted mutation was replayed")
				}
			})
		}
	}
}

func metricsAction(endpoint string) Action {
	start := time.Unix(1700000000, 0).UTC()
	return Action{ID: "metrics", Kind: MetricsExport, Recovery: Measurement, Timeout: time.Minute, MaxAttempts: 2, Metrics: &MetricsParameters{Endpoint: endpoint, Start: start, End: start, Step: time.Second, Queries: []metrics.Query{{Name: "up", Expr: "up"}}}}
}

func TestValidationAndFrozenHashes(t *testing.T) {
	for _, mutate := range []func(*Spec){
		func(s *Spec) { s.Version = "v2" },
		func(s *Spec) { s.Actions[0].Kind = "shell" },
		func(s *Spec) { s.Actions[0].Recovery = RemoteMutation },
		func(s *Spec) { s.Actions[0].DependsOn = []Dependency{{Action: "inspect", Milestone: Completed}} },
		func(s *Spec) { s.Actions[0].Timeout = 0 },
		func(s *Spec) {
			s.Actions[0].Kind = RPCreateCluster
			s.Actions[0].Recovery = RemoteMutation
			s.Actions[0].RP.Payload = json.RawMessage(`{"bad":`)
		},
	} {
		spec := testSpec()
		mutate(&spec)
		if err := spec.Validate(); err == nil {
			t.Fatalf("accepted malformed spec: %+v", spec)
		}
	}
	for _, filename := range []string{"spec.json", "worker"} {
		t.Run(filename, func(t *testing.T) {
			c := testController(t, nil)
			if _, err := c.Submit(t.Context(), "run", testSpec()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(c.root, "run", filename)
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := c.Reconcile(t.Context(), "run"); err == nil || !strings.Contains(err.Error(), "SHA-256") {
				t.Fatalf("tampering not rejected: %v", err)
			}
		})
	}
}

func TestSizingAndMetricsLibraries(t *testing.T) {
	profile := sizing.Profile{Version: "v1", Name: "small", Anchor: sizing.Budget{CPUMilli: 250, MemoryMiB: 1024}, Rounding: sizing.Budget{CPUMilli: 1, MemoryMiB: 1}, SafetyLimit: sizing.Ratio{Numerator: 3, Denominator: 1}, GoMemLimitPercent: 80, Containers: []sizing.Container{{Identity: sizing.Identity{Kind: "Deployment", Workload: "kube-apiserver", Container: "kube-apiserver"}, CPURatio: sizing.Ratio{Numerator: 1, Denominator: 1}, MemoryRatio: sizing.Ratio{Numerator: 1, Denominator: 1}}}}
	action := Action{ID: "size", Kind: SizingResolve, Recovery: ReadOnly, Timeout: time.Minute, MaxAttempts: 1, Sizing: &SizingParameters{Profile: profile}}
	if err := (libraryExecutor{}).Execute(t.Context(), action, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"__name__":"up"},"values":[[1700000000,"1"]]}]}}`)
	}))
	defer server.Close()
	if err := (libraryExecutor{}).Execute(t.Context(), metricsAction(server.URL), t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
}

func helperFactory(binary, root, token string) error {
	command := exec.Command(binary, "-test.run=^TestDetachedHelper$", "--", root, token)
	command.Env = append(os.Environ(), "ITERATION_TEST_HELPER=worker")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

func TestDetachedHelper(t *testing.T) {
	role := os.Getenv("ITERATION_TEST_HELPER")
	if role == "" {
		return
	}
	root, argument := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	c, err := NewController(root, Options{ProcessFactory: helperFactory})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if role == "worker" {
		if err := c.Work(t.Context(), argument); err != nil {
			t.Fatal(err)
		}
		return
	}
	spec := testSpec()
	spec.Actions = []Action{metricsAction(argument)}
	if _, err := c.Submit(t.Context(), "run", spec); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
}

func TestControllerExitWorkerContinues(t *testing.T) {
	requested, finish := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requested)
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1700000000,"1"]]}]}}`)
	}))
	defer server.Close()
	defer close(finish)
	root := filepath.Join(t.TempDir(), "private")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	controller := exec.Command(binary, "-test.run=^TestDetachedHelper$", "--", root, server.URL)
	controller.Env = append(os.Environ(), "ITERATION_TEST_HELPER=controller")
	if output, err := controller.CombinedOutput(); err != nil {
		t.Fatalf("controller helper: %v: %s", err, output)
	}
	select {
	case <-requested:
	case <-time.After(10 * time.Second):
		t.Fatal("detached worker did not survive controller exit")
	}
	c, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Stop(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if status(t, c).State == Blocked {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("detached measurement worker did not journal stopped measurement uncertainty")
}

func TestRetryBoundAndStopBeforeClaim(t *testing.T) {
	c := testController(t, executorFunc(func(context.Context, Action, string, string) error { return errors.New("must not execute") }))
	token := submit(t, c, testSpec())
	if err := c.Stop(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if err := c.Work(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if status(t, c).State != Stopped {
		t.Fatal("undispatched stop not finalized")
	}
	if _, err := c.Submit(t.Context(), "next", testSpec()); err != nil {
		t.Fatalf("safe stop retained claim: %v", err)
	}
}

func TestAcceptedReceiptWhileWorkerActive(t *testing.T) {
	polling := make(chan struct{})
	var pollOnce sync.Once
	var deletes atomic.Int32
	spec := testSpec()
	operationPath := "/subscriptions/" + spec.Actions[0].RP.Config.SubscriptionID + "/providers/Microsoft.RedHatOpenShift/locations/test/hcpOperationStatuses/op"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			w.Header().Set("Azure-AsyncOperation", operationPath)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"status":"Accepted"}`)
			return
		}
		pollOnce.Do(func() { close(polling) })
		<-r.Context().Done()
	}))
	defer server.Close()
	spec.Actions[0].Kind, spec.Actions[0].Recovery = RPDelete, RemoteMutation
	spec.Actions[0].RP.Config.Endpoint = server.URL
	spec.Actions[0].RP.ConfirmDelete = true
	c := testController(t, nil)
	token := submit(t, c, spec)
	done := make(chan error, 1)
	go func() { done <- c.Work(t.Context(), token) }()
	select {
	case <-polling:
	case <-time.After(5 * time.Second):
		t.Fatal("RP operation was not polled")
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	run := status(t, c)
	if run.Actions[0].Binding.OperationURL == "" || run.Actions[0].Binding.ResourceID == "" || run.Actions[0].State != Running {
		t.Fatalf("active receipt not imported: %+v", run)
	}
	if err := c.Stop(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accepted mutation observer did not stop")
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if status(t, c).State != Blocked || deletes.Load() != 1 {
		t.Fatal("stop canceled remote certainty or replayed DELETE")
	}
}

func TestStopBeforeResumeClaimRetainsUncertainty(t *testing.T) {
	c := testController(t, nil)
	spec := testSpec()
	spec.Actions[0].Kind, spec.Actions[0].Recovery = RPDelete, RemoteMutation
	spec.Actions[0].RP.ConfirmDelete = true
	submit(t, c, spec)
	if err := c.update(t.Context(), "run", func(run *Run) error {
		run.Actions[0].Attempts[0].ResumeURL = "accepted-operation"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if status(t, c).State != Blocked {
		t.Fatal("stopping undispatched resume released uncertain mutation")
	}
}

func TestReadOnlyRetryLimit(t *testing.T) {
	c := testController(t, nil)
	spec := testSpec()
	spec.Actions[0].MaxAttempts = 1
	submit(t, c, spec)
	if err := c.update(t.Context(), "run", func(run *Run) error {
		run.Actions[0].State, run.Actions[0].Attempts[0].State = Running, Running
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if run := status(t, c); run.State != Failed || len(run.Actions[0].Attempts) != 1 {
		t.Fatalf("retry limit ignored: %+v", run)
	}
}

func TestCompletedDependency(t *testing.T) {
	c := testController(t, executorFunc(func(context.Context, Action, string, string) error { return nil }))
	spec := testSpec()
	second := spec.Actions[0]
	second.ID = "second"
	second.DependsOn = []Dependency{{Action: "inspect", Milestone: Completed}}
	spec.Actions = append(spec.Actions, second)
	token := submit(t, c, spec)
	if status(t, c).Actions[1].State != Prepared {
		t.Fatal("completed dependency dispatched early")
	}
	if err := c.Work(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	run := status(t, c)
	if run.Actions[1].State != Dispatched {
		t.Fatal("completed dependency did not dispatch")
	}
	if err := c.Work(t.Context(), run.Actions[1].Attempts[0].Token); err != nil {
		t.Fatal(err)
	}
	if status(t, c).State != Succeeded {
		t.Fatal("dependent lifecycle did not complete")
	}
}

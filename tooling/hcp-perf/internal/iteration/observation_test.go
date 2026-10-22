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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSuppressedObserverDescendants(t *testing.T) {
	spec := Spec{Actions: []Action{{ID: "observe"}, {ID: "create"}, {ID: "credential", DependsOn: []Dependency{{Action: "observe"}, {Action: "create"}}}, {ID: "workers", DependsOn: []Dependency{{Action: "credential"}}}, {ID: "unrelated"}}}
	for _, scenario := range []string{"eligible", "attempted", "operator-stop", "independent-failure", "run-stop", "bound"} {
		t.Run(scenario, func(t *testing.T) {
			run := &Run{Actions: []ActionStatus{{ID: "observe", State: Blocked}, {ID: "create", State: Succeeded}, {ID: "credential", State: Stopped, Message: "initial action suppressed after sibling failure"}, {ID: "workers", State: Stopped, Message: "initial action suppressed after sibling failure"}, {ID: "unrelated", State: Stopped, Message: "initial action suppressed after sibling failure"}}}
			want := []string{"credential", "workers"}
			switch scenario {
			case "attempted":
				run.Actions[2].Attempts = []Attempt{{Token: "attempt"}}
				want = []string{"workers"}
			case "operator-stop":
				run.Actions[2].Message = "operator stopped"
				want = []string{"workers"}
			case "independent-failure":
				run.Actions[1].State = Failed
				want = nil
			case "run-stop":
				run.StopRequested = true
				want = nil
			case "bound":
				run.Actions[2].Binding.OperationURL = "accepted"
				want = []string{"workers"}
			}
			if got := suppressedObserverDescendants(spec, run, "observe"); !reflect.DeepEqual(got, want) {
				t.Fatalf("rearmed %v, want %v", got, want)
			}
		})
	}
}

func TestRetryObservationPreservesRPAndFrozenInputs(t *testing.T) {
	spec := startupIterationSpec()
	const clusterID = "140ce363-e35f-4513-b031-cc7e754685b5"
	const csid = "2sr5e1kh4uueg1t09jdnjuphnrlalkbf"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("observer replacement mutated %s", r.Method)
			w.WriteHeader(500)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/hostedclusters"):
			fmt.Fprintf(w, `{"apiVersion":"hypershift.openshift.io/v1beta1","kind":"HostedClusterList","items":[{"apiVersion":"hypershift.openshift.io/v1beta1","kind":"HostedCluster","metadata":{"namespace":"hc","name":"cluster","uid":"hc-uid","annotations":{"azure.microsoft.com/hcp-cluster-azure-resource-id":%q},"labels":{"api.openshift.com/id":%q}},"spec":{"clusterID":%q}}]}`, spec.Actions[2].Startup.ResourceID, csid, clusterID)
		case strings.HasSuffix(r.URL.Path, "/hostedcontrolplanes"):
			fmt.Fprintf(w, `{"apiVersion":"hypershift.openshift.io/v1beta1","kind":"HostedControlPlaneList","items":[{"apiVersion":"hypershift.openshift.io/v1beta1","kind":"HostedControlPlane","metadata":{"namespace":"cp","name":"plane","uid":"hcp-uid","annotations":{"hypershift.openshift.io/cluster":"hc/cluster"},"labels":{"api.openshift.com/id":%q}},"spec":{"clusterID":%q}}]}`, csid, clusterID)
		default:
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"cp","uid":"namespace-uid"}}`)
		}
	}))
	defer server.Close()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	data := fmt.Sprintf(`{"apiVersion":"v1","kind":"Config","current-context":"test","clusters":[{"name":"cluster","cluster":{"server":%q}}],"contexts":[{"name":"test","context":{"cluster":"cluster","user":"user"}}],"users":[{"name":"user","user":{}}]}`, server.URL)
	if err := os.WriteFile(kubeconfig, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	spec.Actions[1].Candidate.Kubeconfig, spec.Actions[2].Startup.Kubeconfig = kubeconfig, kubeconfig
	calls := 0
	c := testController(t, executorFunc(func(ctx context.Context, action Action, _, _ string) error {
		calls++
		revision, ok := ctx.Value(observationContextKey{}).(*ObservationRevision)
		if !ok || revision.Identity.ClusterID != clusterID || revision.Identity.CSID != csid {
			t.Error("continuation lost separate cluster UUID and CSID")
		}
		if action.Kind != StartupObserve {
			t.Error("replacement executed non-observer")
		}
		return nil
	}))
	if _, err := c.Submit(t.Context(), "run", spec); err != nil {
		t.Fatal(err)
	}
	if err := c.update(t.Context(), "run", func(run *Run) error {
		for i := range run.Actions {
			token := fmt.Sprintf("%032d", i+1)
			if err := os.Mkdir(c.attemptDir("run", token), 0700); err != nil {
				return err
			}
			run.Actions[i].Attempts = []Attempt{{Token: token, State: Running}}
			run.Actions[i].State = Running
		}
		run.Actions[1].State, run.Actions[1].Attempts[0].State = Succeeded, Succeeded
		run.Actions[2].State, run.Actions[2].Attempts[0].State = Blocked, Blocked
		run.Actions[3].Binding.OperationURL = "accepted-rp-operation"
		summarize(run)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := status(t, c)
	var dispatchedBinary, dispatchedToken string
	c.options.ProcessFactory = func(binary, root, token string) error { dispatchedBinary, dispatchedToken = binary, token; return nil }
	if err := c.RetryObservation(t.Context(), "run", "create", "wrong action"); err == nil {
		t.Fatal("RP action accepted for observer replacement")
	}
	if err := c.RetryObservation(t.Context(), "run", "observe", "separate CSID from cluster UUID"); err != nil {
		t.Fatal(err)
	}
	after := status(t, c)
	if after.StopRequested || after.SpecSHA256 != before.SpecSHA256 || after.BinarySHA256 != before.BinarySHA256 {
		t.Fatal("replacement modified run inputs or stopped run")
	}
	if len(after.Actions[3].Attempts) != 1 || after.Actions[3].State != Running || after.Actions[3].Binding.OperationURL != "accepted-rp-operation" {
		t.Fatal("replacement touched accepted RP operation")
	}
	if len(after.Actions[2].Attempts) != 2 || dispatchedBinary != filepath.Join(c.attemptDir("run", dispatchedToken), "worker") {
		t.Fatal("per-attempt worker not frozen/dispatched")
	}
	// Simulate the old RP worker reserializing a Run without new fields.
	if err := c.update(t.Context(), "run", func(run *Run) error {
		run.Actions[2].Attempts[1].Observation = nil
		run.Actions[2].Attempts[1].BinarySHA256 = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if status(t, c).Actions[2].Attempts[1].Observation == nil {
		t.Fatal("old sibling erased immutable revision")
	}
	if err := c.Work(t.Context(), dispatchedToken); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("replacement worker did not execute exactly once")
	}
	manifest, err := os.ReadFile(filepath.Join(c.attemptDir("run", dispatchedToken), "observation-revision.json"))
	if err != nil || !json.Valid(manifest) {
		t.Fatal("replacement audit manifest missing")
	}
}

func TestObserverSignalHelper(t *testing.T) {
	if os.Getenv("ITERATION_OBSERVER_SIGNAL_HELPER") != "1" {
		return
	}
	time.Sleep(time.Minute)
}

func TestObserverPidfdSignalFence(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-test.run=^TestObserverSignalHelper$")
	command.Env = append(os.Environ(), "ITERATION_OBSERVER_SIGNAL_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	identity, err := processIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	wrong := identity
	wrong.StartTicks += "0"
	if err := stopObserverProcess(wrong); err == nil {
		t.Fatal("wrong start ticks permitted signal")
	}
	if !sameProcess(identity) {
		t.Fatal("identity mismatch killed helper")
	}
	if err := stopObserverProcess(identity); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pidfd SIGTERM did not terminate exact helper")
	}
}

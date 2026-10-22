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
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/retention"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

func startupIterationSpec() Spec {
	spec := testSpec()
	create := spec.Actions[0]
	create.ID, create.Kind, create.Recovery, create.Timeout = "create", RPCreateCluster, RemoteMutation, time.Hour
	create.RP.Payload = json.RawMessage(`{}`)
	rid := "/subscriptions/" + create.RP.Config.SubscriptionID + "/resourceGroups/group/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/cluster"
	policy := json.RawMessage(`{"concurrency":{"limit":5,"slidingWindow":"0s"},"transitionDelay":{"increase":"0s","decrease":"20m"},"sizes":[{"name":"small","criteria":{"from":0},"management":{"placeholders":0},"effects":{"maximumRequestsInflight":150,"maximumMutatingRequestsInflight":50,"containerResourcePolicy":{"defaultRequests":{"cpu":"100m","memory":"128Mi"},"memoryLimitMultiplier":2,"goMemoryLimitPercent":80,"cpuLimitEqualsRequest":true,"containers":[]}}}]}`)
	observe := startup.Spec{Kubeconfig: "/external/management.kubeconfig", ResourceID: rid, CandidateName: "cluster", CandidateSpec: policy, MaxDuration: 55 * time.Minute, PollInterval: 15 * time.Second, HealthyWindow: 5 * time.Minute}
	for _, name := range []string{"kube-apiserver", "openshift-apiserver", "packageserver", "etcd"} {
		kind := "Deployment"
		if name == "etcd" {
			kind = "StatefulSet"
		}
		resources := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}}
		observe.Components = append(observe.Components, startup.Component{Kind: kind, Name: name, Replicas: 3, Endpoints: name, ReadyEndpoints: 3, Containers: []startup.Container{{Name: name, Resources: resources}}})
	}
	refresh := retention.NativeSpec{Required: retention.Spec{Version: "v1", ResourceGroups: []string{"/subscriptions/" + create.RP.Config.SubscriptionID + "/resourceGroups/group"}}, TokenFile: "/external/arm-token", Interval: 5 * time.Minute, Duration: 2 * time.Hour}
	spec.Actions = []Action{
		{ID: "retain", Kind: RetentionRefresh, Recovery: GuardedMutation, Timeout: 125 * time.Minute, MaxAttempts: 1, Retention: &refresh},
		{ID: "candidate", Kind: CandidateApply, Recovery: GuardedMutation, Timeout: 5 * time.Minute, MaxAttempts: 1, DependsOn: []Dependency{{Action: "retain", Milestone: Started}}, Candidate: &startup.Candidate{Kubeconfig: observe.Kubeconfig, Name: "cluster", UID: "inventory-required", ResourceVersion: "inventory-required", ExpectedSpec: policy, DesiredSpec: policy}},
		{ID: "observe", Kind: StartupObserve, Recovery: Measurement, Timeout: time.Hour, MaxAttempts: 1, DependsOn: []Dependency{{Action: "candidate", Milestone: Completed}}, Startup: &observe},
	}
	create.DependsOn = []Dependency{{Action: "retain", Milestone: Started}, {Action: "observe", Milestone: Started}}
	spec.Actions = append(spec.Actions, create)
	return spec
}

func TestStartupIterationReadinessAndFailureIsolation(t *testing.T) {
	spec := startupIterationSpec()
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	prepared, arm, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c := testController(t, executorFunc(func(ctx context.Context, action Action, _, _ string) error {
		if action.Kind == RetentionRefresh {
			close(prepared)
			<-arm
			if err := markReady(ctx); err != nil {
				return err
			}
			<-finish
		}
		return nil
	}))
	token := submit(t, c, spec)
	done := make(chan error, 1)
	go func() { done <- c.Work(t.Context(), token) }()
	<-prepared
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if status(t, c).Actions[1].State != Prepared {
		t.Fatal("candidate dispatched before retention was verified")
	}
	close(arm)
	deadline := time.Now().Add(5 * time.Second)
	for !status(t, c).Actions[0].Armed && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := c.Reconcile(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if status(t, c).Actions[1].State != Dispatched {
		t.Fatal("candidate did not dispatch after retention armed")
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	broken := startupIterationSpec()
	broken.Actions[3].DependsOn = nil
	if broken.Validate() == nil {
		t.Fatal("startup create accepted without early observation")
	}
}

func TestAdminCredentialDirectLibrary(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("credential action used %s", r.Method)
		}
		posts++
		fmt.Fprint(w, `{"kubeconfig":"private-kubeconfig"}`)
	}))
	defer server.Close()
	action := testSpec().Actions[0]
	action.Kind, action.Recovery = RPAdminCredential, GuardedMutation
	action.RP.Config.Endpoint = server.URL
	action.RP.CredentialPath = filepath.Join(t.TempDir(), "guest.kubeconfig")
	if err := os.Chmod(filepath.Dir(action.RP.CredentialPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := action.validate(); err != nil {
		t.Fatal(err)
	}
	if err := (libraryExecutor{}).Execute(t.Context(), action, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(action.RP.CredentialPath)
	if err != nil || string(data) != "private-kubeconfig" || posts != 1 {
		t.Fatal("credential library output missing")
	}
	info, err := os.Stat(action.RP.CredentialPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file is not private")
	}
}

func TestStartupExample(t *testing.T) {
	data, err := os.ReadFile("startup-example.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
}

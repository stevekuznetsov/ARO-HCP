// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package startup

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCandidateRejectsFalseIsolationBeforeAPI(t *testing.T) {
	for _, test := range []struct{ name, csc, selector string }{
		{"different CSC", "medium", ""},
		{"hide retained Small", "cluster", "api.openshift.com/name=medium"},
		{"match nothing", "cluster", "!api.openshift.com/id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := Candidate{Kubeconfig: "/external/kubeconfig", Name: test.csc, UID: "csc-uid", ResourceVersion: "7", ExpectedSpec: json.RawMessage(`{"value":"old"}`), DesiredSpec: json.RawMessage(`{"value":"new"}`), EmptyHostedClusterSelector: test.selector}
			client := dynamicClient(candidateObject())
			err := Apply(t.Context(), Clients{Dynamic: client}, candidate)
			var noMutation *NoMutationError
			if !errors.As(err, &noMutation) || len(client.Actions()) != 0 {
				t.Fatalf("false isolation must fail before API access: err=%v actions=%v", err, client.Actions())
			}
		})
	}
}

func TestCandidateRetainedSmallGuardIsUnfiltered(t *testing.T) {
	hc := object("HostedCluster", "retained", "small", "small-uid", map[string]any{})
	hc.SetLabels(map[string]string{"api.openshift.com/name": "small"})
	client := dynamicClient(candidateObject(), hc)
	candidate := Candidate{Kubeconfig: "/external/kubeconfig", Name: "cluster", UID: "csc-uid", ResourceVersion: "7", ExpectedSpec: json.RawMessage(`{"value":"old"}`), DesiredSpec: json.RawMessage(`{"value":"new"}`)}
	var noMutation *NoMutationError
	if err := Apply(t.Context(), Clients{Dynamic: client}, candidate); !errors.As(err, &noMutation) {
		t.Fatalf("retained Small must prevent candidate mutation: %v", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatalf("retained Small caused a write or dry-run: %v", action)
		}
	}
}

func TestStartupRejectsPerHostedClusterCSC(t *testing.T) {
	spec := Spec{Kubeconfig: "/external/kubeconfig", ResourceID: testResourceID, CandidateName: "medium", CandidateSpec: json.RawMessage(`{"value":"new"}`), MaxDuration: time.Hour, PollInterval: 5 * time.Second, HealthyWindow: 5 * time.Minute}
	if err := spec.Validate(); err == nil || err.Error() != "startup requires singleton CSC cluster; per-HostedCluster CSC references are unsupported" {
		t.Fatalf("observer must reject unsupported CSC before inventory validation: %v", err)
	}
}

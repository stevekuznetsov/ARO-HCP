// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apiload

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func TestCheckpointSettlesFrozenCohort(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	stop := errors.New("screen rejected")
	var observed Checkpoint
	spec := Spec{Version: "v1", DurationSeconds: 5, RatePerSecond: 10, MaxInFlight: 10, RequestTimeoutSeconds: 1, Namespaces: []string{"scope"}, Targets: []Target{{Namespace: "scope", APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Name: "app", Weight: 1}}}
	result, err := Run(t.Context(), spec, Options{OutputDir: filepath.Join(t.TempDir(), "api"), Factory: func(string) (*rest.Config, error) {
		return &rest.Config{Host: server.URL, Transport: server.Client().Transport}, nil
	}, Checkpoints: []int{1, 2}, ObserveCheckpoint: func(c Checkpoint) error { observed = c; return stop }})
	if !errors.Is(err, stop) || result.Status != "screening-stopped" || len(result.Checkpoints) != 1 {
		t.Fatalf("early stop: %+v %v", result, err)
	}
	if observed.Offered != 10 || observed.Dispatched != 10 || observed.Completed != 10 || observed.DegradedRequests != 10 || observed.GeneratorLimited != 0 {
		t.Fatalf("pending/later requests diluted cohort: %+v", observed)
	}
	if observed.SettledAt.Sub(result.Started) < 1100*time.Millisecond || result.Finished.Sub(result.Started) > 4*time.Second {
		t.Fatal("did not wait for cohort drain or stop early")
	}
}

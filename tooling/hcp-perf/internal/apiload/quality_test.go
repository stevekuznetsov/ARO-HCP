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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func TestLogicalDegradedUnion(t *testing.T) {
	var counts Counts
	for _, event := range []Event{{StatusCode: 429}, {ErrClass: "timeout"}, {StatusCode: 429, ErrClass: "timeout"}, {StatusCode: 200}, {StatusCode: 403, ErrClass: "timeout"}} {
		counts.observe(event)
	}
	if counts.Completed != 5 || counts.Rejected429 != 2 || counts.Timeouts != 3 || counts.DegradedRequests != 4 || counts.OtherHTTP != 1 || counts.Successful != 1 {
		t.Fatalf("incorrect union: %+v", counts)
	}
}

func Test429BodyTimeoutCountsOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	event := execute(t.Context(), clientTarget{client: server.Client(), url: server.URL}, 30*time.Millisecond, Event{})
	var counts Counts
	counts.observe(event)
	if counts.Rejected429 != 1 || counts.Timeouts != 1 || counts.DegradedRequests != 1 {
		t.Fatalf("timeout after rejection headers: %+v %+v", event, counts)
	}
}

func TestSuccessfulLatencyByOperation(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	spec := Spec{Version: "v1", DurationSeconds: 1, RatePerSecond: 60, MaxInFlight: 10, RequestTimeoutSeconds: 1, Namespaces: []string{"scope"}, Targets: []Target{
		{Namespace: "scope", APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Name: "app-000000", Weight: 1},
		{Namespace: "scope", APIVersion: "v1", Resource: "pods", Verb: "list", Weight: 1},
		{Namespace: "scope", APIVersion: "apps/v1", Resource: "replicasets", Verb: "list", Weight: 1},
	}}
	result, err := Run(t.Context(), spec, Options{OutputDir: filepath.Join(t.TempDir(), "api"), Factory: func(string) (*rest.Config, error) {
		return &rest.Config{Host: server.URL, Transport: server.Client().Transport}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequestAccountingVersion != RequestAccountingVersion || len(result.Operations) != 3 || result.SuccessfulLatencyP99 < 20*time.Millisecond || result.DegradedRequests != result.Rejected429 {
		t.Fatalf("missing useful latency/union: %+v", result)
	}
	for _, operation := range result.Operations {
		if operation.Successful == 0 || operation.SuccessfulLatencyP50 < 20*time.Millisecond || operation.SuccessfulLatencyP95 < 20*time.Millisecond || operation.SuccessfulLatencyP99 < 20*time.Millisecond || operation.SuccessfulBodyQPS <= 0 {
			t.Fatalf("rejections improved successful %s/%s latency: %+v", operation.Resource, operation.Verb, operation)
		}
	}
}

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
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func testSpec() Spec {
	return Spec{Version: "v1", DurationSeconds: 1, RatePerSecond: 20, MaxInFlight: 4, RequestTimeoutSeconds: 1,
		Namespaces: []string{"bench-0"}, Targets: []Target{{Namespace: "bench-0", APIVersion: "v1", Resource: "pods", Verb: "list", Weight: 1}}}
}

func serverConfig(s *httptest.Server) *rest.Config {
	return &rest.Config{Host: s.URL, TLSClientConfig: rest.TLSClientConfig{CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})}}
}

func TestValidate(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"version":              func(s *Spec) { s.Version = "v2" },
		"duration low":         func(s *Spec) { s.DurationSeconds = 0 },
		"duration high":        func(s *Spec) { s.DurationSeconds = 3601 },
		"rate low":             func(s *Spec) { s.RatePerSecond = 0 },
		"rate high":            func(s *Spec) { s.RatePerSecond = 10001 },
		"workers low":          func(s *Spec) { s.MaxInFlight = 0 },
		"workers high":         func(s *Spec) { s.MaxInFlight = 257 },
		"timeout low":          func(s *Spec) { s.RequestTimeoutSeconds = 0 },
		"timeout high":         func(s *Spec) { s.RequestTimeoutSeconds = 31 },
		"attempt cap":          func(s *Spec) { s.DurationSeconds = 3600; s.RatePerSecond = 10000 },
		"namespace traversal":  func(s *Spec) { s.Namespaces[0] = "../x" },
		"duplicate namespaces": func(s *Spec) { s.Namespaces = append(s.Namespaces, "bench-0") },
		"unused namespace":     func(s *Spec) { s.Namespaces = append(s.Namespaces, "bench-1") },
		"namespace limit":      func(s *Spec) { s.Namespaces = make([]string, 1001) },
		"target limit":         func(s *Spec) { s.Targets = make([]Target, 1001) },
		"no targets":           func(s *Spec) { s.Targets = nil },
		"prefix":               func(s *Spec) { s.NamespacePrefix = "other" },
		"invalid prefix":       func(s *Spec) { s.NamespacePrefix = "../" },
		"target namespace":     func(s *Spec) { s.Targets[0].Namespace = "bench-1" },
		"group":                func(s *Spec) { s.Targets[0].APIVersion = "../v1" },
		"resource":             func(s *Spec) { s.Targets[0].Resource = "pods/log" },
		"configmaps":           func(s *Spec) { s.Targets[0].Resource = "configmaps" },
		"write":                func(s *Spec) { s.Targets[0].Verb = "patch" },
		"get pod":              func(s *Spec) { s.Targets[0].Verb = "get"; s.Targets[0].Name = "pod" },
		"list name":            func(s *Spec) { s.Targets[0].Name = "pod" },
		"weight low":           func(s *Spec) { s.Targets[0].Weight = 0 },
		"weight high":          func(s *Spec) { s.Targets[0].Weight = MaxAttempts + 1 },
		"limit low":            func(s *Spec) { n := int64(-1); s.Targets[0].Limit = &n },
		"limit high":           func(s *Spec) { n := int64(10001); s.Targets[0].Limit = &n },
		"get no name": func(s *Spec) {
			s.Targets[0] = Target{Namespace: "bench-0", APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Weight: 1}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := testSpec()
			mutate(&s)
			if _, err := s.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	s, err := testSpec().Validate()
	if err != nil || *s.Seed != 42 || *s.Targets[0].Limit != 100 {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	zero := int64(0)
	s.Seed, s.Targets[0].Limit = &zero, &zero
	s, err = s.Validate()
	if err != nil || *s.Seed != 0 || *s.Targets[0].Limit != 0 {
		t.Fatal("explicit zero lost", err)
	}
	base, _ := url.Parse("https://example.test/prefix")
	if got := s.Targets[0].requestURL(base); got != "https://example.test/prefix/api/v1/namespaces/bench-0/pods?limit=0" {
		t.Fatal(got)
	}
}

func TestWeightedSchedule(t *testing.T) {
	targets := []Target{{Weight: 1}, {Weight: 3}, {Weight: 2}}
	a, b := rand.New(rand.NewSource(42)), rand.New(rand.NewSource(42))
	counts := [3]int{}
	for range 6000 {
		i := chooseTarget(a, targets, 6)
		if i != chooseTarget(b, targets, 6) {
			t.Fatal("not deterministic")
		}
		counts[i]++
	}
	if counts[0] < 850 || counts[0] > 1150 || counts[1] < 2800 || counts[1] > 3200 {
		t.Fatal(counts)
	}
	if quantile([]int{0, 1, 8, 1}, 10, 95) != 3*time.Millisecond {
		t.Fatal("bad percentile")
	}
}

func TestHTTPOutcomes(t *testing.T) {
	for _, code := range []int{200, 422, 429, 302} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/bench-0/pods" || r.URL.RawQuery != "limit=100" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				w.Header().Set("X-Kubernetes-PF-FlowSchema-UID", "12345678-1234-1234-1234-123456789abc")
				w.Header().Set("X-Kubernetes-PF-PriorityLevel-UID", "not-a-uid-secret")
				w.Header().Set("Retry-After", "3")
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "secret-body")
			}))
			defer server.Close()
			dir := filepath.Join(t.TempDir(), "output")
			r, err := Run(context.Background(), testSpec(), Options{OutputDir: dir, Factory: func(string) (*rest.Config, error) { return serverConfig(server), nil }})
			if err != nil {
				t.Fatal(err)
			}
			if r.Pacing.MissedDeadline+r.Pacing.SlotsFull+r.Pacing.CanceledOffers != r.GeneratorLimited || r.Pacing.MaxSlotsUsed > r.Spec.MaxInFlight {
				t.Fatalf("pacing diagnostics disagree with accounting: %+v", r)
			}
			if r.Offered != 20 || r.Offered != r.Dispatched+r.GeneratorLimited || r.Completed != r.Dispatched || r.HTTPAttempts != int(calls.Load()) || r.HTTPAttempts != r.Completed || r.Completed == 0 {
				t.Fatalf("bad counts: %+v", r)
			}
			if code == 200 && r.Successful != r.Completed || code == 429 && r.Rejected429 != r.Completed || (code == 422 || code == 302) && r.OtherHTTP != r.Completed {
				t.Fatalf("bad outcome: %+v", r.Counts)
			}
			data, err := os.ReadFile(filepath.Join(dir, "requests.ndjson"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "secret") || strings.Contains(string(data), server.URL) {
				t.Fatal("sensitive data logged")
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != r.Completed {
				t.Fatal("records lost")
			}
			var e Event
			if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
				t.Fatal(err)
			}
			if e.StatusCode != code || e.APFFlowSchemaUID == "" || e.APFPriorityLevelUID != "" || e.RetryAfter != "3" || e.ResponseBytes != 11 || e.Latency < e.HeaderLatency || e.Enqueued.IsZero() || e.QueueDelay < 0 {
				t.Fatalf("bad event: %+v", e)
			}
			for _, path := range []string{dir, filepath.Join(dir, "requests.ndjson"), filepath.Join(dir, "result.json"), filepath.Join(dir, "spec.json")} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm()&0077 != 0 {
					t.Fatal("not private", path, err)
				}
			}
			var saved Result
			data, _ = os.ReadFile(filepath.Join(dir, "result.json"))
			if err := json.Unmarshal(data, &saved); err != nil || !reflect.DeepEqual(saved.Counts, r.Counts) {
				t.Fatal("final snapshot mismatch", err)
			}
			if _, err := Run(context.Background(), testSpec(), Options{OutputDir: dir, Factory: func(string) (*rest.Config, error) { t.Fatal("factory called for existing output"); return nil, nil }}); err == nil {
				t.Fatal("overwrote existing evidence")
			}
		})
	}
}

func TestSaturationAndCancellation(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[cancelRun], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{}, 1)
			var active atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				active.Add(1)
				defer active.Add(-1)
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				entered <- struct{}{}
				<-r.Context().Done()
			}))
			defer server.Close()
			spec := testSpec()
			spec.RatePerSecond, spec.MaxInFlight = 100, 1
			if cancelRun {
				go func() { <-entered; time.Sleep(50 * time.Millisecond); cancel() }()
			}
			dir := filepath.Join(t.TempDir(), "output")
			r, err := Run(ctx, spec, Options{OutputDir: dir, Factory: func(string) (*rest.Config, error) { return serverConfig(server), nil }})
			if cancelRun && !errors.Is(err, context.Canceled) || !cancelRun && err != nil {
				t.Fatal(err)
			}
			if r.Dispatched != 1 || r.Completed != 1 || r.GeneratorLimited == 0 || r.Offered != r.Dispatched+r.GeneratorLimited || r.Successful != 0 {
				t.Fatalf("saturation hidden: %+v", r)
			}
			if !cancelRun && (r.Offered != 100 || r.Timeouts != 1) || cancelRun && (r.Canceled != 1 || r.Status != "canceled" || r.Offered >= 100) {
				t.Fatalf("wrong stop result: %+v", r)
			}
			data, _ := os.ReadFile(filepath.Join(dir, "requests.ndjson"))
			var e Event
			if err := json.Unmarshal(data, &e); err != nil || e.Latency <= e.HeaderLatency {
				t.Fatal("body latency not measured", err)
			}
			// Run joins its workers and flushes even when the caller is canceled.
			if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBodyBoundAndNamespaceClients(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Split(r.URL.Path, "/")[5] {
			t.Errorf("wrong namespace credentials: %s", r.URL.Path)
		}
		_, _ = io.CopyN(w, &repeatingReader{}, MaxResponseBytes+1024)
	}))
	defer server.Close()
	spec := testSpec()
	spec.RatePerSecond = 2
	spec.Namespaces = []string{"bench-0", "bench-1"}
	spec.Targets = []Target{
		{Namespace: "bench-0", APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Name: "app-000000", Weight: 1},
		{Namespace: "bench-1", APIVersion: "apps/v1", Resource: "replicasets", Verb: "list", Weight: 1},
	}
	seen := map[string]bool{}
	r, err := Run(context.Background(), spec, Options{OutputDir: filepath.Join(t.TempDir(), "output"), Factory: func(ns string) (*rest.Config, error) {
		seen[ns] = true
		c := serverConfig(server)
		c.BearerToken = ns
		return c, nil
	}})
	if err != nil || len(seen) != 2 || r.BodyLimitErrors != r.Completed || r.Completed == 0 || r.Successful != 0 {
		t.Fatalf("body bound: %+v %v", r, err)
	}
}

type repeatingReader struct{}

func (*repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestRejectUnsafeTransport(t *testing.T) {
	for _, config := range []*rest.Config{
		{Host: "http://localhost"}, {Host: "https://token@localhost"}, {Host: "https://localhost?token=secret"},
		{Host: "https://localhost#secret"}, {Host: "https://localhost", TLSClientConfig: rest.TLSClientConfig{Insecure: true}},
	} {
		_, err := Run(context.Background(), testSpec(), Options{OutputDir: filepath.Join(t.TempDir(), "output"), Factory: func(string) (*rest.Config, error) { return config, nil }})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe transport accepted or leaked", err)
		}
	}
}

func TestTransportAndHeaderTimeout(t *testing.T) {
	for _, kind := range []string{"timeout", "transport", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "timeout" {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, "short")
			}))
			defer server.Close()
			client, err := rest.HTTPClientFor(serverConfig(server))
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			if kind == "transport" {
				server.Close()
			}
			e := execute(context.Background(), clientTarget{client: client, url: server.URL}, 50*time.Millisecond, Event{})
			want := kind
			if kind == "truncated" {
				want = "transport"
			}
			if e.ErrClass != want {
				t.Fatalf("wrong error class: %+v", e)
			}
			var counts Counts
			counts.observe(e)
			if counts.Successful != 0 || counts.Completed != 1 {
				t.Fatalf("incorrect failure accounting: %+v", counts)
			}
			if kind == "transport" && e.HTTPAttempts != 0 {
				t.Fatal("failed dial counted as fully written HTTP attempt")
			}
		})
	}
}

func TestHTTP2KeepAlive(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("expected HTTP/2, got %s", r.Proto)
		}
		_, _ = io.WriteString(w, "{}")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client, err := rest.HTTPClientFor(serverConfig(server))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	for range 2 {
		e := execute(context.Background(), clientTarget{client: client, url: server.URL}, time.Second, Event{})
		if e.ErrClass != "" || e.HTTPAttempts != 1 || e.StatusCode != 200 {
			t.Fatalf("HTTP/2 request failed: %+v", e)
		}
	}
}

func TestMaxInFlightConcurrent(t *testing.T) {
	t.Parallel()
	var active, peak atomic.Int64
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			_, _ = io.WriteString(w, "{}")
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	spec := testSpec()
	spec.RatePerSecond, spec.MaxInFlight, spec.RequestTimeoutSeconds = 100, 4, 5
	dir := filepath.Join(t.TempDir(), "output")
	var result Result
	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, runErr = Run(ctx, spec, Options{OutputDir: dir, Factory: func(string) (*rest.Config, error) { return serverConfig(server), nil }})
	}()
	defer func() { cancel(); <-done }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for range 4 {
		select {
		case <-entered:
		case <-deadline.C:
			t.Fatal("four requests did not enter concurrently")
		}
	}
	// Hold all four slots beyond the offered window: later offers must drop,
	// rather than queue or start extra requests. No request timeout is needed.
	time.Sleep(1100 * time.Millisecond)
	if peak.Load() != 4 || active.Load() != 4 {
		t.Fatalf("concurrency peak=%d active=%d", peak.Load(), active.Load())
	}
	close(release)
	select {
	case <-done:
	case <-deadline.C:
		t.Fatal("workers did not drain after release")
	}
	if runErr != nil || result.Dispatched != 4 || result.Successful != 4 || result.Offered != 100 || result.GeneratorLimited != 96 || result.Completed != 4 || peak.Load() != 4 {
		t.Fatalf("incorrect bounded concurrency: %+v peak=%d err=%v", result.Counts, peak.Load(), runErr)
	}
}

func TestPeriodicFlushWhileRunning(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	blocked := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "{}")
			return
		}
		select {
		case blocked <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	spec := testSpec()
	spec.DurationSeconds, spec.MaxInFlight, spec.RequestTimeoutSeconds = 10, 1, 5
	dir := filepath.Join(t.TempDir(), "output")
	var result Result
	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, runErr = Run(ctx, spec, Options{OutputDir: dir, Factory: func(string) (*rest.Config, error) { return serverConfig(server), nil }})
	}()
	defer func() { cancel(); <-done }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	select {
	case <-blocked:
	case <-deadline.C:
		t.Fatal("second request did not block")
	}
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-done:
			t.Fatal("run ended before periodic flush", runErr)
		case <-deadline.C:
			t.Fatal("completed request was not flushed while another request was blocked")
		case <-poll.C:
		}
		data, err := os.ReadFile(filepath.Join(dir, "requests.ndjson"))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(data, &event); err != nil || event.Sequence != 1 || event.StatusCode != 200 || event.ErrClass != "" {
			t.Fatal("invalid periodically flushed event", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "result.json")); !os.IsNotExist(err) {
			t.Fatal("run already finalized", err)
		}
		break
	}
	cancel()
	select {
	case <-done:
	case <-deadline.C:
		t.Fatal("canceled run did not join workers")
	}
	if !errors.Is(runErr, context.Canceled) || result.Successful != 1 || result.Canceled != 1 || result.Completed != 2 {
		t.Fatalf("incorrect final flush result: %+v %v", result.Counts, runErr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests.ndjson"))
	if err != nil || len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 2 {
		t.Fatal("final flush lost requests", err)
	}
}

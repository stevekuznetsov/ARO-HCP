// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package requesttrace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const testUID = "f8092353-d705-4852-97fc-65cbb819ade4"
const testPath = "/api/v1/namespaces/bench/pods"

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newLog(t *testing.T) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "requests.ndjson")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

func observations(t *testing.T, path string) []Observation {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"credential", "private", "Authorization", "Cookie", "RawQuery", "http://", "https://"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("log contains forbidden field/content %q: %s", forbidden, data)
		}
	}
	var records []Observation
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var o Observation
		if err := decoder.Decode(&o); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		records = append(records, o)
	}
	return records
}

func TestHTTPRetriesAndWrapperComposition(t *testing.T) {
	l, path := newLog(t)
	var calls, wrapped atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer credential" || r.Header.Get("Existing-Wrapper") != "private" {
			t.Error("authentication or existing wrapper was lost")
		}
		if r.Header.Get("Audit-ID") != "" || r.Header.Get("X-Client-Request-ID") != "" {
			t.Error("observer injected a request identity")
		}
		w.Header().Set("X-Kubernetes-PF-FlowSchema-UID", testUID)
		w.Header().Set("X-Kubernetes-PF-PriorityLevel-UID", testUID)
		w.Header().Set("Audit-ID", testUID)
		w.Header().Set("Private-Header", "credential")
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"private","reason":"TooManyRequests","code":429}`)
			return
		}
		w.Header().Set("Retry-After", "Wed, 21 Oct 2015 07:28:00 GMT")
		_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
	}))
	defer server.Close()
	config := &rest.Config{Host: server.URL, BearerToken: "credential", TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
	config.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return roundTripper(func(r *http.Request) (*http.Response, error) {
			wrapped.Add(1)
			r = r.Clone(r.Context())
			r.Header.Set("Existing-Wrapper", "private")
			return next.RoundTrip(r)
		})
	})
	config.Wrap(l.WrapTransport("bench"))
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods("bench").List(t.Context(), metav1.ListOptions{LabelSelector: "private=credential"}); err != nil {
		t.Fatal(err)
	}
	// Each record is already on disk before Close, including client-go's retry.
	records := observations(t, path)
	if calls.Load() != 2 || wrapped.Load() != 2 || len(records) != 2 {
		t.Fatalf("requests=%d wrappers=%d records=%+v", calls.Load(), wrapped.Load(), records)
	}
	for i, o := range records {
		if o.Version != "v1" || o.Namespace != "bench" || o.Method != "GET" || o.Path != testPath || o.Kind != "headerReceived" || o.ErrClass != "" || o.Started.IsZero() || o.Duration <= 0 || o.APFFlowSchemaUID != testUID || o.APFPriorityLevelUID != testUID || o.AuditID != testUID {
			t.Fatalf("incomplete observation: %+v", o)
		}
		if i == 0 && (o.StatusCode != 429 || o.RetryAfter != "0") || i == 1 && (o.StatusCode != 200 || o.RetryAfter != "Wed, 21 Oct 2015 07:28:00 GMT") {
			t.Fatalf("lost HTTP outcome: %+v", o)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

type untouchedBody struct{ read, closed bool }

func (b *untouchedBody) Read([]byte) (int, error) { b.read = true; return 0, io.EOF }
func (b *untouchedBody) Close() error             { b.closed = true; return nil }

func TestBodiesUntouched(t *testing.T) {
	l, path := newLog(t)
	requestBody, responseBody := &untouchedBody{}, &untouchedBody{}
	req, _ := http.NewRequest("GET", "https://private"+testPath+"?watch=true&credential=private", requestBody)
	req.Header.Set("Authorization", "credential")
	req.Header.Set("Cookie", "credential")
	response := &http.Response{StatusCode: 200, Body: responseBody, Header: http.Header{}}
	tr := l.WrapTransport("bench")(roundTripper(func(r *http.Request) (*http.Response, error) {
		if r != req {
			t.Error("observer changed request")
		}
		return response, nil
	}))
	got, err := tr.RoundTrip(req)
	if got != response || err != nil || got.Body != responseBody || requestBody.read || requestBody.closed || responseBody.read || responseBody.closed {
		t.Fatal("observer touched or replaced a body/response")
	}
	if records := observations(t, path); len(records) != 1 || records[0].Kind != "headerReceived" || !records[0].Watch {
		t.Fatalf("watch did not record header receipt immediately: %+v", records)
	}
	if err := l.Close(); err != nil || responseBody.closed {
		t.Fatal("log close must not close response bodies", err)
	}
}

func TestLiveWatchHeaders(t *testing.T) {
	l, path := newLog(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+testPath+"?watch=true", nil)
	response, err := l.WrapTransport("bench")(http.DefaultTransport).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if records := observations(t, path); len(records) != 1 || records[0].Kind != "headerReceived" || !records[0].Watch {
		t.Fatalf("watch headers not recorded before body completion: %+v", records)
	}
	cancel()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTransportErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"canceled", fmt.Errorf("private credential: %w", context.Canceled)},
		{"timeout", fmt.Errorf("private credential: %w", context.DeadlineExceeded)},
		{"timeout", &timeoutError{}},
		{"transport", errors.New("private URL and credential")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, path := newLog(t)
			req, _ := http.NewRequest("GET", "https://private"+testPath+"?credential=private", nil)
			tr := l.WrapTransport("bench")(roundTripper(func(*http.Request) (*http.Response, error) { return nil, tc.err }))
			if response, err := tr.RoundTrip(req); response != nil || err != tc.err {
				t.Fatal("observer changed transport outcome")
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			records := observations(t, path)
			if len(records) != 1 || records[0].ErrClass != tc.name || records[0].StatusCode != 0 || records[0].Kind != "transportError" {
				t.Fatalf("error classification: %+v", records)
			}
			data, _ := os.ReadFile(path)
			if bytes.Contains(data, []byte("statusCode")) {
				t.Fatal("absent status should be omitted")
			}
		})
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "private timeout credential" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

func TestHTTPTimeoutAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	for _, canceled := range []bool{false, true} {
		l, path := newLog(t)
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		if canceled {
			cancel()
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+testPath, nil)
		_, err := l.WrapTransport("bench")(http.DefaultTransport).RoundTrip(req)
		cancel()
		if err == nil {
			t.Fatal("expected HTTP context error")
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		want := "timeout"
		if canceled {
			want = "canceled"
		}
		if records := observations(t, path); len(records) != 1 || records[0].ErrClass != want {
			t.Fatalf("HTTP error: %+v", records)
		}
	}
}

func TestAllowlist(t *testing.T) {
	l, path := newLog(t)
	tr := l.WrapTransport("bench")(roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	}))
	for _, p := range []string{
		"/api", "/apis", "/api/v1/namespaces/bench/secrets", "/api/v1/namespaces/bench/secrets/credential",
		"/api/v1/namespaces/bench/serviceaccounts/bench/token", testPath + "/pod/log", testPath + "/pod/exec",
		testPath + "/pod/proxy/private", testPath + "/pod/status", testPath + "/pod/scale", testPath + "/../private",
		testPath + "/%0Aprivate", testPath + "/pod%2Fprivate", testPath + "/", "/api/v1/namespaces/other/pods",
		"/apis/apps/v1/namespaces/bench/deployments/bench/status", "/metrics/private", "/api/v1/pods",
	} {
		req, _ := http.NewRequest("GET", "https://private"+p+"?credential=private", nil)
		if _, err := tr.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
	}
	if records := observations(t, path); len(records) != 0 {
		t.Fatalf("disallowed paths recorded: %+v", records)
	}
	paths := []string{testPath, testPath + "/bench.0", "/metrics", "/apis/apps/v1/namespaces/bench/deployments", "/apis/apps/v1/namespaces/bench/deployments/bench/scale", "/apis/apps/v1/namespaces/bench/replicasets/bench"}
	for _, p := range paths {
		req, _ := http.NewRequest("GET", "https://private"+p+"?credential=private", nil)
		_, _ = tr.RoundTrip(req)
	}
	records := observations(t, path)
	if len(records) != len(paths) {
		t.Fatalf("allowed paths missing: %+v", records)
	}
	for i, o := range records {
		if o.Path != paths[i] {
			t.Fatalf("path changed or contains query: %+v", o)
		}
	}
}

func TestHeaderValidation(t *testing.T) {
	for _, raw := range []string{"0", "120", "4294967295", "Wed, 21 Oct 2015 07:28:00 GMT"} {
		if got := validRetryAfter(raw); got != raw {
			t.Errorf("valid retry-after %q became %q", raw, got)
		}
	}
	for _, raw := range []string{"-1", "+1", "4294967296", "1.2", "credential", "12\nprivate", strings.Repeat("0", 65), "Wed, 99 Oct 2015 07:28:00 GMT"} {
		if got := validRetryAfter(raw); got != "" {
			t.Errorf("unsafe retry-after %q retained", got)
		}
	}
	for _, raw := range []string{"credential", testUID + "\n", "urn:uuid:" + testUID, strings.Repeat("f", 10000)} {
		if got := validUID(raw); got != "" {
			t.Errorf("unsafe UID retained: %q", got)
		}
	}
	if validUID(testUID) != testUID {
		t.Fatal("valid UID rejected")
	}
}

func TestConcurrentCallsAndClose(t *testing.T) {
	l, path := newLog(t)
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			ns := fmt.Sprintf("bench-%d", i%2)
			tr := l.WrapTransport(ns)(roundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
			}))
			req, _ := http.NewRequest("GET", "https://private/api/v1/namespaces/"+ns+"/pods", nil)
			_, _ = tr.RoundTrip(req)
		})
	}
	wg.Wait()
	for range 10 {
		wg.Go(func() {
			if err := l.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if records := observations(t, path); len(records) != 100 {
		t.Fatalf("lost concurrent records: %d", len(records))
	} else {
		for _, o := range records {
			if !strings.Contains(o.Path, "/"+o.Namespace+"/") {
				t.Fatalf("client scopes mixed: %+v", o)
			}
		}
	}
}

func TestFileSafetyAndFailures(t *testing.T) {
	l, path := newLog(t)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("log must be private", info, err)
	}
	if _, err := Open(path); !errors.Is(err, ErrIncomplete) {
		t.Fatal("existing log was not rejected", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); !errors.Is(err, ErrIncomplete) {
		t.Fatal("symlink was not rejected", err)
	}
	for _, failure := range []string{"cap", "write", "close", "late"} {
		t.Run(failure, func(t *testing.T) {
			l, path := newLog(t)
			switch failure {
			case "cap":
				l.size = MaxBytes
			case "write", "close":
				_ = l.file.Close()
			case "late":
				_ = l.Close()
			}
			if failure != "close" {
				req, _ := http.NewRequest("GET", "https://private"+testPath, nil)
				tr := l.WrapTransport("bench")(roundTripper(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
				}))
				if resp, err := tr.RoundTrip(req); err != nil || resp.StatusCode != 200 {
					t.Fatal("recording failure changed HTTP outcome", err)
				}
			}
			if err := l.Close(); !errors.Is(err, ErrIncomplete) {
				t.Fatal("evidence failure not identified", err)
			}
			if records := observations(t, path); len(records) != 0 {
				t.Fatal("failed write unexpectedly recorded", records)
			}
		})
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	disabled, err := Open("")
	next := http.DefaultTransport
	if err != nil || disabled != nil || disabled.Close() != nil || disabled.WrapTransport("bench")(next) != next {
		t.Fatal("disabled tracing changed behavior")
	}
}

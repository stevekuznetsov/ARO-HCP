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

package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

func TestLogRequestBudgetValidation(t *testing.T) {
	for _, tc := range []struct {
		qps, burst int64
		valid      bool
	}{{0, 0, true}, {1, 1, true}, {100, 200, true}, {20, 0, true}, {0, 20, true},
		{-1, 40, false}, {101, 200, false}, {20, -1, false}, {20, 201, false}, {20, 19, false}, {100, 0, false}} {
		t.Run(fmt.Sprintf("%d/%d", tc.qps, tc.burst), func(t *testing.T) {
			spec := testSpec(t)
			spec.LogRequestQPS, spec.LogRequestBurst = tc.qps, tc.burst
			if err := spec.Validate(); (err == nil) != tc.valid {
				t.Fatalf("validation = %v, valid=%t", err, tc.valid)
			}
			session := sessionSpec()
			session.LogRequestQPS, session.LogRequestBurst = tc.qps, tc.burst
			if err := session.Validate(); (err == nil) != tc.valid {
				t.Fatalf("session validation = %v, valid=%t", err, tc.valid)
			}
			if !tc.valid {
				c := sessionClient()
				if _, err := Start(t.Context(), c, session); err == nil || len(c.Actions()) != 0 {
					t.Fatal("invalid budget reached API")
				}
			}
		})
	}
	for _, value := range []string{"1.5", "NaN", "1e100"} {
		var spec Spec
		if err := json.Unmarshal([]byte(`{"logRequestQPS":`+value+`}`), &spec); err == nil {
			t.Fatalf("non-integer budget %q accepted", value)
		}
	}
}

func TestClientLimiterClassificationDoesNotTrustServerMessages(t *testing.T) {
	message := "client rate limiter Wait returned an error: private-payload"
	for _, tc := range []struct {
		err    error
		reason string
		code   int32
	}{
		{fmt.Errorf("%s: %w", message, context.DeadlineExceeded), "clientRateLimited", 0},
		{apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "p", errors.New(message)), "forbidden", 403},
		{apierrors.NewTooManyRequests(message, 0), "tooManyRequests", 429},
	} {
		got := classifyError(tc.err)
		if got.Reason != tc.reason || got.Code != tc.code {
			t.Fatalf("classification = %+v, want %s/%d", got, tc.reason, tc.code)
		}
		data, err := json.Marshal(got)
		if err != nil || strings.Contains(string(data), "private-payload") {
			t.Fatal("raw error leaked")
		}
	}
}

func TestClientsAggregateBudget(t *testing.T) {
	spec := testSpec(t)
	synctest.Test(t, func(t *testing.T) {
		inherited := flowcontrol.NewFakeAlwaysRateLimiter()
		config := &rest.Config{Host: "https://unused.invalid", QPS: 5, Burst: 10, Timeout: time.Second, RateLimiter: inherited}
		observation, logs, err := NewClients(config, spec)
		if err != nil {
			t.Fatal(err)
		}
		ol, ll := observation.CoreV1().RESTClient().GetRateLimiter(), logs.CoreV1().RESTClient().GetRateLimiter()
		if ol == ll || ol == inherited || ll == inherited || ol.QPS() != 10 || ll.QPS() != 20 ||
			observation.AppsV1().RESTClient().GetRateLimiter() != ol || logs.AppsV1().RESTClient().GetRateLimiter() != ll {
			t.Fatal("clientsets do not have separate aggregate budgets")
		}
		if config.QPS != 5 || config.Burst != 10 || config.Timeout != time.Second || config.RateLimiter != inherited {
			t.Fatal("caller config was mutated")
		}
		start := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		for range 215 {
			wg.Go(func() {
				// Every follower verifies UID then opens logs on the same bucket.
				for range 2 {
					if err := ll.Wait(ctx); err != nil {
						t.Errorf("startup reservation failed: %v", err)
					}
				}
			})
		}
		wg.Wait()
		if elapsed := time.Since(start); elapsed != 19500*time.Millisecond {
			t.Fatalf("430 requests must consume burst40 + 19.5s at 20 QPS, got %v", elapsed)
		}
		for range 20 {
			if !ol.TryAccept() {
				t.Fatal("log startup starved observation burst")
			}
		}
		if ol.TryAccept() || ll.TryAccept() {
			t.Fatal("aggregate burst exceeded")
		}
	})
}

func TestRecordHTTP215LogFollowers(t *testing.T) {
	t.Parallel()
	const followers = 215
	const linesPerFollower = 1000
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	catchup := strings.Repeat(stamp+" catchup\n", linesPerFollower-1) + stamp + " capture-marker\n"
	pods := corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}}
	for i := range followers {
		p := logTestPod()
		p.Name, p.UID = fmt.Sprintf("p%d", i), types.UID(fmt.Sprintf("uid%d", i))
		p.Status.ContainerStatuses[0].RestartCount = 0
		pods.Items = append(pods.Items, *p)
	}
	var opened, closed, gets, watches atomic.Int32
	allOpened := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("observer attempted a write")
		}
		if r.URL.Query().Get("watch") == "true" {
			w.Header().Set("Content-Type", "application/json")
			w.(http.Flusher).Flush()
			watches.Add(1)
			<-r.Context().Done()
			return
		}
		if strings.HasSuffix(r.URL.Path, "/log") {
			w.Header().Set("Content-Type", "text/plain")
			w.(http.Flusher).Flush()
			if opened.Add(1) == followers {
				close(allOpened)
			}
			if _, err := fmt.Fprint(w, catchup); err != nil {
				t.Errorf("sending catchup: %v", err)
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			closed.Add(1)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/pods/") {
			gets.Add(1)
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/ns/pods/")
			for _, p := range pods.Items {
				if p.Name == name {
					_ = json.NewEncoder(w).Encode(p)
					return
				}
			}
			t.Error("unexpected pod identity")
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/pods") {
			_ = json.NewEncoder(w).Encode(pods)
			return
		}
		kind, version := "", "v1"
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			kind = "EventList"
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			kind, version = "DeploymentList", "apps/v1"
		case strings.HasSuffix(r.URL.Path, "/statefulsets"):
			kind, version = "StatefulSetList", "apps/v1"
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]string{"resourceVersion": "1"}, "items": []any{}})
	}))
	defer server.Close()
	spec := testSpec(t)
	spec.Namespaces, spec.DurationSeconds = []string{"ns"}, 30
	spec.EnableLogs, spec.RawLogs, spec.EnableWatch = true, true, true
	// Real HTTP and the unchanged default 20/40 limiter, including catchup writes.
	observation, logs, err := NewClients(&rest.Config{Host: server.URL, Timeout: time.Millisecond}, spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 28*time.Second)
	defer cancel()
	done := make(chan struct{})
	var result Result
	var recordErr error
	go func() {
		result, recordErr = RecordWithLogClient(ctx, observation, logs, spec)
		close(done)
	}()
	select {
	case <-allOpened:
	case <-ctx.Done():
		t.Errorf("only %d/%d log streams initialized", opened.Load(), followers)
	}
	// Every timestamped last-line marker must become visible before cancellation,
	// not merely reach the HTTP transport or be written by the final flush.
	started := time.Now()
	markers := 0
	for ctx.Err() == nil {
		data, err := os.ReadFile(spec.OutputDir + "/logs.ndjson")
		if err != nil {
			t.Error(err)
			break
		}
		markers = strings.Count(string(data), `"body":"capture-marker"`)
		if markers == followers {
			break
		}
		if !streamPause(ctx, 100*time.Millisecond) {
			break
		}
	}
	if markers != followers {
		t.Errorf("catchup incomplete before cancellation: markers=%d/%d", markers, followers)
	}
	t.Logf("%d timestamped lines persisted across %d real HTTP followers at 20/40; catchup after last open=%s", followers*linesPerFollower, followers, time.Since(started))
	cancel()
	<-done
	server.Close()
	if !errors.Is(recordErr, context.Canceled) || result.APIErrors != 0 || result.Samples < 4 || result.Live == nil {
		t.Fatalf("polling failed alongside logs: %+v %v", result, recordErr)
	}
	if result.Live.Logs.Errors != 0 || result.Live.Logs.Gaps != 0 || result.Live.Watch.Initialized != 4 || result.Live.Watch.Errors != 0 ||
		opened.Load() != followers || closed.Load() != followers || gets.Load() != followers || watches.Load() != 4 {
		t.Fatalf("startup/cleanup failed: live=%+v opens=%d closes=%d gets=%d watches=%d", result.Live, opened.Load(), closed.Load(), gets.Load(), watches.Load())
	}
	manifest := readJSON[struct {
		Spec Spec `json:"spec"`
	}](t, spec.OutputDir+"/manifest.json")
	if manifest.Spec.LogRequestQPS != 20 || manifest.Spec.LogRequestBurst != 40 || result.Live.LogRequestQPS != 20 || result.Live.LogRequestBurst != 40 {
		t.Fatal("resolved request budgets missing from evidence")
	}
	if result.Live.Logs.Lifecycle.Lines != followers*linesPerFollower || result.Live.Durability.FlushIntervalMilliseconds != 1000 || manifest.Spec.StreamSyncIntervalMillis != 1000 {
		t.Fatalf("capture/persistence policy missing: %+v", result.Live)
	}
	for _, record := range readLines[StreamObservation](t, spec.OutputDir+"/logs.ndjson") {
		if record.Type == "line" && (record.LogTime == nil || record.LogTime.Format(time.RFC3339Nano) != stamp) {
			t.Fatal("timestamped log evidence lost")
		}
	}
}

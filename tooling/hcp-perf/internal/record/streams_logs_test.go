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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/flowcontrol"
)

func logTestPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "uid"}, Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{Name: "c", RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
}

func TestLogFollowReconnectAndPrevious(t *testing.T) {
	t.Parallel()
	f := testLiveFile(t, 1<<20)
	c := fake.NewClientset(logTestPod())
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	opener := func(ctx context.Context, _ kubernetes.Interface, ns, name string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		calls++
		if ns != "ns" || name != "p" || opts.Container != "c" || !opts.Follow || !opts.Timestamps || opts.Previous {
			t.Errorf("bad follow options: %+v", opts)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("log request not bounded")
		}
		if calls == 1 {
			if opts.TailLines == nil || *opts.TailLines != 1000 || opts.SinceTime != nil {
				t.Error("unbounded initial tail")
			}
		} else if opts.TailLines != nil || opts.SinceTime == nil || !opts.SinceTime.Time.Equal(at.Add(-time.Second)) {
			t.Errorf("missing overlap: %+v", opts)
		}
		return io.NopCloser(strings.NewReader(at.Format(time.RFC3339Nano) + " raw-password=secret\n")), nil
	}
	task := logTask{key: logKey{uid: "uid", container: "c", restart: 2}, namespace: "ns", pod: "p", observedRestart: 2}
	out := followPodLogs(t.Context(), c, task, f, opener)
	if out.err != nil || !out.done || calls != 3 || out.since == nil || !out.since.Equal(at) {
		t.Fatalf("retry result: %+v calls=%d", out, calls)
	}
	lines := readLines[StreamObservation](t, flushedPath(t, f))
	lineCount := 0
	for _, line := range lines {
		if line.PodUID != "uid" || line.Restart != 2 || line.Container != "c" {
			t.Fatalf("missing identity: %+v", line)
		}
		if line.Type == "line" {
			lineCount++
			if line.Error != nil {
				t.Fatal("retry error contaminated log line metadata")
			}
			if line.Body != "raw-password=secret" || line.LogTime == nil {
				t.Fatal("raw log lost")
			}
		}
	}
	if lineCount != 3 || f.stats.Gaps != 4 {
		t.Fatalf("overlap must remain explicit, not falsely deduplicated: %+v", f.stats)
	}
	task.key.previous, task.key.restart, task.terminal = true, 1, true
	opener = func(_ context.Context, _ kubernetes.Interface, _, _ string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		if !opts.Previous || opts.Follow {
			t.Errorf("invalid previous options: %+v", opts)
		}
		return io.NopCloser(strings.NewReader(at.Format(time.RFC3339Nano) + " previous\n")), nil
	}
	out = followPodLogs(t.Context(), c, task, f, opener)
	if out.err != nil || !out.done {
		t.Fatalf("previous failed: %+v", out)
	}
	lines = readLines[StreamObservation](t, flushedPath(t, f))
	last := lines[len(lines)-2]
	if !last.Previous || last.Restart != 1 || last.Body != "previous" {
		t.Fatalf("previous metadata: %+v", last)
	}
}

func TestLogIdentityGuards(t *testing.T) {
	for _, change := range []string{"UID", "restart"} {
		t.Run(change, func(t *testing.T) {
			p := logTestPod()
			if change == "UID" {
				p.UID = "new"
			} else {
				p.Status.ContainerStatuses[0].RestartCount++
			}
			f := testLiveFile(t, 1<<20)
			out := followPodLogs(t.Context(), fake.NewClientset(p), logTask{key: logKey{uid: "uid", container: "c", restart: 2}, namespace: "ns", pod: "p", observedRestart: 2}, f,
				func(context.Context, kubernetes.Interface, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
					t.Error("opened logs for wrong pod/restart")
					return nil, errors.New("unexpected")
				})
			if out.err != nil || !out.done || f.stats.Gaps != 1 {
				t.Fatalf("identity mismatch hidden: %+v %+v", out, f.stats)
			}
		})
	}
}

func TestLogLineBound(t *testing.T) {
	f := testLiveFile(t, 4<<20)
	var since *time.Time
	var count int64
	input := "2026-09-01T00:00:00Z " + strings.Repeat("x", maxLogLineBytes+10) + "\n2026-09-01T00:00:01Z kept\nno-timestamp\n"
	if err := readLogLines(t.Context(), strings.NewReader(input), StreamObservation{PodUID: "uid"}, f, &since, &count); err != nil {
		t.Fatal(err)
	}
	lines := readLines[StreamObservation](t, flushedPath(t, f))
	if len(lines) != 4 || lines[0].Error.Reason != "logLineTooLarge" || lines[1].Body != "kept" || lines[2].Error.Reason != "logTimestampMissing" || lines[3].Body != "no-timestamp" {
		t.Fatalf("line bounds: %+v", lines)
	}
	if since == nil || since.Second() != 1 {
		t.Fatal("oversized/malformed line advanced cursor")
	}
}

func TestLogWorkerCapAndCancellation(t *testing.T) {
	t.Parallel()
	p := logTestPod()
	p.Status.ContainerStatuses = nil
	for i := 0; i < defaultLogWorkers+2; i++ {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: fmt.Sprintf("c%d", i), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
	}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
	c := fake.NewClientset(p)
	var opened, closed atomic.Int32
	opener := func(context.Context, kubernetes.Interface, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
		opened.Add(1)
		r, w := io.Pipe()
		return &countedLogReader{ReadCloser: r, writer: w, closed: &closed}, nil
	}
	f := testLiveFile(t, 1<<20)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	spec := testSpec(t)
	spec.Namespaces = []string{"ns"}
	if err := recordLogs(ctx, c, spec, f, opener); err != nil {
		t.Fatal(err)
	}
	if opened.Load() != defaultLogWorkers || closed.Load() != defaultLogWorkers {
		t.Fatalf("workers not bounded/joined: opened=%d closed=%d", opened.Load(), closed.Load())
	}
	if f.stats.Gaps != 3 {
		t.Fatalf("dropped streams not reported: %+v", f.stats)
	}
}

type countedLogReader struct {
	io.ReadCloser
	writer *io.PipeWriter
	closed *atomic.Int32
}

func TestLogConfiguredConcurrency(t *testing.T) {
	for _, limit := range []int{0, 1, 128, 256} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			spec := testSpec(t)
			spec.MaxConcurrentLogs = limit
			spec.Namespaces = []string{"ns", "last"}
			want := spec.withDefaults().MaxConcurrentLogs
			p := logTestPod()
			p.Status.ContainerStatuses = nil
			for i := 0; i < want+2; i++ {
				p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: fmt.Sprintf("c%d", i), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
			}
			c := fake.NewClientset(p)
			discovered, opened := make(chan struct{}), make(chan struct{}, want+2)
			c.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.GetNamespace() == "last" {
					close(discovered)
				}
				return false, nil, nil
			})
			// Hold requests before allocating log readers or producing live data.
			opener := func(ctx context.Context, _ kubernetes.Interface, _, _ string, _ *corev1.PodLogOptions) (io.ReadCloser, error) {
				opened <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			f := testLiveFile(t, 1<<20)
			done := make(chan error, 1)
			go func() { done <- recordLogs(ctx, c, spec, f, opener) }()
			for i := 0; i < want; i++ {
				select {
				case <-opened:
				case <-ctx.Done():
					t.Errorf("only %d of %d requests allocated", i, want)
				}
			}
			select {
			case <-discovered:
			case <-ctx.Done():
				t.Error("discovery did not finish")
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if len(opened) != 0 || f.stats.Gaps != 2 {
				t.Fatalf("concurrency bound ignored: extra=%d stats=%+v", len(opened), f.stats)
			}
		})
	}
}

func (r *countedLogReader) Close() error {
	r.closed.Add(1)
	return errors.Join(r.ReadCloser.Close(), r.writer.Close())
}

func TestLogsRESTCancellation(t *testing.T) {
	t.Parallel()
	requested := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/ns/pods/p/log" || r.URL.Query().Get("follow") != "true" || r.URL.Query().Get("timestamps") != "true" || r.URL.Query().Get("tailLines") != "1000" {
			t.Errorf("unsafe log request: %s", r.URL)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "2026-09-01T00:00:00Z live\n")
		w.(http.Flusher).Flush()
		requested <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	realClient, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	f := testLiveFile(t, 1<<20)
	c := fake.NewClientset(logTestPod())
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	opener := func(ctx context.Context, _ kubernetes.Interface, ns, name string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		return openPodLogs(ctx, realClient, ns, name, opts)
	}
	out := followPodLogs(ctx, c, logTask{key: logKey{uid: "uid", container: "c", restart: 2}, namespace: "ns", pod: "p", observedRestart: 2}, f, opener)
	if out.err != nil || out.lines != 1 || !out.canceled {
		t.Fatalf("cancel lost live data: %+v %+v", out, f.stats)
	}
	select {
	case <-requested:
	default:
		t.Fatal("log request not made")
	}
}

func TestLogAPIErrorsFixedAndBounded(t *testing.T) {
	t.Parallel()
	f := testLiveFile(t, 1<<20)
	c := fake.NewClientset(logTestPod())
	calls := 0
	opener := func(context.Context, kubernetes.Interface, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
		calls++
		return nil, errors.New("raw-password-secret")
	}
	out := followPodLogs(t.Context(), c, logTask{key: logKey{uid: "uid", container: "c", restart: 2}, namespace: "ns", pod: "p", observedRestart: 2}, f, opener)
	if out.err != nil || !out.done || calls != 3 || f.stats.Errors != 3 || f.stats.Gaps != 1 {
		t.Fatalf("retry failures hidden: %+v %+v calls=%d", out, f.stats, calls)
	}
	data, _ := os.ReadFile(flushedPath(t, f))
	if strings.Contains(string(data), "raw-password-secret") {
		t.Fatal("API error leaked")
	}
}

func TestLogIdentityRequestFailureStages(t *testing.T) {
	for _, stage := range []string{"verifyPodIdentity", "verifyLogEOF"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			c := fake.NewClientset(logTestPod())
			gets := 0
			c.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				gets++
				if stage == "verifyPodIdentity" || gets%2 == 0 {
					return true, nil, errors.New("client rate limiter Wait returned an error: private-reservation")
				}
				return false, nil, nil
			})
			f := testLiveFile(t, 1<<20)
			out := followPodLogs(t.Context(), c, logTask{key: logKey{uid: "uid", container: "c", restart: 2}, namespace: "ns", pod: "p", observedRestart: 2}, f,
				func(context.Context, kubernetes.Interface, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
					if stage == "verifyPodIdentity" {
						t.Error("opened logs without verifying identity")
					}
					return io.NopCloser(strings.NewReader("")), nil
				})
			if out.err != nil || !out.done || f.stats.Errors != 3 || f.stats.Gaps != 1 {
				t.Fatalf("identity failures hidden: %+v %+v", out, f.stats)
			}
			for _, record := range readLines[StreamObservation](t, flushedPath(t, f)) {
				if record.Type == "error" && (record.Error.Reason != "clientRateLimited" || record.Error.Stage != stage) {
					t.Fatalf("wrong request stage/reason: %+v", record.Error)
				}
			}
		})
	}
}

func TestLogRESTPlannedRenewal(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(fmt.Sprintf("idle=%t", idle), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			var gets, opens, closed atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/namespaces/ns/pods/p":
					gets.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(logTestPod())
				case "/api/v1/namespaces/ns/pods/p/log":
					n := opens.Add(1)
					q := r.URL.Query()
					if r.Method != http.MethodGet || q.Get("container") != "c" || q.Get("follow") != "true" || q.Get("timestamps") != "true" || q.Get("previous") == "true" {
						t.Error("incorrect log request options")
					}
					if idle || n == 1 {
						if q.Get("tailLines") != "1000" || q.Has("sinceTime") {
							t.Error("initial historical tail missing or zero sinceTime sent")
						}
					} else if q.Has("tailLines") || q.Get("sinceTime") != at.Add(-time.Second).Format(time.RFC3339) {
						t.Error("renewal lost timestamp overlap")
					}
					w.Header().Set("Content-Type", "text/plain")
					if !idle && n < 5 {
						_, _ = fmt.Fprintf(w, "%s %s\n", at.Format(time.RFC3339Nano), strings.Repeat("x", 1024))
					}
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					closed.Add(1)
				default:
					t.Error("unexpected request path (including unsupported status endpoint)")
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			// Isolate renewal from client-side throttling, covered separately below.
			client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, QPS: -1})
			if err != nil {
				t.Fatal(err)
			}
			f := testLiveFile(t, 1<<20)
			opener := func(ctx context.Context, c kubernetes.Interface, ns, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
				body, err := openPodLogs(ctx, c, ns, pod, opts)
				if opens.Load() == 5 {
					cancel()
				}
				return body, err
			}
			out := followPodLogsWithRenewal(ctx, client, logTask{key: logKey{uid: "uid", container: "c", restart: 2}, namespace: "ns", pod: "p", observedRestart: 2}, f, opener, 100*time.Millisecond)
			server.Close()
			if out.err != nil || out.done || opens.Load() != 5 || gets.Load() != 5 || closed.Load() != 5 || f.stats.Errors != 0 || f.stats.Gaps != 0 {
				t.Fatalf("HTTP renewal failed: outcome=%+v stats=%+v gets=%d opens=%d closed=%d", out, f.stats, gets.Load(), opens.Load(), closed.Load())
			}
			lines, reconnects := 0, 0
			for _, record := range readLines[StreamObservation](t, flushedPath(t, f)) {
				switch record.Type {
				case "line":
					lines++
					if record.LogTime == nil || !record.LogTime.Equal(at) || record.Body != strings.Repeat("x", 1024) {
						t.Error("historical line lost or truncated")
					}
				case "reconnect":
					reconnects++
					if record.Error == nil || record.Error.Reason != "logRenewal" || !record.PotentialGap {
						t.Error("renewal lost potential overlap/gap hint")
					}
				case "streamRequested", "streamOpened", "streamCanceled":
				default:
					t.Errorf("unexpected observation type %q", record.Type)
				}
			}
			wantLines := 4
			if idle {
				wantLines = 0
			}
			if lines != wantLines || reconnects != 4 {
				t.Fatalf("capture lost records: lines=%d reconnects=%d", lines, reconnects)
			}
		})
	}
}

func TestLogRESTFailuresRemainExplicit(t *testing.T) {
	for _, mode := range []string{"unauthorized", "forbidden", "notFound", "clientTimeout", "headersTimeout", "throttled"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if mode == "clientTimeout" || mode == "headersTimeout" {
					if mode == "clientTimeout" {
						w.Header().Set("Content-Type", "text/plain")
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
					return
				}
				code := map[string]int{"unauthorized": 401, "forbidden": 403, "notFound": 404}[mode]
				if code == 0 {
					t.Error("throttled request reached server")
					code = 500
				}
				http.Error(w, "private-response-must-not-be-recorded", code)
			}))
			defer server.Close()
			config := &rest.Config{Host: server.URL, QPS: -1}
			renewal := 100 * time.Millisecond
			if mode == "clientTimeout" {
				config.Timeout = 20 * time.Millisecond
			}
			if mode == "throttled" {
				// A depleted bucket rejects a reservation beyond the request deadline
				// immediately, without an APIStatus or context.DeadlineExceeded.
				config.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(0.1, 1)
				config.RateLimiter.TryAccept()
			}
			client, err := kubernetes.NewForConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			opener := func(ctx context.Context, _ kubernetes.Interface, ns, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
				return openPodLogs(ctx, client, ns, pod, opts)
			}
			f := testLiveFile(t, 1<<20)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			out := followPodLogsWithRenewal(ctx, fake.NewClientset(logTestPod()), logTask{key: logKey{uid: "uid", container: "c", restart: 2}, namespace: "ns", pod: "p", observedRestart: 2}, f, opener, renewal)
			if out.err != nil || !out.done || ctx.Err() != nil || f.stats.Errors != 3 || f.stats.Gaps != 1 {
				t.Fatalf("real failure hidden: outcome=%+v stats=%+v", out, f.stats)
			}
			wantReason := mode
			if mode == "clientTimeout" {
				wantReason = "timeout"
			} else if mode == "headersTimeout" {
				wantReason = "timeout"
			} else if mode == "throttled" {
				wantReason = "clientRateLimited"
				if mode == "throttled" && requests.Load() != 0 {
					t.Error("local throttle rejection sent HTTP requests")
				}
			}
			for _, record := range readLines[StreamObservation](t, flushedPath(t, f)) {
				switch record.Type {
				case "error":
					wantStage := "openLogs"
					if mode == "clientTimeout" {
						wantStage = "streamRead"
					}
					wantCode := map[string]int32{"unauthorized": 401, "forbidden": 403, "notFound": 404}[mode]
					if record.Error == nil || record.Error.Reason != wantReason || record.Error.Code != wantCode || record.Error.Stage != wantStage {
						t.Errorf("wrong safe error classification: %+v", record.Error)
					}
				case "gap":
					if record.Error == nil || record.Error.Reason != "logRetriesExhausted" {
						t.Error("retry exhaustion not reported")
					}
				case "streamRequested", "streamOpened", "streamFailed":
				default:
					t.Errorf("failure incorrectly emitted %q", record.Type)
				}
			}
			data, err := os.ReadFile(flushedPath(t, f))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "private-response-must-not-be-recorded") {
				t.Fatal("HTTP error response leaked into archive")
			}
		})
	}
}

func TestLogDiscoveryPreviousAndTerminal(t *testing.T) {
	t.Parallel()
	p := logTestPod()
	p.Status.Phase = corev1.PodSucceeded
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", State: p.Status.ContainerStatuses[0].State}}
	c := fake.NewClientset(p)
	c.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() != "ns" {
			t.Error("wrong log namespace")
		}
		return false, nil, nil
	})
	var previous, current, init atomic.Int32
	opener := func(_ context.Context, _ kubernetes.Interface, _, _ string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		switch {
		case opts.Previous:
			previous.Add(1)
		case opts.Container == "init":
			init.Add(1)
		default:
			current.Add(1)
		}
		return io.NopCloser(strings.NewReader("2026-09-01T00:00:00Z terminal\n")), nil
	}
	f := testLiveFile(t, 1<<20)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	spec := testSpec(t)
	spec.Namespaces = []string{"ns"}
	if err := recordLogs(ctx, c, spec, f, opener); err != nil {
		t.Fatal(err)
	}
	if previous.Load() != 1 || current.Load() != 1 || init.Load() != 1 {
		t.Fatalf("missing terminal streams: previous=%d current=%d init=%d", previous.Load(), current.Load(), init.Load())
	}
	if f.stats.Gaps != 1 {
		t.Fatal("older restarts not reported")
	}
	if counts := f.stats.Lifecycle; counts == nil || counts.Discovered != 3 || counts.Attempted != 3 || counts.Opened != 3 || counts.Completed != 3 || counts.Canceled != 0 || counts.Failed != 0 || counts.Active != 0 || counts.Requests != 3 || counts.StreamsOpened != 3 || counts.Lines != 3 {
		t.Fatalf("terminal identity accounting: %+v", counts)
	}
}

func TestLogPlannedRenewalIsNotFailure(t *testing.T) {
	f := testLiveFile(t, 1<<20)
	c := fake.NewClientset(logTestPod())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	calls := 0
	var closed atomic.Int32
	opener := func(_ context.Context, _ kubernetes.Interface, _, _ string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		calls++
		if !opts.Follow || !opts.Timestamps {
			t.Error("renewal not following timestamped logs")
		}
		if calls == 3 {
			cancel()
			return nil, context.Canceled
		}
		r, w := io.Pipe()
		return &countedLogReader{ReadCloser: r, writer: w, closed: &closed}, nil
	}
	out := followPodLogsWithRenewal(ctx, c, logTask{key: logKey{uid: "uid", container: "c", restart: 2}, namespace: "ns", pod: "p", observedRestart: 2}, f, opener, 20*time.Millisecond)
	if out.err != nil || out.done || calls != 3 || closed.Load() != 2 || f.stats.Errors != 0 || f.stats.Gaps != 0 {
		t.Fatalf("planned renewals failed or leaked: %+v %+v calls=%d closed=%d", out, f.stats, calls, closed.Load())
	}
	lines := readLines[StreamObservation](t, flushedPath(t, f))
	if len(lines) != 8 {
		t.Fatalf("renewals not recorded: %+v", lines)
	}
	for _, line := range lines {
		if line.Type != "reconnect" {
			continue
		}
		if line.Type != "reconnect" || !line.PotentialGap || line.Error.Reason != "logRenewal" {
			t.Fatalf("renewal claimed known loss: %+v", line)
		}
	}
}

func TestLogEOFRechecksTermination(t *testing.T) {
	for _, change := range []string{"terminated", "init terminated", "ephemeral terminated", "disappeared", "UID", "restart", "previous restart"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			p := logTestPod()
			if change == "init terminated" {
				p.Status.InitContainerStatuses, p.Status.ContainerStatuses = p.Status.ContainerStatuses, nil
			}
			if change == "ephemeral terminated" {
				p.Status.EphemeralContainerStatuses, p.Status.ContainerStatuses = p.Status.ContainerStatuses, nil
			}
			c := fake.NewClientset(p)
			f := testLiveFile(t, 1<<20)
			calls := 0
			opener := func(ctx context.Context, _ kubernetes.Interface, _, _ string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
				calls++
				switch change {
				case "disappeared":
					if err := c.CoreV1().Pods("ns").Delete(ctx, "p", metav1.DeleteOptions{}); err != nil {
						t.Fatal(err)
					}
				case "UID":
					p.UID = "replacement"
				case "restart", "previous restart":
					p.Status.ContainerStatuses[0].RestartCount++
				default:
					for _, statuses := range [][]corev1.ContainerStatus{p.Status.ContainerStatuses, p.Status.InitContainerStatuses, p.Status.EphemeralContainerStatuses} {
						for i := range statuses {
							statuses[i].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
						}
					}
				}
				if change != "disappeared" {
					if _, err := c.CoreV1().Pods("ns").UpdateStatus(ctx, p, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				return io.NopCloser(strings.NewReader("2026-09-01T00:00:00Z final\n")), nil
			}
			task := logTask{key: logKey{uid: "uid", container: "c", restart: 2}, namespace: "ns", pod: "p", observedRestart: 2}
			if change == "previous restart" {
				task.key.previous, task.key.restart = true, 1
			}
			out := followPodLogs(t.Context(), c, task, f, opener)
			if out.err != nil || !out.done || calls != 1 {
				t.Fatalf("EOF result: %+v calls=%d", out, calls)
			}
			if strings.Contains(change, "terminated") {
				if f.stats.Gaps != 0 || f.stats.Errors != 0 {
					t.Fatalf("normal completion reported loss: %+v", f.stats)
				}
			} else if f.stats.Gaps != 1 {
				t.Fatalf("uncertain EOF reported complete capture: %+v", f.stats)
			}
		})
	}
}

func TestLogTerminalTimeoutsAreBounded(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(fmt.Sprint(previous), func(t *testing.T) {
			t.Parallel()
			p := logTestPod()
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
			f := testLiveFile(t, 1<<20)
			calls := 0
			var closed atomic.Int32
			opener := func(context.Context, kubernetes.Interface, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
				calls++
				r, w := io.Pipe()
				return &countedLogReader{ReadCloser: r, writer: w, closed: &closed}, nil
			}
			task := logTask{key: logKey{uid: "uid", container: "c", restart: 2, previous: previous}, namespace: "ns", pod: "p", observedRestart: 2}
			if previous {
				task.key.restart--
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			out := followPodLogsWithRenewal(ctx, fake.NewClientset(p), task, f, opener, 20*time.Millisecond)
			if out.err != nil || !out.done || ctx.Err() != nil || calls != 3 || closed.Load() != 3 || f.stats.Gaps != 1 || f.stats.Errors != 3 {
				t.Fatalf("terminal retrieval retried indefinitely: %+v stats=%+v calls=%d closed=%d", out, f.stats, calls, closed.Load())
			}
		})
	}
}

func TestLogCapacityRetriesAllContainers(t *testing.T) {
	t.Parallel()
	p := logTestPod()
	p.Status.ContainerStatuses[0].RestartCount = 0
	p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debug", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
	c := fake.NewClientset(p)
	discovered := make(chan struct{})
	c.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "last" {
			select {
			case <-discovered:
			default:
				close(discovered)
			}
		}
		return false, nil, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var current, ephemeral atomic.Int32
	opener := func(ctx context.Context, _ kubernetes.Interface, _, _ string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		if opts.Previous {
			t.Error("requested previous logs for zero restarts")
		}
		if opts.Container == "c" {
			current.Add(1)
			select {
			case <-discovered:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
			if _, err := c.CoreV1().Pods("ns").UpdateStatus(ctx, p, metav1.UpdateOptions{}); err != nil {
				return nil, err
			}
		} else {
			ephemeral.Add(1)
			cancel()
		}
		return io.NopCloser(strings.NewReader("2026-09-01T00:00:00Z captured\n")), nil
	}
	spec := testSpec(t)
	spec.Namespaces, spec.MaxConcurrentLogs, spec.IntervalSeconds = []string{"ns", "last"}, 1, 1
	f := testLiveFile(t, 1<<20)
	if err := recordLogs(ctx, c, spec, f, opener); err != nil {
		t.Fatal(err)
	}
	if current.Load() != 1 || ephemeral.Load() != 1 || f.stats.Gaps != 1 {
		t.Fatalf("capacity skip became permanent or completed stream reopened: current=%d ephemeral=%d stats=%+v", current.Load(), ephemeral.Load(), f.stats)
	}
}

func TestLogBufferedCancellation(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			f := testLiveFile(t, 4<<20)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
				defer cancel()
				f.buffer = bufio.NewWriterSize(streamWriteFunc(func(p []byte) (int, error) {
					time.Sleep(time.Second)
					return f.f.Write(p)
				}), 1)
				input := strings.Repeat("2026-09-01T00:00:00Z buffered\n", 10000)
				if partial {
					input += "2026-09-01T00:00:00Z unfinished"
				}
				var since *time.Time
				var lines int64
				start := time.Now()
				err := readLogLines(ctx, strings.NewReader(input), StreamObservation{PodUID: "uid"}, f, &since, &lines)
				if !errors.Is(err, context.DeadlineExceeded) || lines != 2 || time.Since(start) != 3*time.Second {
					t.Fatalf("drained after cancellation: lines=%d elapsed=%s err=%v", lines, time.Since(start), err)
				}
				records := readLines[StreamObservation](t, flushedPath(t, f))
				last := records[len(records)-1]
				if len(records) != 3 || last.Error == nil || last.Error.Reason != "canceledWithBufferedData" || last.BufferedBytes == 0 || last.PartialLine != partial {
					t.Fatalf("known discarded buffer not reported: %+v", records)
				}
			})
		})
	}
}

func TestLogCancellationBeforeReadAndDuringOversizedLine(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprint(before), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if before {
				cancel()
			}
			f := testLiveFile(t, 4<<20)
			// Cancel during the oversized-line gap's write. No further body read
			// or draining of that line is allowed after this syscall returns.
			f.buffer = bufio.NewWriterSize(streamWriteFunc(func(p []byte) (int, error) {
				cancel()
				return f.f.Write(p)
			}), 1)
			body := &countLogReads{Reader: strings.NewReader(strings.Repeat("x", 4*maxLogLineBytes) + "\n")}
			var since *time.Time
			var lines int64
			err := readLogLines(ctx, body, StreamObservation{PodUID: "uid"}, f, &since, &lines)
			wantReads := 1
			if before {
				wantReads = 0
			}
			if !errors.Is(err, context.Canceled) || body.reads != wantReads || lines != 0 || since != nil {
				t.Fatalf("read after cancellation: reads=%d lines=%d err=%v", body.reads, lines, err)
			}
		})
	}
}

func TestLogBufferedTerminalRenewalDeadline(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(fmt.Sprint(previous), func(t *testing.T) {
			f := testLiveFile(t, 4<<20)
			p := logTestPod()
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
			c := fake.NewClientset(p)
			synctest.Test(t, func(t *testing.T) {
				f.buffer = bufio.NewWriterSize(streamWriteFunc(func(p []byte) (int, error) {
					time.Sleep(10 * time.Millisecond)
					return f.f.Write(p)
				}), 1)
				calls := 0
				opener := func(context.Context, kubernetes.Interface, string, string, *corev1.PodLogOptions) (io.ReadCloser, error) {
					calls++
					return io.NopCloser(strings.NewReader(strings.Repeat("2026-09-01T00:00:00Z buffered\n", 10000))), nil
				}
				task := logTask{key: logKey{uid: "uid", container: "c", restart: 2, previous: previous}, namespace: "ns", pod: "p", observedRestart: 2}
				if previous {
					task.key.restart--
				}
				start := time.Now()
				out := followPodLogsWithRenewal(t.Context(), c, task, f, opener, 50*time.Millisecond)
				if out.err != nil || !out.done || out.complete || calls != 3 || out.lines > 12 || time.Since(start) > time.Second || f.stats.Errors != 3 || f.stats.Gaps != 4 {
					t.Fatalf("terminal request drained past deadline: %+v calls=%d elapsed=%s stats=%+v", out, calls, time.Since(start), f.stats)
				}
			})
		})
	}
}

type countLogReads struct {
	io.Reader
	reads int
}

func (r *countLogReads) Read(p []byte) (int, error) {
	r.reads++
	return r.Reader.Read(p)
}

func TestLogWriterWaitCancellation(t *testing.T) {
	f := testLiveFile(t, 1<<20)
	entered, release := make(chan struct{}), make(chan struct{})
	f.buffer = bufio.NewWriterSize(streamWriteFunc(func(p []byte) (int, error) {
		close(entered)
		<-release
		return f.f.Write(p)
	}), 1)
	first := make(chan error, 1)
	go func() { first <- f.write(StreamObservation{Type: "line"}) }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	second := make(chan error, 1)
	go func() { second <- f.writeContext(ctx, StreamObservation{Type: "line", Body: "must not append"}) }()
	cancel()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("writer cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("canceled waiter blocked on another writer's write syscall")
	}
	// The active filesystem syscall must finish before finalization can proceed.
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if f.stats.Records != 1 || f.stats.Errors != 0 {
		t.Fatalf("canceled append changed storage: %+v", f.stats)
	}
}

func TestLogEmptyTerminalLifecycle(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(fmt.Sprint(previous), func(t *testing.T) {
			p := logTestPod()
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
			var gets, opens int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/log") {
					opens++
					w.Header().Set("Content-Type", "text/plain")
					return
				}
				gets++
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(p)
			}))
			defer server.Close()
			c, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, QPS: -1})
			if err != nil {
				t.Fatal(err)
			}
			f := testLiveFile(t, 1<<20)
			task := logTask{key: logKey{uid: "uid", container: "c", restart: 2, previous: previous}, namespace: "ns", pod: "p", observedRestart: 2}
			if previous {
				task.key.restart--
			}
			out := followPodLogs(t.Context(), c, task, f, openPodLogs)
			server.Close()
			records := readLines[StreamObservation](t, flushedPath(t, f))
			if out.err != nil || !out.complete || out.lines != 0 || gets != 2 || opens != 1 || len(records) != 3 {
				t.Fatalf("empty EOF not verified/completed: %+v gets=%d opens=%d records=%+v", out, gets, opens, records)
			}
			for i, kind := range []string{"streamRequested", "streamOpened", "streamCompleted"} {
				if records[i].Type != kind || records[i].PodUID != "uid" || records[i].Previous != previous {
					t.Fatalf("missing identity lifecycle: %+v", records)
				}
			}
		})
	}
}

func TestRecordBufferedLogsFinalSummary(t *testing.T) {
	t.Parallel()
	p := logTestPod()
	p.Status.ContainerStatuses[0].RestartCount = 0
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			w.Header().Set("Content-Type", "text/plain")
			if r.URL.Query().Get("container") == "init" {
				return
			}
			chunk := strings.Repeat("2026-09-01T00:00:00Z buffered\n", 32768)
			for range 128 {
				if _, err := io.WriteString(w, chunk); err != nil {
					return
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
	}))
	defer server.Close()
	spec := testSpec(t)
	spec.Namespaces = []string{"ns"}
	spec.EnableLogs, spec.RawLogs = true, true
	c, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, QPS: -1})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result, recordErr := RecordWithLogClient(t.Context(), fake.NewClientset(p), c, spec)
	if time.Since(start) > 5*time.Second {
		t.Fatal("buffered logs delayed finalization")
	}
	if result.Live == nil || result.Live.Logs.Lifecycle == nil {
		t.Fatalf("missing lifecycle summary: %+v %v", result, recordErr)
	}
	counts := result.Live.Logs.Lifecycle
	if counts.Discovered != 2 || counts.Attempted != 2 || counts.Opened != 2 || counts.Completed != 1 || counts.CompletedEmpty != 1 || counts.Canceled != 1 || counts.Failed != 0 || counts.Active != 0 || counts.Lines == 0 {
		t.Fatalf("incorrect lifecycle counts: %+v %v", counts, recordErr)
	}
	if got := readJSON[Result](t, filepath.Join(spec.OutputDir, "summary.json")); !reflect.DeepEqual(got, result) {
		t.Fatal("final result not durable")
	}
	if got := readJSON[LiveSummary](t, filepath.Join(spec.OutputDir, "stream-summary.json")); !reflect.DeepEqual(got, *result.Live) {
		t.Fatal("stream summary not durable")
	}
	window, known := 0, 0
	for _, record := range readLines[StreamObservation](t, filepath.Join(spec.OutputDir, "logs.ndjson")) {
		if record.Type == "captureWindowEnded" && record.PotentialGap {
			window++
		}
		if record.Error != nil && record.Error.Reason == "canceledWithBufferedData" {
			known++
		}
	}
	if window != 1 || (known > 0 && (!result.Live.Incomplete || recordErr == nil)) {
		t.Fatalf("stop evidence hidden: window=%d known=%d result=%+v err=%v", window, known, result, recordErr)
	}
	t.Logf("shutdown=%s lifecycle=%+v captureWindowEnded=%d canceledWithBufferedData=%d elapsed=%s", result.Shutdown, counts, window, known, time.Since(start))
}

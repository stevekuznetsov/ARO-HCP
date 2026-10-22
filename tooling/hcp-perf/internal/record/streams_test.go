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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testLiveFile(t *testing.T, limit int64) *liveFile {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "stream.ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	live := &liveFile{f: f, buffer: bufio.NewWriterSize(f, liveBufferBytes), limit: limit}
	t.Cleanup(func() { _ = f.Close() })
	return live
}

func flushedPath(t *testing.T, f *liveFile) string {
	t.Helper()
	if err := f.flushAndSync(); err != nil {
		t.Fatal(err)
	}
	return f.f.Name()
}

func TestStreamValidation(t *testing.T) {
	spec := testSpec(t)
	for i := 0; i < 100; i++ {
		spec.Namespaces = append(spec.Namespaces, fmt.Sprintf("ns-%d", i))
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("poll-only namespace limit changed: %v", err)
	}
	spec.EnableWatch = true
	if err := spec.Validate(); err == nil {
		t.Fatal("unbounded watches accepted")
	}
	spec.Namespaces = spec.Namespaces[:100]
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	spec.EnableLogs = true
	if err := spec.Validate(); err == nil {
		t.Fatal("raw logs without acknowledgement accepted")
	}
	spec.RawLogs = true
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{-1, MaxLiveStreamBytes + 1} {
		spec.MaxStreamBytes = limit
		if err := spec.Validate(); err == nil {
			t.Fatal("invalid stream budget accepted")
		}
	}
	data, err := json.Marshal(testSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"enableWatch", "enableLogs", "rawLogs", "maxStreamBytes", "maxConcurrentLogs", "maxObservationBytes", "logRequestQPS", "logRequestBurst", "streamSyncIntervalMillis"} {
		if strings.Contains(string(data), field) {
			t.Fatalf("disabled option %s not omitted", field)
		}
	}
}

func TestRecordLimitValidation(t *testing.T) {
	for _, logs := range []int{-1, 0, 1, 32, 128, 256, 257} {
		for _, bytes := range []int64{-1, 0, 1, 1 << 30, 8 << 30, (8 << 30) + 1} {
			spec := sessionSpec()
			spec.MaxConcurrentLogs, spec.MaxStreamBytes, spec.MaxObservationBytes = logs, bytes, bytes
			valid := logs >= 0 && logs <= 256 && bytes >= 0 && bytes <= 8<<30
			if err := spec.Validate(); (err == nil) != valid {
				t.Fatalf("logs=%d bytes=%d: %v", logs, bytes, err)
			}
		}
	}
	for _, field := range []string{"logs", "stream", "observations"} {
		for _, value := range []int64{-1, (8 << 30) + 1} {
			spec := sessionSpec()
			switch field {
			case "logs":
				spec.MaxConcurrentLogs = int(value)
			case "stream":
				spec.MaxStreamBytes = value
			case "observations":
				spec.MaxObservationBytes = value
			}
			c := sessionClient()
			if _, err := Start(t.Context(), c, spec); err == nil || len(c.Actions()) != 0 {
				t.Fatalf("invalid %s limit contacted API or was accepted", field)
			}
		}
	}
	spec := testSpec(t).withDefaults()
	if spec.MaxConcurrentLogs != 256 || spec.MaxStreamBytes != 1<<30 || spec.MaxObservationBytes != 1<<30 {
		t.Fatalf("defaults changed: %+v", spec)
	}
}

func TestLiveFileBoundsIncludeErrors(t *testing.T) {
	f := testLiveFile(t, 500)
	if err := f.write(StreamObservation{Type: "error", Error: &APIError{Reason: "forbidden"}}); err != nil {
		t.Fatal(err)
	}
	if info, err := f.f.Stat(); err != nil || info.Size() != 0 || f.stats.Bytes == 0 || int64(f.buffer.Buffered()) != f.stats.Bytes {
		t.Fatalf("buffered bytes not charged before flush: info=%v err=%v stats=%+v", info, err, f.stats)
	}
	if err := f.write(StreamObservation{Type: "line", Body: strings.Repeat("x", 501)}); err == nil {
		t.Fatal("limit ignored")
	}
	if err := f.write(StreamObservation{Type: "error"}); err == nil {
		t.Fatal("write after limit succeeded")
	}
	flushedPath(t, f)
	info, err := f.f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != f.stats.Bytes || info.Size() > 500 || !f.stats.LimitReached || f.stats.Errors != 2 || f.stats.Records != 1 {
		t.Fatalf("incorrect limit accounting: %+v size=%d", f.stats, info.Size())
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("live file is not private")
	}
	g := testLiveFile(t, 1000)
	_ = g.f.Close()
	if err := g.write(StreamObservation{Type: "bookmark"}); err != nil {
		t.Fatal(err)
	}
	if err := g.flushAndSync(); err == nil || g.stats.Errors != 1 {
		t.Fatal("storage failure ignored")
	}
}

func TestLiveFileConcurrentLargeBudget(t *testing.T) {
	f := testLiveFile(t, 8<<30)
	// Exercise 64-bit accounting at the ceiling without writing GiBs to disk.
	const remaining = 4096
	f.stats.Bytes = f.limit - remaining
	var workers sync.WaitGroup
	for range 128 {
		workers.Go(func() { _ = f.write(StreamObservation{Type: "line", Body: "data"}) })
	}
	workers.Wait()
	flushedPath(t, f)
	info, err := f.f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !f.stats.LimitReached || f.stats.Records == 0 || f.stats.Bytes > f.limit || info.Size() != f.stats.Bytes-(f.limit-remaining) {
		t.Fatalf("concurrent byte accounting failed: %+v size=%d", f.stats, info.Size())
	}
}

func TestWatchRelistAndSafeEvents(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"EOF", "410-event", "410-open"} {
		t.Run(ending, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			c := fake.NewClientset()
			f := testLiveFile(t, 1<<20)
			lists, watches := 0, 0
			var watchers []*watch.RaceFreeFakeWatcher
			c.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				lists++
				if a.GetNamespace() != "ns" || a.(k8stesting.ListActionImpl).GetListOptions().Limit != 500 {
					t.Error("unscoped/unbounded list")
				}
				return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: fmt.Sprint(lists * 10)}, Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", ResourceVersion: "9"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Env: []corev1.EnvVar{{Name: "PASSWORD", Value: "user-secret"}}}}}}}}, nil
			})
			c.PrependWatchReactor("pods", func(a k8stesting.Action) (bool, watch.Interface, error) {
				watches++
				opts := a.(k8stesting.WatchActionImpl).GetWatchRestrictions()
				wantRV := fmt.Sprint(lists * 10)
				if ending == "EOF" && watches == 2 {
					wantRV = "12"
				}
				if opts.ResourceVersion != wantRV {
					t.Errorf("watch RV=%s lists=%d", opts.ResourceVersion, lists)
				}
				w := watch.NewRaceFreeFake()
				watchers = append(watchers, w)
				if watches == 2 {
					cancel()
					return true, w, nil
				}
				if ending == "410-open" {
					return true, nil, apierrors.NewResourceExpired("user-secret")
				}
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", ResourceVersion: "11", Annotations: map[string]string{"password": "user-secret"}}}
				w.Add(p)
				w.Modify(p)
				w.Delete(p)
				w.Action(watch.Bookmark, &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "12"}})
				if ending == "410-event" {
					w.Error(&metav1.Status{Code: 410, Reason: metav1.StatusReasonExpired, Message: "user-secret"})
				} else {
					w.Stop()
				}
				return true, w, nil
			})
			if err := recordWatch(ctx, c, "ns", "pods", f); err != nil {
				t.Fatal(err)
			}
			wantLists := 2
			if ending == "EOF" {
				wantLists = 1
			}
			if lists != wantLists || watches != 2 {
				t.Fatalf("not relisted: lists=%d watches=%d", lists, watches)
			}
			for _, w := range watchers {
				if !w.IsStopped() && ending != "410-open" {
					t.Fatal("watch not stopped")
				}
				w.Stop()
			}
			lines := readLines[StreamObservation](t, flushedPath(t, f))
			types := map[string]int{}
			for _, line := range lines {
				types[line.Type]++
				if line.AtTime.IsZero() {
					t.Fatal("missing timestamp")
				}
			}
			wantGaps := 1
			if ending == "EOF" {
				wantGaps = 0
				if types["reconnect"] != 1 || f.stats.Errors != 0 || f.stats.Gaps != 0 {
					t.Fatalf("normal close marked failure: %+v", f.stats)
				}
			}
			if types["gap"] != wantGaps || types["bookmark"] < 2 {
				t.Fatalf("missing gap/list boundary: %v", types)
			}
			if ending != "410-open" && (types["added"] != wantLists+1 || types["modified"] != 1 || types["deleted"] != 1) {
				t.Fatalf("missing watch events: %v", types)
			}
			data, _ := os.ReadFile(f.f.Name())
			if strings.Contains(string(data), "user-secret") {
				t.Fatal("watch leaked secret")
			}
		})
	}
}

func TestWatchSanitizationMatchesPoll(t *testing.T) {
	const secret = "do-not-persist"
	m := metav1.ObjectMeta{Name: "p", Namespace: "ns", Annotations: map[string]string{"private": secret}}
	p := &corev1.Pod{ObjectMeta: m, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Command: []string{secret}}}}}
	d := &appsv1.Deployment{ObjectMeta: m, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: p.Spec}}, Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Message: secret}}}}
	s := &appsv1.StatefulSet{ObjectMeta: m, Spec: appsv1.StatefulSetSpec{Template: d.Spec.Template}, Status: appsv1.StatefulSetStatus{Conditions: []appsv1.StatefulSetCondition{{Type: "Ready", Message: secret}}}}
	e := &corev1.Event{ObjectMeta: m, Message: secret, Series: &corev1.EventSeries{Count: 2}}
	c := fake.NewClientset(p, d, s, e)
	for resource, obj := range map[string]runtime.Object{"pods": p, "deployments": d, "statefulsets": s, "events": e} {
		body, err := sanitizeWatchObject(obj)
		if err != nil {
			t.Fatal(err)
		}
		entries, _, err := listPage(t.Context(), c, resource, "ns", metav1.ListOptions{})
		if err != nil || len(entries) != 1 || !reflect.DeepEqual(body, entries[0]) {
			t.Fatalf("watch/poll allowlist drift for %s: %v", resource, err)
		}
		data, _ := json.Marshal(body)
		if strings.Contains(string(data), secret) {
			t.Fatalf("%s leaked raw object", resource)
		}
	}
	if _, err := sanitizeWatchObject(&corev1.Secret{ObjectMeta: m}); err == nil {
		t.Fatal("secret object accepted")
	}
}

func TestRecordLiveIntegration(t *testing.T) {
	t.Parallel()
	spec := testSpec(t)
	spec.EnableWatch, spec.EnableLogs, spec.RawLogs = true, true, true
	spec.MaxConcurrentLogs, spec.MaxStreamBytes, spec.MaxObservationBytes = 128, 8<<30, 8<<30
	spec.StreamSyncIntervalMillis = 250
	c := fake.NewClientset()
	c.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() != "future-hcp" {
			t.Error("all-namespace request")
		}
		m := metav1.ListMeta{ResourceVersion: "10"}
		switch a.GetResource().Resource {
		case "pods":
			return true, &corev1.PodList{ListMeta: m}, nil
		case "events":
			return true, &corev1.EventList{ListMeta: m}, nil
		case "deployments":
			return true, &appsv1.DeploymentList{ListMeta: m}, nil
		default:
			return true, &appsv1.StatefulSetList{ListMeta: m}, nil
		}
	})
	c.PrependWatchReactor("*", func(a k8stesting.Action) (bool, watch.Interface, error) {
		if a.GetNamespace() != "future-hcp" {
			t.Error("all-namespace watch")
		}
		w := watch.NewRaceFreeFake()
		w.Stop() // Normal server EOF must not turn a healthy duration run into failure.
		return true, w, nil
	})
	result, err := Record(t.Context(), c, spec)
	if err != nil || result.Live == nil || result.Live.Incomplete || result.Live.Watch.Records <= 4 || result.Live.Watch.Required != 4 || result.Live.Watch.Initialized != 4 || result.Samples != 4 {
		t.Fatalf("integration: %+v %v", result, err)
	}
	if got := readJSON[LiveSummary](t, filepath.Join(spec.OutputDir, "stream-summary.json")); !reflect.DeepEqual(got, *result.Live) {
		t.Fatal("live summary mismatch")
	}
	if result.Live.MaxStreamBytes != 8<<30 || result.Live.MaxConcurrentLogs != 128 {
		t.Fatalf("configured live limits ignored: %+v", result.Live)
	}
	manifest := readJSON[struct {
		Spec Spec `json:"spec"`
	}](t, filepath.Join(spec.OutputDir, "manifest.json"))
	if manifest.Spec.StreamSyncIntervalMillis != 250 || result.Live.Durability.FlushIntervalMilliseconds != 250 || result.Live.Durability.MaximumBufferedBytes != liveBufferBytes {
		t.Fatalf("configured durability missing: manifest=%+v live=%+v", manifest, result.Live)
	}
	if got := readJSON[Result](t, filepath.Join(spec.OutputDir, "summary.json")); !reflect.DeepEqual(got, result) {
		t.Fatal("main summary mismatch")
	}
	for _, name := range []string{"watch.ndjson", "logs.ndjson", "stream-summary.json"} {
		info, err := os.Stat(filepath.Join(spec.OutputDir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("artifact %s: %v", name, err)
		}
	}
}

func TestStreamsFailureIndependent(t *testing.T) {
	t.Parallel()
	spec := testSpec(t)
	spec.OutputDir = t.TempDir()
	spec.EnableWatch, spec.EnableLogs, spec.RawLogs = true, true, true
	path := filepath.Join(spec.OutputDir, "watch.ndjson")
	if err := os.WriteFile(path, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientset()
	summary, err := RunStreams(t.Context(), c, spec)
	if err == nil || !summary.Incomplete {
		t.Fatal("exclusive open failure hidden")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "untouched" {
		t.Fatal("existing stream overwritten")
	}
	if _, err := os.Stat(filepath.Join(spec.OutputDir, "logs.ndjson")); err != nil {
		t.Fatal("watch error suppressed logs")
	}
	if len(c.Actions()) == 0 {
		t.Fatal("independent log discovery did not run")
	}
}

func TestWatchErrorSanitizedAndCancellation(t *testing.T) {
	t.Parallel()
	f := testLiveFile(t, 1<<20)
	c := fake.NewClientset()
	c.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "user-secret", errors.New("user-secret"))
	})
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if err := recordWatch(ctx, c, "ns", "pods", f); err != nil {
		t.Fatal(err)
	}
	if f.stats.Errors < 1 {
		t.Fatal("API error not counted")
	}
	data, _ := os.ReadFile(flushedPath(t, f))
	if strings.Contains(string(data), "user-secret") || !strings.Contains(string(data), "forbidden") {
		t.Fatalf("unsafe classification: %s", data)
	}
}

func TestWatchPaginatedInitialList(t *testing.T) {
	f := testLiveFile(t, 1<<20)
	c := fake.NewClientset()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	pages := 0
	c.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pages++
		opts := a.(k8stesting.ListActionImpl).GetListOptions()
		m := metav1.ListMeta{ResourceVersion: "42"}
		if pages == 1 {
			if opts.Continue != "" {
				t.Error("initial continue token")
			}
			m.Continue = "private-continuation"
		} else {
			if opts.Continue != "private-continuation" {
				t.Error("missing continuation")
			}
			if f.stats.Records != 1 {
				t.Error("previous page not accepted before next request")
			}
		}
		return true, &corev1.PodList{ListMeta: m, Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprint(pages), ResourceVersion: "40"}}}}, nil
	})
	w := watch.NewRaceFreeFake()
	c.PrependWatchReactor("pods", func(a k8stesting.Action) (bool, watch.Interface, error) {
		opts := a.(k8stesting.WatchActionImpl).GetListOptions()
		if opts.ResourceVersion != "42" || !opts.AllowWatchBookmarks {
			t.Errorf("watch not based on complete list: %+v", opts)
		}
		cancel()
		return true, w, nil
	})
	if err := recordWatch(ctx, c, "ns", "pods", f); err != nil {
		t.Fatal(err)
	}
	if pages != 2 || !w.IsStopped() {
		t.Fatal("pagination/watch cleanup incomplete")
	}
	data, _ := os.ReadFile(flushedPath(t, f))
	if strings.Contains(string(data), "private-continuation") {
		t.Fatal("continuation leaked")
	}
}

func TestRecordLiveLimitFailsWithoutStoppingPoll(t *testing.T) {
	t.Parallel()
	spec := testSpec(t)
	spec.EnableWatch, spec.MaxStreamBytes = true, 1
	c := fake.NewClientset()
	result, err := Record(t.Context(), c, spec)
	if err == nil || result.Shutdown != "error" || result.Samples != 4 || result.Live == nil || !result.Live.Incomplete || !result.Live.Watch.LimitReached {
		t.Fatalf("live limit suppressed polls or failure: %+v %v", result, err)
	}
	info, err := os.Stat(filepath.Join(spec.OutputDir, "watch.ndjson"))
	if err != nil || info.Size() > 1 {
		t.Fatalf("limit exceeded: %v %v", info, err)
	}
	summary := readJSON[LiveSummary](t, filepath.Join(spec.OutputDir, "stream-summary.json"))
	if !summary.Incomplete || summary.Watch.Errors == 0 {
		t.Fatal("limit failure not persisted")
	}
}

func TestRunStreamsCancellationAndInitialization(t *testing.T) {
	for _, scenario := range []string{"before-watch", "before-logs", "during-list", "during-watch-open", "after-init", "duration-before-init", "duration-after-init", "partial-init"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			spec := testSpec(t)
			spec.OutputDir = t.TempDir()
			spec.Namespaces = []string{"ns", "other"}
			spec.EnableWatch = scenario != "before-logs"
			spec.EnableLogs, spec.RawLogs = !spec.EnableWatch, !spec.EnableWatch
			ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
			defer cancel()
			if strings.HasPrefix(scenario, "duration-") {
				cancel()
				ctx, cancel = context.WithTimeoutCause(t.Context(), 150*time.Millisecond, errRecordingDuration)
				defer cancel()
			}
			if scenario == "duration-after-init" {
				cancel()
				ctx = t.Context() // Exercise RunStreams' own duration, not only an inherited deadline.
			}
			if strings.HasPrefix(scenario, "before-") {
				cancel()
			}
			c := fake.NewClientset()
			c.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if scenario == "during-list" || scenario == "duration-before-init" {
					<-ctx.Done()
					return true, nil, ctx.Err()
				}
				m := metav1.ListMeta{ResourceVersion: "10"}
				switch a.GetResource().Resource {
				case "pods":
					return true, &corev1.PodList{ListMeta: m}, nil
				case "events":
					return true, &corev1.EventList{ListMeta: m}, nil
				case "deployments":
					return true, &appsv1.DeploymentList{ListMeta: m}, nil
				default:
					return true, &appsv1.StatefulSetList{ListMeta: m}, nil
				}
			})
			var watchers []*watch.RaceFreeFakeWatcher
			c.PrependWatchReactor("*", func(a k8stesting.Action) (bool, watch.Interface, error) {
				if scenario == "during-watch-open" {
					<-ctx.Done()
					return true, nil, ctx.Err()
				}
				if scenario == "partial-init" && a.GetNamespace() == "other" && a.GetResource().Resource == "pods" {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "p", errors.New("private-error"))
				}
				w := watch.NewRaceFreeFake()
				watchers = append(watchers, w)
				return true, w, nil
			})
			started := time.Now()
			summary, err := RunStreams(ctx, c, spec)
			if time.Since(started) > 2*time.Second {
				t.Fatal("workers did not stop promptly")
			}
			wantInitialized := int64(0)
			if scenario == "after-init" || scenario == "duration-after-init" {
				wantInitialized = 8
			}
			if scenario == "partial-init" {
				wantInitialized = 7
			}
			if summary.Watch.Initialized != wantInitialized {
				t.Fatalf("initialization accounting: %+v", summary)
			}
			if spec.EnableWatch && summary.Watch.Required != 8 {
				t.Fatalf("required watches not counted: %+v", summary)
			}
			if scenario == "duration-after-init" {
				if err != nil || summary.Incomplete {
					t.Fatalf("expected duration failed: %+v %v", summary, err)
				}
			} else {
				if err == nil || !summary.Incomplete {
					t.Fatalf("interrupted recording falsely complete: %+v %v", summary, err)
				}
				if !strings.HasPrefix(scenario, "duration-") && !errors.Is(err, ctx.Err()) {
					t.Fatalf("parent error lost: %v", err)
				}
				if spec.EnableWatch && wantInitialized < 8 && !strings.Contains(err.Error(), "required watches did not initialize") {
					t.Fatalf("missing initialization not reported: %v", err)
				}
			}
			if strings.HasPrefix(scenario, "before-") && len(c.Actions()) != 0 {
				t.Fatal("pre-canceled recording contacted API")
			}
			for _, w := range watchers {
				if !w.IsStopped() {
					t.Fatal("watch leaked on cancellation")
				}
			}
			persisted := readJSON[LiveSummary](t, filepath.Join(spec.OutputDir, "stream-summary.json"))
			if !reflect.DeepEqual(persisted, summary) {
				t.Fatalf("persisted cancellation summary differs: %+v %+v", persisted, summary)
			}
		})
	}
}

func TestRecordLivePreCanceled(t *testing.T) {
	spec := testSpec(t)
	spec.EnableWatch, spec.EnableLogs, spec.RawLogs = true, true, true
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := fake.NewClientset()
	result, err := Record(ctx, c, spec)
	if !errors.Is(err, context.Canceled) || result.Shutdown != "canceled" || result.Live == nil || !result.Live.Incomplete || result.Live.Watch.Initialized != 0 || result.Live.Watch.Required != 4 {
		t.Fatalf("cancellation lost at Record boundary: %+v %v", result, err)
	}
	if len(c.Actions()) != 0 {
		t.Fatal("pre-canceled Record contacted API")
	}
	if persisted := readJSON[Result](t, filepath.Join(spec.OutputDir, "summary.json")); !reflect.DeepEqual(persisted, result) {
		t.Fatal("canceled main summary differs")
	}
}

func TestWatchPlannedRenewalResumesRV(t *testing.T) {
	f := testLiveFile(t, 1<<20)
	c := fake.NewClientset()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	lists, watches := 0, 0
	c.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists++
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}}, nil
	})
	var opened []*watch.RaceFreeFakeWatcher
	c.PrependWatchReactor("pods", func(a k8stesting.Action) (bool, watch.Interface, error) {
		watches++
		wantRV := "10"
		if watches == 2 {
			wantRV = "11"
			cancel()
		}
		if rv := a.(k8stesting.WatchActionImpl).GetWatchRestrictions().ResourceVersion; rv != wantRV {
			t.Errorf("renewal RV=%s, want %s", rv, wantRV)
		}
		w := watch.NewRaceFreeFake()
		opened = append(opened, w)
		if watches == 1 {
			w.Action(watch.Bookmark, &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "11"}})
		}
		return true, w, nil
	})
	if err := recordWatchWithRenewal(ctx, c, "ns", "pods", f, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if lists != 1 || watches != 2 || f.stats.Initialized != 1 || f.stats.Errors != 0 || f.stats.Gaps != 0 {
		t.Fatalf("renewal failed/relisted: lists=%d watches=%d %+v", lists, watches, f.stats)
	}
	for _, w := range opened {
		if !w.IsStopped() {
			t.Fatal("renewed watch not stopped")
		}
	}
	lines := readLines[StreamObservation](t, flushedPath(t, f))
	last := lines[len(lines)-1]
	if last.Type != "reconnect" || last.ObjectRV != "11" || last.Error.Reason != "watchRenewal" {
		t.Fatalf("missing renewal record: %+v", last)
	}
}

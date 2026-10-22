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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func testSpec(t *testing.T) Spec {
	t.Helper()
	return Spec{Version: "v1", Namespaces: []string{"future-hcp"}, IntervalSeconds: 5, DurationSeconds: 1, OutputDir: filepath.Join(t.TempDir(), "recording")}
}

func readJSON[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return result
}

func readLines[T any](t *testing.T, path string) []T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		t.Fatalf("unterminated record in %s", path)
	}
	var result []T
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	for {
		var value T
		err := decoder.Decode(&value)
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		result = append(result, value)
	}
}

func TestValidate(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"version":        func(s *Spec) { s.Version = "v2" },
		"no namespaces":  func(s *Spec) { s.Namespaces = nil },
		"all namespaces": func(s *Spec) { s.Namespaces = []string{""} },
		"duplicate":      func(s *Spec) { s.Namespaces = []string{"hcp", "hcp"} },
		"invalid DNS":    func(s *Spec) { s.Namespaces = []string{"UPPER"} },
		"subdomain":      func(s *Spec) { s.Namespaces = []string{"hcp.test"} },
		"too long":       func(s *Spec) { s.Namespaces = []string{strings.Repeat("a", 64)} },
		"short interval": func(s *Spec) { s.IntervalSeconds = 4 },
		"long interval":  func(s *Spec) { s.IntervalSeconds = 301 },
		"short duration": func(s *Spec) { s.DurationSeconds = 0 },
		"long duration":  func(s *Spec) { s.DurationSeconds = 86401 },
		"no output":      func(s *Spec) { s.OutputDir = "" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := testSpec(t)
			output := spec.OutputDir
			mutate(&spec)
			client := fake.NewClientset()
			if _, err := Record(context.Background(), client, spec); err == nil {
				t.Fatal("invalid spec succeeded")
			}
			if len(client.Actions()) != 0 {
				t.Fatal("invalid spec made API calls")
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid spec created output: %v", err)
			}
		})
	}
	spec := testSpec(t)
	spec.IntervalSeconds, spec.DurationSeconds = 300, 86400
	if err := spec.Validate(); err != nil {
		t.Fatalf("valid upper bounds: %v", err)
	}
}

func TestOutputDirectorySafety(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink", "missing parent"} {
		t.Run(kind, func(t *testing.T) {
			spec := testSpec(t)
			switch kind {
			case "directory":
				if err := os.Mkdir(spec.OutputDir, 0700); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(spec.OutputDir, []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), spec.OutputDir); err != nil {
					t.Fatal(err)
				}
			case "missing parent":
				spec.OutputDir = filepath.Join(spec.OutputDir, "child")
			}
			client := fake.NewClientset()
			if _, err := Record(context.Background(), client, spec); err == nil {
				t.Fatal("unsafe output directory accepted")
			}
			if len(client.Actions()) != 0 {
				t.Fatal("unsafe output made API calls")
			}
			if kind == "file" {
				data, err := os.ReadFile(spec.OutputDir)
				if err != nil || string(data) != "untouched" {
					t.Fatalf("existing file changed: %q, %v", data, err)
				}
			}
		})
	}
}

func TestImmediatePollDurationAndPermissions(t *testing.T) {
	t.Parallel()
	spec := testSpec(t)
	client := fake.NewClientset() // Explicit namespace need not exist yet.
	var firstCall time.Time
	client.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if firstCall.IsZero() {
			firstCall = time.Now()
		}
		if action.GetNamespace() != "future-hcp" {
			t.Errorf("unexpected namespace %q", action.GetNamespace())
		}
		if options := action.(k8stesting.ListActionImpl).GetListOptions(); options.Limit != 500 {
			t.Errorf("page limit = %d", options.Limit)
		}
		return false, nil, nil
	})
	started := time.Now()
	result, err := Record(context.Background(), client, spec)
	if err != nil {
		t.Fatal(err)
	}
	if firstCall.Sub(started) > 750*time.Millisecond {
		t.Fatalf("first poll was delayed: %v", firstCall.Sub(started))
	}
	if elapsed := time.Since(started); elapsed < time.Second || elapsed > 3*time.Second {
		t.Fatalf("duration bound: %v", elapsed)
	}
	if result.Polls != 1 || result.Samples != 4 || result.APIErrors != 0 || result.Shutdown != "duration" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(client.Actions()) != 4 {
		t.Fatalf("unexpected calls: %+v", client.Actions())
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "list" {
			t.Fatalf("non-read action: %v", action)
		}
	}
	info, err := os.Stat(spec.OutputDir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory permissions: %v %v", info, err)
	}
	files, err := os.ReadDir(spec.OutputDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 7 {
		t.Fatalf("unexpected artifacts: %v", files)
	}
	var bytes int64
	for _, file := range files {
		info, err := file.Info()
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("file permissions: %v %v", info, err)
		}
		if file.Name() != "checkpoints.ndjson" && strings.HasSuffix(file.Name(), ".ndjson") {
			bytes += info.Size()
		}
	}
	if bytes != result.ObservationBytes {
		t.Fatalf("bytes on disk %d != reported %d", bytes, result.ObservationBytes)
	}
	summary := readJSON[Result](t, filepath.Join(spec.OutputDir, "summary.json"))
	if !reflect.DeepEqual(summary, result) {
		t.Fatalf("summary differs: %+v != %+v", summary, result)
	}
	checkpoints := readLines[Checkpoint](t, filepath.Join(spec.OutputDir, "checkpoints.ndjson"))
	if len(checkpoints) != 2 || checkpoints[0].Type != "poll" || checkpoints[1].Type != "shutdown" {
		t.Fatalf("bad checkpoints: %+v", checkpoints)
	}
	if checkpoints[0].PollFinishedAt.Before(checkpoints[0].PollStartedAt) || checkpoints[0].ScheduledAt.IsZero() {
		t.Fatalf("missing poll timing: %+v", checkpoints[0])
	}
	for _, stream := range result.Streams {
		if stream.LastSuccess == nil || stream.Samples != 1 {
			t.Fatalf("missing success checkpoint: %+v", stream)
		}
		lines := readLines[Observation](t, filepath.Join(spec.OutputDir, stream.Resource+".ndjson"))
		if len(lines) != 2 || lines[0].Type != "page" || lines[1].Type != "complete" {
			t.Fatalf("empty snapshot missing markers: %+v", lines)
		}
	}
}

func TestCanceledFinalizes(t *testing.T) {
	t.Parallel()
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "before first poll", false: "during poll"}[before], func(t *testing.T) {
			spec := testSpec(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := fake.NewClientset()
			if before {
				cancel()
			} else {
				client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { cancel(); return false, nil, nil })
			}
			result, err := Record(ctx, client, spec)
			if !errors.Is(err, context.Canceled) || result.Shutdown != "canceled" {
				t.Fatalf("cancellation: %+v %v", result, err)
			}
			if before && len(client.Actions()) != 0 {
				t.Fatal("canceled context made calls")
			}
			summary := readJSON[Result](t, filepath.Join(spec.OutputDir, "summary.json"))
			if summary.Shutdown != "canceled" || summary.EndedAt.IsZero() {
				t.Fatalf("missing terminal summary: %+v", summary)
			}
			lines := readLines[Checkpoint](t, filepath.Join(spec.OutputDir, "checkpoints.ndjson"))
			if lines[len(lines)-1].Type != "shutdown" {
				t.Fatal("no terminal checkpoint")
			}
		})
	}
}

func TestAPIErrorsContinueAndFail(t *testing.T) {
	t.Parallel()
	spec := testSpec(t)
	client := fake.NewClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "secret-body", errors.New("password=secret-body"))
	})
	client.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "future-hcp")
	})
	result, err := Record(context.Background(), client, spec)
	if err == nil || strings.Contains(err.Error(), "secret-body") {
		t.Fatalf("unsafe or missing error: %v", err)
	}
	if result.APIErrors != 2 || result.Samples != 2 || result.IncompleteStreams != 2 || result.Shutdown != "error" {
		t.Fatalf("wrong failure summary: %+v", result)
	}
	if len(client.Actions()) != 4 {
		t.Fatal("API failure stopped other streams")
	}
	lines := readLines[Observation](t, filepath.Join(spec.OutputDir, "pods.ndjson"))
	if len(lines) != 1 || lines[0].Type != "apiError" || lines[0].Error.Reason != "forbidden" || lines[0].Error.Code != 403 {
		t.Fatalf("missing structured failure: %+v", lines)
	}
	files, _ := os.ReadDir(spec.OutputDir)
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(spec.OutputDir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "secret-body") {
			t.Fatalf("error body leaked into %s", file.Name())
		}
	}
}

func TestPaginationAndPartialFailure(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{true: "page failure", false: "complete"}[fail], func(t *testing.T) {
			spec := testSpec(t)
			client := fake.NewClientset()
			calls := 0
			client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				calls++
				options := action.(k8stesting.ListActionImpl).GetListOptions()
				if options.Limit != 500 {
					t.Errorf("unbounded page: %+v", options)
				}
				if calls == 1 {
					if options.Continue != "" {
						t.Error("first page has continue token")
					}
					return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "private-token"}, Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "future-hcp"}}}}, nil
				}
				if options.Continue != "private-token" {
					t.Errorf("lost continue token: %+v", options)
				}
				// The previous page must already be written before fetching this one.
				lines := readLines[Observation](t, filepath.Join(spec.OutputDir, "pods.ndjson"))
				if len(lines) != 1 || lines[0].Type != "page" {
					t.Errorf("page not incremental: %+v", lines)
				}
				if fail {
					return true, nil, apierrors.NewResourceExpired("private-token")
				}
				return true, &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "future-hcp"}}}}, nil
			})
			result, err := Record(context.Background(), client, spec)
			if (err != nil) != fail || calls != 2 {
				t.Fatalf("pagination: calls=%d err=%v", calls, err)
			}
			lines := readLines[Observation](t, filepath.Join(spec.OutputDir, "pods.ndjson"))
			if fail {
				if len(lines) != 2 || lines[1].Type != "apiError" || result.Streams[0].LastSuccess != nil || result.Streams[0].Samples != 0 {
					t.Fatalf("partial page claimed complete: %+v %+v", lines, result)
				}
			} else if len(lines) != 3 || lines[2].Type != "complete" || result.Streams[0].Entries != 2 || result.Streams[0].Pages != 2 {
				t.Fatalf("missing pagination results: %+v %+v", lines, result)
			}
			data, _ := os.ReadFile(filepath.Join(spec.OutputDir, "pods.ndjson"))
			if strings.Contains(string(data), "private-token") {
				t.Fatal("continue token leaked")
			}
		})
	}
}

func TestMissingNamespaceRetriedAndCheckpointsIncremental(t *testing.T) {
	t.Parallel()
	spec := testSpec(t)
	spec.DurationSeconds = 6
	client := fake.NewClientset()
	calls := 0
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "future-hcp")
		}
		lines := readLines[Checkpoint](t, filepath.Join(spec.OutputDir, "checkpoints.ndjson"))
		if len(lines) != 1 || lines[0].Result.APIErrors != 1 || lines[0].Result.Streams[0].LastSuccess != nil {
			t.Errorf("missing incremental checkpoint: %+v", lines)
		}
		return false, nil, nil
	})
	result, err := Record(context.Background(), client, spec)
	if err == nil || calls != 2 || result.Polls != 2 || result.APIErrors != 1 || result.Samples != 7 || result.IncompleteStreams != 0 {
		t.Fatalf("retry result: %+v %v calls=%d", result, err, calls)
	}
	if result.Streams[0].LastSuccess == nil || result.Streams[0].APIErrors != 1 {
		t.Fatalf("recovery lost history: %+v", result.Streams[0])
	}
	lines := readLines[Checkpoint](t, filepath.Join(spec.OutputDir, "checkpoints.ndjson"))
	if len(lines) != 3 || lines[1].ScheduledAt.Sub(lines[0].ScheduledAt) != 5*time.Second {
		t.Fatalf("poll cadence changed: %+v", lines)
	}
}

func TestAPICallBoundedByRemainingDuration(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "500" || r.URL.Path != "/api/v1/namespaces/future-hcp/pods" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec(t)
	started := time.Now()
	result, err := Record(context.Background(), client, spec)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("API call exceeded duration: %v", elapsed)
	}
	if err == nil || result.Samples != 0 || result.IncompleteStreams != 4 {
		t.Fatalf("deadline falsely successful: %+v %v", result, err)
	}
	lines := readLines[Observation](t, filepath.Join(spec.OutputDir, "pods.ndjson"))
	if len(lines) != 1 || lines[0].Type != "interrupted" {
		t.Fatalf("missing interrupted snapshot: %+v", lines)
	}
}

func TestSlowPollSkipsMissedTicks(t *testing.T) {
	t.Parallel()
	spec := testSpec(t)
	spec.DurationSeconds = 6
	client := fake.NewClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		time.Sleep(5100 * time.Millisecond)
		return false, nil, nil
	})
	result, err := Record(context.Background(), client, spec)
	if err != nil || result.Polls != 1 || result.Samples != 4 {
		t.Fatalf("slow poll replayed missed tick: %+v %v", result, err)
	}
	lines := readLines[Checkpoint](t, filepath.Join(spec.OutputDir, "checkpoints.ndjson"))
	if lines[0].SkippedTicks != 1 || lines[0].DurationMillis < 5100 {
		t.Fatalf("slow poll timing hidden: %+v", lines[0])
	}
}

func TestObservationByteBoundAndWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if n, err := appendObservation(f, Observation{}, MaxObservationBytes-1, MaxObservationBytes); err == nil || n != 0 {
		t.Fatalf("byte bound failed: %d %v", n, err)
	}
	info, _ := f.Stat()
	if info.Size() != 0 {
		t.Fatal("limit left partial record")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := appendObservation(f, Observation{}, 0, MaxObservationBytes); err == nil {
		t.Fatal("write failure ignored")
	}
}

func TestObservationConfiguredBudget(t *testing.T) {
	for _, limit := range []int64{1, 1 << 30, 8 << 30} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := testLiveFile(t, limit).f
			data, err := json.Marshal(Observation{})
			if err != nil {
				t.Fatal(err)
			}
			size := int64(len(data) + 1)
			if limit >= size {
				if n, err := appendObservation(f, Observation{}, limit-size, limit); err != nil || n != size {
					t.Fatalf("exact boundary rejected: %d %v", n, err)
				}
			}
			if n, err := appendObservation(f, Observation{}, limit-1, limit); err == nil || n != 0 {
				t.Fatalf("limit not enforced: %d %v", n, err)
			}
		})
	}
	spec := testSpec(t)
	spec.MaxObservationBytes = 1
	result, err := Record(t.Context(), fake.NewClientset(), spec)
	if err == nil || result.ObservationBytes != 0 || result.Shutdown != "error" {
		t.Fatalf("configured poll budget ignored: %+v %v", result, err)
	}
	manifest := readJSON[struct {
		Spec                Spec  `json:"spec"`
		MaxObservationBytes int64 `json:"maxObservationBytes"`
	}](t, filepath.Join(spec.OutputDir, "manifest.json"))
	if manifest.MaxObservationBytes != 1 || manifest.Spec.MaxObservationBytes != 1 || manifest.Spec.MaxConcurrentLogs != 256 || manifest.Spec.MaxStreamBytes != 1<<30 || manifest.Spec.LogRequestQPS != 20 || manifest.Spec.LogRequestBurst != 40 {
		t.Fatalf("resolved limits not persisted: %+v", manifest)
	}
}

func TestFinalizationFailureReturnsPartialResult(t *testing.T) {
	t.Parallel()
	spec := testSpec(t)
	client := fake.NewClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		// Simulate a filesystem failure during summary creation, without a
		// process-wide umask change or relying on non-root permissions.
		if err := os.Mkdir(filepath.Join(spec.OutputDir, "summary.json"), 0700); err != nil {
			t.Fatal(err)
		}
		return false, nil, nil
	})
	result, err := Record(context.Background(), client, spec)
	if err == nil || result.Shutdown != "error" || result.Samples != 4 || result.EndedAt.IsZero() {
		t.Fatalf("storage failure lost partial result: %+v %v", result, err)
	}
	lines := readLines[Checkpoint](t, filepath.Join(spec.OutputDir, "checkpoints.ndjson"))
	if lines[len(lines)-1].Type != "shutdown" {
		t.Fatal("summary failure lost shutdown checkpoint")
	}
}

func TestMultipleExplicitNamespaces(t *testing.T) {
	t.Parallel()
	spec := testSpec(t)
	spec.Namespaces = []string{"future-hcp", "management"}
	client := fake.NewClientset()
	result, err := Record(context.Background(), client, spec)
	if err != nil || result.Samples != 8 || len(result.Streams) != 8 {
		t.Fatalf("namespace streams: %+v %v", result, err)
	}
	for _, resource := range resources {
		lines := readLines[Observation](t, filepath.Join(spec.OutputDir, resource+".ndjson"))
		if len(lines) != 4 || lines[0].Namespace != "future-hcp" || lines[2].Namespace != "management" {
			t.Fatalf("namespace separation: %+v", lines)
		}
	}
}

func TestSanitizedOperationalSnapshots(t *testing.T) {
	t.Parallel()
	const secret = "do-not-persist-user-password"
	spec := testSpec(t)
	m := metav1.ObjectMeta{Name: "operational", Namespace: "future-hcp", UID: "pod-uid", Generation: 3,
		Labels: map[string]string{"private": secret}, Annotations: map[string]string{"private": secret},
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: secret}}}
	c := corev1.Container{Name: "api", Image: "registry/api:v1", Command: []string{"sh", "-c", secret}, Args: []string{secret},
		Env:       []corev1.EnvVar{{Name: "PASSWORD", Value: secret}, {Name: "REF", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: secret}}}},
		EnvFrom:   []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: secret}}}},
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}}
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	p := &corev1.Pod{ObjectMeta: m, Spec: corev1.PodSpec{NodeName: "worker", Containers: []corev1.Container{c}, InitContainers: []corev1.Container{c},
		EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: c.Image, Command: c.Command, Args: c.Args, Env: c.Env, EnvFrom: c.EnvFrom}}},
		ImagePullSecrets:    []corev1.LocalObjectReference{{Name: secret}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Message: secret,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady", Message: secret}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "api", RestartCount: 2, ContainerID: secret,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: secret}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 7, Message: secret, ContainerID: secret, FinishedAt: now}}}}}}
	p.Status.InitContainerStatuses, p.Status.EphemeralContainerStatuses = p.Status.ContainerStatuses, p.Status.ContainerStatuses
	replicas := int32(2)
	d := &appsv1.Deployment{ObjectMeta: m, Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{ObjectMeta: m, Spec: p.Spec}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 2, ReadyReplicas: 1, Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Reason: "MinimumReplicasUnavailable", Message: secret}}}}
	s := &appsv1.StatefulSet{ObjectMeta: m, Spec: appsv1.StatefulSetSpec{Replicas: &replicas, Template: d.Spec.Template},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 3, Replicas: 2, ReadyReplicas: 1, Conditions: []appsv1.StatefulSetCondition{{Type: "Ready", Reason: "Pending", Message: secret}}}}
	e := &corev1.Event{ObjectMeta: m, Message: secret, Reason: "BackOff", Count: 4, LastTimestamp: now,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: m.Namespace, Name: m.Name, UID: m.UID, FieldPath: secret},
		Series:         &corev1.EventSeries{Count: 5, LastObservedTime: metav1.NewMicroTime(now.Time)}}
	client := fake.NewClientset(p, d, s, e, &corev1.Secret{ObjectMeta: m, Data: map[string][]byte{"password": []byte(secret)}})
	if _, err := Record(context.Background(), client, spec); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(spec.OutputDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(spec.OutputDir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("secret leaked into %s", file.Name())
		}
	}
	lines := readLines[Observation](t, filepath.Join(spec.OutputDir, "pods.ndjson"))
	data, _ := json.Marshal(lines[0].Entries[0])
	var got pod
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Metadata.UID != "pod-uid" || got.NodeName != "worker" || got.Containers[0].Image != c.Image || got.Containers[0].Requests.Cpu().MilliValue() != 100 || got.ContainerStatuses[0].RestartCount != 2 || got.ContainerStatuses[0].LastTerminationState.ExitCode != 7 {
		t.Fatalf("lost operational fields: %+v", got)
	}
	for _, forbidden := range []string{"\"env\"", "\"args\"", "\"command\"", "\"annotations\"", "\"labels\"", "\"message\""} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("forbidden field %s", forbidden)
		}
	}
	for _, resource := range []string{"deployments", "statefulsets"} {
		lines := readLines[Observation](t, filepath.Join(spec.OutputDir, resource+".ndjson"))
		data, _ := json.Marshal(lines[0].Entries[0])
		var got workload
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got.DesiredReplicas == nil || *got.DesiredReplicas != 2 || got.ObservedGeneration != 3 || got.ReadyReplicas != 1 || len(got.Conditions) != 1 {
			t.Fatalf("lost %s operational fields: %+v", resource, got)
		}
	}
	events := readLines[Observation](t, filepath.Join(spec.OutputDir, "events.ndjson"))
	data, _ = json.Marshal(events[0].Entries[0])
	var gotEvent event
	if err := json.Unmarshal(data, &gotEvent); err != nil {
		t.Fatal(err)
	}
	if gotEvent.Reason != "BackOff" || gotEvent.Count != 4 || gotEvent.SeriesCount != 5 || gotEvent.InvolvedObject.UID != m.UID || !gotEvent.LastTimestamp.Equal(&now) {
		t.Fatalf("lost event operational fields: %+v", gotEvent)
	}
}

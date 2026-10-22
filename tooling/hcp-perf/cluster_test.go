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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/capture"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/record"
)

func clusterKubeconfig(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	config := clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"fake": {Server: server.URL}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"fake": {}},
		Contexts:       map[string]*clientcmdapi.Context{"fake": {Cluster: "fake", AuthInfo: "fake"}},
		CurrentContext: "fake",
	}
	if err := clientcmd.WriteToFile(config, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func clusterInput(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClusterCaptureStartAndStatus(t *testing.T) {
	var spec capture.Spec
	if err := decodeFile("examples/capture.json", &spec); err != nil {
		t.Fatal(err)
	}
	spec.ID = "cli-capture"
	spec.Image = "registry.example.test/prometheus@sha256:" + strings.Repeat("a", 64)
	spec.KubernetesDiscovery = true
	namespace := "hcp-perf-" + spec.ID
	corePath := "/api/v1/namespaces/" + namespace + "/"
	appsPath := "/apis/apps/v1/namespaces/" + namespace + "/"
	rbacPath := "/apis/rbac.authorization.k8s.io/v1/"
	objects := map[string]runtime.Object{
		"/api/v1/namespaces":                &corev1.Namespace{},
		corePath + "serviceaccounts":        &corev1.ServiceAccount{},
		corePath + "configmaps":             &corev1.ConfigMap{},
		corePath + "persistentvolumeclaims": &corev1.PersistentVolumeClaim{},
		corePath + "services":               &corev1.Service{},
		appsPath + "statefulsets":           &appsv1.StatefulSet{},
		rbacPath + "clusterroles":           &rbacv1.ClusterRole{},
		rbacPath + "clusterrolebindings":    &rbacv1.ClusterRoleBinding{},
	}
	var mu sync.Mutex
	created := map[string]runtime.Object{}
	reads := 0
	kubeconfig := clusterKubeconfig(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.UserAgent() != "hcp-perf/v1" {
			t.Errorf("unexpected user agent %q", r.UserAgent())
		}
		var response runtime.Object
		switch r.Method {
		case http.MethodPost:
			response = objects[r.URL.Path]
			if response == nil {
				break
			}
			// The real typed client may send protobuf, even though responses are JSON.
			data, err := io.ReadAll(r.Body)
			if err == nil {
				err = runtime.DecodeInto(scheme.Codecs.UniversalDeserializer(), data, response)
			}
			if err != nil {
				t.Errorf("decode %s: %v", r.URL.Path, err)
				http.Error(w, "invalid object", http.StatusBadRequest)
				return
			}
			object, err := meta.Accessor(response)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			path := r.URL.Path + "/" + object.GetName()
			if _, exists := created[path]; exists {
				t.Errorf("duplicate create %s", path)
				w.WriteHeader(http.StatusConflict)
				return
			}
			object.SetUID(types.UID("uid-" + object.GetName() + "-" + response.GetObjectKind().GroupVersionKind().Kind))
			created[path] = response
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			reads++
			response = created[r.URL.Path]
			switch r.URL.Path {
			case corePath + "persistentvolumeclaims":
				if !strings.Contains(r.URL.Query().Get("labelSelector"), capture.SessionIDLabel+"="+spec.ID) {
					t.Errorf("PVC list lacks session selector: %s", r.URL)
				}
				response = &corev1.PersistentVolumeClaimList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaimList"}, Items: []corev1.PersistentVolumeClaim{*objects[corePath+"persistentvolumeclaims"].(*corev1.PersistentVolumeClaim)}}
			case corePath + "pods":
				response = &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}}
			case corePath + "events":
				response = &corev1.EventList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EventList"}}
			}
		}
		if response == nil {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	})
	var out bytes.Buffer
	args := []string{"capture", "start", "--input", clusterInput(t, spec), "--config", "examples/prometheus.yaml", "--kubeconfig", kubeconfig, "--wait=false", "--timeout=10s"}
	if err := run(t.Context(), args, &out); err != nil {
		t.Fatalf("capture start: %v; output: %s", err, &out)
	}
	mu.Lock()
	if reads != 0 || len(created) != len(objects) {
		t.Errorf("wait=false: got %d reads and %d creates, want 0 and %d", reads, len(created), len(objects))
	}
	mu.Unlock()
	ns := objects["/api/v1/namespaces"].(*corev1.Namespace)
	if ns.Name != namespace || ns.Labels[capture.SessionIDLabel] != spec.ID {
		t.Errorf("wrong namespace: %+v", ns.ObjectMeta)
	}
	sts := objects[appsPath+"statefulsets"].(*appsv1.StatefulSet)
	pod := sts.Spec.Template.Spec
	if sts.Namespace != namespace || pod.NodeSelector["aro-hcp.azure.com/role"] != "infra" || len(pod.Containers) != 1 {
		t.Fatalf("wrong collector placement: %+v", sts.Spec)
	}
	container := pod.Containers[0]
	if container.Image != spec.Image || container.Resources.Requests.Cpu().MilliValue() != spec.CPURequestMilli {
		t.Errorf("input image/CPU not propagated: %+v", container)
	}
	pvc := objects[corePath+"persistentvolumeclaims"].(*corev1.PersistentVolumeClaim)
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != spec.StorageClass || pvc.Spec.Resources.Requests.Storage().Value() != spec.StorageGi*1024*1024*1024 {
		t.Errorf("input storage not propagated: %+v", pvc.Spec)
	}
	cm := objects[corePath+"configmaps"].(*corev1.ConfigMap)
	config, err := os.ReadFile("examples/prometheus.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["prometheus.yml"] != string(config) {
		t.Error("--config contents not propagated")
	}
	var started capture.SessionStatus
	if err := json.Unmarshal(out.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Version != "v1" || started.ID != spec.ID || started.Namespace != namespace || started.Phase != capture.PhasePending || len(started.Inventory) != len(objects) {
		t.Fatalf("unexpected start JSON: %s", &out)
	}
	for _, ref := range started.Inventory {
		if ref.UID == "" {
			t.Errorf("missing server UID in inventory: %+v", ref)
		}
	}
	out.Reset()
	if err := run(t.Context(), []string{"capture", "status", "--id", spec.ID, "--kubeconfig", kubeconfig, "--timeout=10s"}, &out); err != nil {
		t.Fatalf("capture status: %v; output: %s", err, &out)
	}
	var status capture.SessionStatus
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.ID != spec.ID || status.Namespace != namespace || status.Phase != capture.PhasePending || status.DesiredReplicas != 1 || len(status.Inventory) != len(started.Inventory) || len(status.Missing) != 0 {
		t.Fatalf("unexpected status JSON: %s", &out)
	}
}

func TestClusterInvalidArgumentsDoNotCallAPI(t *testing.T) {
	var calls atomic.Int64
	kubeconfig := clusterKubeconfig(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected API call", http.StatusBadRequest)
	})
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing subcommand", []string{"capture"}, "requires a subcommand"},
		{"unknown subcommand", []string{"capture", "unknown"}, "unknown command"},
		{"unknown flag", []string{"capture", "status", "--wat"}, "flag provided but not defined"},
		{"missing flag value", []string{"capture", "status", "--id"}, "flag needs an argument"},
		{"positional", []string{"capture", "status", "extra"}, "unexpected positional"},
		{"irrelevant flag", []string{"capture", "status", "--wait=false"}, "does not apply"},
		{"worker timeout", []string{"record", "worker", "--timeout=1s"}, "--timeout does not apply"},
		{"malformed timeout", []string{"capture", "status", "--timeout=bad"}, "invalid value"},
		{"zero timeout", []string{"capture", "status", "--timeout=0s"}, "timeout must be positive"},
		{"negative timeout", []string{"capture", "status", "--timeout=-1s"}, "timeout must be positive"},
		{"excessive timeout", []string{"capture", "status", "--timeout=25h"}, "at most 24h"},
		{"missing capture ID", []string{"capture", "status"}, "capture ID"},
		{"invalid capture ID", []string{"capture", "status", "--id=INVALID"}, "capture ID"},
		{"cleanup consent", []string{"capture", "cleanup", "--id=example"}, "deleteEvidence=true"},
		{"missing capture input", []string{"capture", "start"}, "file path is required"},
		{"missing capture config", []string{"capture", "start", "--input=examples/capture.json"}, "no such file"},
		{"missing adversary input", []string{"adversaries", "start"}, "file path is required"},
		{"missing record input", []string{"record", "start"}, "file path is required"},
		{"missing worker input", []string{"record", "worker"}, "file path is required"},
		{"missing record identity", []string{"record", "status"}, "namespace must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if len(tc.args) >= 2 {
				args = append([]string{}, tc.args[:2]...)
				args = append(args, "--kubeconfig", kubeconfig)
				args = append(args, tc.args[2:]...)
			}
			err := run(t.Context(), args, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run(%v) = %v, want %q", args, err, tc.want)
			}
			if n := calls.Load(); n != 0 {
				t.Fatalf("invalid arguments caused %d API requests", n)
			}
		})
	}
	var spec record.SessionSpec
	if err := decodeFile("examples/record.json", &spec); err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("record example is not valid: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"missing field", func(s map[string]any) { delete(s, "durationSeconds") }, "required field is missing"},
		{"unknown field", func(s map[string]any) { s["unexpected"] = true }, "unknown field"},
		{"invalid duration", func(s map[string]any) { s["durationSeconds"] = 0 }, "durationSeconds must be"},
	} {
		t.Run("record start "+tc.name, func(t *testing.T) {
			data, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			var input map[string]any
			if err := json.Unmarshal(data, &input); err != nil {
				t.Fatal(err)
			}
			tc.mutate(input)
			err = run(t.Context(), []string{"record", "start", "--input", clusterInput(t, input), "--kubeconfig", kubeconfig}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if n := calls.Load(); n != 0 {
				t.Fatalf("invalid record input caused %d API requests", n)
			}
		})
	}
}

func TestClusterCredentials(t *testing.T) {
	var calls atomic.Int64
	kubeconfig := clusterKubeconfig(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected API call", http.StatusBadRequest)
	})
	t.Setenv("KUBECONFIG", kubeconfig)
	for _, name := range []string{"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT"} {
		t.Setenv(name, "unused") // Register restoration before removing the variable entirely.
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing-kubeconfig")
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"explicit missing file", []string{"--kubeconfig", missing}, missing},
		{"in-cluster only", nil, "unable to load in-cluster configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(t.Context(), append([]string{"capture", "status", "--id=example"}, tc.flags...), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "load management credentials") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want credential error containing %q", err, tc.want)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("credentials fell back to KUBECONFIG: %d API requests", n)
	}
}

func TestClusterCPUBurnCanceledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	err := run(ctx, []string{"cpu", "burn", "--workers=1", "--duration=1s"}, &out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context cancellation", err)
	}
	var result map[string]string
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, map[string]string{"version": "v1", "status": "failed"}) {
		t.Fatalf("unexpected CPU result: %s", &out)
	}
}

func TestClusterRecordWorkerUsesSpecDuration(t *testing.T) {
	var calls atomic.Int64
	kubeconfig := clusterKubeconfig(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Close connections so idle network goroutines do not hold synctest's clock.
		w.Header().Set("Connection", "close")
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Query().Get("watch") != "" || r.URL.Query().Get("limit") != "500" {
			t.Errorf("expected paginated polling, got %s %s", r.Method, r.URL)
		}
		var response runtime.Object
		switch r.URL.Path {
		case "/api/v1/namespaces/future-hcp/pods":
			response = &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}}
		case "/api/v1/namespaces/future-hcp/events":
			response = &corev1.EventList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EventList"}}
		case "/apis/apps/v1/namespaces/future-hcp/deployments":
			response = &appsv1.DeploymentList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"}}
		case "/apis/apps/v1/namespaces/future-hcp/statefulsets":
			response = &appsv1.StatefulSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSetList"}}
		default:
			t.Errorf("unexpected worker request: %s", r.URL)
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	})
	spec := record.Spec{Version: "v1", Namespaces: []string{"future-hcp"}, IntervalSeconds: 300, DurationSeconds: 601, OutputDir: filepath.Join(t.TempDir(), "observations")}
	input := clusterInput(t, spec)
	// Advance virtual time past the cluster CLI's five-minute default, not wall time.
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		if err := run(t.Context(), []string{"record", "worker", "--input", input, "--kubeconfig", kubeconfig}, &out); err != nil {
			t.Fatalf("record worker: %v; output: %s", err, &out)
		}
		var result record.Result
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Shutdown != "duration" || result.EndedAt.Sub(result.StartedAt) != 601*time.Second || result.Polls != 3 || result.Samples != 12 || result.APIErrors != 0 {
			t.Fatalf("worker did not honor spec duration: %s", &out)
		}
		data, err := os.ReadFile(filepath.Join(spec.OutputDir, "summary.json"))
		if err != nil {
			t.Fatal(err)
		}
		var summary record.Result
		if err := json.Unmarshal(data, &summary); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(summary, result) {
			t.Error("stdout does not match persisted worker summary")
		}
		data, err = os.ReadFile(filepath.Join(spec.OutputDir, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest struct {
			Spec record.Spec `json:"spec"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.Spec.LogRequestQPS != 20 || manifest.Spec.LogRequestBurst != 40 {
			t.Fatal("CLI worker did not persist resolved log request budgets")
		}
	})
	if n := calls.Load(); n != 12 {
		t.Fatalf("got %d API requests, want three polls of four resource types", n)
	}
}

func TestClusterRecordWorkerRejectsInvalidBudgetBeforeCredentials(t *testing.T) {
	spec := record.Spec{Version: "v1", Namespaces: []string{"ns"}, IntervalSeconds: 5, DurationSeconds: 30, OutputDir: filepath.Join(t.TempDir(), "observations"), LogRequestQPS: 101}
	err := run(t.Context(), []string{"record", "worker", "--input", clusterInput(t, spec), "--kubeconfig", "/missing-kubeconfig"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "logRequestQPS") {
		t.Fatalf("invalid budget did not fail before credential loading: %v", err)
	}
}

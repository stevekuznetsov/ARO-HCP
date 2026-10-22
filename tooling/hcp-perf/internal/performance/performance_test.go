// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package performance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func testSpec() Spec {
	s := Spec{Version: "v1", GuestKubeconfig: "/does-not-exist/guest", MeasurementSeconds: 3600, SetupSeconds: 1800, RecoverySeconds: 30, CanaryCadenceSeconds: 30, MaxOutstanding: 1,
		Seed:    seed.Spec{Version: "v1", ID: "unique", NamespacePrefix: "fresh", GuestNodes: 3, Applications: 1, Image: workload.CanaryImage, PerPodCPU: "5m", PerPodMemory: "8Mi", TimeoutSeconds: 300, Binary: seed.Binary{Path: "/external/kube-burner", ExpectedSHA256: strings.Repeat("a", 64)}},
		Startup: startup.Spec{Kubeconfig: "/does-not-exist/management", ResourceID: "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/customer/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/cluster", CandidateName: "cluster", CandidateSpec: json.RawMessage(`{"fixed":true}`), MaxDuration: time.Hour, PollInterval: 5 * time.Second, HealthyWindow: 5 * time.Minute},
		Capture: Capture{Endpoint: "http://127.0.0.1:1", Reference: "external-owner-1", MaxAgeSeconds: 45, Sources: []Source{{Job: "kas", Replicas: 3}}, Queries: []metrics.Query{{Name: "up", Expr: "up"}}},
	}
	for i := range 3 {
		s.ExpectedGuestNodes = append(s.ExpectedGuestNodes, GuestNodeIdentity{Name: fmt.Sprintf("node-%d", i), UID: fmt.Sprintf("node-uid-%d", i)})
	}
	for _, name := range []string{"kube-apiserver", "openshift-apiserver", "packageserver", "etcd"} {
		kind := "Deployment"
		if name == "etcd" {
			kind = "StatefulSet"
		}
		resources := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}
		s.Startup.Components = append(s.Startup.Components, startup.Component{Kind: kind, Name: name, Replicas: 3, Endpoints: name, ReadyEndpoints: 3, Containers: []startup.Container{{Name: name, Resources: corev1.ResourceRequirements{Requests: resources, Limits: resources}}}})
	}
	return s
}

func TestValidateBoundedPure(t *testing.T) {
	s := testSpec()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.API = &apiload.Spec{Version: "v1", DurationSeconds: 3600, RatePerSecond: 277, MaxInFlight: 10, RequestTimeoutSeconds: 10, Namespaces: []string{"fresh-0"}, Targets: []apiload.Target{{Namespace: "fresh-0", APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Name: "app-000000", Weight: 1}}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s)
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("validation mutated input")
	}
	s.API.RatePerSecond = 278
	if s.Validate() == nil {
		t.Fatal("accepted over one million offers")
	}
	s.API.RatePerSecond = 277
	s.API.Targets[0].Name = "not-owned"
	if s.Validate() == nil {
		t.Fatal("accepted unrelated measured target")
	}
	s.API = nil
	s.RecoverySeconds = 601
	if s.Validate() == nil {
		t.Fatal("accepted unbounded recovery")
	}
}

func TestCapacityAggregateAndPerNode(t *testing.T) {
	s := testSpec()
	s.Seed.Applications = 10
	nodes := []corev1.Node{}
	for i := range 3 {
		nodes = append(nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", i), UID: types.UID(s.ExpectedGuestNodes[i].UID), Labels: map[string]string{corev1.LabelOSStable: "linux"}}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("110"), corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}})
	}
	if _, err := capacity(nodes, nil, s); err != nil {
		t.Fatal(err)
	}
	nodes[0].Status.Allocatable[corev1.ResourcePods] = resource.MustParse("10")
	if _, err := capacity(nodes, nil, s); err == nil {
		t.Fatal("aggregate capacity hid low-slot node")
	}
	nodes[0].Status.Allocatable[corev1.ResourcePods] = resource.MustParse("110")
	pods := []corev1.Pod{{Spec: corev1.PodSpec{NodeName: "node-0", Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}}}}}}}
	if _, err := capacity(nodes, pods, s); err == nil {
		t.Fatal("ignored platform requests")
	}
	nodes[0].Status.Conditions = nil
	if _, err := capacity(nodes, nil, s); err == nil {
		t.Fatal("accepted nonready guest node")
	}
}

func TestCaptureReplicasAndSourceAge(t *testing.T) {
	for _, mode := range []string{"healthy", "stale", "down", "missing", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stamp := strings.HasPrefix(r.URL.Query().Get("query"), "timestamp(")
				results := []any{}
				count := 3
				if mode == "missing" {
					count = 2
				}
				for i := range count {
					value := 1.0
					if mode == "down" {
						value = 0
					}
					if stamp {
						value = float64(time.Now().Unix())
						if mode == "stale" {
							value -= 90
						}
					}
					labels := map[string]string{"job": "kas", "instance": fmt.Sprint(i)}
					if mode == "duplicate" {
						labels["instance"] = "0"
					}
					if !stamp {
						labels["__name__"] = "up"
					}
					results = append(results, map[string]any{"metric": labels, "value": []any{time.Now().Unix(), fmt.Sprint(value)}})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": results}})
			}))
			defer server.Close()
			capture := testSpec().Capture
			capture.Endpoint = server.URL
			if err := captureReady(t.Context(), server.Client(), capture); (err == nil) != (mode == "healthy") {
				t.Fatalf("mode %s: %v", mode, err)
			}
		})
	}
}

func TestRunSetupFailureFinalizesAndNeverReusesOutput(t *testing.T) {
	exports := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exports++
		http.Error(w, "capture gap", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	s := testSpec()
	s.Capture.Endpoint = server.URL
	dir := filepath.Join(t.TempDir(), "case")
	out, err := Run(t.Context(), s, dir, server.Client())
	if err == nil || out.State != "failed" || exports != 1 || out.MetricsStatus != "partial-or-failed" {
		t.Fatalf("missing finalization: %+v %v exports=%d", out, err, exports)
	}
	before, err := os.ReadFile(filepath.Join(dir, "outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), s, dir, server.Client()); err == nil {
		t.Fatal("reused interrupted/failed case")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "outcome.json"))
	if string(before) != string(after) || exports != 1 {
		t.Fatal("modified original outcome")
	}
	for _, name := range []string{"spec.json", "status.json", "outcome.json", "events.ndjson"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("nonprivate %s", name)
		}
	}
}

func TestCanceledWaitAndHourlySchedule(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if wait(ctx, time.Hour) == nil {
		t.Fatal("wait ignored cancellation")
	}
	hour, err := experiment.RolloutsForPeriod(100, 42, time.Hour, time.Hour)
	if err != nil || len(hour) != 100 {
		t.Fatalf("hourly schedule: %d %v", len(hour), err)
	}
	short, err := experiment.RolloutsForPeriod(100, 42, 5*time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i, intent := range short {
		if intent != hour[i] || intent.Offset >= 5*time.Minute {
			t.Fatal("short case accelerated turnover")
		}
	}
}

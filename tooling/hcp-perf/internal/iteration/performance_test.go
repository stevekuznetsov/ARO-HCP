// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iteration

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/performance"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
	startupapi "github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func TestPerformanceTypedMeasurementContract(t *testing.T) {
	startup := startupIterationSpec().Actions[2].Startup
	p := &performance.Spec{Version: "v1", Startup: *startup, GuestKubeconfig: "/external/guest", ExpectedGuestNodes: []performance.GuestNodeIdentity{{Name: "node-0", UID: "uid-0"}, {Name: "node-1", UID: "uid-1"}, {Name: "node-2", UID: "uid-2"}}, Seed: seed.Spec{Version: "v1", ID: "fresh", NamespacePrefix: "fresh", GuestNodes: 3, Applications: 0, Image: workload.CanaryImage, PerPodCPU: "5m", PerPodMemory: "8Mi", TimeoutSeconds: 300, Binary: seed.Binary{Path: "/external/burner", ExpectedSHA256: strings.Repeat("a", 64)}}, MeasurementSeconds: 300, SetupSeconds: 1800, RecoverySeconds: 30, CanaryCadenceSeconds: 30, MaxOutstanding: 1, Capture: performance.Capture{Endpoint: "http://127.0.0.1:9090", Reference: "external", MaxAgeSeconds: 45, Sources: []performance.Source{{Job: "kas", Replicas: 3}}, Queries: []metrics.Query{{Name: "up", Expr: "up"}}}}
	a := Action{ID: "performance", Kind: PerformanceRun, Recovery: Measurement, Timeout: 2 * time.Hour, MaxAttempts: 1, Performance: p}
	if err := a.validate(); err != nil {
		t.Fatal(err)
	}
	p.Seed.Applications = 1
	p.API = &apiload.Spec{Version: "v1", DurationSeconds: 300, RatePerSecond: 1, MaxInFlight: 1, RequestTimeoutSeconds: 10, Namespaces: []string{"fresh-0"}, Targets: []apiload.Target{{Namespace: "fresh-0", APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Name: "app-000000", Weight: 1}}}
	p.RequestQualityPolicy = &performance.RequestQualityPolicy{MaxDegradedBasisPoints: 500}
	if err := a.validate(); err != nil {
		t.Fatalf("typed request quality policy rejected: %v", err)
	}
	p.Capture.JobTiming = map[string]performance.JobTiming{"kas": {Interval: time.Minute, Timeout: 45 * time.Second, MaxAge: 165 * time.Second}}
	if err := a.validate(); err != nil {
		t.Fatalf("typed performance rejected per-job KAS timing: %v", err)
	}
	image := "quay.io/test/catalog@sha256:" + strings.Repeat("b", 64)
	container := startupapi.Container{Name: "registry", Image: image, Resources: p.Startup.Components[0].Containers[0].Resources}
	p.Startup.Components = append(p.Startup.Components, startupapi.Component{Kind: "Deployment", Name: "community-operators-catalog", Replicas: 1, Containers: []startupapi.Container{container}})
	p.ExistingPopulation = &performance.PopulationReference{Directory: "/previous", OutcomeSHA256: strings.Repeat("a", 64), InventorySHA256: strings.Repeat("b", 64), ImageBindings: []performance.PopulationImageBinding{{Kind: "Deployment", Component: "community-operators-catalog", Container: "registry", PreviousImage: "quay.io/test/catalog@sha256:" + strings.Repeat("a", 64), Image: image}}}
	p.ExistingPopulation.ScreenedPopulation = &performance.ScreenedPopulationProof{SpecSHA256: strings.Repeat("c", 64), ResultsSHA256: strings.Repeat("d", 64)}
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "assessed-failure", true: "uncertain-cleanup"}[uncertain], func(t *testing.T) {
			c := testController(t, executorFunc(func(_ context.Context, action Action, _, _ string) error {
				if !reflect.DeepEqual(action.Performance.ExistingPopulation, p.ExistingPopulation) {
					t.Fatal("frozen worker lost reviewed image binding provenance")
				}
				violation := &performance.WorkloadViolation{Violations: []string{"API workload unusable"}}
				if uncertain {
					return violation
				}
				return &performance.AssessedFailure{Err: violation}
			}))
			spec := testSpec()
			spec.Actions = []Action{a}
			token := submit(t, c, spec)
			loaded, err := c.loadSpec(status(t, c))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.Actions[0].Performance.ExistingPopulation, p.ExistingPopulation) {
				t.Fatal("controller journal decoder lost reviewed image binding provenance")
			}
			if err := c.Work(t.Context(), token); err != nil {
				t.Fatal(err)
			}
			want := Failed
			if uncertain {
				want = Blocked
			}
			if got := status(t, c); got.State != want || got.Actions[0].State != want {
				t.Fatalf("outcome should be %s: %+v", want, got)
			}
			if err := c.Reconcile(t.Context(), "run"); err != nil {
				t.Fatal(err)
			}
			if len(status(t, c).Actions[0].Attempts) != 1 {
				t.Fatal("performance replayed")
			}
		})
	}
	a.MaxAttempts = 2
	if a.validate() == nil {
		t.Fatal("measurement retry accepted")
	}
	a.MaxAttempts = 1
	a.Recovery = ReadOnly
	if a.validate() == nil {
		t.Fatal("read-only performance accepted")
	}
	a.Recovery = Measurement
	a.Timeout = p.MaxDuration()
	if a.validate() == nil {
		t.Fatal("missing finalization budget accepted")
	}
	a.Timeout = 2 * time.Hour
	a.Startup = startup
	if a.validate() == nil {
		t.Fatal("contradictory typed families accepted")
	}
}

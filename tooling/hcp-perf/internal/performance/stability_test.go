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
	"errors"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestCriticalAvailabilityBound(t *testing.T) {
	start := time.Now()
	var summary StabilitySummary
	for _, point := range []struct {
		after time.Duration
		ready int
	}{{0, 3}, {time.Second, 1}, {29 * time.Second, 1}, {30 * time.Second, 2}} {
		if err := summary.availability("etcd", point.ready, start.Add(point.after)); err != nil {
			t.Fatal(err)
		}
	}
	if summary.Critical["etcd"].Minimum != 1 || summary.Critical["etcd"].Maximum != 3 || summary.Critical["etcd"].OutageSeconds != 29 {
		t.Fatalf("missing outage evidence: %+v", summary)
	}
	if err := summary.availability("etcd", 1, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := summary.availability("etcd", 2, start.Add(91*time.Second)); err == nil {
		t.Fatal("late recovery erased critical outage")
	}
}

func TestCanaryDeadlineOnly(t *testing.T) {
	for _, err := range []error{errors.Join(context.DeadlineExceeded, errors.New("owner replaced")), errors.Join(context.DeadlineExceeded, apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "canary", errors.New("denied"))), context.Canceled} {
		if canaryDeadline(err) {
			t.Fatalf("unsafe error treated as deadline: %v", err)
		}
	}
	if !canaryDeadline(errors.Join(context.DeadlineExceeded, context.DeadlineExceeded)) {
		t.Fatal("deadline not recoverable")
	}
	if !turnoverOnlyFailure(errors.Join(&workload.TurnoverFailure{Failed: 1})) {
		t.Fatal("workload debt not distinguished")
	}
	if turnoverOnlyFailure(errors.Join(&workload.TurnoverFailure{Failed: 1}, errors.New("disk failure"))) {
		t.Fatal("evidence failure ignored")
	}
}

func TestStabilityPolicyValidation(t *testing.T) {
	spec := testSpec()
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	spec.StabilityPolicy = "observe-degradation"
	if spec.Validate() == nil {
		t.Fatal("missing critical telemetry policy accepted")
	}
	spec.Capture.LossGraceSeconds, spec.Capture.RecoveryGateSeconds = 60, 180
	spec.Capture.CriticalJobs = []string{"kas"}
	spec.Capture.SourceRoles = map[string]string{"kas": "kas", "etcd": "etcd", "container": "cadvisor", "node": "node-exporter", "kubelet": "kubelet"}
	for _, job := range []string{"etcd", "cadvisor", "node-exporter", "kubelet"} {
		spec.Capture.Sources = append(spec.Capture.Sources, Source{Job: job, Replicas: 1})
		spec.Capture.CriticalJobs = append(spec.Capture.CriticalJobs, job)
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	spec.Capture.SourceRoles["node"] = "kas"
	if spec.Validate() == nil {
		t.Fatal("one job accepted for multiple roles")
	}
	spec.Capture.SourceRoles["node"] = "node-exporter"
	delete(spec.Capture.SourceRoles, "kubelet")
	if spec.Validate() == nil {
		t.Fatal("omitted mandatory telemetry role accepted")
	}
	spec.Capture.SourceRoles["kubelet"] = "kubelet"
	spec.Capture.CriticalJobs = []string{"unknown"}
	if spec.Validate() == nil {
		t.Fatal("unknown critical job accepted")
	}
	spec.StabilityPolicy = "strict"
	if spec.Validate() == nil {
		t.Fatal("strict mode silently relaxed capture")
	}
}

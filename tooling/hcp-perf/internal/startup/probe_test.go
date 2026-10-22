// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package startup

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestProbePolicyAvailabilityAndStrict(t *testing.T) {
	for _, policy := range []ProbeFailurePolicy{"", StrictProbes, Availability} {
		t.Run(string(policy), func(t *testing.T) {
			spec := Spec{ProbeFailurePolicy: policy}
			history := healthHistory{probeEvents: map[string]int32{}}
			start := time.Now()
			var sample Sample
			for i := range 73 {
				sample = Sample{At: start.Add(time.Duration(i) * 15 * time.Second)}
				event := corev1.Event{ObjectMeta: metav1.ObjectMeta{UID: "probe"}, Reason: "Unhealthy", Count: int32(i + 1), Message: "Readiness probe failed: internal detail not persisted"}
				observeProbeEvent(event, spec.EffectiveProbeFailurePolicy(), &sample, &history)
				qualify(&sample, &history, 5*time.Minute, true)
				if sample.NewProbeFailures != 1 {
					t.Fatal("incremental probe count lost")
				}
			}
			if policy == Availability {
				if !sample.HealthyWindow || !sample.FreshQualified || len(sample.Warnings) != 1 || len(sample.Issues) != 0 || !history.probeDegraded {
					t.Fatalf("probe warnings incorrectly reset availability: %+v", sample)
				}
			} else if sample.HealthyWindow || sample.FreshQualified || len(sample.Issues) != 1 {
				t.Fatalf("legacy strict behavior changed: %+v", sample)
			}
		})
	}
}

func TestAvailabilityStillRejectsHardFailures(t *testing.T) {
	for _, failure := range []string{"probe-kill", "unready", "restart", "oom", "interrupted"} {
		t.Run(failure, func(t *testing.T) {
			history := healthHistory{probeEvents: map[string]int32{}, healthySince: time.Now().Add(-10 * time.Minute)}
			sample := Sample{At: time.Now()}
			switch failure {
			case "probe-kill":
				event := corev1.Event{ObjectMeta: metav1.ObjectMeta{UID: "kill"}, Reason: "Killing", Count: 1, Message: "Container worker failed liveness probe, will be restarted"}
				observeProbeEvent(event, Availability, &sample, &history)
				observeProbeEvent(event, Availability, &sample, &history)
				if sample.NewProbeKills != 1 {
					t.Fatal("probe kill was missed or counted twice")
				}
			case "unready":
				sample.Issues = []string{"observed pods not ready: Deployment/kube-apiserver"}
			case "restart":
				sample.NewRestarts = 1
			case "oom":
				sample.NewOOMs = 1
			case "interrupted":
				history.violated = true
			}
			qualify(&sample, &history, 5*time.Minute, true)
			if failure != "interrupted" && sample.HealthyWindow {
				t.Fatal("hard failure did not reset availability window")
			}
			if failure != "unready" && sample.FreshQualified {
				t.Fatal("hard failure/interruption incorrectly freshly qualified")
			}
		})
	}
}

func TestAvailabilityCompletesBeforeMaximumDeadline(t *testing.T) {
	for _, scenario := range []string{"healthy", "warning", "strict", "incomplete", "restart", "oom", "kill", "issue", "short-window"} {
		t.Run(scenario, func(t *testing.T) {
			spec := Spec{ProbeFailurePolicy: Availability}
			sample := Sample{Healthy: true, HealthyWindow: true, Coverage: &Coverage{Complete: true}}
			switch scenario {
			case "warning":
				sample.NewProbeFailures = 1
				sample.Warnings = []string{"new probe failure event"}
			case "strict":
				spec.ProbeFailurePolicy = StrictProbes
			case "incomplete":
				sample.Coverage.Complete = false
			case "restart":
				sample.NewRestarts = 1
			case "oom":
				sample.NewOOMs = 1
			case "kill":
				sample.NewProbeKills = 1
			case "issue":
				sample.Issues = []string{"Kubernetes health inspection failed"}
			case "short-window":
				sample.HealthyWindow = false
			}
			if got, want := availabilityComplete(spec, sample), scenario == "healthy" || scenario == "warning"; got != want {
				t.Fatalf("completion=%v, want %v", got, want)
			}
		})
	}
}

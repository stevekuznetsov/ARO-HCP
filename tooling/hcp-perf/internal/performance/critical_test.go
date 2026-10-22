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
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func TestCriticalSampleWholeCycleDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	sample := criticalSample(ctx, fake.NewClientset(), testSpec(), "control", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !sample.Unknown || time.Since(started) > time.Second {
		t.Fatalf("unbounded critical cycle: %+v", sample)
	}
}

func TestCriticalWatchdogNotBlockedByJournal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	release := make(chan struct{})
	failure := make(chan error, 1)
	done := make(chan struct{})
	monitor := &criticalMonitor{}
	go func() {
		defer close(done)
		monitor.run(ctx, 20*time.Millisecond, 10*time.Millisecond, 60*time.Millisecond, func(context.Context) CriticalSample {
			return CriticalSample{Started: time.Now(), Finished: time.Now(), Ready: map[string]int{"kube-apiserver": 3, "etcd": 3, "authenticated-guest-livez": 2}}
		}, func(CriticalSample) error { <-release; return nil }, func(err error) {
			select {
			case failure <- err:
			default:
			}
		})
	}()
	select {
	case <-failure:
	case <-ctx.Done():
		t.Error("journal blocked watchdog")
	}
	close(release)
	cancel()
	<-done
}

func TestCriticalMonitorDeadlineAndHeartbeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	monitor := &criticalMonitor{}
	var calls atomic.Int32
	failures := make(chan error, 1)
	monitor.run(ctx, 20*time.Millisecond, 10*time.Millisecond, 70*time.Millisecond, func(ctx context.Context) CriticalSample {
		calls.Add(1)
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing whole-cycle deadline")
		}
		<-ctx.Done()
		return CriticalSample{Started: time.Now(), Finished: time.Now(), Unknown: true}
	}, func(CriticalSample) error { return nil }, func(err error) {
		select {
		case failures <- err:
		default:
		}
	})
	if calls.Load() < 2 {
		t.Fatal("deadline prevented recurring evaluations")
	}
	select {
	case <-failures:
	default:
		t.Fatal("missing stale-heartbeat failure")
	}
	if !monitor.status.LastSuccessfulEvaluation.IsZero() || monitor.status.UnknownSamples == 0 {
		t.Fatal("unknown cycle renewed heartbeat")
	}
}

func TestCriticalMonitorIndependentOfSlowEvidence(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var calls atomic.Int32
	monitor := &criticalMonitor{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitor.run(ctx, 20*time.Millisecond, 10*time.Millisecond, 80*time.Millisecond, func(context.Context) CriticalSample {
			calls.Add(1)
			return CriticalSample{Started: time.Now(), Finished: time.Now(), Ready: map[string]int{"kube-apiserver": 3, "etcd": 3, "authenticated-guest-livez": 2}}
		}, func(CriticalSample) error { return nil }, func(err error) { t.Error(err); cancel() })
	}()
	// Models a capture worker blocked in an independent API call. Its lifetime
	// cannot serialize the critical loop or heartbeat.
	time.Sleep(110 * time.Millisecond)
	cancel()
	<-done
	if calls.Load() < 4 || monitor.status.LastSuccessfulEvaluation.IsZero() {
		t.Fatalf("critical polling stalled behind capture: %d", calls.Load())
	}
}

func TestObservationAxesPreserveWorkloadResult(t *testing.T) {
	for _, kind := range []string{"probe", "capture"} {
		t.Run(kind, func(t *testing.T) {
			out := Outcome{Workload: Assessment{Status: "met-not-qualified", PerformanceStatus: "within-expectations"}, RecordStatus: "recorded-not-lossless", MetricsStatus: "exported-not-qualified"}
			if kind == "probe" {
				out.Stability.ProbeFailures = 1
			} else {
				out.CaptureHistory.Incomplete = true
			}
			reportObservation(&out)
			if !out.Degraded || out.Workload.PerformanceStatus != "degraded" || len(out.Workload.Warnings) == 0 {
				t.Fatal("warnings reported within expectations")
			}
			if out.Workload.Status != "met-not-qualified" || len(out.Workload.Violations) != 0 {
				t.Fatal("observation warning changed workload acceptance")
			}
		})
	}
}

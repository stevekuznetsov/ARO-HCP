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
	"sync"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
)

type CriticalSample struct {
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished"`
	Ready    map[string]int `json:"ready"`
	Unknown  bool           `json:"unknown"`
}

type CriticalStatus struct {
	LastSuccessfulEvaluation time.Time               `json:"lastSuccessfulEvaluation"`
	Samples                  int                     `json:"samples"`
	UnknownSamples           int                     `json:"unknownSamples"`
	Availability             map[string]Availability `json:"availability"`
}

type criticalMonitor struct {
	mu     sync.Mutex
	status CriticalStatus
}

func criticalSample(ctx context.Context, client kubernetes.Interface, spec Spec, namespace string, livez func(context.Context) error) CriticalSample {
	sample := CriticalSample{Started: time.Now(), Ready: map[string]int{}}
	type result struct {
		name  string
		ready int
		err   error
	}
	results := make(chan result, 3)
	for _, name := range []string{"kube-apiserver", "etcd", "authenticated-guest-livez"} {
		go func(name string) {
			defer utilruntime.HandleCrash()
			var ready int
			var err error
			if name == "authenticated-guest-livez" {
				err = livez(ctx)
				if err == nil {
					ready = 2
				}
			} else {
				ready, err = endpointReady(ctx, client, spec, namespace, name)
			}
			results <- result{name, ready, err}
		}(name)
	}
	for range 3 {
		select {
		case <-ctx.Done():
			sample.Unknown = true
			sample.Finished = time.Now()
			return sample
		case result := <-results:
			if result.err != nil {
				sample.Unknown = true
			} else {
				sample.Ready[result.name] = result.ready
			}
		}
	}
	sample.Unknown = sample.Unknown || ctx.Err() != nil
	sample.Finished = time.Now()
	return sample
}

// Production uses a 5s ticker, 4s cycle deadline and 15s evidence-gap bound.
// Smaller test timings exercise the same loop. Sample functions must honor ctx.
// The watchdog is independent of API requests, journaling and all other workers.
func (m *criticalMonitor) run(ctx context.Context, interval, deadline, maxGap time.Duration, sample func(context.Context) CriticalSample, emit func(CriticalSample) error, fail func(error)) {
	defer utilruntime.HandleCrash()
	if interval <= 0 || interval > 5*time.Second || deadline <= 0 || deadline >= interval || maxGap <= interval || maxGap > 15*time.Second {
		fail(errors.New("invalid critical monitor bounds"))
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := time.Now()
	done := make(chan struct{})
	go func() {
		defer utilruntime.HandleCrash()
		defer close(done)
		ticker := time.NewTicker(min(interval, time.Second))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.mu.Lock()
				last := m.status.LastSuccessfulEvaluation
				m.mu.Unlock()
				if last.IsZero() {
					last = started
				}
				if time.Since(last) >= maxGap {
					fail(errors.New("critical availability evidence stale for watchdog bound"))
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	history := StabilitySummary{}
	for {
		cycle, stop := context.WithTimeout(ctx, deadline)
		observed := sample(cycle)
		observed.Unknown = observed.Unknown || cycle.Err() != nil || len(observed.Ready) != 3
		stop()
		if ctx.Err() != nil {
			return
		}
		var criticalErr error
		m.mu.Lock()
		m.status.Samples++
		if observed.Unknown {
			m.status.UnknownSamples++ // Do not reset known outages or heartbeat.
		} else {
			m.status.LastSuccessfulEvaluation = time.Now()
			for name, ready := range observed.Ready {
				criticalErr = errors.Join(criticalErr, history.availability(name, ready, observed.Finished))
			}
			m.status.Availability = history.Critical
		}
		m.mu.Unlock()
		if criticalErr != nil {
			fail(criticalErr)
		} // Cancel before potentially slow evidence I/O.
		if err := emit(observed); err != nil {
			fail(err)
			return
		}
		if criticalErr != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

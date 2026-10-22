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
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
)

type CaptureHistoryStatus struct {
	Warnings   int  `json:"warnings"`
	Recoveries int  `json:"recoveries"`
	Incomplete bool `json:"incomplete"`
}

type captureWitness struct {
	Target          string    `json:"target"`
	SourceAt        time.Time `json:"sourceAt"`
	Up              bool      `json:"up"`
	RecoverySeconds float64   `json:"recoverySeconds,omitempty"`
}

type captureTarget struct {
	processed     time.Time
	success       time.Time
	confirmations []time.Time
	failedAt      time.Time
}

// One tracker per case; identities bind on the first complete history response.
// No failed observation resets last success, and a genuine expired gap latches
// before a newer success can hide it. Query evaluation time never renews a lease.
type captureTracker struct {
	sync.Mutex
	jobs      map[string]map[string]*captureTarget
	armed     bool
	fatal     error
	status    CaptureHistoryStatus
	observing bool
}

func (t *captureTracker) gate(ctx context.Context, client *http.Client, capture Capture, j *journal) error {
	if capture.LossGraceSeconds == 0 {
		return captureReady(ctx, client, capture)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(capture.RecoveryGateSeconds)*time.Second)
	defer cancel()
	for {
		ready, err := t.check(ctx, client, capture, j)
		if err != nil {
			return err
		}
		if ready {
			t.Lock()
			t.armed = true
			t.Unlock()
			return nil
		}
		if err := wait(ctx, 5*time.Second); err != nil {
			return errors.Join(errors.New("capture recovery gate lacks two fresh successes per current target"), err)
		}
	}
}

func (t *captureTracker) check(ctx context.Context, client *http.Client, capture Capture, j *journal) (bool, error) {
	if capture.LossGraceSeconds == 0 {
		err := captureReady(ctx, client, capture)
		return err == nil, err
	}
	t.Lock()
	defer t.Unlock()
	if t.fatal != nil {
		return false, t.fatal
	}
	if t.jobs == nil {
		t.jobs = map[string]map[string]*captureTarget{}
	}
	ready := true
	for _, source := range capture.Sources {
		critical := !t.observing
		for _, job := range capture.CriticalJobs {
			if job == source.Job {
				critical = true
			}
		}
		end := time.Now().Truncate(time.Millisecond)
		bound := max(time.Duration(capture.LossGraceSeconds)*time.Second, capture.maxAge(source.Job))
		horizon := ((2*bound + 5*time.Second - 1) / (5 * time.Second)) * (5 * time.Second)
		start := end.Add(-horizon)
		selector := "up{job=" + strconv.Quote(source.Job) + "}"
		up, upErr := metrics.Range(ctx, client, capture.Endpoint, selector, start, end, 5*time.Second)
		stamps, stampErr := metrics.Range(ctx, client, capture.Endpoint, "timestamp("+selector+")", start, end, 5*time.Second)
		var witnesses []captureWitness
		currentReady := false
		queryErr := errors.Join(upErr, stampErr)
		if queryErr == nil {
			currentReady, witnesses, t.fatal = t.observe(source, capture, up, stamps, end, time.Now())
		} else {
			t.status.Warnings++
			t.status.Incomplete = true
			for _, target := range t.jobs[source.Job] {
				target.confirmations = nil
			}
		}
		if t.armed {
			for _, target := range t.jobs[source.Job] {
				if target.success.IsZero() || time.Since(target.success) > bound {
					t.fatal = errors.New("capture sustained loss exceeded per-job bound")
				}
			}
		}
		if !currentReady {
			t.status.Incomplete = true
		}
		stopReason := ""
		if t.fatal != nil {
			stopReason = t.fatal.Error()
			t.status.Warnings++
			if !critical {
				// Preserve the bound inventory. A replacement ancillary series is
				// incomplete telemetry, not an automatically adopted healthy target.
				t.fatal = nil
			}
		}
		if err := j.event("capture-history", struct {
			Job              string               `json:"job"`
			Ready            bool                 `json:"ready"`
			QueryFailed      bool                 `json:"queryFailed"`
			LossBoundSeconds float64              `json:"lossBoundSeconds"`
			Witnesses        []captureWitness     `json:"witnesses"`
			Status           CaptureHistoryStatus `json:"status"`
			StopReason       string               `json:"stopReason,omitempty"`
		}{source.Job, currentReady, queryErr != nil, bound.Seconds(), witnesses, t.status, stopReason}); err != nil {
			return false, err
		}
		ready = ready && currentReady
		if t.fatal != nil {
			return false, t.fatal
		}
	}
	return ready, ctx.Err()
}

// observe pairs history by exact labels AND evaluation time, then deduplicates
// by source timestamp. A 5s grid can repeat a 60s scrape twelve times: still one
// witness. Missing/replaced replica identities fail closed, not auto-discovered.
func (t *captureTracker) observe(source Source, capture Capture, up, stamps []metrics.Point, end, now time.Time) (bool, []captureWitness, error) {
	type sampleKey struct {
		target string
		at     int64
	}
	paired := map[sampleKey]metrics.Point{}
	key := func(point metrics.Point) sampleKey {
		copied := map[string]string{}
		for name, value := range point.Labels {
			if name != "__name__" {
				copied[name] = value
			}
		}
		return sampleKey{labels.Set(copied).String(), point.At.UnixMilli()}
	}
	for _, point := range stamps {
		k := key(point)
		if _, exists := paired[k]; exists {
			return false, nil, errors.New("duplicate capture timestamp identity")
		}
		paired[k] = point
	}
	byTarget := map[string][]captureWitness{}
	current := map[string]captureWitness{}
	seen := map[sampleKey]bool{}
	for _, point := range up {
		k := key(point)
		stamp, ok := paired[k]
		if !ok || seen[k] || point.Labels["job"] != source.Job || (point.Value != 0 && point.Value != 1) {
			return false, nil, errors.New("capture history labels or samples mismatch")
		}
		seen[k] = true
		at := time.UnixMilli(int64(stamp.Value * 1000))
		if stamp.Value <= 0 || at.After(point.At) || point.At.After(end) {
			return false, nil, errors.New("capture history has invalid source time")
		}
		witness := captureWitness{Target: k.target, SourceAt: at, Up: point.Value == 1}
		byTarget[k.target] = append(byTarget[k.target], witness)
		if point.At.Equal(end) {
			current[k.target] = witness
		}
	}
	if len(seen) != len(paired) || len(current) != source.Replicas || len(byTarget) != source.Replicas {
		return false, nil, errors.New("capture history replica inventory incomplete")
	}
	targets := t.jobs[source.Job]
	if targets == nil {
		targets = map[string]*captureTarget{}
		for target := range current {
			targets[target] = &captureTarget{}
		}
		t.jobs[source.Job] = targets
	}
	if len(targets) != len(current) {
		return false, nil, errors.New("capture replica inventory changed")
	}
	for target := range current {
		if targets[target] == nil {
			return false, nil, errors.New("capture replica identity changed")
		}
	}
	bound := max(time.Duration(capture.LossGraceSeconds)*time.Second, capture.maxAge(source.Job))
	ready := true
	events := []captureWitness{}
	// Sorting makes durable evidence deterministic, including simultaneous scrapes.
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		target := targets[name]
		history := byTarget[name]
		sort.Slice(history, func(i, j int) bool { return history[i].SourceAt.Before(history[j].SourceAt) })
		for _, witness := range history {
			if witness.SourceAt.Equal(target.processed) && witness.Up != target.processed.Equal(target.success) {
				return false, events, errors.New("capture source timestamp has contradictory outcomes")
			}
			if !witness.SourceAt.After(target.processed) {
				continue
			}
			if t.armed && !target.success.IsZero() && witness.SourceAt.Sub(target.success) > bound {
				return false, events, fmt.Errorf("capture source outage exceeded bound for %s", name)
			}
			target.processed = witness.SourceAt
			if !witness.Up {
				target.confirmations = nil
				if target.failedAt.IsZero() {
					target.failedAt = witness.SourceAt
				}
				t.status.Warnings++
				t.status.Incomplete = true
			} else {
				target.success = witness.SourceAt
				target.confirmations = append(target.confirmations, witness.SourceAt)
				if len(target.confirmations) > 2 {
					target.confirmations = target.confirmations[len(target.confirmations)-2:]
				}
				if !target.failedAt.IsZero() {
					witness.RecoverySeconds = witness.SourceAt.Sub(target.failedAt).Seconds()
					target.failedAt = time.Time{}
					t.status.Recoveries++
				}
			}
			events = append(events, witness)
		}
		latest := current[name]
		if t.armed && (target.success.IsZero() || now.Sub(target.success) > bound) {
			return false, events, errors.New("capture sustained loss exceeded per-job bound")
		}
		confirmed := len(target.confirmations) == 2 && now.Sub(target.confirmations[0]) <= capture.maxAge(source.Job)
		ready = ready && latest.Up && now.Sub(latest.SourceAt) <= capture.maxAge(source.Job) && confirmed
	}
	return ready, events, nil
}

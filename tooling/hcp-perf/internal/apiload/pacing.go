// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apiload

import (
	"context"
	"time"

	"k8s.io/utils/clock"
)

// Pacing diagnoses generator loss without changing request-quality accounting.
// Queue occupancy is sampled at offers, not an exact continuous high-water mark.
type Pacing struct {
	Version               string        `json:"version"`
	MissedDeadline        int           `json:"missedDeadline"`
	SlotsFull             int           `json:"slotsFull"`
	CanceledOffers        int           `json:"canceledOffers"`
	Wakeups               int           `json:"wakeups"`
	EarlyWakeups          int           `json:"earlyWakeups"`
	MaxDueBatch           int           `json:"maxDueBatch"`
	MaxWakeLateness       time.Duration `json:"maxWakeLatenessNanoseconds"`
	MaxSlotsUsed          int           `json:"maxSlotsUsed"`
	MaxJobsQueued         int           `json:"maxJobsQueued"`
	MaxEventsQueued       int           `json:"maxEventsQueued"`
	MaxEvidenceSync       time.Duration `json:"maxEvidenceSyncNanoseconds"`
	MaxWorkerEvidenceWait time.Duration `json:"maxWorkerEvidenceWaitNanoseconds"`
}

// pace uses monotonic absolute deadlines, rounded up so a fractional nanosecond
// cannot wake before an offer is due. Unlike a truncated-period ticker, rounding
// does not accumulate across the run. offer must not block on workers or disk.
func pace(ctx context.Context, c clock.Clock, started time.Time, spec Spec, offer func(int, time.Time, time.Time, bool) bool) (Pacing, time.Time) {
	p := Pacing{Version: "absolute-deadline-v1"}
	rate := time.Duration(spec.RatePerSecond)
	window := time.Duration(spec.DurationSeconds) * time.Second
	total := spec.DurationSeconds * spec.RatePerSecond
	period := max(time.Second/rate, time.Millisecond)
	budget := int((period*rate + time.Second - 1) / time.Second)
	offered := 0
	stopped := started
	timer := c.NewTimer(0)
	defer timer.Stop()
	for offered < total {
		offset := (time.Duration(offered+1)*time.Second + rate - 1) / rate
		if rate > 1000 {
			// Retain the minimum 1ms quantum and the original per-wake cap.
			offset = (stopped.Sub(started)/time.Millisecond + 1) * time.Millisecond
		}
		deadline := started.Add(min(offset, window))
		timer.Stop()
		select {
		case <-timer.C():
		default:
		}
		timer.Reset(max(deadline.Sub(c.Now()), 0))
		select {
		case <-ctx.Done():
		case <-timer.C():
		}
		stopped = c.Now()
		p.Wakeups++
		if stopped.Before(deadline) && ctx.Err() == nil {
			p.EarlyWakeups++
		}
		p.MaxWakeLateness = max(p.MaxWakeLateness, stopped.Sub(deadline))
		due := int(min(max(stopped.Sub(started), 0), window) * rate / time.Second)
		p.MaxDueBatch = max(p.MaxDueBatch, due-offered)
		for offered < due {
			offered++
			canceled := ctx.Err() != nil
			eligible := !canceled && due-offered < budget
			scheduled := started.Add(time.Duration(offered) * time.Second / rate)
			if offer(offered, scheduled, stopped, eligible) {
				continue
			}
			switch {
			case canceled:
				p.CanceledOffers++
			case !eligible:
				p.MissedDeadline++
			default:
				p.SlotsFull++
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	return p, stopped
}

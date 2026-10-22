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
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
)

type immediateTransport struct{}

func (immediateTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
}

func TestPacingEvidenceBackpressure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "api")
	synctest.Test(t, func(t *testing.T) {
		blocked, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		spec := testSpec()
		spec.DurationSeconds, spec.RatePerSecond = 10, 30
		var result Result
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			result, err = Run(context.Background(), spec, Options{OutputDir: dir, Factory: func(string) (*rest.Config, error) {
				return &rest.Config{Host: "https://offline.test", Transport: immediateTransport{}}, nil
			}, syncEvidence: func() error { once.Do(func() { close(blocked); <-release }); return nil }})
		}()
		<-blocked
		time.Sleep(11 * time.Second)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("evidence was not blocked")
		default:
		}
		close(release)
		<-done
		if err != nil || result.Offered != 300 || result.Completed != result.Dispatched || result.GeneratorLimited == 0 || result.Pacing.SlotsFull != result.GeneratorLimited || result.Pacing.MissedDeadline != 0 || result.Pacing.MaxSlotsUsed != 4 || result.Pacing.MaxEventsQueued != 4 || result.Pacing.MaxEvidenceSync < 11*time.Second || result.Pacing.MaxWorkerEvidenceWait < 10*time.Second {
			t.Fatalf("backpressure hidden or unbounded: %+v err=%v", result, err)
		}
	})
}

// Synchronous fake clock: Reset advances to the deadline plus scripted host
// jitter. No sleeps, goroutine scheduling, wall clock, or external server needed.
type pacingClock struct {
	clock.Clock
	now     time.Time
	wake    int
	jitter  func(int) time.Duration
	onReset func()
}

func (c *pacingClock) Now() time.Time { return c.now }
func (c *pacingClock) NewTimer(time.Duration) clock.Timer {
	return &pacingTimer{c: c, ch: make(chan time.Time, 1)}
}

type pacingTimer struct {
	c  *pacingClock
	ch chan time.Time
}

func (t *pacingTimer) C() <-chan time.Time { return t.ch }
func (t *pacingTimer) Stop() bool          { return false }
func (t *pacingTimer) Reset(d time.Duration) bool {
	jitter := time.Duration(0)
	if t.c.jitter != nil {
		jitter = t.c.jitter(t.c.wake)
	}
	t.c.wake++
	t.c.now = t.c.now.Add(d + jitter)
	if t.c.onReset != nil {
		t.c.onReset()
	}
	t.ch <- t.c.now
	return false
}

func TestPacingHourPrecision(t *testing.T) {
	for _, rate := range []int{3, 30, 999, 1000, 1001, 10000} {
		t.Run(time.Duration(rate).String(), func(t *testing.T) {
			seconds := min(3600, MaxAttempts/rate)
			c := &pacingClock{now: time.Unix(0, 0), jitter: func(n int) time.Duration { return time.Duration(n%31) * time.Microsecond }}
			start := c.Now()
			spec := Spec{RatePerSecond: rate, DurationSeconds: seconds}
			count := 0
			p, stopped := pace(context.Background(), c, start, spec, func(seq int, scheduled, observed time.Time, eligible bool) bool {
				count++
				if seq != count || !eligible || observed.Before(scheduled) || scheduled.After(start.Add(time.Duration(seconds)*time.Second)) {
					t.Fatalf("bad offer %d eligible=%t scheduled=%v observed=%v", seq, eligible, scheduled, observed)
				}
				return true
			})
			if count != seconds*rate || p.MissedDeadline != 0 || p.SlotsFull != 0 || stopped.Sub(start) < time.Duration(seconds)*time.Second || stopped.Sub(start) > time.Duration(seconds)*time.Second+30*time.Microsecond {
				t.Fatalf("count=%d pacing=%+v elapsed=%v", count, p, stopped.Sub(start))
			}
		})
	}
}

func TestPacingTruncatedTickerRegression(t *testing.T) {
	// At 30 QPS the old period loses 1/3 ns/tick (36us/hour). Alternating
	// 10/100us wake lateness produces zero-due then two-due wakes without a stall.
	const rate = 30
	period := time.Second / rate
	offered, lost, zero := 0, 0, 0
	for tick := 1; tick <= 108001; tick++ {
		jitter := 100 * time.Microsecond
		if tick%2 == 0 {
			jitter = 10 * time.Microsecond
		}
		due := min(108000, int((time.Duration(tick)*period+jitter)*rate/time.Second))
		if due == offered {
			zero++
		}
		lost += max(0, due-offered-1)
		offered = due
	}
	if lost == 0 || zero == 0 || offered != 108000 {
		t.Fatalf("regression not reproduced: lost=%d zero=%d offered=%d", lost, zero, offered)
	}
	c := &pacingClock{now: time.Unix(0, 0), jitter: func(n int) time.Duration {
		if n%2 == 0 {
			return 10 * time.Microsecond
		}
		return 100 * time.Microsecond
	}}
	dispatched := 0
	p, _ := pace(context.Background(), c, c.Now(), Spec{RatePerSecond: rate, DurationSeconds: 3600}, func(_ int, _, _ time.Time, eligible bool) bool {
		if eligible {
			dispatched++
		}
		return eligible
	})
	if dispatched != 108000 || p.MissedDeadline != 0 {
		t.Fatalf("precision loss remains: dispatched=%d pacing=%+v", dispatched, p)
	}
}

func TestPacingStallSaturationAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		rate                              int
		stall                             time.Duration
		slots                             int
		cancel                            bool
		missed, full, dispatched, offered int
	}{
		{"stall30", 30, 500 * time.Millisecond, 128, false, 15, 0, 15, 30},
		{"stall10000", 10000, 499 * time.Millisecond, 10000, false, 4990, 0, 5010, 10000},
		{"queuedJobs", 30, 0, 2, false, 0, 28, 2, 30},
		{"stallAndFull", 30, 500 * time.Millisecond, 0, false, 15, 15, 0, 30},
		{"canceledBeforeDeadline", 30, 0, 128, true, 0, 0, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &pacingClock{now: time.Unix(0, 0), jitter: func(n int) time.Duration {
				if n == 0 {
					return tc.stall
				}
				return 0
			}}
			if tc.cancel {
				c.onReset = cancel
			}
			slots := make(chan struct{}, tc.slots)
			offered, dispatched := 0, 0
			p, _ := pace(ctx, c, c.Now(), Spec{RatePerSecond: tc.rate, DurationSeconds: 1}, func(seq int, _, _ time.Time, eligible bool) bool {
				offered++
				if seq != offered {
					t.Fatal("sequence lost")
				}
				if !eligible {
					return false
				}
				select {
				case slots <- struct{}{}:
					dispatched++
					return true
				default:
					return false
				}
			})
			if offered != tc.offered || dispatched != tc.dispatched || p.MissedDeadline != tc.missed || p.SlotsFull != tc.full || p.MissedDeadline+p.SlotsFull+p.CanceledOffers != offered-dispatched {
				t.Fatalf("offered=%d dispatched=%d pacing=%+v", offered, dispatched, p)
			}
		})
	}
}

func TestPacingEarlyWakeAndTerminalStall(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		c := &pacingClock{now: time.Unix(0, 0), jitter: func(n int) time.Duration {
			if n == 0 {
				if terminal {
					return 2 * time.Second
				}
				return -time.Nanosecond
			}
			return 0
		}}
		offered, dispatched := 0, 0
		p, stopped := pace(context.Background(), c, c.Now(), Spec{RatePerSecond: 30, DurationSeconds: 1}, func(_ int, _, _ time.Time, eligible bool) bool {
			offered++
			if eligible {
				dispatched++
			}
			return eligible
		})
		if offered != 30 {
			t.Fatal("duration extended or offers lost", offered)
		}
		if terminal {
			if dispatched != 1 || p.MissedDeadline != 29 {
				t.Fatalf("unbounded terminal catchup: %+v", p)
			}
		} else if dispatched != 30 || p.EarlyWakeups != 1 || !stopped.Equal(time.Unix(1, 0)) {
			t.Fatalf("early wake dropped offer: %+v", p)
		}
	}
}

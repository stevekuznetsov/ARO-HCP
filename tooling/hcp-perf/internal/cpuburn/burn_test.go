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

package cpuburn

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestRunCompletion(t *testing.T) {
	previous := runtime.GOMAXPROCS(0)
	start := time.Now()
	if err := Run(context.Background(), 2, time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < time.Second || elapsed > 5*time.Second {
		t.Fatalf("duration = %v, want bounded one-second burn", elapsed)
	}
	if got := runtime.GOMAXPROCS(0); got != previous {
		t.Fatalf("GOMAXPROCS = %d, want restored %d", got, previous)
	}
}

func TestRunCancellation(t *testing.T) {
	previous := runtime.GOMAXPROCS(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	start := time.Now()
	if err := Run(ctx, 4, 24*time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("workers did not join promptly")
	}
	if runtime.GOMAXPROCS(0) != previous {
		t.Fatal("GOMAXPROCS was not restored after joining workers")
	}
	// Returning also releases the process-wide run guard.
	if err := Run(context.Background(), 1, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestRunBounds(t *testing.T) {
	for _, tc := range []struct {
		workers  int
		duration time.Duration
	}{{0, time.Second}, {-1, time.Second}, {1025, time.Second}, {1, 0}, {1, time.Second - 1}, {1, 24*time.Hour + 1}} {
		if err := Run(context.Background(), tc.workers, tc.duration); err == nil {
			t.Errorf("accepted workers=%d duration=%v", tc.workers, tc.duration)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, workers := range []int{1, 1024} {
		if err := Run(ctx, workers, 24*time.Hour); !errors.Is(err, context.Canceled) {
			t.Errorf("valid bounds with canceled context: %v", err)
		}
	}
}

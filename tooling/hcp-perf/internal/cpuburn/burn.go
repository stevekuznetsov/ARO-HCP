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

// Package cpuburn implements a bounded, continuously runnable CPU workload.
package cpuburn

import (
	"context"
	"crypto/sha256"
	"fmt"
	"runtime"
	"sync"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

var running sync.Mutex

// Run burns CPU until duration expires or ctx is canceled, and joins all workers.
// Expiration is successful completion; parent cancellation returns ctx.Err().
// Call only from the dedicated burner command: GOMAXPROCS is process-wide.
// The caller must supply a signal-aware context to handle SIGTERM.
func Run(ctx context.Context, workers int, duration time.Duration) error {
	if workers < 1 || workers > 1024 {
		return fmt.Errorf("workers must be between 1 and 1024")
	}
	if duration < time.Second || duration > 24*time.Hour {
		return fmt.Errorf("duration must be between 1s and 24h")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !running.TryLock() {
		return fmt.Errorf("CPU burner is already running in this process")
	}
	defer running.Unlock()
	previous := runtime.GOMAXPROCS(workers)
	defer runtime.GOMAXPROCS(previous)
	bounded, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	var wg sync.WaitGroup
	var panicked bool
	var panicMu sync.Mutex
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(seed int) {
			defer utilruntime.HandleCrash()
			defer wg.Done()
			completed := false
			defer func() {
				if !completed {
					panicMu.Lock()
					panicked = true
					panicMu.Unlock()
					cancel()
				}
			}()
			var sink [32]byte
			sink[0], sink[1] = byte(seed), byte(seed>>8)
			for bounded.Err() == nil {
				// Each worker owns its sink: no shared cache-line contention or
				// sleeps. Chunking bounds cancellation latency without timing a duty cycle.
				for chunk := 0; chunk < 256; chunk++ {
					sink = sha256.Sum256(sink[:])
				}
			}
			runtime.KeepAlive(sink)
			completed = true
		}(worker)
	}
	wg.Wait()
	if panicked {
		return fmt.Errorf("CPU worker panicked")
	}
	return ctx.Err()
}

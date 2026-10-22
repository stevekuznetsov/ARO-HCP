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

package record

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type streamWriteFunc func([]byte) (int, error)

func (fn streamWriteFunc) Write(p []byte) (int, error) { return fn(p) }

func TestStreamSyncInterval(t *testing.T) {
	for _, interval := range []int64{-1, 0, 99, 100, 1000, 5000, 5001} {
		spec, session := testSpec(t), sessionSpec()
		spec.StreamSyncIntervalMillis, session.StreamSyncIntervalMillis = interval, interval
		valid := interval == 0 || interval >= 100 && interval <= 5000
		if err := spec.Validate(); (err == nil) != valid {
			t.Fatalf("worker interval %d: %v", interval, err)
		}
		if err := session.Validate(); (err == nil) != valid {
			t.Fatalf("session interval %d: %v", interval, err)
		}
		if !valid {
			client := sessionClient()
			if _, err := Start(t.Context(), client, session); err == nil || len(client.Actions()) != 0 {
				t.Fatal("invalid interval reached the API")
			}
		}
	}
	if got := testSpec(t).withDefaults().StreamSyncIntervalMillis; got != 1000 {
		t.Fatalf("default interval = %d", got)
	}
	for _, value := range []string{"1.5", "1e100", `"1000"`} {
		var spec Spec
		if err := json.Unmarshal([]byte(`{"streamSyncIntervalMillis":`+value+`}`), &spec); err == nil {
			t.Fatalf("invalid interval accepted: %s", value)
		}
	}
}

func TestLiveFilePeriodicPersistence(t *testing.T) {
	f := testLiveFile(t, 8<<30)
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		f.syncFile = func() error {
			calls.Add(1)
			// The callback runs after bufio.Flush, while the recorder is still open.
			info, err := f.f.Stat()
			if err != nil || info.Size() == 0 {
				t.Errorf("periodic data not visible: %v %v", info, err)
			}
			f.mu.Lock()
			if f.buffer.Buffered() != 0 || err == nil && info.Size() != f.stats.Bytes {
				t.Error("sync ran before all accepted bytes reached the file")
			}
			f.mu.Unlock()
			return nil
		}
		f.startSync(time.Second)
		var workers sync.WaitGroup
		const followers, lines = 145, 1000
		for range followers {
			workers.Go(func() {
				for range lines {
					if err := f.write(StreamObservation{Type: "line", Body: "catchup"}); err != nil {
						t.Error(err)
						return
					}
				}
			})
		}
		workers.Wait()
		if calls.Load() != 0 || f.stats.Records != followers*lines || f.buffer.Size() != liveBufferBytes {
			t.Fatalf("per-line sync or lost records: calls=%d stats=%+v", calls.Load(), f.stats)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("periodic sync calls=%d", calls.Load())
		}
		if err := f.write(StreamObservation{Type: "captureWindowEnded", PotentialGap: true}); err != nil {
			t.Fatal(err)
		}
		if err := f.close(); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 {
			t.Fatalf("missing final sync: calls=%d", calls.Load())
		}
		before := calls.Load()
		time.Sleep(2 * time.Second)
		if calls.Load() != before {
			t.Fatal("sync worker survived close")
		}
	})
	records := readLines[StreamObservation](t, f.f.Name())
	if len(records) != 145001 || records[len(records)-1].Type != "captureWindowEnded" {
		t.Fatal("final accepted records not persisted")
	}
	info, err := os.Stat(f.f.Name())
	if err != nil || info.Size() != f.stats.Bytes {
		t.Fatalf("accepted byte accounting: %v %v %+v", info, err, f.stats)
	}
}

func TestLiveFileSyncDoesNotBlockAppendAndCloseJoins(t *testing.T) {
	f := testLiveFile(t, 1<<20)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		calls := 0
		f.syncFile = func() error {
			calls++
			if calls == 1 {
				close(entered)
				<-release
			}
			return f.f.Sync()
		}
		f.startSync(time.Second)
		<-entered
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := f.writeContext(ctx, StreamObservation{Type: "line"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled append = %v", err)
		}
		// Independent finalization can append while a periodic fsync is stalled.
		if err := f.write(StreamObservation{Type: "captureWindowEnded"}); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- f.close() }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("closed during sync: %v", err)
		default:
		}
		close(release)
		if err := <-done; err != nil || calls != 2 {
			t.Fatalf("final sync/close: %v calls=%d", err, calls)
		}
	})
	if records := readLines[StreamObservation](t, f.f.Name()); len(records) != 1 || records[0].Type != "captureWindowEnded" {
		t.Fatalf("final marker missing: %+v", records)
	}
}

func TestLiveFileStorageErrorsLatch(t *testing.T) {
	for _, stage := range []string{"flush", "sync", "finalSync", "close", "shortWrite"} {
		t.Run(stage, func(t *testing.T) {
			f := testLiveFile(t, 1<<20)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				f.stop = cancel
				failure := errors.New("injected storage failure")
				calls := 0
				f.syncFile = func() error {
					calls++
					if stage == "sync" && calls == 1 || stage == "finalSync" && calls == 2 {
						return failure
					}
					if stage == "close" {
						_ = f.f.Close()
					}
					return nil
				}
				if stage == "flush" || stage == "shortWrite" {
					f.buffer = bufio.NewWriterSize(streamWriteFunc(func(p []byte) (int, error) {
						if stage == "shortWrite" {
							return len(p) - 1, nil
						}
						return 0, failure
					}), liveBufferBytes)
				}
				if err := f.write(StreamObservation{Type: "line", Body: "accepted"}); err != nil {
					t.Fatal(err)
				}
				f.startSync(time.Second)
				time.Sleep(time.Second)
				synctest.Wait()
				if stage != "close" && stage != "finalSync" {
					if ctx.Err() == nil || !f.failed() {
						t.Fatal("asynchronous failure did not stop streams")
					}
					if err := f.write(StreamObservation{Type: "captureWindowEnded"}); err == nil {
						t.Fatal("write silently continued after storage failure")
					}
				}
				if err := f.close(); err == nil || f.stats.Errors != 1 {
					t.Fatalf("failure lost during close: err=%v stats=%+v", err, f.stats)
				}
				// Final summary uses a separate file even when the original is unusable.
				summary := LiveSummary{Incomplete: true, Logs: f.stats}
				path := filepath.Join(filepath.Dir(f.f.Name()), "stream-summary.json")
				if err := writeNewJSON(path, summary); err != nil {
					t.Fatal(err)
				}
				if got := readJSON[LiveSummary](t, path); !got.Incomplete || got.Logs.Errors != 1 {
					t.Fatalf("storage failure missing from summary: %+v", got)
				}
			})
		})
	}
}

func BenchmarkLiveFileSink(b *testing.B) {
	for _, perLine := range []bool{true, false} {
		b.Run(fmt.Sprintf("perLineSync=%t", perLine), func(b *testing.B) {
			file, err := os.CreateTemp(b.TempDir(), "sink")
			if err != nil {
				b.Fatal(err)
			}
			defer file.Close()
			f := &liveFile{f: file, buffer: bufio.NewWriterSize(file, liveBufferBytes), limit: 1<<63 - 1}
			if !perLine {
				f.startSync(time.Second)
			}
			record := StreamObservation{Type: "line", Body: strings.Repeat("x", 256)}
			b.ResetTimer()
			for range b.N {
				if perLine {
					record.Version, record.AtTime = "v1", time.Now().UTC()
					data, err := json.Marshal(record)
					if err == nil {
						_, err = appendLine(file, data)
					}
					if err == nil {
						err = file.Sync()
					}
					if err != nil {
						b.Fatal(err)
					}
				} else if err := f.write(record); err != nil {
					b.Fatal(err)
				}
			}
			if err := f.close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

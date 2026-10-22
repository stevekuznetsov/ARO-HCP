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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	k8sutilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
)

const (
	MaxLiveStreamBytes int64 = 8 << 30
	maxLiveNamespaces        = 100
	defaultLogWorkers        = 256
	maxLogLineBytes          = 1 << 20
	liveBufferBytes          = 64 << 10
)

var (
	errRecordingDuration = errors.New("recording duration elapsed")
	errStreamRenewal     = errors.New("planned stream renewal")
)

const liveLimitations = "Watch is best effort, not lossless: initial/relisted objects are added records, initial bookmarks mark completed lists; normal EOF/renewal reconnects resume from the last accepted RV, while 410 causes a gap and relist. Every requested namespace/resource watch must complete its initial list and open a watch. " +
	"Logs are RAW, UNSANITIZED and CAN CONTAIN SECRETS. All init/regular/ephemeral container statuses are discovered by polling; short-lived pods, rotations and older restarts can be missed. Previous logs are requested only for nonzero restart counts. " +
	"First log requests tail at most 1000 lines; reconnects overlap sinceTime by one second, without deduplication or exactly-once guarantees. Pod UID/restart checks cannot atomically bind the name-based log API. " +
	"CLI workers use separate aggregate request buckets: observation LIST/WATCH/discovery at 10 QPS/20 burst, log GETs including identity checks at logRequestQPS/logRequestBurst (default 20/40). Multiple workers multiply observer pressure; low budgets and API latency can exhaust attempt deadlines. " +
	"Concurrent log requests per worker are bounded by maxConcurrentLogs (default/maximum 256), with a 4096-entry lifetime identity cache; capacity/cache drops are gaps and capture is partial. Include previous requests in capacity planning. Live follows renew every 30 seconds with nonfatal reconnect/potentialGap records, not proof of lost data or lossless delivery; three failed attempts stop an identity with a gap. Clean EOF completes only after a fresh matching pod UID/restart status confirms termination (or previous-log retrieval); unavailable status remains uncertain and retries are bounded. Lines over 1 MiB are dropped with a gap. " +
	"Live files buffer at most 64 KiB each (128 KiB total, not per follower), flushing on buffer capacity and flushing/fsyncing every streamSyncIntervalMillis (default 1000, range 100..5000), independently of capture cancellation. Sync runs outside the append lock; buffer flushes and capacity writes can block appends on disk I/O. Accepted bytes INCLUDING buffered bytes and error/gap records count against maxStreamBytes (default 1 GiB, maximum 8 GiB per file); both live files total at most 16 GiB, separate from maxObservationBytes (default 1 GiB, maximum 8 GiB) and checkpoint/summary metadata. Poll batches are fsynced separately. " +
	"Capture stops without draining buffered logs. captureWindowEnded marks the finite window, not gap-free capture: unread server/transport data may include earlier timestamps. canceledWithBufferedData reports only known local discarded bytes; partialLine marks a known unfinished line. Log lifecycle counts describe admitted identities in the bounded cache: attempted = completed + canceled + failed + active; opened overlaps these outcomes, completedEmpty is a subset of completed. Requests and streamsOpened count multiple attempts/renewals; discovered includes cached identities waiting for capacity, not uncached capacity drops. Lifecycle finalization has a separate five-second writer-wait budget; summaries remain independent of capture cancellation. An in-flight write/fsync cannot be interrupted, so filesystem stalls can exceed any shutdown deadline. " +
	"Normal stop flushes all accepted records and final lifecycle markers, joins sync workers and performs final fsync before close; storage errors stop the affected streams and fail the result. A crash can lose the last sync interval (longer if storage stalls); this is not lossless physical-power-outage protection. Filesystem fsync and API cancellation compliance are external requirements. Ignore an unterminated final NDJSON line after a crash."

func (s Spec) validateStreams() error {
	if (s.EnableWatch || s.EnableLogs) && len(s.Namespaces) > maxLiveNamespaces {
		return errors.New("live recording supports at most 100 explicit namespaces")
	}
	if s.EnableLogs != s.RawLogs {
		return errors.New("enableLogs requires explicit rawLogs: true acknowledgement; rawLogs requires enableLogs")
	}
	if s.MaxStreamBytes < 0 || s.MaxStreamBytes > MaxLiveStreamBytes {
		return errors.New("maxStreamBytes must be 0 (default 1 GiB) or 1..8589934592 bytes per live file")
	}
	if s.MaxConcurrentLogs < 0 || s.MaxConcurrentLogs > 256 {
		return errors.New("maxConcurrentLogs must be 0 (default 256) or 1..256")
	}
	s = s.withDefaults()
	if s.StreamSyncIntervalMillis < 100 || s.StreamSyncIntervalMillis > 5000 {
		return errors.New("streamSyncIntervalMillis must be 0 (default 1000) or 100..5000")
	}
	if s.LogRequestQPS < 1 || s.LogRequestQPS > 100 {
		return errors.New("logRequestQPS must be 0 (default 20) or 1..100")
	}
	if s.LogRequestBurst < s.LogRequestQPS || s.LogRequestBurst > 200 {
		return errors.New("logRequestBurst must be 0 (default 40) or 1..200 and at least the resolved logRequestQPS")
	}
	return nil
}

type LiveSummary struct {
	EnableWatch       bool            `json:"enableWatch"`
	EnableLogs        bool            `json:"enableLogs"`
	RawLogs           bool            `json:"rawLogs"`
	MaxStreamBytes    int64           `json:"maxStreamBytes"`
	MaxConcurrentLogs int             `json:"maxConcurrentLogs"`
	LogRequestQPS     int64           `json:"logRequestQPS"`
	LogRequestBurst   int64           `json:"logRequestBurst"`
	Watch             LiveFileSummary `json:"watch"`
	Logs              LiveFileSummary `json:"logs"`
	Incomplete        bool            `json:"incomplete"`
	Limitations       string          `json:"limitations"`
	Durability        LiveDurability  `json:"durability"`
}

type LiveDurability struct {
	FlushIntervalMilliseconds int64 `json:"flushIntervalMilliseconds"`
	MaximumBufferedBytes      int   `json:"maximumBufferedBytes"` // per live file, not per follower
}

type LiveFileSummary struct {
	Lifecycle    *LogLifecycle `json:"lifecycle,omitempty"`
	Required     int64         `json:"required,omitempty"` // namespace/resource watches
	Initialized  int64         `json:"initialized,omitempty"`
	Records      int64         `json:"records"`
	Bytes        int64         `json:"bytes"`
	Errors       int64         `json:"errors"`
	Gaps         int64         `json:"gaps"`
	LimitReached bool          `json:"limitReached"`
}

type LogLifecycle struct {
	Discovered     int64 `json:"discovered"`
	Attempted      int64 `json:"attempted"`
	Opened         int64 `json:"opened"`
	Completed      int64 `json:"completed"`
	CompletedEmpty int64 `json:"completedEmpty"`
	Canceled       int64 `json:"canceled"`
	Failed         int64 `json:"failed"`
	Active         int64 `json:"active"`
	Requests       int64 `json:"requests"`
	StreamsOpened  int64 `json:"streamsOpened"`
	Lines          int64 `json:"lines"`
}

// StreamObservation never contains a raw Kubernetes object. Body is an
// allowlisted snapshot for watches, or an explicitly acknowledged raw log line.
type StreamObservation struct {
	Version       string     `json:"version"`
	AtTime        time.Time  `json:"atTime"`
	Type          string     `json:"type"`
	Namespace     string     `json:"namespace,omitempty"`
	Resource      string     `json:"resource,omitempty"`
	ObjectRV      string     `json:"objectRV,omitempty"`
	Initial       bool       `json:"initial,omitempty"`
	Body          any        `json:"body,omitempty"`
	Error         *APIError  `json:"error,omitempty"`
	Pod           string     `json:"pod,omitempty"`
	PodUID        types.UID  `json:"podUID,omitempty"`
	Container     string     `json:"container,omitempty"`
	Restart       int32      `json:"restart"`
	Previous      bool       `json:"previous,omitempty"`
	LogTime       *time.Time `json:"logTime,omitempty"`
	PotentialGap  bool       `json:"potentialGap,omitempty"`
	Lines         int64      `json:"lines,omitempty"`
	BufferedBytes int        `json:"bufferedBytes,omitempty"`
	PartialLine   bool       `json:"partialLine,omitempty"`
}

// One mutex serializes complete lines, the byte budget, buffer flushes and counters.
// Sync is safe concurrently with os.File writes and never holds the append lock.
// Workers do not mutate shared Result or LiveSummary values.
type liveFile struct {
	mu        sync.Mutex
	writeOnce sync.Once
	writeGate chan struct{}
	syncFile  func() error // Optional fsync injection for deterministic backpressure tests.
	f         *os.File
	buffer    *bufio.Writer
	syncStop  chan struct{}
	syncDone  chan struct{}
	closed    bool
	limit     int64
	stats     LiveFileSummary
	err       error
	stop      context.CancelFunc
}

func (f *liveFile) write(record StreamObservation) error {
	return f.writeContext(context.Background(), record)
}

func (f *liveFile) writeContext(ctx context.Context, record StreamObservation) error {
	f.writeOnce.Do(func() { f.writeGate = make(chan struct{}, 1) })
	select {
	case f.writeGate <- struct{}{}:
		defer func() { <-f.writeGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.err != nil {
		return f.err
	}
	if f.closed {
		return os.ErrClosed
	}
	if record.Type == "error" {
		f.stats.Errors++
	}
	if record.Type == "gap" {
		f.stats.Gaps++
	}
	record.Version, record.AtTime = "v1", time.Now().UTC()
	data, err := json.Marshal(record)
	if err == nil && int64(len(data))+1 > f.limit-f.stats.Bytes {
		f.stats.LimitReached = true
		err = errors.New("live stream byte limit reached")
	}
	if err == nil {
		var n int
		n, err = f.buffer.Write(append(data, '\n'))
		f.stats.Bytes += int64(n) // Includes bytes still in bufio, not only file size.
		if err == nil {
			f.stats.Records++
		}
	}
	f.latchError(err)
	return err
}

// Caller holds mu. Preserve the first failure, including asynchronous sync errors.
func (f *liveFile) latchError(err error) {
	if err != nil && f.err == nil {
		f.stats.Errors++
		f.err = err
		if f.stop != nil {
			f.stop()
		}
	}
}

func (f *liveFile) startSync(interval time.Duration) {
	f.syncStop, f.syncDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer k8sutilruntime.HandleCrash()
		defer close(f.syncDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-f.syncStop:
				return
			case <-ticker.C:
				if f.flushAndSync() != nil {
					return
				}
			}
		}
	}()
}

func (f *liveFile) flushAndSync() error {
	f.writeOnce.Do(func() { f.writeGate = make(chan struct{}, 1) })
	// Use the same gate/lock order as append so canceled append waiters can leave.
	// An active flush's disk write cannot be preempted by context cancellation.
	f.writeGate <- struct{}{}
	f.mu.Lock()
	err := f.buffer.Flush()
	f.latchError(err)
	f.mu.Unlock()
	<-f.writeGate
	var syncErr error
	if f.syncFile != nil {
		syncErr = f.syncFile()
	} else {
		syncErr = f.f.Sync()
	}
	f.mu.Lock()
	f.latchError(syncErr)
	f.mu.Unlock()
	return errors.Join(err, syncErr)
}

// Producers must be joined first. The flusher outlives capture cancellation so
// final markers can still be accepted. Join before Sync/Close to avoid EBADF races.
func (f *liveFile) close() error {
	if f.syncStop != nil {
		close(f.syncStop)
		<-f.syncDone
	}
	err := f.flushAndSync() // Flush even after a byte-limit or earlier sync failure.
	f.mu.Lock()
	defer f.mu.Unlock()
	closeErr := f.f.Close()
	f.closed = true
	f.latchError(closeErr)
	return errors.Join(f.err, err, closeErr)
}

func (f *liveFile) failed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err != nil
}

// RunStreams writes into an existing private recording directory. Each enabled
// file is create-exclusive and independent of the other file and the poller.
// Callers must not concurrently run another recorder in that directory.
// All workers are joined before files and stream-summary.json are finalized.
// Caller cancellation/deadlines are errors; the configured duration is not.
// Missing initial watches and observed errors/gaps still make duration runs fail.
func RunStreams(ctx context.Context, client kubernetes.Interface, spec Spec) (summary LiveSummary, retErr error) {
	return runStreamsWithLogClient(ctx, client, client, spec)
}

func runStreamsWithLogClient(ctx context.Context, client, logClient kubernetes.Interface, spec Spec) (summary LiveSummary, retErr error) {
	if err := spec.Validate(); err != nil {
		return summary, err
	}
	if client == nil || (spec.EnableLogs && logClient == nil) {
		return summary, errors.New("Kubernetes client is required")
	}
	ctx, cancel := context.WithTimeoutCause(ctx, time.Duration(spec.DurationSeconds)*time.Second, errRecordingDuration)
	defer cancel()
	spec = spec.withDefaults()
	limit := spec.MaxStreamBytes
	summary = LiveSummary{EnableWatch: spec.EnableWatch, EnableLogs: spec.EnableLogs, RawLogs: spec.RawLogs, MaxStreamBytes: limit, MaxConcurrentLogs: spec.MaxConcurrentLogs, Limitations: liveLimitations}
	summary.LogRequestQPS, summary.LogRequestBurst = spec.LogRequestQPS, spec.LogRequestBurst
	summary.Durability = LiveDurability{FlushIntervalMilliseconds: spec.StreamSyncIntervalMillis, MaximumBufferedBytes: liveBufferBytes}
	if spec.EnableWatch {
		summary.Watch.Required = int64(len(spec.Namespaces) * len(resources))
	}
	files := map[string]*liveFile{}
	contexts := map[string]context.Context{}
	defer func() {
		if ctx.Err() != nil && !errors.Is(context.Cause(ctx), errRecordingDuration) {
			retErr = errors.Join(retErr, ctx.Err())
		}
		for name, f := range files {
			if err := f.close(); err != nil {
				retErr = errors.Join(retErr, err)
			}
			if name == "watch" {
				summary.Watch = f.stats
			} else {
				summary.Logs = f.stats
			}
		}
		if missing := summary.Watch.Required - summary.Watch.Initialized; missing > 0 {
			retErr = errors.Join(retErr, fmt.Errorf("%d required watches did not initialize", missing))
		}
		summary.Incomplete = retErr != nil || summary.Watch.Errors+summary.Logs.Errors+summary.Watch.Gaps+summary.Logs.Gaps > 0
		if summary.Incomplete {
			retErr = errors.Join(retErr, errors.New("live recording incomplete; inspect stream-summary.json"))
		}
		retErr = errors.Join(retErr, writeNewJSON(filepath.Join(spec.OutputDir, "stream-summary.json"), summary), syncDirectory(spec.OutputDir))
		if retErr != nil {
			summary.Incomplete = true
		}
	}()
	// Open both independently: a collision on one must not suppress the other.
	for _, name := range []string{"watch", "logs"} {
		if name == "watch" && !spec.EnableWatch || name == "logs" && !spec.EnableLogs {
			continue
		}
		f, err := os.OpenFile(filepath.Join(spec.OutputDir, name+".ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("open %s stream: %w", name, err))
			if name == "watch" {
				summary.Watch.Errors++
			} else {
				summary.Logs.Errors++
			}
			continue
		}
		fileCtx, fileCancel := context.WithCancel(ctx)
		defer fileCancel()
		contexts[name] = fileCtx
		files[name] = &liveFile{f: f, buffer: bufio.NewWriterSize(f, liveBufferBytes), limit: limit, stop: fileCancel}
		if name == "watch" {
			files[name].stats.Required = summary.Watch.Required
		}
		files[name].startSync(time.Duration(spec.StreamSyncIntervalMillis) * time.Millisecond)
	}
	if err := syncDirectory(spec.OutputDir); err != nil {
		return summary, errors.Join(retErr, err)
	}
	var workers []<-chan error
	start := func(fn func() error) {
		done := make(chan error, 1)
		workers = append(workers, done)
		go func() {
			defer k8sutilruntime.HandleCrash()
			done <- fn()
		}()
	}
	if f := files["watch"]; f != nil {
		for _, ns := range spec.Namespaces {
			for _, resource := range resources {
				start(func() error { return recordWatch(contexts["watch"], client, ns, resource, f) })
			}
		}
	}
	if f := files["logs"]; f != nil {
		start(func() error {
			return recordLogsWithLogClient(contexts["logs"], client, logClient, spec, f, openPodLogs)
		})
	}
	for _, done := range workers {
		retErr = errors.Join(retErr, <-done)
	}
	return summary, retErr
}

func streamPause(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

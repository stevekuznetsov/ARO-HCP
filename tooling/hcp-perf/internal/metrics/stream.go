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

package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StreamOptions are explicit, including Lag (zero disables the scrape delay).
// Chunk must be a multiple of Step to preserve one evaluation grid. The final
// window may be shorter. Drain bounds catch-up after End+Lag, not per window.
type StreamOptions struct {
	Duration     time.Duration
	Chunk        time.Duration
	Step         time.Duration
	Lag          time.Duration
	Drain        time.Duration
	ChunkTimeout time.Duration
}

// StreamResult is also the atomically replaced manifest.json checkpoint.
// Complete means every window's Export returned, NOT that all metrics exist or
// all queries succeeded. DataQuality and the per-query manifests describe that.
// Endpoint/authentication are deliberately absent, just as in Export.
type StreamResult struct {
	Version         int           `json:"version"`
	Status          string        `json:"status"`
	DataQuality     string        `json:"dataQuality"`
	Start           time.Time     `json:"start"`
	End             time.Time     `json:"end"`
	Deadline        time.Time     `json:"deadline"`
	Updated         time.Time     `json:"updated"`
	Chunk           time.Duration `json:"chunkNanoseconds"`
	Step            time.Duration `json:"stepNanoseconds"`
	Lag             time.Duration `json:"lagNanoseconds"`
	Drain           time.Duration `json:"drainNanoseconds"`
	ChunkTimeout    time.Duration `json:"chunkTimeoutNanoseconds"`
	Queries         []Query       `json:"queries"`
	Intervals       string        `json:"intervals"`
	PlannedChunks   int           `json:"plannedChunks"`
	CompletedChunks int           `json:"completedChunks"`
	PendingChunks   int           `json:"pendingChunks"`
	NextStart       time.Time     `json:"nextStart"`
	Behind          bool          `json:"behind"`
	ErrorCount      int           `json:"errorCount"`
	IncompleteCount int           `json:"incompleteCount"`
	WarningCount    int           `json:"warningCount"`
	Error           string        `json:"error,omitempty"`
}

// The leading dot cannot occur in a valid query name, keeping metadata disjoint
// from every raw query response filename.
const streamWindowFilename = ".stream-window.json"

// StreamChunk is synced to chunk-NNNNNN/.stream-window.json and journal.jsonl. ErrorCount
// counts only nonempty query errors; warnings and natural label churn are separate.
type StreamChunk struct {
	Index           int           `json:"index"`
	Directory       string        `json:"directory"`
	Start           time.Time     `json:"start"`
	End             time.Time     `json:"end"`
	Started         time.Time     `json:"started"`
	Finished        time.Time     `json:"finished"`
	Behind          bool          `json:"behind"`
	BehindBy        time.Duration `json:"behindNanoseconds"`
	Status          string        `json:"status"`
	ErrorCount      int           `json:"errorCount"`
	IncompleteCount int           `json:"incompleteCount"`
	WarningCount    int           `json:"warningCount"`
	Error           string        `json:"error,omitempty"`
}

// Stream is a blocking local worker, suitable for a systemd service with a
// persistent local output path. It never launches/detaches processes or retries
// queries. Each completed window is exported once, sequentially, even when behind.
// Raw responses are synced and hashed by Export before the next query. A synced
// journal precedes each atomic checkpoint; after a crash the journal can be ahead
// of that checkpoint. Resume is not supported; existing output is never reused.
// HTTP transports must honor contexts. Filesystem calls, as in Export, cannot be
// preempted by context deadlines. Cancellation still writes a final checkpoint.
func Stream(ctx context.Context, client *http.Client, endpoint string, queries []Query, options StreamOptions, dir string) (StreamResult, error) {
	return stream(ctx, client, endpoint, queries, options, dir, time.Now, streamWait)
}

func streamWait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func stream(ctx context.Context, client *http.Client, endpoint string, queries []Query, o StreamOptions, dir string, now func() time.Time, wait func(context.Context, time.Duration) error) (result StreamResult, retErr error) {
	if ctx == nil {
		return result, errors.New("context is required")
	}
	if o.Chunk < time.Second || o.Chunk > 5*time.Minute || o.Step < time.Second || o.Step > o.Chunk || o.Chunk%o.Step != 0 || o.Duration < o.Chunk || o.Duration > 24*time.Hour || o.Lag < 0 || o.Lag > 5*time.Minute || o.Drain <= 0 || o.Drain > 5*time.Minute || o.ChunkTimeout <= 0 || o.ChunkTimeout > 90*time.Second {
		return result, errors.New("require step >=1s, chunk a step multiple <=5m, chunk <= duration <=24h, lag 0..5m, drain (0,5m], chunk-timeout (0,90s]")
	}
	start := now().UTC().Truncate(time.Millisecond)
	end := start.Add(o.Duration)
	if err := validate(queries, start, end, o.Step); err != nil {
		return result, err
	}
	// Match Export validation before creating the top-level output directory.
	u, err := url.Parse(endpoint)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(endpoint, "#") {
		return result, errors.New("endpoint must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return result, err
	}
	result = StreamResult{
		Version: 1, Status: "running", DataQuality: StatusPending, Start: start, End: end,
		Deadline: end.Add(o.Lag + o.Drain), Chunk: o.Chunk, Step: o.Step, Lag: o.Lag,
		Drain: o.Drain, ChunkTimeout: o.ChunkTimeout, Queries: append([]Query(nil), queries...),
		Intervals:     "inclusive [start,end]; adjacent windows share one grid boundary; deduplicate by query, full label set, timestamp",
		PlannedChunks: int((o.Duration-1)/o.Chunk + 1), NextStart: start,
	}
	result.PendingChunks = result.PlannedChunks
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(result.Deadline) {
		result.Deadline = deadline.UTC()
	}
	// Use a relative timeout so the injected scheduling clock need not be wall time.
	runCtx, cancel := context.WithTimeout(ctx, result.Deadline.Sub(now()))
	defer cancel()
	defer func() {
		result.Status = "incomplete"
		if retErr == nil && result.PendingChunks == 0 {
			result.Status = "complete"
		}
		if result.ErrorCount > 0 {
			retErr = errors.Join(retErr, fmt.Errorf("%d query errors; see chunk manifests", result.ErrorCount))
		}
		if retErr != nil {
			result.Error = retErr.Error()
		}
		if result.PendingChunks > 0 || retErr != nil || result.IncompleteCount > 0 {
			result.DataQuality = StatusIncomplete
		} else {
			result.DataQuality = StatusSuccess
		}
		result.Updated = now().UTC()
		retErr = errors.Join(retErr, streamWriteJSON(dir, "manifest.json", result))
	}()
	journal, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0600)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, journal.Close()) }()
	if err := journal.Sync(); err != nil {
		return result, err
	}
	result.Updated = now().UTC()
	if err := streamWriteJSON(dir, "manifest.json", result); err != nil {
		return result, err
	}
	if err := streamSyncDir(filepath.Dir(dir)); err != nil {
		return result, err
	}
	for result.PendingChunks > 0 {
		windowEnd := result.NextStart.Add(o.Chunk)
		if windowEnd.After(end) {
			windowEnd = end
		}
		ready := windowEnd.Add(o.Lag)
		for now().Before(ready) {
			if err := wait(runCtx, ready.Sub(now())); err != nil {
				return result, err
			}
		}
		if err := runCtx.Err(); err != nil {
			return result, err
		}
		if !now().Before(result.Deadline) {
			return result, context.DeadlineExceeded
		}
		chunk := StreamChunk{Index: result.CompletedChunks, Directory: fmt.Sprintf("chunk-%06d", result.CompletedChunks), Start: result.NextStart, End: windowEnd, Started: now().UTC(), Status: StatusSuccess}
		chunk.BehindBy = max(0, chunk.Started.Sub(ready))
		// Timer jitter is not backlog: another whole window must be ready.
		chunk.Behind = chunk.BehindBy >= o.Chunk
		chunkDir := filepath.Join(dir, chunk.Directory)
		chunkCtx, chunkCancel := context.WithTimeout(runCtx, min(o.ChunkTimeout, result.Deadline.Sub(now())))
		manifest, exportErr := Export(chunkCtx, client, endpoint, queries, chunk.Start, chunk.End, o.Step, chunkDir)
		chunkCancel()
		if manifest.Version == 0 {
			return result, exportErr
		}
		chunk.Finished = now().UTC()
		chunk.Behind = chunk.Behind || chunk.Finished.Sub(ready) >= o.Chunk
		for _, query := range manifest.Queries {
			if query.Error != "" {
				chunk.ErrorCount++
			}
			if query.Status == StatusIncomplete {
				chunk.IncompleteCount++
			}
			chunk.WarningCount += len(query.Warnings)
		}
		if exportErr != nil {
			chunk.Status, chunk.Error = StatusError, exportErr.Error()
		} else if chunk.IncompleteCount > 0 {
			chunk.Status = StatusIncomplete
		}
		if err := streamWriteJSON(chunkDir, streamWindowFilename, chunk); err != nil {
			return result, errors.Join(exportErr, err)
		}
		if err := json.NewEncoder(journal).Encode(chunk); err != nil {
			return result, err
		}
		if err := journal.Sync(); err != nil {
			return result, err
		}
		result.CompletedChunks++
		result.PendingChunks--
		result.NextStart = windowEnd
		result.ErrorCount += chunk.ErrorCount
		result.IncompleteCount += chunk.IncompleteCount
		result.WarningCount += chunk.WarningCount
		result.Behind = result.Behind || chunk.Behind
		result.DataQuality = StatusSuccess
		if result.ErrorCount > 0 || result.IncompleteCount > 0 {
			result.DataQuality = StatusIncomplete
		}
		result.Updated = now().UTC()
		if err := streamWriteJSON(dir, "manifest.json", result); err != nil {
			return result, err
		}
		// Query failures are retained, never retried; a fresh context is used for
		// the next window. Storage failures must not be mistaken for query errors.
		var pathErr *os.PathError
		if errors.As(exportErr, &pathErr) || (exportErr != nil && chunk.ErrorCount == 0) {
			return result, exportErr
		}
		if err := runCtx.Err(); err != nil {
			return result, err
		}
		if !now().Before(result.Deadline) {
			return result, context.DeadlineExceeded
		}
	}
	return result, nil
}

func streamWriteJSON(dir, name string, value any) error {
	f, err := os.CreateTemp(dir, ".stream-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	writeErr := json.NewEncoder(f).Encode(value)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if name == streamWindowFilename {
		// Publish immutable window metadata atomically without replacing any
		// existing target. Only the top-level checkpoint may be replaced.
		err = os.Link(f.Name(), filepath.Join(dir, name))
	} else {
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		return err
	}
	return streamSyncDir(dir)
}

func streamSyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

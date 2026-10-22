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

// BackfillOptions describes a fixed inclusive range. Chunk must be a Step
// multiple so all windows use the grid anchored at Start, including a short
// final window. End is never rounded up. At most 1440 windows may be requested.
type BackfillOptions struct {
	Start, End  time.Time
	Step, Chunk time.Duration
}

// BackfillResult uses Stream's completion/quality accounting without its live
// scheduling fields (lag, drain, chunk timeout and behind remain zero).
// Windows enumerates the entire plan before the first request. Completed means
// Export returned, not that the queries succeeded; per-query hashes and errors
// remain in each window's Export manifest. Pending windows have no directory yet.
type BackfillResult struct {
	StreamResult
	Windows []StreamChunk `json:"windows"`
}

// A leading dot cannot occur in a valid query name.
const backfillWindowFilename = ".window.json"

// Backfill exports a static range immediately, one query/window at a time. It
// never waits for wall time, retries, splits failed windows, changes expressions,
// or filters labels. Adjacent inclusive windows overlap at one grid point;
// offline consumers must deduplicate by query, full label set and timestamp.
// Export exclusively creates each chunk directory, syncs raw responses and
// records their SHA256. A synced atomic checkpoint follows every window and
// cancellation. Existing output is refused; resume is not supported.
func Backfill(ctx context.Context, client *http.Client, endpoint string, queries []Query, o BackfillOptions, dir string) (result BackfillResult, retErr error) {
	if ctx == nil {
		return result, errors.New("context is required")
	}
	if err := validate(queries, o.Start, o.End, o.Step); err != nil {
		return result, err
	}
	duration := o.End.Sub(o.Start)
	if duration > 24*time.Hour || o.Chunk < o.Step || o.Chunk > 5*time.Minute || o.Chunk%o.Step != 0 {
		return result, errors.New("require range <=24h and chunk a step multiple between step and 5m")
	}
	planned := max(1, int((duration+o.Chunk-1)/o.Chunk))
	if planned > 1440 {
		return result, errors.New("backfill exceeds 1440 windows; increase chunk or shorten range")
	}
	// Match Export's endpoint guard before creating any output. Never echo a URL
	// parse error: it can contain credentials supplied by the caller.
	u, err := url.Parse(endpoint)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(endpoint, "#") {
		return result, errors.New("endpoint must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return result, err
	}
	result.StreamResult = StreamResult{
		Version: 1, Status: "running", DataQuality: StatusPending,
		Start: o.Start.UTC(), End: o.End.UTC(), Step: o.Step, Chunk: o.Chunk,
		Queries: append([]Query(nil), queries...), PlannedChunks: planned,
		PendingChunks: planned, NextStart: o.Start.UTC(),
		Intervals: "inclusive [start,end]; grid anchored at start; end included only if on grid; adjacent windows share one grid boundary; deduplicate by query, full label set, timestamp",
	}
	if deadline, ok := ctx.Deadline(); ok {
		result.Deadline = deadline.UTC()
	}
	for i := 0; i < planned; i++ {
		start := result.Start.Add(time.Duration(i) * o.Chunk)
		end := start.Add(o.Chunk)
		if end.After(result.End) {
			end = result.End
		}
		result.Windows = append(result.Windows, StreamChunk{
			Index: i, Directory: fmt.Sprintf("chunk-%06d", i), Start: start, End: end, Status: StatusPending,
		})
	}
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
		result.DataQuality = StatusSuccess
		if retErr != nil || result.PendingChunks > 0 || result.IncompleteCount > 0 {
			result.DataQuality = StatusIncomplete
		}
		result.Updated = time.Now().UTC()
		retErr = errors.Join(retErr, streamWriteJSON(dir, "manifest.json", result))
	}()
	result.Updated = time.Now().UTC()
	if err := streamWriteJSON(dir, "manifest.json", result); err != nil {
		return result, err
	}
	if err := streamSyncDir(filepath.Dir(dir)); err != nil {
		return result, err
	}
	for i := range result.Windows {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		window := &result.Windows[i]
		window.Started = time.Now().UTC()
		chunkDir := filepath.Join(dir, window.Directory)
		manifest, exportErr := Export(ctx, client, endpoint, queries, window.Start, window.End, o.Step, chunkDir)
		window.Finished = time.Now().UTC()
		window.Status = StatusSuccess
		for _, query := range manifest.Queries {
			if query.Error != "" {
				window.ErrorCount++
			}
			if query.Status == StatusIncomplete {
				window.IncompleteCount++
			}
			window.WarningCount += len(query.Warnings)
		}
		if exportErr != nil {
			window.Status, window.Error = StatusError, exportErr.Error()
		} else if window.IncompleteCount > 0 {
			window.Status = StatusIncomplete
		}
		if manifest.Version == 0 {
			return result, exportErr
		}
		// Metadata is immutable and disjoint from raw response names. Export,
		// not this worker, creates the directory; never precreate it.
		f, err := os.OpenFile(filepath.Join(chunkDir, backfillWindowFilename), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return result, errors.Join(exportErr, err)
		}
		writeErr := json.NewEncoder(f).Encode(window)
		syncErr := f.Sync()
		closeErr := f.Close()
		if err := errors.Join(writeErr, syncErr, closeErr, streamSyncDir(chunkDir)); err != nil {
			return result, errors.Join(exportErr, err)
		}
		result.CompletedChunks++
		result.PendingChunks--
		result.NextStart = window.End
		result.ErrorCount += window.ErrorCount
		result.IncompleteCount += window.IncompleteCount
		result.WarningCount += window.WarningCount
		result.DataQuality = StatusSuccess
		if result.ErrorCount > 0 || result.IncompleteCount > 0 {
			result.DataQuality = StatusIncomplete
		}
		result.Updated = time.Now().UTC()
		if err := streamWriteJSON(dir, "manifest.json", result); err != nil {
			return result, err
		}
		// Query errors do not poison later windows. Storage errors stop capture.
		var pathErr *os.PathError
		if errors.As(exportErr, &pathErr) || (exportErr != nil && window.ErrorCount == 0) {
			return result, exportErr
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}
	return result, nil
}

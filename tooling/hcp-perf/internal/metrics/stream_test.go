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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type streamClock struct {
	mu   sync.Mutex
	time time.Time
}

type streamCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b streamCancelBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.cancel()
	}
	return n, err
}

func (c *streamClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.time
}

func (c *streamClock) wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.time = c.time.Add(d)
	c.mu.Unlock()
	return nil
}

func streamTestOptions() StreamOptions {
	return StreamOptions{Duration: 75 * time.Second, Chunk: 30 * time.Second, Step: 5 * time.Second, Lag: 15 * time.Second, Drain: 90 * time.Second, ChunkTimeout: 90 * time.Second}
}

func streamReadJSON(t *testing.T, path string, value any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, value); err != nil {
		t.Fatal(err)
	}
}

func streamCheckResult(t *testing.T, dir string, result StreamResult) []StreamChunk {
	t.Helper()
	var persisted StreamResult
	streamReadJSON(t, filepath.Join(dir, "manifest.json"), &persisted)
	if !reflect.DeepEqual(result, persisted) {
		t.Fatalf("returned/checkpoint mismatch: %+v / %+v", result, persisted)
	}
	f, err := os.Open(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var chunks []StreamChunk
	decoder := json.NewDecoder(f)
	for {
		var chunk StreamChunk
		if err := decoder.Decode(&chunk); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var persisted StreamChunk
		streamReadJSON(t, filepath.Join(dir, chunk.Directory, streamWindowFilename), &persisted)
		if !reflect.DeepEqual(chunk, persisted) {
			t.Fatal("chunk/journal mismatch")
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) != result.CompletedChunks {
		t.Fatalf("journal has %d chunks, want %d", len(chunks), result.CompletedChunks)
	}
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0600)
		if entry.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Errorf("permissions %s: %v", path, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return chunks
}

func streamResponse(r *http.Request, label, value string) string {
	start, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start"))
	end, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end"))
	step, _ := time.ParseDuration(r.URL.Query().Get("step") + "s")
	var values []string
	for ts := start; !ts.After(end); ts = ts.Add(step) {
		values = append(values, fmt.Sprintf("[%d,%q]", ts.Unix(), value))
	}
	return fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"instance":%q},"values":[%s]}]}}`, label, strings.Join(values, ","))
}

func TestStreamCompletedWindowsDurableBeforeEnd(t *testing.T) {
	clock := &streamClock{time: testStart}
	o := streamTestOptions()
	dir := filepath.Join(t.TempDir(), "stream")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := calls / 2
		start := testStart.Add(time.Duration(index) * o.Chunk)
		end := start.Add(o.Chunk)
		if end.After(testStart.Add(o.Duration)) {
			end = testStart.Add(o.Duration)
		}
		if r.URL.Path != "/prom/api/v1/query_range" || r.URL.Query().Get("start") != start.Format(time.RFC3339Nano) || r.URL.Query().Get("end") != end.Format(time.RFC3339Nano) || r.URL.Query().Get("step") != "5" || clock.now().Before(end.Add(o.Lag)) {
			t.Errorf("wrong interval or premature request: %s at %s", r.URL, clock.now())
		}
		if calls == 1 {
			m := loadManifest(t, filepath.Join(dir, "chunk-000000"))
			if m.Queries[0].SHA256 == "" || m.Queries[0].Status != StatusSuccess || m.Queries[1].Status != StatusPending || !clock.now().Before(testStart.Add(o.Duration)) {
				t.Error("raw evidence not persisted during capture")
			}
		}
		if calls == 2 {
			var checkpoint StreamResult
			streamReadJSON(t, filepath.Join(dir, "manifest.json"), &checkpoint)
			if checkpoint.CompletedChunks != 1 || checkpoint.Status != "running" {
				t.Errorf("checkpoint not incremental: %+v", checkpoint)
			}
		}
		calls++
		_, _ = io.WriteString(w, streamResponse(r, fmt.Sprintf("dynamic-%d", index), "1"))
	}))
	defer srv.Close()
	result, err := stream(context.Background(), srv.Client(), srv.URL+"/prom", []Query{{"first", "up"}, {"second", "other"}}, o, dir, clock.now, clock.wait)
	if err != nil || calls != 6 || result.Status != "complete" || result.DataQuality != StatusSuccess || result.PendingChunks != 0 || result.ErrorCount != 0 || result.Behind || !result.NextStart.Equal(result.End) {
		t.Fatalf("unexpected result: %+v, %v, calls %d", result, err, calls)
	}
	chunks := streamCheckResult(t, dir, result)
	for _, chunk := range chunks {
		m := loadManifest(t, filepath.Join(dir, chunk.Directory))
		for _, q := range m.Queries {
			body, err := os.ReadFile(filepath.Join(dir, chunk.Directory, q.File))
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(body)
			if hex.EncodeToString(hash[:]) != q.SHA256 {
				t.Fatal("raw hash mismatch")
			}
		}
	}
}

func TestStreamMetadataDoesNotOverwriteQueries(t *testing.T) {
	if queryName.MatchString(strings.TrimSuffix(streamWindowFilename, ".json")) {
		t.Fatal("window metadata filename can collide with a valid query response")
	}
	clock := &streamClock{time: testStart}
	dir := filepath.Join(t.TempDir(), "stream")
	queries := []Query{{"stream", "up"}, {"window", "up"}, {"stream-window", "up"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, streamResponse(r, "a", "1"))
	}))
	defer srv.Close()
	result, err := stream(context.Background(), srv.Client(), srv.URL, queries, streamTestOptions(), dir, clock.now, clock.wait)
	if err != nil || result.Status != "complete" || result.DataQuality != StatusSuccess {
		t.Fatalf("stream failed: %+v %v", result, err)
	}
	for _, chunk := range streamCheckResult(t, dir, result) {
		chunkDir := filepath.Join(dir, chunk.Directory)
		analysis, err := Analyze(chunkDir)
		if err != nil || len(analysis.Queries) != len(queries) {
			t.Fatalf("chunk %s no longer analyzable: %+v %v", chunk.Directory, analysis, err)
		}
		for i, query := range analysis.Manifest.Queries {
			if query.Name != queries[i].Name || query.File != query.Name+".json" || analysis.Queries[i].Status != StatusSuccess {
				t.Fatalf("query evidence changed: %+v", query)
			}
			body, err := os.ReadFile(filepath.Join(chunkDir, query.File))
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(body)
			if hex.EncodeToString(hash[:]) != query.SHA256 {
				t.Fatalf("raw hash mismatch for %s", query.Name)
			}
		}
	}
}

func TestStreamWindowMetadataDoesNotReplaceExistingTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, streamWindowFilename)
	if err := streamWriteJSON(dir, streamWindowFilename, "existing"); err != nil {
		t.Fatal(err)
	}
	if err := streamWriteJSON(dir, streamWindowFilename, "replacement"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing window metadata accepted: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "\"existing\"\n" {
		t.Fatalf("existing window metadata overwritten: %s %v", body, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary metadata files not removed: %v %v", entries, err)
	}
}

func TestStreamQueryQualityContinues(t *testing.T) {
	for _, quality := range []string{"missing", "nonfinite", "http", "warning", "gap"} {
		t.Run(quality, func(t *testing.T) {
			clock := &streamClock{time: testStart}
			dir := filepath.Join(t.TempDir(), "stream")
			calls := 0
			var firstBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := streamResponse(r, "changing-label", "1")
				if calls == 0 {
					switch quality {
					case "missing":
						body = `{"status":"success","data":{"resultType":"matrix","result":[]}}`
					case "nonfinite":
						body = streamResponse(r, "a", "NaN")
					case "http":
						w.WriteHeader(503)
						body = `{"status":"error"}`
					case "warning":
						body = strings.Replace(body, `"status":"success"`, `"status":"success","warnings":["partial response"]`, 1)
					case "gap":
						body = strings.Replace(body, fmt.Sprintf(`[%d,"1"],`, testStart.Add(5*time.Second).Unix()), "", 1)
					}
					firstBody = body
				}
				calls++
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			result, err := stream(context.Background(), srv.Client(), srv.URL, []Query{{"up", "up"}}, streamTestOptions(), dir, clock.now, clock.wait)
			wantError := quality != "warning" && quality != "gap"
			if (err != nil) != wantError || result.Status != "complete" || result.DataQuality != StatusIncomplete || calls != 3 {
				t.Fatalf("failed window stopped capture or gaps hidden: %+v %v", result, err)
			}
			wantCount := 0
			if wantError {
				wantCount = 1
			}
			if result.ErrorCount != wantCount {
				t.Fatalf("non-error counted as error: %+v", result)
			}
			chunks := streamCheckResult(t, dir, result)
			if chunks[1].Status != StatusSuccess || chunks[2].Status != StatusSuccess {
				t.Fatal("failure poisoned later windows")
			}
			b, err := os.ReadFile(filepath.Join(dir, "chunk-000000", "up.json"))
			if err != nil || string(b) != firstBody {
				t.Fatal("raw failure response lost")
			}
		})
	}
}

func TestStreamBacklogAndDrain(t *testing.T) {
	for _, exhaust := range []bool{false, true} {
		t.Run(fmt.Sprint(exhaust), func(t *testing.T) {
			clock := &streamClock{time: testStart}
			o := streamTestOptions()
			o.Duration = 2 * time.Minute
			o.Drain = 30 * time.Second
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start := testStart.Add(time.Duration(calls) * o.Chunk)
				if r.URL.Query().Get("start") != start.Format(time.RFC3339Nano) {
					t.Error("backlog skipped a window")
				}
				calls++
				if calls == 1 || exhaust {
					_ = clock.wait(context.Background(), 70*time.Second)
				}
				_, _ = io.WriteString(w, streamResponse(r, "a", "1"))
			}))
			defer srv.Close()
			dir := filepath.Join(t.TempDir(), "stream")
			result, err := stream(context.Background(), srv.Client(), srv.URL, []Query{{"up", "up"}}, o, dir, clock.now, clock.wait)
			if !result.Behind {
				t.Fatal("backlog not recorded")
			}
			if exhaust {
				if !errors.Is(err, context.DeadlineExceeded) || result.PendingChunks == 0 || result.Status != "incomplete" {
					t.Fatalf("drain unbounded: %+v %v", result, err)
				}
			} else if err != nil || calls != 4 || result.Status != "complete" {
				t.Fatalf("catch-up failed: %+v %v", result, err)
			}
			streamCheckResult(t, dir, result)
		})
	}
}

func TestStreamChunkTimeoutDoesNotPoisonParent(t *testing.T) {
	clock := &streamClock{time: testStart}
	o := streamTestOptions()
	// The deadline includes durable manifest writes before the first request.
	// Allow persistent-storage setup; the transport still forces a real timeout.
	o.ChunkTimeout = 5 * time.Second
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > o.ChunkTimeout {
			t.Error("whole batch context deadline not applied to request")
		}
		calls++
		if calls == 1 {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(streamResponse(r, "a", "1")))}, nil
	})}
	dir := filepath.Join(t.TempDir(), "stream")
	result, err := stream(context.Background(), client, "http://example.com", []Query{{"up", "up"}, {"other", "other"}}, o, dir, clock.now, clock.wait)
	if err == nil || result.Status != "complete" || result.ErrorCount != 2 || calls != 5 {
		t.Fatalf("timeout propagated to next window: %+v %v calls=%d", result, err, calls)
	}
	chunks := streamCheckResult(t, dir, result)
	if chunks[0].ErrorCount != 2 || chunks[1].Status != StatusSuccess || chunks[2].Status != StatusSuccess {
		t.Fatal("chunk timeout did not remain isolated to the first window")
	}
}

func TestStreamParentDeadlineBoundsWaiting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "stream")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("queried an unfinished window")
		return nil, errors.New("unexpected request")
	})}
	result, err := Stream(ctx, client, "http://example.com", []Query{{"up", "up"}}, streamTestOptions(), dir)
	deadline, _ := ctx.Deadline()
	if !errors.Is(err, context.DeadlineExceeded) || result.Status != "incomplete" || result.CompletedChunks != 0 || !result.Deadline.Equal(deadline) {
		t.Fatalf("parent deadline not respected: %+v %v", result, err)
	}
	streamCheckResult(t, dir, result)
}

func TestStreamCancellationDurability(t *testing.T) {
	for _, duringRequest := range []bool{false, true} {
		t.Run(fmt.Sprint(duringRequest), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := &streamClock{time: testStart}
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if duringRequest {
					_, _ = io.WriteString(w, `{"status":`)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, streamResponse(r, "a", "1"))
			}))
			defer srv.Close()
			client := srv.Client()
			transport := client.Transport
			if duringRequest {
				client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					resp, err := transport.RoundTrip(r)
					if err == nil {
						resp.Body = streamCancelBody{ReadCloser: resp.Body, cancel: cancel}
					}
					return resp, err
				})
			}
			waits := 0
			wait := func(ctx context.Context, d time.Duration) error {
				waits++
				if waits == 2 {
					cancel()
				}
				return clock.wait(ctx, d)
			}
			dir := filepath.Join(t.TempDir(), "stream")
			result, err := stream(ctx, client, srv.URL, []Query{{"up", "up"}}, streamTestOptions(), dir, clock.now, wait)
			if !errors.Is(err, context.Canceled) || result.Status != "incomplete" || result.CompletedChunks != 1 || result.PendingChunks != 2 || result.End != testStart.Add(75*time.Second) || calls != 1 {
				t.Fatalf("cancellation summary lost: %+v %v", result, err)
			}
			streamCheckResult(t, dir, result)
			m := loadManifest(t, filepath.Join(dir, "chunk-000000"))
			if m.Queries[0].SHA256 == "" {
				t.Fatal("partial raw hash missing")
			}
		})
	}
}

func TestStreamValidationAndExclusiveStorage(t *testing.T) {
	for _, change := range []func(*StreamOptions){
		func(o *StreamOptions) { o.Duration = 0 },
		func(o *StreamOptions) { o.Duration = 25 * time.Hour },
		func(o *StreamOptions) { o.Chunk = 6 * time.Minute },
		func(o *StreamOptions) { o.Chunk = 31 * time.Second },
		func(o *StreamOptions) { o.Step = 0 },
		func(o *StreamOptions) { o.Step = time.Millisecond },
		func(o *StreamOptions) { o.Lag = -time.Second },
		func(o *StreamOptions) { o.Lag = 6 * time.Minute },
		func(o *StreamOptions) { o.Drain = 0 },
		func(o *StreamOptions) { o.Drain = 6 * time.Minute },
		func(o *StreamOptions) { o.ChunkTimeout = 0 },
		func(o *StreamOptions) { o.ChunkTimeout = 91 * time.Second },
	} {
		o := streamTestOptions()
		change(&o)
		dir := filepath.Join(t.TempDir(), "stream")
		if _, err := Stream(context.Background(), nil, "http://example.com", []Query{{"up", "up"}}, o, dir); err == nil {
			t.Fatalf("invalid options accepted: %+v", o)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid options created output")
		}
	}
	for _, endpoint := range []string{"https://user:secret@example.com", "https://example.com?secret=x", "https://example.com#", "/relative"} {
		dir := filepath.Join(t.TempDir(), "stream")
		if _, err := Stream(context.Background(), nil, endpoint, []Query{{"up", "up"}}, streamTestOptions(), dir); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid endpoint accepted/leaked: %v", err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid endpoint created output")
		}
	}
	parent := t.TempDir()
	for _, dir := range []string{parent, filepath.Join(parent, "absent", "stream")} {
		if _, err := Stream(context.Background(), nil, "http://example.com", []Query{{"up", "up"}}, streamTestOptions(), dir); err == nil {
			t.Fatal("existing/missing parent accepted")
		}
	}
	clock := &streamClock{time: testStart}
	dir := filepath.Join(parent, "stream")
	wait := func(ctx context.Context, d time.Duration) error {
		if err := os.Mkdir(filepath.Join(dir, "chunk-000000"), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "chunk-000000", streamWindowFilename), []byte("existing"), 0600); err != nil {
			return err
		}
		return clock.wait(ctx, d)
	}
	result, err := stream(context.Background(), nil, "http://example.com", []Query{{"up", "up"}}, streamTestOptions(), dir, clock.now, wait)
	if err == nil || result.CompletedChunks != 0 {
		t.Fatal("chunk collision accepted")
	}
	b, err := os.ReadFile(filepath.Join(dir, "chunk-000000", streamWindowFilename))
	if err != nil || string(b) != "existing" {
		t.Fatal("existing chunk overwritten")
	}
	streamCheckResult(t, dir, result)
}

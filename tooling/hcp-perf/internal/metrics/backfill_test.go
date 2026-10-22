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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func checkBackfillCheckpoint(t *testing.T, dir string, result BackfillResult) {
	t.Helper()
	var persisted BackfillResult
	streamReadJSON(t, filepath.Join(dir, "manifest.json"), &persisted)
	if !reflect.DeepEqual(persisted, result) {
		t.Fatalf("returned result differs from checkpoint: %+v / %+v", result, persisted)
	}
	for i := 0; i < result.CompletedChunks; i++ {
		var window StreamChunk
		streamReadJSON(t, filepath.Join(dir, result.Windows[i].Directory, backfillWindowFilename), &window)
		if !reflect.DeepEqual(window, result.Windows[i]) {
			t.Fatalf("window %d differs from checkpoint", i)
		}
	}
}

func TestBackfillStaticWindows(t *testing.T) {
	for _, duration := range []time.Duration{0, 2 * time.Second, time.Minute, 62 * time.Second, 125 * time.Second, 2 * time.Hour} {
		t.Run(duration.String(), func(t *testing.T) {
			// A future, non-wall-clock-aligned start also proves there is no live wait.
			start := time.Date(2099, 1, 1, 0, 0, 2, 0, time.UTC)
			o := BackfillOptions{Start: start, End: start.Add(duration), Step: 5 * time.Second, Chunk: time.Minute}
			dir := filepath.Join(t.TempDir(), "backfill")
			queries := []Query{{"window", "latency_sum"}, {"stream-window", "latency_bucket"}}
			calls := 0
			points := map[time.Time]bool{}
			bodies := map[string]string{}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				index := calls / len(queries)
				var checkpoint BackfillResult
				streamReadJSON(t, filepath.Join(dir, "manifest.json"), &checkpoint)
				if checkpoint.Status != "running" || checkpoint.CompletedChunks != index || len(checkpoint.Windows) != checkpoint.PlannedChunks || !checkpoint.Start.Equal(start) || !checkpoint.End.Equal(o.End) || checkpoint.PendingChunks != checkpoint.PlannedChunks-index || !reflect.DeepEqual(checkpoint.Queries, queries) || checkpoint.Updated.IsZero() {
					t.Fatalf("plan/checkpoint not persisted before request: %+v", checkpoint)
				}
				window := checkpoint.Windows[index]
				if window.Status != StatusPending || !window.Start.Equal(start.Add(time.Duration(index)*o.Chunk)) {
					t.Fatalf("unexpected static window: %+v", window)
				}
				wantEnd := window.Start.Add(o.Chunk)
				if wantEnd.After(o.End) {
					wantEnd = o.End
				}
				query := queries[calls%len(queries)]
				if r.URL.Path != "/prom/api/v1/query_range" || r.URL.Query().Get("start") != window.Start.Format(time.RFC3339Nano) || r.URL.Query().Get("end") != wantEnd.Format(time.RFC3339Nano) || r.URL.Query().Get("step") != "5" || r.URL.Query().Get("query") != query.Expr {
					t.Fatalf("query changed or outside window: %s", r.URL)
				}
				for ts := window.Start; !ts.After(wantEnd); ts = ts.Add(o.Step) {
					if ts.After(o.End) || ts.Sub(start)%o.Step != 0 {
						t.Fatal("overshoot or evaluation grid shifted")
					}
					points[ts] = true
				}
				body := streamResponse(r, fmt.Sprintf("raw-label-%d", index), "1")
				body = strings.Replace(body, `"metric":{`, `"metric":{"__name__":"latency_bucket","le":"0.5","namespace":"customer","pod":"pod-123",`, 1)
				bodies[filepath.Join(window.Directory, query.Name+".json")] = body
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			result, err := Backfill(t.Context(), client, "https://example.com/prom", queries, o, dir)
			wantChunks := max(1, int((duration+o.Chunk-1)/o.Chunk))
			if err != nil || result.Status != "complete" || result.DataQuality != StatusSuccess || result.ErrorCount != 0 || result.CompletedChunks != wantChunks || result.PendingChunks != 0 || calls != wantChunks*len(queries) || !result.NextStart.Equal(o.End) {
				t.Fatalf("unexpected result: %+v %v calls=%d", result, err, calls)
			}
			if len(points) != int(duration/o.Step)+1 || points[o.End] != (duration%o.Step == 0) {
				t.Fatal("deduplicated evaluation grid does not match one full range query")
			}
			checkBackfillCheckpoint(t, dir, result)
			for _, window := range result.Windows {
				m := loadManifest(t, filepath.Join(dir, window.Directory))
				for _, q := range m.Queries {
					body, err := os.ReadFile(filepath.Join(dir, window.Directory, q.File))
					hash := sha256.Sum256(body)
					if err != nil || q.SHA256 != hex.EncodeToString(hash[:]) || string(body) != bodies[filepath.Join(window.Directory, q.File)] {
						t.Fatalf("raw response or labels modified, missing or hash mismatch: %s %v", q.File, err)
					}
				}
			}
		})
	}
}

func TestBackfillFailureContinues(t *testing.T) {
	for _, failure := range []string{"http422", "transport", "warning", "missing"} {
		t.Run(failure, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "backfill")
			calls := 0
			var firstBody string
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				body := streamResponse(r, "raw", "1")
				status := 200
				if calls == 1 {
					switch failure {
					case "http422":
						status, body = 422, `{"status":"error","error":"sample limit secret-credential"}`
					case "transport":
						return nil, errors.New("https://user:secret-credential@example.com?token=secret-credential")
					case "warning":
						body = strings.Replace(body, `"status":"success"`, `"status":"success","warnings":["partial response"]`, 1)
					case "missing":
						body = `{"status":"success","data":{"resultType":"matrix","result":[]}}`
					}
					firstBody = body
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			o := BackfillOptions{Start: testStart, End: testStart.Add(2 * time.Minute), Step: 5 * time.Second, Chunk: time.Minute}
			result, err := Backfill(context.Background(), client, "http://example.com", []Query{{"raw", "latency_sum"}}, o, dir)
			wantErrors := 1
			if failure == "warning" {
				wantErrors = 0
			}
			if (err != nil) != (wantErrors > 0) || result.ErrorCount != wantErrors || result.Status != "complete" || result.DataQuality != StatusIncomplete || calls != 2 || result.Windows[1].Status != StatusSuccess {
				t.Fatalf("failure retried, stopped or hidden: %+v %v calls=%d", result, err, calls)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "secret-credential") || (err != nil && strings.Contains(err.Error(), "secret-credential")) {
				t.Fatal("credentials leaked in result/error")
			}
			if firstBody != "" {
				body, err := os.ReadFile(filepath.Join(dir, "chunk-000000", "raw.json"))
				if err != nil || string(body) != firstBody {
					t.Fatal("failed window raw evidence lost")
				}
				m := loadManifest(t, filepath.Join(dir, "chunk-000000"))
				hash := sha256.Sum256(body)
				if m.Queries[0].SHA256 != hex.EncodeToString(hash[:]) {
					t.Fatal("failed window hash missing")
				}
			}
			checkBackfillCheckpoint(t, dir, result)
		})
	}
}

func TestBackfillCancellation(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(fmt.Sprint(during), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if !during {
				cancel()
			}
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: streamCancelBody{ReadCloser: io.NopCloser(strings.NewReader(`{"status":`)), cancel: cancel}}, nil
			})}
			dir := filepath.Join(t.TempDir(), "backfill")
			o := BackfillOptions{Start: testStart, End: testStart.Add(2 * time.Hour), Step: 5 * time.Second, Chunk: time.Minute}
			result, err := Backfill(ctx, client, "http://example.com", []Query{{"raw", "latency_sum"}}, o, dir)
			wantCalls := 0
			if during {
				wantCalls = 1
			}
			if !errors.Is(err, context.Canceled) || result.Status != "incomplete" || result.DataQuality != StatusIncomplete || calls != wantCalls || result.CompletedChunks != wantCalls || result.PendingChunks != 120-wantCalls || len(result.Windows) != 120 {
				t.Fatalf("partial cancellation not preserved: %+v %v", result, err)
			}
			if during {
				m := loadManifest(t, filepath.Join(dir, "chunk-000000"))
				if m.Queries[0].SHA256 == "" {
					t.Fatal("partial response not hashed")
				}
			}
			checkBackfillCheckpoint(t, dir, result)
		})
	}
}

func TestBackfillValidationAndStorageGuards(t *testing.T) {
	base := BackfillOptions{Start: testStart, End: testStart.Add(2 * time.Hour), Step: 5 * time.Second, Chunk: time.Minute}
	for _, change := range []func(*BackfillOptions){
		func(o *BackfillOptions) { o.Start = time.Time{} },
		func(o *BackfillOptions) { o.End = o.Start.Add(-time.Second) },
		func(o *BackfillOptions) { o.End = o.Start.Add(24*time.Hour + time.Second) },
		func(o *BackfillOptions) { o.Chunk = 0 },
		func(o *BackfillOptions) { o.Chunk = 61 * time.Second },
		func(o *BackfillOptions) { o.Chunk = 6 * time.Minute },
		func(o *BackfillOptions) { o.Step = 0 },
		func(o *BackfillOptions) { o.Step = time.Millisecond },
		func(o *BackfillOptions) { o.Step = time.Second + time.Nanosecond },
		func(o *BackfillOptions) { o.Chunk = 5 * time.Second; o.End = o.Start.Add(2*time.Hour + time.Second) },
	} {
		o := base
		change(&o)
		dir := filepath.Join(t.TempDir(), "backfill")
		if _, err := Backfill(context.Background(), nil, "http://example.com", []Query{{"raw", "up"}}, o, dir); err == nil {
			t.Fatalf("invalid options accepted: %+v", o)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid options created output")
		}
	}
	for _, endpoint := range []string{"https://user:secret@example.com", "https://example.com?token=secret", "https://example.com#secret", "/relative", "https://%secret"} {
		dir := filepath.Join(t.TempDir(), "backfill")
		if _, err := Backfill(context.Background(), nil, endpoint, []Query{{"raw", "up"}}, base, dir); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid endpoint accepted/leaked: %v", err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid endpoint created output")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	base.End = base.Start.Add(24 * time.Hour)
	dir := filepath.Join(t.TempDir(), "backfill")
	result, err := Backfill(ctx, nil, "http://example.com", []Query{{"raw", "up"}}, base, dir)
	if !errors.Is(err, context.Canceled) || result.PlannedChunks != 1440 {
		t.Fatalf("maximum bounded plan rejected: %+v %v", result, err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Backfill(ctx, nil, "http://example.com", []Query{{"raw", "up"}}, base, dir); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing output accepted: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if string(before) != string(after) {
		t.Fatal("existing output overwritten")
	}
	if queryName.MatchString(strings.TrimSuffix(backfillWindowFilename, ".json")) {
		t.Fatal("metadata can collide with query filenames")
	}
}

func TestBackfillWindowMetadataNoOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backfill")
	path := filepath.Join(dir, "chunk-000000", backfillWindowFilename)
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(streamResponse(r, "raw", "1")))}, nil
	})}
	o := BackfillOptions{Start: testStart, End: testStart.Add(2 * time.Minute), Step: 5 * time.Second, Chunk: time.Minute}
	result, err := Backfill(context.Background(), client, "http://example.com", []Query{{"window", "up"}}, o, dir)
	if !errors.Is(err, os.ErrExist) || calls != 1 || result.Status != "incomplete" || result.CompletedChunks != 0 {
		t.Fatalf("storage collision ignored: %+v %v", result, err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "existing" {
		t.Fatal("existing metadata overwritten")
	}
	checkBackfillCheckpoint(t, dir, result)
}

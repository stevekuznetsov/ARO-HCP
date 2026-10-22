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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
)

func TestMetricsStreamCLIOptionsAndFinalOutput(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "explicit"}[explicit], func(t *testing.T) {
			t.Setenv("HCP_PERF_BEARER_TOKEN", "secret-token")
			queriesFile := filepath.Join(t.TempDir(), "queries.json")
			if err := os.WriteFile(queriesFile, []byte(`[{"name":"up","expr":"up"}]`), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"metrics", "stream", "--endpoint", "https://example.com/prom", "--queries", queriesFile, "--output", "new-dir"}
			want := metrics.StreamOptions{Duration: 6 * time.Hour, Chunk: 30 * time.Second, Step: 5 * time.Second, Lag: 15 * time.Second, Drain: 90 * time.Second, ChunkTimeout: 90 * time.Second}
			if explicit {
				args = append(args, "--duration", "10m", "--chunk", "20s", "--step", "2s", "--lag", "0s", "--drain", "1m", "--chunk-timeout", "10s")
				want = metrics.StreamOptions{Duration: 10 * time.Minute, Chunk: 20 * time.Second, Step: 2 * time.Second, Drain: time.Minute, ChunkTimeout: 10 * time.Second}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out bytes.Buffer
			result := metrics.StreamResult{Version: 1, Status: "incomplete", CompletedChunks: 2, PendingChunks: 1, ErrorCount: 1}
			called := false
			err := runMetricsStreamWith(ctx, args, &out, func(gotCtx context.Context, client *http.Client, endpoint string, queries []metrics.Query, options metrics.StreamOptions, dir string) (metrics.StreamResult, error) {
				called = true
				if gotCtx != ctx || endpoint != "https://example.com/prom" || dir != "new-dir" || !reflect.DeepEqual(queries, []metrics.Query{{Name: "up", Expr: "up"}}) || options != want {
					t.Fatalf("unexpected stream arguments: %s %s %+v %+v", endpoint, dir, queries, options)
				}
				transport, ok := client.Transport.(bearerTransport)
				if !ok || transport.token != "secret-token" || client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
					t.Fatal("parent authentication/redirect policy not reused")
				}
				if out.Len() != 0 {
					t.Fatal("stdout emitted before stream finished")
				}
				return result, context.Canceled
			})
			if !called || !errors.Is(err, context.Canceled) {
				t.Fatalf("stream result error lost: %v", err)
			}
			decoder := json.NewDecoder(&out)
			var got metrics.StreamResult
			if err := decoder.Decode(&got); err != nil || !reflect.DeepEqual(got, result) {
				t.Fatalf("partial final output lost: %+v %v", got, err)
			}
			if err := decoder.Decode(&got); err != io.EOF {
				t.Fatal("stdout contains more than one JSON result")
			}
		})
	}
}

func TestMetricsStreamCLIRejectsInput(t *testing.T) {
	queries := filepath.Join(t.TempDir(), "queries.json")
	if err := os.WriteFile(queries, []byte(`[{"name":"up","expr":"up"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"metrics", "stream", "--endpoint", "http://example.com", "--queries", queries, "--output", "new-dir"}
	for _, args := range [][]string{
		nil, {"metrics", "export"}, {"metrics", "stream"},
		append(append([]string(nil), base...), "extra"),
		append(append([]string(nil), base...), "--start", "2026-09-09T00:00:00Z"),
		append(append([]string(nil), base...), "--duration", "NaN"),
		append(append([]string(nil), base...), "--endpoint", ""),
	} {
		var out bytes.Buffer
		err := runMetricsStreamWith(context.Background(), args, &out, func(context.Context, *http.Client, string, []metrics.Query, metrics.StreamOptions, string) (metrics.StreamResult, error) {
			t.Fatal("invalid input invoked stream")
			return metrics.StreamResult{}, nil
		})
		if err == nil || out.Len() != 0 {
			t.Fatalf("invalid input accepted: %v, %v", args, err)
		}
	}
	for _, body := range []string{`[{"name":"up"}]`, `[{"name":"up","expr":"up","extra":true}]`, `[{"name":"up","name":"other","expr":"up"}]`, `null`, `[] []`} {
		if err := os.WriteFile(queries, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := runMetricsStream(context.Background(), base, io.Discard); err == nil {
			t.Fatalf("invalid queries accepted: %s", body)
		}
	}
}

func TestMetricsStreamCLICanceledPersistsFinal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	parent := t.TempDir()
	queries := filepath.Join(parent, "queries.json")
	if err := os.WriteFile(queries, []byte(`[{"name":"up","expr":"up"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "stream")
	var out bytes.Buffer
	err := runMetricsStream(ctx, []string{"metrics", "stream", "--endpoint", "http://example.com", "--queries", queries, "--output", dir, "--duration", "10m"}, &out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	var result, persisted metrics.StreamResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &persisted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, persisted) || result.Status != "incomplete" || result.PendingChunks != 20 || result.CompletedChunks != 0 {
		t.Fatalf("final result not durable: %+v %+v", result, persisted)
	}
}

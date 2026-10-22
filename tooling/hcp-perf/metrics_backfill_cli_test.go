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

func TestMetricsBackfillCLIOptions(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "explicit"}[explicit], func(t *testing.T) {
			t.Setenv("HCP_PERF_BEARER_TOKEN", "secret-token")
			queriesFile := filepath.Join(t.TempDir(), "queries.json")
			if err := os.WriteFile(queriesFile, []byte(`[{"name":"raw","expr":"latency_sum"}]`), 0600); err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 9, 9, 1, 0, 2, 0, time.UTC)
			end := start.Add(2 * time.Hour)
			args := []string{"metrics", "backfill", "--endpoint", "https://example.com/prom", "--queries", queriesFile, "--output", "new-dir", "--start", start.Format(time.RFC3339Nano), "--end", end.Format(time.RFC3339Nano)}
			want := metrics.BackfillOptions{Start: start, End: end, Step: 5 * time.Second, Chunk: time.Minute}
			timeout := 30 * time.Minute
			if explicit {
				args = append(args, "--chunk", "30s", "--step", "2s", "--timeout", "24h")
				want.Step, want.Chunk, timeout = 2*time.Second, 30*time.Second, 24*time.Hour
			}
			var out bytes.Buffer
			result := metrics.BackfillResult{StreamResult: metrics.StreamResult{Version: 1, Status: "incomplete", CompletedChunks: 2, PendingChunks: 1, ErrorCount: 1}}
			called := false
			before := time.Now()
			err := runMetricsBackfillWith(context.Background(), args, &out, func(ctx context.Context, client *http.Client, endpoint string, queries []metrics.Query, options metrics.BackfillOptions, dir string) (metrics.BackfillResult, error) {
				called = true
				if options != want || endpoint != "https://example.com/prom" || dir != "new-dir" || !reflect.DeepEqual(queries, []metrics.Query{{Name: "raw", Expr: "latency_sum"}}) {
					t.Fatalf("unexpected backfill arguments: %+v %s %s %+v", options, endpoint, dir, queries)
				}
				deadline, ok := ctx.Deadline()
				if !ok || deadline.Before(before.Add(timeout)) || deadline.After(time.Now().Add(timeout)) {
					t.Fatalf("whole run timeout not applied: %s", deadline)
				}
				transport, ok := client.Transport.(bearerTransport)
				if !ok || transport.token != "secret-token" || client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
					t.Fatal("authentication/redirect policy not reused")
				}
				if out.Len() != 0 {
					t.Fatal("output emitted before backfill finished")
				}
				return result, context.Canceled
			})
			if !called || !errors.Is(err, context.Canceled) {
				t.Fatalf("partial result error lost: %v", err)
			}
			decoder := json.NewDecoder(&out)
			var got metrics.BackfillResult
			if err := decoder.Decode(&got); err != nil || !reflect.DeepEqual(got, result) {
				t.Fatalf("partial final output lost: %+v %v", got, err)
			}
			if err := decoder.Decode(&got); err != io.EOF {
				t.Fatal("stdout contains more than one JSON result")
			}
		})
	}
}

func TestMetricsBackfillCLIRejectsInput(t *testing.T) {
	queries := filepath.Join(t.TempDir(), "queries.json")
	if err := os.WriteFile(queries, []byte(`[{"name":"raw","expr":"latency_sum"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"metrics", "backfill", "--endpoint", "http://example.com", "--queries", queries, "--output", "new-dir", "--start", "2026-09-09T00:00:00Z", "--end", "2026-09-09T02:00:00Z"}
	for _, extra := range [][]string{
		{"extra"}, {"--duration", "2h"}, {"--lag", "0s"}, {"--chunk", "NaN"},
		{"--start", "invalid"}, {"--end", "invalid"}, {"--start", ""}, {"--end", ""},
		{"--endpoint", ""}, {"--queries", ""}, {"--output", ""},
		{"--timeout", "0s"}, {"--timeout", "-1s"}, {"--timeout", "24h1s"},
	} {
		args := append(append([]string(nil), base...), extra...)
		var out bytes.Buffer
		err := runMetricsBackfillWith(context.Background(), args, &out, func(context.Context, *http.Client, string, []metrics.Query, metrics.BackfillOptions, string) (metrics.BackfillResult, error) {
			t.Fatal("invalid input invoked backfill")
			return metrics.BackfillResult{}, nil
		})
		if err == nil || out.Len() != 0 {
			t.Fatalf("invalid input accepted: %v %v", args, err)
		}
	}
	for _, args := range [][]string{nil, {"metrics", "export"}, {"metrics", "backfill"}} {
		if err := runMetricsBackfill(context.Background(), args, io.Discard); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
	for _, body := range []string{`[{"name":"up"}]`, `[{"name":"up","expr":"up","extra":true}]`, `[{"name":"up","name":"other","expr":"up"}]`, `null`, `[] []`} {
		if err := os.WriteFile(queries, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := runMetricsBackfill(context.Background(), base, io.Discard); err == nil {
			t.Fatalf("invalid queries accepted: %s", body)
		}
	}
}

func TestMetricsBackfillCLICancellationAndParentDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	cancel()
	parent := t.TempDir()
	queries := filepath.Join(parent, "queries.json")
	if err := os.WriteFile(queries, []byte(`[{"name":"raw","expr":"latency_sum"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "backfill")
	var out bytes.Buffer
	err := runMetricsBackfill(ctx, []string{"metrics", "backfill", "--endpoint", "http://example.com", "--queries", queries, "--output", dir, "--start", "2026-09-09T00:00:00Z", "--end", "2026-09-09T02:00:00Z"}, &out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	var result, persisted metrics.BackfillResult
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
	if !reflect.DeepEqual(result, persisted) || result.Status != "incomplete" || result.PendingChunks != 120 || len(result.Windows) != 120 || result.CompletedChunks != 0 || !result.Deadline.Equal(parentDeadline) {
		t.Fatalf("final result not durable or parent deadline extended: %+v", result)
	}
}

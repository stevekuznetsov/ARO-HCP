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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/record"
)

func TestRecordHealthCLI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"spec":{"version":"v1","namespaces":["hcp"],"outputDir":"SECRET"},"startedAt":"2026-01-01T00:00:00Z"}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Parent dispatch must select the local primitive, not load cluster credentials.
	t.Setenv("KUBECONFIG", "/missing/SECRET")
	args := []string{"record", "health", "--input", dir}
	var out bytes.Buffer
	if err := run(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	var result record.HealthResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Version != "v1" || result.Status != "unknown" || result.Final != nil || result.Checkpoint != nil || strings.Contains(out.String(), "SECRET") {
		t.Fatalf("unexpected output: %s", out.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, args, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if err := run(context.Background(), args, healthErrorWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writer failure ignored: %v", err)
	}
	if !strings.Contains(usage, "record health --input DIR") {
		t.Fatal("missing health command help")
	}
}

func TestRecordHealthCLIArguments(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		nil, {"record"}, {"record", "status"}, {"health", "--input", dir},
		{"record", "health"}, {"record", "health", "--input"},
		{"record", "health", "--input", dir, "extra"},
		{"record", "health", "--input", dir, "--kubeconfig", "SECRET"},
		{"record", "health", "--input", dir, "--follow"},
		{"record", "health", "--input", dir},
		{"record", "health", "--input", filepath.Join(dir, "missing")},
	} {
		var out bytes.Buffer
		if err := runRecordHealth(context.Background(), args, &out); err == nil {
			t.Fatalf("accepted invalid arguments/input: %v", args)
		}
		if out.Len() != 0 {
			t.Fatalf("emitted output for invalid arguments/input: %v", args)
		}
	}
}

func TestRecordHealthRequireReady(t *testing.T) {
	t.Setenv("KUBECONFIG", "/missing/SECRET")
	for _, mode := range []string{"missing-manifest", "manifest-only", "partial-checkpoint", "ready", "no-polls", "api-error", "missing-stream", "missing-namespace", "duplicate-stream", "no-samples", "no-success", "old-success", "stream-error", "unfinished", "shutdown", "final"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			args := []string{"record", "health", "--input", dir, "--require-ready"}
			started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			finished := started.Add(time.Second)
			write := func(name string, value any, newline bool) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if newline {
					data = append(data, '\n')
				}
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "missing-manifest" {
				write("manifest.json", map[string]any{"spec": map[string]any{"version": "v1", "namespaces": []string{"hcp", "other"}, "enableWatch": true}, "startedAt": started}, true)
			}
			checkpoint := record.HealthCheckpoint{Type: "poll", PollStartedAt: started, PollFinishedAt: finished,
				Result: record.HealthSummary{Version: "v1", StartedAt: started, Polls: 1, Shutdown: "running"}}
			for _, namespace := range []string{"hcp", "other"} {
				for _, resource := range []string{"pods", "events", "deployments", "statefulsets"} {
					checkpoint.Result.Streams = append(checkpoint.Result.Streams, record.StreamResult{Namespace: namespace, Resource: resource, Samples: 1, LastSuccess: &finished})
				}
			}
			switch mode {
			case "no-polls":
				checkpoint.Result.Polls = 0
			case "api-error":
				checkpoint.Result.APIErrors = 1
			case "missing-stream":
				checkpoint.Result.Streams = checkpoint.Result.Streams[:7]
			case "missing-namespace":
				checkpoint.Result.Streams = checkpoint.Result.Streams[:4]
			case "duplicate-stream":
				checkpoint.Result.Streams[7] = checkpoint.Result.Streams[0]
			case "no-samples":
				checkpoint.Result.Streams[0].Samples = 0
			case "no-success":
				checkpoint.Result.Streams[0].LastSuccess = nil
			case "old-success":
				old := started.Add(-time.Second)
				checkpoint.Result.Streams[0].LastSuccess = &old
			case "stream-error":
				checkpoint.Result.Streams[0].APIErrors = 1
			case "unfinished":
				checkpoint.PollFinishedAt = time.Time{}
			case "shutdown":
				checkpoint.Type, checkpoint.Result.Shutdown = "shutdown", "canceled"
			case "final":
				write("summary.json", checkpoint.Result, true)
			}
			if mode != "manifest-only" {
				write("checkpoints.ndjson", checkpoint, mode != "partial-checkpoint")
			}
			var out bytes.Buffer
			err := run(t.Context(), args, &out)
			if (err == nil) != (mode == "ready") {
				t.Fatalf("unexpected readiness result: %v, output %s", err, out.String())
			}
			if mode != "ready" && out.Len() != 0 {
				t.Fatal("unready probe emitted success output")
			}
		})
	}
}

type healthErrorWriter struct{}

func (healthErrorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

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
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/retention"
)

func TestRetentionCLI(t *testing.T) {
	for _, dry := range []bool{false, true} {
		args := []string{"environment", "refresh", "--input", "examples/retention.json", "--timeout", "2m"}
		if dry {
			args = append(args, "--dry-run")
		}
		for _, operationErr := range []error{nil, &retention.Failure{Code: "created_at_mismatch"}} {
			var out bytes.Buffer
			want := retention.Result{Version: "v1", Status: "verified", DryRun: dry}
			if operationErr != nil {
				want.Status = "failed"
			}
			err := runRetentionWith(context.Background(), args, &out, func(ctx context.Context, spec retention.Spec, dryRun bool) (retention.Result, error) {
				if spec.Version != "v1" || len(spec.ResourceGroups) != 3 || dryRun != dry {
					t.Fatalf("bad API input: %+v dry=%v", spec, dryRun)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Minute || time.Until(deadline) < time.Minute {
					t.Fatal("timeout not passed through")
				}
				return want, operationErr
			})
			if !errors.Is(err, operationErr) {
				t.Fatalf("operation error lost: %v", err)
			}
			var got retention.Result
			if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Status != want.Status || got.DryRun != dry {
				t.Fatalf("partial JSON missing: %s %v", out.String(), err)
			}
		}
	}
}

func TestRetentionCLIRejectsArgumentsAndInput(t *testing.T) {
	noRefresh := func(context.Context, retention.Spec, bool) (retention.Result, error) {
		t.Fatal("invalid CLI input reached refresh")
		return retention.Result{}, nil
	}
	for _, args := range [][]string{
		nil, {"environment"}, {"environment", "list"}, {"refresh"}, {"environment", "refresh"},
		{"environment", "refresh", "--input"},
		{"environment", "refresh", "--input", "examples/retention.json", "extra"},
		{"environment", "refresh", "--input", "examples/retention.json", "--subscription", "all"},
		{"environment", "refresh", "--input", "examples/retention.json", "--timeout", "0"},
		{"environment", "refresh", "--input", "examples/retention.json", "--timeout", "-1m"},
		{"environment", "refresh", "--input", "examples/retention.json", "--timeout", "11m"},
		{"environment", "refresh", "--input", "examples/retention.json", "--timeout", "bad"},
		{"environment", "refresh", "--input", "does-not-exist.json"},
	} {
		var out bytes.Buffer
		if err := runRetentionWith(context.Background(), args, &out, noRefresh); err == nil || out.Len() != 0 {
			t.Fatalf("accepted %v: %v %s", args, err, out.String())
		}
	}
	path := filepath.Join(t.TempDir(), "input.json")
	for _, input := range []string{
		`{}`, `null`, `{"version":"v1"}`, `{"resourceGroups":[]}`,
		`{"version":"v1","resourceGroups":null}`,
		`{"version":"v1","resourceGroups":[],"subscription":"all"}`,
		`{"version":"v1","resourceGroups":[],"version":"v1"}`,
		`{"version":"v1","resourceGroups":[],"Version":"v2"}`,
		`{"version":"v1","resourceGroups":[]} {}`,
		`{"version":"v1","resourceGroups":[12]}`,
	} {
		if err := os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if err := runRetentionWith(context.Background(), []string{"environment", "refresh", "--input", path}, io.Discard, noRefresh); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestRetentionCLICancellationAndWriterFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	args := []string{"environment", "refresh", "--input", "examples/retention.json"}
	if err := runRetention(ctx, args, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	operationErr := &retention.Failure{Code: "azure_cli_failed"}
	err := runRetentionWith(context.Background(), args, retentionErrorWriter{}, func(context.Context, retention.Spec, bool) (retention.Result, error) {
		return retention.Result{Version: "v1", Status: "failed"}, operationErr
	})
	if !errors.Is(err, io.ErrClosedPipe) || !errors.Is(err, operationErr) {
		t.Fatalf("lost operation or writer error: %v", err)
	}
}

type retentionErrorWriter struct{}

func (retentionErrorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

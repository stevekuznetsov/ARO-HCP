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

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/progress"
)

func TestProgressCLI(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runProgress(context.Background(), []string{"case", "progress", "--input", dir}, &out); err != nil {
		t.Fatal(err)
	}
	var r progress.Result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Version != "v1" || r.Phase != "incomplete" || !r.Provisional {
		t.Fatalf("unexpected result: %+v", r)
	}
	if err := os.Mkdir(filepath.Join(dir, "evidence"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "evidence/outcome.json"), []byte(`{"exitCode":`), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runProgress(context.Background(), []string{"case", "progress", "--input", dir}, &out); err != nil {
		t.Fatalf("partial artifact should be a warning: %v", err)
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) == 0 || r.Phase == "failed" {
		t.Fatalf("bad partial snapshot: %+v", r)
	}
}

func TestProgressCLIArguments(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		nil, {"case"}, {"case", "status", "--input", dir}, {"progress", "--input", dir},
		{"case", "progress"}, {"case", "progress", "--input"},
		{"case", "progress", "--input", dir, "--follow"},
		{"case", "progress", "--input", dir, "--kubeconfig", "SECRET"},
		{"case", "progress", "--input", dir, "extra"},
		{"case", "progress", "--input", filepath.Join(dir, "missing")},
	} {
		var out bytes.Buffer
		if err := runProgress(context.Background(), args, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
		if out.Len() != 0 {
			t.Fatalf("output on invalid argv: %v", args)
		}
	}
}

func TestProgressCLICancelAndWriterError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	args := []string{"case", "progress", "--input", t.TempDir()}
	if err := runProgress(ctx, args, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if err := runProgress(context.Background(), args, progressErrorWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writer error ignored: %v", err)
	}
}

type progressErrorWriter struct{}

func (progressErrorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

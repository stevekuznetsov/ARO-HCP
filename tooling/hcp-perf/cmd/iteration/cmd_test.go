// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iteration

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

	"github.com/spf13/pflag"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/cost"
	controller "github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/iteration"
)

func execute(ctx context.Context, out io.Writer, args ...string) error {
	command := NewCommand()
	command.SetContext(ctx)
	command.SetOut(out)
	command.SetErr(out)
	command.SetArgs(args)
	return command.Execute()
}

func TestRejectFlagsBeforeCreatingRoot(t *testing.T) {
	for _, args := range [][]string{
		{"submit"},
		{"submit", "--input", "missing.json"},
		{"submit", "--id", "run"},
		{"submit", "--id", "../escape", "--input", "missing.json"},
		{"submit", "--id", "run", "--input", "missing.json", "--unknown"},
		{"status", "positional"},
		{"status", "--input", "missing.json"},
		{"list", "--id", "run"},
		{"watch"},
		{"watch", "--id", "run", "--interval", "0s"},
		{"stop"},
		{"reconcile", "--id", "../escape"},
		{"reconcile-operation", "--id", "run"},
		{"reconcile-operation", "--id", "run", "--action", "create", "--timeout", "0s"},
		{"reconcile-operation", "--id", "run", "--action", "create", "--timeout", "31m"},
		{"serve", "--interval", "-1s"},
		{"worker"},
		{"worker", "--attempt", "../escape"},
		{"retry-observation"},
		{"retry-observation", "--id", "run", "--action", "observe"},
		{"retry-observation", "--id", "run", "--action", "../observe", "--reason", "fix"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			var out bytes.Buffer
			err := execute(context.Background(), &out, append(args, "--root", root)...)
			if err == nil {
				t.Fatal("expected invalid flags to fail")
			}
			if out.Len() != 0 {
				t.Fatalf("Cobra must not duplicate outer error output: %s", out.String())
			}
			if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid flags created state: %v", err)
			}
		})
	}
}

func TestOptionsPhases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	raw := NewRawOptions(Submit)
	flags := pflag.NewFlagSet("submit", pflag.ContinueOnError)
	raw.BindOptions(flags)
	if err := flags.Parse([]string{"--input", "missing.json", "--id", "demo"}); err != nil {
		t.Fatal(err)
	}
	validated, err := raw.Validate(context.Background())
	if err != nil {
		t.Fatalf("Validate should not open missing input: %v", err)
	}
	raw.Input = "../../examples/iteration-offline.json"
	if _, err := validated.Complete(context.Background()); err == nil {
		t.Fatal("validated options should retain their own snapshot of raw flags")
	}
	validated, err = raw.Validate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	options, err := validated.Complete(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(home, ".local", "state", "hcp-perf", "go-iterations")
	if options.root != wantRoot {
		t.Fatalf("default root = %q, want %q", options.root, wantRoot)
	}
	if _, err := os.Stat(wantRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Complete must not create the root: %v", err)
	}
}

func TestStrictInputBeforeCreatingRoot(t *testing.T) {
	example, err := os.ReadFile("../../examples/iteration-offline.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"unknown top-level": []byte(`{"unexpected":true}`),
		"unknown nested":    bytes.Replace(example, []byte(`"cpuMilli": 250`), []byte(`"typoCPU": 250`), 1),
		"missing fields":    []byte(`{"version":"v1"}`),
		"null":              []byte(`null`),
		"trailing value":    append(append([]byte{}, example...), []byte(` {}`)...),
		"oversized":         bytes.Repeat([]byte(" "), maxInputBytes+1),
		"unsupported kind":  bytes.Replace(example, []byte(`"sizing-resolve"`), []byte(`"shell"`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "input.json")
			if err := os.WriteFile(input, data, 0600); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(dir, "state")
			var out bytes.Buffer
			if err := execute(context.Background(), &out, "submit", "--root", root, "--input", input, "--id", "demo"); err == nil {
				t.Fatal("expected invalid JSON/spec to fail")
			}
			if out.Len() != 0 {
				t.Fatalf("unexpected command output: %s", out.String())
			}
			if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid spec created state: %v", err)
			}
		})
	}
}

func TestNestedEvidencePaths(t *testing.T) {
	data, err := os.ReadFile("../../examples/iteration-offline.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec controller.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	abs := filepath.Join(dir, "absolute-evidence.json")
	spec.Actions[1].Cost.DataPoints[0].Evidence = []cost.Evidence{
		{Path: "../evidence/request.json", SHA256: strings.Repeat("a", 64)},
		{Path: abs, SHA256: strings.Repeat("b", 64)},
	}
	data, err = json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input.json")
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	raw := NewRawOptions(Submit)
	raw.Input, raw.ID, raw.Root = input, "demo", filepath.Join(dir, "state")
	validated, err := raw.Validate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	options, err := validated.Complete(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	evidence := options.spec.Actions[1].Cost.DataPoints[0].Evidence
	if want := filepath.Join(dir, "../evidence/request.json"); evidence[0].Path != want || evidence[1].Path != abs {
		t.Fatalf("incorrect resolved evidence paths: %+v", evidence)
	}
}

func TestSubmitStatusStop(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	ctx := context.Background()
	var out bytes.Buffer
	if err := execute(ctx, &out, "submit", "--root", root, "--input", "../../examples/iteration-offline.json", "--id", "demo"); err != nil {
		t.Fatal(err)
	}
	var submitted controller.Run
	if err := json.Unmarshal(out.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	if submitted.State != controller.Prepared || submitted.ID != "demo" || len(submitted.Actions) != 2 {
		t.Fatalf("unexpected submitted run: %+v", submitted)
	}
	for _, action := range submitted.Actions {
		if len(action.Attempts) != 0 {
			t.Fatalf("submit dispatched an action: %+v", action)
		}
	}
	out.Reset()
	if err := execute(ctx, &out, "status", "--root", root, "--id", "demo"); err != nil {
		t.Fatal(err)
	}
	var status controller.Run
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.SpecSHA256 != submitted.SpecSHA256 || status.BinarySHA256 != submitted.BinarySHA256 || status.State != controller.Prepared {
		t.Fatalf("status did not read the persisted submission: %+v", status)
	}
	for _, verb := range []string{"list", "status"} {
		out.Reset()
		if err := execute(ctx, &out, verb, "--root", root); err != nil {
			t.Fatal(err)
		}
		var runs []controller.Run
		if err := json.Unmarshal(out.Bytes(), &runs); err != nil || len(runs) != 1 || runs[0].ID != "demo" {
			t.Fatalf("%s did not list the persisted run: %s, %v", verb, out.String(), err)
		}
	}
	out.Reset()
	if err := execute(ctx, &out, "stop", "--root", root, "--id", "demo"); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || !status.StopRequested {
		t.Fatalf("stop did not persist intent: %s, %v", out.String(), err)
	}
	// With stop already persisted, reconciliation cannot spawn a test executable.
	out.Reset()
	if err := execute(ctx, &out, "reconcile", "--root", root); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := execute(ctx, &out, "watch", "--root", root, "--id", "demo"); err == nil || !strings.Contains(err.Error(), "STOPPED") {
		t.Fatalf("watch must report stopped state as an error: %v", err)
	}
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.State != controller.Stopped {
		t.Fatalf("watch did not emit terminal snapshot: %s, %v", out.String(), err)
	}
}

func TestCanceledCommandDoesNotCreateRoot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := filepath.Join(t.TempDir(), "state")
	if err := execute(ctx, io.Discard, "serve", "--root", root); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled context: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled command created state: %v", err)
	}
}

func TestWorkerCommandAndSuccessfulWatch(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	if err := execute(ctx, io.Discard, "submit", "--root", root, "--input", "../../examples/iteration-offline.json", "--id", "demo"); err != nil {
		t.Fatal(err)
	}
	var tokens []string
	c, err := controller.NewController(root, controller.Options{
		ProcessFactory: func(binary, workerRoot, token string) error {
			if binary != filepath.Join(root, "demo", "worker") || workerRoot != root {
				t.Fatalf("unexpected frozen worker dispatch: %q, %q", binary, workerRoot)
			}
			tokens = append(tokens, token)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Dispatch is injected, but each token runs through the real worker command
	// and core library executor. Only the offline sizing/cost example is executed.
	for i := range 2 {
		if err := c.Reconcile(ctx, "demo"); err != nil {
			t.Fatal(err)
		}
		if len(tokens) != i+1 {
			t.Fatalf("expected one newly eligible action, got tokens %v", tokens)
		}
		if err := execute(ctx, io.Discard, "worker", "--root", root, "--attempt", tokens[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, "demo", tokens[i], "result.json")); err != nil {
			t.Fatalf("worker did not produce its offline result: %v", err)
		}
	}
	var out bytes.Buffer
	if err := execute(ctx, &out, "watch", "--root", root, "--id", "demo"); err != nil {
		t.Fatal(err)
	}
	var run controller.Run
	if err := json.Unmarshal(out.Bytes(), &run); err != nil || run.State != controller.Succeeded {
		t.Fatalf("watch did not report success: %s, %v", out.String(), err)
	}
}

func TestServeHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	root := filepath.Join(t.TempDir(), "state")
	if err := execute(ctx, io.Discard, "serve", "--root", root, "--interval", "1ms"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected serve to return context deadline: %v", err)
	}
}

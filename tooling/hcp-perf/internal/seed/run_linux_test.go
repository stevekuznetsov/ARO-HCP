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

package seed

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Shell scripts are only subprocess fixtures, never the production launcher.
func fixture(t *testing.T, script string) (Spec, Options) {
	t.Helper()
	spec := example(t)
	spec.Applications = 40
	dir := t.TempDir()
	data := []byte("#!/bin/sh\n" + script)
	spec.Binary = Binary{Path: filepath.Join(dir, "binary with spaces"), ExpectedSHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
	if err := os.WriteFile(spec.Binary.Path, data, 0700); err != nil {
		t.Fatal(err)
	}
	opts := Options{KubeconfigPath: filepath.Join(dir, "private credentials;not-a-command"), OutputDir: filepath.Join(dir, "output with spaces")}
	if err := os.WriteFile(opts.KubeconfigPath, []byte("customer-secret-payload"), 0600); err != nil {
		t.Fatal(err)
	}
	return spec, opts
}

func TestRun(t *testing.T) {
	t.Setenv("KUBECONFIG", "/inherited-credentials")
	t.Setenv("AZURE_CLIENT_SECRET", "inherited-secret")
	t.Setenv("LD_PRELOAD", "/inherited-loader")
	spec, opts := fixture(t, `test "$#" = 9 || exit 10
test "$1" = init && test "$2" = -c && test "$3" = config.yaml || exit 11
test "$4" = --kubeconfig && test -f "$5" || exit 12
test "$6" = --uuid && test "$7" = seed-small-001 || exit 13
test "$8" = --timeout && test "$9" = 3600s || exit 14
test -z "$KUBECONFIG$AZURE_CLIENT_SECRET$LD_PRELOAD" || exit 15
test "$HOME" = "$PWD" && test "$TMPDIR" = "$PWD" || exit 16
test -f deployment.yaml && test -f service.yaml && test -f configmap.yaml && test -f secret.yaml || exit 17
printf 'seed stdout\n'
printf 'seed stderr\n' >&2
`)
	result, err := Run(context.Background(), spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Invoked || result.State != "completed" || result.ExitCode != 0 || result.BinarySHA256 != spec.Binary.ExpectedSHA256 || result.FinishedAt.Before(result.StartedAt) {
		t.Fatal(result)
	}
	if result.Warning != noPrePullWarning {
		t.Fatal("missing no-pre-pull warning", result)
	}
	for _, name := range []string{"bundle.json", "result.json"} {
		data, err := os.ReadFile(filepath.Join(opts.OutputDir, name))
		if err != nil {
			t.Fatal(err)
		}
		var saved map[string]any
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if saved["warning"] != noPrePullWarning {
			t.Fatalf("%s missing no-pre-pull warning", name)
		}
	}
	config, err := os.ReadFile(filepath.Join(opts.OutputDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"  preLoadImages: false\n", "  cleanup: false\n", "  gc: false\n", "  churn: false\n", "  jobType: create\n"} {
		if !strings.Contains(string(config), setting) {
			t.Errorf("executed config missing %q", setting)
		}
	}
	for name, want := range map[string]string{"stdout.log": "seed stdout\n", "stderr.log": "seed stderr\n"} {
		data, err := os.ReadFile(filepath.Join(opts.OutputDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q", name, data)
		}
	}
	checkPrivate(t, opts.OutputDir)
	if _, err := os.Stat(filepath.Join(opts.OutputDir, ".kube-burner")); !os.IsNotExist(err) {
		t.Fatal("temporary executable retained", err)
	}
	entries, err := os.ReadDir(opts.OutputDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(opts.OutputDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{opts.KubeconfigPath, filepath.Base(opts.KubeconfigPath), "customer-secret-payload", "inherited-secret", "inherited-credentials"} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s leaked %q", entry.Name(), secret)
			}
		}
	}
	if _, err := Run(context.Background(), spec, opts); err == nil {
		t.Fatal("existing output accepted")
	}
}

func TestRunFailure(t *testing.T) {
	spec, opts := fixture(t, "printf 'partial output\\n'\nprintf 'partial error\\n' >&2\nexit 7\n")
	result, err := Run(context.Background(), spec, opts)
	if err == nil || result.State != "failed" || result.ExitCode != 7 || !result.Invoked {
		t.Fatal(result, err)
	}
	data, readErr := os.ReadFile(filepath.Join(opts.OutputDir, "result.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var saved Result
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.State != "failed" || saved.ExitCode != 7 || saved.Error == "" || saved.Warning != noPrePullWarning || result.Warning != noPrePullWarning {
		t.Fatal(saved)
	}
	data, readErr = os.ReadFile(filepath.Join(opts.OutputDir, "stdout.log"))
	if readErr != nil || string(data) != "partial output\n" {
		t.Fatal(string(data), readErr)
	}
	checkPrivate(t, opts.OutputDir)
}

func TestRefuseBinary(t *testing.T) {
	for _, name := range []string{"mismatch", "missing", "not executable", "directory", "missing kubeconfig"} {
		t.Run(name, func(t *testing.T) {
			spec, opts := fixture(t, "exit 99\n")
			switch name {
			case "mismatch":
				spec.Binary.ExpectedSHA256 = strings.Repeat("a", 64)
			case "missing":
				spec.Binary.Path += ".absent"
			case "not executable":
				if err := os.Chmod(spec.Binary.Path, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				spec.Binary.Path = filepath.Dir(spec.Binary.Path)
			case "missing kubeconfig":
				opts.KubeconfigPath += ".absent"
			}
			result, err := Run(context.Background(), spec, opts)
			if err == nil || result.Invoked || result.State != "failed" {
				t.Fatal(result, err)
			}
			checkPrivate(t, opts.OutputDir)
		})
	}
}

func TestEmpty(t *testing.T) {
	for _, nodes := range []int64{0, 3} {
		t.Run(fmt.Sprint(nodes), func(t *testing.T) {
			spec := example(t)
			spec.GuestNodes = nodes
			spec.Applications = 0
			spec.Binary.Path = "/absent"
			opts := Options{OutputDir: filepath.Join(t.TempDir(), "empty")}
			result, err := Run(context.Background(), spec, opts)
			if err != nil || result.State != "empty" || result.Invoked || result.ExitCode != -1 || result.Warning != noPrePullWarning {
				t.Fatal(result, err)
			}
			checkPrivate(t, opts.OutputDir)
			entries, err := os.ReadDir(opts.OutputDir)
			if err != nil || len(entries) != 3 {
				t.Fatal("empty seed should write only config, bundle, and result", entries, err)
			}
		})
	}
}

func TestCancellation(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			spec, opts := fixture(t, "printf 'started\\n'\nsleep 300 &\nwait\n")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if timeout {
				spec.TimeoutSeconds = 1
			}
			type outcome struct {
				result Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() { r, err := Run(ctx, spec, opts); done <- outcome{r, err} }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				data, _ := os.ReadFile(filepath.Join(opts.OutputDir, "stdout.log"))
				if strings.Contains(string(data), "started") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture never started")
				}
				time.Sleep(10 * time.Millisecond)
			}
			want := context.DeadlineExceeded
			if !timeout {
				cancel()
				want = context.Canceled
			}
			select {
			case got := <-done:
				if !errors.Is(got.err, want) || !got.result.Invoked || got.result.State != "failed" {
					t.Fatal(got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not stop subprocess")
			}
			checkPrivate(t, opts.OutputDir)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	spec, opts := fixture(t, "exit 99\n")
	if _, err := Run(ctx, spec, opts); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(opts.OutputDir); !os.IsNotExist(err) {
		t.Fatal("canceled run created output")
	}
}

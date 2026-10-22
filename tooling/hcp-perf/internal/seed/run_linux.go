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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type Options struct {
	KubeconfigPath string // Caller-owned, never copied into the output or result.
	OutputDir      string // Must not exist; parent must exist and be trusted.
}

type Result struct {
	Version           string    `json:"version"`
	ID                string    `json:"id"`
	KubeBurnerVersion string    `json:"kubeBurnerVersion"`
	BinarySHA256      string    `json:"binarySHA256,omitempty"`
	State             string    `json:"state"` // empty, completed, failed
	StartedAt         time.Time `json:"startedAt"`
	FinishedAt        time.Time `json:"finishedAt"`
	Invoked           bool      `json:"invoked"`
	ExitCode          int       `json:"exitCode"` // -1 when not exited normally (including no invocation).
	Error             string    `json:"error,omitempty"`
	Warning           string    `json:"warning"`
}

// Run executes kube-burner directly, never a shell. A private copy is hashed
// before execution so replacing the configured path cannot bypass the pin.
// The pin is a trust decision by the operator, not a sandbox for hostile code.
// No inherited environment (including KUBECONFIG/cloud credentials) is passed;
// auth plugins must obtain their configuration explicitly from the kubeconfig.
// Logs are raw PRIVATE evidence and may contain sensitive child diagnostics.
// The kubeconfig filename is passed in argv, not written to the bundle/result;
// upstream error diagnostics in the private logs can still mention that path.
// No cluster cleanup or workload rollback occurs, including on timeout or failure.
// Image preloading is disabled: upstream v1.17.3 can adopt a fixed namespace and
// delete all namespaces labeled kube-burner-preload=true. No replacement pre-pull
// is performed; image pulls and warmup must finish outside the scored window.
func Run(ctx context.Context, spec Spec, opts Options) (result Result, err error) {
	if err := spec.Validate(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if opts.OutputDir == "" || (spec.Applications > 0 && opts.KubeconfigPath == "") {
		return result, fmt.Errorf("new outputDir and explicit kubeconfigPath are required for a nonempty seed")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
	defer cancel()
	outDir, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return result, err
	}
	if _, err := Write(spec, outDir); err != nil {
		return result, err
	}
	result = Result{Version: Version, ID: spec.ID, KubeBurnerVersion: KubeBurnerVersion, State: "failed", StartedAt: time.Now().UTC(), ExitCode: -1, Warning: noPrePullWarning}
	defer func() {
		result.FinishedAt = time.Now().UTC()
		if err != nil {
			// Do not serialize errors containing executable/credential paths or
			// arbitrary subprocess output into the machine-readable result.
			result.State = "failed"
			result.Error = "seed failed; inspect private logs and returned error"
		}
		data, marshalErr := json.MarshalIndent(result, "", "  ")
		err = errors.Join(err, marshalErr)
		if marshalErr == nil {
			err = errors.Join(err, writePrivate(filepath.Join(outDir, "result.json"), append(data, '\n')))
		}
	}()
	if spec.Applications == 0 {
		result.State = "empty"
		return result, nil
	}
	kubeconfig, err := filepath.Abs(opts.KubeconfigPath)
	if err != nil {
		return result, fmt.Errorf("resolve caller kubeconfig path")
	}
	info, err := os.Stat(kubeconfig)
	if err != nil || !info.Mode().IsRegular() {
		return result, fmt.Errorf("caller kubeconfig must be an existing regular file")
	}
	// Nonblocking open lets us reject a FIFO without hanging before fstat.
	source, err := os.OpenFile(spec.Binary.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return result, fmt.Errorf("open configured kube-burner executable")
	}
	defer source.Close()
	info, err = source.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return result, fmt.Errorf("configured kube-burner must be a regular executable file")
	}
	binaryPath := filepath.Join(outDir, ".kube-burner")
	binary, err := os.OpenFile(binaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, os.Remove(binaryPath)) }()
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(binary, hash), source)
	if err := errors.Join(copyErr, binary.Sync(), binary.Close()); err != nil {
		return result, err
	}
	result.BinarySHA256 = fmt.Sprintf("%x", hash.Sum(nil))
	if result.BinarySHA256 != spec.Binary.ExpectedSHA256 {
		return result, fmt.Errorf("kube-burner executable SHA-256 mismatch; refusing execution")
	}
	// kube-burner unconditionally creates this file; pre-create it privately so
	// os.Create truncates an existing 0600 file rather than using its 0666 default.
	logfile, err := os.OpenFile(filepath.Join(outDir, "kube-burner-"+spec.ID+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, logfile.Sync(), logfile.Close()) }()
	stdout, err := os.OpenFile(filepath.Join(outDir, "stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, stdout.Sync(), stdout.Close()) }()
	stderr, err := os.OpenFile(filepath.Join(outDir, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, stderr.Sync(), stderr.Close()) }()
	cmd := exec.CommandContext(ctx, binaryPath, "init", "-c", "config.yaml", "--kubeconfig", kubeconfig, "--uuid", spec.ID, "--timeout", fmt.Sprintf("%ds", spec.TimeoutSeconds))
	cmd.Dir = outDir
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + outDir, "TMPDIR=" + outDir}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Kill the entire group, including credential-plugin children. SIGKILL is
	// deliberate: no grace-period cleanup may continue issuing cluster writes.
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		return result, errors.Join(fmt.Errorf("start verified kube-burner executable"), ctx.Err())
	}
	result.Invoked = true
	err = cmd.Wait()
	result.ExitCode = cmd.ProcessState.ExitCode()
	if err = errors.Join(err, ctx.Err()); err != nil {
		return result, fmt.Errorf("kube-burner seed: %w", err)
	}
	result.State = "completed"
	return result, nil
}

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
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/record"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/requesttrace"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func TestTurnoverCLIArguments(t *testing.T) {
	for _, args := range [][]string{
		nil, {"turnover"}, {"turnover", "unknown"}, {"canary", "run"},
		{"turnover", "run"},
		{"turnover", "run", "--input"},
		{"turnover", "run", "--kubeconfig=admin"},
		{"turnover", "run", "extra"},
		{"turnover", "run", "--input=examples/turnover.json", "--output=new"},
		{"turnover", "run", "--input=examples/turnover.json", "--credentials-dir=missing"},
		{"turnover", "run", "--input=missing", "--credentials-dir=missing", "--output=new"},
	} {
		var out bytes.Buffer
		if err := runTurnover(t.Context(), args, &out); err == nil {
			t.Errorf("accepted invalid arguments %v", args)
		}
		if out.Len() != 0 {
			t.Errorf("argument failure wrote stdout: %s", &out)
		}
	}
}

func TestTurnoverCLIZeroApplications(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.json")
	if err := os.WriteFile(input, []byte(`{"version":"v1","guestNodes":3,"applications":0,"seed":42,"namespacePrefix":"apps","durationSeconds":1,"recoverySeconds":0,"maxOutstanding":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "evidence")
	// An unusable credentials directory proves an empty schedule never loads a client.
	args := []string{"turnover", "run", "--input", input, "--credentials-dir", input, "--output", output}
	var out bytes.Buffer
	if err := runTurnover(t.Context(), args, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		workload.TurnoverResult
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Version != "v1" || result.RunID == "" || result.Spec.GuestNodes != 3 || result.Spec.Applications != 0 || result.Spec.TurnoverPeriodSeconds != 3600 || result.Scheduled != 0 || result.Failed != 0 || len(result.Warnings) == 0 || result.FinishedAt.Sub(result.StartedAt) < time.Second {
		t.Fatalf("unexpected result: %s", &out)
	}
	for _, name := range []string{"events.ndjson", "result.json"} {
		info, err := os.Stat(filepath.Join(output, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private evidence %s: %v, %v", name, info, err)
		}
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("private output directory: %v, %v", info, err)
	}
	out.Reset()
	if err := runTurnover(t.Context(), args, &out); err == nil || !json.Valid(out.Bytes()) {
		t.Fatalf("existing output must fail with JSON result: %v, %s", err, &out)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	args[len(args)-1] = filepath.Join(dir, "cancelled")
	out.Reset()
	if err := runTurnover(ctx, args, &out); !errors.Is(err, context.Canceled) || !json.Valid(out.Bytes()) {
		t.Fatalf("cancellation must retain partial JSON: %v, %s", err, &out)
	}
	if err := os.WriteFile(input, []byte(`{"version":"v1","guestNodes":3,"applications":0,"seed":42,"namespacePrefix":"../admin","durationSeconds":1,"recoverySeconds":0,"maxOutstanding":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	args[len(args)-1] = filepath.Join(dir, "invalid-prefix")
	out.Reset()
	if err := runTurnover(t.Context(), args, &out); err == nil || !strings.Contains(err.Error(), "namespacePrefix") || !json.Valid(out.Bytes()) {
		t.Fatalf("unsafe prefix not rejected with JSON result: %v, %s", err, &out)
	}
	if _, err := os.Stat(args[len(args)-1]); !os.IsNotExist(err) {
		t.Fatalf("invalid prefix created evidence: %v", err)
	}
}

func TestTurnoverNamespaceCredentials(t *testing.T) {
	dir := t.TempDir()
	for _, namespace := range []string{"", "../admin", "apps/../../admin", "/admin", `apps\admin`, "UPPER"} {
		if _, err := turnoverNamespaceClient(dir, namespace, nil); err == nil || !strings.Contains(err.Error(), "DNS-label") {
			t.Errorf("unsafe namespace %q: %v", namespace, err)
		}
	}
	path := filepath.Join(dir, "apps-0.kubeconfig")
	if _, err := turnoverNamespaceClient(dir, "apps-0", nil); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), `namespace "apps-0"`) {
		t.Fatalf("missing exact credential location: %v", err)
	}
	if err := os.WriteFile(path, []byte("secret-malformed-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := turnoverNamespaceClient(dir, "apps-0", nil); err == nil || strings.Contains(err.Error(), "secret-malformed-credential") {
		t.Fatalf("malformed credential not safely rejected: %v", err)
	}
	kubeconfig := clusterKubeconfig(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/apps-0/pods" || r.Header.Get("User-Agent") != "hcp-perf/v1" {
			t.Errorf("unexpected namespace request: %s, %s", r.URL, r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"PodList","items":[]}`)
	})
	data, err := os.ReadFile(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	requestLog := filepath.Join(dir, "requests.ndjson")
	trace, err := requesttrace.Open(requestLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trace.Close() })
	client, err := turnoverNamespaceClient(dir, "apps-0", trace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods("apps-0").List(t.Context(), metav1.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := trace.Close(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(requestLog)
	var observation requesttrace.Observation
	if err != nil || json.Unmarshal(data, &observation) != nil || observation.Namespace != "apps-0" || observation.Path != "/api/v1/namespaces/apps-0/pods" || observation.StatusCode != 200 {
		t.Fatalf("namespace factory lost HTTP evidence: %s, %v", data, err)
	}
	if _, err := turnoverNamespaceClient(dir, "apps-1", nil); err == nil {
		t.Fatal("missing namespace credentials fell back to ambient credentials")
	}
	config, err := recordRESTConfig(kubeconfig)
	if err != nil || config.Timeout != 30*time.Second || config.UserAgent != "hcp-perf/v1" {
		t.Fatalf("export REST settings: %v, %v", config, err)
	}
	if _, err := recordRESTConfig(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("explicit missing export kubeconfig fell back to ambient credentials")
	}
}

func TestTurnoverCLIRequiresApplications(t *testing.T) {
	for _, field := range []string{"", `"applications":null,`, `"applications":-1,`, `"applications":40001,`} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			input, output := filepath.Join(dir, "input.json"), filepath.Join(dir, "evidence")
			data := `{"version":"v1","guestNodes":3,` + field + `"seed":42,"namespacePrefix":"apps","durationSeconds":1,"recoverySeconds":0,"maxOutstanding":1}`
			if err := os.WriteFile(input, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := runTurnover(t.Context(), []string{"turnover", "run", "--input", input, "--credentials-dir", dir, "--output", output}, &out)
			if err == nil || !strings.Contains(err.Error(), "applications") {
				t.Fatalf("invalid applications accepted: %v", err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("invalid applications created evidence: %v", err)
			}
		})
	}
}

func TestRequestLogCLIFileLifecycle(t *testing.T) {
	for _, command := range []string{"canary", "turnover"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "requests.ndjson")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			args := []string{command, "run", "--request-log", path}
			if command == "canary" {
				kubeconfig := clusterKubeconfig(t, func(http.ResponseWriter, *http.Request) {
					t.Error("canceled command made an HTTP request")
				})
				args = append(args, "--kubeconfig", kubeconfig, "--namespace=bench", "--deployment=bench")
			} else {
				input := filepath.Join(dir, "input.json")
				if err := os.WriteFile(input, []byte(`{"version":"v1","guestNodes":3,"applications":0,"seed":42,"namespacePrefix":"apps","durationSeconds":1,"recoverySeconds":0,"maxOutstanding":1}`), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--input", input, "--credentials-dir", dir, "--output", filepath.Join(dir, "evidence"))
			}
			var out bytes.Buffer
			err := run(ctx, args, &out)
			if !errors.Is(err, context.Canceled) || errors.Is(err, requesttrace.ErrIncomplete) || !json.Valid(out.Bytes()) {
				t.Fatalf("cancellation lost workload result or invalidated trace: %v, %s", err, &out)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("missing private request file", info, err)
			}
			out.Reset()
			if err := run(t.Context(), args, &out); !errors.Is(err, requesttrace.ErrIncomplete) || out.Len() != 0 {
				t.Fatalf("existing trace not rejected before workload: %v, %s", err, &out)
			}
		})
	}
	var out bytes.Buffer
	if err := run(t.Context(), []string{"capture", "status", "--request-log=unused"}, &out); err == nil || !strings.Contains(err.Error(), "does not apply") {
		t.Fatal("request-log accepted outside workload commands", err)
	}
}

func TestRecordExportCLIArguments(t *testing.T) {
	for _, args := range [][]string{
		nil, {"record"}, {"record", "unknown"}, {"other", "archive"},
		{"record", "archive"}, {"record", "archive", "--input"},
		{"record", "archive", "--kubeconfig=admin"}, {"record", "archive", "extra"},
		{"record", "hold"}, {"record", "hold", "--duration=0s"},
		{"record", "hold", "--duration=-1s"}, {"record", "hold", "--duration=11m"},
		{"record", "hold", "--duration=bad"}, {"record", "hold", "--duration=1s", "extra"},
		{"record", "hold", "--input=unused"},
		{"record", "export"}, {"record", "export", "--namespace=ns", "--id=test"},
		{"record", "export", "--namespace=../ns", "--id=test", "--output=new"},
		{"record", "export", "--namespace=ns", "--id=INVALID", "--output=new"},
		{"record", "export", "--timeout=0s"}, {"record", "export", "--timeout=-1s"},
		{"record", "export", "--timeout=25h"}, {"record", "export", "--duration=1s"},
		{"record", "export", "extra"},
	} {
		var out bytes.Buffer
		if err := runRecordExport(t.Context(), args, &out); err == nil {
			t.Errorf("accepted invalid arguments %v", args)
		}
		if out.Len() != 0 {
			t.Errorf("argument failure wrote stdout: %s", &out)
		}
	}
	var out bytes.Buffer
	err := runRecordExport(t.Context(), []string{"record", "export", "--namespace=ns", "--id=test", "--output=" + filepath.Join(t.TempDir(), "new"), "--kubeconfig=" + filepath.Join(t.TempDir(), "missing")}, &out)
	if err == nil || !strings.Contains(err.Error(), "credentials") || out.Len() != 0 {
		t.Fatalf("missing credentials: %v, %s", err, &out)
	}
}

func TestRecordArchiveCLIOnlyTar(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`{"version":"v1"}`)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), content, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"record", "archive", "--input", dir}
	if err := runRecordExport(t.Context(), args, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 2048 {
		t.Fatalf("unexpected tar size (possible stdout contamination): %d", out.Len())
	}
	r := tar.NewReader(&out)
	header, err := r.Next()
	if err != nil || header.Name != "manifest.json" || header.Mode != 0600 {
		t.Fatalf("archive header: %v, %v", header, err)
	}
	data, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(data, content) {
		t.Fatalf("archive payload: %q, %v", data, err)
	}
	if _, err := r.Next(); err != io.EOF || out.Len() != 0 {
		t.Fatalf("unexpected trailing output: %v, %q", err, out.Bytes())
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.kubeconfig"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runRecordExport(t.Context(), args, &out); err == nil || out.Len() != 0 {
		t.Fatalf("unknown archive file not rejected before streaming: %v", err)
	}
}

func TestRecordHoldCLICancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	if err := runRecordExport(ctx, []string{"record", "hold", "--duration=5m"}, &out); !errors.Is(err, context.Canceled) || !json.Valid(out.Bytes()) || !strings.Contains(out.String(), "failed") {
		t.Fatalf("cancelled hold: %v, %s", err, &out)
	}
}

func TestRecordExportCLIPartialResult(t *testing.T) {
	kubeconfig := clusterKubeconfig(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
	})
	var out bytes.Buffer
	err := runRecordExport(t.Context(), []string{"record", "export", "--namespace=ns", "--id=test", "--output=" + filepath.Join(t.TempDir(), "new"), "--kubeconfig", kubeconfig}, &out)
	var result record.ExportManifest
	if err == nil || json.Unmarshal(out.Bytes(), &result) != nil || result.Version != "v1" || result.Namespace != "ns" || result.ID != "test" {
		t.Fatalf("failed export lost partial result: %v, %s", err, &out)
	}
}

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
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/scrape"
)

func TestScrapeGenerate(t *testing.T) {
	t.Setenv("HCP_PERF_BEARER_TOKEN", "")
	for _, mode := range []string{"complete", "missing exporter", "unsupported monitor", "empty monitors", "API failure"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			kubeconfig := clusterKubeconfig(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodGet || r.UserAgent() != "hcp-perf/v1" {
					t.Errorf("unexpected request: %s %s user-agent=%q", r.Method, r.URL, r.UserAgent())
				}
				var response any
				switch r.URL.Path {
				case "/api/v1/namespaces/monitoring/services/node-exporter":
					response = map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "node-exporter", "namespace": "monitoring"}, "spec": map[string]any{"ports": []any{map[string]any{"name": "metrics", "port": 9100}}}}
				case "/apis/monitoring.coreos.com/v1/namespaces/hcp/servicemonitors":
					if mode == "API failure" {
						http.Error(w, "forbidden", http.StatusForbidden)
						return
					}
					items := []any{}
					if mode != "empty monitors" {
						endpoint := map[string]any{"port": "metrics", "scheme": "https", "bearerTokenFile": "/mounted/token"}
						if mode == "unsupported monitor" {
							endpoint["futureUnknownField"] = true
						}
						items = append(items, map[string]any{"apiVersion": "monitoring.coreos.com/v1", "kind": "ServiceMonitor", "metadata": map[string]any{"name": "api", "namespace": "hcp"}, "spec": map[string]any{"selector": map[string]any{}, "endpoints": []any{endpoint}}})
					}
					response = map[string]any{"apiVersion": "monitoring.coreos.com/v1", "kind": "ServiceMonitorList", "items": items}
				case "/apis/monitoring.coreos.com/v1/namespaces/hcp/podmonitors":
					response = map[string]any{"apiVersion": "monitoring.coreos.com/v1", "kind": "PodMonitorList", "items": []any{}}
				default:
					t.Errorf("unexpected API request (no secret reads allowed): %s", r.URL)
					http.NotFound(w, r)
					return
				}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			})
			spec := scrape.Spec{TargetNamespaces: []string{"hcp"}, ClusterID: "test", PrometheusNamespace: "monitoring", NodeExporterServiceNamespace: "monitoring", NodeExporterServiceName: "node-exporter", NodeExporterPort: "metrics"}
			if mode == "missing exporter" {
				spec.NodeExporterServiceName = ""
			}
			dir := filepath.Join(t.TempDir(), "bundle")
			args := []string{"scrape", "generate", "--input", clusterInput(t, spec), "--kubeconfig", kubeconfig, "--output", dir}
			var out bytes.Buffer
			err := runScrape(t.Context(), args, &out)
			if (err != nil) != (mode != "complete") {
				t.Fatalf("generate: %v; output: %s", err, &out)
			}
			var bundle scrape.Bundle
			if err := json.Unmarshal(out.Bytes(), &bundle); err != nil {
				t.Fatal(err)
			}
			if (len(bundle.Gaps) > 0) != (mode != "complete") {
				t.Fatalf("wrong generation gaps: %+v", bundle.Gaps)
			}
			for path, want := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "prometheus.yaml"): 0600, filepath.Join(dir, "bundle.json"): 0600} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != want {
					t.Errorf("%s permissions %o, want %o", path, info.Mode().Perm(), want)
				}
			}
			data, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, out.Bytes()) {
				t.Error("persisted bundle differs from JSON output")
			}
			config, err := os.ReadFile(filepath.Join(dir, "prometheus.yaml"))
			if err != nil || string(config) != bundle.Config {
				t.Fatalf("configuration file differs: %v", err)
			}
			if mode != "API failure" {
				var configData map[string]any
				if err := yaml.UnmarshalStrict(config, &configData); err != nil || configData["scrape_configs"] == nil {
					t.Fatalf("invalid Prometheus YAML: %v", err)
				}
			}
			if mode == "complete" && !strings.Contains(string(config), "/mounted/token") {
				t.Error("token file reference not preserved")
			}
			before := calls.Load()
			if err := runScrape(t.Context(), args, io.Discard); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing bundle must be rejected: %v", err)
			}
			if calls.Load() != before {
				t.Error("existing output triggered API requests")
			}
			persisted, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
			if err != nil || !bytes.Equal(data, persisted) {
				t.Fatalf("existing bundle was modified: %v", err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targets := []scrape.Target{}
				for _, job := range bundle.ExpectedJobs {
					targets = append(targets, scrape.Target{ScrapePool: job, Health: "up"})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"activeTargets": targets}})
			}))
			defer server.Close()
			out.Reset()
			err = runScrape(t.Context(), []string{"scrape", "check", "--input", filepath.Join(dir, "bundle.json"), "--endpoint", server.URL}, &out)
			if (err != nil) != (mode != "complete") || !json.Valid(out.Bytes()) {
				t.Fatalf("generated bundle round trip: %v; output: %s", err, &out)
			}
		})
	}
}

func TestScrapeCheck(t *testing.T) {
	t.Setenv("HCP_PERF_BEARER_TOKEN", "")
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  string
	}{
		{"healthy", `{"status":"success","data":{"activeTargets":[{"scrapePool":"api","health":"up","labels":{"job":"relabeled"}}]}}`, 200, ""},
		{"unhealthy", `{"status":"success","data":{"activeTargets":[{"scrapePool":"api","health":"down","lastError":"connection refused"}]}}`, 200, "blocker"},
		{"empty", `{"status":"success","data":{"activeTargets":[]}}`, 200, "blocker"},
		{"unrelated", `{"status":"success","data":{"activeTargets":[{"scrapePool":"other","health":"up"}]}}`, 200, "blocker"},
		{"API error", `{"status":"error","error":"unavailable"}`, 200, "unavailable"},
		{"HTTP error", `unavailable`, 503, "503"},
		{"invalid JSON", `{`, 200, "decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/prometheus/api/v1/targets" || r.URL.Query().Get("state") != "active" {
					t.Errorf("unexpected targets request: %s %s", r.Method, r.URL)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			var out bytes.Buffer
			err := runScrape(t.Context(), []string{"scrape", "check", "--input", clusterInput(t, scrape.Bundle{ExpectedJobs: []string{"api"}}), "--endpoint", server.URL + "/prometheus", "--timeout=1m"}, &out)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("check: %v; want %q; output: %s", err, tc.wantError, &out)
			}
			var report scrape.TargetReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if tc.name == "healthy" && (len(report.Targets) != 1 || len(report.Gaps) != 0) {
				t.Fatalf("wrong healthy report: %+v", report)
			}
			if tc.name == "unhealthy" && (len(report.Targets) != 1 || len(report.Gaps) == 0) {
				t.Fatalf("partial unhealthy report missing: %+v", report)
			}
		})
	}
}

func TestScrapeCheckSecurityAndCancellation(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	defer server.Close()
	input := clusterInput(t, scrape.Bundle{ExpectedJobs: []string{"api"}})
	args := []string{"scrape", "check", "--input", input, "--endpoint", server.URL}
	t.Setenv("HCP_PERF_BEARER_TOKEN", "sensitive-token")
	var out bytes.Buffer
	err := runScrape(t.Context(), args, &out)
	if err == nil || !strings.Contains(err.Error(), "HTTPS") || calls.Load() != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("HTTP bearer protection failed: %v; calls=%d; output=%s", err, calls.Load(), &out)
	}
	if strings.Contains(err.Error()+out.String(), "sensitive-token") {
		t.Fatal("bearer token leaked")
	}
	t.Setenv("HCP_PERF_BEARER_TOKEN", "")
	out.Reset()
	err = runScrape(t.Context(), args, &out)
	if err == nil || !strings.Contains(err.Error(), "302") || calls.Load() != 1 || !json.Valid(out.Bytes()) {
		t.Fatalf("redirect protection failed: %v; calls=%d; output=%s", err, calls.Load(), &out)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out.Reset()
	err = runScrape(ctx, args, &out)
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 || !json.Valid(out.Bytes()) {
		t.Fatalf("cancellation failed: %v; calls=%d; output=%s", err, calls.Load(), &out)
	}
}

func TestScrapeCheckInvalidBundleAndEndpoint(t *testing.T) {
	t.Setenv("HCP_PERF_BEARER_TOKEN", "")
	for _, tc := range []struct {
		name     string
		bundle   scrape.Bundle
		endpoint string
		want     string
	}{
		{"empty expected jobs", scrape.Bundle{}, "http://127.0.0.1:1", "no expected jobs"},
		{"invalid endpoint", scrape.Bundle{ExpectedJobs: []string{"api"}}, "file:///tmp/targets", "endpoint"},
		{"embedded credentials", scrape.Bundle{ExpectedJobs: []string{"api"}}, "https://user:password@example.test", "endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runScrape(t.Context(), []string{"scrape", "check", "--input", clusterInput(t, tc.bundle), "--endpoint", tc.endpoint}, &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !json.Valid(out.Bytes()) {
				t.Fatalf("got %v, want %q with JSON report; output: %s", err, tc.want, &out)
			}
		})
	}
}

func TestScrapeValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "requires a subcommand"},
		{[]string{"scrape"}, "requires a subcommand"},
		{[]string{"scrape", "unknown"}, "unknown scrape command"},
		{[]string{"other", "check"}, "unknown scrape command"},
		{[]string{"scrape", "generate", "extra"}, "unexpected positional"},
		{[]string{"scrape", "generate", "--endpoint=x"}, "flag provided but not defined"},
		{[]string{"scrape", "check", "--kubeconfig=x"}, "flag provided but not defined"},
		{[]string{"scrape", "check", "--input"}, "flag needs an argument"},
		{[]string{"scrape", "check", "--timeout=bad"}, "invalid value"},
		{[]string{"scrape", "check", "--timeout=0s"}, "timeout must be positive"},
		{[]string{"scrape", "generate", "--timeout=-1s"}, "timeout must be positive"},
		{[]string{"scrape", "generate", "--timeout=25h"}, "at most 24h"},
		{[]string{"scrape", "generate"}, "--input is required"},
		{[]string{"scrape", "generate", "--input=x"}, "--output is required"},
		{[]string{"scrape", "check", "--input=x"}, "--endpoint is required"},
	} {
		err := runScrape(t.Context(), tc.args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want %q", tc.args, err, tc.want)
		}
	}
	for _, content := range []string{
		`{"clusterID":"test","prometheusNamespace":"monitoring","unknown":true}`,
		`{"prometheusNamespace":"monitoring"}`,
		`{"clusterID":"test","clusterID":"other","prometheusNamespace":"monitoring"}`,
		`{"clusterID":"test","prometheusNamespace":"monitoring","targetNamespaces":null}`,
		`{"clusterID":"test","prometheusNamespace":"monitoring"} {}`,
	} {
		dir := t.TempDir()
		input := filepath.Join(dir, "spec.json")
		if err := os.WriteFile(input, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		err := runScrape(t.Context(), []string{"scrape", "generate", "--input", input, "--output", filepath.Join(dir, "bundle")}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "scrape spec:") {
			t.Fatalf("strict input validation: %v for %s", err, content)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("invalid spec created output: %v, %v", entries, err)
		}
	}
	// Optional spec fields must remain optional with the shared strict decoder.
	spec := scrape.Spec{ClusterID: "test", PrometheusNamespace: "monitoring", TargetNamespaces: []string{"hcp"}}
	var decoded scrape.Spec
	if err := decodeFile(clusterInput(t, spec), &decoded); err != nil || !reflect.DeepEqual(decoded, spec) {
		t.Fatalf("optional spec fields: %+v, %v", decoded, err)
	}
}

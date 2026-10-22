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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
)

func TestCommands(t *testing.T) {
	for _, args := range [][]string{
		{"sizing", "resolve", "--profile", "examples/small.json"},
		{"sizing", "hypershift", "--profile", "examples/small-complete.json"},
		{"sizing", "hypershift", "--profile", "examples/small-catalog-v2.json"},
		{"case", "plan", "--case", "examples/case.json", "--profile", "examples/small.json"},
		{"case", "schedule", "--case", "examples/case.json", "--profile", "examples/small.json"},
		{"adversaries", "plan", "--input", "examples/nodes.json"},
		{"cost", "estimate", "--input", "examples/cost.json"},
		{"scan", "plan", "--input", "examples/scan.json", "--profile", "examples/small-catalog-v2.json"},
	} {
		t.Run(strings.Join(args[:2], "-"), func(t *testing.T) {
			var first, second bytes.Buffer
			if err := run(context.Background(), args, &first); err != nil {
				t.Fatal(err)
			}
			if err := run(context.Background(), args, &second); err != nil {
				t.Fatal(err)
			}
			if !json.Valid(first.Bytes()) || first.String() != second.String() {
				t.Fatalf("expected deterministic JSON, got %s and %s", &first, &second)
			}
		})
	}
}

func TestBadArguments(t *testing.T) {
	for _, args := range [][]string{
		{"case"}, {"unknown", "command"}, {"case", "run"},
		{"sizing", "resolve"}, {"sizing", "resolve", "--profile", "missing"},
		{"sizing", "resolve", "--wat"}, {"sizing", "resolve", "extra"},
		{"sizing", "resolve", "--profile", "examples/small.json", "--step", "10s"},
		{"sizing", "resolve", "--profile", "examples/small.json", "--default-cpu-milli", "10"},
		{"sizing", "hypershift"},
		{"sizing", "hypershift", "--profile", "examples/small.json", "--default-memory-mib", "0"},
		{"sizing", "hypershift", "--profile", "examples/small.json", "--kubeconfig", "unused"},
		{"metrics", "export", "--start", "bad"}, {"analyze", "--input", "missing"},
	} {
		if err := run(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Errorf("expected error for %v", args)
		}
	}
}

func TestCaseSchedulePeriodJSON(t *testing.T) {
	base, err := os.ReadFile("examples/validation-case.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		period                string
		apps, duration, count int64
	}{
		{"", 120, 300, 10}, {"0", 120, 300, 10}, {"3600", 120, 300, 10},
		{"1800", 120, 300, 20}, {"900", 120, 300, 40}, {"600", 120, 300, 60}, {"300", 120, 300, 120},
		{"60", 120, 300, 600}, {"1", 120, 300, 36000},
		{"86400", 120, 300, 1}, {"1", 0, 300, 0}, {"150", 40000, 300, 80000},
		{"149", 40000, 300, -1}, {"1", 40000, 7200, -1}, {"-1", 120, 300, -1},
		{"86401", 120, 300, -1}, {"9223372036854775807", 120, 300, -1},
		{"9223372036854775808", 120, 300, -1}, {"null", 120, 300, -1},
		{`"300"`, 120, 300, -1}, {"300.5", 120, 300, -1},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.period, tc.apps), func(t *testing.T) {
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(base, &doc); err != nil {
				t.Fatal(err)
			}
			doc["applications"] = json.RawMessage(fmt.Sprint(tc.apps))
			doc["guestNodes"] = json.RawMessage("1000")
			doc["steadySeconds"] = json.RawMessage(fmt.Sprint(tc.duration))
			if tc.duration == 7200 {
				doc["mode"] = json.RawMessage(`"soak"`)
			}
			if tc.period != "" {
				doc["turnoverPeriodSeconds"] = json.RawMessage(tc.period)
			}
			data, _ := json.Marshal(doc)
			path := filepath.Join(t.TempDir(), "case.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := run(t.Context(), []string{"case", "schedule", "--case", path, "--profile", "examples/small.json"}, &out)
			if tc.count < 0 {
				if err == nil || out.Len() != 0 {
					t.Fatalf("invalid input emitted schedule: %v", err)
				}
				return
			}
			var got []experiment.Rollout
			if err != nil || json.Unmarshal(out.Bytes(), &got) != nil || int64(len(got)) != tc.count {
				t.Fatalf("schedule count: %d, want %d: %v", len(got), tc.count, err)
			}
			var spec experiment.Spec
			if err := json.Unmarshal(data, &spec); err != nil {
				t.Fatal(err)
			}
			if spec.TurnoverPeriodSeconds == 0 {
				spec.TurnoverPeriodSeconds = 3600
			}
			want, err := experiment.RolloutsForPeriod(tc.apps, spec.Seed, time.Duration(tc.duration)*time.Second, time.Duration(spec.TurnoverPeriodSeconds)*time.Second)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatal("CLI diverged from period schedule", err)
			}
		})
	}
}

func TestHyperShiftDefaultsAndNoPartialArtifact(t *testing.T) {
	var out bytes.Buffer
	args := []string{"sizing", "hypershift", "--profile", "examples/small-complete.json", "--default-cpu-milli", "20", "--default-memory-mib", "128"}
	if err := run(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	var object struct {
		Spec struct {
			Sizes []struct {
				Effects struct {
					Policy struct {
						Defaults map[string]string `json:"defaultRequests"`
					} `json:"containerResourcePolicy"`
				} `json:"effects"`
			} `json:"sizes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(out.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	if len(object.Spec.Sizes) != 1 || object.Spec.Sizes[0].Effects.Policy.Defaults["cpu"] != "20m" || object.Spec.Sizes[0].Effects.Policy.Defaults["memory"] != "128Mi" {
		t.Fatalf("fallback flags were not rendered: %s", &out)
	}
	out.Reset()
	args[len(args)-1] = "0"
	if err := run(context.Background(), args, &out); err == nil || out.Len() != 0 {
		t.Fatalf("invalid render returned error %v and artifact %s", err, &out)
	}
}

func TestStrictJSON(t *testing.T) {
	for _, content := range []string{`{"unknown":1}`, `{} {}`, `{} garbage`, `[]`} {
		path := filepath.Join(t.TempDir(), "input.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		var target struct{}
		if err := decodeFile(path, &target); err == nil {
			t.Errorf("accepted %s", content)
		}
	}
}

func TestExportAndAnalyze(t *testing.T) {
	t.Setenv("HCP_PERF_BEARER_TOKEN", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" || r.URL.Query().Get("query") != "up" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"fixture"},"values":[[1788883200,"1"],[1788883230,"2"]]}]}}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	queries := filepath.Join(dir, "queries.json")
	if err := os.WriteFile(queries, []byte(`[{"name":"up","expr":"up"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "capture")
	args := []string{"metrics", "export", "--endpoint", server.URL, "--queries", queries, "--start", "2026-09-08T16:00:00Z", "--end", "2026-09-08T16:00:30Z", "--output", output}
	var out bytes.Buffer
	if err := run(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"analyze", "--input", output}, &out); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), "fixture") {
		t.Fatalf("missing series evidence: %s", &out)
	}
	if err := run(context.Background(), args, &bytes.Buffer{}); err == nil {
		t.Fatal("export overwrote existing capture")
	}
}

func TestBearerRequiresTLS(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (bearerTransport{token: "secret"}).RoundTrip(req); err == nil {
		t.Fatal("sent credential over HTTP")
	}
}

func TestCaseAliasCannotOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, []byte(`{"count":5,"Count":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	var target struct {
		Count int `json:"count"`
	}
	if err := decodeFile(path, &target); err == nil {
		t.Fatal("accepted case-insensitive override")
	}
}

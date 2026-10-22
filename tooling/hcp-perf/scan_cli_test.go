// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/scan"
)

func TestScanCLI(t *testing.T) {
	t.Setenv("KUBECONFIG", "/does-not-exist")
	t.Setenv("HCP_PERF_BEARER_TOKEN", "must-not-be-used")
	args := []string{"plan", "--profile", "examples/small.json", "--input", "examples/scan.json"}
	var out, again bytes.Buffer
	if err := runScan(args, &out); err != nil {
		t.Fatal(err)
	}
	if err := runScan(args, &again); err != nil || !bytes.Equal(out.Bytes(), again.Bytes()) {
		t.Fatal("non-deterministic CLI output", err)
	}
	var result scan.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "planned" || len(result.Cases) != 7 || result.Cases[6].Workload.Pods != 600 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if strings.Contains(out.String(), "executionReady") || strings.Contains(out.String(), "must-not-be-used") {
		t.Fatal("unexpected readiness claim or credentials")
	}
	for _, args := range [][]string{
		nil, {"execute"}, {"plan"}, {"plan", "--input", "examples/scan.json"},
		{"plan", "--profile", "examples/small.json"},
		{"plan", "--profile", "missing.json", "--input", "examples/scan.json"},
		{"plan", "--profile", "examples/small.json", "--input", "missing.json"},
		append(append([]string{}, args...), "extra"),
		append(append([]string{}, args...), "--output", "must-not-create"),
		append(append([]string{}, args...), "--execute"),
	} {
		out.Reset()
		if err := runScan(args, &out); err == nil || out.Len() != 0 {
			t.Fatalf("expected error and no output: %v, %v", args, err)
		}
	}
	if err := runScan(args, scanErrorWriter{}); !errors.Is(err, errScanWrite) {
		t.Fatalf("writer error not propagated: %v", err)
	}
}

func TestScanCLIStrictInput(t *testing.T) {
	data, err := os.ReadFile("examples/scan.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]string{
		"unknown":            strings.Replace(string(data), `"seed": 42`, `"seed": 42, "optimizer": true`, 1),
		"duplicate":          strings.Replace(string(data), `"seed": 42`, `"seed": 42, "seed": 43`, 1),
		"missing seed":       strings.Replace(string(data), `"seed": 42,`, "", 1),
		"missing cpu":        strings.Replace(string(data), ",\n  \"cpuTreatment\": \"quiet\"", "", 1),
		"fractional apps":    strings.Replace(string(data), "[0, 3,", "[0, 3.5,", 1),
		"invalid population": strings.Replace(string(data), "[0, 3,", "[3, 3,", 1),
		"null":               "null", "trailing": string(data) + "{}",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "scan.json")
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := runScan([]string{"plan", "--profile", "examples/small.json", "--input", path}, &out); err == nil || out.Len() != 0 {
				t.Fatalf("expected error and no output: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != input {
				t.Fatal("input file modified", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatal("unexpected output files", err)
			}
		})
	}
}

func TestScanCLIPeriod(t *testing.T) {
	data, err := os.ReadFile("examples/scan.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, period := range []string{"0", "3600", "1800", "900", "600", "300", "86400", "-1", "86401", "9223372036854775808", "null", `"300"`, "300.5"} {
		t.Run(period, func(t *testing.T) {
			input := strings.Replace(string(data), `"seed": 42`, `"seed": 42, "turnoverPeriodSeconds":`+period, 1)
			path := filepath.Join(t.TempDir(), "scan.json")
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := run(t.Context(), []string{"scan", "plan", "--profile", "examples/small.json", "--input", path}, &out)
			var seconds int64
			valid := json.Unmarshal([]byte(period), &seconds) == nil && period != "null" && seconds >= 0 && seconds <= 86400
			if !valid {
				if err == nil || out.Len() != 0 {
					t.Fatalf("invalid period produced plan: %v", err)
				}
				return
			}
			if seconds == 0 {
				seconds = 3600
			}
			var got scan.Result
			if err != nil || json.Unmarshal(out.Bytes(), &got) != nil || got.Spec.TurnoverPeriodSeconds != seconds {
				t.Fatalf("resolved scan period: %v", err)
			}
			for _, c := range got.Cases {
				if c.Spec.TurnoverPeriodSeconds != seconds || c.Workload.Rollouts != (c.Spec.Applications*300+seconds-1)/seconds {
					t.Fatalf("case lost fixed period: %+v", c)
				}
			}
		})
	}
}

var errScanWrite = errors.New("write failed")

type scanErrorWriter struct{}

func (scanErrorWriter) Write([]byte) (int, error) { return 0, errScanWrite }

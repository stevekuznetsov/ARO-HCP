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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/adversary"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

func TestInlineVolumeSourceJSON(t *testing.T) {
	for _, tc := range []struct {
		input string
		valid bool
	}{
		{`{"name":"token","secret":{"secretName":"token","items":[{"key":"token","path":"value"}],"defaultMode":288,"optional":false}}`, true},
		{`{"name":"ca","configMap":{"name":"ca","items":[{"key":"ca.crt","path":"value"}]}}`, true},
		{`{"name":"token","secret":{"SecretName":"token"}}`, false},
		{`{"name":"token","Secret":{"secretName":"token"}}`, false},
		{`{"name":"token","secret":{"secretName":"token","unknown":true}}`, false},
	} {
		var volume corev1.Volume
		path := filepath.Join(t.TempDir(), "volume.json")
		if err := os.WriteFile(path, []byte(tc.input), 0600); err != nil {
			t.Fatal(err)
		}
		if err := decodeFile(path, &volume); (err == nil) != tc.valid {
			t.Fatalf("valid=%v err=%v", tc.valid, err)
		}
	}
}

func TestDecodeFileStrictExamples(t *testing.T) {
	for _, tc := range []struct {
		name, file, from, to, wantError string
	}{
		{"profile", "small", "", "", ""},
		{"case", "case", "", "", ""},
		{"nodes with timestamp", "nodes", "", "", ""},
		{"top-level duplicate", "case", `"guestNodes": 3`, `"guestNodes": 3, "guestNodes": 0`, `duplicate JSON key "guestNodes"`},
		{"nested duplicate", "small", `"numerator": 1`, `"numerator": 1, "numerator": 0`, `$.containers[0].cpuRatio: duplicate JSON key "numerator"`},
		{"escaped duplicate", "nodes", `"reserveCPUMilli": 500`, `"reserveCPUMilli": 500, "reserveCPU\u004dilli": 0`, `$.nodes[0]: duplicate JSON key "reserveCPUMilli"`},
		{"missing guest count", "case", `"guestNodes": 3,`, "", "$.guestNodes: required field is missing"},
		{"missing application count", "case", `"applications": 0,`, "", "$.applications: required field is missing"},
		{"missing seed", "case", `"seed": 42,`, "", "$.seed: required field is missing"},
		{"missing nested deadline", "case", `"startup": 3600,`, "", "$.deadlinesSeconds.startup: required field is missing"},
		{"missing committed CPU", "nodes", `"committedCPUMilli": 10000,`, "", "$.nodes[0].committedCPUMilli: required field is missing"},
		{"missing reserve CPU", "nodes", `,` + "\n" + `      "reserveCPUMilli": 500`, "", "$.nodes[0].reserveCPUMilli: required field is missing"},
		{"missing bool", "nodes", `"hostsTarget": true,`, "", "$.nodes[0].hostsTarget: required field is missing"},
		{"missing ratio numerator", "small", `"numerator": 1,`, "", "$.containers[0].cpuRatio.numerator: required field is missing"},
		{"missing floor field", "small", `"floor": {"cpuMilli": 250,`, `"floor": {`, "$.containers[0].floor.cpuMilli: required field is missing"},
		{"null guest count", "case", `"guestNodes": 3`, `"guestNodes": null`, "$.guestNodes: null is not allowed"},
		{"null struct", "small", `"anchor": {"cpuMilli": 250, "memoryMiB": 1024}`, `"anchor": null`, "$.anchor: null is not allowed"},
		{"null timestamp", "nodes", `"collectedAt": "2026-09-08T12:00:00Z"`, `"collectedAt": null`, "$.collectedAt: null is not allowed"},
		{"null optional bool", "small", `"kind": "Deployment"`, `"initContainer": null, "kind": "Deployment"`, "$.containers[0].identity.initContainer: null is not allowed"},
		{"null optional pointer", "small", `"cpuRatio":`, `"goMaxProcs": null, "cpuRatio":`, "$.containers[0].goMaxProcs: null is not allowed"},
		{"unknown nested field", "small", `"numerator": 1`, `"numerator": 1, "typo": 0`, `unknown field "typo"`},
		{"wrong scalar type", "case", `"guestNodes": 3`, `"guestNodes": "3"`, "cannot unmarshal string"},
		{"wrong struct type", "small", `"anchor": {"cpuMilli": 250, "memoryMiB": 1024}`, `"anchor": []`, "cannot unmarshal array"},
		{"zero guest count", "case", `"guestNodes": 3`, `"guestNodes": 0`, ""},
		{"zero committed CPU", "nodes", `"committedCPUMilli": 10000`, `"committedCPUMilli": 0`, ""},
		{"zero reserve CPU", "nodes", `"reserveCPUMilli": 500`, `"reserveCPUMilli": 0`, ""},
		{"zero ratio numerator", "small", `"numerator": 1`, `"numerator": 0`, ""},
		{"zero floor", "small", `"floor": {"cpuMilli": 250, "memoryMiB": 1024}`, `"floor": {"cpuMilli": 0, "memoryMiB": 0}`, ""},
		{"explicit optional bool", "small", `"kind": "Deployment"`, `"initContainer": false, "kind": "Deployment"`, ""},
		{"explicit optional pointer", "small", `"cpuRatio":`, `"goMaxProcs": 1, "cpuRatio":`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile("examples/" + tc.file + ".json")
			if err != nil {
				t.Fatal(err)
			}
			content := string(data)
			if tc.from != "" {
				if !strings.Contains(content, tc.from) {
					t.Fatalf("fixture lacks %q", tc.from)
				}
				content = strings.Replace(content, tc.from, tc.to, 1)
			}
			var target any
			switch tc.file {
			case "small":
				target = &sizing.Profile{}
			case "case":
				target = &experiment.Spec{}
			case "nodes":
				target = &adversary.Spec{}
			}
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			err = decodeFile(path, target)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected error containing %q, got %v", tc.wantError, err)
			}
		})
	}
}

func TestDecodeFileJSONStructure(t *testing.T) {
	for _, tc := range []struct {
		input, wantError string
	}{
		{`null`, "null is not allowed"},
		{`[null]`, "$[0]: null is not allowed"},
		{`[{}]`, "$[0].numerator: required field is missing"},
		{`[{"numerator":0,"denominator":1},{"denominator":1}]`, "$[1].numerator: required field is missing"},
		{`[{"numerator":0,"numerator":1,"denominator":1}]`, `duplicate JSON key "numerator"`},
		{`[{"numerator":0,"denominator":1,"unknown":0}]`, `unknown field "unknown"`},
		{`[{"numerator":0,"denominator":1}] {}`, "expected exactly one JSON value"},
		{`[] []`, "expected exactly one JSON value"},
		{`[] garbage`, "expected exactly one JSON value"},
		{`[{`, "EOF"},
		{``, "EOF"},
		{`[{"numerator":0,"denominator":1}]`, ""},
		{"[] \n\t", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, []byte(tc.input), 0600); err != nil {
				t.Fatal(err)
			}
			var target []sizing.Ratio
			err := decodeFile(path, &target)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected error containing %q, got %v", tc.wantError, err)
			}
		})
	}
}

func TestDecodeFilePreservesScalars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, []byte(`{"integer":9223372036854775807,"number":9007199254740993,"time":"2026-09-08T12:00:00Z","raw":{"value":9007199254740993}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var target struct {
		Integer int64           `json:"integer"`
		Number  json.Number     `json:"number"`
		Time    time.Time       `json:"time"`
		Raw     json.RawMessage `json:"raw"`
	}
	if err := decodeFile(path, &target); err != nil {
		t.Fatal(err)
	}
	if target.Integer != 9223372036854775807 || target.Number.String() != "9007199254740993" ||
		!target.Time.Equal(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)) || string(target.Raw) != `{"value":9007199254740993}` {
		t.Fatalf("scalar values changed: %+v", target)
	}
}

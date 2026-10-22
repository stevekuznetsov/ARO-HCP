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
	"testing"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/cost"
)

func TestCostCLI(t *testing.T) {
	var out bytes.Buffer
	if err := runCost([]string{"estimate", "--input", "examples/cost.json"}, &out); err != nil {
		t.Fatal(err)
	}
	var result cost.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.TargetBudgetUSDhr != "0.125000000" || len(result.Estimates) != 4 {
		t.Fatalf("unexpected result: %+v", result)
	}
	for _, args := range [][]string{nil, {"unknown"}, {"estimate"}, {"estimate", "--bad"}, {"estimate", "--input", "missing.json"}, {"estimate", "--input", "examples/cost.json", "extra"}} {
		if err := runCost(args, &out); err == nil {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
}

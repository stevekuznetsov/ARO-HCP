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

package sizing

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSmallConstrainedV1(t *testing.T) {
	data, err := os.ReadFile("../../examples/small-constrained-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var p Profile
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "small-constrained-v1" || !p.CPULimitEqualsRequest || p.SafetyLimit != (Ratio{11, 10}) || p.GoMemLimitPercent != 80 || p.Rounding != (Budget{1, 1}) {
		t.Fatalf("unexpected constrained policy: %+v", p)
	}
	r, err := Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Containers {
		if c.Limits.CPU != c.Requests.CPU {
			t.Fatalf("CPU limit differs from request: %+v", c)
		}
		if _, ok := c.Env["GOMAXPROCS"]; ok {
			t.Fatalf("must not invent GOMAXPROCS: %+v", c)
		}
	}
	if _, err := ToHyperShift(r, Budget{10, 64}); err != nil {
		t.Fatal(err)
	}
}

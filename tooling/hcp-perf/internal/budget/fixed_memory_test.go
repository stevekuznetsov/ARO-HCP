// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package budget

import (
	"bytes"
	"math"
	"reflect"
	"testing"
)

func TestFixedMemoryPreservesBaselineAndCPU(t *testing.T) {
	s := example(t)
	baseline, err := Allocate(s)
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.Rows {
		s.Rows[i].OldRequests = baseline.Ledger.Rows[i].Requests
	}
	s.PreserveMemory = true
	unchanged, err := Allocate(s)
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	for s.Rows[i].Identity.Container != "kube-apiserver" {
		i++
	}
	old := s.Rows[i].OldRequests.MemoryMiB
	fixed := old - 1
	s.Rows[i].Floors.MemoryMiB = fixed
	s.Rows[i].FixedMemoryMiB = &fixed
	raw, err := JSON(s)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Allocate(decoded)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := JSON(s)
	if !bytes.Equal(raw, after) {
		t.Fatal("input mutated")
	}
	if r.Ledger.Rows[i].OldRequests.MemoryMiB != old || r.Ledger.Rows[i].Requests.MemoryMiB != fixed || r.Ledger.Rows[i].Delta.MemoryMiB != -1 {
		t.Fatal("baseline/override/delta mismatch")
	}
	if r.Ledger.Resources["memoryMiB"].OldOfferingCharge != unchanged.Ledger.Resources["memoryMiB"].OfferingCharge {
		t.Fatal("old charge was rewritten")
	}
	if !reflect.DeepEqual(r.Ledger.Resources["cpuMilli"], unchanged.Ledger.Resources["cpuMilli"]) {
		t.Fatal("CPU ledger changed")
	}
	for j, row := range r.Ledger.Rows {
		if row.Requests.CPUMilli != unchanged.Ledger.Rows[j].Requests.CPUMilli {
			t.Fatal("CPU changed")
		}
		if j != i && row.Requests.MemoryMiB != unchanged.Ledger.Rows[j].Requests.MemoryMiB {
			t.Fatal("unlisted row changed")
		}
	}
	if r.Ledger.Resources["memoryMiB"].NextGridCharge != "" || r.Ledger.Resources["memoryMiB"].Factor != "preserved-with-explicit-overrides" {
		t.Fatal("fixed allocation mislabeled weighted")
	}
}

func TestFixedMemoryValidationAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		fixed, floor int64
		preserve     bool
		wantError    bool
	}{
		{"increase above old", 64, 64, true, false},
		{"decrease", 2, 1, true, false},
		{"requires preserve", 64, 1, false, true},
		{"zero", 0, 1, true, true},
		{"negative", -1, 1, true, true},
		{"below floor", 1, 2, true, true},
		{"over budget", 100, 1, true, true},
		{"overflow", math.MaxInt64, 1, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tiny(t)
			s.PreserveMemory = tc.preserve
			s.Rows[0].OldRequests.MemoryMiB = 20
			s.Rows[0].Floors.MemoryMiB = tc.floor
			s.Rows[0].FixedMemoryMiB = &tc.fixed
			r, err := Allocate(s)
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected result: %v", err)
			}
			if err == nil && (r.Ledger.Rows[0].OldRequests.MemoryMiB != 20 || r.Ledger.Rows[0].Requests.MemoryMiB != tc.fixed) {
				t.Fatal("wrong baseline or target")
			}
		})
	}
}

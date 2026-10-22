// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dashboard

import "testing"

func TestDistribution(t *testing.T) {
	if distribution(nil, 20) != nil {
		t.Fatal("missing samples must remain null")
	}
	x := []float64{100, 1, 2, 3, 4, 5, 6, 7}
	d := distribution(x, 10)
	if d.Count != 8 || d.Missing != 2 || d.Q1 != 2 || d.Median != 4 || d.Q3 != 6 || d.P95 != 100 || d.P99 != 100 || d.Max != 100 || d.WhiskerHigh != 7 || d.OutlierCount != 1 {
		t.Fatalf("incorrect nearest-rank/Tukey summary: %+v", d)
	}
	if x[0] != 100 {
		t.Fatal("input mutated")
	}
	d = distribution([]float64{0}, 1)
	if d.Count != 1 || d.Min != 0 || d.WhiskerLow != 0 {
		t.Fatalf("measured zero lost: %+v", d)
	}
}

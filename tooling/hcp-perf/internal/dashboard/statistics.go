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

import (
	"math"
	"sort"
)

// distribution never substitutes zero for an absent observation. Quantiles are
// nearest-rank and whiskers are observed values inside Tukey's 1.5-IQR fences.
func distribution(values []float64, expected int) *Distribution {
	if len(values) == 0 {
		return nil
	}
	x := append([]float64(nil), values...)
	sort.Float64s(x)
	q := func(p float64) float64 { return x[max(0, int(math.Ceil(p*float64(len(x))))-1)] }
	d := &Distribution{Count: len(x), Expected: max(expected, len(x)), Min: x[0], Max: x[len(x)-1], Q1: q(.25), Median: q(.5), Q3: q(.75), P95: q(.95), P99: q(.99)}
	d.Missing = d.Expected - d.Count
	low, high := d.Q1-1.5*(d.Q3-d.Q1), d.Q3+1.5*(d.Q3-d.Q1)
	i, j := 0, len(x)-1
	for x[i] < low {
		i++
	}
	for x[j] > high {
		j--
	}
	d.WhiskerLow, d.WhiskerHigh, d.OutlierCount = x[i], x[j], i+len(x)-1-j
	return d
}

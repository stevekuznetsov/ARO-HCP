// Copyright 2025 Microsoft Corporation
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

// Package quantity provides Kubernetes CPU/memory quantity formatting helpers.
// It is a small copy of tooling/rightsize-requests/internal/rightsize/quantity.go.
package quantity

import "fmt"

// FormatCPU renders CPU cores as a compact human string (e.g. "1.5", "250m").
func FormatCPU(cores float64) string {
	if cores <= 0 {
		return "0"
	}
	if cores < 1 {
		return fmt.Sprintf("%dm", int64(cores*1000+0.5))
	}
	return fmt.Sprintf("%.2f", cores)
}

// FormatMemory renders bytes as a compact binary quantity (e.g. "512Mi", "3.5Gi").
func FormatMemory(bytes float64) string {
	if bytes <= 0 {
		return "0"
	}
	const (
		ki = 1 << 10
		mi = 1 << 20
		gi = 1 << 30
		ti = 1 << 40
	)
	switch {
	case bytes >= ti:
		return fmt.Sprintf("%.2fTi", bytes/ti)
	case bytes >= gi:
		return fmt.Sprintf("%.2fGi", bytes/gi)
	case bytes >= mi:
		return fmt.Sprintf("%.0fMi", bytes/mi)
	default:
		return fmt.Sprintf("%.0fKi", bytes/ki)
	}
}

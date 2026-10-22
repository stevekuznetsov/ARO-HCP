// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package budget allocates explicit floors and weights against saved Pod layouts.
// It models resource equivalents, not startup health or packing feasibility.
package budget

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

// Number accepts exact rational strings or JSON integers, never JSON floats.
type Number string

func (n *Number) UnmarshalJSON(b []byte) error {
	var s string
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	} else {
		s = string(b)
		if !regexp.MustCompile(`^[0-9]+$`).MatchString(s) {
			return fmt.Errorf("use an integer or rational string")
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 {
		return fmt.Errorf("positive rational required: %q", s)
	}
	*n = Number(s)
	return nil
}

type Weights struct {
	CPUMilli  Number `json:"cpuMilli"`
	MemoryMiB Number `json:"memoryMiB"`
}

type Provenance struct {
	Basis  string `json:"basis"`
	SHA256 string `json:"sha256"`
}

type Source struct {
	Provenance
	Path   string `json:"path"`
	Method string `json:"method"`
}

type Node struct {
	Provenance
	sizing.Budget
	SKU             string `json:"sku"`
	Region          string `json:"region"`
	Currency        string `json:"currency"`
	CapacityBasis   string `json:"capacityBasis"`
	CapacitySource  string `json:"capacitySource"`
	SourceURL       string `json:"sourceURL"`
	RetrievedAt     string `json:"retrievedAt"`
	PriceUSDPerHour Number `json:"priceUSDPerHour"`
}

type Quota struct {
	Provenance
	PeriodUS  int64 `json:"periodUS"`
	MinimumUS int64 `json:"minimumUS"`
	QuantumUS int64 `json:"quantumUS"`
}

type Row struct {
	Identity    sizing.Identity `json:"identity"`
	Replicas    int64           `json:"replicas"`
	NonGo       *bool           `json:"nonGo"`
	GoMaxProcs  *int64          `json:"goMaxProcs,omitempty"`
	Source      Source          `json:"source"`
	StartupNote string          `json:"startupNote"`
	FloorBasis  string          `json:"floorBasis"`
	WeightBasis string          `json:"weightBasis"`
	Weights     Weights         `json:"weights"`
	Floors      sizing.Budget   `json:"floors"`
	OldRequests sizing.Budget   `json:"oldRequests"`
	// FixedMemoryMiB overrides only this row in preserveMemory mode. OldRequests
	// remains the actual baseline for ledger deltas; other rows are preserved.
	FixedMemoryMiB     *int64  `json:"fixedMemoryMiB,omitempty"`
	QuietObserved      *bool   `json:"quietObserved"`
	QuietPeakMemoryMiB *Number `json:"quietPeakMemoryMiB"`
	QuietP95CPUMilli   *Number `json:"quietP95CPUMilli"`
}

type Init struct {
	Row         int   `json:"row"`
	Restartable *bool `json:"restartable"`
}

type Pod struct {
	Workload string        `json:"workload"`
	Replicas int64         `json:"replicas"`
	Regular  []int         `json:"regular"`
	Init     []Init        `json:"init"`
	Overhead sizing.Budget `json:"overhead"`
}

type DownstreamRow struct {
	Workload  string        `json:"workload"`
	Container string        `json:"container"`
	Replicas  int64         `json:"replicas"`
	Requests  sizing.Budget `json:"requests"`
}

type Downstream struct {
	Provenance
	sizing.Budget
	Mode                 string          `json:"mode"`
	Limitations          string          `json:"limitations"`
	ActualLayoutRequests sizing.Budget   `json:"actualLayoutRequests"`
	Rows                 []DownstreamRow `json:"rows"`
}

type Exclusion struct {
	Workload       string        `json:"workload"`
	Reason         string        `json:"reason"`
	Requests       sizing.Budget `json:"requests"`
	MemoryLimitMiB int64         `json:"memoryLimitMiB"`
}

type Spec struct {
	Version                 string          `json:"version"`
	Name                    string          `json:"name"`
	BudgetUSDPerHour        Number          `json:"budgetUSDPerHour"`
	Node                    Node            `json:"node"`
	Quota                   Quota           `json:"quota"`
	Inventory               json.RawMessage `json:"inventory"`
	Rows                    []Row           `json:"rows"`
	Pods                    []Pod           `json:"pods"`
	Downstream              Downstream      `json:"downstream"`
	ExcludedInstrumentation []Exclusion     `json:"excludedInstrumentation"`
	Fallback                sizing.Budget   `json:"fallback"`
	FactorGridDenominator   int64           `json:"factorGridDenominator"`
	GoMemLimitPercent       int64           `json:"goMemLimitPercent"`
	PreserveMemory          bool            `json:"preserveMemory,omitempty"`
}

// Decode rejects duplicate keys before the typed decoder can discard evidence.
func Decode(raw []byte) (Spec, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var visit func() error
	visit = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if t == json.Delim('{') {
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name := key.(string)
				if seen[name] {
					return fmt.Errorf("duplicate JSON key %q", name)
				}
				seen[name] = true
				if err := visit(); err != nil {
					return err
				}
			}
			_, err = d.Token()
		} else if t == json.Delim('[') {
			for d.More() {
				if err := visit(); err != nil {
					return err
				}
			}
			_, err = d.Token()
		}
		return err
	}
	if err := visit(); err != nil {
		return Spec{}, err
	}
	if _, err := d.Token(); err != io.EOF {
		return Spec{}, fmt.Errorf("expected exactly one JSON value")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var s Spec
	if err := d.Decode(&s); err != nil {
		return Spec{}, err
	}
	if err := fields(raw, reflect.TypeFor[Spec]()); err != nil {
		return Spec{}, err
	}
	return s, s.validate()
}

// encoding/json accepts case aliases and defaults missing integers to zero.
// Neither is appropriate for an explicit, cost-bearing inventory.
func fields(raw []byte, typ reflect.Type) error {
	if typ == reflect.TypeFor[json.RawMessage]() || typ == reflect.TypeFor[Number]() || typ == reflect.TypeFor[*Number]() {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("null is only allowed for quiet evidence")
	}
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		known := map[string]bool{}
		var walk func(reflect.Type) error
		walk = func(t reflect.Type) error {
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if f.Anonymous {
					if err := walk(f.Type); err != nil {
						return err
					}
					continue
				}
				parts := strings.Split(f.Tag.Get("json"), ",")
				name := parts[0]
				known[name] = true
				value, ok := object[name]
				if !ok {
					if len(parts) > 1 && parts[1] == "omitempty" {
						continue
					}
					return fmt.Errorf("missing field %s", name)
				}
				if err := fields(value, f.Type); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
			}
			return nil
		}
		if err := walk(typ); err != nil {
			return err
		}
		for name := range object {
			if !known[name] {
				return fmt.Errorf("unknown or incorrectly cased field %s", name)
			}
		}
	case reflect.Slice:
		var array []json.RawMessage
		if err := json.Unmarshal(raw, &array); err != nil {
			return err
		}
		for i, item := range array {
			if err := fields(item, typ.Elem()); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
	}
	return nil
}

func (s Spec) validate() error {
	validReceipt := func(p Provenance) bool {
		return p.Basis != "" && regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(p.SHA256)
	}
	positive := func(b sizing.Budget) bool { return b.CPUMilli > 0 && b.MemoryMiB > 0 }
	nonnegative := func(b sizing.Budget) bool { return b.CPUMilli >= 0 && b.MemoryMiB >= 0 }
	validNumber := func(n Number) bool { r, ok := new(big.Rat).SetString(string(n)); return ok && r.Sign() > 0 }
	if s.Version != "hcp-perf.budget/v1" || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(s.Name) || !validNumber(s.BudgetUSDPerHour) || !validNumber(s.Node.PriceUSDPerHour) {
		return fmt.Errorf("invalid version, name, budget or price")
	}
	n := s.Node
	_, dateErr := time.Parse(time.RFC3339Nano, n.RetrievedAt)
	if !validReceipt(n.Provenance) || !positive(n.Budget) || n.Currency != "USD" || n.CapacityBasis != "raw" || n.SKU == "" || n.Region == "" || n.SourceURL == "" || n.CapacitySource == "" || dateErr != nil {
		return fmt.Errorf("USD/raw node capacity and complete price provenance required")
	}
	if !validReceipt(s.Quota.Provenance) || s.Quota.PeriodUS <= 0 || s.Quota.MinimumUS <= 0 || s.Quota.QuantumUS <= 0 || s.FactorGridDenominator <= 0 || !positive(s.Fallback) || s.GoMemLimitPercent < 1 || s.GoMemLimitPercent > 100 {
		return fmt.Errorf("invalid quota, grid, fallback or Go memory policy")
	}
	var inventory struct {
		Provenance
		ScopeMethod       string            `json:"scopeMethod"`
		SourceAuditHashes map[string]string `json:"sourceAuditHashes"`
		Windows           []json.RawMessage `json:"windows"`
		Limitations       string            `json:"limitations"`
	}
	if err := json.Unmarshal(s.Inventory, &inventory); err != nil {
		return err
	}
	if !validReceipt(inventory.Provenance) || inventory.ScopeMethod != "source-verified-projector" || len(inventory.Windows) == 0 || inventory.Limitations == "" {
		return fmt.Errorf("scoped inventory receipt/windows/limitations required")
	}
	if len(s.Rows) == 0 || len(s.Rows) > 256 || len(s.Pods) == 0 {
		return fmt.Errorf("nonempty inventory/layout required (at most 256 rows)")
	}
	keys, workloads := map[[2]string]bool{}, map[string]bool{}
	for i, row := range s.Rows {
		id := row.Identity
		for _, text := range []string{id.Kind, id.Workload, id.Container} {
			if text == "" || strings.TrimSpace(text) != text {
				return fmt.Errorf("row %d: invalid identity", i)
			}
		}
		key := [2]string{id.Workload, id.Container}
		if keys[key] {
			return fmt.Errorf("duplicate workload/container %v", key)
		}
		keys[key], workloads[id.Workload] = true, true
		if row.Replicas < 0 || row.NonGo == nil || row.QuietObserved == nil || !positive(row.Floors) || !positive(row.OldRequests) || !validNumber(row.Weights.CPUMilli) || !validNumber(row.Weights.MemoryMiB) {
			return fmt.Errorf("row %d: invalid counts, explicit classifications, weights or floors", i)
		}
		if row.GoMaxProcs != nil && (*row.NonGo || *row.GoMaxProcs < 1 || *row.GoMaxProcs > 1024) {
			return fmt.Errorf("row %d: goMaxProcs requires a Go container and a value in [1,1024]", i)
		}
		if row.FixedMemoryMiB != nil && (!s.PreserveMemory || *row.FixedMemoryMiB <= 0 || *row.FixedMemoryMiB < row.Floors.MemoryMiB) {
			return fmt.Errorf("row %d: fixedMemoryMiB requires preserveMemory and a positive value at or above the protected floor", i)
		}
		if !validReceipt(row.Source.Provenance) || row.Source.Method != "source-verified-projector" || row.Source.Path == "" || inventory.SourceAuditHashes[row.Source.Path] != row.Source.SHA256 || row.StartupNote == "" || row.FloorBasis == "" || row.WeightBasis == "" {
			return fmt.Errorf("row %d: source audit/basis missing or mismatched", i)
		}
		if *row.QuietObserved && (row.QuietPeakMemoryMiB == nil || row.QuietP95CPUMilli == nil) {
			return fmt.Errorf("row %d: missing quiet evidence", i)
		}
		for _, evidence := range []*Number{row.QuietPeakMemoryMiB, row.QuietP95CPUMilli} {
			if evidence != nil && !validNumber(*evidence) {
				return fmt.Errorf("row %d: invalid quiet evidence", i)
			}
		}
	}
	counts := make([]*big.Int, len(s.Rows))
	for i := range counts {
		counts[i] = new(big.Int)
	}
	for _, pod := range s.Pods {
		if pod.Replicas <= 0 || len(pod.Regular) == 0 || !nonnegative(pod.Overhead) {
			return fmt.Errorf("invalid Pod layout %q", pod.Workload)
		}
		seen := map[int]bool{}
		bind := func(i int, init, running bool) error {
			if i < 0 || i >= len(s.Rows) || seen[i] {
				return fmt.Errorf("invalid/duplicate Pod row %d", i)
			}
			seen[i] = true
			id := s.Rows[i].Identity
			if id.Workload != pod.Workload || id.InitContainer != init {
				return fmt.Errorf("Pod binding identity mismatch for %d", i)
			}
			if running {
				counts[i].Add(counts[i], big.NewInt(pod.Replicas))
			}
			return nil
		}
		for _, i := range pod.Regular {
			if err := bind(i, false, true); err != nil {
				return err
			}
		}
		for _, init := range pod.Init {
			if init.Restartable == nil {
				return fmt.Errorf("explicit restartable classification required")
			}
			if err := bind(init.Row, true, *init.Restartable); err != nil {
				return err
			}
		}
	}
	for i, row := range s.Rows {
		if counts[i].Cmp(big.NewInt(row.Replicas)) != 0 {
			return fmt.Errorf("row %d: replica/layout mismatch", i)
		}
	}
	d := s.Downstream
	if !validReceipt(d.Provenance) || d.Mode != "fixed-requests" || d.Limitations == "" || !nonnegative(d.Budget) || !nonnegative(d.ActualLayoutRequests) {
		return fmt.Errorf("explicit downstream accounting required")
	}
	cpu, memory := new(big.Int), new(big.Int)
	for _, row := range d.Rows {
		key := [2]string{row.Workload, row.Container}
		if keys[key] || row.Workload == "" || row.Container == "" || row.Replicas <= 0 || !positive(row.Requests) {
			return fmt.Errorf("invalid/overlapping downstream row %v", key)
		}
		keys[key], workloads[row.Workload] = true, true
		cpu.Add(cpu, new(big.Int).Mul(big.NewInt(row.Requests.CPUMilli), big.NewInt(row.Replicas)))
		memory.Add(memory, new(big.Int).Mul(big.NewInt(row.Requests.MemoryMiB), big.NewInt(row.Replicas)))
	}
	if cpu.Cmp(big.NewInt(d.CPUMilli)) != 0 || memory.Cmp(big.NewInt(d.MemoryMiB)) != 0 {
		return fmt.Errorf("downstream totals differ from inventory")
	}
	if len(s.ExcludedInstrumentation) == 0 {
		return fmt.Errorf("explicit instrumentation exclusions required")
	}
	for _, row := range s.ExcludedInstrumentation {
		if row.Workload == "" || row.Reason == "" || workloads[row.Workload] || !positive(row.Requests) {
			return fmt.Errorf("invalid/overlapping instrumentation exclusion")
		}
	}
	return nil
}

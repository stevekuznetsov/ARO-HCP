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
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	core "github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/dashboard"
)

type CommandMode uint8

const (
	Export CommandMode = iota + 1
	Render
	Overview
)

type RawOptions struct {
	Mode   CommandMode
	Input  string
	Output string
}

type ValidatedOptions struct{ raw RawOptions }

type Options struct {
	mode     CommandMode
	output   string
	spec     core.ExportSpec
	dataset  core.Dataset
	overview core.Overview
}

func NewRawOptions(mode CommandMode) *RawOptions { return &RawOptions{Mode: mode} }

func (o *RawOptions) BindOptions(flags *pflag.FlagSet) {
	inputHelp := "Portable dashboard JSON (required; at most 32 MiB)"
	if o.Mode == Export {
		inputHelp = "Private reviewed export manifest JSON (required; at most 32 MiB)"
	} else if o.Mode == Overview {
		inputHelp = "Public overview manifest with relative portable JSON paths (required; at most 32 MiB)"
	}
	flags.StringVar(&o.Input, "input", o.Input, inputHelp)
	flags.StringVar(&o.Output, "output", o.Output, "New output file; parent must already exist (required; never overwritten)")
}

// Validate checks flags without reading evidence or creating output.
func (o *RawOptions) Validate(ctx context.Context) (*ValidatedOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.Mode != Export && o.Mode != Render && o.Mode != Overview {
		return nil, errors.New("invalid dashboard command mode")
	}
	if strings.TrimSpace(o.Input) == "" || strings.TrimSpace(o.Output) == "" {
		return nil, errors.New("--input and --output are required")
	}
	return &ValidatedOptions{raw: *o}, nil
}

// Complete resolves paths and parses input, but never creates output.
func (v *ValidatedOptions) Complete(ctx context.Context) (*Options, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input, err := filepath.Abs(v.raw.Input)
	if err != nil {
		return nil, fmt.Errorf("resolve input: %w", err)
	}
	output, err := filepath.Abs(v.raw.Output)
	if err != nil {
		return nil, fmt.Errorf("resolve output: %w", err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return nil, fmt.Errorf("output already exists: %s", output)
		}
		return nil, fmt.Errorf("inspect output: %w", err)
	}
	parent, err := os.Stat(filepath.Dir(output))
	if err != nil {
		return nil, fmt.Errorf("output parent must exist: %w", err)
	}
	if !parent.IsDir() {
		return nil, errors.New("output parent must be a directory")
	}
	info, err := os.Stat(input)
	if err != nil {
		return nil, fmt.Errorf("inspect input: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > core.MaxInputBytes {
		return nil, errors.New("input must be a regular JSON file of at most 32 MiB")
	}
	o := &Options{mode: v.raw.Mode, output: output}
	if o.mode == Export {
		o.spec, err = core.ReadExportSpec(input)
		if err != nil {
			return nil, fmt.Errorf("read export manifest: %w", err)
		}
	} else if o.mode == Overview {
		o.overview, err = readOverview(ctx, input)
		if err != nil {
			return nil, err
		}
	} else {
		file, err := os.Open(input)
		if err != nil {
			return nil, fmt.Errorf("open input: %w", err)
		}
		defer file.Close()
		if err := readJSON(file, &o.dataset); err != nil {
			return nil, err
		}
		if err := validateDataset(o.dataset); err != nil {
			return nil, err
		}
	}
	return o, ctx.Err()
}

func (o *Options) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Finish all input-dependent work before exclusively creating the destination.
	var buffer bytes.Buffer
	switch o.mode {
	case Export:
		dataset, err := core.Export(ctx, o.spec)
		if err != nil {
			return err
		}
		if err := validateDataset(dataset); err != nil {
			return err
		}
		encoder := json.NewEncoder(&buffer)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(dataset); err != nil {
			return fmt.Errorf("encode dashboard: %w", err)
		}
		if buffer.Len() > core.MaxInputBytes {
			return errors.New("exported dashboard exceeds 32 MiB")
		}
	case Render:
		if err := core.Render(&buffer, o.dataset); err != nil {
			return err
		}
	case Overview:
		if err := core.RenderOverview(&buffer, o.overview); err != nil {
			return err
		}
	default:
		return errors.New("invalid dashboard command mode")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.OpenFile(o.output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	_, writeErr := buffer.WriteTo(file)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// Shared by render and overview so portable input has exactly the same bounds.
func readJSON(file *os.File, value any) error {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > core.MaxInputBytes {
		return errors.New("input must be a regular JSON file of at most 32 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, core.MaxInputBytes+1))
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	if len(data) > core.MaxInputBytes {
		return errors.New("input exceeds 32 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode dashboard: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("input must contain exactly one JSON value")
	}
	return nil
}

// These structural checks do not rescore supplied assessments or infer missing data.
func validateDataset(dataset core.Dataset) error {
	if dataset.SchemaVersion != core.SchemaVersion || len(dataset.Sizes) == 0 {
		return errors.New("dashboard requires the supported schema and at least one size")
	}
	unique := func(seen map[string]bool, id string) bool {
		if strings.TrimSpace(id) == "" || seen[id] {
			return false
		}
		seen[id] = true
		return true
	}
	sources, sizes := map[string]bool{}, map[string]bool{}
	for _, source := range dataset.Sources {
		hash, err := hex.DecodeString(source.SHA256)
		if !unique(sources, source.ID) || source.Label == "" || err != nil || len(hash) != 32 {
			return errors.New("sources require unique IDs, labels and SHA256 hashes")
		}
	}
	for _, size := range dataset.Sizes {
		if !unique(sizes, size.ID) || size.Name == "" {
			return errors.New("sizes require unique IDs and names")
		}
		b := size.Budget
		for _, value := range []float64{b.FeeUSD, b.TargetUSD, b.ModeledUSD, b.TargetCPUCores, b.TargetMemoryGiB, b.ChargedCPUCores, b.ChargedMemoryGiB, b.RequestedCPUCores, b.RequestedMemoryGiB} {
			if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("size %s: invalid budget value", size.ID)
			}
		}
		components, runs := map[string]bool{}, map[string]bool{}
		for _, component := range size.Components {
			if !unique(components, component.ID) {
				return fmt.Errorf("size %s: duplicate or empty component ID", size.ID)
			}
		}
		for _, run := range size.Runs {
			if !unique(runs, run.ID) {
				return fmt.Errorf("size %s: duplicate or empty run ID", size.ID)
			}
			if (run.Kind != "screen" && run.Kind != "confirmation") || (run.Population != "loaded" && run.Population != "unloaded") {
				return fmt.Errorf("run %s: invalid kind or population", run.ID)
			}
			r := run.Requests
			for _, value := range []int{run.ApplicationPods, run.Applications, run.GuestNodes, run.TurnoverPeriodSeconds, run.RolloutsCompleted, run.RolloutsScheduled, run.CanaryCycles, run.CanarySoftDegraded, r.Offered, r.Dispatched, r.Successful, r.Rejected429, r.Timeouts, r.DegradedUnion, r.Canceled, r.OtherErrors, r.GeneratorLimited} {
				if value < 0 {
					return fmt.Errorf("run %s: negative count", run.ID)
				}
			}
			for _, value := range []*int{run.Restarts, run.OOMs} {
				if value != nil && *value < 0 {
					return fmt.Errorf("run %s: negative stability count", run.ID)
				}
			}
			if run.CanaryHardFailed != nil && *run.CanaryHardFailed < 0 {
				return fmt.Errorf("run %s: canaryHardFailed must be null or nonnegative", run.ID)
			}
			for _, value := range []float64{run.OfferedQPS, run.PlannedSeconds, run.ActualSeconds, r.SuccessfulQPS} {
				if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
					return fmt.Errorf("run %s: invalid duration or rate", run.ID)
				}
			}
			if value := r.MaxDegradedFraction; value != nil && (*value < 0 || *value > 1 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
				return fmt.Errorf("run %s: maxDegradedFraction must be null or finite in [0,1]", run.ID)
			}
			if r.Dispatched > r.Offered || r.Successful > r.Dispatched || r.DegradedUnion > r.Dispatched || r.Rejected429 > r.DegradedUnion || r.Timeouts > r.DegradedUnion {
				return fmt.Errorf("run %s: inconsistent request counts", run.ID)
			}
			refs := map[string]bool{}
			for _, ref := range run.Sources {
				if !sources[ref] || !unique(refs, ref) {
					return fmt.Errorf("run %s: unknown or duplicate source reference", run.ID)
				}
			}
			for _, metrics := range [][]core.MetricDistribution{run.Latencies, run.WholeHCP} {
				ids := map[string]bool{}
				for _, metric := range metrics {
					if !unique(ids, metric.ID) || metric.Censored < 0 || metric.Excluded < 0 {
						return fmt.Errorf("run %s: invalid metric ID or coverage", run.ID)
					}
					if err := validateDistribution(metric.Distribution); err != nil {
						return fmt.Errorf("run %s metric %s: %w", run.ID, metric.ID, err)
					}
				}
			}
			ids := map[string]bool{}
			for _, usage := range run.Resources {
				if !components[usage.ComponentID] || !unique(ids, usage.ComponentID) || usage.Replicas < 0 || usage.Containers < 0 {
					return fmt.Errorf("run %s: invalid component usage", run.ID)
				}
				for _, d := range []*core.Distribution{usage.CPU, usage.Memory, usage.WorkingSet, usage.RSS} {
					if err := validateDistribution(d); err != nil {
						return fmt.Errorf("run %s component %s: %w", run.ID, usage.ComponentID, err)
					}
				}
				for _, value := range []*float64{usage.CPURequest, usage.CPULimit, usage.MemoryRequest, usage.MemoryLimit, usage.GoTarget} {
					if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
						return fmt.Errorf("run %s: invalid resource reference value", run.ID)
					}
				}
			}
		}
		if (len(size.Runs) > 0 || size.DefaultRun != "") && !runs[size.DefaultRun] {
			return fmt.Errorf("size %s: defaultRun must reference a supplied run", size.ID)
		}
	}
	return nil
}

func validateDistribution(d *core.Distribution) error {
	if d == nil {
		return nil
	}
	if d.Count <= 0 || d.Expected < d.Count || d.Missing != d.Expected-d.Count || d.OutlierCount < 0 || d.OutlierCount > d.Count {
		return errors.New("invalid distribution coverage")
	}
	previous := 0.0
	for _, value := range []float64{d.Min, d.Q1, d.Median, d.Q3, d.P95, d.P99, d.Max} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < previous {
			return errors.New("distribution percentiles must be finite, nonnegative and monotonic")
		}
		previous = value
	}
	if math.IsNaN(d.WhiskerLow) || math.IsNaN(d.WhiskerHigh) || d.WhiskerLow < d.Min || d.WhiskerLow > d.Q1 || d.WhiskerHigh < d.Q3 || d.WhiskerHigh > d.Max {
		return errors.New("invalid distribution whiskers")
	}
	return nil
}

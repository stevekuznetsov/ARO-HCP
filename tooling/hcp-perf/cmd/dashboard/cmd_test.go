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
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	core "github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/dashboard"
)

func execute(ctx context.Context, out io.Writer, args ...string) error {
	command := NewCommand()
	command.SetContext(ctx)
	command.SetOut(out)
	command.SetErr(out)
	command.SetArgs(args)
	return command.Execute()
}

// Synthetic summaries are test-only, never published as measured evidence.
func fixture() core.Dataset {
	return core.Dataset{
		SchemaVersion: core.SchemaVersion, Title: "Synthetic test dashboard",
		Sizes: []core.Size{{
			ID: "test", Name: "Test", DefaultRun: "run",
			Components: []core.Component{{ID: "component", Name: "Test component", Scope: "controlled"}},
			Runs: []core.Run{{
				ID: "run", Kind: "screen", Population: "loaded",
				Sources: []string{"source"},
				Latencies: []core.MetricDistribution{{ID: "latency", Distribution: &core.Distribution{
					Count: 1, Expected: 1, Min: 1, Q1: 1, Median: 1, Q3: 1, P95: 1, P99: 1, Max: 1, WhiskerLow: 1, WhiskerHigh: 1,
				}}},
				Resources: []core.ComponentUsage{{ComponentID: "component"}},
			}},
		}},
		Sources: []core.Source{{ID: "source", Label: "Synthetic source", SHA256: strings.Repeat("a", 64)}},
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected output %s: %v", path, err)
	}
}

func TestRejectFlags(t *testing.T) {
	for _, args := range [][]string{
		{}, {"unknown"}, {"export"}, {"render"},
		{"render", "--input", "missing"}, {"render", "--output", "new"},
		{"render", "--input", "missing", "--output", "new", "--unknown"},
		{"export", "--input", "missing", "--output", "new", "positional"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output bytes.Buffer
			if err := execute(context.Background(), &output, args...); err == nil {
				t.Fatal("invalid flags succeeded")
			}
			if output.Len() != 0 {
				t.Fatalf("Cobra duplicated outer error reporting: %s", &output)
			}
		})
	}
	for _, verb := range []string{"export", "render"} {
		var output bytes.Buffer
		if err := execute(context.Background(), &output, verb, "--help"); err != nil || !strings.Contains(output.String(), "--input") {
			t.Fatalf("missing help for %s: %v", verb, err)
		}
	}
}

func TestOptionsPhases(t *testing.T) {
	dir := t.TempDir()
	raw := NewRawOptions(Render)
	flags := pflag.NewFlagSet("render", pflag.ContinueOnError)
	raw.BindOptions(flags)
	if err := flags.Parse([]string{"--input", filepath.Join(dir, "missing"), "--output", filepath.Join(dir, "new.html")}); err != nil {
		t.Fatal(err)
	}
	validated, err := raw.Validate(context.Background())
	if err != nil {
		t.Fatalf("Validate accessed the filesystem: %v", err)
	}
	raw.Input = filepath.Join(dir, "input.json")
	writeJSON(t, raw.Input, fixture())
	if _, err := validated.Complete(context.Background()); err == nil {
		t.Fatal("Validate did not snapshot raw flags")
	}
	validated, err = raw.Validate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	options, err := validated.Complete(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, raw.Output)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := raw.Validate(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Validate lost cancellation: %v", err)
	}
	if _, err := validated.Complete(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete lost cancellation: %v", err)
	}
	if err := options.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run lost cancellation: %v", err)
	}
	assertAbsent(t, raw.Output)
	// A destination created after Complete must still be preserved by O_EXCL.
	if err := os.WriteFile(raw.Output, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := options.Run(context.Background()); !errors.Is(err, os.ErrExist) {
		t.Fatalf("Run overwrote a concurrent destination: %v", err)
	}
	data, err := os.ReadFile(raw.Output)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("existing bytes changed: %q, %v", data, err)
	}
}

func TestStrictJSON(t *testing.T) {
	valid, err := json.Marshal(fixture())
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"empty": nil, "null": []byte("null"), "malformed": []byte("{"),
		"unknown":        []byte(`{"unknown":true}`),
		"nested unknown": bytes.Replace(valid, []byte(`"name":"Test"`), []byte(`"typo":"Test"`), 1),
		"trailing value": append(append([]byte{}, valid...), []byte(` {}`)...),
		"trailing junk":  append(append([]byte{}, valid...), 'x'),
		"oversized":      bytes.Repeat([]byte(" "), core.MaxInputBytes+1),
		"nonfinite":      bytes.Replace(valid, []byte(`"p95":1`), []byte(`"p95":1e999`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			input, output := filepath.Join(dir, "input.json"), filepath.Join(dir, "output.html")
			if err := os.WriteFile(input, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := execute(context.Background(), io.Discard, "render", "--input", input, "--output", output); err == nil {
				t.Fatal("invalid JSON succeeded")
			}
			assertAbsent(t, output)
		})
	}
}

func TestSemanticValidation(t *testing.T) {
	for name, edit := range map[string]func(*core.Dataset){
		"negative budget":        func(d *core.Dataset) { d.Sizes[0].Budget.FeeUSD = -1 },
		"negative count":         func(d *core.Dataset) { d.Sizes[0].Runs[0].Requests.Timeouts = -1 },
		"request accounting":     func(d *core.Dataset) { d.Sizes[0].Runs[0].Requests.Successful = 1 },
		"request fraction":       func(d *core.Dataset) { v := 2.0; d.Sizes[0].Runs[0].Requests.MaxDegradedFraction = &v },
		"negative fraction":      func(d *core.Dataset) { v := -0.1; d.Sizes[0].Runs[0].Requests.MaxDegradedFraction = &v },
		"negative hard failures": func(d *core.Dataset) { v := -1; d.Sizes[0].Runs[0].CanaryHardFailed = &v },
		"kind":                   func(d *core.Dataset) { d.Sizes[0].Runs[0].Kind = "unknown" },
		"schema":                 func(d *core.Dataset) { d.SchemaVersion = "unknown" },
		"size ID":                func(d *core.Dataset) { d.Sizes = append(d.Sizes, d.Sizes[0]) },
		"run ID":                 func(d *core.Dataset) { d.Sizes[0].Runs = append(d.Sizes[0].Runs, d.Sizes[0].Runs[0]) },
		"component ID":           func(d *core.Dataset) { d.Sizes[0].Components = append(d.Sizes[0].Components, d.Sizes[0].Components[0]) },
		"default run":            func(d *core.Dataset) { d.Sizes[0].DefaultRun = "absent" },
		"source ref":             func(d *core.Dataset) { d.Sizes[0].Runs[0].Sources[0] = "absent" },
		"source hash":            func(d *core.Dataset) { d.Sources[0].SHA256 = "invalid" },
		"component ref":          func(d *core.Dataset) { d.Sizes[0].Runs[0].Resources[0].ComponentID = "absent" },
		"percentiles":            func(d *core.Dataset) { d.Sizes[0].Runs[0].Latencies[0].Distribution.P95 = 2 },
		"coverage":               func(d *core.Dataset) { d.Sizes[0].Runs[0].Latencies[0].Distribution.Missing = 1 },
		"whisker":                func(d *core.Dataset) { d.Sizes[0].Runs[0].Latencies[0].Distribution.WhiskerHigh = 2 },
		"reference value":        func(d *core.Dataset) { v := -1.0; d.Sizes[0].Runs[0].Resources[0].GoTarget = &v },
	} {
		t.Run(name, func(t *testing.T) {
			dataset := fixture()
			edit(&dataset)
			dir := t.TempDir()
			input, output := filepath.Join(dir, "input.json"), filepath.Join(dir, "output.html")
			writeJSON(t, input, dataset)
			if err := execute(context.Background(), io.Discard, "render", "--input", input, "--output", output); err == nil {
				t.Fatal("invalid semantics succeeded")
			}
			assertAbsent(t, output)
		})
	}
	d := fixture().Sizes[0].Runs[0].Latencies[0].Distribution
	d.P95 = math.NaN()
	if err := validateDistribution(d); err == nil {
		t.Fatal("NaN accepted")
	}
}

func TestNullablePolicyAndHardFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		fields string
	}{
		{"unknown", `"maxDegradedFraction":null,"canaryHardFailed":null`},
		{"explicit zero", `"maxDegradedFraction":0,"canaryHardFailed":0`},
		{"allowance", `"maxDegradedFraction":0.05,"canaryHardFailed":2`},
		{"upper boundary", `"maxDegradedFraction":1,"canaryHardFailed":50`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var values struct {
				Fraction   *float64 `json:"maxDegradedFraction"`
				HardFailed *int     `json:"canaryHardFailed"`
			}
			if err := json.Unmarshal([]byte("{"+test.fields+"}"), &values); err != nil {
				t.Fatal(err)
			}
			dataset := fixture()
			run := &dataset.Sizes[0].Runs[0]
			run.Requests.MaxDegradedFraction = values.Fraction
			run.CanaryHardFailed = values.HardFailed
			// Do not infer an upper bound from a potentially different cycle cohort.
			run.CanaryCycles = 0
			dir := t.TempDir()
			input := filepath.Join(dir, "input.json")
			writeJSON(t, input, dataset)
			validated, err := (&RawOptions{Mode: Render, Input: input, Output: filepath.Join(dir, "new.html")}).Validate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			options, err := validated.Complete(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := options.dataset.Sizes[0].Runs[0]
			if (got.Requests.MaxDegradedFraction == nil) != (values.Fraction == nil) || (got.CanaryHardFailed == nil) != (values.HardFailed == nil) {
				t.Fatal("null and explicit zero were conflated")
			}
			if values.Fraction != nil && *got.Requests.MaxDegradedFraction != *values.Fraction {
				t.Fatal("request fraction changed during decoding")
			}
			if values.HardFailed != nil && *got.CanaryHardFailed != *values.HardFailed {
				t.Fatal("hard failure count changed during decoding")
			}
			if err := options.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		dataset := fixture()
		dataset.Sizes[0].Runs[0].Requests.MaxDegradedFraction = &value
		if err := validateDataset(dataset); err == nil || !strings.Contains(err.Error(), "maxDegradedFraction") {
			t.Fatalf("nonfinite fraction did not produce a field-specific error: %v", err)
		}
	}
}

func TestPathsAndExclusiveOutput(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.json")
	writeJSON(t, input, fixture())
	for _, badInput := range []string{filepath.Join(dir, "missing"), dir} {
		output := filepath.Join(dir, "new.html")
		if err := execute(context.Background(), io.Discard, "render", "--input", badInput, "--output", output); err == nil {
			t.Fatal("invalid input path succeeded")
		}
		assertAbsent(t, output)
	}
	parent := filepath.Join(dir, "missing-parent")
	if err := execute(context.Background(), io.Discard, "render", "--input", input, "--output", filepath.Join(parent, "new.html")); err == nil {
		t.Fatal("missing output parent accepted")
	}
	assertAbsent(t, parent)
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink(filepath.Join(dir, "absent-target"), link); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{input, dir, link} {
		if err := execute(context.Background(), io.Discard, "render", "--input", input, "--output", output); err == nil {
			t.Fatal("existing destination accepted")
		}
	}
	assertAbsent(t, filepath.Join(dir, "absent-target"))
}

func TestRenderInertSourcesAndEscaping(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "private")
	if err := os.WriteFile(secret, []byte("PRIVATE_CONTENT_MUST_NOT_APPEAR"), 0600); err != nil {
		t.Fatal(err)
	}
	dataset := fixture()
	payload := `</script><script>alert(1)</script><img src=x onerror=alert(1)>`
	dataset.Title = payload
	dataset.Sources[0].Label = secret
	dataset.Sources = append(dataset.Sources, core.Source{ID: "url", Label: "https://127.0.0.1:1/never-fetch", SHA256: strings.Repeat("b", 64)})
	input, output := filepath.Join(dir, "input.json"), filepath.Join(dir, "new.html")
	writeJSON(t, input, dataset)
	if err := execute(context.Background(), io.Discard, "render", "--input", input, "--output", output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{payload, "PRIVATE_CONTENT_MUST_NOT_APPEAR", "<script src", "fetch(", ".innerHTML"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Errorf("unsafe rendering contains %q", forbidden)
		}
	}
	if !bytes.Contains(data, []byte("connect-src 'none'")) || !bytes.Contains(data, []byte(`\u003c/script\u003e`)) {
		t.Fatal("missing CSP or default JSON escaping")
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("output is not private: %v, %v", info, err)
	}
}

func TestExportManifestBaseAndConfinement(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../escape.json", filepath.Join(dir, "absolute.json"), "missing.json"} {
		spec := core.ExportSpec{
			SchemaVersion: core.ExportSchemaVersion, BaseDirectory: "private", Dataset: fixture(),
			Runs: []core.RunInput{{SizeID: "test", RunID: "run", Sources: core.RunSources{
				Spec: core.InputRef{Path: path, ID: "source", Label: "Source", SHA256: strings.Repeat("a", 64)},
			}}},
		}
		input, output := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "new.json")
		writeJSON(t, input, spec)
		raw := &RawOptions{Mode: Export, Input: input, Output: output}
		validated, err := raw.Validate(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		options, err := validated.Complete(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if options.spec.BaseDirectory != private || !filepath.IsAbs(options.spec.BaseDirectory) {
			t.Fatalf("base resolved against CWD instead of manifest: %s", options.spec.BaseDirectory)
		}
		if err := options.Run(context.Background()); err == nil {
			t.Fatalf("unsafe/missing source %q accepted", path)
		}
		assertAbsent(t, output)
	}
}

func TestRenderReviewedSmallDataset(t *testing.T) {
	input := "../../docs/dashboard/small.json"
	if _, err := os.Stat(input); errors.Is(err, os.ErrNotExist) {
		t.Skip("reviewed dataset not included in this checkout")
	}
	output := filepath.Join(t.TempDir(), "small.html")
	if err := execute(context.Background(), io.Discard, "render", "--input", input, "--output", output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"envelope-observe5-hour-004", "Request quality: passed", "Health: failed", "Not populated"} {
		if !bytes.Contains(data, []byte(text)) {
			t.Errorf("reviewed output missing %q", text)
		}
	}
}

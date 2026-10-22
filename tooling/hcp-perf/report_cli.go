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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/assessment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/progress"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/report"
)

// runReport accepts the full argv including "report render". Parent command
// dispatch is deliberately separate. Repeated --metrics flags select exact
// manifest roots; --input may be omitted for a metrics-only report.
func runReport(ctx context.Context, args []string, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) < 2 || args[0] != "report" || args[1] != "render" {
		return fmt.Errorf("expected report render --input RUN_DIR --metrics DIR --output NEW_HTML")
	}
	f := flag.NewFlagSet("report render", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	input := f.String("input", "", "local run directory (optional for metrics-only reports)")
	output := f.String("output", "", "new HTML file; parent must exist")
	assessmentFile := f.String("assessment", "", "existing assessment JSON; checked against input evidence when --input is supplied")
	recorderSummary := f.String("recorder-summary", "", "extracted recorder stream summary for building or validating an assessment; requires --input")
	requestEvidence := f.String("request-evidence", "", "reviewer-correlated request evidence for building or validating an assessment; requires --input")
	maxSeries := f.Int("max-series", report.DefaultMaxSeries, "overall preview series limit (1..1000; non-up queries limited to 20 each)")
	maxPoints := f.Int("max-points", report.DefaultMaxPoints, "preview points per series (2..500)")
	var dirs []string
	f.Func("metrics", "metrics manifest root; repeat for multiple exports", func(dir string) error {
		if dir == "" {
			return fmt.Errorf("metrics directory must not be empty")
		}
		dirs = append(dirs, dir)
		return nil
	})
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if *output == "" || (*input == "" && len(dirs) == 0 && *assessmentFile == "") || f.NArg() != 0 {
		return fmt.Errorf("report render requires --output NEW_HTML, --input, --metrics and/or --assessment, and no positional arguments")
	}
	if *recorderSummary != "" && *input == "" {
		return fmt.Errorf("--recorder-summary requires --input")
	}
	if *requestEvidence != "" && *input == "" {
		return fmt.Errorf("--request-evidence requires --input")
	}
	limits := report.Limits{MaxSeries: *maxSeries, MaxPoints: *maxPoints}
	if err := limits.Validate(); err != nil {
		return err
	}
	p := progress.Result{Version: "v1", ObservedAt: time.Now().UTC(), Phase: "incomplete", ActivityPhase: "incomplete", Provisional: true}
	var assessed *assessment.Result
	if *assessmentFile != "" {
		file, err := os.Open(*assessmentFile)
		if err != nil {
			return err
		}
		// Imported assessments are explicit inputs, but must remain bounded JSON.
		const maxAssessmentBytes = 32 << 20
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > maxAssessmentBytes {
			file.Close()
			return fmt.Errorf("assessment must be a regular JSON file of at most 32 MiB")
		}
		body, readErr := io.ReadAll(io.LimitReader(file, maxAssessmentBytes+1))
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if len(body) > maxAssessmentBytes {
			return fmt.Errorf("assessment exceeds 32 MiB")
		}
		result, err := assessment.Decode(body)
		if err != nil {
			return err
		}
		assessed = &result
	}
	if *input != "" {
		var err error
		p, err = progress.InspectContext(ctx, *input, p.ObservedAt)
		if err != nil {
			p.Warnings = append(p.Warnings, progress.Warning{Source: "run directory", Reason: err.Error()})
		}
		built, err := assessment.Build(ctx, assessment.Options{Input: *input, Metrics: dirs, RecorderSummary: *recorderSummary, RequestEvidence: *requestEvidence, Now: p.ObservedAt})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if assessed != nil || *recorderSummary != "" || *requestEvidence != "" {
				return fmt.Errorf("assess input: %w", err)
			}
			p.Warnings = append(p.Warnings, progress.Warning{Source: "assessment", Reason: err.Error()})
		} else if assessed == nil {
			assessed = &built
		} else {
			if a, b := assessed.Experiment.ID, built.Experiment.ID; a != "" && a != "unknown" && b != "" && b != "unknown" && a != b {
				return fmt.Errorf("assessment experiment ID does not match explicit input evidence")
			}
			// Compare only paths read by Build's allowlist. Imported paths never
			// authorize filesystem reads, and a directory basename is not an ID.
			current := make(map[string]string, len(built.Evidence))
			for _, e := range built.Evidence {
				current[e.Path] = e.SHA256
			}
			matched := false
			for _, e := range assessed.Evidence {
				hash, ok := current[e.Path]
				if !ok {
					return fmt.Errorf("assessment evidence cannot be verified: %s; restore missing run artifacts and supply the original --metrics and --recorder-summary sources, and --request-evidence when used", e.Path)
				}
				if e.SHA256 != hash {
					return fmt.Errorf("assessment evidence differs from input: %s", e.Path)
				}
				// A shared external metrics export alone does not identify a run.
				if e.Path != ".." && !strings.HasPrefix(e.Path, "../") {
					matched = true
				}
			}
			if !matched {
				return fmt.Errorf("assessment has no matching evidence tying it to --input")
			}
			// Newly recorded failures may leave every old evidence hash unchanged.
			if assessed.Status.Workload != built.Status.Workload {
				return fmt.Errorf("assessment workload status %q differs from current input status %q; rebuild the assessment", assessed.Status.Workload, built.Status.Workload)
			}
			if assessed.Status.Capture != built.Status.Capture && !(assessed.Status.Capture == "incomplete" && built.Status.Capture == "unknown") {
				return fmt.Errorf("assessment capture status %q differs from current input status %q; rebuild the assessment", assessed.Status.Capture, built.Status.Capture)
			}
			if (len(assessed.Failures) > 0 || len(built.Failures) > 0) && !reflect.DeepEqual(assessed.Failures, built.Failures) {
				return fmt.Errorf("assessment failures or server observations differ from current input; supply the original --request-evidence and rebuild the assessment")
			}
		}
	}
	sources, err := report.LoadMetricsWithLimits(ctx, dirs, limits)
	if err != nil {
		return err
	}
	var html bytes.Buffer
	page := report.Compose(p, sources)
	page.Assessment = assessed
	page.Limits = limits
	if err := report.Render(&html, page); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	htmlSize, writeErr := html.WriteTo(file)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Version       string        `json:"version"`
		Output        string        `json:"output"`
		Bytes         int           `json:"bytes"`
		Limits        report.Limits `json:"limits"`
		TotalSeries   int           `json:"totalSeries"`
		PreviewSeries int           `json:"previewSeries"`
		OmittedSeries int           `json:"omittedSeries"`
		PlottedPoints int           `json:"plottedPoints"`
	}{"v1", *output, int(htmlSize), limits, page.TotalSeries, page.PreviewSeries, page.OmittedSeries, page.PlottedPoints})
}

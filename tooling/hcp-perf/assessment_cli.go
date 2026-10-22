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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/assessment"
)

// runAssessment accepts full argv. The parent owns routing and report imports.
func runAssessment(ctx context.Context, args []string, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) < 2 || args[0] != "assessment" || args[1] != "build" {
		return fmt.Errorf("expected assessment build --input RUN_DIR --metrics DIR --recorder-summary FILE --output NEW_JSON")
	}
	f := flag.NewFlagSet("assessment build", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	opts := assessment.Options{}
	f.StringVar(&opts.Input, "input", "", "existing local run directory")
	f.StringVar(&opts.RecorderSummary, "recorder-summary", "", "explicit extracted stream-summary.json; never untar automatically")
	f.StringVar(&opts.RequestEvidence, "request-evidence", "", "reviewer-correlated request evidence JSON; verify referenced source hashes")
	output := f.String("output", "", "new JSON file; parent must exist")
	f.Func("metrics", "exact metrics export root; repeat for multiple exports", func(dir string) error {
		if dir == "" {
			return fmt.Errorf("metrics directory must not be empty")
		}
		opts.Metrics = append(opts.Metrics, dir)
		return nil
	})
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if opts.Input == "" || *output == "" || f.NArg() != 0 {
		return fmt.Errorf("assessment build requires --input RUN_DIR, --output NEW_JSON and no positional arguments")
	}
	result, err := assessment.Build(ctx, opts)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(*output))
	if err != nil {
		return err
	}
	syncErr = parent.Sync()
	closeErr = parent.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		SchemaVersion string            `json:"schemaVersion"`
		Output        string            `json:"output"`
		Status        assessment.Status `json:"status"`
	}{assessment.SchemaVersion, *output, result.Status})
}

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
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/progress"
)

// runProgress accepts the full argv, including "case progress". It is a single
// local snapshot; callers orchestrate polling and metric analysis separately.
func runProgress(ctx context.Context, args []string, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) < 2 || args[0] != "case" || args[1] != "progress" {
		return fmt.Errorf("expected case progress --input RUN_DIR")
	}
	f := flag.NewFlagSet("case progress", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	input := f.String("input", "", "local run-case.sh artifact directory")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if *input == "" || f.NArg() != 0 {
		return fmt.Errorf("case progress requires --input RUN_DIR and no positional arguments")
	}
	result, err := progress.InspectContext(ctx, *input, time.Now())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

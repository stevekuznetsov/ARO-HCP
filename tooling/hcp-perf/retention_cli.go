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
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/retention"
)

// runRetention accepts the full argv, including "environment refresh".
func runRetention(ctx context.Context, args []string, out io.Writer) error {
	return runRetentionWith(ctx, args, out, retention.Refresh)
}

func runRetentionWith(ctx context.Context, args []string, out io.Writer, refresh func(context.Context, retention.Spec, bool) (retention.Result, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) < 2 || args[0] != "environment" || args[1] != "refresh" {
		return fmt.Errorf("expected environment refresh --input FILE [--timeout 5m] [--dry-run]")
	}
	f := flag.NewFlagSet("environment refresh", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	input := f.String("input", "", "explicit resource group IDs JSON")
	timeout := f.Duration("timeout", 5*time.Minute, "overall deadline (maximum 10m)")
	dryRun := f.Bool("dry-run", false, "preflight and plan without changing tags")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if *input == "" || f.NArg() != 0 {
		return fmt.Errorf("environment refresh requires --input FILE and no positional arguments")
	}
	if *timeout <= 0 || *timeout > 10*time.Minute {
		return fmt.Errorf("timeout must be positive and at most 10m")
	}
	var spec retention.Spec
	if err := decodeFile(*input, &spec); err != nil {
		return fmt.Errorf("retention input: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	result, operationErr := refresh(ctx, spec, *dryRun)
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return errors.Join(operationErr, enc.Encode(result))
}

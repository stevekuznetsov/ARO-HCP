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
	"net/http"
	"os"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
)

// runMetricsBackfill accepts full command arguments; main owns routing.
func runMetricsBackfill(ctx context.Context, args []string, out io.Writer) error {
	return runMetricsBackfillWith(ctx, args, out, metrics.Backfill)
}

func runMetricsBackfillWith(ctx context.Context, args []string, out io.Writer, backfill func(context.Context, *http.Client, string, []metrics.Query, metrics.BackfillOptions, string) (metrics.BackfillResult, error)) error {
	if len(args) < 2 || args[0] != "metrics" || args[1] != "backfill" {
		return fmt.Errorf("expected metrics backfill")
	}
	f := flag.NewFlagSet("metrics backfill", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	endpoint := f.String("endpoint", "", "Prometheus server URL")
	queriesPath := f.String("queries", "", "query list JSON")
	output := f.String("output", "", "new private local directory; parent must exist")
	startText := f.String("start", "", "RFC3339 inclusive start, anchors evaluation grid")
	endText := f.String("end", "", "RFC3339 inclusive end, never rounded up")
	var options metrics.BackfillOptions
	f.DurationVar(&options.Step, "step", 5*time.Second, "query evaluation step, at least 1s")
	f.DurationVar(&options.Chunk, "chunk", time.Minute, "window size, step multiple <=5m; at most 1440 windows")
	timeout := f.Duration("timeout", 30*time.Minute, "whole backfill timeout, positive and at most 24h")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *endpoint == "" || *queriesPath == "" || *output == "" || *startText == "" || *endText == "" {
		return fmt.Errorf("metrics backfill requires --endpoint, --queries, --output, --start, --end and no positional arguments")
	}
	if *timeout <= 0 || *timeout > 24*time.Hour {
		return fmt.Errorf("timeout must be positive and at most 24h")
	}
	var err error
	options.Start, err = time.Parse(time.RFC3339Nano, *startText)
	if err != nil {
		return fmt.Errorf("start must be RFC3339")
	}
	options.End, err = time.Parse(time.RFC3339Nano, *endText)
	if err != nil {
		return fmt.Errorf("end must be RFC3339")
	}
	var queries []metrics.Query
	if err := decodeFile(*queriesPath, &queries); err != nil {
		return fmt.Errorf("queries: %w", err)
	}
	client := &http.Client{Transport: bearerTransport{token: os.Getenv("HCP_PERF_BEARER_TOKEN")}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	result, err := backfill(runCtx, client, *endpoint, queries, options, *output)
	return errors.Join(err, json.NewEncoder(out).Encode(result))
}

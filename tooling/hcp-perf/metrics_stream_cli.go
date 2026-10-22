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

// runMetricsStream accepts the full command arguments; main owns routing.
// This blocks locally and emits only a final JSON result. For durable operation,
// run under systemd with a persistent local --output NEW_DIR, e.g. --duration 45m
// for the remaining experiment. Do not restart against the same output directory.
func runMetricsStream(ctx context.Context, args []string, out io.Writer) error {
	return runMetricsStreamWith(ctx, args, out, metrics.Stream)
}

func runMetricsStreamWith(ctx context.Context, args []string, out io.Writer, runStream func(context.Context, *http.Client, string, []metrics.Query, metrics.StreamOptions, string) (metrics.StreamResult, error)) error {
	if len(args) < 2 || args[0] != "metrics" || args[1] != "stream" {
		return fmt.Errorf("expected metrics stream")
	}
	f := flag.NewFlagSet("metrics stream", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	endpoint := f.String("endpoint", "", "Prometheus server URL")
	queriesPath := f.String("queries", "", "query list JSON")
	output := f.String("output", "", "new private local directory; parent must exist")
	var options metrics.StreamOptions
	f.DurationVar(&options.Duration, "duration", 6*time.Hour, "capture duration, at least chunk and at most 24h; starts now")
	f.DurationVar(&options.Chunk, "chunk", 30*time.Second, "completed window size, step multiple and at most 5m")
	f.DurationVar(&options.Step, "step", 5*time.Second, "query evaluation step, at least 1s")
	f.DurationVar(&options.Lag, "lag", 15*time.Second, "wait after each window for scraping, 0..5m")
	f.DurationVar(&options.Drain, "drain", 90*time.Second, "maximum catch-up after duration+lag, positive and at most 5m")
	f.DurationVar(&options.ChunkTimeout, "chunk-timeout", 90*time.Second, "whole query batch timeout, positive and at most 90s")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *endpoint == "" || *queriesPath == "" || *output == "" {
		return fmt.Errorf("metrics stream requires --endpoint, --queries, --output and no positional arguments")
	}
	var queries []metrics.Query
	if err := decodeFile(*queriesPath, &queries); err != nil {
		return fmt.Errorf("queries: %w", err)
	}
	client := &http.Client{Transport: bearerTransport{token: os.Getenv("HCP_PERF_BEARER_TOKEN")}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	result, err := runStream(ctx, client, *endpoint, queries, options, *output)
	return errors.Join(err, json.NewEncoder(out).Encode(result))
}

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
	"io"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/record"
)

// runRecordHealth reads local evidence, including an active worker's mounted PVC
// via exec or an extracted archive. It needs no Kubernetes client or credentials.
func runRecordHealth(ctx context.Context, args []string, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) < 2 || args[0] != "record" || args[1] != "health" {
		return errors.New("expected record health --input DIR")
	}
	f := flag.NewFlagSet("record health", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	input := f.String("input", "", "local recording directory (for example /data/observations)")
	requireReady := f.Bool("require-ready", false, "require a running checkpoint with successful baseline polls for every namespace/resource; not watch/log readiness")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if *input == "" || f.NArg() != 0 {
		return errors.New("record health requires --input DIR and no positional arguments")
	}
	result, err := record.Health(*input)
	if err != nil {
		return err
	}
	if *requireReady {
		checkpoint := result.Checkpoint
		if checkpoint == nil || checkpoint.Type != "poll" || checkpoint.Result.Shutdown != "running" ||
			checkpoint.Result.Polls < 1 || checkpoint.Result.APIErrors != 0 || checkpoint.Result.IncompleteStreams != 0 ||
			!checkpoint.Result.StartedAt.Equal(result.Manifest.StartedAt) || !checkpoint.Result.EndedAt.IsZero() ||
			checkpoint.PollStartedAt.Before(result.Manifest.StartedAt) || checkpoint.PollFinishedAt.Before(checkpoint.PollStartedAt) ||
			checkpoint.PollFinishedAt.IsZero() || result.Final != nil || result.StreamSummary != nil {
			return errors.New("recorder not ready: a completed running poll without API errors is required")
		}
		// Poll success validates baseline sources only. Watch/log summaries are
		// final-only and cannot establish live stream initialization here.
		seen := map[string]bool{}
		for _, stream := range checkpoint.Result.Streams {
			key := stream.Namespace + "/" + stream.Resource
			if seen[key] || stream.Samples < 1 || stream.APIErrors != 0 || stream.LastSuccess == nil ||
				stream.LastSuccess.Before(checkpoint.PollStartedAt) || stream.LastSuccess.After(checkpoint.PollFinishedAt) {
				return errors.New("recorder not ready: each baseline source must have a complete successful snapshot")
			}
			seen[key] = true
		}
		for _, namespace := range result.Manifest.Spec.Namespaces {
			for _, resource := range []string{"pods", "events", "deployments", "statefulsets"} {
				if !seen[namespace+"/"+resource] {
					return errors.New("recorder not ready: required baseline source is missing")
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

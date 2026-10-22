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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

// Export reads only declared hash-verified local sources. It never discovers
// files, follows references embedded in artifacts, or contacts a cluster.
func Export(ctx context.Context, spec ExportSpec) (Dataset, error) {
	if spec.SchemaVersion != ExportSchemaVersion || spec.Dataset.SchemaVersion != SchemaVersion {
		return Dataset{}, errors.New("unsupported export or dataset schema version")
	}
	if len(spec.Runs) == 0 || len(spec.Runs) > 1000 {
		return Dataset{}, errors.New("require 1-1000 explicitly reviewed runs")
	}
	root, err := os.OpenRoot(spec.BaseDirectory)
	if err != nil {
		return Dataset{}, errors.New("cannot open private source base directory")
	}
	defer root.Close()
	reader := &sourceReader{ctx: ctx, root: root, sources: map[string]Source{}}
	// Clone public metadata so the caller's contract cannot be changed in place.
	b, err := json.Marshal(spec.Dataset)
	if err != nil {
		return Dataset{}, err
	}
	var dataset Dataset
	if err := json.Unmarshal(b, &dataset); err != nil {
		return Dataset{}, err
	}
	dataset.Sources = nil
	dataset.Method = Method{
		Quantiles: "Nearest-rank empirical quantiles over the entire covered cohort; no pooling across runs, recovery, warmup or baseline.",
		Whiskers:  "Observed extrema within 1.5 IQR of Q1/Q3; outlier count and full maximum retained.",
		CPU:       "Raw counter deltas over paired advancing source timestamps, excluding resets, nonpositive timestamps and source gaps over 45 seconds; integrated on the saved export grid. Sum complete component/container intervals before quantiles.",
		Memory:    "Cgroup charge in MiB, independently fresh source timestamps within 45 seconds; RSS/working set without their own timestamp companions are unavailable, not zero.",
		Scope:     "Actual API offer window only for resources; successful API bodies include final drain. All declared HCP workloads except kube-state-metrics. Missing identities or replicas never become healthy zeroes.",
	}
	indices := map[string]int{}
	for i := range dataset.Sizes {
		s := &dataset.Sizes[i]
		if _, exists := indices[s.ID]; exists || s.ID == "" {
			return Dataset{}, errors.New("duplicate or empty size ID")
		}
		indices[s.ID] = i
		s.Runs, s.Components = nil, nil
	}
	seen := map[string]bool{}
	for _, input := range spec.Runs {
		i, exists := indices[input.SizeID]
		if !exists || input.RunID == "" || seen[input.SizeID+"/"+input.RunID] {
			return Dataset{}, errors.New("unknown size or duplicate/empty run ID")
		}
		seen[input.SizeID+"/"+input.RunID] = true
		run, components, err := exportRun(reader, input)
		if err != nil {
			return Dataset{}, fmt.Errorf("run %s: %w", input.RunID, err)
		}
		size := &dataset.Sizes[i]
		size.Runs = append(size.Runs, run)
		for _, component := range components {
			found := false
			for _, existing := range size.Components {
				if existing.ID == component.ID {
					found = true
					break
				}
			}
			if !found {
				size.Components = append(size.Components, component)
			}
		}
	}
	for _, source := range reader.sources {
		dataset.Sources = append(dataset.Sources, source)
	}
	sort.Slice(dataset.Sources, func(i, j int) bool { return dataset.Sources[i].ID < dataset.Sources[j].ID })
	for i := range dataset.Sizes {
		s := &dataset.Sizes[i]
		if !seen[s.ID+"/"+s.DefaultRun] {
			return Dataset{}, errors.New("default run absent from reviewed manifest")
		}
		sort.Slice(s.Components, func(i, j int) bool { return s.Components[i].ID < s.Components[j].ID })
	}
	return dataset, ctx.Err()
}

type savedResults struct {
	API      *apiload.Result `json:"api"`
	Turnover *struct {
		Completed int `json:"completed"`
		Scheduled int `json:"scheduled"`
	} `json:"turnover"`
}

type savedOutcome struct {
	State           string `json:"state"`
	ExecutionStatus string `json:"executionStatus"`
	Stability       *struct {
		Restarts int `json:"restarts"`
		OOMs     int `json:"ooms"`
	} `json:"stability"`
}

func exportRun(reader *sourceReader, in RunInput) (Run, []Component, error) {
	run := in.RunMeta
	run.ID = in.RunID
	run.Sources, run.Latencies, run.Resources, run.WholeHCP = nil, nil, nil, nil
	run.CanaryCycles, run.CanarySoftDegraded, run.CanaryHardFailed = 0, 0, nil
	run.Requests.MaxDegradedFraction = nil
	run.RolloutsCompleted, run.RolloutsScheduled = 0, 0
	run.Restarts, run.OOMs = nil, nil
	refs := in.Sources
	var frozenSpec struct {
		API                  *apiload.Spec         `json:"api"`
		StabilityPolicy      string                `json:"stabilityPolicy"`
		RequestQualityPolicy *requestQualityPolicy `json:"requestQualityPolicy"`
	}
	readJSON := func(ref InputRef, value any) error {
		b, err := reader.read(in.Directory, ref)
		if err != nil {
			return err
		}
		run.Sources = append(run.Sources, ref.ID)
		if err := json.Unmarshal(b, value); err != nil {
			return fmt.Errorf("source %s: malformed JSON", ref.ID)
		}
		return nil
	}
	if err := readJSON(refs.Spec, &frozenSpec); err != nil {
		return Run{}, nil, err
	}
	var result savedResults
	if err := readJSON(refs.Results, &result); err != nil {
		return Run{}, nil, err
	}
	var outcome savedOutcome
	if err := readJSON(refs.Outcome, &outcome); err != nil {
		return Run{}, nil, err
	}
	if result.API == nil || frozenSpec.API == nil {
		return Run{}, nil, errors.New("missing API result or spec")
	}
	api := result.API
	if api.Started.IsZero() || !api.SchedulingStopped.After(api.Started) || api.Finished.Before(api.SchedulingStopped) {
		return Run{}, nil, errors.New("invalid actual API window")
	}
	if api.Spec.RatePerSecond != frozenSpec.API.RatePerSecond || api.Spec.DurationSeconds != frozenSpec.API.DurationSeconds {
		return Run{}, nil, errors.New("API result/spec mismatch")
	}
	run.Start, run.End = api.Started.UTC().Format(time.RFC3339Nano), api.SchedulingStopped.UTC().Format(time.RFC3339Nano)
	run.PlannedSeconds, run.ActualSeconds = float64(api.Spec.DurationSeconds), api.SchedulingStopped.Sub(api.Started).Seconds()
	run.OfferedQPS = float64(api.Spec.RatePerSecond)
	run.StabilityPolicy = frozenSpec.StabilityPolicy
	if run.StabilityPolicy == "" {
		run.StabilityPolicy = "strict"
	}
	if outcome.ExecutionStatus != "" {
		run.Assessment.Execution = outcome.ExecutionStatus
	}
	if result.Turnover != nil {
		run.RolloutsCompleted, run.RolloutsScheduled = result.Turnover.Completed, result.Turnover.Scheduled
	}
	if outcome.Stability != nil {
		run.Restarts, run.OOMs = &outcome.Stability.Restarts, &outcome.Stability.OOMs
	}
	requests, err := reader.read(in.Directory, refs.Requests)
	if err != nil {
		return Run{}, nil, err
	}
	run.Sources = append(run.Sources, refs.Requests.ID)
	if err := exportRequests(&run, api, requests); err != nil {
		return Run{}, nil, err
	}
	if err := exportRequestPolicy(&run, frozenSpec.RequestQualityPolicy); err != nil {
		return Run{}, nil, err
	}
	events, err := reader.read(in.Directory, refs.Events)
	if err != nil {
		return Run{}, nil, err
	}
	run.Sources = append(run.Sources, refs.Events.ID)
	if err := exportCanaries(&run, events); err != nil {
		return Run{}, nil, err
	}
	components, err := exportResources(reader, in, &run, api.Started, api.SchedulingStopped)
	if err != nil {
		return Run{}, nil, err
	}
	for _, ref := range refs.Evidence {
		if _, err := reader.read(in.Directory, ref); err != nil {
			return Run{}, nil, err
		}
		run.Sources = append(run.Sources, ref.ID)
	}
	run.Notes = append(run.Notes, "Successful API latency uses the whole offered logical cohort, including bodies finishing during drain; resource windows exclude warmup and recovery.", "Restart/OOM counters are frozen outcome observations, not sums of cumulative recorder histories; capture is not lossless.")
	return run, components, nil
}

func exportRequests(run *Run, api *apiload.Result, raw []byte) error {
	counts := apiload.Counts{}
	samples := map[string][]float64{"api-all": nil}
	labels := map[string]string{"api-all": "All successful API reads"}
	seen := map[int]bool{}
	err := eachJSON(raw, func(raw json.RawMessage) error {
		var event apiload.Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return errors.New("invalid API request event")
		}
		if event.Target < 0 || event.Target >= len(api.Spec.Targets) || seen[event.Sequence] || event.Latency < 0 {
			return errors.New("invalid target, latency or duplicate request sequence")
		}
		seen[event.Sequence] = true
		counts.Completed++
		counts.HTTPAttempts += event.HTTPAttempts
		if event.StatusCode == 429 || event.ErrClass == "timeout" {
			counts.DegradedRequests++
		}
		success := event.StatusCode >= 200 && event.StatusCode < 300 && event.ErrClass == ""
		switch {
		case event.StatusCode == 429:
			counts.Rejected429++
		case event.StatusCode != 0 && (event.StatusCode < 200 || event.StatusCode >= 300):
			counts.OtherHTTP++
		case success:
			counts.Successful++
		}
		switch event.ErrClass {
		case "":
		case "timeout":
			counts.Timeouts++
		case "canceled":
			counts.Canceled++
		case "transport":
			counts.TransportErrors++
		case "bodyLimit":
			counts.BodyLimitErrors++
		default:
			return errors.New("unknown request error class")
		}
		target := api.Spec.Targets[event.Target]
		id := "api-" + target.Verb + "-" + target.Resource
		labels[id] = strings.ToUpper(target.Verb) + " " + target.Resource
		if success {
			seconds := event.Latency.Seconds()
			samples[id] = append(samples[id], seconds)
			samples["api-all"] = append(samples["api-all"], seconds)
		}
		return nil
	})
	if err != nil {
		return err
	}
	want := api.Counts
	counts.Offered, counts.Dispatched, counts.GeneratorLimited = want.Offered, want.Dispatched, want.GeneratorLimited
	// Older saved results predate union accounting; reconstruct it from raw events
	// but still compare every count that the original format actually stored.
	if api.RequestAccountingVersion == "" {
		want.DegradedRequests = counts.DegradedRequests
	}
	if counts != want || counts.Completed != counts.Dispatched {
		return errors.New("raw request counts disagree with saved API results")
	}
	run.Requests = Requests{Offered: counts.Offered, Dispatched: counts.Dispatched, Successful: counts.Successful, Rejected429: counts.Rejected429, Timeouts: counts.Timeouts, DegradedUnion: counts.DegradedRequests, Canceled: counts.Canceled, OtherErrors: counts.OtherHTTP + counts.TransportErrors + counts.BodyLimitErrors, GeneratorLimited: counts.GeneratorLimited, SuccessfulQPS: float64(counts.Successful) / api.Finished.Sub(api.Started).Seconds(), ThroughputBasis: "Successful response bodies / actual start-to-final-body time, including request drain"}
	ids := make([]string, 0, len(labels))
	for id := range labels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		run.Latencies = append(run.Latencies, MetricDistribution{ID: id, Label: labels[id], Unit: "seconds", Distribution: distribution(samples[id], len(samples[id])), Note: "2xx responses with no body/transport error only; excludes rejected, canceled and failed requests."})
	}
	return nil
}

type requestQualityPolicy struct {
	MaxDegradedBasisPoints int `json:"maxDegradedBasisPoints"`
}

func exportRequestPolicy(run *Run, policy *requestQualityPolicy) error {
	run.Requests.MaxDegradedFraction = nil
	if policy == nil {
		run.Notes = append(run.Notes, "Legacy strict request policy: HTTP 429 shedding was permitted but any timeout failed requirements; no 429-or-timeout union percentage threshold was declared.")
		return nil
	}
	if policy.MaxDegradedBasisPoints < 0 || policy.MaxDegradedBasisPoints > 10000 {
		return errors.New("invalid request-quality basis-point threshold")
	}
	fraction := float64(policy.MaxDegradedBasisPoints) / 10000
	run.Requests.MaxDegradedFraction = &fraction
	return nil
}

func exportCanaries(run *Run, raw []byte) error {
	run.CanaryCycles, run.CanarySoftDegraded, run.CanaryHardFailed = 0, 0, nil
	names := []string{"ack", "ready", "cleanup"}
	values := make([][]float64, 3)
	censored := make([]int, 3)
	failed, known := 0, true
	err := eachJSON(raw, func(raw json.RawMessage) error {
		var event struct {
			Kind  string          `json:"kind"`
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return err
		}
		if event.Kind != "measured-canary" {
			return nil
		}
		run.CanaryCycles++
		if len(event.Value) == 0 {
			event.Value = json.RawMessage("null")
		}
		var stages map[string]json.RawMessage
		if err := json.Unmarshal(event.Value, &stages); err != nil {
			return errors.New("invalid measured canary")
		}
		// Missing stage data is not a zero-valued failure or a proven success.
		// The producer's cumulative outcome counter is policy-dependent and also
		// includes recovery probes, so only explicit measured results count here.
		completeEvidence, hardFailure := true, false
		for _, name := range []string{"ack", "ready", "cleanup", "preflight"} {
			rawStage, present := stages[name]
			if name == "preflight" && !present {
				continue
			}
			var stage struct {
				Completed *time.Time `json:"completed"`
			}
			if err := json.Unmarshal(rawStage, &stage); present && err != nil {
				return errors.New("invalid measured canary stage")
			}
			if stage.Completed == nil {
				completeEvidence = false
				continue
			}
			hardFailure = hardFailure || stage.Completed.IsZero()
		}
		if !completeEvidence {
			known = false
		}
		if hardFailure {
			failed++
		}
		var value workload.Result
		if err := json.Unmarshal(event.Value, &value); err != nil {
			return errors.New("invalid measured canary")
		}
		if value.Degraded {
			run.CanarySoftDegraded++
		}
		for i, stage := range []workload.Stage{value.Ack, value.Ready, value.Cleanup} {
			if stage.Completed.IsZero() {
				censored[i]++
				continue
			}
			if stage.Duration < 0 || stage.Started.IsZero() || stage.Completed.Before(stage.Started) {
				return errors.New("invalid completed canary stage")
			}
			values[i] = append(values[i], stage.Duration.Seconds())
		}
		return nil
	})
	if err != nil {
		return err
	}
	if run.CanaryCycles > 0 && known {
		run.CanaryHardFailed = &failed
	}
	if run.CanaryHardFailed == nil {
		run.Notes = append(run.Notes, "Measured canary hard-failure count is unknown: no measured results or incomplete explicit stage-completion evidence. Outcome counters are not substituted.")
	} else {
		run.Notes = append(run.Notes, "Canary hard failures count measured result cycles with any explicitly incomplete stage once per cycle; baseline, warmup and recovery probes are excluded.")
	}
	for i, name := range names {
		run.Latencies = append(run.Latencies, MetricDistribution{ID: "canary-" + name, Label: "Canary " + name, Unit: "seconds", Distribution: distribution(values[i], run.CanaryCycles), Censored: censored[i], Note: "Measured cycles only; incomplete stages are censored, never zero latency. Ready includes ACK; cleanup is independent. Recovery and baseline excluded."})
	}
	return nil
}

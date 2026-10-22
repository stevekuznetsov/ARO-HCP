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

// Package progress takes a bounded, local, read-only snapshot of run-case.sh
// artifacts. It neither checks process liveness nor qualifies an experiment.
package progress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxFileBytes = 1 << 20

type Result struct {
	Version                      string      `json:"version"`
	Mode                         string      `json:"mode"`
	CanaryCadenceSeconds         int64       `json:"canaryCadenceSeconds"`
	ObservedAt                   time.Time   `json:"observedAt"`
	Phase                        string      `json:"phase"`
	ActivityPhase                string      `json:"activityPhase"`
	Provisional                  bool        `json:"provisional"`
	Qualified                    bool        `json:"qualified"`
	Applications                 *int64      `json:"applications,omitempty"`
	WindowStart                  *time.Time  `json:"windowStart,omitempty"`
	WindowEnd                    *time.Time  `json:"windowEnd,omitempty"`
	Steady                       Window      `json:"steady"`
	Baseline                     Canaries    `json:"baseline"`
	SteadyCanaries               Canaries    `json:"steadyCanaries"`
	Deadlines                    *Durations  `json:"deadlinesNanoseconds,omitempty"`
	ExpectedDurationsNanoseconds *Durations  `json:"expectedDurationsNanoseconds,omitempty"`
	HardDeadlinesNanoseconds     *Durations  `json:"hardDeadlinesNanoseconds,omitempty"`
	CanaryPolicy                 string      `json:"canaryPolicy,omitempty"`
	Commands                     []Command   `json:"commands"`
	Turnover                     *Turnover   `json:"turnover,omitempty"`
	Exports                      []Export    `json:"exports"`
	Outcome                      *Outcome    `json:"outcome,omitempty"`
	Conditions                   []Condition `json:"conditions"`
	LastActivityAt               *time.Time  `json:"lastActivityAt,omitempty"`
	LastActivitySource           string      `json:"lastActivitySource,omitempty"`
	StaleAgeSeconds              *float64    `json:"staleAgeSeconds,omitempty"`
	Warnings                     []Warning   `json:"warnings"`
}

type Window struct {
	StartedAt        *time.Time `json:"startedAt,omitempty"`
	Deadline         *time.Time `json:"deadline,omitempty"`
	DurationSeconds  *int64     `json:"durationSeconds,omitempty"`
	RecoverySeconds  *int64     `json:"recoverySeconds,omitempty"`
	ElapsedSeconds   *float64   `json:"elapsedSeconds,omitempty"`
	RemainingSeconds *float64   `json:"remainingSeconds,omitempty"`
	OverdueSeconds   *float64   `json:"overdueSeconds,omitempty"`
}

type Command struct {
	Name      string     `json:"name"`
	State     string     `json:"state"` // inProgress, pending, completed, failed, expectedFailure
	StartedAt *time.Time `json:"startedAt,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
}

type Stage struct {
	Started             time.Time `json:"started"`
	Completed           time.Time `json:"completed"`
	Duration            int64     `json:"durationNanoseconds"`
	Expected            *int64    `json:"expectedDurationNanoseconds,omitempty"`
	Deadline            *int64    `json:"deadlineNanoseconds,omitempty"`
	ExpectationExceeded bool      `json:"expectationExceeded,omitempty"`
}

type Canary struct {
	Command
	MeasurementMode string            `json:"measurementMode,omitempty"` // Empty for historical artifacts; never inferred.
	Watch           *WatchDiagnostics `json:"watch,omitempty"`
	Preflight       Stage             `json:"preflight"`
	Ack             Stage             `json:"ack"`
	Ready           Stage             `json:"ready"`
	Cleanup         Stage             `json:"cleanup"`
	FailureStage    string            `json:"failureStage,omitempty"`
	Performance     string            `json:"performance,omitempty"`
}

// WatchDiagnostics mirrors workload diagnostics without importing its dependencies.
type WatchDiagnostics struct {
	Events     int      `json:"events"`
	Reconnects int      `json:"reconnects"`
	Gaps       int      `json:"gaps"`
	Errors     []string `json:"errors,omitempty"`
}

type Canaries struct {
	Expected  *int         `json:"expected,omitempty"`
	Passed    int          `json:"passed"`
	Pending   int          `json:"pending"`
	Failed    int          `json:"failed"`
	Degraded  int          `json:"degraded"`
	Evaluated int          `json:"evaluated"`
	Skipped   int          `json:"skipped"`
	Remaining *int         `json:"remaining,omitempty"` // Not yet attempted; first cycle is scheduled at t=0.
	Latest    *Canary      `json:"latest,omitempty"`
	Active    *Canary      `json:"active,omitempty"`
	Samples   []Canary     `json:"samples"`
	Ack       Distribution `json:"ackSeconds"`
	Ready     Distribution `json:"readySeconds"`
	Cleanup   Distribution `json:"cleanupSeconds"`
}

type Distribution struct {
	Count  int     `json:"count"`
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	P95    float64 `json:"p95"`
	Max    float64 `json:"max"`
}

type Durations struct {
	Ack     int64 `json:"ack"`
	Ready   int64 `json:"ready"`
	Cleanup int64 `json:"cleanup"`
}

type Turnover struct {
	StartedAt    time.Time `json:"startedAt"`
	AdmissionEnd time.Time `json:"admissionEnd"`
	FinishedAt   time.Time `json:"finishedAt"`
	Scheduled    int       `json:"scheduled"`
	Completed    int       `json:"completed"`
	Failed       int       `json:"failed"`
	NotStarted   int       `json:"notStarted"`
	Backlog      int       `json:"backlog"`
	Outstanding  int       `json:"outstanding"`
}

type Export struct {
	Command
	HelperCleanupFailed *bool `json:"helperCleanupFailed,omitempty"`
}

type Outcome struct {
	ExitCode               *int  `json:"exitCode"`
	ShutdownOrExportFailed *bool `json:"shutdownOrExportFailed"`
	CapturesLeftRunning    *bool `json:"capturesLeftRunning,omitempty"`
}

type Condition struct {
	Type   string `json:"type"`
	Status string `json:"status"` // True, False, Unknown
}

type Warning struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// Inspect does not retry evolving artifacts. Missing evidence is not success.
func Inspect(dir string, now time.Time) (Result, error) {
	return InspectContext(context.Background(), dir, now)
}

// InspectContext also checks cancellation between local filesystem operations.
func InspectContext(ctx context.Context, dir string, now time.Time) (Result, error) {
	r := Result{Version: "v1", Mode: "unknown", CanaryCadenceSeconds: 300, ObservedAt: now, Phase: "incomplete", ActivityPhase: "incomplete", Provisional: true,
		Commands: []Command{}, Exports: []Export{}, Conditions: []Condition{}, Warnings: []Warning{}}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return r, fmt.Errorf("open run directory: unavailable or not a directory")
	}
	defer root.Close()
	files := map[string]os.FileInfo{}
	warn := func(source, reason string) { r.Warnings = append(r.Warnings, Warning{source, reason}) }
	// Do not recurse: private/, metric payloads, recordings and event streams are
	// never opened. Metadata from the event file can still show writer activity.
	for _, sub := range []string{".", "inputs", "commands", "evidence", "evidence/turnover"} {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if sub != "." {
			info, err := root.Lstat(sub)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || !info.IsDir() {
				warn(sub, "directory unavailable or not a real directory")
				continue
			}
		}
		d, err := root.Open(sub)
		if err != nil {
			warn(sub, "cannot list directory")
			continue
		}
		entries, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			warn(sub, "cannot list directory")
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return r, err
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			name := filepath.Join(sub, entry.Name())
			files[name] = info
			if r.LastActivityAt == nil || info.ModTime().After(*r.LastActivityAt) {
				at := info.ModTime()
				r.LastActivityAt = &at
				r.LastActivitySource = sub + " (file mtime)"
				if (sub == "commands" && commandPhase(strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))) != "") ||
					name == "evidence/steady-start.txt" || name == "evidence/window-start.txt" || name == "evidence/window-end.txt" || name == "evidence/outcome.json" ||
					name == "evidence/turnover/events.ndjson" || name == "evidence/turnover/result.json" || name == "evidence/turnover/latest.json" {
					r.LastActivitySource = name
				}
			}
		}
	}
	read := func(name string) []byte {
		if ctx.Err() != nil {
			return nil
		}
		info, exists := files[name]
		if !exists {
			return nil
		}
		if info.Size() > maxFileBytes {
			warn(name, "exceeds 1 MiB read limit")
			return nil
		}
		f, err := root.Open(name)
		if err != nil {
			warn(name, "file unavailable during inspection")
			return nil
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
		if err != nil || len(b) > maxFileBytes {
			warn(name, "cannot read within 1 MiB limit")
			return nil
		}
		return b
	}
	decode := func(name string, value any) bool {
		b := read(name)
		if b == nil {
			return false
		}
		// Never echo parser errors: type and timestamp errors can include bodies.
		if strings.TrimSpace(string(b)) == "null" || json.Unmarshal(b, value) != nil {
			warn(name, "invalid or partially written JSON; evidence pending")
			return false
		}
		return true
	}
	timestamp := func(name string) *time.Time {
		b := read(name)
		if b == nil {
			return nil
		}
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
		if err != nil || at.IsZero() {
			warn(name, "invalid or partially written timestamp")
			return nil
		}
		return &at
	}
	r.WindowStart = timestamp("evidence/window-start.txt")
	r.WindowEnd = timestamp("evidence/window-end.txt")
	r.Steady.StartedAt = timestamp("evidence/steady-start.txt")
	var spec struct {
		Applications *int64 `json:"applications"`
		Duration     *int64 `json:"durationSeconds"`
		Recovery     *int64 `json:"recoverySeconds"`
	}
	if decode("inputs/turnover.json", &spec) {
		r.Applications = spec.Applications
		r.Steady.DurationSeconds, r.Steady.RecoverySeconds = spec.Duration, spec.Recovery
	}
	// The next harness copies session metadata into run.json. Older runs have
	// neither mode nor cadence; their persisted turnover duration still applies.
	var recordedMode string
	var recordedCadence *int64
	for _, name := range []string{"evidence/run.json", "inputs/session.json"} {
		var metadata struct {
			Mode          string     `json:"mode"`
			Cadence       *int64     `json:"canaryCadenceSeconds"`
			Steady        *int64     `json:"steadySeconds"`
			Recovery      *int64     `json:"recoverySeconds"`
			Applications  *int64     `json:"applications"`
			CanaryPolicy  string     `json:"canaryPolicy"`
			HardDeadlines *Durations `json:"hardDeadlineSeconds"`
		}
		if !decode(name, &metadata) {
			continue
		}
		if recordedMode == "" {
			recordedMode = metadata.Mode
		}
		if metadata.CanaryPolicy == "observe-degradation" {
			r.CanaryPolicy = metadata.CanaryPolicy
		}
		if metadata.HardDeadlines != nil && r.HardDeadlinesNanoseconds == nil {
			d := *metadata.HardDeadlines
			valid := true
			for _, n := range []*int64{&d.Ack, &d.Ready, &d.Cleanup} {
				if *n <= 0 || *n > math.MaxInt64/int64(time.Second) {
					valid = false
					break
				}
				*n *= int64(time.Second)
			}
			if valid {
				r.HardDeadlinesNanoseconds = &d
			} else {
				warn(name, "invalid hardDeadlineSeconds; positive seconds within nanosecond range required")
			}
		}
		if recordedCadence == nil {
			recordedCadence = metadata.Cadence
		}
		if r.Steady.DurationSeconds == nil {
			r.Steady.DurationSeconds = metadata.Steady
		}
		if r.Steady.RecoverySeconds == nil {
			r.Steady.RecoverySeconds = metadata.Recovery
		}
		if r.Applications == nil {
			r.Applications = metadata.Applications
		}
	}
	if recordedCadence != nil {
		if *recordedCadence > 0 {
			r.CanaryCadenceSeconds = *recordedCadence
		} else {
			warn("canaryCadenceSeconds", "invalid recorded cadence; using legacy 300-second cadence")
		}
	}
	if r.Applications == nil {
		var seed struct {
			Applications *int64 `json:"applications"`
		}
		if decode("inputs/seed.json", &seed) {
			r.Applications = seed.Applications
		}
	}
	type turnoverDocument struct {
		Turnover
		Spec struct {
			Duration     *int64 `json:"durationSeconds"`
			Recovery     *int64 `json:"recoverySeconds"`
			Applications *int64 `json:"applications"`
		} `json:"spec"`
	}
	for _, name := range []string{"commands/turnover.json", "evidence/turnover/result.json", "evidence/turnover/latest.json"} {
		var turnover turnoverDocument
		if decode(name, &turnover) {
			t := turnover.Turnover
			r.Turnover = &t
			if r.Steady.DurationSeconds == nil {
				r.Steady.DurationSeconds = turnover.Spec.Duration
			}
			if r.Steady.RecoverySeconds == nil {
				r.Steady.RecoverySeconds = turnover.Spec.Recovery
			}
			if r.Applications == nil {
				r.Applications = turnover.Spec.Applications
			}
			break
		}
	}
	if d := r.Steady.DurationSeconds; d != nil && (*d <= 0 || *d > math.MaxInt64/int64(time.Second)) {
		warn("inputs/turnover.json", "invalid steady duration; timing goal unknown")
		r.Steady.DurationSeconds = nil
	}
	switch recordedMode {
	case "validation", "soak":
		r.Mode = recordedMode
	case "":
		if r.Steady.DurationSeconds != nil && *r.Steady.DurationSeconds == 7200 {
			r.Mode = "soak"
		}
	default:
		warn("mode", "unrecognized recorded mode; mode unknown")
	}
	var budgets Durations
	if decode("commands/baseline-budgets.json", &budgets) {
		if budgets.Ack > 0 && budgets.Ready > 0 && budgets.Cleanup > 0 {
			r.Deadlines = &budgets
		} else {
			warn("commands/baseline-budgets.json", "positive numeric nanosecond budgets required")
		}
	}
	var expectations Durations
	if decode("commands/baseline-expectations.json", &expectations) {
		if expectations.Ack > 0 && expectations.Ready > 0 && expectations.Cleanup > 0 {
			r.ExpectedDurationsNanoseconds = &expectations
		} else {
			warn("commands/baseline-expectations.json", "positive numeric nanosecond expectations required")
		}
	}
	phases := []string{"incomplete", "preflight", "seed", "baseline", "calibration", "settle", "steady", "recovery", "finalizing"}
	rank := 0
	advance := func(phase string) {
		for i, p := range phases {
			if p == phase && i > rank {
				rank = i
			}
		}
	}
	commands := map[string]Command{}
	for path := range files {
		if filepath.Dir(path) != "commands" || (filepath.Ext(path) != ".started" && filepath.Ext(path) != ".exit") {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if commandPhase(name) != "" {
			commands[name] = Command{Name: name}
		}
	}
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	failed := false
	for _, name := range names {
		c := commands[name]
		c.StartedAt = timestamp("commands/" + name + ".started")
		c.State = "pending"
		_, started := files["commands/"+name+".started"]
		_, exited := files["commands/"+name+".exit"]
		if started && !exited {
			c.State = "inProgress"
		}
		if b := read("commands/" + name + ".exit"); b != nil {
			code, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil || code < 0 || code > 255 {
				warn("commands/"+name+".exit", "invalid or partially written exit code")
			} else {
				c.ExitCode = &code
				c.State = "completed"
				if code != 0 {
					c.State = "failed"
					if name == "workload-dispatch" {
						c.State = "expectedFailure"
					} else {
						failed = true
					}
				}
			}
		}
		advance(commandPhase(name))
		commands[name] = c
		r.Commands = append(r.Commands, c)
	}
	for _, group := range []struct {
		prefix  string
		summary *Canaries
	}{
		{"canary-baseline-", &r.Baseline}, {"canary-steady-", &r.SteadyCanaries},
	} {
		s := group.summary
		s.Samples = []Canary{}
		var ack, ready, cleanup []float64
		var cycles []int
		for _, name := range names {
			if strings.HasPrefix(name, group.prefix) {
				i, _ := strconv.Atoi(strings.TrimPrefix(name, group.prefix))
				if i > 0 && (group.summary != &r.Baseline || i <= 5) {
					cycles = append(cycles, i)
				}
			}
		}
		sort.Ints(cycles)
		for _, i := range cycles {
			name := group.prefix + strconv.Itoa(i)
			c := commands[name]
			var sample Canary
			// FailureStage is derived locally, never trusted from a JSON body.
			valid := decode("commands/"+name+".json", &sample)
			sample.Command = c
			sample.FailureStage = ""
			sample.Performance = "not_evaluated"
			if valid && sample.Watch != nil && sample.Watch.Gaps > 0 && c.State == "completed" {
				warn("commands/"+name+".json", "zero exit with watch gaps; measurement invalid")
			}
			if c.ExitCode != nil && *c.ExitCode != 0 {
				s.Failed++
				if valid {
					for _, stage := range []struct {
						name  string
						value Stage
					}{
						{"preflight", sample.Preflight}, {"ack", sample.Ack}, {"ready", sample.Ready}, {"cleanup", sample.Cleanup},
					} {
						if stage.value.Completed.IsZero() && (!stage.value.Started.IsZero() || stage.value.Duration > 0) {
							sample.FailureStage = stage.name
							break
						}
					}
				}
			} else if c.ExitCode != nil && *c.ExitCode == 0 && valid && complete(sample.Ack) && complete(sample.Ready) && complete(sample.Cleanup) {
				s.Passed++
				evaluated, degraded := false, false
				for _, stage := range []Stage{sample.Ack, sample.Ready, sample.Cleanup} {
					if stage.Expected == nil || *stage.Expected <= 0 {
						continue
					}
					evaluated = true
					degraded = degraded || stage.Duration > *stage.Expected
				}
				if evaluated || degraded {
					s.Evaluated++
					sample.Performance = "within_expectations"
				}
				if degraded {
					s.Degraded++
					sample.Performance = "degraded"
				}
				ack = append(ack, float64(sample.Ack.Duration)/1e9)
				ready = append(ready, float64(sample.Ready.Duration)/1e9)
				cleanup = append(cleanup, float64(sample.Cleanup.Duration)/1e9)
			} else {
				s.Pending++
				if c.State != "inProgress" {
					sample.State = "pending"
				}
				if c.ExitCode != nil && *c.ExitCode == 0 {
					warn("commands/"+name+".json", "zero exit without complete Ack/Ready/Cleanup; evidence pending")
				}
			}
			s.Samples = append(s.Samples, sample)
			s.Latest = &s.Samples[len(s.Samples)-1]
			if sample.State == "inProgress" {
				active := sample
				s.Active = &active
			}
		}
		s.Ack, s.Ready, s.Cleanup = distribution(ack), distribution(ready), distribution(cleanup)
	}
	if data := read("evidence/canary-cadence.jsonl"); data != nil {
		slots, err := CadenceSkips(data)
		if err != nil {
			warn("evidence/canary-cadence.jsonl", "invalid cadence observations; skipped count unknown")
		} else {
			for _, slot := range slots {
				if _, exists := commands["canary-steady-"+strconv.Itoa(slot)]; exists {
					warn("evidence/canary-cadence.jsonl", "skipped slot also has command evidence")
					continue
				}
				if d := r.Steady.DurationSeconds; d != nil && int64(slot) > (*d-1)/r.CanaryCadenceSeconds+1 {
					warn("evidence/canary-cadence.jsonl", "skipped slot exceeds expected observation window")
					continue
				}
				r.SteadyCanaries.Skipped++
			}
			if r.SteadyCanaries.Skipped > 0 {
				warn("evidence/canary-cadence.jsonl", fmt.Sprintf("%d skipped canary slots: observation gaps, not workload failures", r.SteadyCanaries.Skipped))
			}
		}
	}
	five := 5
	r.Baseline.Expected = &five
	remaining := max(0, five-len(r.Baseline.Samples))
	r.Baseline.Remaining = &remaining
	if d := r.Steady.DurationSeconds; d != nil {
		expected := int((*d-1)/r.CanaryCadenceSeconds + 1)
		r.SteadyCanaries.Expected = &expected
		remaining := max(0, expected-len(r.SteadyCanaries.Samples)-r.SteadyCanaries.Skipped)
		r.SteadyCanaries.Remaining = &remaining
	}
	if r.WindowStart != nil {
		advance("preflight")
	}
	if start := r.Steady.StartedAt; start != nil {
		advance("steady")
		end := now
		if r.WindowEnd != nil && r.WindowEnd.Before(end) {
			end = *r.WindowEnd
		}
		if d := r.Steady.DurationSeconds; d != nil {
			deadline := start.Add(time.Duration(*d) * time.Second)
			r.Steady.Deadline = &deadline
			overdue := max(0, end.Sub(deadline).Seconds())
			r.Steady.OverdueSeconds = &overdue
			if !end.Before(deadline) {
				end = deadline
				advance("recovery")
			}
			remaining := min(float64(*d), max(0, deadline.Sub(end).Seconds()))
			r.Steady.RemainingSeconds = &remaining
		} else {
			warn("inputs/turnover.json", "steady duration unavailable; deadline and expected canary count unknown")
		}
		elapsed := max(0, end.Sub(*start).Seconds())
		r.Steady.ElapsedSeconds = &elapsed
	}
	if r.WindowEnd != nil {
		advance("finalizing")
	}
	for _, name := range []string{"record-export", "metrics-capture", "metrics-nodes", "metrics-pressure", "metrics-optional-capture", "metrics-optional-nodes"} {
		if c, exists := commands[name]; exists {
			e := Export{Command: c}
			if name == "record-export" {
				var body struct {
					HelperCleanupFailed *bool `json:"helperCleanupFailed"`
				}
				if decode("commands/record-export.json", &body) {
					e.HelperCleanupFailed = body.HelperCleanupFailed
				}
			}
			r.Exports = append(r.Exports, e)
		}
	}
	r.ActivityPhase = phases[rank]
	r.Phase = r.ActivityPhase
	var outcome Outcome
	if decode("evidence/outcome.json", &outcome) {
		r.Outcome = &outcome
		if outcome.ExitCode == nil || outcome.ShutdownOrExportFailed == nil {
			warn("evidence/outcome.json", "outcome requires exitCode and shutdownOrExportFailed; evidence pending")
		} else if *outcome.ExitCode < 0 || *outcome.ExitCode > 255 {
			warn("evidence/outcome.json", "invalid outcome exit code; evidence pending")
		} else {
			r.Provisional = false
			failed = failed || *outcome.ExitCode != 0 || *outcome.ShutdownOrExportFailed
			r.Phase = "finished"
		}
	}
	if failed {
		r.Phase = "failed"
	}
	finalStatus := "Unknown"
	if !r.Provisional {
		finalStatus = "True"
		if failed {
			finalStatus = "False"
		}
	}
	qualification := "Unknown"
	if r.Mode == "validation" {
		qualification = "False"
	}
	r.Conditions = append(r.Conditions, Condition{"FinalOutcomeSuccessful", finalStatus}, Condition{"ProcessAlive", "Unknown"}, Condition{"ExperimentQualified", qualification})
	if r.LastActivityAt != nil {
		age := max(0, now.Sub(*r.LastActivityAt).Seconds())
		r.StaleAgeSeconds = &age
	}
	return r, ctx.Err()
}

func complete(s Stage) bool { return !s.Completed.IsZero() && s.Duration > 0 }

func distribution(values []float64) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}
	sort.Float64s(values)
	n := len(values)
	return Distribution{Count: n, Min: values[0], Median: (values[(n-1)/2] + values[n/2]) / 2, P95: values[int(math.Ceil(float64(n)*0.95))-1], Max: values[n-1]}
}

// Only labels emitted by run-case.sh can become command evidence or output.
func commandPhase(name string) string {
	// Numbered commands are generated by bounded shell loops. Do not treat
	// arbitrary filenames sharing a prefix as commands (or disclose their names).
	if i := strings.LastIndexByte(name, '-'); i >= 0 {
		if n, err := strconv.Atoi(name[i+1:]); err == nil && n >= 0 && strconv.Itoa(n) == name[i+1:] {
			switch name[:i] {
			case "canary-baseline":
				return "baseline"
			case "canary-steady":
				return "steady"
			case "record-running", "seed-warmup":
				return "seed"
			case "adversaries-running":
				return "calibration"
			case "record-stopped", "adversaries-stopped":
				return "finalizing"
			case "capture-status", "capture-gate":
				return "preflight"
			}
		}
	}
	base := name
	for _, suffix := range []string{"-analysis", "-gate", "-health"} {
		base = strings.TrimSuffix(base, suffix)
	}
	switch base {
	case "metrics-capture", "metrics-nodes", "metrics-pressure", "metrics-optional-capture", "metrics-optional-nodes":
		return "finalizing"
	case "preflight-capture", "preflight-nodes":
		return "preflight"
	case "pressure-observed":
		return "settle"
	}
	switch name {
	case "failure-recovery", "recovery-window", "turnover-gate":
		return "recovery"
	case "record-stop", "record-export", "adversaries-stop", "hcp-final", "guest-final", "scrape-after":
		return "finalizing"
	case "turnover":
		return "steady"
	case "settle", "scrape-steady", "record-steady", "record-steady-gate", "token-lifetime":
		return "settle"
	case "baseline-budgets", "baseline-expectations", "adversaries-start":
		return "calibration"
	case "canary-warmup":
		return "baseline"
	case "seed", "seed-deployments", "seed-deployment-gate", "seed-pods", "seed-pod-gate", "seed-identities", "seed-credentials", "workload-setup", "canary-credentials", "record-start":
		return "seed"
	case "environment-refresh", "environment-refresh-gate", "cli-help", "workload-dispatch", "burner-hash", "guest-nodes", "guest-node-gate", "guest-baseline", "guest-capacity", "hcp-inventory", "hcp-gate", "record-absent", "adversaries-absent", "scrape-before",
		"validate-inputs", "validate-bundle", "validate-captures", "validate-queries", "validate-node-queries", "validate-optional-queries", "validate-optional-node-queries", "validate-queries-health", "validate-node-queries-health", "validate-adversaries", "validate-cpu-query", "validate-node-preflight":
		return "preflight"
	default:
		return ""
	}
}

// CadenceSkips decodes bounded, one-based skipped-slot observations. A malformed
// stream is not a partial count; callers must retain the observation gap warning.
func CadenceSkips(data []byte) ([]int, error) {
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("cadence exceeds read limit")
	}
	slots := []int{}
	seen := map[int]map[string]bool{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Slot   *int   `json:"slotIndex"`
			Number int    `json:"canaryNumber"`
			Type   string `json:"type"`
		}
		if json.Unmarshal(line, &record) != nil || record.Slot == nil || *record.Slot < 0 || record.Number <= 0 || *record.Slot != record.Number-1 || (record.Type != "missed-slot" && record.Type != "cycle-started" && record.Type != "cycle-completed") {
			return nil, fmt.Errorf("invalid skipped-slot observation")
		}
		if seen[record.Number] == nil {
			seen[record.Number] = map[string]bool{}
		}
		types := seen[record.Number]
		if types[record.Type] || types["missed-slot"] || (record.Type == "missed-slot" && len(types) > 0) {
			return nil, fmt.Errorf("conflicting cadence observations")
		}
		types[record.Type] = true
		if record.Type == "missed-slot" {
			slots = append(slots, record.Number)
		}
	}
	return slots, nil
}

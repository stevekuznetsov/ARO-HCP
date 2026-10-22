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

package progress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var epoch = time.Date(2026, 9, 9, 15, 0, 0, 0, time.UTC)

func put(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, epoch, epoch); err != nil {
		t.Fatal(err)
	}
}

func inspect(t *testing.T, dir string, now time.Time) Result {
	t.Helper()
	r, err := Inspect(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "v1" || r.Qualified {
		t.Fatalf("unexpected schema/qualification: %+v", r)
	}
	return r
}

func sampleJSON(t *testing.T, ns int64) string {
	t.Helper()
	s := Stage{Started: epoch, Completed: epoch.Add(time.Duration(ns)), Duration: ns}
	b, err := json.Marshal(Canary{Preflight: s, Ack: s, Ready: s, Cleanup: s})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func command(t *testing.T, dir, name, body string, exit *int) {
	t.Helper()
	put(t, dir, "commands/"+name+".started", epoch.Format(time.RFC3339Nano))
	if body != "" {
		put(t, dir, "commands/"+name+".json", body)
	}
	if exit != nil {
		put(t, dir, "commands/"+name+".exit", fmt.Sprint(*exit))
	}
}

func TestPhases(t *testing.T) {
	for _, tt := range []struct{ label, phase string }{
		{"", "incomplete"}, {"cli-help", "preflight"}, {"seed", "seed"},
		{"canary-baseline-1", "baseline"}, {"baseline-budgets", "calibration"},
		{"adversaries-running-3", "calibration"}, {"settle", "settle"},
		{"canary-steady-1", "steady"}, {"failure-recovery", "recovery"},
		{"recovery-window", "recovery"}, {"record-export", "finalizing"},
	} {
		t.Run(tt.phase+tt.label, func(t *testing.T) {
			dir := t.TempDir()
			// Neither folders nor a systemd default success value prove progress.
			if err := os.MkdirAll(filepath.Join(dir, "evidence/turnover"), 0700); err != nil {
				t.Fatal(err)
			}
			put(t, dir, "systemd.json", `{"Result":"success","ExecMainStatus":0}`)
			if tt.label != "" {
				command(t, dir, tt.label, "", nil)
			}
			r := inspect(t, dir, epoch)
			if r.Phase != tt.phase || !r.Provisional {
				t.Fatalf("phase=%s provisional=%v", r.Phase, r.Provisional)
			}
		})
	}
}

func TestEnvironmentRefreshProgress(t *testing.T) {
	for _, name := range []string{"environment-refresh", "environment-refresh-gate"} {
		t.Run(name, func(t *testing.T) {
			for _, tt := range []struct {
				name, exit, phase, state string
			}{
				{"active", "", "preflight", "inProgress"},
				{"partial-exit", "\n", "preflight", "pending"},
				{"failed", "1\n", "failed", "failed"},
			} {
				t.Run(tt.name, func(t *testing.T) {
					dir := t.TempDir()
					command(t, dir, name, `{"partial":`, nil)
					if tt.exit != "" {
						put(t, dir, "commands/"+name+".exit", tt.exit)
					}
					r := inspect(t, dir, epoch)
					if r.Phase != tt.phase || r.ActivityPhase != "preflight" || !r.Provisional || r.Outcome != nil {
						t.Fatalf("wrong refresh phase without final outcome: %+v", r)
					}
					if len(r.Commands) != 1 || r.Commands[0].Name != name || r.Commands[0].State != tt.state || r.Commands[0].StartedAt == nil || !r.Commands[0].StartedAt.Equal(epoch) {
						t.Fatalf("refresh command not tracked: %+v", r.Commands)
					}
					if tt.state == "failed" {
						if r.Commands[0].ExitCode == nil || *r.Commands[0].ExitCode != 1 {
							t.Fatalf("nonzero refresh exit ignored: %+v", r.Commands[0])
						}
					} else if r.Commands[0].ExitCode != nil {
						t.Fatalf("invented exit code from incomplete evidence: %+v", r.Commands[0])
					}
					if tt.name == "partial-exit" {
						if len(r.Warnings) != 1 || r.Warnings[0].Source != "commands/"+name+".exit" {
							t.Fatalf("partial exit must warn without failing inspection: %+v", r.Warnings)
						}
					} else if len(r.Warnings) != 0 {
						t.Fatalf("refresh JSON body should not be decoded: %+v", r.Warnings)
					}
				})
			}
		})
	}
}

func TestZeroApplicationsFullWindow(t *testing.T) {
	dir := t.TempDir()
	zero := 0
	put(t, dir, "inputs/turnover.json", `{"applications":0,"durationSeconds":7200,"recoverySeconds":600}`)
	put(t, dir, "evidence/window-start.txt", epoch.Add(-time.Hour).Format(time.RFC3339Nano))
	put(t, dir, "evidence/steady-start.txt", epoch.Format(time.RFC3339Nano))
	put(t, dir, "commands/baseline-budgets.json", `{"ack":300000000,"ready":16000000000,"cleanup":17000000000}`)
	for i := 1; i <= 5; i++ {
		command(t, dir, fmt.Sprintf("canary-baseline-%d", i), sampleJSON(t, int64(i)*1e9), &zero)
	}
	command(t, dir, "canary-steady-1", sampleJSON(t, 2e9), &zero)
	r := inspect(t, dir, epoch.Add(150*time.Second))
	if r.Applications == nil || *r.Applications != 0 || r.Phase != "steady" || r.WindowStart == nil {
		t.Fatalf("wrong zero-app progress: %+v", r)
	}
	if *r.SteadyCanaries.Expected != 24 || *r.SteadyCanaries.Remaining != 23 || r.SteadyCanaries.Passed != 1 {
		t.Fatalf("wrong cadence: %+v", r.SteadyCanaries)
	}
	if r.Baseline.Passed != 5 || r.Baseline.Ready.Max != 5 || r.Baseline.Ready.Median != 3 || r.Baseline.Ready.P95 != 5 || r.Deadlines.Ready != 16000000000 {
		t.Fatalf("wrong baseline statistics: %+v", r.Baseline)
	}
	if *r.Steady.ElapsedSeconds != 150 || *r.Steady.RemainingSeconds != 7050 {
		t.Fatalf("wrong timing: %+v", r.Steady)
	}
	for i := 2; i <= 24; i++ {
		command(t, dir, fmt.Sprintf("canary-steady-%d", i), sampleJSON(t, int64(i)*1e9), &zero)
	}
	command(t, dir, "turnover", `{"scheduled":0,"completed":0,"failed":0,"backlog":0,"outstanding":0}`, &zero)
	command(t, dir, "recovery-window", "", nil)
	r = inspect(t, dir, epoch.Add(7500*time.Second))
	if r.Phase != "recovery" || !r.Provisional || r.SteadyCanaries.Passed != 24 || *r.SteadyCanaries.Remaining != 0 || r.Turnover.Completed != 0 {
		t.Fatalf("wrong recovery: %+v", r)
	}
	if *r.Steady.ElapsedSeconds != 7200 || *r.Steady.OverdueSeconds != 300 {
		t.Fatalf("wrong bounded elapsed: %+v", r.Steady)
	}
	put(t, dir, "evidence/window-end.txt", epoch.Add(7800*time.Second).Format(time.RFC3339))
	command(t, dir, "record-export", `{"helperCleanupFailed":false,"archive":"SECRET","status":{"message":"SECRET"}}`, &zero)
	r = inspect(t, dir, epoch.Add(7900*time.Second))
	if r.Phase != "finalizing" || !r.Provisional || len(r.Exports) != 1 || *r.Exports[0].HelperCleanupFailed {
		t.Fatalf("wrong export progress: %+v", r)
	}
	put(t, dir, "evidence/outcome.json", `{"exitCode":0,"shutdownOrExportFailed":false,"qualified":true,"capturesLeftRunning":true}`)
	r = inspect(t, dir, epoch.Add(8000*time.Second))
	if r.Phase != "finished" || r.Provisional || *r.Steady.OverdueSeconds != 600 {
		t.Fatalf("wrong outcome: %+v", r)
	}
}

func TestCanaryEvidenceAndActiveElapsed(t *testing.T) {
	dir := t.TempDir()
	zero, one := 0, 1
	put(t, dir, "inputs/turnover.json", `{"durationSeconds":601}`)
	put(t, dir, "evidence/steady-start.txt", epoch.Format(time.RFC3339))
	command(t, dir, "canary-steady-1", "", &zero)
	command(t, dir, "canary-steady-2", `{"ack":`, nil)
	r := inspect(t, dir, epoch.Add(350*time.Second))
	if r.SteadyCanaries.Passed != 0 || r.SteadyCanaries.Pending != 2 || r.SteadyCanaries.Active.Name != "canary-steady-2" || *r.Steady.ElapsedSeconds != 350 || *r.SteadyCanaries.Expected != 3 || *r.SteadyCanaries.Remaining != 1 {
		t.Fatalf("wrong active/pending snapshot: %+v", r)
	}
	if r.Phase != "steady" || len(r.Warnings) < 2 {
		t.Fatalf("partial writes must warn, not fail: %+v", r)
	}
	command(t, dir, "canary-steady-1", sampleJSON(t, 1e9), &zero)
	command(t, dir, "canary-steady-2", sampleJSON(t, 99e9), &one)
	r = inspect(t, dir, epoch.Add(400*time.Second))
	if r.Phase != "failed" || !r.Provisional || r.SteadyCanaries.Passed != 1 || r.SteadyCanaries.Failed != 1 || r.SteadyCanaries.Ack.Max != 1 {
		t.Fatalf("failed sample contaminated distribution: %+v", r)
	}
}

func TestCanaryMeasurementMetadata(t *testing.T) {
	zero, one := 0, 1
	for _, tt := range []struct {
		name, metadata, mode string
		watch                *WatchDiagnostics
		exit                 *int
		state                string
		warning              bool
	}{
		{name: "historical", exit: &zero, state: "completed"},
		{name: "explicit-empty", metadata: `,"measurementMode":""`, exit: &zero, state: "completed"},
		{name: "polling", metadata: `,"measurementMode":"polling"`, mode: "polling", exit: &zero, state: "completed"},
		{name: "watch-zero", metadata: `,"measurementMode":"watch","watch":{"events":0,"reconnects":0,"gaps":0}`, mode: "watch", watch: &WatchDiagnostics{}, exit: &zero, state: "completed"},
		{name: "watch", metadata: `,"measurementMode":"watch","watch":{"events":12,"reconnects":0,"gaps":0}`, mode: "watch", watch: &WatchDiagnostics{Events: 12}, exit: &zero, state: "completed"},
		{name: "watch-gap-zero-exit", metadata: `,"measurementMode":"watch","watch":{"events":12,"reconnects":1,"gaps":2,"errors":["watch stream closed"]}`, mode: "watch", watch: &WatchDiagnostics{Events: 12, Reconnects: 1, Gaps: 2, Errors: []string{"watch stream closed"}}, exit: &zero, state: "completed", warning: true},
		{name: "watch-gap-failed", metadata: `,"measurementMode":"watch","watch":{"events":12,"reconnects":0,"gaps":1,"errors":["watch stream closed"]}`, mode: "watch", watch: &WatchDiagnostics{Events: 12, Gaps: 1, Errors: []string{"watch stream closed"}}, exit: &one, state: "failed"},
		{name: "watch-active", metadata: `,"measurementMode":"watch","watch":{"events":3,"reconnects":0,"gaps":0}`, mode: "watch", watch: &WatchDiagnostics{Events: 3}, state: "inProgress"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			body := strings.TrimSuffix(sampleJSON(t, 1234567890), "}") + tt.metadata + "}"
			for _, name := range []string{"canary-baseline-1", "canary-steady-1"} {
				command(t, dir, name, body, tt.exit)
			}
			r := inspect(t, dir, epoch)
			if tt.warning {
				want := []Warning{
					{"commands/canary-baseline-1.json", "zero exit with watch gaps; measurement invalid"},
					{"commands/canary-steady-1.json", "zero exit with watch gaps; measurement invalid"},
				}
				if !reflect.DeepEqual(r.Warnings, want) {
					t.Fatalf("wrong gap warnings: %+v", r.Warnings)
				}
			} else if len(r.Warnings) != 0 {
				t.Fatalf("unexpected warnings: %+v", r.Warnings)
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if tt.mode == "" && strings.Contains(string(b), `"measurementMode"`) {
				t.Fatalf("invented historical measurement mode: %s", b)
			}
			if tt.watch == nil && strings.Contains(string(b), `"watch"`) {
				t.Fatalf("invented watch diagnostics: %s", b)
			}
			var roundTrip Result
			if err := json.Unmarshal(b, &roundTrip); err != nil {
				t.Fatal(err)
			}
			for _, s := range []Canaries{roundTrip.Baseline, roundTrip.SteadyCanaries} {
				if len(s.Samples) != 1 || s.Latest == nil || !reflect.DeepEqual(*s.Latest, s.Samples[0]) {
					t.Fatalf("missing sample/latest: %+v", s)
				}
				sample := s.Samples[0]
				if sample.MeasurementMode != tt.mode || !reflect.DeepEqual(sample.Watch, tt.watch) || sample.State != tt.state || !reflect.DeepEqual(sample.ExitCode, tt.exit) {
					t.Fatalf("metadata or command evidence changed: %+v", sample)
				}
				wantStage := Stage{Started: epoch, Completed: epoch.Add(1234567890), Duration: 1234567890}
				if sample.Preflight != wantStage || sample.Ack != wantStage || sample.Ready != wantStage || sample.Cleanup != wantStage {
					t.Fatalf("stage durations reinterpreted: %+v", sample)
				}
				switch tt.state {
				case "completed":
					want := Distribution{Count: 1, Min: 1.23456789, Median: 1.23456789, P95: 1.23456789, Max: 1.23456789}
					if s.Passed != 1 || s.Failed != 0 || s.Pending != 0 || s.Ack != want || s.Ready != want || s.Cleanup != want {
						t.Fatalf("command counts or durations changed: %+v", s)
					}
				case "failed":
					if s.Failed != 1 || s.Passed != 0 || s.Pending != 0 || s.Ack.Count != 0 || s.Ready.Count != 0 || s.Cleanup.Count != 0 {
						t.Fatalf("failed watch sample counted as passed: %+v", s)
					}
				case "inProgress":
					if s.Pending != 1 || s.Passed != 0 || s.Failed != 0 || s.Active == nil || !reflect.DeepEqual(*s.Active, sample) {
						t.Fatalf("active watch metadata lost: %+v", s)
					}
				}
			}
		})
	}
}

func TestRecordedModeAndCadence(t *testing.T) {
	for _, tt := range []struct {
		name, run, session, turnover, mode string
		cadence, duration                  int64
		expected                           int
	}{
		{"validation", `{"mode":"validation","steadySeconds":300,"recoverySeconds":30,"canaryCadenceSeconds":30}`, "", `{"durationSeconds":300}`, "validation", 30, 300, 10},
		{"evidence-only", `{"mode":"validation","steadySeconds":300,"recoverySeconds":30,"canaryCadenceSeconds":30}`, "", "", "validation", 30, 300, 10},
		{"session-only", "", `{"mode":"validation","steadySeconds":300,"canaryCadenceSeconds":30}`, "", "validation", 30, 300, 10},
		{"evidence-precedence", `{"mode":"validation","canaryCadenceSeconds":30}`, `{"mode":"soak","canaryCadenceSeconds":300}`, `{"durationSeconds":301}`, "validation", 30, 301, 11},
		{"legacy-soak", `{"experimental":true,"qualified":false}`, "", `{"durationSeconds":7200}`, "soak", 300, 7200, 24},
		{"short-unknown", "", "", `{"durationSeconds":300}`, "unknown", 300, 300, 1},
		{"explicit-soak", `{"mode":"soak","canaryCadenceSeconds":60}`, "", `{"durationSeconds":7200}`, "soak", 60, 7200, 120},
		{"validation-legacy-cadence", `{"mode":"validation"}`, "", `{"durationSeconds":300}`, "validation", 300, 300, 1},
		{"invalid-mode", `{"mode":"SECRET"}`, "", `{"durationSeconds":7200}`, "unknown", 300, 7200, 24},
		{"invalid-cadence", `{"mode":"validation","canaryCadenceSeconds":0}`, "", `{"durationSeconds":300}`, "validation", 300, 300, 1},
		{"partial-metadata", `{"mode":"validation",`, `{"mode":"validation","canaryCadenceSeconds":30}`, `{"durationSeconds":300}`, "validation", 30, 300, 10},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{"evidence/run.json": tt.run, "inputs/session.json": tt.session, "inputs/turnover.json": tt.turnover} {
				if body != "" {
					put(t, dir, name, body)
				}
			}
			put(t, dir, "evidence/steady-start.txt", epoch.Format(time.RFC3339))
			r := inspect(t, dir, epoch.Add(150*time.Second))
			if r.Mode != tt.mode || r.CanaryCadenceSeconds != tt.cadence || r.SteadyCanaries.Expected == nil || *r.SteadyCanaries.Expected != tt.expected {
				t.Fatalf("wrong mode/cadence: %+v", r)
			}
			if r.Phase != "steady" || r.Steady.Deadline == nil || !r.Steady.Deadline.Equal(epoch.Add(time.Duration(tt.duration)*time.Second)) || *r.Steady.RemainingSeconds != float64(tt.duration-150) {
				t.Fatalf("wrong steady timing: %+v", r)
			}
			r = inspect(t, dir, epoch.Add(time.Duration(tt.duration)*time.Second))
			if r.Phase != "recovery" || *r.Steady.RemainingSeconds != 0 {
				t.Fatalf("wrong recovery deadline: %+v", r)
			}
			put(t, dir, "evidence/window-end.txt", epoch.Add(time.Duration(tt.duration)*time.Second).Format(time.RFC3339))
			put(t, dir, "evidence/outcome.json", `{"exitCode":0,"shutdownOrExportFailed":false,"qualified":true}`)
			r = inspect(t, dir, epoch.Add(time.Duration(tt.duration)*time.Second+time.Hour))
			if r.Phase != "finished" || r.Provisional || r.Qualified || *r.Baseline.Expected != 5 || *r.Steady.ElapsedSeconds != float64(tt.duration) {
				t.Fatalf("wrong completed run: %+v", r)
			}
			for _, condition := range r.Conditions {
				if condition.Type == "ExperimentQualified" && tt.mode == "validation" && condition.Status != "False" {
					t.Fatalf("validation must not qualify: %+v", condition)
				}
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), `"mode":"`+tt.mode+`"`) || !strings.Contains(string(b), fmt.Sprintf(`"canaryCadenceSeconds":%d`, tt.cadence)) || strings.Contains(string(b), "SECRET") {
				t.Fatalf("wrong JSON contract: %s", b)
			}
		})
	}
}

func TestRecordedCadenceBeyondLegacyCanaryLimit(t *testing.T) {
	dir := t.TempDir()
	zero := 0
	put(t, dir, "evidence/run.json", `{"mode":"validation","steadySeconds":300,"canaryCadenceSeconds":5}`)
	for i := 1; i <= 60; i++ {
		command(t, dir, fmt.Sprintf("canary-steady-%d", i), sampleJSON(t, int64(i)*1e9), &zero)
	}
	r := inspect(t, dir, epoch)
	if *r.SteadyCanaries.Expected != 60 || *r.SteadyCanaries.Remaining != 0 || r.SteadyCanaries.Passed != 60 || r.SteadyCanaries.Latest.Name != "canary-steady-60" || r.SteadyCanaries.Ack.Max != 60 {
		t.Fatalf("steady samples truncated or out of order: %+v", r.SteadyCanaries)
	}
}

func TestCanaryCompletionRequirements(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `{"ack":{"completed":"2026-09-09T15:00:00Z","durationNanoseconds":1}}`,
		strings.ReplaceAll(sampleJSON(t, 1), `"durationNanoseconds":1`, `"durationNanoseconds":0`),
		strings.ReplaceAll(sampleJSON(t, 1), `"durationNanoseconds":1`, `"durationNanoseconds":"SECRET"`),
		strings.ReplaceAll(sampleJSON(t, 1), `2026-09-09T15:00:00.000000001Z`, `0001-01-01T00:00:00Z`),
	} {
		dir := t.TempDir()
		zero := 0
		command(t, dir, "canary-baseline-1", body, &zero)
		r := inspect(t, dir, epoch)
		if r.Baseline.Passed != 0 || r.Baseline.Pending != 1 || len(r.Warnings) == 0 {
			t.Fatalf("incomplete sample counted: %+v", r)
		}
	}
}

func TestFailureStagePreflight(t *testing.T) {
	dir := t.TempDir()
	one := 1
	command(t, dir, "canary-steady-1", `{"preflight":{"started":"2026-09-09T15:00:00Z","durationNanoseconds":30}}`, &one)
	r := inspect(t, dir, epoch)
	if r.SteadyCanaries.Latest.FailureStage != "preflight" || r.Phase != "failed" {
		t.Fatalf("preflight misclassified: %+v", r)
	}
}

func TestExpectedFailureAndOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name, body, phase string
		provisional       bool
	}{
		{"absent", "", "preflight", true},
		{"partial", `{"exitCode":`, "preflight", true},
		{"malformed-final", `{"exitCode":"SECRET","shutdownOrExportFailed":false}`, "preflight", true},
		{"missing-keys", `{}`, "preflight", true},
		{"invalid-exit", `{"exitCode":-1,"shutdownOrExportFailed":false}`, "preflight", true},
		{"success", `{"exitCode":0,"shutdownOrExportFailed":false}`, "finished", false},
		{"failure", `{"exitCode":1,"shutdownOrExportFailed":false}`, "failed", false},
		{"export-failure", `{"exitCode":0,"shutdownOrExportFailed":true}`, "failed", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			one := 1
			command(t, dir, "workload-dispatch", "", &one)
			if tt.body != "" {
				put(t, dir, "evidence/outcome.json", tt.body)
			}
			r := inspect(t, dir, epoch)
			if r.Phase != tt.phase || r.Provisional != tt.provisional || r.Commands[0].State != "expectedFailure" {
				t.Fatalf("wrong outcome: %+v", r)
			}
			if tt.provisional && tt.body != "" && len(r.Warnings) == 0 {
				t.Fatal("bad outcome needs warning")
			}
		})
	}
	dir := t.TempDir()
	one := 1
	command(t, dir, "validate-inputs", "", &one)
	put(t, dir, "evidence/outcome.json", `{"exitCode":0,"shutdownOrExportFailed":false}`)
	if r := inspect(t, dir, epoch); r.Phase != "failed" {
		t.Fatalf("nonzero command ignored: %+v", r)
	}
}

func TestTimingFallbackAndStaleness(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "evidence/steady-start.txt", epoch.Format(time.RFC3339))
	r := inspect(t, dir, epoch.Add(48*time.Hour))
	if r.Steady.DurationSeconds != nil || r.SteadyCanaries.Expected != nil || r.Steady.Deadline != nil || r.Phase == "failed" || *r.StaleAgeSeconds != 172800 || r.LastActivitySource != "evidence/steady-start.txt" {
		t.Fatalf("invented duration or liveness: %+v", r)
	}
	put(t, dir, "commands/turnover.json", `{"spec":{"durationSeconds":600},"completed":`)
	put(t, dir, "evidence/turnover/result.json", `{"spec":{"durationSeconds":301,"applications":0},"scheduled":0,"completed":0}`)
	r = inspect(t, dir, epoch.Add(200*time.Second))
	if *r.Steady.DurationSeconds != 301 || *r.SteadyCanaries.Expected != 2 || *r.Steady.ElapsedSeconds != 200 {
		t.Fatalf("fallback failed: %+v", r)
	}
	put(t, dir, "inputs/turnover.json", `{"durationSeconds":1}`)
	r = inspect(t, dir, epoch.Add(time.Second))
	if *r.SteadyCanaries.Expected != 1 || *r.Steady.ElapsedSeconds != 1 || r.Phase != "recovery" {
		t.Fatalf("input precedence/cadence failed: %+v", r)
	}
}

func TestReadBoundsRedactionAndNoWrites(t *testing.T) {
	dir := t.TempDir()
	zero := 0
	command(t, dir, "canary-steady-1", sampleJSON(t, 1e9), &zero)
	put(t, dir, "commands/canary-steady-2.json", strings.Repeat("X", maxFileBytes+1))
	command(t, dir, "canary-steady-2", "", &zero)
	put(t, dir, "commands/baseline-budgets.json", `{"ack":"SECRET"}`)
	put(t, dir, "commands/canary-steady-1.json", strings.TrimSuffix(sampleJSON(t, 1e9), "}")+`,"failureStage":"SECRET","unknown":{"token":"SECRET"}}`)
	put(t, dir, "commands/seed-credentials.json", `{"token":"SECRET"}`)
	put(t, dir, "evidence/run.json", `{"mode":"SECRET","unknown":{"token":"SECRET"}}`)
	put(t, dir, "commands/seed-SECRET.started", "SECRET")
	put(t, dir, "private/canary.kubeconfig", "SECRET")
	put(t, dir, "evidence/turnover/events.ndjson", "SECRET")
	path := filepath.Join(dir, "evidence/turnover/events.ndjson")
	if err := os.Truncate(path, 1<<34); err != nil {
		t.Fatal(err)
	}
	activity := epoch.Add(60 * time.Second)
	if err := os.Chtimes(path, activity, activity); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	r := inspect(t, dir, epoch.Add(120*time.Second))
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SECRET") || r.SteadyCanaries.Passed != 1 || r.SteadyCanaries.Pending != 1 || *r.StaleAgeSeconds != 60 || r.LastActivitySource != "evidence/turnover/events.ndjson" {
		t.Fatalf("unsafe snapshot: %s", b)
	}
	if len(r.Warnings) < 2 {
		t.Fatal("oversize/type errors need warnings")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || after.ModTime() != before.ModTime() {
		t.Fatal("inspection modified source")
	}
}

func TestLocalPathsAndCancellation(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	put(t, outside, "outcome.json", `{"exitCode":0,"shutdownOrExportFailed":false}`)
	if err := os.Symlink(outside, filepath.Join(dir, "evidence")); err != nil {
		t.Fatal(err)
	}
	if r := inspect(t, dir, epoch); r.Phase != "incomplete" || !r.Provisional {
		t.Fatalf("followed external artifact link: %+v", r)
	}
	if _, err := Inspect(filepath.Join(dir, "missing"), epoch); err == nil {
		t.Fatal("missing root should error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectContext(ctx, dir, epoch); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestSoftExpectationsPreserveCompletionAndCadence(t *testing.T) {
	dir := t.TempDir()
	zero := 0
	put(t, dir, "inputs/session.json", `{"mode":"validation","steadySeconds":90,"canaryCadenceSeconds":30,"canaryPolicy":"observe-degradation","hardDeadlineSeconds":{"ack":30,"ready":120,"cleanup":120}}`)
	put(t, dir, "commands/baseline-expectations.json", `{"ack":999999999,"ready":999999999,"cleanup":999999999}`)
	put(t, dir, "evidence/outcome.json", `{"exitCode":0,"shutdownOrExportFailed":false}`)
	expected, deadline := int64(180*time.Millisecond), int64(30*time.Second)
	s := Stage{Started: epoch, Completed: epoch.Add(500 * time.Millisecond), Duration: int64(500 * time.Millisecond), Expected: &expected, Deadline: &deadline, ExpectationExceeded: true}
	body, _ := json.Marshal(Canary{Ack: s, Ready: s, Cleanup: s, Performance: "not_evaluated"})
	command(t, dir, "canary-steady-1", string(body), &zero)
	command(t, dir, "canary-steady-3", string(body), &zero)
	put(t, dir, "evidence/canary-cadence.jsonl", "{\"type\":\"cycle-started\",\"slotIndex\":0,\"canaryNumber\":1}\n{\"type\":\"cycle-completed\",\"slotIndex\":0,\"canaryNumber\":1}\n{\"type\":\"missed-slot\",\"slotIndex\":1,\"canaryNumber\":2}\n")
	r := inspect(t, dir, epoch.Add(time.Minute))
	if r.Phase != "finished" || r.SteadyCanaries.Passed != 2 || r.SteadyCanaries.Degraded != 2 || r.SteadyCanaries.Failed != 0 || r.SteadyCanaries.Skipped != 1 || *r.SteadyCanaries.Remaining != 0 || r.SteadyCanaries.Ack.Max != .5 || r.SteadyCanaries.Ack.Count != 2 {
		t.Fatalf("soft overrun/cadence became failure: %+v", r)
	}
	if r.Deadlines != nil || r.ExpectedDurationsNanoseconds.Ack != 999999999 || r.HardDeadlinesNanoseconds.Ack != int64(30*time.Second) || r.HardDeadlinesNanoseconds.Ready != int64(120*time.Second) || r.CanaryPolicy != "observe-degradation" {
		t.Fatalf("ambiguous thresholds: %+v", r)
	}
	if !reflect.DeepEqual(r.SteadyCanaries.Latest.Ack, s) || r.SteadyCanaries.Latest.Performance != "degraded" || len(r.Warnings) != 1 {
		t.Fatalf("recorded expectations lost: %+v", r)
	}
	data, _ := json.Marshal(r)
	var decoded Result
	if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(decoded.SteadyCanaries, r.SteadyCanaries) {
		t.Fatalf("round trip: %v", err)
	}
	// The same elapsed overrun without completion is not performance degradation.
	one := 1
	s.Completed = time.Time{}
	body, _ = json.Marshal(Canary{Ack: s, Ready: s, Cleanup: s, Performance: "degraded"})
	command(t, dir, "canary-steady-1", string(body), &one)
	r = inspect(t, dir, epoch.Add(time.Minute))
	if r.Phase != "failed" || r.SteadyCanaries.Samples[0].Performance != "not_evaluated" || r.SteadyCanaries.Passed != 1 || r.SteadyCanaries.Degraded != 1 || r.SteadyCanaries.Ack.Count != 1 {
		t.Fatalf("censored stage evaluated: %+v", r)
	}
}

func TestHardDeadlineSecondsBounds(t *testing.T) {
	for _, value := range []string{"0", "-1", "9223372037", "9223372036854775808", "0.5", `"30"`, "null"} {
		t.Run(value, func(t *testing.T) {
			dir := t.TempDir()
			put(t, dir, "inputs/session.json", `{"hardDeadlineSeconds":{"ack":`+value+`,"ready":120,"cleanup":120}}`)
			r := inspect(t, dir, epoch)
			if r.HardDeadlinesNanoseconds != nil || len(r.Warnings) == 0 {
				t.Fatalf("unsafe seconds conversion: %+v", r)
			}
		})
	}
}

func TestCadenceSkipsValidation(t *testing.T) {
	for _, body := range []string{
		`{"type":"missed-slot","slotIndex":-1,"canaryNumber":0}`,
		`{"type":"missed-slot","slotIndex":0,"canaryNumber":2}`,
		`{"type":"missed-slot","canaryNumber":1}`,
		`{"type":"unknown","slotIndex":0,"canaryNumber":1}`,
		"{\"type\":\"missed-slot\",\"slotIndex\":0,\"canaryNumber\":1}\n{\"type\":\"cycle-started\",\"slotIndex\":0,\"canaryNumber\":1}",
		"{\"type\":\"missed-slot\",\"slotIndex\":0,\"canaryNumber\":1}\n{",
	} {
		if slots, err := CadenceSkips([]byte(body)); err == nil || slots != nil {
			t.Fatalf("invalid cadence counted: %s %+v %v", body, slots, err)
		}
	}
}

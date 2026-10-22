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

package assessment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

// RunOutcome is the runner's terminal observation, not capacity qualification.
// Optional fields permit assessment of already persisted, older run artifacts.
type RunOutcome struct {
	ExitCode               *int                   `json:"exitCode"`
	WorkloadExitCode       *int                   `json:"workloadExitCode"`
	ShutdownOrExportFailed *bool                  `json:"shutdownOrExportFailed"`
	ExecutionStatus        string                 `json:"executionStatus"`
	ExperimentStatus       string                 `json:"experimentStatus"`
	CaptureStatus          string                 `json:"captureStatus"`
	ShutdownStatus         string                 `json:"shutdownStatus"`
	StopReason             string                 `json:"stopReason"`
	FailedLabel            *string                `json:"failedLabel"`
	ExpectationViolations  *ExpectationViolations `json:"expectationViolations"`
}

type ExpectationViolations struct {
	DegradedCanaries     *int `json:"degradedCanaries"`
	MissedCanarySlots    *int `json:"missedCanarySlots"`
	CompletedAfterWindow *int `json:"completedAfterWindow"`
	// External safety monitors have no canonical input contract. Never infer zero.
	Safety *int `json:"safety"`
}

var outcomeLabel = regexp.MustCompile(`^[a-z][a-z0-9-]{0,127}$`)

type CommandProblem struct {
	Label          string   `json:"label"`
	ExitCode       int      `json:"exitCode"`
	Classification string   `json:"classification"`
	Phase          string   `json:"phase"`
	EvidenceRefs   []string `json:"evidenceRefs"`
}

func (b *builder) commandProblems(root *os.Root) {
	const path = "evidence/failures.jsonl"
	data, err := readMetadata(b.ctx, root, path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		b.finding("measurement", "inconclusive", "Command failure ledger unavailable or exceeds metadata bound.", nil, "command problems")
		return
	}
	hash := sha256.Sum256(data)
	ref := b.evidence(filepath.Join(b.base, path), hex.EncodeToString(hash[:]))
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var p CommandProblem
		valid := json.Unmarshal(line, &p) == nil && outcomeLabel.MatchString(p.Label) && p.ExitCode > 0 && p.ExitCode <= 255
		valid = valid && (p.Phase == "input" || p.Phase == "preflight" || p.Phase == "workload" || p.Phase == "finalization")
		switch p.Classification {
		case "expected_probe", "input_error", "harness_error", "capture_error", "shutdown_error", "preflight_not_viable", "expectation_violation", "workload_aborted":
		default:
			valid = false
		}
		if !valid {
			b.finding("measurement", "inconclusive", "Invalid command failure ledger entry; raw entry omitted.", []string{ref}, "command problems")
			continue
		}
		p.EvidenceRefs = []string{ref}
		b.result.CommandProblems = append(b.result.CommandProblems, p)
	}
}

func (b *builder) outcome(doc map[string]json.RawMessage, ref string) {
	s := &b.result.Status
	s.Execution, s.Experiment = "unknown", "unknown"
	if s.Performance == "degraded" {
		s.Experiment = "degraded"
	} else if s.Workload == "pass" && s.Performance == "within_expectations" {
		s.Experiment = "within_expectations"
	}
	if _, classified := doc["executionStatus"]; !classified {
		return
	}
	var o RunOutcome
	data, _ := json.Marshal(doc)
	valid := json.Unmarshal(data, &o) == nil
	for _, n := range []*int{o.ExitCode, o.WorkloadExitCode} {
		valid = valid && n != nil && *n >= 0 && *n <= 255
	}
	valid = valid && o.ShutdownOrExportFailed != nil
	valid = valid && (o.ExecutionStatus == "completed" || o.ExecutionStatus == "aborted" || o.ExecutionStatus == "harness_error")
	valid = valid && (o.ExperimentStatus == "within_expectations" || o.ExperimentStatus == "degraded" || o.ExperimentStatus == "violations" || o.ExperimentStatus == "unknown")
	valid = valid && (o.CaptureStatus == "complete" || o.CaptureStatus == "incomplete" || o.CaptureStatus == "unknown")
	valid = valid && (o.ShutdownStatus == "complete" || o.ShutdownStatus == "incomplete")
	valid = valid && (o.FailedLabel == nil || outcomeLabel.MatchString(*o.FailedLabel))
	switch o.StopReason {
	case "completed", "signal_INT", "signal_TERM", "signal_HUP", "workload_aborted", "preflight_not_viable", "expectation_violation", "input_error", "harness_error", "capture_error", "shutdown_error":
	default:
		valid = false
	}
	if v := o.ExpectationViolations; v != nil {
		for _, n := range []*int{v.DegradedCanaries, v.MissedCanarySlots, v.CompletedAfterWindow} {
			valid = valid && (n == nil || (*n >= 0 && *n <= 80000))
		}
		valid = valid && v.Safety == nil
	}
	if valid {
		valid = (*o.WorkloadExitCode == 0 && o.ExecutionStatus == "completed" && o.StopReason == "completed") ||
			(*o.WorkloadExitCode != 0 && o.StopReason != "completed" && (o.ExecutionStatus != "completed" || o.StopReason == "expectation_violation"))
		valid = valid && (*o.ExitCode == *o.WorkloadExitCode || (*o.WorkloadExitCode == 0 && *o.ExitCode == 1 && *o.ShutdownOrExportFailed))
	}
	if !valid {
		b.finding("measurement", "inconclusive", "Invalid terminal outcome classification; execution remains unknown. Raw command evidence is assessed independently.", []string{ref}, "terminal outcome")
		return
	}
	b.result.Outcome = &o
	s.Execution = o.ExecutionStatus
	if o.ExperimentStatus == "violations" {
		s.Experiment = "violations"
	} else if o.ExperimentStatus == "degraded" {
		s.Experiment = "degraded"
	} else if s.Execution != "completed" {
		s.Experiment = "unknown"
	}
	if o.CaptureStatus == "incomplete" {
		s.Capture = "incomplete"
	}
	b.finding("measurement", "observed", "Runner execution is "+o.ExecutionStatus+" ("+o.StopReason+"). A recorded expectation violation is terminal experiment feedback, not an unhandled harness crash. Capture and qualification remain independent.", []string{ref}, "terminal outcome")
}

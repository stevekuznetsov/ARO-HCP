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

// Package assessment builds durable, non-qualifying assessments of local artifacts.
// It never contacts a cluster, executes commands, or changes experiment inputs.
package assessment

import (
	"encoding/json"
	"fmt"
	"time"
)

const SchemaVersion = "hcp-perf.assessment/v1"
const AnalysisVersion = "v2"

type Options struct {
	Input           string
	Metrics         []string
	RecorderSummary string
	RequestEvidence string
	// Now permits reproducible offline analysis. Zero uses the current UTC time.
	Now time.Time
}

type Result struct {
	SchemaVersion   string           `json:"schemaVersion"`
	AnalysisVersion string           `json:"analysisVersion"`
	GeneratedAt     time.Time        `json:"generatedAt"`
	Experiment      Experiment       `json:"experiment"`
	Status          Status           `json:"status"`
	Findings        []Finding        `json:"findings"`
	Failures        []Failure        `json:"failures"`
	Evidence        []Evidence       `json:"evidence"`
	DataPoints      []DataPoint      `json:"dataPoints"`
	CarryForward    []CarryForward   `json:"carryForward"`
	MetricsCoverage []Coverage       `json:"metricsCoverage"`
	Outcome         *RunOutcome      `json:"outcome,omitempty"`
	CommandProblems []CommandProblem `json:"commandProblems,omitempty"`
}

type Experiment struct {
	ID                    string `json:"id"`
	Title                 string `json:"title"`
	Question              string `json:"question"`
	Mode                  string `json:"mode"`
	GuestNodes            *int64 `json:"guestNodes"`
	Applications          *int64 `json:"applications"`
	Pods                  *int64 `json:"pods"`
	SteadySeconds         *int64 `json:"steadySeconds"`
	TurnoverPeriodSeconds *int64 `json:"turnoverPeriodSeconds,omitempty"`
	CPUTreatment          string `json:"cpuTreatment"`
	MeasurementMode       string `json:"measurementMode"`
	CanaryPolicy          string `json:"canaryPolicy,omitempty"`
	// FieldEvidence links explicit values to their source, not a guessed run type.
	FieldEvidence map[string][]string `json:"fieldEvidence"`
}

type Status struct {
	Execution     string `json:"execution,omitempty"`   // completed, aborted, harness_error, unknown
	Experiment    string `json:"experiment,omitempty"`  // within_expectations, degraded, violations, unknown
	Workload      string `json:"workload"`              // pass, fail, incomplete
	Performance   string `json:"performance,omitempty"` // within_expectations, degraded, unknown; absent in imported v1
	Capture       string `json:"capture"`               // complete, incomplete, unknown
	Qualification string `json:"qualification"`         // always not_assessed in v1
}

type Window struct {
	Start *time.Time `json:"start"`
	End   *time.Time `json:"end"`
}

type Evidence struct {
	ID          string  `json:"id"`
	Path        string  `json:"path"` // relative to Options.Input, including ../ for explicit external sources
	SHA256      string  `json:"sha256"`
	Schema      string  `json:"schema,omitempty"`
	Window      *Window `json:"window,omitempty"`
	Query       string  `json:"query,omitempty"`
	JSONPointer string  `json:"jsonPointer,omitempty"`
	Units       string  `json:"units,omitempty"`
}

type Finding struct {
	ID        string   `json:"id"`
	Category  string   `json:"category"` // workload, performance, measurement, capture, resources
	Status    string   `json:"status"`   // observed, inconclusive
	Statement string   `json:"statement"`
	Evidence  []string `json:"evidence"`
	Scope     string   `json:"scope"`
}

// Failure describes a failed client workload gate, not service availability or
// whether an ambiguous write was applied by the server.
type Failure struct {
	Command            string             `json:"command"`
	DeploymentUID      string             `json:"deploymentUID,omitempty"`
	Stage              string             `json:"stage"`
	Operation          string             `json:"operation"`
	Status             string             `json:"status"`         // client_timeout, error
	ElapsedSeconds     float64            `json:"elapsedSeconds"` // zero when unavailable
	DeadlineSeconds    *float64           `json:"deadlineSeconds"`
	ExpectedSeconds    *float64           `json:"expectedSeconds,omitempty"`
	CompletionObserved bool               `json:"completionObserved"`
	ElapsedKind        string             `json:"elapsedKind"`        // time_to_timeout, time_until_error, completed
	ApplicationOutcome string             `json:"applicationOutcome"` // unknown without linked server evidence
	Recovery           Recovery           `json:"recovery"`
	EvidenceRefs       []string           `json:"evidenceRefs"`
	ServerObservation  *ServerObservation `json:"serverObservation,omitempty"`
}

// ServerObservation is reviewer-correlated metadata backed by source byte hashes,
// not independently extracted audit facts or proof of client receipt.
type ServerObservation struct {
	ResponseCode    int       `json:"responseCode"`
	DurationSeconds float64   `json:"durationSeconds"`
	ReceivedAt      time.Time `json:"receivedAt"`
	CompletedAt     time.Time `json:"completedAt"`
	AuditID         string    `json:"auditID"`
	Correlation     string    `json:"correlation"`
	Provenance      string    `json:"provenance"`
	SourceLine      *int64    `json:"sourceLine,omitempty"`
	EvidenceRefs    []string  `json:"evidenceRefs"`
}

type Recovery struct {
	Status               string  `json:"status"` // observed, unknown
	ElapsedSeconds       float64 `json:"elapsedSeconds"`
	ZeroReplicasObserved bool    `json:"zeroReplicasObserved"`
	NoOwnedPodsObserved  bool    `json:"noOwnedPodsObserved"`
	Scope                string  `json:"scope"`
}

type Provenance struct {
	EvidenceRefs []string `json:"evidenceRefs"`
	JSONPointers []string `json:"jsonPointers"`
	Formula      string   `json:"formula,omitempty"`
}

type DataPoint struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Value             float64    `json:"value"`
	Unit              string     `json:"unit"`
	Statistic         string     `json:"statistic"`
	Window            Window     `json:"window"`
	Population        string     `json:"population"`
	Provenance        Provenance `json:"provenance"`
	MeasurementMethod string     `json:"measurementMethod"`
	Limitations       []string   `json:"limitations"`
	Applicability     string     `json:"applicability"`
}

type CarryForward struct {
	ID             string   `json:"id"`
	Action         string   `json:"action"`
	Recommendation string   `json:"recommendation"`
	EvidenceRefs   []string `json:"evidenceRefs"`
	DataPointRefs  []string `json:"dataPointRefs"`
	Scope          string   `json:"scope"`
	BlockedBy      []string `json:"blockedBy"`
	Conditions     []string `json:"conditions"`
	Kind           string   `json:"kind"` // hypothesis, retain, fix_harness
}

// Coverage includes all analyzed series, never the chart preview population.
// Missing grid samples do not distinguish series churn from recorder failure.
type Coverage struct {
	EvidenceRefs     []string `json:"evidenceRefs"`
	Query            string   `json:"query"`
	Status           string   `json:"status"`
	Window           Window   `json:"window"`
	SampleWindow     Window   `json:"sampleWindow"`
	Series           int      `json:"series"`
	Samples          int64    `json:"samples"`
	ValidSamples     int64    `json:"validSamples"`
	InvalidSamples   int64    `json:"invalidSamples"`
	Expected         int64    `json:"expected"`
	MissingApprox    int64    `json:"missingApprox"`
	IncompleteSeries int      `json:"incompleteSeries"`
	Fraction         *float64 `json:"fraction"`
}

// Decode permits additive fields but rejects unsupported schema/analysis versions.
// It does not authenticate evidence or turn imported assessments into qualification.
func Decode(data []byte) (Result, error) {
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("invalid assessment JSON")
	}
	if r.SchemaVersion != SchemaVersion || (r.AnalysisVersion != "v1" && r.AnalysisVersion != AnalysisVersion) {
		return Result{}, fmt.Errorf("unsupported assessment schema or analysis version")
	}
	if r.GeneratedAt.IsZero() || r.Status.Qualification != "not_assessed" ||
		(r.Status.Execution != "" && r.Status.Execution != "unknown" && r.Status.Execution != "completed" && r.Status.Execution != "aborted" && r.Status.Execution != "harness_error") ||
		(r.Status.Experiment != "" && r.Status.Experiment != "unknown" && r.Status.Experiment != "within_expectations" && r.Status.Experiment != "degraded" && r.Status.Experiment != "violations") ||
		(r.Status.Performance != "" && r.Status.Performance != "unknown" && r.Status.Performance != "within_expectations" && r.Status.Performance != "degraded") ||
		(r.Status.Workload != "pass" && r.Status.Workload != "fail" && r.Status.Workload != "incomplete") ||
		(r.Status.Capture != "complete" && r.Status.Capture != "incomplete" && r.Status.Capture != "unknown") {
		return Result{}, fmt.Errorf("invalid assessment status or generation time")
	}
	return r, nil
}

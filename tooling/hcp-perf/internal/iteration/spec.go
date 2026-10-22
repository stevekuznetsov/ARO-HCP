// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package iteration runs a bounded, typed lifecycle using detached workers.
// CLI handshake: Open(root), Close(), Submit(ctx,id,spec), Status(ctx,id),
// List(ctx), Stop(ctx,id), Reconcile(ctx,id), Serve(ctx,interval), and
// Work(ctx,attemptToken). The executable must route
// "iteration worker --root ROOT --attempt TOKEN" to Work.
// Submit persists only; Reconcile/Serve dispatch. Close never stops workers.
package iteration

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/cost"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/performance"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/provision"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/retention"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

const ControllerName = "iteration"
const Version = "v1"

type Kind string

const (
	SizingResolve          Kind = "sizing-resolve"
	CostEstimate           Kind = "cost-estimate"
	RPCreateCluster        Kind = "rp-create-cluster"
	RPUpdateClusterImage   Kind = "rp-update-cluster-image"
	RPCreateNodePool       Kind = "rp-create-node-pool"
	RPUpdateNodePool       Kind = "rp-update-node-pool"
	RPInspect              Kind = "rp-inspect"
	RPWait                 Kind = "rp-wait"
	RPDelete               Kind = "rp-delete"
	MetricsExport          Kind = "metrics-export"
	RPAdminCredential      Kind = "rp-admin-credential"
	CandidateApply         Kind = "candidate-apply"
	CandidateUpdate        Kind = "candidate-update"
	CatalogPin             Kind = "catalog-pin"
	StartupObserve         Kind = "startup-observe"
	StartupObserveExisting Kind = "startup-observe-existing"
	RetentionRefresh       Kind = "retention-refresh"
	PerformanceRun         Kind = "performance"
	PerformanceCleanup     Kind = "performance-cleanup"
)

type Recovery string

const (
	ReadOnly        Recovery = "read-only"
	RemoteMutation  Recovery = "remote-mutation"
	Measurement     Recovery = "measurement"
	GuardedMutation Recovery = "guarded-mutation"
)

type Milestone string

const (
	Started   Milestone = "started"
	Completed Milestone = "completed"
)

type Environment struct {
	ID          string `json:"id"`
	Incarnation string `json:"incarnation"`
}

type Dependency struct {
	Action    string    `json:"action"`
	Milestone Milestone `json:"milestone"`
}

// All executable inputs are inline typed JSON, frozen together in spec.json.
// Credential contents must not be embedded. External evidence is not frozen.
type Spec struct {
	Version     string      `json:"version"`
	Environment Environment `json:"environment"`
	Actions     []Action    `json:"actions"`
}

type Action struct {
	CatalogPin          *startup.CatalogPin      `json:"catalogPin,omitempty"`
	ID                  string                   `json:"id"`
	Kind                Kind                     `json:"kind"`
	Recovery            Recovery                 `json:"recovery"`
	DependsOn           []Dependency             `json:"dependsOn,omitempty"`
	Timeout             time.Duration            `json:"timeoutNanoseconds"`
	MaxAttempts         int                      `json:"maxAttempts"`
	Sizing              *SizingParameters        `json:"sizing,omitempty"`
	Cost                *cost.Spec               `json:"cost,omitempty"`
	RP                  *RPParameters            `json:"rp,omitempty"`
	Metrics             *MetricsParameters       `json:"metrics,omitempty"`
	Candidate           *startup.Candidate       `json:"candidate,omitempty"`
	Startup             *startup.Spec            `json:"startup,omitempty"`
	ObservationIdentity *startup.Identity        `json:"observationIdentity,omitempty"`
	Retention           *retention.NativeSpec    `json:"retention,omitempty"`
	Performance         *performance.Spec        `json:"performance,omitempty"`
	PerformanceCleanup  *performance.CleanupSpec `json:"performanceCleanup,omitempty"`
}

type SizingParameters struct {
	Profile            sizing.Profile `json:"profile"`
	HyperShiftDefaults *sizing.Budget `json:"hyperShiftDefaults,omitempty"`
}

type RPParameters struct {
	Config               provision.Config   `json:"config"`
	Resource             provision.Resource `json:"resource"`
	Payload              json.RawMessage    `json:"payload,omitempty"`
	OperationURL         string             `json:"operationURL,omitempty"`
	ConfirmDelete        bool               `json:"confirmDelete,omitempty"`
	CredentialPath       string             `json:"credentialPath,omitempty"`
	ExpectedResource     json.RawMessage    `json:"expectedResource,omitempty"`
	Replicas             *int               `json:"replicas,omitempty"`
	Image                string             `json:"image,omitempty"`
	ManagementKubeconfig string             `json:"managementKubeconfig,omitempty"`
	AllowExecCredential  bool               `json:"allowExecCredential,omitempty"`
	TargetIdentity       *startup.Identity  `json:"targetIdentity,omitempty"`
}

type MetricsParameters struct {
	Endpoint string          `json:"endpoint"`
	Queries  []metrics.Query `json:"queries"`
	Start    time.Time       `json:"start"`
	End      time.Time       `json:"end"`
	Step     time.Duration   `json:"stepNanoseconds"`
}

type State string

const (
	Prepared   State = "PREPARED"
	Dispatched State = "DISPATCHED"
	Running    State = "RUNNING"
	Succeeded  State = "SUCCEEDED"
	Failed     State = "FAILED"
	Blocked    State = "BLOCKED"
	Stopped    State = "STOPPED"
)

type ProcessIdentity struct {
	PID        int    `json:"pid"`
	BootID     string `json:"bootID"`
	StartTicks string `json:"startTicks"`
}

type Attempt struct {
	// Only explicit startup-observer replacements can override the run binary.
	BinarySHA256 string               `json:"binarySHA256,omitempty"`
	Observation  *ObservationRevision `json:"observation,omitempty"`
	Token        string               `json:"token"`
	State        State                `json:"state"`
	Process      ProcessIdentity      `json:"process"`
	ResumeURL    string               `json:"resumeURL,omitempty"`
}

type ObservationRevision struct {
	ImageBindings   []ObservationImageBinding `json:"imageBindings,omitempty"`
	RearmedActions  []string                  `json:"rearmedActions,omitempty"`
	PreviousAttempt string                    `json:"previousAttempt"`
	Reason          string                    `json:"reason"`
	RequestedAt     time.Time                 `json:"requestedAt"`
	Identity        startup.Identity          `json:"identity"`
}

// ObservationImageBinding revises only digest expectations, never live images,
// resource allocations, required replicas, or the candidate configuration.
type ObservationImageBinding struct {
	Kind          string `json:"kind"`
	Component     string `json:"component"`
	Container     string `json:"container"`
	Init          bool   `json:"init,omitempty"`
	PreviousImage string `json:"previousImage"`
	Image         string `json:"image"`
}

type Binding struct {
	ResourceID   string `json:"resourceID,omitempty"`
	OperationURL string `json:"operationURL,omitempty"`
}

type ActionStatus struct {
	StartupProgress        *startup.Status        `json:"startupProgress,omitempty"`
	WorkerIdentityVerified bool                   `json:"workerIdentityVerified"`
	RetentionVerification  *RetentionVerification `json:"retentionVerification,omitempty"`
	CandidateVerification  *CandidateVerification `json:"candidateVerification,omitempty"`
	ReceiptInvalid         bool                   `json:"receiptInvalid,omitempty"`
	ID                     string                 `json:"id"`
	State                  State                  `json:"state"`
	Attempts               []Attempt              `json:"attempts"`
	Binding                Binding                `json:"binding"`
	Message                string                 `json:"message,omitempty"`
	Armed                  bool                   `json:"armed,omitempty"`
	StartupIdentity        *startup.Identity      `json:"startupIdentity,omitempty"`
}

type RetentionVerification struct {
	AttemptToken string    `json:"attemptToken"`
	CreatedAt    string    `json:"createdAt"`
	VerifiedAt   time.Time `json:"verifiedAt"`
}

type CandidateVerification struct {
	AttemptToken    string    `json:"attemptToken"`
	SpecSHA256      string    `json:"specSHA256"`
	Name            string    `json:"name"`
	UID             string    `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
	VerifiedAt      time.Time `json:"verifiedAt"`
}

type Run struct {
	LastReconciledAt           *time.Time       `json:"lastReconciledAt,omitempty"`
	ReconcileState             string           `json:"reconcileState,omitempty"`
	ControllerProcess          *ProcessIdentity `json:"controllerProcess,omitempty"`
	ControllerIdentityVerified bool             `json:"controllerIdentityVerified"`
	ID                         string           `json:"id"`
	Environment                Environment      `json:"environment"`
	State                      State            `json:"state"`
	StopRequested              bool             `json:"stopRequested"`
	SpecSHA256                 string           `json:"specSHA256"`
	BinarySHA256               string           `json:"binarySHA256"`
	Actions                    []ActionStatus   `json:"actions"`
}

// Executor is injectable for context/cancellation tests. outputDir exists;
// adapters create their own exclusive artifact subdirectories within it.
type Executor interface {
	Execute(ctx context.Context, action Action, outputDir, resumeURL string) error
}

// ProcessFactory starts only the supplied frozen executable. It must detach
// from the controller and must not tie process lifetime to a controller context.
type ProcessFactory func(binary, root, attemptToken string) error

type Options struct {
	BinaryPath     string
	ProcessFactory ProcessFactory
	Executor       Executor
	HTTPClient     *http.Client
}

// NewController(root string, options Options) (*Controller, error) provides
// test seams. Production callers normally use Open(root string).

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

package workload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
)

const RolloutIDAnnotation = "hcp-perf.azure.com/rollout-id"

type TurnoverSpec struct {
	Version         string `json:"version"`
	GuestNodes      int64  `json:"guestNodes"`
	Applications    int64  `json:"applications"`
	Seed            int64  `json:"seed"`
	NamespacePrefix string `json:"namespacePrefix"`
	// Zero selects the canonical two hours. Explicit shorter windows truncate
	// admission, never accelerate the configured schedule.
	DurationSeconds int64 `json:"durationSeconds"`
	// Zero selects hourly turnover, independently of population and concurrency.
	// Explicit periods must be in [1,86400] seconds.
	TurnoverPeriodSeconds int64 `json:"turnoverPeriodSeconds,omitempty"`
	RecoverySeconds       int64 `json:"recoverySeconds"` // 0..600; admits no new work.
	MaxOutstanding        int   `json:"maxOutstanding"`  // Required, 1..100.
}

// NamespaceClientFactory must return the identity for this namespace, not an
// administrator fallback. Loading credentials is the caller's responsibility;
// the factory must be local/nonblocking and must not mint credentials.
type NamespaceClientFactory func(namespace string) (kubernetes.Interface, error)

type TurnoverOptions struct {
	Factory   NamespaceClientFactory
	OutputDir string // Must not exist; its parent must exist. Contains private evidence.
	// Nil uses experiment.RolloutsForPeriod. A supplied schedule retains its exact
	// offsets, bounded by the period's number of rounds within two hours and the
	// global schedule cap. It is never rescaled to fit the period or duration.
	// Both schedules exclude offsets >= DurationSeconds from intents and counts;
	// recovery does not extend the scheduled workload window.
	Schedule []experiment.Rollout
	// Now and Wait must be supplied together, or omitted for the real clock.
	Now  func() time.Time
	Wait func(context.Context, time.Duration) error
	// Zero selects 5s polling / 30s requests. Requests are also bounded by the
	// remaining run window. Supplied clients must honor request contexts.
	PollInterval   time.Duration
	RequestTimeout time.Duration
}

// Attempts counts typed Update calls, not transport/internal client retries.
// No HTTP/APF receipt is inferred from these application-level observations.
type TurnoverIntent struct {
	Index         int                `json:"index"`
	Intent        experiment.Rollout `json:"intent"`
	Namespace     string             `json:"namespace"`
	Deployment    string             `json:"deployment"`
	RolloutID     string             `json:"rolloutID"`
	State         string             `json:"state"` // pending, accepted, completed, failed
	ScheduledAt   time.Time          `json:"scheduledAt"`
	AdmittedAt    time.Time          `json:"admittedAt"`
	AttemptedAt   time.Time          `json:"attemptedAt"`
	AcceptedAt    time.Time          `json:"acceptedAt"`
	CompletedAt   time.Time          `json:"completedAt"`
	FinishedAt    time.Time          `json:"finishedAt"`
	Attempts      int                `json:"attempts"`
	ReadAttempts  int                `json:"readAttempts"`
	DeploymentUID types.UID          `json:"deploymentUID"`
	Generation    int64              `json:"generation"` // Exact successful Update response.
	Last          Snapshot           `json:"last"`
	Error         string             `json:"error,omitempty"`
}

type TurnoverResult struct {
	Version         string           `json:"version"`
	RunID           string           `json:"runID"`
	Spec            TurnoverSpec     `json:"spec"`
	StartedAt       time.Time        `json:"startedAt"`
	AdmissionEnd    time.Time        `json:"admissionEnd"`
	FinishedAt      time.Time        `json:"finishedAt"`
	Scheduled       int              `json:"scheduled"`
	Admitted        int              `json:"admitted"`
	Accepted        int              `json:"accepted"`
	Completed       int              `json:"completed"`
	Failed          int              `json:"failed"`
	NotStarted      int              `json:"notStarted"`
	Backlog         int              `json:"backlog"` // Due, unadmitted intents, including blocked apps.
	Outstanding     int              `json:"outstanding"`
	PeakOutstanding int              `json:"peakOutstanding"`
	Intents         []TurnoverIntent `json:"intents"`
	Error           string           `json:"error,omitempty"`
}

// TurnoverFailure reports failed workload intents, not evidence/client setup
// failure. Other joined errors must still be treated as unsafe to continue.
type TurnoverFailure struct{ Failed int }

func (e *TurnoverFailure) Error() string {
	return fmt.Sprintf("%d turnover intents failed; see evidence", e.Failed)
}

type TurnoverEvent struct {
	Sequence    int64           `json:"sequence"`
	At          time.Time       `json:"at"`
	Type        string          `json:"type"`
	Intent      *TurnoverIntent `json:"intent,omitempty"`
	Operation   string          `json:"operation,omitempty"`
	Error       string          `json:"error,omitempty"`
	Backlog     int             `json:"backlog"`
	Outstanding int             `json:"outstanding"`
}

// TurnoverApplication is the seeding contract: 20 applications per namespace,
// with zero-based, global Deployment indices (not namespace-local indices).
func TurnoverApplication(prefix string, application int64) (namespace, deployment string) {
	return fmt.Sprintf("%s-%d", prefix, application/20), fmt.Sprintf("app-%06d", application)
}

// RunTurnover drives existing five-replica RollingUpdate Deployments, serially
// issuing bounded API calls for at most MaxOutstanding apps. It never creates,
// repairs, deletes, rolls back, or resumes objects. Callers must exclude other
// writers. Failed apps remain blocked, including after ambiguous write errors.
// events.ndjson is append-only and synced before writes and after observations;
// result.json is a final snapshot, not proof of completed throughput. A nil
// error can still have unadmitted backlog; callers must inspect the result.
// MaxOutstanding is never raised to meet cadence. Generator-limited backlog
// alone cannot establish an APF capacity limit.
func RunTurnover(ctx context.Context, spec TurnoverSpec, opts TurnoverOptions) (result TurnoverResult, err error) {
	if spec.Version != experiment.Version || spec.GuestNodes < 0 || spec.GuestNodes > experiment.MaxGuestNodes {
		return result, fmt.Errorf("require version v1 and guestNodes in [0,1000]")
	}
	if spec.Applications < 0 || spec.Applications > experiment.MaxApplications {
		return result, fmt.Errorf("require applications in [0,40000]")
	}
	if len(validation.IsDNS1123Label(spec.NamespacePrefix)) != 0 || len(validation.IsDNS1123Label(spec.NamespacePrefix+"-1999")) != 0 {
		return result, fmt.Errorf("namespacePrefix must be a DNS label with room for -1999")
	}
	if spec.DurationSeconds == 0 {
		spec.DurationSeconds = 7200
	}
	if spec.TurnoverPeriodSeconds == 0 {
		spec.TurnoverPeriodSeconds = 3600
	}
	if spec.TurnoverPeriodSeconds < 1 || spec.TurnoverPeriodSeconds > 86400 {
		return result, fmt.Errorf("require turnoverPeriodSeconds 1..86400")
	}
	if spec.DurationSeconds < 1 || spec.DurationSeconds > 7200 || spec.RecoverySeconds < 0 || spec.RecoverySeconds > 600 || spec.MaxOutstanding < 1 || spec.MaxOutstanding > 100 {
		return result, fmt.Errorf("require durationSeconds 1..7200, recoverySeconds 0..600, maxOutstanding 1..100")
	}
	if opts.Factory == nil || opts.OutputDir == "" || (opts.Now == nil) != (opts.Wait == nil) {
		return result, fmt.Errorf("factory, new outputDir and paired now/wait are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
		opts.Wait = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 30 * time.Second
	}
	if opts.PollInterval <= 0 || opts.PollInterval > 30*time.Second || opts.RequestTimeout <= 0 || opts.RequestTimeout > 30*time.Second {
		return result, fmt.Errorf("pollInterval and requestTimeout must be positive and at most 30s")
	}
	schedule := opts.Schedule
	if schedule == nil {
		schedule, err = experiment.RolloutsForPeriod(spec.Applications, spec.Seed, time.Duration(spec.DurationSeconds)*time.Second, time.Duration(spec.TurnoverPeriodSeconds)*time.Second)
		if err != nil {
			return result, err
		}
	}
	maxRounds := (7200 + spec.TurnoverPeriodSeconds - 1) / spec.TurnoverPeriodSeconds
	if len(schedule) > experiment.MaxScheduledRollouts || int64(len(schedule)) > maxRounds*spec.Applications {
		return result, fmt.Errorf("schedule exceeds %d rollouts or %d rounds per application", experiment.MaxScheduledRollouts, maxRounds)
	}
	seen := make(map[[2]int64]bool, len(schedule))
	for i, intent := range schedule {
		key := [2]int64{intent.Application, int64(intent.Round)}
		if intent.Application < 0 || intent.Application >= spec.Applications || intent.Round < 1 || int64(intent.Round) > maxRounds || intent.Offset < 0 || intent.Offset >= 2*time.Hour || (i > 0 && intent.Offset < schedule[i-1].Offset) || seen[key] {
			return result, fmt.Errorf("invalid, duplicate, or out-of-order schedule intent %d", i)
		}
		seen[key] = true
	}
	for i, intent := range schedule {
		if intent.Offset >= time.Duration(spec.DurationSeconds)*time.Second {
			schedule = schedule[:i]
			break
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return result, err
	}
	if err := os.Mkdir(opts.OutputDir, 0700); err != nil {
		return result, err
	}
	file, err := os.OpenFile(filepath.Join(opts.OutputDir, "events.ndjson"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0600)
	if err != nil {
		return result, err
	}
	result = TurnoverResult{Version: experiment.Version, RunID: hex.EncodeToString(nonce[:]), Spec: spec, StartedAt: opts.Now(), Scheduled: len(schedule), NotStarted: len(schedule), Intents: make([]TurnoverIntent, len(schedule))}
	result.AdmissionEnd = result.StartedAt.Add(time.Duration(spec.DurationSeconds) * time.Second)
	end := result.AdmissionEnd.Add(time.Duration(spec.RecoverySeconds) * time.Second)
	sequence := int64(0)
	emit := func(kind string, intent *TurnoverIntent, operation string, cause error) error {
		sequence++
		event := TurnoverEvent{Sequence: sequence, At: opts.Now(), Type: kind, Intent: intent, Operation: operation, Backlog: result.Backlog, Outstanding: result.Outstanding}
		if cause != nil {
			event.Error = cause.Error()
		}
		if err := json.NewEncoder(file).Encode(event); err != nil {
			return err
		}
		return file.Sync()
	}
	for i, intent := range schedule {
		ns, name := TurnoverApplication(spec.NamespacePrefix, intent.Application)
		result.Intents[i] = TurnoverIntent{Index: i, Intent: intent, Namespace: ns, Deployment: name, RolloutID: fmt.Sprintf("%s-%06d", result.RunID, i), State: "pending", ScheduledAt: result.StartedAt.Add(intent.Offset)}
	}
	defer func() {
		result.FinishedAt = opts.Now()
		result.Backlog = 0
		result.Admitted, result.Accepted, result.Completed, result.Failed, result.NotStarted, result.Outstanding = 0, 0, 0, 0, 0, 0
		for i := range result.Intents {
			r := &result.Intents[i]
			if r.AdmittedAt.IsZero() {
				result.NotStarted++
			} else {
				result.Admitted++
			}
			if !r.AcceptedAt.IsZero() {
				result.Accepted++
			}
			if r.AdmittedAt.IsZero() && !r.ScheduledAt.After(result.FinishedAt) {
				result.Backlog++
			}
			if !r.AdmittedAt.IsZero() && r.State != "completed" && r.State != "failed" {
				r.State, r.Error, r.FinishedAt = "failed", "run stopped before completion", result.FinishedAt
				err = errors.Join(err, emit("failed", r, "stop", errors.New(r.Error)))
			}
			if r.State == "failed" {
				result.Failed++
			} else if r.State == "completed" {
				result.Completed++
			}
		}
		if result.Failed > 0 {
			err = errors.Join(err, &TurnoverFailure{Failed: result.Failed})
		}
		err = errors.Join(err, emit("stopped", nil, "", err), file.Close())
		if err != nil {
			result.Error = err.Error()
		}
		out, openErr := os.OpenFile(filepath.Join(opts.OutputDir, "result.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if openErr != nil {
			err = errors.Join(err, openErr)
			result.Error = err.Error()
			return
		}
		err = errors.Join(err, json.NewEncoder(out).Encode(result), out.Sync(), out.Close())
		if err != nil {
			result.Error = err.Error()
		}
	}()
	for i := range result.Intents {
		if err := emit("pending", &result.Intents[i], "", nil); err != nil {
			return result, err
		}
	}
	// Only due intents enter per-app queues. Each intent enters ready once;
	// blocked apps never cause a scan or API read of the full population.
	queues := make([][]int, int(spec.Applications))
	busy := make([]bool, len(queues))
	ready := make([]int, 0, len(schedule))
	readyCursor, due := 0, 0
	active := make([]*turnoverWork, 0, spec.MaxOutstanding)
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		now := opts.Now()
		if !now.Before(end) {
			return result, nil
		}
		for due < len(schedule) && !result.Intents[due].ScheduledAt.After(now) {
			app := schedule[due].Application
			if !busy[app] && len(queues[app]) == 0 {
				ready = append(ready, due)
			}
			queues[app] = append(queues[app], due)
			result.Backlog++
			due++
		}
		for readyCursor < len(ready) && len(active) < spec.MaxOutstanding && opts.Now().Before(result.AdmissionEnd) && ctx.Err() == nil {
			i := ready[readyCursor]
			readyCursor++
			r := &result.Intents[i]
			app := r.Intent.Application
			queues[app] = queues[app][1:]
			busy[app] = true
			r.AdmittedAt = opts.Now()
			result.Admitted++
			result.NotStarted--
			result.Backlog--
			result.Outstanding++
			result.PeakOutstanding = max(result.PeakOutstanding, result.Outstanding)
			w := &turnoverWork{record: r, sets: map[types.UID]turnoverSet{}}
			active = append(active, w)
			if err := emit("admitted", r, "", nil); err != nil {
				return result, err
			}
		}
		remaining := active[:0]
		for _, w := range active {
			if ctx.Err() != nil || !opts.Now().Before(end) {
				return result, ctx.Err()
			}
			if opts.Now().Before(w.next) {
				remaining = append(remaining, w)
				continue
			}
			wasAccepted := !w.record.AcceptedAt.IsZero()
			terminal, stepErr := w.step(ctx, opts, end, emit)
			if stepErr != nil { // Evidence failure: no more mutations are safe.
				return result, stepErr
			}
			if !wasAccepted && !w.record.AcceptedAt.IsZero() {
				result.Accepted++
			}
			if !terminal {
				remaining = append(remaining, w)
				continue
			}
			result.Outstanding--
			if w.record.State == "completed" {
				result.Completed++
				app := w.record.Intent.Application
				busy[app] = false
				if len(queues[app]) > 0 {
					ready = append(ready, queues[app][0])
				}
			} else {
				result.Failed++
			}
		}
		active = remaining
		if err := emit("backlog", nil, "", nil); err != nil {
			return result, err
		}
		now = opts.Now()
		wake := end
		if now.Before(result.AdmissionEnd) {
			// Hold the steady window even with no applications or remaining work.
			wake = result.AdmissionEnd
			if due < len(schedule) {
				wake = minTime(wake, result.Intents[due].ScheduledAt)
			}
			if readyCursor < len(ready) && len(active) < spec.MaxOutstanding {
				continue
			}
		}
		for _, w := range active {
			wake = minTime(wake, w.next)
		}
		if !wake.After(now) {
			continue
		}
		if err := opts.Wait(ctx, wake.Sub(now)); err != nil {
			return result, err
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

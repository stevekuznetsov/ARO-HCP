// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iteration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/provision"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

func (c *Controller) Stop(ctx context.Context, id string) error {
	return c.update(ctx, id, func(run *Run) error {
		run.StopRequested = true
		return nil
	})
}

func (c *Controller) Serve(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("reconcile interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		runs, err := c.List(ctx)
		if err != nil {
			return err
		}
		for _, run := range runs {
			if err := c.Reconcile(ctx, run.ID); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// A damaged run must not stop observation of other environments.
				// Do not log arbitrary artifact/transport error text.
				slog.ErrorContext(ctx, "iteration reconciliation failed", "controller", ControllerName, "run", run.ID)
				if persistErr := c.update(ctx, run.ID, func(current *Run) error {
					current.State = Blocked
					return nil
				}); persistErr != nil {
					return fmt.Errorf("persist run reconciliation failure: %w", persistErr)
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Controller) Reconcile(ctx context.Context, id string) (retErr error) {
	if !safeName.MatchString(id) {
		return errors.New("invalid run ID")
	}
	lock, err := acquireLock(filepath.Join(c.root, id, "reconcile.lock"))
	if errors.Is(err, errLocked) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock reconciliation: %w", err)
	}
	defer lock.Close()
	run, err := c.Status(ctx, id)
	if err != nil {
		return err
	}
	if err := c.saveReconcileHeartbeat(run, "running"); err != nil {
		return err
	}
	defer func() {
		state := "completed"
		if retErr != nil {
			state = "error"
		}
		retErr = errors.Join(retErr, c.saveReconcileHeartbeat(run, state))
	}()
	if run.State == Succeeded || run.State == Failed || run.State == Stopped {
		return nil
	}
	spec, err := c.loadSpec(run)
	if err != nil {
		return fmt.Errorf("load run inputs: %w", err)
	}
	binary := filepath.Join(c.root, id, "worker")
	if err := verifyBinary(binary, run.BinarySHA256); err != nil {
		return err
	}
	for i, action := range spec.Actions {
		if action.Kind == CatalogPin {
			if err := c.recheckCatalogPin(ctx, id, i, action); err != nil {
				return err
			}
		}
		if action.Kind == PerformanceCleanup {
			if err := c.recheckCleanup(ctx, id, i, action); err != nil {
				return err
			}
		}
		if action.Kind == RetentionRefresh {
			if err := c.recheckRetention(ctx, id, i, action); err != nil {
				return err
			}
		}
		if action.Kind == CandidateApply {
			if err := c.recheckCandidate(ctx, id, i, action); err != nil {
				return err
			}
		}
		if action.Kind == PerformanceRun {
			if err := c.recheckPerformance(ctx, id, i, action); err != nil {
				return err
			}
		}
		var dispatch string
		var attemptLock *os.File
		err := c.update(ctx, id, func(current *Run) error {
			defer summarize(current)
			status := &current.Actions[i]
			if (action.Kind == StartupObserve || action.Kind == StartupObserveExisting) && len(status.Attempts) > 0 {
				attempt := status.Attempts[len(status.Attempts)-1]
				var identity startup.Identity
				if c.readProgressFile(filepath.Join(id, attempt.Token, "identity.json"), &identity) == "available" && identity.ResourceID == action.Startup.ResourceID {
					status.StartupIdentity = &identity
				}
			}
			if err := c.scanBindings(id, action, status); err != nil {
				blockReceipt(status)
			}
			if status.ReceiptInvalid {
				if len(status.Attempts) > 0 {
					attempt := &status.Attempts[len(status.Attempts)-1]
					var lockErr error
					attemptLock, lockErr = acquireLock(c.attemptDir(id, attempt.Token) + ".lock")
					if lockErr == nil {
						attempt.State = Blocked
					} else if !errors.Is(lockErr, errLocked) {
						return lockErr
					}
				}
				return nil
			}
			if len(status.Attempts) > 0 {
				attempt := &status.Attempts[len(status.Attempts)-1]
				if attempt.State == Running || attempt.State == Dispatched {
					var lockErr error
					attemptLock, lockErr = acquireLock(c.attemptDir(id, attempt.Token) + ".lock")
					if errors.Is(lockErr, errLocked) {
						return nil
					}
					if lockErr != nil {
						return lockErr
					}
					if attempt.State == Dispatched {
						ready, failed := initialEligibility(*current, action)
						recovery := attempt.Observation != nil || attempt.ResumeURL != "" || (action.Recovery == ReadOnly && len(status.Attempts) > 1)
						if !current.StopRequested && (recovery || ready) {
							dispatch = attempt.Token
							return nil
						}
						if !current.StopRequested && !failed {
							return nil
						}
						attempt.State, status.State = Stopped, Stopped
						if attempt.ResumeURL != "" {
							attempt.State, status.State = Blocked, Blocked
							status.Message = "stopped observation does not cancel the accepted remote mutation"
						}
					} else {
						attempt.State = Blocked
						status.State = Blocked
						status.Message = "worker lost; remote outcome or measurement is uncertain"
						if action.Recovery == ReadOnly {
							status.State = Prepared
						}
					}
				}
			}
			if current.StopRequested {
				if (action.Kind == StartupObserve || action.Kind == StartupObserveExisting) && status.State == Blocked && len(status.Attempts) > 0 {
					attempt := &status.Attempts[len(status.Attempts)-1]
					if !sameProcess(attempt.Process) {
						lock, err := acquireLock(c.attemptDir(id, attempt.Token) + ".lock")
						if err == nil {
							lock.Close()
							attempt.State, status.State, status.Armed = Stopped, Stopped, false
							status.Message = "explicit stop finalized exited read-only observer; evidence remains interrupted, not qualified"
						} else if !errors.Is(err, errLocked) {
							return err
						}
					}
				}
				if status.State == Prepared {
					status.State = Stopped
				}
				return nil
			}
			resumeURL := ""
			if status.State == Blocked && action.Recovery == RemoteMutation && status.Binding.OperationURL != "" {
				resumeURL = status.Binding.OperationURL
			} else if status.State != Prepared {
				return nil
			}
			if len(status.Attempts) >= action.MaxAttempts {
				if action.Recovery == ReadOnly {
					status.State = Failed
				}
				status.Message = "attempt limit reached"
				return nil
			}
			if resumeURL == "" && !(action.Recovery == ReadOnly && len(status.Attempts) > 0) {
				ready, failed := initialEligibility(*current, action)
				if failed {
					status.State = Stopped
					status.Message = "initial action suppressed after sibling failure"
				}
				if !ready {
					return nil
				}
			}
			token := fmt.Sprintf("%x", randomToken())
			if err := os.Mkdir(c.attemptDir(id, token), 0700); err != nil {
				return err
			}
			if err := syncDirectory(filepath.Join(c.root, id)); err != nil {
				return err
			}
			status.Attempts = append(status.Attempts, Attempt{Token: token, State: Dispatched, ResumeURL: resumeURL})
			status.State, status.Message = Dispatched, ""
			status.Armed = false
			dispatch = token
			return nil
		})
		// Intent is durable before exec. A crash here is recovered by dispatching
		// the same token; flock plus the worker CAS permits only one claimant.
		if attemptLock != nil {
			attemptLock.Close()
		}
		if err == nil && dispatch != "" {
			current, readErr := c.Status(ctx, id)
			if readErr != nil {
				return readErr
			}
			attempt := current.Actions[i].Attempts[len(current.Actions[i].Attempts)-1]
			workerBinary := binary
			if attempt.Observation != nil {
				workerBinary = filepath.Join(c.attemptDir(id, dispatch), "worker")
				if err := verifyBinary(workerBinary, attempt.BinarySHA256); err != nil {
					return err
				}
			}
			err = c.options.ProcessFactory(workerBinary, c.root, dispatch)
		}
		if err != nil {
			return fmt.Errorf("reconcile action %q: %w", action.ID, err)
		}
	}
	return c.update(ctx, id, func(run *Run) error { summarize(run); return nil })
}

// The attempt flock prevents any original worker from completing an in-flight
// request during verification. Network reads stay outside SQLite transactions.
func (c *Controller) recheckCandidate(ctx context.Context, id string, index int, action Action) error {
	run, err := c.Status(ctx, id)
	if err != nil {
		return err
	}
	status := run.Actions[index]
	if status.State != Blocked || len(status.Attempts) == 0 {
		return nil
	}
	attempt := status.Attempts[len(status.Attempts)-1]
	lock, err := acquireLock(c.attemptDir(id, attempt.Token) + ".lock")
	if errors.Is(err, errLocked) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock candidate verification: %w", err)
	}
	defer lock.Close()
	// Flock absence alone is insufficient if a still-live process dropped it.
	if sameProcess(attempt.Process) {
		return nil
	}
	clients, err := startup.NewClients(action.Candidate.Kubeconfig, action.Candidate.AllowExecCredential)
	if err != nil {
		return nil
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := startup.VerifyUnchanged(verifyCtx, clients, *action.Candidate); err != nil {
		return nil
	}
	return c.update(ctx, id, func(current *Run) error {
		status := &current.Actions[index]
		latest := &status.Attempts[len(status.Attempts)-1]
		if status.State != Blocked || latest.Token != attempt.Token || latest.State != attempt.State {
			return errors.New("candidate verification fencing mismatch")
		}
		status.State, latest.State, status.Armed = Failed, Failed, false
		status.Message = "read-only verification proved original candidate UID/resourceVersion/spec unchanged; no update replayed"
		status.CandidateVerification = &CandidateVerification{AttemptToken: attempt.Token, SpecSHA256: current.SpecSHA256, Name: action.Candidate.Name, UID: action.Candidate.UID, ResourceVersion: action.Candidate.ResourceVersion, VerifiedAt: time.Now().UTC()}
		summarize(current)
		return nil
	})
}

func randomToken() []byte {
	// crypto/rand.Read terminates the process on entropy failure in Go 1.25.
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return value
}

func (c *Controller) attemptDir(id, token string) string { return filepath.Join(c.root, id, token) }

// Only initial work is gated by sibling failures and dependency milestones.
// Recovery must remain able to observe an already accepted remote operation.
func initialEligibility(run Run, action Action) (ready, failed bool) {
	for _, other := range run.Actions {
		if other.ID != action.ID && (other.State == Failed || other.State == Blocked || other.State == Stopped) {
			return false, true
		}
	}
	for _, dependency := range action.DependsOn {
		for _, other := range run.Actions {
			if other.ID == dependency.Action {
				if dependency.Milestone == Completed && other.State != Succeeded {
					return false, false
				}
				if dependency.Milestone == Started && (!other.Armed || (other.State != Running && other.State != Succeeded)) {
					if other.State == Succeeded {
						return false, true
					}
					return false, false
				}
			}
		}
	}
	return true, false
}

func blockReceipt(status *ActionStatus) {
	status.ReceiptInvalid = true
	status.State = Blocked
	status.Message = "RP receipt invalid; remote outcome requires operator investigation"
}

func (c *Controller) scanBindings(id string, action Action, status *ActionStatus) error {
	if action.RP == nil || action.Kind == RPAdminCredential || status.ReceiptInvalid {
		return nil
	}
	client, err := provision.New(action.RP.Config, nil)
	if err != nil {
		return fmt.Errorf("configure receipt validation: %w", err)
	}
	binding := status.Binding
	for _, attempt := range status.Attempts {
		data, err := os.ReadFile(filepath.Join(c.attemptDir(id, attempt.Token), "rp", "operation.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read RP receipt: %w", err)
		}
		var result provision.Result
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("decode RP receipt: %w", err)
		}
		method, payload := action.requestMethod(), action.RP.Payload
		expectedOperation := action.RP.OperationURL
		if attempt.ResumeURL != "" {
			method, payload = http.MethodGet, nil
			expectedOperation = attempt.ResumeURL
			if binding.OperationURL == "" || expectedOperation != binding.OperationURL {
				return errors.New("resume intent does not match immutable operation binding")
			}
		}
		expected, err := client.ValidateActionInputs(action.RP.Resource, method, payload, expectedOperation)
		if err != nil {
			return fmt.Errorf("validate receipt request identity: %w", err)
		}
		if result.Version != expected.Version || result.ResourceID != expected.ResourceID || result.RequestURL != expected.RequestURL || result.Method != expected.Method {
			return errors.New("RP receipt does not match action request identity")
		}
		if binding.ResourceID != "" && binding.ResourceID != result.ResourceID {
			return errors.New("RP receipt changed resource binding")
		}
		binding.ResourceID = result.ResourceID
		if result.OperationURL != "" {
			operation, err := client.ValidateActionInputs(action.RP.Resource, http.MethodGet, nil, result.OperationURL)
			if err != nil || operation.OperationURL != result.OperationURL {
				return errors.New("RP receipt has an unsafe or noncanonical operation URL")
			}
			if binding.OperationURL != "" && binding.OperationURL != result.OperationURL {
				return errors.New("RP receipt changed operation binding")
			}
			binding.OperationURL = result.OperationURL
		}
		if expected.OperationURL != "" && result.OperationURL != expected.OperationURL {
			return errors.New("RP receipt does not match observation intent")
		}
	}
	status.Binding = binding
	return nil
}

func summarize(run *Run) {
	active, blocked, failed, pending, stopped := false, false, false, false, false
	for _, action := range run.Actions {
		switch action.State {
		case Running, Dispatched:
			active = true
		case Blocked:
			blocked = true
		case Failed:
			failed = true
		case Prepared:
			pending = true
		case Stopped:
			stopped = true
		}
	}
	switch {
	case blocked:
		run.State = Blocked
	case active:
		run.State = Running
	case failed:
		run.State = Failed
	case stopped || run.StopRequested:
		run.State = Stopped
	case pending:
		run.State = Prepared
	default:
		run.State = Succeeded
	}
}

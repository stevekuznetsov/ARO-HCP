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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/provision"
)

// ReconcileOperation runs one explicitly requested, bounded observation attempt
// in this process. It is independent of automatic MaxAttempts and never invokes
// the original PUT/PATCH/DELETE. Cancellation journals uncertainty, not remote cancel.
func (c *Controller) ReconcileOperation(ctx context.Context, id, actionID string, timeout time.Duration) error {
	if !safeName.MatchString(id) || !safeName.MatchString(actionID) || timeout <= 0 || timeout > 30*time.Minute {
		return errors.New("operation reconciliation requires safe run/action IDs and timeout in (0,30m]")
	}
	lock, err := acquireLock(filepath.Join(c.root, id, "reconcile.lock"))
	if err != nil {
		return fmt.Errorf("lock operation reconciliation: %w", err)
	}
	defer lock.Close()
	run, err := c.Status(ctx, id)
	if err != nil {
		return err
	}
	spec, err := c.loadSpec(run)
	if err != nil {
		return err
	}
	index := -1
	for i, action := range spec.Actions {
		if action.ID == actionID && action.Recovery == RemoteMutation && (action.Kind == RPCreateCluster || action.Kind == RPCreateNodePool || action.Kind == RPUpdateClusterImage || action.Kind == RPUpdateNodePool || action.Kind == RPDelete) {
			index = i
		}
	}
	if index < 0 {
		return errors.New("operation reconciliation only accepts RP create/update/delete actions")
	}
	status := run.Actions[index]
	if status.State != Blocked && status.State != Running {
		return errors.New("RP action must be blocked or have a lost running observer")
	}
	if len(status.Attempts) == 0 {
		return errors.New("RP action has no dispatched attempt")
	}
	previous := status.Attempts[len(status.Attempts)-1]
	previousLock, err := acquireLock(c.attemptDir(id, previous.Token) + ".lock")
	if err != nil {
		return fmt.Errorf("lock previous operation attempt: %w", err)
	}
	defer previousLock.Close()
	if previous.State == Running && sameProcess(previous.Process) {
		return errors.New("previous operation observer is still live")
	}
	if err := c.scanBindings(id, spec.Actions[index], &status); err != nil {
		persistErr := c.update(ctx, id, func(run *Run) error { blockReceipt(&run.Actions[index]); summarize(run); return nil })
		return errors.Join(errors.New("invalid RP operation receipt; no HTTP request issued"), persistErr)
	}
	if status.ReceiptInvalid || status.Binding.OperationURL == "" {
		return errors.New("operation reconciliation requires a validated accepted operation URL")
	}
	token := fmt.Sprintf("%x", randomToken())
	dir := c.attemptDir(id, token)
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	attemptLock, err := acquireLock(dir + ".lock")
	if err != nil {
		return err
	}
	defer attemptLock.Close()
	identity, err := processIdentity(os.Getpid())
	if err != nil {
		return err
	}
	attempt := Attempt{Token: token, State: Running, Process: identity, ResumeURL: status.Binding.OperationURL}
	audit := struct {
		Attempt         Attempt       `json:"attempt"`
		PreviousAttempt string        `json:"previousAttempt"`
		SpecSHA256      string        `json:"specSHA256"`
		Action          string        `json:"action"`
		RequestedAt     time.Time     `json:"requestedAt"`
		Timeout         time.Duration `json:"timeoutNanoseconds"`
		Reason          string        `json:"reason"`
	}{attempt, previous.Token, run.SpecSHA256, actionID, time.Now().UTC(), timeout, "explicit operator reconciliation of accepted RP operation"}
	data, err := json.Marshal(audit)
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(dir, "operation-check.json"), data, 0400); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Join(c.root, id)); err != nil {
		return err
	}
	if err := c.update(ctx, id, func(current *Run) error {
		status := &current.Actions[index]
		if status.Attempts[len(status.Attempts)-1].Token != previous.Token || (status.State != Blocked && status.State != Running) {
			return errors.New("operation reconciliation claim changed")
		}
		if err := c.scanBindings(id, spec.Actions[index], status); err != nil {
			return err
		}
		if status.ReceiptInvalid || status.Binding.OperationURL != attempt.ResumeURL {
			return errors.New("operation reconciliation binding changed")
		}
		status.Attempts[len(status.Attempts)-1].State = Blocked
		status.Attempts = append(status.Attempts, attempt)
		status.State, status.Message = Running, "explicit observation of accepted RP operation"
		summarize(current)
		return nil
	}); err != nil {
		return err
	}
	// Release run-wide reconciliation while holding the attempt fence: other
	// actions and diagnostics continue, but no duplicate operation check can run.
	lock.Close()
	observeCtx, cancel := context.WithTimeout(ctx, timeout)
	executionErr := (libraryExecutor{client: c.options.HTTPClient}).Execute(observeCtx, spec.Actions[index], dir, attempt.ResumeURL)
	cancel()
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer persistCancel()
	persistErr := c.update(persistCtx, id, func(current *Run) error {
		status := &current.Actions[index]
		latest := &status.Attempts[len(status.Attempts)-1]
		if latest.Token != token || latest.State != Running || latest.Process != identity {
			return errors.New("operation reconciliation completion fence changed")
		}
		if err := c.scanBindings(id, spec.Actions[index], status); err != nil {
			blockReceipt(status)
		}
		status.State = Blocked
		status.Message = "operation observation incomplete; accepted remote outcome remains uncertain"
		var terminal *provision.OperationFailedError
		if !status.ReceiptInvalid {
			if executionErr == nil {
				status.State, status.Message = Succeeded, "accepted operation succeeded and desired resource state verified"
			} else if errors.As(executionErr, &terminal) && terminal.OperationURL == status.Binding.OperationURL {
				status.State, status.Message = Failed, "accepted RP operation terminal: "+terminal.State+"; resource cleanup remains explicit"
			}
		}
		latest.State = status.State
		summarize(current)
		return nil
	})
	if persistErr != nil {
		return persistErr
	}
	var terminal *provision.OperationFailedError
	if executionErr != nil && !errors.As(executionErr, &terminal) {
		return errors.New("operation check incomplete; uncertainty persisted, repeat reconcile-operation when appropriate")
	}
	return nil
}

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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/performance"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/provision"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

var errLocked = errors.New("attempt already locked")
var errNotClaimable = errors.New("attempt is no longer claimable")

func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return f, nil
}

func startWorker(binary, root, token string) error {
	command := exec.Command(binary, "iteration", "worker", "--root", root, "--attempt", token)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start detached iteration worker: %w", err)
	}
	go func() {
		defer utilruntime.HandleCrash()
		_ = command.Wait()
	}()
	return nil
}

func processIdentity(pid int) (ProcessIdentity, error) {
	identity := ProcessIdentity{PID: pid}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return identity, err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return identity, err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return identity, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return identity, errors.New("process is not live")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return identity, err
	}
	identity.BootID, identity.StartTicks = strings.TrimSpace(string(boot)), fields[19]
	return identity, nil
}

// Never signal a PID. Stop is a durable request consumed by the owning worker;
// identity is recorded for diagnostics and fencing, not a kill capability.
func sameProcess(expected ProcessIdentity) bool {
	actual, err := processIdentity(expected.PID)
	return err == nil && expected == actual
}

func (c *Controller) Work(ctx context.Context, token string) error {
	var id string
	if err := c.db.QueryRowContext(ctx, "SELECT run_id FROM attempts WHERE token=?", token).Scan(&id); err != nil {
		return fmt.Errorf("locate worker attempt: %w", err)
	}
	lock, err := acquireLock(c.attemptDir(id, token) + ".lock")
	if errors.Is(err, errLocked) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock worker attempt: %w", err)
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
	expectedHash := run.BinarySHA256
	var observation *ObservationRevision
	for i, status := range run.Actions {
		for _, attempt := range status.Attempts {
			if attempt.Token != token || attempt.Observation == nil {
				continue
			}
			if spec.Actions[i].Kind != StartupObserve && spec.Actions[i].Kind != StartupObserveExisting {
				return errors.New("binary revision is only allowed for startup observation")
			}
			expectedHash, observation = attempt.BinarySHA256, attempt.Observation
		}
	}
	if err := verifyBinary("/proc/self/exe", expectedHash); err != nil {
		return fmt.Errorf("verify executing worker: %w", err)
	}
	identity, err := processIdentity(os.Getpid())
	if err != nil {
		return fmt.Errorf("identify worker process: %w", err)
	}
	index := -1
	resumeURL := ""
	err = c.update(ctx, id, func(current *Run) error {
		for i := range current.Actions {
			status := &current.Actions[i]
			if len(status.Attempts) == 0 {
				continue
			}
			attempt := &status.Attempts[len(status.Attempts)-1]
			if attempt.Token != token {
				continue
			}
			if attempt.State != Dispatched {
				return errNotClaimable
			}
			defer summarize(current)
			action := spec.Actions[i]
			if err := c.scanBindings(id, action, status); err != nil {
				blockReceipt(status)
			}
			if status.ReceiptInvalid {
				attempt.State = Blocked
				return nil
			}
			if attempt.ResumeURL != "" && attempt.ResumeURL != status.Binding.OperationURL {
				blockReceipt(status)
				attempt.State = Blocked
				return nil
			}
			ready, failed := initialEligibility(*current, action)
			recovery := attempt.Observation != nil || attempt.ResumeURL != "" || (action.Recovery == ReadOnly && len(status.Attempts) > 1)
			if current.StopRequested || (!recovery && failed) {
				attempt.State, status.State = Stopped, Stopped
				if attempt.ResumeURL != "" {
					attempt.State, status.State = Blocked, Blocked
				}
				return nil
			}
			if !recovery && !ready {
				return nil
			}
			index, resumeURL = i, attempt.ResumeURL
			attempt.State, attempt.Process = Running, identity
			status.State, current.State = Running, Running
			status.Armed = action.Kind != StartupObserve && action.Kind != StartupObserveExisting && action.Kind != RetentionRefresh
			return nil
		}
		return errNotClaimable
	})
	if errors.Is(err, errNotClaimable) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim worker attempt: %w", err)
	}
	if index < 0 {
		return nil
	}
	action := spec.Actions[index]
	workerCtx, cancel := context.WithTimeout(ctx, action.Timeout)
	if observation != nil {
		workerCtx = context.WithValue(workerCtx, observationContextKey{}, observation)
	}
	workerCtx = context.WithValue(workerCtx, readyContextKey{}, func() error {
		return c.update(workerCtx, id, func(current *Run) error {
			status := &current.Actions[index]
			attempt := status.Attempts[len(status.Attempts)-1]
			if attempt.Token != token || attempt.State != Running || current.StopRequested {
				return errors.New("worker readiness fencing mismatch")
			}
			status.Armed = true
			return nil
		})
	})
	done := make(chan struct{})
	go func() {
		defer utilruntime.HandleCrash()
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				current, err := c.Status(workerCtx, id)
				if err != nil || current.StopRequested {
					cancel()
					return
				}
			}
		}
	}()
	executionErr := c.options.Executor.Execute(workerCtx, action, c.attemptDir(id, token), resumeURL)
	cancel()
	<-done
	// Completion needs its own deadline: stopping the local observer must not
	// prevent journaling uncertainty about an already dispatched remote mutation.
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer persistCancel()
	return c.update(persistCtx, id, func(current *Run) error {
		status := &current.Actions[index]
		attempt := &status.Attempts[len(status.Attempts)-1]
		if attempt.Token != token || attempt.State != Running || attempt.Process != identity || !sameProcess(identity) {
			return errors.New("worker completion fencing mismatch")
		}
		if err := c.scanBindings(id, action, status); err != nil {
			blockReceipt(status)
		}
		status.State = Succeeded
		if status.ReceiptInvalid {
			status.State = Blocked
		} else if executionErr != nil {
			// Do not persist arbitrary transport errors, which can contain secrets.
			status.Message = "action execution failed; inspect private library artifacts"
			status.State = Failed
			if action.Recovery == RemoteMutation || action.Recovery == Measurement || action.Recovery == GuardedMutation {
				status.State = Blocked
			} else if current.StopRequested {
				status.State = Stopped
			}
			var noMutation *startup.NoMutationError
			var assessedFailure *performance.AssessedFailure
			if action.Kind == PerformanceRun && errors.As(executionErr, &assessedFailure) {
				status.State = Failed
				status.Message = "performance workload requirements violated; execution completed and competitor cleanup verified"
			}
			var operationFailed *provision.OperationFailedError
			if action.Recovery == RemoteMutation && errors.As(executionErr, &operationFailed) && operationFailed.OperationURL == status.Binding.OperationURL {
				status.State = Failed
				status.Message = "accepted RP operation terminal: " + operationFailed.State + "; resource cleanup remains explicit"
			}
			if (action.Kind == CandidateApply || action.Kind == CandidateUpdate || action.Kind == CatalogPin) && errors.As(executionErr, &noMutation) {
				status.State = Failed
				status.Message = "candidate failed before non-dry-run Update was dispatched"
			}
		}
		attempt.State = status.State
		if action.Kind == StartupObserve || action.Kind == StartupObserveExisting || action.Kind == RetentionRefresh {
			status.Armed = false
		}
		summarize(current)
		return nil
	})
}

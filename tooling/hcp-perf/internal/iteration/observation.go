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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

// RetryObservation replaces only startup-observe, never candidate/RP/retention.
// It freezes the caller's configured executable, records the reason, stops the
// exact old observer via pidfd, then publishes a new measurement attempt. The
// entire run is not stopped, and no sibling is signaled or resubmitted.
func (c *Controller) RetryObservation(ctx context.Context, id, actionID, reason string) error {
	return c.RetryObservationImages(ctx, id, actionID, reason, nil)
}

// RetryObservationImages records explicitly reviewed image-only expectation
// corrections in the immutable attempt revision. Other spec fields cannot change.
func (c *Controller) RetryObservationImages(ctx context.Context, id, actionID, reason string, bindings []ObservationImageBinding) error {
	if !safeName.MatchString(id) || !safeName.MatchString(actionID) || strings.TrimSpace(reason) == "" || len(reason) > 1024 {
		return errors.New("observer retry requires safe run/action IDs and a nonempty reason of at most 1024 bytes")
	}
	lock, err := acquireLock(filepath.Join(c.root, id, "reconcile.lock"))
	if err != nil {
		return fmt.Errorf("lock observer replacement: %w", err)
	}
	defer lock.Close()
	run, err := c.Status(ctx, id)
	if err != nil {
		return err
	}
	if run.StopRequested || run.State == Succeeded || run.State == Failed || run.State == Stopped {
		return errors.New("cannot replace observer in stopped or terminal run")
	}
	spec, err := c.loadSpec(run)
	if err != nil {
		return err
	}
	index := -1
	for i, action := range spec.Actions {
		if action.ID == actionID && (action.Kind == StartupObserve || action.Kind == StartupObserveExisting) {
			index = i
		}
	}
	if index < 0 {
		return errors.New("retry-observation only accepts a startup-observe action")
	}
	status := run.Actions[index]
	if len(status.Attempts) == 0 || len(status.Attempts) >= 10 {
		return errors.New("observer must have an existing attempt and fewer than ten attempts")
	}
	old := status.Attempts[len(status.Attempts)-1]
	if len(bindings) == 0 && old.Observation != nil {
		bindings = old.Observation.ImageBindings
	}
	if _, err := revisedObservation(*spec.Actions[index].Startup, bindings); err != nil {
		return err
	}
	if status.State != Running && status.State != Blocked && status.State != Failed {
		return errors.New("observer must be running, blocked or failed")
	}
	clients, err := startup.NewClients(spec.Actions[index].Startup.Kubeconfig, spec.Actions[index].Startup.AllowExecCredential)
	if err != nil {
		return err
	}
	previous := startup.Identity{}
	if status.StartupIdentity != nil {
		previous = *status.StartupIdentity
	}
	identity, err := startup.Discover(ctx, clients, spec.Actions[index].Startup.ResourceID, previous)
	if err != nil {
		return fmt.Errorf("verify observer replacement identity: %w", err)
	}
	if identity.HCUID == "" {
		return errors.New("observer replacement requires the exact existing HostedCluster")
	}
	token := fmt.Sprintf("%x", randomToken())
	dir := c.attemptDir(id, token)
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	source, err := os.Open(c.options.BinaryPath)
	if err != nil {
		return fmt.Errorf("open replacement observer binary: %w", err)
	}
	defer source.Close()
	target, err := os.OpenFile(filepath.Join(dir, "worker"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0500)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(target, hash), source)
	if err := errors.Join(copyErr, target.Sync(), target.Close()); err != nil {
		return err
	}
	replacement := Attempt{Token: token, State: Dispatched, BinarySHA256: fmt.Sprintf("%x", hash.Sum(nil)), Observation: &ObservationRevision{PreviousAttempt: old.Token, Reason: reason, RequestedAt: time.Now().UTC(), Identity: identity, ImageBindings: bindings}}
	replacement.Observation.RearmedActions = suppressedObserverDescendants(spec, &run, actionID)
	data, err := json.Marshal(replacement)
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(dir, "observation-revision.json"), data, 0400); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Join(c.root, id)); err != nil {
		return err
	}
	// Audit intent is durable before signaling; an interrupted replacement can
	// leave a reserved directory, but cannot launch an unjournaled attempt.
	if sameProcess(old.Process) {
		if old.Process.PID == os.Getpid() {
			return errors.New("refusing to signal the current controller process")
		}
		if err := stopObserverProcess(old.Process); err != nil {
			return err
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var workerLock *os.File
	for {
		workerLock, err = acquireLock(c.attemptDir(id, old.Token) + ".lock")
		if err == nil && !sameProcess(old.Process) {
			break
		}
		if workerLock != nil {
			workerLock.Close()
			workerLock = nil
		}
		if err != nil && !errors.Is(err, errLocked) {
			return err
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait for old observer exit: %w", waitCtx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer workerLock.Close()
	// Repeat identity verification after shutdown; do not silently adopt a
	// replacement HC/HCP while the old observer was being stopped.
	identity, err = startup.Discover(ctx, clients, spec.Actions[index].Startup.ResourceID, identity)
	if err != nil {
		return err
	}
	if err := c.update(ctx, id, func(current *Run) error {
		status := &current.Actions[index]
		latest := &status.Attempts[len(status.Attempts)-1]
		if current.StopRequested || latest.Token != old.Token || current.State == Succeeded || current.State == Failed || current.State == Stopped {
			return errors.New("observer replacement fencing mismatch or released environment claim")
		}
		latest.State = Blocked
		status.Attempts = append(status.Attempts, replacement)
		status.State, status.Armed, status.StartupIdentity = Dispatched, false, &identity
		status.Message = "explicit observer replacement; interrupted measurement cannot qualify fresh startup"
		for _, id := range replacement.Observation.RearmedActions {
			for i := range current.Actions {
				dependent := &current.Actions[i]
				if dependent.ID != id {
					continue
				}
				if dependent.State != Stopped || len(dependent.Attempts) != 0 || dependent.Message != "initial action suppressed after sibling failure" {
					return errors.New("observer dependent rearm fencing mismatch")
				}
				dependent.State = Prepared
				dependent.Message = "rearmed by explicit observer retry " + token
			}
		}
		summarize(current)
		return nil
	}); err != nil {
		return err
	}
	return c.options.ProcessFactory(filepath.Join(dir, "worker"), c.root, token)
}

// Only undispatched descendants suppressed by the failed observation are eligible.
// Any independent failed/uncertain action prevents rearming initial work.
func suppressedObserverDescendants(spec Spec, run *Run, observer string) []string {
	if run.StopRequested {
		return nil
	}
	for _, status := range run.Actions {
		if status.ID != observer && (status.State == Blocked || status.State == Failed) {
			return nil
		}
	}
	descendants := map[string]bool{observer: true}
	var rearm []string
	for _, action := range spec.Actions {
		for _, dependency := range action.DependsOn {
			if descendants[dependency.Action] {
				descendants[action.ID] = true
			}
		}
		if action.ID == observer || !descendants[action.ID] {
			continue
		}
		for _, status := range run.Actions {
			if status.ID == action.ID && status.State == Stopped && len(status.Attempts) == 0 && status.Binding.ResourceID == "" && status.Binding.OperationURL == "" && status.Message == "initial action suppressed after sibling failure" {
				rearm = append(rearm, action.ID)
			}
		}
	}
	return rearm
}

func revisedObservation(spec startup.Spec, bindings []ObservationImageBinding) (startup.Spec, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return spec, err
	}
	var revised startup.Spec
	if err := json.Unmarshal(data, &revised); err != nil {
		return spec, err
	}
	seen := map[string]bool{}
	for _, binding := range bindings {
		if !regexp.MustCompile(`^[^\s]+@sha256:[0-9a-f]{64}$`).MatchString(binding.Image) {
			return spec, errors.New("replacement image must be digest pinned")
		}
		key := fmt.Sprintf("%s/%s/%t/%s", binding.Kind, binding.Component, binding.Init, binding.Container)
		if seen[key] || binding.Image == binding.PreviousImage {
			return spec, errors.New("duplicate or unchanged image binding")
		}
		seen[key] = true
		found := false
		for i := range revised.Components {
			component := &revised.Components[i]
			if component.Kind != binding.Kind || component.Name != binding.Component {
				continue
			}
			containers := component.Containers
			if binding.Init {
				containers = component.InitContainers
			}
			for j := range containers {
				if containers[j].Name != binding.Container {
					continue
				}
				if containers[j].Image != binding.PreviousImage {
					return spec, errors.New("previous image binding mismatch")
				}
				containers[j].Image = binding.Image
				found = true
			}
		}
		if !found {
			return spec, errors.New("image binding owner/container absent")
		}
	}
	return revised, revised.Validate()
}

func stopObserverProcess(identity ProcessIdentity) error {
	fd, err := unix.PidfdOpen(identity.PID, 0)
	if err != nil {
		return fmt.Errorf("open observer pidfd: %w", err)
	}
	defer unix.Close(fd)
	if !sameProcess(identity) {
		return errors.New("observer identity changed before signal")
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil {
		return fmt.Errorf("signal exact observer process: %w", err)
	}
	return nil
}

// Old, still-running sibling workers preserve attempt tokens but cannot preserve
// newly added JSON fields. The immutable revision file is the durable authority
// for the attempt executable and audit metadata during this shipped transition.
func (c *Controller) loadObservationRevisions(run *Run) error {
	for i := range run.Actions {
		for j := range run.Actions[i].Attempts {
			attempt := &run.Actions[i].Attempts[j]
			data, err := os.ReadFile(filepath.Join(c.attemptDir(run.ID, attempt.Token), "observation-revision.json"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			var revision Attempt
			if err := json.Unmarshal(data, &revision); err != nil {
				return err
			}
			if revision.Token != attempt.Token || revision.Observation == nil || len(revision.BinarySHA256) != 64 {
				return errors.New("invalid observation revision manifest")
			}
			attempt.BinarySHA256, attempt.Observation = revision.BinarySHA256, revision.Observation
		}
	}
	return nil
}

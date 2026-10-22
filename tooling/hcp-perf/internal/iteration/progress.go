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
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

const maxProgressBytes = 256 << 10

// This reader never follows a final symlink or blocks on a FIFO. os.Root also
// confines intermediate symlinks to the controller root. Diagnostics cannot
// turn malformed artifacts into controller failure or credential disclosure.
func (c *Controller) readProgressFile(relative string, target any) string {
	root, err := os.OpenRoot(c.root)
	if err != nil {
		return "unavailable"
	}
	defer root.Close()
	parent := ""
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)
		info, err := root.Lstat(parent)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "unavailable"
		}
	}
	file, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "missing"
	}
	if err != nil {
		return "unavailable"
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxProgressBytes {
		return "invalid"
	}
	data, err := io.ReadAll(io.LimitReader(file, maxProgressBytes+1))
	if err != nil {
		return "unavailable"
	}
	if len(data) > maxProgressBytes || json.Unmarshal(data, target) != nil {
		return "invalid"
	}
	return "available"
}

type reconcileHeartbeat struct {
	RunID      string          `json:"runID"`
	SpecSHA256 string          `json:"specSHA256"`
	At         time.Time       `json:"at"`
	State      string          `json:"state"`
	Process    ProcessIdentity `json:"process"`
}

func (c *Controller) saveReconcileHeartbeat(run Run, state string) error {
	identity, err := processIdentity(os.Getpid())
	if err != nil {
		return err
	}
	heartbeat := reconcileHeartbeat{RunID: run.ID, SpecSHA256: run.SpecSHA256, At: time.Now().UTC(), State: state, Process: identity}
	data, err := json.Marshal(heartbeat)
	if err != nil {
		return err
	}
	dir := filepath.Join(c.root, run.ID)
	file, err := os.CreateTemp(dir, ".reconcile-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, "reconcile-status.json")); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func (c *Controller) enrichProgress(run *Run) {
	run.LastReconciledAt, run.ControllerProcess = nil, nil
	run.ControllerIdentityVerified = false
	var heartbeat reconcileHeartbeat
	run.ReconcileState = c.readProgressFile(filepath.Join(run.ID, "reconcile-status.json"), &heartbeat)
	if run.ReconcileState == "available" {
		if heartbeat.RunID == run.ID && heartbeat.SpecSHA256 == run.SpecSHA256 && !heartbeat.At.IsZero() && (heartbeat.State == "completed" || heartbeat.State == "error" || heartbeat.State == "running") {
			run.LastReconciledAt, run.ControllerProcess = &heartbeat.At, &heartbeat.Process
			run.ReconcileState = heartbeat.State
			run.ControllerIdentityVerified = sameProcess(heartbeat.Process)
		} else {
			run.ReconcileState = "invalid"
		}
	}
	// The hash-checked immutable spec identifies which action owns the snapshot.
	spec, specErr := c.loadSpec(*run)
	for i := range run.Actions {
		status := &run.Actions[i]
		status.StartupProgress, status.WorkerIdentityVerified = nil, false
		if len(status.Attempts) == 0 {
			continue
		}
		attempt := status.Attempts[len(status.Attempts)-1]
		status.WorkerIdentityVerified = attempt.State == Running && sameProcess(attempt.Process)
		if specErr != nil || i >= len(spec.Actions) || spec.Actions[i].ID != status.ID || (spec.Actions[i].Kind != StartupObserve && spec.Actions[i].Kind != StartupObserveExisting) {
			continue
		}
		action := spec.Actions[i]
		progress := &startup.Status{Phase: "unknown", SnapshotState: "missing", Stale: true, StaleAfterSeconds: int64(max(3*action.Startup.PollInterval, time.Minute) / time.Second), UnhealthyWorkloads: []string{}, IssueCodes: []string{}}
		status.StartupProgress = progress
		var sample startup.Sample
		progress.SnapshotState = c.readProgressFile(filepath.Join(run.ID, attempt.Token, "startup-status.json"), &sample)
		if progress.SnapshotState != "available" {
			progress.LastError = "snapshot-" + progress.SnapshotState
			continue
		}
		if sample.At.IsZero() || !strings.EqualFold(sample.Identity.ResourceID, action.Startup.ResourceID) || sample.NewRestarts < 0 || sample.NewOOMs < 0 || sample.NewProbeFailures < 0 || !validCoverage(sample.Coverage) {
			progress.SnapshotState, progress.LastError = "invalid", "snapshot-invalid"
			continue
		}
		progress.ObservedAt = &sample.At
		progress.Stale = time.Since(sample.At) > time.Duration(progress.StaleAfterSeconds)*time.Second || sample.At.After(time.Now().Add(5*time.Second))
		progress.Coverage = sample.Coverage
		if sample.LastSuccessfulPoll != nil && !sample.LastSuccessfulPoll.After(sample.At) {
			progress.LastSuccessfulPoll = sample.LastSuccessfulPoll
		}
		if sample.HealthySince != nil && !sample.HealthySince.After(sample.At) {
			progress.HealthySince = sample.HealthySince
		}
		progress.NewRestarts, progress.NewOOMs, progress.NewProbeFailures = sample.NewRestarts, sample.NewOOMs, sample.NewProbeFailures
		progress.ProbeFailurePolicy = sample.ProbeFailurePolicy
		if progress.ProbeFailurePolicy == "" {
			progress.ProbeFailurePolicy = startup.StrictProbes
		}
		if progress.ProbeFailurePolicy != startup.StrictProbes && progress.ProbeFailurePolicy != startup.Availability {
			progress.SnapshotState, progress.LastError, progress.Stale = "invalid", "snapshot-invalid", true
			continue
		}
		progress.NewProbeKills = max(0, sample.NewProbeKills)
		if sample.ProbeQuality == "clean" || sample.ProbeQuality == "degraded" {
			progress.ProbeQuality = sample.ProbeQuality
		}
		for _, warning := range sample.Warnings {
			code, _ := startupIssue(warning)
			if len(progress.WarningCodes) < 32 && !contains(progress.WarningCodes, code) {
				progress.WarningCodes = append(progress.WarningCodes, code)
			}
		}
		progress.HealthyWindow, progress.FreshQualified = sample.HealthyWindow, sample.FreshQualified
		progress.Phase = "observing"
		if sample.Identity.HCPUID == "" {
			progress.Phase = "discovering"
		}
		if sample.Healthy {
			progress.Phase = "stabilizing"
		}
		if sample.HealthyWindow {
			progress.Phase = "healthy-window"
		}
		for _, issue := range sample.Issues {
			code, name := startupIssue(issue)
			if len(progress.IssueCodes) < 32 && !contains(progress.IssueCodes, code) {
				progress.IssueCodes = append(progress.IssueCodes, code)
			}
			if name != "" && len(progress.UnhealthyWorkloads) < 64 && !contains(progress.UnhealthyWorkloads, name) {
				progress.UnhealthyWorkloads = append(progress.UnhealthyWorkloads, name)
			}
			if progress.LastError == "" {
				progress.LastError = code
			}
		}
		if len(sample.Issues) > 0 {
			progress.Phase = "unhealthy"
		}
		if progress.Stale {
			progress.Phase = "stale"
		}
	}
}

func validCoverage(coverage *startup.Coverage) bool {
	return coverage == nil || (coverage.RequiredWorkloads >= 0 && coverage.ObservedWorkloads >= 0 && coverage.ReadyPods >= 0 && coverage.RequiredPods >= 0 && coverage.ReadyEndpoints >= 0 && coverage.RequiredEndpoints >= 0)
}

func contains(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

var progressResourceName = regexp.MustCompile(`^(?:(?:Deployment|StatefulSet|DaemonSet|Job|CronJob|Pod)/)?[a-z0-9][a-z0-9.-]{0,252}(?:/[a-z0-9][a-z0-9.-]{0,252})?$`)

func startupIssue(issue string) (code, name string) {
	// Fixed codes only: never propagate arbitrary response/log/URL/error text.
	for prefix, code := range map[string]string{
		"missing ": "missing-workload", "replicas not ready: ": "replicas-not-ready",
		"template resources mismatch: ": "template-profile-mismatch", "observed pod resources mismatch: ": "pod-profile-mismatch",
		"observed pods not ready: ": "pods-not-ready", "endpoint inspection failed: ": "endpoint-request-failed", "endpoints not ready: ": "endpoints-not-ready",
		"undeclared workload: ": "undeclared-workload", "undeclared pod: ": "undeclared-pod", "container crash loop: ": "crash-loop",
		"disabled workload has active pod: ": "disabled-workload-active", "failed Job: ": "job-failed", "Job not completed: ": "job-incomplete",
		"CronJob suspension mismatch: ": "cronjob-suspension-mismatch", "disabled CronJob has active Jobs: ": "disabled-cronjob-active",
		"disabled CronJob has unfinished Job: ": "disabled-cronjob-unfinished", "failed batch/standalone Pod: ": "pod-failed",
		"standalone Pod has owner: ": "standalone-pod-owned", "standalone Pod not completed: ": "standalone-pod-incomplete", "standalone Pod not ready: ": "standalone-pod-not-ready",
	} {
		if strings.HasPrefix(issue, prefix) {
			name := strings.TrimPrefix(issue, prefix)
			if !progressResourceName.MatchString(name) {
				name = ""
			}
			return code, name
		}
	}
	switch issue {
	case "HostedCluster CS identity mismatch", "HostedControlPlane CS identity mismatch":
		return "cs-identity-mismatch", ""
	case "HostedControlPlane supplemental clusterID mismatch":
		return "cluster-uuid-mismatch", ""
	case "HostedControlPlane ARM identity mismatch":
		return "arm-identity-mismatch", ""
	case "bound startup identity changed or disappeared", "bound HostedCluster disappeared":
		return "bound-identity-changed", ""
	case "ambiguous HostedCluster ARM identity", "ambiguous HostedControlPlane HostedCluster reference":
		return "identity-ambiguous", ""
	case "HostedCluster lacks immutable identity", "control plane lacks immutable identity":
		return "identity-incomplete", ""
	case "HostedCluster discovery request failed", "HostedControlPlane discovery request failed", "control plane namespace identity request failed":
		return "identity-request-failed", ""
	case "Kubernetes health inspection failed":
		return "health-request-failed", ""
	case "snapshot recorder failed":
		return "recorder-failed", ""
	case "waiting for exact control plane identity":
		return "waiting-for-identity", ""
	case "candidate spec drift":
		return "candidate-drift", ""
	case "new probe failure event":
		return "probe-failure", ""
	case "new probe-triggered container kill":
		return "probe-kill", ""
	default:
		return "unclassified-observation-error", ""
	}
}

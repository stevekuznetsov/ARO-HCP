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
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Pointers distinguish observed zero counters from absent or null evidence.
type failureSnapshot struct {
	ObservedAt time.Time `json:"observedAt"`
	Deployment struct {
		UID                string `json:"uid"`
		Generation         *int64 `json:"generation"`
		ObservedGeneration *int64 `json:"observedGeneration"`
		Desired            *int32 `json:"desired"`
		Replicas           *int32 `json:"replicas"`
		Updated            *int32 `json:"updated"`
		Ready              *int32 `json:"ready"`
		Available          *int32 `json:"available"`
	} `json:"deployment"`
	Pods json.RawMessage `json:"pods"`
}

func (b *builder) failure(root *os.Root, command, exitRef string, deadlines map[string]float64, budgetRef string) {
	f := Failure{Command: command, Stage: "unknown", Operation: "unknown", Status: "error", ElapsedKind: "time_until_error", ApplicationOutcome: "unknown", EvidenceRefs: []string{exitRef}, Recovery: Recovery{Status: "unknown", Scope: "canary Deployment and its owned pods only; not service availability"}}
	var sample struct {
		Preflight     stage           `json:"preflight"`
		Ack           stage           `json:"ack"`
		Ready         stage           `json:"ready"`
		Cleanup       stage           `json:"cleanup"`
		DeploymentUID string          `json:"deploymentUID"`
		Generation    *int64          `json:"generation"`
		Last          failureSnapshot `json:"last"`
		CleanupLast   failureSnapshot `json:"cleanupLast"`
	}
	canary := canaryName.MatchString(command+".json") || command == "canary-warmup"
	ref := ""
	if canary {
		ref = b.document(root, "commands/"+command+".json", &sample)
	}
	if ref != "" {
		f.DeploymentUID = sample.DeploymentUID
		f.EvidenceRefs = append(f.EvidenceRefs, ref)
		var s stage
		switch {
		case !sample.Preflight.Started.IsZero() && sample.Preflight.Completed.IsZero():
			f.Stage, f.Operation, s = "preflight", "Observe and validate Deployment at zero replicas without mutation", sample.Preflight
		case !sample.Preflight.Completed.IsZero() && sample.Ack.Attempts > 0 && sample.Ack.Completed.IsZero():
			f.Stage, f.Operation, s = "ack", "Scale Deployment from 0 to 1 using PUT deployments/scale", sample.Ack
		case !sample.Preflight.Completed.IsZero() && !sample.Ack.Completed.IsZero() && !sample.Ready.Started.IsZero() && sample.Ready.Completed.IsZero():
			f.Stage, f.Operation, s = "ready", "Observe Deployment and owned pod Ready after scale-up", sample.Ready
		case !sample.Preflight.Completed.IsZero() && !sample.Ack.Completed.IsZero() && !sample.Ready.Completed.IsZero() && !sample.Cleanup.Started.IsZero() && sample.Cleanup.Completed.IsZero():
			f.Stage, f.Operation, s = "cleanup", "Scale Deployment to 0 using PUT deployments/scale and observe no owned pods", sample.Cleanup
		}
		if !s.Started.IsZero() && !s.Started.After(b.result.GeneratedAt) && s.Duration > 0 {
			f.ElapsedSeconds = float64(s.Duration) / 1e9
		}
		// Fitted budgets apply to steady cycles, not the earlier baseline or the
		// independent preflight. Never infer those deadlines from current defaults.
		if s.Expected != nil && *s.Expected > 0 {
			expected := float64(*s.Expected) / 1e9
			f.ExpectedSeconds = &expected
		}
		if s.Deadline != nil && *s.Deadline > 0 {
			deadline := float64(*s.Deadline) / 1e9
			f.DeadlineSeconds = &deadline
		} else if deadline, ok := deadlines[f.Stage]; ok && s.Deadline == nil && s.Expected == nil && strings.HasPrefix(command, "canary-steady-") {
			f.DeadlineSeconds = &deadline
			f.EvidenceRefs = append(f.EvidenceRefs, budgetRef)
		}
		cleanup, last := sample.Cleanup, sample.CleanupLast
		d := last.Deployment
		valid := !cleanup.Started.IsZero() && !cleanup.Completed.IsZero() && !cleanup.Completed.Before(cleanup.Started) && !cleanup.Completed.After(b.result.GeneratedAt) && cleanup.Duration > 0 &&
			!last.ObservedAt.IsZero() && !last.ObservedAt.Before(cleanup.Started) && !last.ObservedAt.After(b.result.GeneratedAt) &&
			sample.DeploymentUID != "" && d.UID == sample.DeploymentUID &&
			sample.Last.Deployment.UID == sample.DeploymentUID &&
			d.Generation != nil && *d.Generation > 0 && d.ObservedGeneration != nil && *d.ObservedGeneration >= *d.Generation
		// Generation is the attempted scale-up target, not proof that the write
		// applied. Cleanup may legitimately remain at the preflight generation.
		valid = valid && sample.Generation != nil && *sample.Generation > 0 &&
			sample.Last.Deployment.Generation != nil && *sample.Last.Deployment.Generation > 0 && *d.Generation >= *sample.Last.Deployment.Generation
		for _, n := range []*int32{d.Desired, d.Replicas, d.Updated, d.Ready, d.Available} {
			valid = valid && n != nil && *n == 0
		}
		var pods []json.RawMessage
		// The canary contract records nil as null after observing no owned pods.
		// Missing pods or {} are not observations; completion proves the owner set
		// was checked, rather than treating a selector-only empty list as recovery.
		valid = valid && len(last.Pods) > 0 && json.Unmarshal(last.Pods, &pods) == nil && len(pods) == 0
		if valid {
			f.Recovery.Status = "observed"
			f.Recovery.ElapsedSeconds = float64(cleanup.Duration) / 1e9
			f.Recovery.ZeroReplicasObserved, f.Recovery.NoOwnedPodsObserved = true, true
		}
	}
	if canary {
		var stderr struct {
			Error string `json:"error"`
		}
		if stderrRef := b.document(root, "commands/"+command+".stderr", &stderr); stderrRef != "" {
			f.EvidenceRefs = append(f.EvidenceRefs, stderrRef)
			// Only classify the primary error, not a joined cleanup timeout. Never
			// persist the error text, URLs, credentials, or guessed HTTP codes.
			primary := strings.SplitN(stderr.Error, "\ncleanup:", 2)[0]
			if f.Stage == "cleanup" {
				primary = stderr.Error
			}
			if f.Stage != "unknown" && strings.Contains(primary, "context deadline exceeded") {
				f.Status, f.ElapsedKind = "client_timeout", "time_to_timeout"
			}
		}
	}
	f.EvidenceRefs = unique(f.EvidenceRefs)
	b.result.Failures = append(b.result.Failures, f)
	statement := "Workload command failed: " + command + "; failing stage unknown."
	if f.Stage != "unknown" {
		label := f.Stage
		if f.Stage == "ack" {
			label = "ACK"
		}
		statement = command + ": " + label + " completion not observed"
		if f.DeadlineSeconds != nil && f.Status == "client_timeout" {
			statement = command + ": " + label + " not observed before " + failureDuration(*f.DeadlineSeconds)
		}
		if f.ElapsedSeconds > 0 {
			if f.Status == "client_timeout" {
				statement += "; client timed out after " + failureDuration(f.ElapsedSeconds)
			} else {
				statement += "; time until error " + failureDuration(f.ElapsedSeconds)
			}
		}
		statement += "."
	}
	if f.Recovery.Status == "observed" {
		statement += " Cleanup completed in " + failureDuration(f.Recovery.ElapsedSeconds) + ", zero replicas/no owned pods."
	} else {
		statement += " Recovery unknown."
	}
	if f.Stage == "ack" || f.Stage == "preflight" {
		statement += " Ready not reached; its elapsed field is not a completed Ready latency."
	}
	statement += " Application outcome and cause unknown; no server rejection or service availability failure established."
	b.finding("workload", "observed", statement, f.EvidenceRefs, "recorded workload gate")
	if f.Stage == "ack" {
		b.result.CarryForward = append(b.result.CarryForward, CarryForward{ID: "diagnose-ack-" + command, Action: "diagnose", Recommendation: "For future artifacts, record overrun; wait to an explicit hard deadline; compare completed latency. Diagnose end-to-end ACK delay without claiming server rejection or reinterpreting historical canceled waits as completions.", EvidenceRefs: f.EvidenceRefs, DataPointRefs: []string{}, Scope: "client-observed ACK gate", BlockedBy: []string{"cause unknown; linked server evidence unavailable"}, Conditions: []string{"do not automatically retune expectations or infer CPU/APF causation"}, Kind: "hypothesis"})
	}
}

func failureDuration(seconds float64) string {
	if seconds < 1 {
		return fmt.Sprintf("%.3fms", seconds*1000)
	}
	return fmt.Sprintf("%.3fs", seconds)
}

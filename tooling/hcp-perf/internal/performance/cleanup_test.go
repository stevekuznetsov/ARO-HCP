// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package performance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupExactInventory(t *testing.T) {
	base := CleanupSpec{Source: testSpec(), Directory: "/case", OutcomeSHA256: strings.Repeat("a", 64), Objects: []CleanupObject{
		{Kind: "Namespace", Name: "hcp-perf-canary-unique", UID: "1"}, {Kind: "Namespace", Name: "fresh-0", UID: "2"},
		{Kind: "FlowSchema", Name: "hcp-perf-canary-unique", UID: "3"}, {Kind: "FlowSchema", Name: "hcp-perf-benchmark-unique-0", UID: "4"},
		{Kind: "PriorityLevelConfiguration", Name: "hcp-perf-canary-unique", UID: "5"}, {Kind: "PriorityLevelConfiguration", Name: "hcp-perf-benchmark-unique", UID: "6"},
	}}
	for _, scenario := range []string{"valid", "legacy-source", "rebound-source", "missing", "duplicate", "other-namespace", "pvc", "uid", "competitor"} {
		t.Run(scenario, func(t *testing.T) {
			spec := base
			spec.Objects = append([]CleanupObject(nil), base.Objects...)
			switch scenario {
			case "legacy-source":
				spec.Source.ExpectedGuestNodes = nil
			case "rebound-source":
				spec.Source = catalogSpec("b")
				spec.Source.ExistingPopulation = &PopulationReference{Directory: "/old", OutcomeSHA256: strings.Repeat("a", 64), InventorySHA256: strings.Repeat("b", 64), ImageBindings: catalogBindings("a", "b")}
			case "missing":
				spec.Objects = spec.Objects[:5]
			case "duplicate":
				spec.Objects[1] = spec.Objects[0]
			case "other-namespace":
				spec.Objects[0].Name = "kube-system"
			case "pvc":
				spec.Objects[0].Kind = "PersistentVolumeClaim"
			case "uid":
				spec.Objects[0].UID = ""
			case "competitor":
				spec.Source.Competitor = &Competitor{}
			}
			before, _ := json.Marshal(spec.Source)
			if err := spec.Validate(); (err == nil) != (scenario == "valid" || scenario == "legacy-source" || scenario == "rebound-source") {
				t.Fatalf("validation %v", err)
			}
			after, _ := json.Marshal(spec.Source)
			if string(before) != string(after) {
				t.Fatal("cleanup rewrote historical source")
			}
		})
	}
}

func TestUnfinalizedPerformanceNeverRecovers(t *testing.T) {
	spec := testSpec()
	for _, scenario := range []string{"missing", "invalid", "unfinished", "cleanup-uncertain", "spec-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			raw, _ := json.Marshal(spec)
			if err := os.WriteFile(filepath.Join(dir, "spec.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			out := Outcome{Started: time.Now().Add(-time.Minute), Finished: time.Now(), State: "failed", ExecutionStatus: "failed", CleanupStatus: "not-needed"}
			switch scenario {
			case "unfinished":
				out.Finished = time.Time{}
			case "cleanup-uncertain":
				out.CleanupStatus = "unresolved"
			case "spec-mismatch":
				spec.Seed.ID = "different"
			}
			if scenario != "missing" {
				raw, _ = json.Marshal(out)
				if scenario == "invalid" {
					raw = []byte("bad")
				}
				if err := os.WriteFile(filepath.Join(dir, "outcome.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := VerifyFinalized(t.Context(), spec, dir); err == nil {
				t.Fatal("unproven receipt accepted")
			}
		})
	}
}

func TestCleanupIdentity(t *testing.T) {
	object := CleanupObject{Kind: "Namespace", Name: "owned", UID: "original"}
	if cleanupIdentity(object, "original", "run", "run", false) != nil {
		t.Fatal("valid identity rejected")
	}
	for _, v := range []struct {
		uid, owner string
		deleting   bool
	}{{"replacement", "run", false}, {"original", "other", false}, {"original", "run", true}} {
		if cleanupIdentity(object, v.uid, v.owner, "run", v.deleting) == nil {
			t.Fatal("unsafe cleanup accepted")
		}
	}
}

func TestCompletedCleanupDoesNotBecomeFailedRecovery(t *testing.T) {
	base := Outcome{Started: time.Now().Add(-time.Minute), Finished: time.Now(), State: "completed", ExecutionStatus: "completed", CleanupStatus: "not-needed"}
	if !finalizedOutcome(base, true) || finalizedOutcome(base, false) {
		t.Fatal("completed case must be cleanable, not recoverable as failed")
	}
	for _, scenario := range []string{"unfinished", "cleanup-uncertain", "interrupted", "running"} {
		out := base
		switch scenario {
		case "unfinished":
			out.Finished = time.Time{}
		case "cleanup-uncertain":
			out.CleanupStatus = "unresolved"
		case "interrupted":
			out.ExecutionStatus = "interrupted"
		case "running":
			out.State = "running"
		}
		if finalizedOutcome(out, true) {
			t.Fatalf("accepted %s", scenario)
		}
	}
}

func TestPartialSeedCleanupInventory(t *testing.T) {
	s := CleanupSpec{Source: testSpec(), Directory: "/case", OutcomeSHA256: strings.Repeat("a", 64), Objects: []CleanupObject{{Kind: "Namespace", Name: "fresh-0", UID: "created"}}, AbsentObjects: []CleanupObject{
		{Kind: "Namespace", Name: "hcp-perf-canary-unique"},
		{Kind: "FlowSchema", Name: "hcp-perf-canary-unique"},
		{Kind: "FlowSchema", Name: "hcp-perf-benchmark-unique-0"},
		{Kind: "PriorityLevelConfiguration", Name: "hcp-perf-canary-unique"},
		{Kind: "PriorityLevelConfiguration", Name: "hcp-perf-benchmark-unique"},
	}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.AbsentObjects[0].UID = "invented"
	if s.Validate() == nil {
		t.Fatal("accepted fabricated absent UID")
	}
}

func TestRebindingDoesNotNormalizeCleanupReceipt(t *testing.T) {
	source := catalogSpec("a")
	dir := t.TempDir()
	populationReceipt(t, dir, "spec.json", source)
	target := catalogSpec("b")
	target.ExistingPopulation = &PopulationReference{Directory: dir, OutcomeSHA256: strings.Repeat("a", 64), InventorySHA256: strings.Repeat("b", 64), ImageBindings: catalogBindings("a", "b")}
	if _, err := verifyFinalized(t.Context(), target, dir, true); err == nil || err.Error() != "performance spec receipt mismatch" {
		t.Fatalf("cleanup must retain exact original source, not reuse normalization: %v", err)
	}
}

func TestVerifyFinalizedRejectsPopulationBindingMismatch(t *testing.T) {
	for _, mode := range []string{"omitted", "changed"} {
		t.Run(mode, func(t *testing.T) {
			spec := catalogSpec("b")
			spec.ExistingPopulation = &PopulationReference{Directory: "/previous", OutcomeSHA256: strings.Repeat("a", 64), InventorySHA256: strings.Repeat("b", 64), ImageBindings: catalogBindings("a", "b")}
			dir := t.TempDir()
			populationReceipt(t, dir, "spec.json", spec)
			if mode == "omitted" {
				spec.ExistingPopulation.ImageBindings = nil
			} else {
				spec.ExistingPopulation.ImageBindings[0].PreviousImage = "quay.io/test/catalog@sha256:" + strings.Repeat("c", 64)
			}
			// Only bindings differ; rejection must precede outcome or live reads.
			if _, err := VerifyFinalized(t.Context(), spec, dir); err == nil || err.Error() != "performance spec receipt mismatch" {
				t.Fatalf("binding-only mismatch was not rejected: %v", err)
			}
		})
	}
}

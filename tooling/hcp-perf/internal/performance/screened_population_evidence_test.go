// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package performance

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Optional local forensic test: reads only frozen receipts, never credentials or
// clients. Live UID/content/convergence checks cannot be proven by these files.
func TestScreenedPopulationFrozenEvidence(t *testing.T) {
	root := os.Getenv("HCP_PERF_EVIDENCE_ROOT")
	if root == "" {
		t.Skip("local frozen evidence not supplied")
	}
	read := func(path string, value any) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(compact.Bytes(), value); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var target Spec
	read(filepath.Join(root, "medium-2400-q40-hour-20260920T1828Z/performance.json"), &target)
	var source Spec
	var outcome Outcome
	dir := target.ExistingPopulation.Directory
	specRaw := read(filepath.Join(dir, "spec.json"), &source)
	outRaw := read(filepath.Join(dir, "outcome.json"), &outcome)
	var results Results
	resultsRaw := read(filepath.Join(dir, "results.json"), &results)
	var inventory []populationObject
	popRaw := read(filepath.Join(dir, "population.json"), &inventory)
	hash := func(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }
	if hash(specRaw) != "c74183b2d3eb0d357d1df0d4d02f7dce94235d07f67b7e32118f8682df5ad93b" || hash(outRaw) != "b04214c17d7a44cf9277be38a14397bf3eeb634da7e7555b8afec35a7c49efa5" || hash(resultsRaw) != "05a96f67abb6e306c10b4322e3f6226267bbc087489ae616b1e6cc15c9dea416" || hash(popRaw) != "c41d7e9f19701aa42691534e05a6c68338e09f22596641e3123fe29a36936b39" {
		t.Fatal("q45 receipt changed")
	}
	if err := verifyPopulation(t.Context(), nil, target, outcome.Identity); err == nil || err.Error() != "prior population hash mismatch" {
		t.Fatalf("legacy reproduction: %v", err)
	}
	target.ExistingPopulation.InventorySHA256 = hash(popRaw)
	if err := verifyPopulation(t.Context(), nil, target, outcome.Identity); err == nil || err.Error() != "prior population hash mismatch" {
		t.Fatalf("correcting top pin must not hide legacy chain: %v", err)
	}
	target.ExistingPopulation.RecoverFailed = false
	if _, err := populationPredecessor(target, outcome.Identity); err == nil || err.Error() != "population predecessor not completed" {
		t.Fatalf("ordinary reuse must reject: %v", err)
	}
	target.ExistingPopulation.ScreenedPopulation = &ScreenedPopulationProof{SpecSHA256: hash(specRaw), ResultsSHA256: hash(resultsRaw)}
	prior, err := populationPredecessor(target, outcome.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prior, source) {
		t.Fatal("source normalized or historical link edited")
	}
	if err := target.ValidateRun(); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, entry := range inventory {
		if entry.UID == "" || len(entry.SHA256) != 64 {
			t.Fatal("incomplete inventory identity/content")
		}
		counts[entry.Resource.Resource]++
	}
	want := map[string]int{"configmaps": 480, "deployments": 481, "flowschemas": 25, "namespaces": 25, "prioritylevelconfigurations": 2, "rolebindings": 25, "roles": 25, "secrets": 480, "serviceaccounts": 25, "services": 480}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("inventory incomplete: %v", counts)
	}
	var q60, q30 Spec
	var q60Outcome Outcome
	read(filepath.Join(source.ExistingPopulation.Directory, "spec.json"), &q60)
	read(filepath.Join(source.ExistingPopulation.Directory, "outcome.json"), &q60Outcome)
	var q60Inventory, q30Inventory []populationObject
	q60Raw := read(filepath.Join(source.ExistingPopulation.Directory, "population.json"), &q60Inventory)
	q30Raw := read(filepath.Join(q60.ExistingPopulation.Directory, "population.json"), &q30Inventory)
	read(filepath.Join(q60.ExistingPopulation.Directory, "spec.json"), &q30)
	if hash(q60Raw) != "a7b66e3ce276e05f6d9c8e8ace73640109ff2ff525d368e1582c8be09c85125d" || hash(q30Raw) != source.ExistingPopulation.InventorySHA256 || hash(q30Raw) != q60.ExistingPopulation.InventorySHA256 {
		t.Fatal("historical inconsistency changed")
	}
	if !q60Outcome.Finished.Before(outcome.Started) || q60Outcome.ExecutionStatus != "completed" {
		t.Fatal("q60 was not finalized before q45")
	}
	if len(q30Inventory) != len(inventory) || len(q60Inventory) != len(inventory) {
		t.Fatal("inventory sizes differ")
	}
	for i, entry := range inventory {
		for _, ancestor := range []populationObject{q60Inventory[i], q30Inventory[i]} {
			if entry.Resource != ancestor.Resource || entry.Namespace != ancestor.Namespace || entry.Name != ancestor.Name || entry.UID != ancestor.UID {
				t.Fatal("historical population identity changed")
			}
			if entry.SHA256 != ancestor.SHA256 && entry.Resource.Resource != "deployments" {
				t.Fatal("non-deployment historical content changed")
			}
		}
	}
	t.Log("q45/q60/q30 pins, identities, complete 2048-object inventory and q45 finalized screened-out proof verified OFFLINE; no live inventory proof or rescore")
	for _, mode := range []string{"policy", "checkpoint", "capture", "guard", "turnover-seed", "intent-uid", "intent-index", "pending-accepted", "counter"} {
		t.Run("repinned-q45-"+mode, func(t *testing.T) {
			var s Spec
			var o Outcome
			var r Results
			if err := json.Unmarshal(specRaw, &s); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(outRaw, &o); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(resultsRaw, &r); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "policy":
				s.RequestQualityPolicy.MaxDegradedBasisPoints = 9000
			case "checkpoint":
				r.Screening[0].Checkpoint.Completed--
				r.API.Checkpoints[0] = r.Screening[0].Checkpoint
			case "capture":
				o.CaptureReference = "unrelated"
			case "guard":
				o.GuardFailure = &GuardFailure{At: o.Started, Error: "guard canceled"}
			case "turnover-seed":
				r.Turnover.Spec.Seed++
			case "intent-uid":
				r.Turnover.Intents[0].DeploymentUID = "wrong"
			case "intent-index":
				r.Turnover.Intents[0].Index++
			case "pending-accepted":
				r.Turnover.Intents[len(r.Turnover.Intents)-1].AcceptedAt = r.Turnover.StartedAt
			case "counter":
				r.Turnover.Accepted++
			}
			if err := screenedReceipts(s, o, r, inventory); err == nil {
				t.Fatal("contradictory real receipts accepted")
			}
		})
	}
	// These objects have no volatile spec defaults: prove the actual q45 APF/RBAC
	// hashes match the native source renderer, not just the generated test fixture.
	topology, err := screenedTopology(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range inventory {
		switch entry.Resource.Resource {
		case "flowschemas", "prioritylevelconfigurations", "roles", "rolebindings":
		default:
			continue
		}
		object := topology[populationKey(entry.Resource, entry.Namespace, entry.Name)]
		content := object.DeepCopy().Object
		delete(content, "metadata")
		delete(content, "status")
		content["labels"] = object.GetLabels()
		content["owners"] = object.GetOwnerReferences()
		raw, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		if hash(raw) != entry.SHA256 {
			t.Fatalf("q45 native policy hash differs: %s/%s", entry.Resource.Resource, entry.Name)
		}
	}
}

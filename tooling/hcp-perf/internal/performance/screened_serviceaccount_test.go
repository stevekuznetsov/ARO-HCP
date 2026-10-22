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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestScreenedServiceAccountDefaults(t *testing.T) {
	for _, mode := range []string{"dockercfg", "token", "both", "empty", "uid-drift", "refs-drift", "refs-removed", "automount", "rbac", "unknown-field", "malformed-list", "empty-name", "invalid-name", "duplicate", "extra-ref-field"} {
		t.Run(mode, func(t *testing.T) {
			s := catalogSpec("a")
			client, out, r := screenedFixture(t, &s)
			gvr := schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
			ns := s.setup().CanaryNamespace()
			o, err := client.Resource(gvr).Namespace(ns).Get(t.Context(), "canary", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			refs := []any{map[string]any{"name": "canary-dockercfg-94jk8"}}
			if mode == "token" {
				refs = []any{map[string]any{"name": "canary-token-abcde"}}
			}
			if mode == "both" {
				refs = append(refs, map[string]any{"name": "canary-token-abcde"})
			}
			if mode == "empty" {
				refs = []any{}
			}
			o.Object["secrets"] = refs
			o.Object["imagePullSecrets"] = []any{map[string]any{"name": "canary-dockercfg-94jk8"}}
			if _, err := client.Resource(gvr).Namespace(ns).Update(t.Context(), o, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			inventory, err := populationInventory(t.Context(), client, s)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "uid-drift":
				o.SetUID("replacement")
			case "refs-drift":
				o.Object["secrets"] = []any{map[string]any{"name": "canary-dockercfg-other"}}
			case "refs-removed":
				delete(o.Object, "secrets")
			case "automount":
				o.Object["automountServiceAccountToken"] = true
			case "unknown-field":
				o.Object["unexpectedPolicy"] = true
			case "malformed-list":
				o.Object["secrets"] = "wrong"
			case "empty-name":
				o.Object["secrets"] = []any{map[string]any{"name": ""}}
			case "invalid-name":
				o.Object["secrets"] = []any{map[string]any{"name": "bad/name"}}
			case "duplicate":
				o.Object["secrets"] = append(refs, refs[0])
			case "extra-ref-field":
				o.Object["secrets"] = []any{map[string]any{"name": "canary-token-abcde", "namespace": "other"}}
			case "rbac":
				roles := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
				role, err := client.Resource(roles).Namespace(ns).Get(t.Context(), "canary", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				role.Object["rules"] = []any{map[string]any{"apiGroups": []any{"*"}, "resources": []any{"*"}, "verbs": []any{"*"}}}
				if _, err := client.Resource(roles).Namespace(ns).Update(t.Context(), role, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := client.Resource(gvr).Namespace(ns).Update(t.Context(), o, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			// Repinning must not make malformed references, automount or RBAC valid.
			if mode != "uid-drift" && mode != "refs-drift" && mode != "refs-removed" {
				inventory, err = populationInventory(t.Context(), client, s)
				if err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			target := s
			target.ExistingPopulation = &PopulationReference{Directory: dir, OutcomeSHA256: populationReceipt(t, dir, "outcome.json", out), InventorySHA256: populationReceipt(t, dir, "population.json", inventory), ScreenedPopulation: &ScreenedPopulationProof{SpecSHA256: populationReceipt(t, dir, "spec.json", s), ResultsSHA256: populationReceipt(t, dir, "results.json", r)}}
			before, _ := json.Marshal(o.Object)
			client.ClearActions()
			err = verifyPopulation(t.Context(), client, target, out.Identity)
			valid := mode == "dockercfg" || mode == "token" || mode == "both" || mode == "empty"
			if (err == nil) != valid {
				t.Fatalf("SA default mode %s: %v", mode, err)
			}
			if (mode == "uid-drift" || mode == "refs-drift" || mode == "refs-removed") && err.Error() != "population UID or desired content changed" {
				t.Fatalf("expected checkpoint mismatch, got %v", err)
			}
			for _, a := range client.Actions() {
				if a.GetVerb() != "get" && a.GetVerb() != "list" {
					t.Fatalf("mutation: %s", a.GetVerb())
				}
			}
			after, err := client.Resource(gvr).Namespace(ns).Get(t.Context(), "canary", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(after.Object)
			if !bytes.Equal(before, raw) {
				t.Fatal("SA modified by verification")
			}
		})
	}
}

func TestScreenedCapturedServiceAccount(t *testing.T) {
	root := os.Getenv("HCP_PERF_EVIDENCE_ROOT")
	if root == "" {
		t.Skip("captured local evidence not supplied")
	}
	load := func(p string, v any) {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	var ref PopulationReference
	load(filepath.Join(root, "medium150-20260920-v8bdelivery/existing-population.reviewed.json"), &ref)
	var s Spec
	load(filepath.Join(ref.Directory, "spec.json"), &s)
	var o unstructured.Unstructured
	load(filepath.Join(root, "medium-2400-q40-hour-v8b-20260920T1938Z/canary-sa.json"), &o.Object)
	topology, err := screenedTopology(s)
	if err != nil {
		t.Fatal(err)
	}
	want := topology["serviceaccounts/"+o.GetNamespace()+"/"+o.GetName()]
	before, _ := json.Marshal(o.Object)
	if err := screenedObject(s, &o, want); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(o.Object)
	if !bytes.Equal(before, after) {
		t.Fatal("captured object mutated")
	}
	content := o.DeepCopy().Object
	delete(content, "metadata")
	delete(content, "status")
	content["labels"] = o.GetLabels()
	content["owners"] = o.GetOwnerReferences()
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	var inventory []populationObject
	load(filepath.Join(ref.Directory, "population.json"), &inventory)
	for _, entry := range inventory {
		if entry.Resource.Resource == "serviceaccounts" && entry.Namespace == o.GetNamespace() && entry.Name == o.GetName() {
			if entry.UID != string(o.GetUID()) || entry.SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
				t.Fatal("captured SA does not match exact q45 UID/content checkpoint")
			}
			t.Log("captured canary SA matches native policy AND exact q45 UID/content hash including dockercfg references; no Secret fetched")
			return
		}
	}
	t.Fatal("SA missing from checkpoint")
}

func TestScreenedExplicitServiceAccountReferences(t *testing.T) {
	s := testSpec()
	topology, err := screenedTopology(s)
	if err != nil {
		t.Fatal(err)
	}
	want := topology["serviceaccounts/"+s.setup().CanaryNamespace()+"/canary"]
	want.Object["secrets"] = []any{map[string]any{"name": "explicit-reviewed-reference"}}
	actual := want.DeepCopy()
	if err := screenedObject(s, actual, want); err != nil {
		t.Fatal(err)
	}
	actual.Object["secrets"] = []any{map[string]any{"name": "canary-dockercfg-other"}}
	if err := screenedObject(s, actual, want); err == nil {
		t.Fatal("explicitly rendered secret reference was ignored")
	}
}

func TestScreenedCapturedTopologyAudit(t *testing.T) {
	root := os.Getenv("HCP_PERF_EVIDENCE_ROOT")
	if root == "" {
		t.Skip("captured local evidence not supplied")
	}
	s := testSpec()
	s.Seed.ID = "medium-screen-retry1434-001"
	s.Seed.NamespacePrefix = s.Seed.ID
	s.Seed.Applications = 60
	s.Seed.Image = "arohcpocpdev.azurecr.io/stevekuznetsov/pause@sha256:7031c1b283388d2c2e09b57badb803c05ebed362dc88d84b480cc47f72a21097"
	s.Seed.PerPodCPU = "10m"
	s.Seed.PerPodMemory = "32Mi"
	topology, err := screenedTopology(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"deployments", "namespaces"} {
		b, err := os.ReadFile(filepath.Join(root, "medium-population-preflight-20260918T1956Z/owned-"+resource+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var list unstructured.UnstructuredList
		if err := list.UnmarshalJSON(b); err != nil {
			t.Fatal(err)
		}
		wantCount := 61
		if resource == "namespaces" {
			wantCount = 4
		}
		if len(list.Items) != wantCount {
			t.Fatal("audit fixture count changed")
		}
		for _, o := range list.Items {
			want := topology[resource+"/"+o.GetNamespace()+"/"+o.GetName()]
			if want == nil {
				t.Fatalf("unexpected captured %s %s", resource, o.GetName())
			}
			if err := screenedObject(s, &o, want); err != nil {
				t.Errorf("captured %s/%s: %v", resource, o.GetName(), err)
			}
		}
		t.Logf("audited all %d saved %s; no live calls", len(list.Items), resource)
	}
}

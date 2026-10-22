// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package performance

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestScreenedRepinnedReceipts(t *testing.T) {
	for _, mode := range []string{"valid", "no-api", "no-source-api", "no-policy", "wrong-policy", "api-spec", "api-identity", "capture", "guard", "checkpoint-time", "checkpoint-count", "negative-count", "decision", "outcome-quality", "outcome-shed", "turnover-spec", "turnover-count", "intent-index", "intent-namespace", "intent-rollout", "intent-pending", "intent-time", "inventory-name", "inventory-rbac"} {
		t.Run(mode, func(t *testing.T) {
			s := catalogSpec("a")
			client, out, r := screenedFixture(t, &s)
			inventory, err := populationInventory(t.Context(), client, s)
			if err != nil {
				t.Fatal(err)
			}
			target := s
			switch mode {
			case "no-api":
				r.API = nil
			case "no-source-api":
				s.API = nil
			case "no-policy":
				s.RequestQualityPolicy = nil
			case "wrong-policy":
				s.RequestQualityPolicy.MaxDegradedBasisPoints = 9000
			case "api-spec":
				r.API.Spec.RatePerSecond++
			case "api-identity":
				s.Startup.ResourceID += "other"
			case "capture":
				out.CaptureReference = "another-capture"
			case "guard":
				out.GuardFailure = &GuardFailure{At: out.Started, Error: "lost guard"}
			case "checkpoint-time":
				r.Screening[0].Checkpoint.SettledAt = r.API.Started
				r.API.Checkpoints[0] = r.Screening[0].Checkpoint
			case "checkpoint-count":
				r.Screening[0].Checkpoint.Offered++
				r.API.Checkpoints[0] = r.Screening[0].Checkpoint
			case "negative-count":
				r.Screening[0].Checkpoint.Timeouts = -1
				r.API.Checkpoints[0] = r.Screening[0].Checkpoint
			case "decision":
				r.Screening[0].MaxDegradedBasisPoints++
			case "outcome-quality":
				out.Workload.RequestQuality.Dispatched++
			case "outcome-shed":
				out.Workload.Shed429++
			case "turnover-spec":
				r.Turnover.Spec.Seed++
			case "turnover-count":
				r.Turnover.Accepted++
			case "intent-index":
				r.Turnover.Intents[0].Index++
			case "intent-namespace":
				r.Turnover.Intents[0].Namespace = "other"
			case "intent-rollout":
				r.Turnover.Intents[0].RolloutID = "unrelated"
			case "intent-pending":
				r.Turnover.Intents[0].State = "pending"
			case "intent-time":
				r.Turnover.Intents[0].ScheduledAt = r.Turnover.StartedAt.Add(time.Second)
			case "inventory-name":
				inventory[0].Name = "unexpected"
			case "inventory-rbac":
				for i, o := range inventory {
					if o.Resource.Resource == "roles" {
						inventory = append(inventory[:i], inventory[i+1:]...)
						break
					}
				}
			}
			dir := t.TempDir()
			target.ExistingPopulation = &PopulationReference{Directory: dir, OutcomeSHA256: populationReceipt(t, dir, "outcome.json", out), InventorySHA256: populationReceipt(t, dir, "population.json", inventory), ScreenedPopulation: &ScreenedPopulationProof{SpecSHA256: populationReceipt(t, dir, "spec.json", s), ResultsSHA256: populationReceipt(t, dir, "results.json", r)}}
			_, err = populationPredecessor(target, out.Identity)
			if (err == nil) != (mode == "valid") {
				t.Fatalf("repinned receipt %s: %v", mode, err)
			}
		})
	}
}

func TestScreenedRepinnedTopology(t *testing.T) {
	for _, mode := range []string{"valid-defaults", "plc", "flow-schema", "role", "rolebinding", "sa", "missing-role", "missing-binding", "missing-sa", "unexpected-name", "image", "cpu", "replicas", "sidecar", "command", "selector", "template-label", "service-selector", "configmap", "secret", "canary-template"} {
		t.Run(mode, func(t *testing.T) {
			s := catalogSpec("a")
			client, out, r := screenedFixture(t, &s)
			gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
			ns, name := "fresh-0", "app-000000"
			switch mode {
			case "plc":
				gvr = schema.GroupVersionResource{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "prioritylevelconfigurations"}
				ns = ""
				name = "hcp-perf-benchmark-" + s.Seed.ID
			case "flow-schema":
				gvr = schema.GroupVersionResource{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "flowschemas"}
				ns = ""
				name = "hcp-perf-benchmark-" + s.Seed.ID + "-0"
			case "role", "missing-role":
				gvr = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
				name = "benchmark"
			case "rolebinding", "missing-binding":
				gvr = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
				name = "benchmark"
			case "sa", "missing-sa":
				gvr = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
				name = "benchmark"
			case "service-selector":
				gvr = schema.GroupVersionResource{Version: "v1", Resource: "services"}
			case "configmap":
				gvr = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
			case "secret":
				gvr = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
			case "canary-template":
				ns = s.setup().CanaryNamespace()
				name = "canary"
			}
			o, err := client.Resource(gvr).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			set := func(value any, path ...string) {
				t.Helper()
				if err := unstructured.SetNestedField(o.Object, value, path...); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "valid-defaults":
				set(int64(600), "spec", "progressDeadlineSeconds")
				set("Always", "spec", "template", "spec", "restartPolicy")
			case "plc":
				set(int64(999), "spec", "limited", "nominalConcurrencyShares")
			case "flow-schema":
				set(int64(1), "spec", "matchingPrecedence")
			case "role":
				set([]any{map[string]any{"apiGroups": []any{"*"}, "resources": []any{"*"}, "verbs": []any{"*"}}}, "rules")
			case "rolebinding":
				set("cluster-admin", "roleRef", "name")
			case "sa":
				set(true, "automountServiceAccountToken")
			case "image", "cpu", "sidecar", "command", "canary-template":
				containers, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
				c := containers[0].(map[string]any)
				if mode == "image" || mode == "canary-template" {
					c["image"] = "wrong-image"
				}
				if mode == "cpu" {
					c["resources"].(map[string]any)["requests"].(map[string]any)["cpu"] = "100m"
				}
				if mode == "sidecar" {
					containers = append(containers, map[string]any{"name": "other", "image": s.Seed.Image})
				}
				if mode == "command" {
					c["command"] = []any{"sh"}
				}
				set(containers, "spec", "template", "spec", "containers")
			case "replicas":
				set(int64(6), "spec", "replicas")
			case "selector":
				set("wrong", "spec", "selector", "matchLabels", "hcp-perf.azure.com/app-index")
			case "template-label":
				set("wrong", "spec", "template", "metadata", "labels", "hcp-perf.azure.com/app-index")
			case "service-selector":
				set("wrong", "spec", "selector", "hcp-perf.azure.com/app-index")
			case "configmap":
				set("wrong", "data", "synthetic")
			case "secret":
				set("d3Jvbmc=", "data", "synthetic")
			}
			if strings.HasPrefix(mode, "missing-") || mode == "unexpected-name" {
				if err := client.Resource(gvr).Namespace(ns).Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				if mode == "unexpected-name" {
					o.SetName("unexpected")
					if _, err := client.Resource(gvr).Namespace(ns).Create(t.Context(), o, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
			} else if _, err := client.Resource(gvr).Namespace(ns).Update(t.Context(), o, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			inventory, err := populationInventory(t.Context(), client, s)
			if mode == "replicas" {
				if err == nil {
					t.Fatal("unconverged replicas accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			target := s
			target.ExistingPopulation = &PopulationReference{Directory: dir, OutcomeSHA256: populationReceipt(t, dir, "outcome.json", out), InventorySHA256: populationReceipt(t, dir, "population.json", inventory), ScreenedPopulation: &ScreenedPopulationProof{SpecSHA256: populationReceipt(t, dir, "spec.json", s), ResultsSHA256: populationReceipt(t, dir, "results.json", r)}}
			client.ClearActions()
			err = verifyPopulation(t.Context(), client, target, out.Identity)
			if (err == nil) != (mode == "valid-defaults") {
				t.Fatalf("matching repinned topology %s: %v", mode, err)
			}
			for _, a := range client.Actions() {
				if a.GetVerb() != "get" && a.GetVerb() != "list" {
					t.Fatalf("verifier mutated: %s", a.GetVerb())
				}
			}
		})
	}
}

func TestScreenedNativeSavedDeployment(t *testing.T) {
	root := os.Getenv("HCP_PERF_EVIDENCE_ROOT")
	if root == "" {
		t.Skip("local saved deployment fixture not supplied")
	}
	raw, err := os.ReadFile(filepath.Join(root, "medium-population-preflight-20260918T1956Z/owned-deployments.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list unstructured.UnstructuredList
	if err := list.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	for _, o := range list.Items {
		if o.GetName() != "app-000000" && o.GetName() != "canary" {
			continue
		}
		s := testSpec()
		s.Seed.ID = o.GetLabels()["hcp-perf.azure.com/run-id"]
		s.Seed.NamespacePrefix = s.Seed.ID
		s.Seed.Image = "arohcpocpdev.azurecr.io/stevekuznetsov/pause@sha256:7031c1b283388d2c2e09b57badb803c05ebed362dc88d84b480cc47f72a21097"
		s.Seed.PerPodCPU = "10m"
		s.Seed.PerPodMemory = "32Mi"
		objects, err := screenedTopology(s)
		if err != nil {
			t.Fatal(err)
		}
		want := objects["deployments/"+o.GetNamespace()+"/"+o.GetName()]
		if want == nil {
			t.Fatal("fixture topology missing")
		}
		before, _ := json.Marshal(o.Object)
		if err := screenedObject(s, &o, want); err != nil {
			t.Fatalf("saved native Deployment %s: %v", o.GetName(), err)
		}
		after, _ := json.Marshal(o.Object)
		if !bytes.Equal(before, after) {
			t.Fatal("validation mutated source object")
		}
	}
}

func TestScreenedRecordedRollout(t *testing.T) {
	for _, mode := range []string{"accepted-failed-converged", "marker-mismatch", "uid-mismatch", "pending-write", "pending-counter", "pending-current-marker"} {
		t.Run(mode, func(t *testing.T) {
			s := catalogSpec("a")
			s.Seed.Applications = 2
			client, out, r := screenedFixture(t, &s)
			i := &r.Turnover.Intents[0]
			gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
			o, err := client.Resource(gvr).Namespace(i.Namespace).Get(t.Context(), i.Deployment, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			i.AttemptedAt = i.AdmittedAt
			i.AcceptedAt = i.AdmittedAt
			i.Attempts = 1
			i.DeploymentUID = o.GetUID()
			i.Generation = 2
			r.Turnover.Accepted = 1
			marker := i.RolloutID
			if mode == "marker-mismatch" {
				marker = strings.Repeat("b", 32) + "-000000"
			}
			if mode == "uid-mismatch" {
				i.DeploymentUID = "replacement"
			}
			if mode == "pending-write" {
				r.Turnover.Intents[1].AttemptedAt = r.Turnover.StartedAt
			}
			if mode == "pending-counter" {
				r.Turnover.NotStarted = 0
			}
			if err := unstructured.SetNestedField(o.Object, marker, "spec", "template", "metadata", "annotations", "hcp-perf.azure.com/rollout-id"); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Resource(gvr).Namespace(i.Namespace).Update(t.Context(), o, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if mode == "pending-current-marker" {
				pending := r.Turnover.Intents[1]
				o, err := client.Resource(gvr).Namespace(pending.Namespace).Get(t.Context(), pending.Deployment, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if err := unstructured.SetNestedField(o.Object, pending.RolloutID, "spec", "template", "metadata", "annotations", "hcp-perf.azure.com/rollout-id"); err != nil {
					t.Fatal(err)
				}
				if _, err := client.Resource(gvr).Namespace(pending.Namespace).Update(t.Context(), o, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			inventory, err := populationInventory(t.Context(), client, s)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			target := s
			target.ExistingPopulation = &PopulationReference{Directory: dir, OutcomeSHA256: populationReceipt(t, dir, "outcome.json", out), InventorySHA256: populationReceipt(t, dir, "population.json", inventory), ScreenedPopulation: &ScreenedPopulationProof{SpecSHA256: populationReceipt(t, dir, "spec.json", s), ResultsSHA256: populationReceipt(t, dir, "results.json", r)}}
			client.ClearActions()
			err = verifyPopulation(t.Context(), client, target, out.Identity)
			if (err == nil) != (mode == "accepted-failed-converged") {
				t.Fatalf("rollout proof: %v", err)
			}
			for _, a := range client.Actions() {
				if a.GetVerb() != "get" && a.GetVerb() != "list" {
					t.Fatal("verification mutated")
				}
			}
		})
	}
}

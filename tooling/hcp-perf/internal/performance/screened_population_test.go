// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package performance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func screenedFixture(t *testing.T, s *Spec) (*dynamicfake.FakeDynamicClient, Outcome, Results) {
	t.Helper()
	s.API = &apiload.Spec{Version: "v1", DurationSeconds: int(s.MeasurementSeconds), RatePerSecond: 2, MaxInFlight: 4, RequestTimeoutSeconds: 10, Namespaces: []string{"fresh-0"}, Targets: []apiload.Target{{Namespace: "fresh-0", APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Name: "app-000000", Weight: 1}}}
	s.RequestQualityPolicy = &RequestQualityPolicy{MaxDegradedBasisPoints: 500, ScreeningSeconds: []int{60}, MinimumCompletedRequests: 100}
	objects, err := screenedTopology(*s)
	if err != nil {
		t.Fatal(err)
	}
	lists := map[schema.GroupVersionResource]string{{Version: "v1", Resource: "pods"}: "PodList"}
	var runtimeObjects []runtime.Object
	for key, o := range objects {
		o.SetUID(types.UID(key))
		o.SetGeneration(1)
		r := schema.GroupVersionResource{Group: o.GroupVersionKind().Group, Version: o.GroupVersionKind().Version, Resource: strings.Split(key, "/")[0]}
		lists[r] = o.GetKind() + "List"
		if o.GetKind() == "Deployment" {
			n, _, _ := unstructured.NestedInt64(o.Object, "spec", "replicas")
			o.Object["status"] = map[string]any{"replicas": n, "readyReplicas": n, "updatedReplicas": n, "observedGeneration": int64(1)}
		}
		runtimeObjects = append(runtimeObjects, o)
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, runtimeObjects...)
	start := time.Date(2026, 9, 20, 17, 0, 0, 0, time.UTC)
	checkpoint := apiload.Checkpoint{AfterSeconds: 60, SettledAt: start.Add(60 * time.Second), Counts: apiload.Counts{Offered: 120, Dispatched: 120, HTTPAttempts: 120, Completed: 120, Successful: 100, Rejected429: 20, DegradedRequests: 20}}
	apiSpec, err := s.API.Validate()
	if err != nil {
		t.Fatal(err)
	}
	r := Results{API: &apiload.Result{Version: "v1", Status: "canceled", RequestAccountingVersion: apiload.RequestAccountingVersion, Spec: apiSpec, Started: start, SchedulingStopped: checkpoint.SettledAt, Finished: checkpoint.SettledAt, IntendedQPS: 2, Counts: checkpoint.Counts, Checkpoints: []apiload.Checkpoint{checkpoint}}, Screening: []ScreeningDecision{screenRequests(*s.RequestQualityPolicy, checkpoint)}}
	r.Turnover = &workload.TurnoverResult{Version: "v1", RunID: strings.Repeat("a", 32), Spec: workload.TurnoverSpec{Version: "v1", GuestNodes: s.Seed.GuestNodes, Applications: s.Seed.Applications, Seed: s.ScheduleSeed, NamespacePrefix: s.Seed.NamespacePrefix, DurationSeconds: s.MeasurementSeconds, TurnoverPeriodSeconds: 3600, RecoverySeconds: s.RecoverySeconds, MaxOutstanding: s.MaxOutstanding}, StartedAt: start, AdmissionEnd: start.Add(time.Duration(s.MeasurementSeconds) * time.Second), FinishedAt: checkpoint.SettledAt}
	schedule, err := experiment.RolloutsForPeriod(s.Seed.Applications, s.ScheduleSeed, time.Duration(s.MeasurementSeconds)*time.Second, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range schedule {
		ns, name := workload.TurnoverApplication(s.Seed.NamespacePrefix, v.Application)
		intent := workload.TurnoverIntent{Index: i, Intent: v, Namespace: ns, Deployment: name, RolloutID: fmt.Sprintf("%s-%06d", r.Turnover.RunID, i), State: "pending", ScheduledAt: start.Add(v.Offset)}
		if !intent.ScheduledAt.After(checkpoint.SettledAt) {
			intent.State = "failed"
			intent.AdmittedAt = intent.ScheduledAt
			intent.FinishedAt = checkpoint.SettledAt
			intent.Error = "context canceled"
			r.Turnover.Admitted++
			r.Turnover.Failed++
			r.Turnover.PeakOutstanding = 1
		} else {
			r.Turnover.NotStarted++
		}
		r.Turnover.Intents = append(r.Turnover.Intents, intent)
	}
	r.Turnover.Scheduled = len(schedule)
	fraction := float64(20) / 120
	out := Outcome{State: "failed", ExecutionStatus: "completed", Phase: "recovery", Started: start.Add(-time.Minute), Finished: start.Add(2 * time.Minute), CleanupStatus: "not-needed", PopulationRetained: true, Identity: startup.Identity{ClusterID: "cluster", HCPUID: "hcp", NamespaceUID: "namespace", ResourceID: s.Startup.ResourceID}, CaptureReference: s.Capture.Reference, Workload: Assessment{Status: "screened-out", Shed429: 20, Violations: []string{"predeclared request quality screening rejected the point; full measurement not completed"}, RequestQuality: &RequestQualityAssessment{Status: "exceeded", CapacityStatus: "screened-out", MaxDegradedBasisPoints: 500, Dispatched: 120, DegradedRequests: 20, Rejected429: 20, DegradedFraction: &fraction}}}
	return client, out, r
}

func TestScreenedPopulation(t *testing.T) {
	for _, mode := range []string{"own", "binding", "legacy", "recover", "both", "failed", "interrupted", "unfinished", "reversed-time", "cleanup", "not-retained", "phase", "full-window", "not-screened", "outstanding", "backlog", "unfinished-turnover", "no-turnover", "no-screen", "missing-spec", "missing-outcome", "missing-results", "missing-population", "bad-spec", "bad-outcome", "bad-results", "bad-population", "malformed-pin", "seed", "node", "admission", "startup", "missing-binding", "identity", "unloaded", "restore", "partial", "uid", "content", "rollout", "unconverged", "canary-replicas", "canary-pods", "canary-list-error"} {
		t.Run(mode, func(t *testing.T) {
			source := catalogSpec("a")
			// This historical link is deliberately invalid and must remain untouched.
			source.ExistingPopulation = &PopulationReference{Directory: "/missing-history", RecoverFailed: true, OutcomeSHA256: strings.Repeat("a", 64), InventorySHA256: strings.Repeat("b", 64)}
			client, out, results := screenedFixture(t, &source)
			id := out.Identity
			inventory, err := populationInventory(t.Context(), client, source)
			if err != nil {
				t.Fatal(err)
			}
			target := catalogSpec("a")
			dir := t.TempDir()
			switch mode {
			case "failed":
				out.ExecutionStatus = "failed"
			case "interrupted":
				out.State, out.ExecutionStatus = "interrupted", "interrupted"
			case "unfinished":
				out.Finished = time.Time{}
			case "reversed-time":
				out.Finished = out.Started.Add(-time.Second)
			case "cleanup":
				out.CleanupStatus = "unknown"
			case "not-retained":
				out.PopulationRetained = false
			case "phase":
				out.Phase = "seed"
			case "full-window":
				out.Workload.FullWindow = true
			case "not-screened":
				out.Workload.Status = "violated"
			case "outstanding":
				results.Turnover.Outstanding = 1
			case "backlog":
				results.Turnover.Backlog = 1
			case "unfinished-turnover":
				results.Turnover.FinishedAt = time.Time{}
			case "no-turnover":
				results.Turnover = nil
			case "no-screen":
				results.Screening = nil
			case "seed":
				target.Seed.ID = "other"
			case "node":
				target.ExpectedGuestNodes[0].UID = "other"
			case "admission":
				target.Admission = &workload.AdmissionPolicy{}
			case "startup":
				target.Startup.CandidateSpec = json.RawMessage(`{"changed":true}`)
			case "binding", "missing-binding":
				target = catalogSpec("b")
			case "identity":
				out.Identity.HCPUID = "other"
			case "unloaded":
				source.Unloaded = true
			case "restore":
				target.RestorePopulation = true
			case "partial":
				inventory = inventory[:len(inventory)-1]
			}
			ref := &PopulationReference{Directory: dir, OutcomeSHA256: populationReceipt(t, dir, "outcome.json", out), InventorySHA256: populationReceipt(t, dir, "population.json", inventory), ScreenedPopulation: &ScreenedPopulationProof{SpecSHA256: populationReceipt(t, dir, "spec.json", source), ResultsSHA256: populationReceipt(t, dir, "results.json", results)}}
			target.ExistingPopulation = ref
			if mode == "binding" {
				ref.ImageBindings = catalogBindings("a", "b")
			}
			if mode == "legacy" || mode == "recover" {
				ref.ScreenedPopulation = nil
			}
			if mode == "recover" || mode == "both" {
				ref.RecoverFailed = true
			}
			if mode == "malformed-pin" {
				ref.ScreenedPopulation.SpecSHA256 = strings.Repeat("z", 64)
			}
			for _, name := range []string{"spec", "outcome", "results", "population"} {
				if mode == "missing-"+name {
					if err := os.Remove(filepath.Join(dir, name+".json")); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "bad-"+name {
					if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
			ns, name := "fresh-0", "app-000000"
			if mode == "canary-replicas" {
				ns, name = source.setup().CanaryNamespace(), "canary"
			}
			o, err := client.Resource(gvr).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "uid":
				o.SetUID("replacement")
			case "content":
				o.Object["spec"].(map[string]any)["other"] = true
			case "rollout":
				if err := unstructured.SetNestedField(o.Object, "unrecorded", "spec", "template", "metadata", "annotations", workload.RolloutIDAnnotation); err != nil {
					t.Fatal(err)
				}
			case "unconverged":
				o.Object["status"].(map[string]any)["readyReplicas"] = int64(4)
			case "canary-replicas":
				o.Object["status"].(map[string]any)["replicas"] = int64(1)
			}
			if _, err := client.Resource(gvr).Namespace(ns).Update(t.Context(), o, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if mode == "canary-pods" {
				pod := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "remaining"}}}
				if _, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Namespace(source.setup().CanaryNamespace()).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string][]byte{}
			if mode == "canary-list-error" {
				client.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("pod list unavailable")
				})
			}
			for _, name := range []string{"spec", "outcome", "results", "population"} {
				files[name], _ = os.ReadFile(filepath.Join(dir, name+".json"))
			}
			client.ClearActions()
			err = verifyPopulation(t.Context(), client, target, id)
			valid := mode == "own" || mode == "binding"
			if (err == nil) != valid {
				t.Fatalf("reuse (%s): %v", mode, err)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" && action.GetVerb() != "list" {
					t.Fatalf("mutation: %s", action.GetVerb())
				}
			}
			for name, before := range files {
				after, _ := os.ReadFile(filepath.Join(dir, name+".json"))
				if !bytes.Equal(before, after) {
					t.Fatalf("receipt changed: %s", name)
				}
			}
			if valid {
				raw, err := json.Marshal(target)
				if err != nil {
					t.Fatal(err)
				}
				var frozen Spec
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&frozen); err != nil {
					t.Fatal(err)
				}
				if err := verifyPopulation(t.Context(), client, frozen, id); err != nil {
					t.Fatalf("round trip: %v", err)
				}
			}
		})
	}
}

func TestScreenedPopulationTypedValidation(t *testing.T) {
	for _, mode := range []string{"valid", "both", "no-spec-pin", "no-results-pin", "bad-outcome-pin", "unloaded", "restore", "unknown-field"} {
		t.Run(mode, func(t *testing.T) {
			s := catalogSpec("a")
			s.ExistingPopulation = &PopulationReference{Directory: "/previous", OutcomeSHA256: strings.Repeat("a", 64), InventorySHA256: strings.Repeat("b", 64), ScreenedPopulation: &ScreenedPopulationProof{SpecSHA256: strings.Repeat("c", 64), ResultsSHA256: strings.Repeat("d", 64)}}
			switch mode {
			case "both":
				s.ExistingPopulation.RecoverFailed = true
			case "no-spec-pin":
				s.ExistingPopulation.ScreenedPopulation.SpecSHA256 = ""
			case "no-results-pin":
				s.ExistingPopulation.ScreenedPopulation.ResultsSHA256 = ""
			case "bad-outcome-pin":
				s.ExistingPopulation.OutcomeSHA256 = strings.Repeat("z", 64)
			case "unloaded":
				s.Unloaded = true
			case "restore":
				s.RestorePopulation = true
			}
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unknown-field" {
				raw = bytes.ReplaceAll(raw, []byte(`"resultsSHA256"`), []byte(`"resultsTypo"`))
			}
			var decoded Spec
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			err = d.Decode(&decoded)
			if err == nil {
				err = decoded.ValidateRun()
			}
			if (err == nil) != (mode == "valid") {
				t.Fatalf("typed validation: %v", err)
			}
		})
	}
}

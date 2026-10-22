// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package performance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestUnloadedResultHasNoTurnover(t *testing.T) {
	s := testSpec()
	s.Unloaded = true
	s.API = &apiload.Spec{DurationSeconds: 3600, RatePerSecond: 1}
	start := time.Now()
	result := Results{API: &apiload.Result{Status: "completed", Started: start, SchedulingStopped: start.Add(time.Hour), Counts: apiload.Counts{Offered: 3600, Dispatched: 3600, Completed: 3600, Successful: 3600}}}
	if a, err := ValidateResult(s, result, false); err != nil || !a.FullWindow {
		t.Fatalf("unloaded read window rejected: %+v %v", a, err)
	}
}

func TestUnloadedScaleRejectsReplacedDeployment(t *testing.T) {
	s := testSpec()
	dir := t.TempDir()
	s.ExistingPopulation = &PopulationReference{Directory: dir}
	s.Unloaded = true
	prior := s
	prior.Unloaded = false
	raw, _ := json.Marshal(prior)
	if err := os.WriteFile(filepath.Join(dir, "spec.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	raw = []byte(`[{"resource":{"group":"apps","version":"v1","resource":"deployments"},"namespace":"fresh-0","name":"app-000000","uid":"old"}]`)
	if err := os.WriteFile(filepath.Join(dir, "population.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	replicas := int32(5)
	client := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app-000000", Namespace: "fresh-0", UID: "replacement", Labels: map[string]string{"hcp-perf.azure.com/run-id": s.Seed.ID}}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}})
	if err := unloadPopulation(t.Context(), client, s, nil, []populationObject{{Namespace: "fresh-0", Name: "app-000000", UID: "old"}}); err == nil {
		t.Fatal("accepted replacement deployment")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatal("patched a replacement deployment")
		}
	}
}

func TestRestorePopulationRequiresExplicitQuietMode(t *testing.T) {
	s := testSpec()
	s.RestorePopulation = true
	if s.Validate() == nil {
		t.Fatal("restore accepted without predecessor")
	}
	s.ExistingPopulation = &PopulationReference{Directory: "/previous", OutcomeSHA256: strings.Repeat("a", 64), InventorySHA256: strings.Repeat("b", 64)}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.Unloaded = true
	if s.Validate() == nil {
		t.Fatal("conflicting scale directions accepted")
	}
}

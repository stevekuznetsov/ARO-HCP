// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package performance

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

func TestPopulationRejectsUnprovenPredecessor(t *testing.T) {
	for _, mode := range []string{"hash", "failed", "identity", "seed", "node-uid", "node-count", "applications"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			spec := testSpec()
			spec.ExistingPopulation = &PopulationReference{Directory: dir, InventorySHA256: strings.Repeat("a", 64)}
			id := startup.Identity{ClusterID: "cluster"}
			out := Outcome{State: "completed", ExecutionStatus: "completed", Finished: time.Now(), Identity: id, Workload: Assessment{FullWindow: true}}
			if mode == "failed" {
				out.State = "failed"
				out.ExecutionStatus = "failed"
			}
			if mode == "identity" {
				out.Identity.ClusterID = "other"
			}
			raw, _ := json.Marshal(out)
			if err := os.WriteFile(filepath.Join(dir, "outcome.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			spec.ExistingPopulation.OutcomeSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
			if mode == "hash" {
				spec.ExistingPopulation.OutcomeSHA256 = strings.Repeat("0", 64)
			}
			if mode == "seed" || mode == "node-uid" || mode == "node-count" || mode == "applications" {
				prior := spec
				prior.ExpectedGuestNodes = append([]GuestNodeIdentity(nil), spec.ExpectedGuestNodes...)
				switch mode {
				case "seed":
					prior.Seed.ID = "different"
				case "node-uid":
					prior.ExpectedGuestNodes[0].UID = "replaced"
				case "node-count":
					prior.ExpectedGuestNodes = prior.ExpectedGuestNodes[:2]
					prior.Seed.GuestNodes = 2
				case "applications":
					prior.Seed.Applications++
				}
				raw, _ = json.Marshal(prior)
				if err := os.WriteFile(filepath.Join(dir, "spec.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := verifyPopulation(t.Context(), nil, spec, id)
			if err == nil {
				t.Fatal("unproven predecessor accepted")
			}
			if (mode == "seed" || mode == "node-uid" || mode == "node-count" || mode == "applications") && err.Error() != "population policy changed" {
				t.Fatalf("expected rejection before live inventory reads: %v", err)
			}
		})
	}
}

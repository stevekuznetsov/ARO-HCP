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

package main

import (
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
)

func TestCampaignHourPrimitiveContracts(t *testing.T) {
	for _, apps := range []int64{1, 7, 120, 240} {
		schedule, err := experiment.RolloutsForPeriod(apps, 42, time.Hour, time.Hour)
		if err != nil || int64(len(schedule)) != apps {
			t.Fatalf("hourly schedule for %d apps: count=%d err=%v", apps, len(schedule), err)
		}
		seen := map[int64]bool{}
		for _, event := range schedule {
			if seen[event.Application] || event.Round != 1 || event.Offset < 0 || event.Offset >= time.Hour {
				t.Fatalf("each application must appear once before the exclusive hour end: %+v", event)
			}
			seen[event.Application] = true
		}
	}
	for _, rate := range []int{30, 50, 278} {
		seed := int64(42)
		spec := apiload.Spec{Version: "v1", DurationSeconds: 3600, RatePerSecond: rate,
			MaxInFlight: 128, RequestTimeoutSeconds: 10, Seed: &seed, Namespaces: []string{"apps-0"},
			Targets: []apiload.Target{{Namespace: "apps-0", APIVersion: "apps/v1", Resource: "deployments", Verb: "get", Name: "app-000000", Weight: 2}}}
		_, err := spec.Validate()
		if (err != nil) != (rate*3600 > 1000000) {
			t.Fatalf("hour API offer budget at %d QPS: %v", rate, err)
		}
	}
}

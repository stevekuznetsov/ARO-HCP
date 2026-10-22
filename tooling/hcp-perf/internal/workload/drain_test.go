// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package workload

import (
	"context"
	"k8s.io/utils/ptr"
	"testing"
	"time"
)

func TestWaitCanaryDrainedReadOnly(t *testing.T) {
	for _, mode := range []string{"drained", "replaced", "still-one", "remaining-pod", "unknown-rs", "replaced-rs", "stale-generation"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture()
			switch mode {
			case "replaced":
				f.deployment.UID = "other"
			case "still-one":
				f.deployment.Spec.Replicas = ptr.To(int32(1))
			case "remaining-pod":
				f.pods = append(f.pods, newPod())
			case "unknown-rs":
				pod := newPod()
				pod.OwnerReferences[0].UID = "deleted-rs"
				f.pods = append(f.pods, pod)
			case "replaced-rs":
				f.pods = append(f.pods, newPod())
				f.set.UID = "replacement-rs"
			case "stale-generation":
				f.deployment.Status.ObservedGeneration--
			}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			_, err := WaitCanaryDrained(ctx, f.client, testSpec(), "deployment")
			if (err == nil) != (mode == "drained") {
				t.Fatalf("drain: %v", err)
			}
			if len(f.writes) != 0 {
				t.Fatal("recovery performed a mutation")
			}
		})
	}
}

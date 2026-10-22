// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package startup

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

func TestCandidateNumericParityAndPrecision(t *testing.T) {
	expected, err := DecodeCandidateSpec([]byte(`{"concurrency":{"limit":5},"sizes":[{"from":9007199254740993}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !candidateSpecEqual(map[string]any{"concurrency": map[string]any{"limit": float64(5)}, "sizes": []any{map[string]any{"from": int64(9007199254740993)}}}, expected) {
		t.Fatal("integer/JSON parity failed")
	}
	if candidateSpecEqual(map[string]any{"concurrency": map[string]any{"limit": int64(5)}, "sizes": []any{map[string]any{"from": int64(9007199254740992)}}}, expected) {
		t.Fatal("large integer precision lost")
	}
	for _, data := range []string{`{"limit":1.5}`, `{"limit":9223372036854775808}`, `{"limit":5.0}`, `{"limit":1e2}`} {
		if _, err := DecodeCandidateSpec([]byte(data)); err == nil {
			t.Fatalf("accepted invalid integer encoding %s", data)
		}
	}
}

func FuzzCandidateIntegerPrecision(f *testing.F) {
	for _, value := range []int64{0, 5, 9007199254740993, 9223372036854775807, -9223372036854775808} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value int64) {
		expected, err := DecodeCandidateSpec([]byte(fmt.Sprintf(`{"nested":[{"value":%d}]}`, value)))
		if err != nil {
			t.Fatal(err)
		}
		actual := map[string]any{"nested": []any{map[string]any{"value": value}}}
		if !candidateSpecEqual(actual, expected) {
			t.Fatal("exact integer lost")
		}
		if value < 9223372036854775807 {
			actual["nested"] = []any{map[string]any{"value": value + 1}}
			if candidateSpecEqual(actual, expected) {
				t.Fatal("distinct integers compare equal")
			}
		}
	})
}

func TestNumericCandidateApplyAndPreflightDisposition(t *testing.T) {
	for _, scenario := range []string{"equal", "uid", "rv", "dry-run", "update"} {
		t.Run(scenario, func(t *testing.T) {
			live := candidateObject()
			live.Object["spec"] = map[string]any{"concurrency": map[string]any{"limit": int64(5)}}
			client := dynamicClient(live)
			candidate := Candidate{Kubeconfig: "/external/kube", Name: "cluster", UID: "csc-uid", ResourceVersion: "7", ExpectedSpec: json.RawMessage(`{"concurrency":{"limit":5}}`), DesiredSpec: json.RawMessage(`{"concurrency":{"limit":6}}`)}
			if scenario == "uid" {
				candidate.UID = "other"
			}
			if scenario == "rv" {
				candidate.ResourceVersion = "8"
			}
			writes := 0
			client.PrependReactor("update", "clustersizingconfigurations", func(action clienttesting.Action) (bool, runtime.Object, error) {
				update := action.(clienttesting.UpdateActionImpl)
				if len(update.UpdateOptions.DryRun) > 0 {
					if scenario == "dry-run" {
						return true, nil, errors.New("rejected")
					}
					return true, update.Object.DeepCopyObject(), nil
				}
				writes++
				if scenario == "update" {
					return true, nil, errors.New("unknown delivery")
				}
				return false, nil, nil
			})
			err := Apply(t.Context(), Clients{Dynamic: client}, candidate)
			var noMutation *NoMutationError
			if scenario == "equal" {
				if err != nil || writes != 1 {
					t.Fatalf("numeric apply failed: %v writes=%d", err, writes)
				}
			} else if scenario == "update" {
				if err == nil || errors.As(err, &noMutation) {
					t.Fatal("post-dispatch error claimed no mutation")
				}
			} else if !errors.As(err, &noMutation) || writes != 0 {
				t.Fatalf("preflight disposition incorrect: %v writes=%d", err, writes)
			}
		})
	}
}

func TestVerifyUnchangedRequiresOriginalUIDAndRV(t *testing.T) {
	live := candidateObject()
	live.Object["spec"] = map[string]any{"limit": int64(5)}
	client := dynamicClient(live)
	candidate := Candidate{Kubeconfig: "/external/kube", Name: "cluster", UID: "csc-uid", ResourceVersion: "7", ExpectedSpec: json.RawMessage(`{"limit":5}`), DesiredSpec: json.RawMessage(`{"limit":6}`)}
	if err := VerifyUnchanged(t.Context(), Clients{Dynamic: client}, candidate); err != nil {
		t.Fatal(err)
	}
	live.SetResourceVersion("8")
	if _, err := client.Resource(SizingConfigurations).Update(t.Context(), live, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if VerifyUnchanged(t.Context(), Clients{Dynamic: client}, candidate) == nil {
		t.Fatal("spec equality alone cleared changed RV")
	}
}

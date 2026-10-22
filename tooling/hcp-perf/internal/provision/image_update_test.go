// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provision

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestImageUpdateGuardAndOnlyTagChange(t *testing.T) {
	image := "registry/cpo@sha256:" + strings.Repeat("b", 64)
	for _, scenario := range []string{"success", "changed", "missing", "busy", "wrong-id", "same-image"} {
		t.Run(scenario, func(t *testing.T) {
			id := "/subscriptions/" + subID + "/resourceGroups/customer-rg/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/perf-cluster"
			expected := json.RawMessage(fmt.Sprintf(`{"id":%q,"location":"eastus","tags":{%q:%q,"keep":"value"},"properties":{"provisioningState":"Succeeded","version":{"id":"4.21"}}}`, id, cpoImageTag, "registry/cpo@sha256:"+strings.Repeat("a", 64)))
			puts := 0
			c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/subscriptions/"+subID) {
					fmt.Fprint(w, registered)
					return
				}
				if r.Method == http.MethodGet {
					if scenario == "missing" {
						w.WriteHeader(404)
						return
					}
					body := string(expected)
					if scenario == "changed" {
						body = strings.Replace(body, "value", "changed", 1)
					}
					if scenario == "busy" {
						body = strings.Replace(body, "Succeeded", "Updating", 1)
					}
					fmt.Fprint(w, body)
					return
				}
				if r.Method != http.MethodPut {
					t.Errorf("unexpected mutation %s", r.Method)
					return
				}
				puts++
				body, _ := io.ReadAll(r.Body)
				want, err := ImageUpdatePayload(expected, image)
				if err != nil || !sameResourceJSON(body, want) {
					t.Errorf("unexpected update payload %s", body)
				}
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, string(want))
			})
			if scenario == "wrong-id" {
				expected = json.RawMessage(strings.Replace(string(expected), "perf-cluster", "other-cluster", 1))
			}
			selected := image
			if scenario == "same-image" {
				selected = "registry/cpo@sha256:" + strings.Repeat("a", 64)
			}
			_, err := c.UpdateClusterImage(t.Context(), ref, expected, selected, output(t))
			if scenario == "success" {
				if err != nil || puts != 1 {
					t.Fatalf("update failed %v puts%d", err, puts)
				}
			} else if err == nil || puts != 0 {
				t.Fatalf("unsafe update %v puts%d", err, puts)
			}
		})
	}
}

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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func nodePoolSnapshot() json.RawMessage {
	return json.RawMessage(`{"id":"/subscriptions/` + subID + `/resourceGroups/customer-rg/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/perf-cluster/nodePools/workers","location":"eastus","tags":{"keep":"value"},"systemData":{"createdBy":"original"},"properties":{"provisioningState":"Succeeded","replicas":2,"autoScaling":null,"platform":{"vmSize":"Standard_D4s_v5","subnetId":"/original/subnet","osDisk":{"sizeGiB":128}},"version":{"id":"4.21","channelGroup":"stable"},"autoRepair":true}}`)
}

func TestNodePoolScaleOutGuard(t *testing.T) {
	for _, scenario := range []string{"success", "busy", "wrong-target", "drift", "missing", "downscale", "same-count", "zero", "negative", "above-bound", "autoscaling", "empty-autoscaling", "missing-count", "fractional-count", "incomplete", "cluster-target"} {
		t.Run(scenario, func(t *testing.T) {
			expected, live := nodePoolSnapshot(), nodePoolSnapshot()
			target := ref
			target.NodePool = "workers"
			replicas := 4
			switch scenario {
			case "busy":
				live = json.RawMessage(strings.Replace(string(live), "Succeeded", "Updating", 1))
			case "wrong-target":
				target.NodePool = "other"
			case "drift":
				live = json.RawMessage(strings.Replace(string(live), "value", "changed", 1))
			case "downscale":
				replicas = 1
			case "same-count":
				replicas = 2
			case "zero":
				replicas = 0
			case "negative":
				replicas = -1
			case "above-bound":
				replicas = 201
			case "autoscaling":
				expected = json.RawMessage(strings.Replace(string(expected), `"autoScaling":null`, `"autoScaling":{"min":0,"max":10}`, 1))
			case "empty-autoscaling":
				expected = json.RawMessage(strings.Replace(string(expected), `"autoScaling":null`, `"autoScaling":{}`, 1))
			case "missing-count":
				expected = json.RawMessage(strings.Replace(string(expected), `"replicas":2,`, "", 1))
			case "fractional-count":
				expected = json.RawMessage(strings.Replace(string(expected), `"replicas":2`, `"replicas":2.5`, 1))
			case "incomplete":
				expected = json.RawMessage(strings.Replace(string(expected), `"tags":{"keep":"value"},`, "", 1))
			case "cluster-target":
				target.NodePool = ""
			}
			var patches, gets atomic.Int32
			c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "0")
				if r.Method == http.MethodGet {
					gets.Add(1)
					if r.URL.Path == statusPath {
						fmt.Fprint(w, `{"status":"Succeeded"}`)
						return
					}
					if scenario == "missing" {
						w.WriteHeader(404)
						return
					}
					body := string(live)
					if patches.Load() > 0 {
						body = strings.Replace(body, `"replicas":2`, `"replicas":4`, 1)
					}
					fmt.Fprint(w, body)
					return
				}
				if r.Method != http.MethodPatch {
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(500)
					return
				}
				if gets.Load() != 1 {
					t.Error("PATCH was not immediately preceded by exact resource GET")
				}
				patches.Add(1)
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"properties":{"replicas":4}}` {
					t.Errorf("nonminimal PATCH: %s", body)
				}
				var system map[string]string
				if json.Unmarshal([]byte(r.Header.Get("X-Ms-Arm-Resource-System-Data")), &system) != nil || system["lastModifiedBy"] != "personal-user" || system["createdBy"] != "" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("If-Match") != "" {
					t.Error("invalid PATCH headers")
				}
				w.Header().Set("Azure-AsyncOperation", statusPath)
				w.WriteHeader(http.StatusAccepted)
				fmt.Fprint(w, `{"properties":{"provisioningState":"Updating"}}`)
			})
			result, err := c.UpdateNodePool(t.Context(), target, expected, replicas, output(t))
			if scenario == "success" {
				if err != nil || patches.Load() != 1 || result.Method != http.MethodPatch {
					t.Fatalf("update failed: %+v %v patches=%d", result, err, patches.Load())
				}
				if err := c.VerifyNodePoolUpdate(t.Context(), target, expected, replicas, output(t)); err != nil {
					t.Fatal(err)
				}
				saved(t, result)
			} else if err == nil || patches.Load() != 0 {
				t.Fatalf("unsafe mutation: %v patches=%d", err, patches.Load())
			}
		})
	}
}

func TestNodePoolScaleOutFinalConfiguration(t *testing.T) {
	for _, scenario := range []string{"success", "system-data", "wrong-count", "busy", "wrong-id", "vm", "subnet", "version", "tags", "autoscaling"} {
		t.Run(scenario, func(t *testing.T) {
			expected := nodePoolSnapshot()
			body := strings.Replace(string(expected), `"replicas":2`, `"replicas":4`, 1)
			switch scenario {
			case "system-data":
				body = strings.Replace(body, `"createdBy":"original"`, `"createdBy":"original","lastModifiedBy":"updater"`, 1)
			case "wrong-count":
				body = string(expected)
			case "busy":
				body = strings.Replace(body, "Succeeded", "Updating", 1)
			case "wrong-id":
				body = strings.Replace(body, "nodePools/workers", "nodePools/other", 1)
			case "vm":
				body = strings.Replace(body, "Standard_D4s_v5", "Standard_D8s_v5", 1)
			case "subnet":
				body = strings.Replace(body, "/original/subnet", "/other/subnet", 1)
			case "version":
				body = strings.Replace(body, "4.21", "4.22", 1)
			case "tags":
				body = strings.Replace(body, "value", "changed", 1)
			case "autoscaling":
				body = strings.Replace(body, `"autoScaling":null`, `"autoScaling":{"min":0,"max":10}`, 1)
			}
			c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("verification mutated")
				}
				fmt.Fprint(w, body)
			})
			target := ref
			target.NodePool = "workers"
			err := c.VerifyNodePoolUpdate(t.Context(), target, expected, 4, output(t))
			wantSuccess := scenario == "success" || scenario == "system-data"
			if (err == nil) != wantSuccess {
				t.Fatalf("verification: %v", err)
			}
			if scenario == "wrong-count" && !strings.Contains(err.Error(), "replicas") {
				t.Fatal("missing explicit wrong-count error")
			}
		})
	}
}

func TestNodePoolScaleOutAcceptedBodyTimeout(t *testing.T) {
	var patches atomic.Int32
	c, server := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, string(nodePoolSnapshot()))
			return
		}
		if r.Method != http.MethodPatch {
			t.Error("unexpected mutation")
		}
		patches.Add(1)
		w.Header().Set("Azure-AsyncOperation", statusPath)
		w.WriteHeader(http.StatusAccepted)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	supplied := *server.Client()
	supplied.Timeout = 100 * time.Millisecond
	c, err := New(c.config, &supplied)
	if err != nil {
		t.Fatal(err)
	}
	target := ref
	target.NodePool = "workers"
	result, err := c.UpdateNodePool(t.Context(), target, nodePoolSnapshot(), 4, output(t))
	if !errors.Is(err, context.DeadlineExceeded) || result.OperationURL == "" || result.Method != http.MethodPatch || result.HTTPStatus != http.StatusAccepted || patches.Load() != 1 {
		t.Fatalf("accepted metadata lost: %+v %v", result, err)
	}
	saved(t, result)
}

func TestCreateNodePoolStillRejectsExistingResource(t *testing.T) {
	var mutations atomic.Int32
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations.Add(1)
			w.WriteHeader(500)
			return
		}
		if r.URL.Path == "/subscriptions/"+subID {
			fmt.Fprint(w, registered)
			return
		}
		fmt.Fprint(w, string(nodePoolSnapshot()))
	})
	target := ref
	target.NodePool = "workers"
	result, err := c.CreateNodePool(t.Context(), target, json.RawMessage(`{"properties":{"replicas":4}}`), output(t))
	if err == nil || result.ErrorCode != "ResourceAlreadyExists" || mutations.Load() != 0 {
		t.Fatalf("create relaxed existing-resource rejection: %+v %v", result, err)
	}
}

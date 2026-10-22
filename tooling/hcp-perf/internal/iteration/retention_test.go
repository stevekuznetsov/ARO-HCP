// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iteration

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/retention"
)

type retentionTransport func(*http.Request) (*http.Response, error)

func (f retentionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOptionalRetentionBecomesRequiredAndTokenReloads(t *testing.T) {
	const customer = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/customer"
	const managed = customer + "-managed"
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("first-token"), 0600); err != nil {
		t.Fatal(err)
	}
	spec := retention.NativeSpec{Required: retention.Spec{Version: "v1", ResourceGroups: []string{customer}}, OptionalResourceGroups: []string{managed}, TokenFile: token, Interval: time.Minute, Duration: time.Hour}
	managedExists, expired := false, false
	expectedToken := "first-token"
	patches := 0
	tags := map[string]string{}
	client := &http.Client{Transport: retentionTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer "+expectedToken {
			t.Error("token file was not reloaded")
		}
		status, body := 200, "{}"
		switch {
		case expired:
			status = 401
		case r.URL.Path == managed && !managedExists:
			status = 404
		case r.Method == http.MethodPatch:
			patches++
			var update struct {
				Properties struct {
					Tags map[string]string `json:"tags"`
				} `json:"properties"`
			}
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				t.Fatal(err)
			}
			id := strings.TrimSuffix(r.URL.Path, "/providers/Microsoft.Resources/tags/default")
			tags[id] = update.Properties.Tags["createdAt"]
		default:
			data, _ := json.Marshal(map[string]any{"id": r.URL.Path, "properties": map[string]string{"provisioningState": "Succeeded"}, "tags": map[string]string{"createdAt": tags[r.URL.Path]}})
			body = string(data)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	result, err := retention.RefreshNative(t.Context(), spec, client)
	if err != nil {
		t.Fatal(err)
	}
	spec = requireObservedGroups(spec, result)
	if len(spec.OptionalResourceGroups) != 1 {
		t.Fatal("absent optional group was prematurely required")
	}
	managedExists = true
	expectedToken = "renewed-token"
	if err := os.WriteFile(token, []byte(expectedToken), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = retention.RefreshNative(t.Context(), spec, client)
	if err != nil {
		t.Fatal(err)
	}
	spec = requireObservedGroups(spec, result)
	if len(spec.OptionalResourceGroups) != 0 || len(spec.Required.ResourceGroups) != 2 {
		t.Fatal("observed managed group did not become required")
	}
	managedExists = false
	before := patches
	if _, err := retention.RefreshNative(t.Context(), spec, client); err == nil || patches != before {
		t.Fatal("deleted managed group was silently skipped or mutations occurred before preflight")
	}
	managedExists, expired = true, true
	if _, err := retention.RefreshNative(t.Context(), spec, client); err == nil || patches != before {
		t.Fatal("expired ARM token did not fail closed")
	}
}

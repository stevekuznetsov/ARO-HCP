// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package retention

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type nativeTransport func(*http.Request) (*http.Response, error)

func (f nativeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeRefreshAllowlistAndVerification(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("external-token"), 0600); err != nil {
		t.Fatal(err)
	}
	id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/customer"
	spec := NativeSpec{Required: Spec{Version: "v1", ResourceGroups: []string{id}}, OptionalResourceGroups: []string{id + "-managed"}, TokenFile: tokenPath, Interval: 5 * time.Minute, Duration: time.Hour}
	createdAt := "old"
	patches := 0
	client := &http.Client{Transport: nativeTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer external-token" || r.URL.Host != "management.azure.com" {
			t.Fatal("incorrect native authentication or destination")
		}
		status, body := 200, "{}"
		if strings.HasSuffix(r.URL.Path, "-managed") {
			status = 404
		} else if r.Method == http.MethodPatch {
			patches++
			var data struct {
				Properties struct {
					Tags map[string]string `json:"tags"`
				} `json:"properties"`
			}
			if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
				t.Fatal(err)
			}
			createdAt = data.Properties.Tags["createdAt"]
		} else {
			data, _ := json.Marshal(map[string]any{"id": id, "properties": map[string]string{"provisioningState": "Succeeded"}, "tags": map[string]string{"createdAt": createdAt, "keep": "unchanged"}})
			body = string(data)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	result, err := RefreshNative(t.Context(), spec, client)
	if err != nil || result.Status != "verified" || patches != 1 || result.ResourceGroups[1].Status != "not-created" {
		t.Fatalf("native refresh failed: %+v patches=%d err=%v", result, patches, err)
	}
}

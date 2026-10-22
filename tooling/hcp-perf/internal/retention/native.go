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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NativeSpec allows only full resource-group IDs. Optional groups are an explicit
// allowlist for resources created later; only HTTP 404 is skipped, never denied
// access or transport failure. The token file is external and may be rotated.
type NativeSpec struct {
	Required               Spec          `json:"required"`
	OptionalResourceGroups []string      `json:"optionalResourceGroups,omitempty"`
	TokenFile              string        `json:"tokenFile"`
	Interval               time.Duration `json:"intervalNanoseconds"`
	Duration               time.Duration `json:"durationNanoseconds"`
}

func (s NativeSpec) Validate() error {
	if s.Required.Version != "v1" || len(s.Required.ResourceGroups) == 0 || !filepath.IsAbs(s.TokenFile) {
		return errors.New("native retention requires v1, explicit required groups and absolute token file")
	}
	if s.Interval < time.Minute || s.Interval > 30*time.Minute || s.Duration < s.Interval || s.Duration > 24*time.Hour {
		return errors.New("retention interval must be 1m..30m and duration interval..24h")
	}
	seen := map[string]bool{}
	for _, id := range append(append([]string{}, s.Required.ResourceGroups...), s.OptionalResourceGroups...) {
		if !groupID.MatchString(id) || strings.HasSuffix(id, ".") || seen[strings.ToLower(id)] {
			return errors.New("retention group IDs must be valid, explicit and unique")
		}
		seen[strings.ToLower(id)] = true
	}
	return nil
}

// RefreshNative performs ARM GET, tag Merge PATCH and verification GET directly.
// It never invokes Azure CLI, retries a write, changes subscriptions or logs tokens.
// The supplied transport is a test seam and must honor request cancellation.
func RefreshNative(ctx context.Context, spec NativeSpec, supplied *http.Client) (Result, error) {
	result := Result{Version: "v1", Status: "failed", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ResourceGroups: []GroupResult{}}
	if err := spec.Validate(); err != nil {
		return result, err
	}
	client := http.Client{Timeout: time.Minute}
	if supplied != nil {
		client = *supplied
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	token, err := os.ReadFile(spec.TokenFile)
	if err != nil {
		return result, errors.New("read external ARM token failed")
	}
	credential := strings.TrimSpace(string(token))
	if credential == "" || strings.ContainsAny(credential, "\r\n") {
		return result, errors.New("invalid ARM token file")
	}
	request := func(method, path string, body []byte) (snapshot, int, error) {
		callCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		req, err := http.NewRequestWithContext(callCtx, method, "https://management.azure.com"+path, bytes.NewReader(body))
		if err != nil {
			return snapshot{}, 0, errors.New("construct ARM request failed")
		}
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return snapshot{}, 0, errors.New("ARM retention request failed")
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			return snapshot{}, response.StatusCode, errors.New("read ARM retention response failed")
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return snapshot{}, response.StatusCode, errors.New("ARM retention response was not successful")
		}
		var snap snapshot
		if method == http.MethodGet && json.Unmarshal(data, &snap) != nil {
			return snap, response.StatusCode, errors.New("invalid ARM retention response")
		}
		return snap, response.StatusCode, nil
	}
	snapshots := []snapshot{}
	groups := append(append([]string{}, spec.Required.ResourceGroups...), spec.OptionalResourceGroups...)
	for i, id := range groups {
		group := GroupResult{ID: id, Status: "pending", Operation: "preflight", OldStatus: "unknown", NewCreatedAt: result.CreatedAt}
		result.ResourceGroups = append(result.ResourceGroups, group)
		g := &result.ResourceGroups[i]
		snap, status, err := request(http.MethodGet, id+"?api-version=2021-04-01", nil)
		if status == http.StatusNotFound && i >= len(spec.Required.ResourceGroups) {
			g.Status = "not-created"
			snapshots = append(snapshots, snapshot{})
			continue
		}
		if err != nil {
			return result, err
		}
		if !strings.EqualFold(snap.ID, id) || snap.Properties.ProvisioningState == "" || strings.EqualFold(snap.Properties.ProvisioningState, "Deleting") {
			return result, errors.New("ARM retention resource identity/state mismatch")
		}
		g.TagKey, g.OldStatus = "createdAt", "missing"
		found := false
		for key, value := range snap.Tags {
			if !strings.EqualFold(key, "createdAt") {
				continue
			}
			if found {
				return result, errors.New("ambiguous createdAt tag")
			}
			found = true
			g.TagKey, g.OldCreatedAt, g.OldStatus = key, &value, "valid"
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
				g.OldStatus = "invalid"
			}
		}
		snapshots = append(snapshots, snap)
	}
	for i := range result.ResourceGroups {
		g := &result.ResourceGroups[i]
		if g.Status == "not-created" {
			continue
		}
		body, err := json.Marshal(map[string]any{"operation": "Merge", "properties": map[string]any{"tags": map[string]string{g.TagKey: result.CreatedAt}}})
		if err != nil {
			return result, err
		}
		g.Attempted, g.Operation = true, "update"
		if _, _, err := request(http.MethodPatch, g.ID+"/providers/Microsoft.Resources/tags/default?api-version=2021-04-01", body); err != nil {
			g.PossiblyUpdated = true
			return result, err
		}
		g.Updated, g.Operation = true, "verify"
		snap, _, err := request(http.MethodGet, g.ID+"?api-version=2021-04-01", nil)
		if err != nil {
			return result, err
		}
		if !strings.EqualFold(snap.ID, g.ID) || snap.Tags[g.TagKey] != result.CreatedAt {
			return result, errors.New("retention verification mismatch")
		}
		for key := range snapshots[i].Tags {
			if _, found := snap.Tags[key]; !found {
				return result, errors.New("unrelated tag disappeared during retention refresh")
			}
		}
		g.Verified, g.Status = true, "verified"
	}
	result.Status = "verified"
	return result, nil
}

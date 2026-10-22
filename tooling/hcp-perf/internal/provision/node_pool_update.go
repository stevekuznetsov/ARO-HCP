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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ValidateNodePoolUpdate performs no I/O. The full GET snapshot is required:
// the immediate preflight compares every field, including unknown fields.
func (c *Client) ValidateNodePoolUpdate(ref Resource, expected json.RawMessage, replicas int) (json.RawMessage, error) {
	id, err := c.resourceID(ref)
	if err != nil {
		return nil, err
	}
	if ref.NodePool == "" || replicas <= 0 || replicas > 200 {
		// Conservatively retain the unzoned RP bound even for zoned pools.
		return nil, errors.New("node pool scale-out requires a node pool and replicas in [1,200]")
	}
	if err := validatePayload(expected, c.config.APIVersion); err != nil {
		return nil, err
	}
	var snapshot struct {
		ID         string `json:"id"`
		Location   string `json:"location"`
		Properties struct {
			ProvisioningState string          `json:"provisioningState"`
			Replicas          *int            `json:"replicas"`
			AutoScaling       json.RawMessage `json:"autoScaling"`
			Platform          struct {
				VMSize   string `json:"vmSize"`
				SubnetID string `json:"subnetId"`
			} `json:"platform"`
			Version struct {
				ID string `json:"id"`
			} `json:"version"`
		} `json:"properties"`
	}
	if json.Unmarshal(expected, &snapshot) != nil || snapshot.ID != id || snapshot.Location == "" || snapshot.Properties.ProvisioningState != "Succeeded" || snapshot.Properties.Platform.VMSize == "" || snapshot.Properties.Platform.SubnetID == "" || snapshot.Properties.Version.ID == "" {
		return nil, errors.New("node pool update requires an exact-identity Succeeded resource snapshot with platform and version")
	}
	p := snapshot.Properties
	if len(p.AutoScaling) != 0 && !bytes.Equal(bytes.TrimSpace(p.AutoScaling), []byte("null")) {
		return nil, errors.New("node pool update requires autoscaling disabled (absent or null)")
	}
	if p.Replicas == nil || *p.Replicas < 0 || replicas <= *p.Replicas {
		return nil, errors.New("node pool update requires explicit current replicas and strict scale-out")
	}
	return json.RawMessage(fmt.Sprintf(`{"properties":{"replicas":%d}}`, replicas)), nil
}

// UpdateNodePool issues one replicas-only merge PATCH after an exact GET guard.
// The RP has no HTTP ETag precondition: this is not atomic and requires exclusive
// writer ownership. An uncertain mutation must never be replayed.
func (c *Client) UpdateNodePool(ctx context.Context, ref Resource, expected json.RawMessage, replicas int, outputDir string) (Result, error) {
	payload, err := c.ValidateNodePoolUpdate(ref, expected, replicas)
	if err != nil {
		return Result{}, err
	}
	return c.run(ctx, ref, http.MethodPatch, payload, outputDir, "", "", expected)
}

// VerifyNodePoolUpdate is GET-only, including after accepted-operation recovery.
// Only replicas, provisioningState and ARM systemData may differ from the guard.
func (c *Client) VerifyNodePoolUpdate(ctx context.Context, ref Resource, expected json.RawMessage, replicas int, outputDir string) error {
	if _, err := c.ValidateNodePoolUpdate(ref, expected, replicas); err != nil {
		return err
	}
	id, _ := c.resourceID(ref)
	a, err := newArtifacts(outputDir)
	if err != nil {
		return err
	}
	defer a.close()
	r, err := c.request(ctx, http.MethodGet, c.resourceURL(id, c.config.APIVersion), nil, a)
	if err != nil {
		return err
	}
	if err := a.snapshot(r.body); err != nil {
		return err
	}
	if r.status != http.StatusOK || validatePayload(r.body, c.config.APIVersion) != nil {
		return errors.New("node pool update final resource GET failed")
	}
	var actual struct {
		ID         string `json:"id"`
		Properties struct {
			ProvisioningState string `json:"provisioningState"`
			Replicas          *int   `json:"replicas"`
		} `json:"properties"`
	}
	if json.Unmarshal(r.body, &actual) != nil || actual.ID != id || actual.Properties.ProvisioningState != "Succeeded" {
		return errors.New("node pool update final identity/state verification failed")
	}
	if actual.Properties.Replicas == nil || *actual.Properties.Replicas != replicas {
		return fmt.Errorf("node pool update final replicas do not match desired %d", replicas)
	}
	normalize := func(raw []byte) []byte {
		var object, properties map[string]json.RawMessage
		_ = json.Unmarshal(raw, &object)
		_ = json.Unmarshal(object["properties"], &properties)
		delete(object, "systemData")
		delete(properties, "provisioningState")
		delete(properties, "replicas")
		// Absent and null both represent disabled autoscaling.
		if bytes.Equal(bytes.TrimSpace(properties["autoScaling"]), []byte("null")) {
			delete(properties, "autoScaling")
		}
		object["properties"], _ = json.Marshal(properties)
		out, _ := json.Marshal(object)
		return out
	}
	if !sameResourceJSON(normalize(expected), normalize(r.body)) {
		return errors.New("node pool update final configuration changed beyond replicas")
	}
	return nil
}

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

package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OperationFailedError is emitted only for a terminal Failed/Canceled response
// from the accepted operation status endpoint, never from resource inspection.
type OperationFailedError struct {
	OperationURL string
	State        string
}

func (e *OperationFailedError) Error() string { return "accepted RP operation " + e.State }

// CreateCluster refuses an existing resource. The GET-before-PUT check is NOT
// atomic: the RP does not enforce create-only semantics and a concurrent creator
// can race it. Use unique names and do not share a target with another writer.
// Payload is the user's existing-customer-resource JSON, sent byte-for-byte.
func (c *Client) CreateCluster(ctx context.Context, ref Resource, payload json.RawMessage, outputDir string) (Result, error) {
	if ref.NodePool != "" {
		return Result{}, errors.New("cluster creation cannot target a node pool")
	}
	return c.run(ctx, ref, http.MethodPut, payload, outputDir, "", "")
}

// CreateNodePool has the same non-atomic freshness check as CreateCluster.
func (c *Client) CreateNodePool(ctx context.Context, ref Resource, payload json.RawMessage, outputDir string) (Result, error) {
	if ref.NodePool == "" {
		return Result{}, errors.New("node pool name is required")
	}
	return c.run(ctx, ref, http.MethodPut, payload, outputDir, "", "")
}

// ValidateActionInputs performs no I/O. It reuses the request validators and
// returns canonical request identity for callers that journal RP operations.
// PATCH identifies the typed replicas-only action; arbitrary PATCH bodies are
// forbidden. ValidateNodePoolUpdate separately validates its typed inputs.
func (c *Client) ValidateActionInputs(ref Resource, method string, payload json.RawMessage, operationURL string) (Result, error) {
	id, err := c.resourceID(ref)
	if err != nil {
		return Result{}, err
	}
	switch method {
	case http.MethodPut:
		if err := validatePayload(payload, c.config.APIVersion); err != nil {
			return Result{}, err
		}
	case http.MethodGet, http.MethodDelete:
		if len(payload) != 0 {
			return Result{}, errors.New("payload is only permitted for PUT")
		}
	case http.MethodPatch:
		if ref.NodePool == "" || len(payload) != 0 {
			return Result{}, errors.New("PATCH requires a node pool and typed replicas input, not a payload")
		}
	default:
		return Result{}, errors.New("unsupported resource action method")
	}
	if operationURL != "" {
		if method != http.MethodGet {
			return Result{}, errors.New("operation resume requires GET")
		}
		operationURL, err = c.operationURL(operationURL)
		if err != nil {
			return Result{}, err
		}
		if !strings.Contains(strings.ToLower(operationURL), "/hcpoperationstatuses/") {
			return Result{}, errors.New("resume requires an operation status URL")
		}
	}
	return Result{Version: "v1", ResourceID: id, RequestURL: c.resourceURL(id, c.config.APIVersion), Method: method, OperationURL: operationURL}, nil
}

// Inspect GETs only the exact cluster or node pool. The response is a private
// snapshot, never a body embedded in Result. It cannot invoke credential actions.
func (c *Client) Inspect(ctx context.Context, ref Resource, outputDir string) (Result, error) {
	return c.run(ctx, ref, http.MethodGet, nil, outputDir, "", "")
}

// Resume observes an existing operation without resubmitting any mutation. Only
// status URLs at the configured origin and subscription are accepted: credential
// result retrieval is deliberately unsupported. Poll bodies are metadata-only.
// The caller must associate the operation with ref; a status URL cannot prove
// that association. After Succeeded, GET ref must show Succeeded or HTTP 404
// (successful absence after deletion). Result.Method is GET, not the old action.
func (c *Client) Resume(ctx context.Context, ref Resource, operationURL, outputDir string) (Result, error) {
	target, err := c.operationURL(operationURL)
	if err != nil {
		return Result{}, err
	}
	if !strings.Contains(strings.ToLower(target), "/hcpoperationstatuses/") {
		return Result{}, errors.New("resume requires an operation status URL; credential and operation result retrieval is unsupported")
	}
	return c.run(ctx, ref, http.MethodGet, nil, outputDir, "", target)
}

// Delete never deletes resource groups. The parent CLI must pass its explicit
// confirmation flag; no other operation calls Delete, even after failure.
func (c *Client) Delete(ctx context.Context, ref Resource, outputDir string, confirmed bool) (Result, error) {
	if !confirmed {
		return Result{}, errors.New("explicit deletion confirmation is required")
	}
	return c.run(ctx, ref, http.MethodDelete, nil, outputDir, "", "")
}

// RequestAdminCredential writes the decoded kubeconfig string (not base64) to
// an exclusive 0600 file. The explicit outputPath must not exist. Bodies from
// this entire operation, including errors and polling, are never archived.
func (c *Client) RequestAdminCredential(ctx context.Context, ref Resource, outputDir, outputPath string) (Result, error) {
	if ref.NodePool != "" {
		return Result{}, errors.New("credentials require a cluster reference")
	}
	if outputPath == "" {
		return Result{}, errors.New("explicit credential output path is required")
	}
	return c.run(ctx, ref, http.MethodPost, nil, outputDir, outputPath, "")
}

// Payload remains schema-free, but ambiguous JSON objects are rejected even
// when the library is used without the parent CLI's strict decoder.
func validatePayload(payload []byte, version string) error {
	if len(payload) > maxBody || !json.Valid(payload) || !bytes.HasPrefix(bytes.TrimSpace(payload), []byte("{")) {
		return errors.New("payload must be a JSON object no larger than 16 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(payload))
	d.UseNumber()
	var objects []map[string]bool
	var expectingKey []bool
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("invalid payload JSON")
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				if len(objects) > 0 && objects[len(objects)-1] != nil {
					expectingKey[len(objects)-1] = true
				}
				var keys map[string]bool
				if delimiter == '{' {
					keys = map[string]bool{}
				}
				objects = append(objects, keys)
				expectingKey = append(expectingKey, true)
			case '}', ']':
				objects = objects[:len(objects)-1]
				expectingKey = expectingKey[:len(expectingKey)-1]
			}
		} else if n := len(objects) - 1; n >= 0 && objects[n] != nil {
			if expectingKey[n] {
				key := strings.ToLower(token.(string))
				if objects[n][key] {
					return errors.New("payload contains duplicate or case-aliased object keys")
				}
				objects[n][key] = true
			}
			expectingKey[n] = !expectingKey[n]
		}
	}
	var object map[string]json.RawMessage
	_ = json.Unmarshal(payload, &object)
	for key, value := range object {
		if strings.EqualFold(key, "apiVersion") {
			var supplied string
			if json.Unmarshal(value, &supplied) != nil || supplied != version {
				return errors.New("payload apiVersion must match config apiVersion")
			}
		}
	}
	return nil
}

func (c *Client) run(ctx context.Context, ref Resource, method string, payload []byte, outputDir, credentialPath, resumeURL string, expectedUpdate ...json.RawMessage) (result Result, err error) {
	id, err := c.resourceID(ref)
	if err != nil {
		return result, err
	}
	if method == http.MethodPut || method == http.MethodPatch {
		if err := validatePayload(payload, c.config.APIVersion); err != nil {
			return result, err
		}
	}
	target := id
	secret := method == http.MethodPost
	if secret {
		target += "/requestAdminCredential"
	}
	result = Result{Version: "v1", ResourceID: id, RequestURL: c.resourceURL(target, c.config.APIVersion), Method: method, OutputDir: outputDir}
	result.OperationURL = resumeURL
	a, err := newArtifacts(outputDir)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, a.save(result), a.close()) }()
	if err = a.save(result); err != nil {
		return result, err
	}
	if method == http.MethodPut || method == http.MethodPatch {
		if err = a.write("payload.json", payload); err != nil {
			return result, err
		}
	}
	var credentialFile *os.File
	if secret {
		credentialFile, err = os.OpenFile(credentialPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return result, err
		}
		defer func() { err = errors.Join(err, credentialFile.Close()) }()
		// A failed request leaves a private empty file, not a reusable destination.
	}

	// Record metadata before returning any HTTP/body-read error. In particular,
	// preserve accepted operation URLs even when reading the response times out.
	observe := func(r response, requestErr error, links bool) error {
		result.HTTPStatus = r.status
		var linkErr error
		if links {
			if raw := r.header.Get("Azure-AsyncOperation"); raw != "" {
				result.OperationURL, linkErr = c.operationURL(raw)
			}
			if raw := r.header.Get("Location"); raw != "" {
				var e error
				result.ResultURL, e = c.operationURL(raw)
				linkErr = errors.Join(linkErr, e)
			}
			if result.OperationURL == "" {
				result.OperationURL = result.ResultURL
			}
		}
		state := responseState(r.body)
		if state != "" {
			result.LastState = state
		}
		if state == "Failed" || state == "Canceled" {
			result.FinalState = state
			result.ErrorCode = errorCode(r)
		}
		if r.status >= 400 {
			result.ErrorCode = errorCode(r)
		}
		var snapshotErr error
		if !secret && resumeURL == "" && len(r.body) > 0 {
			snapshotErr = a.snapshot(r.body)
		}
		return errors.Join(requestErr, linkErr, snapshotErr, a.event(event{HTTPStatus: r.status, State: state}), a.save(result))
	}
	if method == http.MethodPut {
		r, reqErr := c.request(ctx, http.MethodGet, c.resourceURL("/subscriptions/"+c.config.SubscriptionID, "2.0"), nil, a)
		if err = observe(r, reqErr, false); err != nil {
			return result, err
		}
		if r.status != http.StatusOK {
			return result, httpError(r)
		}
		var subscription struct {
			State      string `json:"state"`
			Properties struct {
				TenantID string `json:"tenantId"`
			} `json:"properties"`
		}
		if json.Unmarshal(r.body, &subscription) != nil || subscription.State != "Registered" || !strings.EqualFold(subscription.Properties.TenantID, c.config.TenantID) {
			return result, errors.New("subscription must already be Registered with the configured tenant; registration is never written")
		}
		r, reqErr = c.request(ctx, http.MethodGet, result.RequestURL, nil, a)
		if err = observe(r, reqErr, false); err != nil {
			return result, err
		}
		if len(expectedUpdate) != 0 {
			if r.status != http.StatusOK || responseState(r.body) != "Succeeded" || !sameResourceJSON(r.body, expectedUpdate[0]) {
				return result, errors.New("image update resource snapshot/state changed; no PUT issued")
			}
		} else if r.status == http.StatusOK {
			result.ErrorCode = "ResourceAlreadyExists"
			return result, errors.New("resource already exists; refusing PUT (freshness check is not atomic)")
		}
		if len(expectedUpdate) == 0 && r.status != http.StatusNotFound {
			return result, httpError(r)
		}
		result.ErrorCode, result.LastState, result.FinalState = "", "", ""
	}
	if method == http.MethodPatch {
		r, reqErr := c.request(ctx, http.MethodGet, result.RequestURL, nil, a)
		if err = observe(r, reqErr, false); err != nil {
			return result, err
		}
		if len(expectedUpdate) != 1 || r.status != http.StatusOK || validatePayload(r.body, c.config.APIVersion) != nil || responseState(r.body) != "Succeeded" || !sameResourceJSON(r.body, expectedUpdate[0]) {
			return result, errors.New("node pool update resource snapshot/state changed; no PATCH issued")
		}
		result.ErrorCode, result.LastState, result.FinalState = "", "", ""
	}
	// Resume starts with polling, not the original PUT/PATCH/DELETE/credential POST.
	r := response{status: http.StatusAccepted, header: http.Header{"Retry-After": {"0"}}}
	var reqErr error
	if resumeURL == "" {
		r, reqErr = c.request(ctx, method, result.RequestURL, payload, a)
		if err = observe(r, reqErr, method != http.MethodGet); err != nil {
			return result, err
		}
	}
	if r.status < 200 || r.status >= 300 {
		return result, httpError(r)
	}
	if method == http.MethodGet && resumeURL == "" {
		if r.status != http.StatusOK {
			return result, errors.New("resource GET did not return HTTP 200")
		}
		if result.LastState == "Succeeded" {
			result.FinalState = "Succeeded"
		}
		return result, terminalError(result.LastState)
	}
	if err = terminalError(result.LastState); err != nil {
		return result, err
	}
	if result.OperationURL != "" {
		pollURL := result.OperationURL
		for {
			if err = sleep(ctx, retryAfter(r.header.Get("Retry-After"), time.Now())); err != nil {
				return result, err
			}
			r, reqErr = c.request(ctx, http.MethodGet, pollURL, nil, a)
			if err = observe(r, reqErr, false); err != nil {
				return result, err
			}
			if r.status < 200 || r.status >= 300 {
				return result, httpError(r)
			}
			state := responseState(r.body)
			if r.status == http.StatusOK && strings.Contains(strings.ToLower(pollURL), "/hcpoperationstatuses/") && (state == "Failed" || state == "Canceled") {
				return result, &OperationFailedError{OperationURL: pollURL, State: state}
			}
			if err = terminalError(state); err != nil {
				return result, err
			}
			if r.status == http.StatusAccepted {
				continue
			}
			isResult := strings.Contains(strings.ToLower(pollURL), "/hcpoperationresults/")
			if state == "Succeeded" {
				if secret && !isResult {
					if result.ResultURL == "" || result.ResultURL == pollURL {
						return result, errors.New("credential operation has no result URL")
					}
					pollURL = result.ResultURL
					continue
				}
				break
			}
			if isResult && ((secret && r.status == http.StatusOK && hasKubeconfig(r.body)) || (method == http.MethodDelete && r.status == http.StatusNoContent)) {
				break
			}
			if state == "" || state == "Unknown" {
				return result, errors.New("operation response has no recognized completion state")
			}
		}
	} else if r.status == http.StatusAccepted {
		return result, errors.New("accepted request has no safe operation URL")
	}
	if method == http.MethodPut || method == http.MethodPatch || resumeURL != "" {
		if result.OperationURL != "" {
			r, reqErr = c.request(ctx, http.MethodGet, c.resourceURL(id, c.config.APIVersion), nil, a)
			// Resume only snapshots this exact resource GET, never a poll body.
			if resumeURL != "" && len(r.body) > 0 {
				if err = a.snapshot(r.body); err != nil {
					return result, err
				}
			}
			if err = observe(r, reqErr, false); err != nil {
				return result, err
			}
		}
		if resumeURL != "" && r.status == http.StatusNotFound {
			result.ErrorCode = ""
			result.LastState, result.FinalState = "Succeeded", "Succeeded"
			return result, nil
		}
		if r.status != http.StatusOK && r.status != http.StatusCreated {
			return result, httpError(r)
		}
		var resource struct {
			Properties struct {
				ProvisioningState string `json:"provisioningState"`
			} `json:"properties"`
		}
		if json.Unmarshal(r.body, &resource) != nil || !strings.EqualFold(resource.Properties.ProvisioningState, "Succeeded") {
			return result, errors.New("resource provisioningState is not Succeeded")
		}
	} else if secret {
		var credential struct {
			Kubeconfig string `json:"kubeconfig"`
		}
		if r.status != http.StatusOK || json.Unmarshal(r.body, &credential) != nil || credential.Kubeconfig == "" {
			return result, errors.New("credential result must contain a nonempty kubeconfig string")
		}
		if _, err = credentialFile.WriteString(credential.Kubeconfig); err != nil {
			return result, errors.New("write credential output failed")
		}
		if err = credentialFile.Sync(); err != nil {
			return result, errors.New("sync credential output failed")
		}
		result.CredentialFile = credentialPath
	} else if result.OperationURL == "" && r.status != http.StatusNoContent && responseState(r.body) != "Succeeded" {
		return result, errors.New("DELETE did not establish completion")
	}
	result.LastState, result.FinalState = "Succeeded", "Succeeded"
	return result, nil
}

const cpoImageTag = "aro-hcp.experimental.cluster.control-plane-operator-image-override"

// ImageUpdatePayload changes only the canonical image tag in a reviewed resource.
func ImageUpdatePayload(expected json.RawMessage, image string) (json.RawMessage, error) {
	if !regexp.MustCompile(`^[^\s]+@sha256:[0-9a-f]{64}$`).MatchString(image) {
		return nil, errors.New("image must be digest pinned")
	}
	if err := validatePayload(expected, "2025-12-23-preview"); err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(expected, &object) != nil || responseState(expected) != "Succeeded" {
		return nil, errors.New("image update requires Succeeded resource snapshot")
	}
	var tags map[string]string
	if json.Unmarshal(object["tags"], &tags) != nil || tags[cpoImageTag] == "" || tags[cpoImageTag] == image {
		return nil, errors.New("image update requires existing distinct canonical image tag")
	}
	tags[cpoImageTag] = image
	raw, err := json.Marshal(tags)
	if err != nil {
		return nil, err
	}
	object["tags"] = raw
	return json.Marshal(object)
}

func sameResourceJSON(a, b []byte) bool {
	decode := func(raw []byte) (any, error) {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var v any
		err := d.Decode(&v)
		return v, err
	}
	x, e := decode(a)
	y, f := decode(b)
	return e == nil && f == nil && reflect.DeepEqual(x, y)
}

// UpdateClusterImage is non-atomic when the RP provides no ETag. The exclusive
// environment owner must exclude other writers; the exact GET preflight fails
// closed on any observed change. Accepted operations are resumed, never replayed.
func (c *Client) UpdateClusterImage(ctx context.Context, ref Resource, expected json.RawMessage, image, outputDir string) (Result, error) {
	if ref.NodePool != "" {
		return Result{}, errors.New("image update cannot target node pool")
	}
	id, err := c.resourceID(ref)
	if err != nil {
		return Result{}, err
	}
	var object struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(expected, &object) != nil || !strings.EqualFold(object.ID, id) {
		return Result{}, errors.New("image update resource identity mismatch")
	}
	payload, err := ImageUpdatePayload(expected, image)
	if err != nil {
		return Result{}, err
	}
	result, err := c.run(ctx, ref, http.MethodPut, payload, outputDir, "", "", expected)
	return result, err
}

func (c *Client) VerifyClusterImage(ctx context.Context, ref Resource, image, outputDir string) error {
	id, err := c.resourceID(ref)
	if err != nil {
		return err
	}
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
	var object struct {
		ID   string            `json:"id"`
		Tags map[string]string `json:"tags"`
	}
	if r.status != http.StatusOK || json.Unmarshal(r.body, &object) != nil || !strings.EqualFold(object.ID, id) || responseState(r.body) != "Succeeded" || object.Tags[cpoImageTag] != image {
		return errors.New("image update final verification failed")
	}
	return nil
}

func hasKubeconfig(body []byte) bool {
	var value struct {
		Kubeconfig string `json:"kubeconfig"`
	}
	return json.Unmarshal(body, &value) == nil && value.Kubeconfig != ""
}

func responseState(body []byte) string {
	var value struct {
		Status     string `json:"status"`
		Properties struct {
			ProvisioningState string `json:"provisioningState"`
		} `json:"properties"`
	}
	if json.Unmarshal(body, &value) != nil {
		return ""
	}
	state := value.Status
	if state == "" {
		state = value.Properties.ProvisioningState
	}
	switch strings.ToLower(state) {
	case "":
		return ""
	case "succeeded":
		return "Succeeded"
	case "failed":
		return "Failed"
	case "canceled", "cancelled":
		return "Canceled"
	case "accepted":
		return "Accepted"
	// Keep the RP states aligned with internal/api/coreapi/resource.go.
	case "provisioning":
		return "Provisioning"
	case "awaitingsecret":
		return "AwaitingSecret"
	case "running":
		return "Running"
	case "inprogress":
		return "InProgress"
	case "creating":
		return "Creating"
	case "updating":
		return "Updating"
	case "deleting":
		return "Deleting"
	default:
		return "Unknown"
	}
}

func terminalError(state string) error {
	if state == "Unknown" {
		return errors.New("unrecognized RP provisioning state; expected Accepted, Provisioning, Updating, Deleting, AwaitingSecret, Running, InProgress, Creating, Succeeded, Failed or Canceled (raw state retained only in private response artifacts when enabled)")
	}
	if state == "Failed" || state == "Canceled" {
		return errors.New("RP operation " + state)
	}
	return nil
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		return time.Duration(min(seconds, 30)) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(0, min(at.Sub(now), 30*time.Second))
	}
	return 10 * time.Second
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

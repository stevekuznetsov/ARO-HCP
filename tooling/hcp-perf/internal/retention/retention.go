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

// Package retention refreshes only explicitly named resource groups. It never
// discovers resources, changes accounts, retries writes, or rolls back tags.
package retention

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Spec struct {
	Version        string   `json:"version"`
	ResourceGroups []string `json:"resourceGroups"`
}

type Result struct {
	Version        string        `json:"version"`
	Status         string        `json:"status"`
	DryRun         bool          `json:"dryRun"`
	CreatedAt      string        `json:"createdAt,omitempty"`
	ResourceGroups []GroupResult `json:"resourceGroups"`
	Failure        *Failure      `json:"failure,omitempty"`
}

type GroupResult struct {
	ID              string   `json:"id"`
	Status          string   `json:"status"`
	Operation       string   `json:"operation"`
	TagKey          string   `json:"tagKey,omitempty"`
	OldCreatedAt    *string  `json:"oldCreatedAt,omitempty"`
	OldStatus       string   `json:"oldStatus"` // unknown, missing, valid, or invalid
	NewCreatedAt    string   `json:"newCreatedAt,omitempty"`
	Attempted       bool     `json:"attempted"` // a write was issued
	Updated         bool     `json:"updated"`   // Azure CLI acknowledged the write
	Verified        bool     `json:"verified"`  // a subsequent read matched
	PossiblyUpdated bool     `json:"possiblyUpdated"`
	Failure         *Failure `json:"failure,omitempty"`
}

// Failure deliberately excludes Azure stdout/stderr, which may contain secrets.
type Failure struct {
	Code     string `json:"code"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

func (f *Failure) Error() string { return "retention refresh: " + f.Code }

type executor interface {
	run(context.Context, ...string) ([]byte, error)
}

type azureCLI struct{}

func (azureCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, &Failure{Code: "home_directory_unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "az", args...)
	cmd.WaitDelay = time.Second
	cmd.Stderr = io.Discard
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "AZURE_CONFIG_DIR=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "AZURE_CONFIG_DIR="+filepath.Join(home, ".azure-redhat"))
	data, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return data, err
}

// Refresh must succeed before starting each test. A failed update can have
// reached Azure; inspect the partial result rather than assuming no changes.
func Refresh(ctx context.Context, spec Spec, dryRun bool) (Result, error) {
	return refresh(ctx, spec, dryRun, azureCLI{}, time.Now)
}

var groupID = regexp.MustCompile(`(?i)^/subscriptions/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/resourcegroups/[\p{L}\p{N}_.()\-]{1,90}$`)

func refresh(ctx context.Context, spec Spec, dryRun bool, client executor, now func() time.Time) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	result := Result{Version: "v1", Status: "failed", DryRun: dryRun, ResourceGroups: []GroupResult{}}
	fail := func(index int, err error) (Result, error) {
		failure := classify(err)
		result.Failure = failure
		if index >= 0 {
			result.ResourceGroups[index].Status = "failed"
			result.ResourceGroups[index].Failure = failure
		}
		return result, failure
	}
	if spec.Version != "v1" || len(spec.ResourceGroups) == 0 {
		return fail(-1, &Failure{Code: "invalid_spec"})
	}
	for _, id := range spec.ResourceGroups {
		if !groupID.MatchString(id) || strings.HasSuffix(id, ".") {
			return fail(-1, &Failure{Code: "invalid_resource_group_id"})
		}
		duplicate := false
		for _, group := range result.ResourceGroups {
			duplicate = duplicate || strings.EqualFold(group.ID, id)
		}
		if duplicate {
			continue
		}
		result.ResourceGroups = append(result.ResourceGroups, GroupResult{ID: id, Status: "pending", Operation: "pending", OldStatus: "unknown"})
	}
	// Read every target before the first mutation; managed/node RGs are allowed
	// because the caller explicitly supplied their full IDs, not a name prefix.
	snapshots := make([]snapshot, len(result.ResourceGroups))
	for i := range result.ResourceGroups {
		g := &result.ResourceGroups[i]
		g.Operation = "preflight"
		snap, key, err := readGroup(ctx, client, g.ID)
		if err != nil {
			return fail(i, err)
		}
		snapshots[i] = snap
		g.TagKey = key
		g.OldStatus = "missing"
		if old, ok := snap.Tags[key]; ok {
			g.OldCreatedAt = &old
			g.OldStatus = "valid"
			if _, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(old)); err != nil {
				g.OldStatus = "invalid"
			}
		}
		g.Operation = "pending"
	}
	if err := ctx.Err(); err != nil {
		return fail(-1, err)
	}
	result.CreatedAt = now().UTC().Format(time.RFC3339Nano)
	for i := range result.ResourceGroups {
		result.ResourceGroups[i].NewCreatedAt = result.CreatedAt
	}
	for i := range result.ResourceGroups {
		g := &result.ResourceGroups[i]
		if dryRun {
			g.Status, g.Operation = "planned", "none"
			continue
		}
		g.Operation = "update"
		if err := ctx.Err(); err != nil {
			return fail(i, err)
		}
		g.Attempted = true
		_, err := client.run(ctx, "tag", "update", "--resource-id", g.ID, "--operation", "Merge", "--tags", g.TagKey+"="+result.CreatedAt, "--output", "none", "--only-show-errors")
		if err != nil {
			// A failed/timed-out request may have committed. Do not retry or roll back.
			g.PossiblyUpdated = true
			return fail(i, err)
		}
		g.Updated = true
		g.Operation = "verify"
		snap, key, err := readGroup(ctx, client, g.ID)
		if err != nil {
			return fail(i, err)
		}
		if snap.Tags[key] != result.CreatedAt {
			return fail(i, &Failure{Code: "created_at_mismatch"})
		}
		// Concurrent value changes are legitimate. Disappearance is reported
		// conservatively, without exposing unrelated tag keys or values.
		for oldKey := range snapshots[i].Tags {
			if strings.EqualFold(oldKey, "createdAt") {
				continue
			}
			found := false
			for newKey := range snap.Tags {
				found = found || strings.EqualFold(oldKey, newKey)
			}
			if !found {
				return fail(i, &Failure{Code: "unrelated_tag_disappeared"})
			}
		}
		g.Verified = true
		g.Status = "verified"
	}
	result.Status = "verified"
	if dryRun {
		result.Status = "planned"
	}
	return result, nil
}

type snapshot struct {
	ID         string `json:"id"`
	Properties struct {
		ProvisioningState string `json:"provisioningState"`
	} `json:"properties"`
	Tags map[string]string `json:"tags"`
}

func readGroup(ctx context.Context, client executor, id string) (snapshot, string, error) {
	var snap snapshot
	if err := ctx.Err(); err != nil {
		return snap, "", err
	}
	parts := strings.Split(id, "/")
	data, err := client.run(ctx, "group", "show", "--name="+parts[4], "--subscription", parts[2], "--query", "{id:id,managedBy:managedBy,properties:properties,tags:tags}", "--output", "json", "--only-show-errors")
	if err != nil {
		return snap, "", err
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return snap, "", &Failure{Code: "invalid_group_response"}
	}
	if !strings.EqualFold(snap.ID, id) {
		return snap, "", &Failure{Code: "resource_group_id_mismatch"}
	}
	if snap.Properties.ProvisioningState == "" {
		return snap, "", &Failure{Code: "missing_provisioning_state"}
	}
	if strings.EqualFold(snap.Properties.ProvisioningState, "Deleting") {
		return snap, "", &Failure{Code: "resource_group_deleting"}
	}
	key := ""
	for candidate := range snap.Tags {
		if strings.EqualFold(candidate, "createdAt") {
			if key != "" {
				return snap, "", &Failure{Code: "ambiguous_created_at"}
			}
			key = candidate
		}
	}
	if key == "" {
		key = "createdAt"
	}
	return snap, key, nil
}

func classify(err error) *Failure {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure
	}
	if errors.Is(err, context.Canceled) {
		return &Failure{Code: "canceled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Failure{Code: "deadline_exceeded"}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		return &Failure{Code: "azure_cli_failed", ExitCode: &code}
	}
	return &Failure{Code: "azure_cli_unavailable"}
}

// Is permits callers to recognize cancellation without exposing subprocess errors.
func (f *Failure) Is(target error) bool {
	return (f.Code == "canceled" && target == context.Canceled) ||
		(f.Code == "deadline_exceeded" && target == context.DeadlineExceeded)
}

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

package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const id1 = "/subscriptions/01234567-89ab-cdef-0123-456789abcdef/resourceGroups/customer-personal"
const id2 = "/subscriptions/11234567-89ab-cdef-0123-456789abcdef/resourceGroups/MC_personal_aks_westus3"

var instant = time.Date(2026, 9, 9, 10, 11, 12, 123456789, time.FixedZone("offset", 2*60*60))

type call struct {
	args []string
	data []byte
	err  error
}

type fakeExecutor struct {
	t     *testing.T
	calls []call
	index int
}

func (f *fakeExecutor) run(ctx context.Context, args ...string) ([]byte, error) {
	f.t.Helper()
	if f.index >= len(f.calls) {
		f.t.Fatalf("unexpected Azure call: %v", args)
	}
	c := f.calls[f.index]
	f.index++
	if !reflect.DeepEqual(args, c.args) {
		f.t.Fatalf("call %d: got %v, want %v", f.index, args, c.args)
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 10*time.Minute {
		f.t.Fatal("request has no bounded overall deadline")
	}
	return c.data, c.err
}

func show(id string, tags map[string]string) call {
	parts := strings.Split(id, "/")
	data, _ := json.Marshal(map[string]any{
		"id": id, "managedBy": "explicit-managed-node-group-is-allowed",
		"properties": map[string]string{"provisioningState": "Succeeded"}, "tags": tags,
	})
	return call{args: []string{"group", "show", "--name=" + parts[4], "--subscription", parts[2], "--query", "{id:id,managedBy:managedBy,properties:properties,tags:tags}", "--output", "json", "--only-show-errors"}, data: data}
}

func update(id, key string) call {
	return call{args: []string{"tag", "update", "--resource-id", id, "--operation", "Merge", "--tags", key + "=" + instant.UTC().Format(time.RFC3339Nano), "--output", "none", "--only-show-errors"}}
}

func TestRefresh(t *testing.T) {
	old := "2020-01-01T00:00:00.1234567+01:00"
	now := instant.UTC().Format(time.RFC3339Nano)
	f := &fakeExecutor{t: t, calls: []call{
		show(id1, map[string]string{"CreatedAT": old, "owner": "old-owner-secret"}),
		show(id2, nil),
		update(id1, "CreatedAT"),
		show(id1, map[string]string{"CreatedAT": now, "OWNER": "concurrent-new-owner-secret", "new-tag": "preserved"}),
		update(id2, "createdAt"), show(id2, map[string]string{"createdAt": now}),
	}}
	clockCalls := 0
	r, err := refresh(context.Background(), Spec{Version: "v1", ResourceGroups: []string{id1, strings.ToUpper(id1), id2}}, false, f, func() time.Time {
		clockCalls++
		if f.index != 2 {
			t.Fatal("clock used before all preflight reads")
		}
		return instant
	})
	if err != nil || r.Status != "verified" || len(r.ResourceGroups) != 2 || clockCalls != 1 || f.index != 6 {
		t.Fatalf("unexpected result %+v: %v (calls %d)", r, err, f.index)
	}
	if r.CreatedAt != "2026-09-09T08:11:12.123456789Z" {
		t.Fatalf("noncanonical time %s", r.CreatedAt)
	}
	for _, g := range r.ResourceGroups {
		if !g.Attempted || !g.Updated || !g.Verified || g.PossiblyUpdated || g.NewCreatedAt != now {
			t.Fatalf("bad write status: %+v", g)
		}
	}
	if r.ResourceGroups[0].OldStatus != "valid" || *r.ResourceGroups[0].OldCreatedAt != old || r.ResourceGroups[1].OldStatus != "missing" || r.ResourceGroups[1].OldCreatedAt != nil {
		t.Fatalf("old timestamps lost: %+v", r)
	}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), "secret") {
		t.Fatalf("unrelated tags exposed: %s", data)
	}
}

func TestDryRunAndInvalidOldTime(t *testing.T) {
	f := &fakeExecutor{t: t, calls: []call{show(id1, map[string]string{"createdAt": "not a date"})}}
	r, err := refresh(context.Background(), Spec{"v1", []string{id1}}, true, f, func() time.Time { return instant })
	if err != nil || r.Status != "planned" || r.ResourceGroups[0].OldStatus != "invalid" || r.ResourceGroups[0].Attempted || r.ResourceGroups[0].Verified || *r.ResourceGroups[0].OldCreatedAt != "not a date" {
		t.Fatalf("bad dry-run: %+v %v", r, err)
	}
}

func TestInvalidIDsNeverReadOrWrite(t *testing.T) {
	for _, id := range []string{
		"", "customer", strings.Replace(id1, "01234567-89ab-cdef-0123-456789abcdef", "not-uuid", 1),
		id1 + "/", id1 + "/providers/Microsoft.Compute/virtualMachines/vm", id1 + "?q=1", id1 + "#fragment",
		id1 + "%2Fchild", id1 + "%20", id1 + ";touch pwned", id1 + "$(id)", id1 + "`id`", id1 + "\n",
		strings.Replace(id1, "/resourceGroups/", "//resourceGroups/", 1), id1 + "\\other", id1 + ".",
	} {
		t.Run(id, func(t *testing.T) {
			f := &fakeExecutor{t: t}
			r, err := refresh(context.Background(), Spec{"v1", []string{id2, id}}, false, f, time.Now)
			if err == nil || r.Failure.Code != "invalid_resource_group_id" {
				t.Fatalf("accepted invalid ID: %+v %v", r, err)
			}
		})
	}
	for _, spec := range []Spec{{}, {"v2", []string{id1}}, {"v1", nil}} {
		if _, err := refresh(context.Background(), spec, false, &fakeExecutor{t: t}, time.Now); err == nil {
			t.Fatalf("accepted invalid spec %+v", spec)
		}
	}
}

func TestPreflightFailuresDoNotMutate(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		data       string
		err        error
	}{
		{name: "missing", code: "azure_cli_unavailable", err: errors.New("SECRET Azure stderr")},
		{name: "invalid JSON", code: "invalid_group_response", data: `{"secret":`},
		{name: "null", code: "resource_group_id_mismatch", data: `null`},
		{name: "wrong id", code: "resource_group_id_mismatch", data: `{"id":"` + id1 + `"}`},
		{name: "missing state", code: "missing_provisioning_state", data: `{"id":"` + id2 + `"}`},
		{name: "deleting", code: "resource_group_deleting", data: `{"id":"` + id2 + `","properties":{"provisioningState":"dElEtInG"}}`},
		{name: "ambiguous", code: "ambiguous_created_at", data: `{"id":"` + id2 + `","properties":{"provisioningState":"Succeeded"},"tags":{"createdAt":"a","CREATEDAT":"b"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := show(id2, nil)
			bad.data, bad.err = []byte(tc.data), tc.err
			f := &fakeExecutor{t: t, calls: []call{show(id1, nil), bad}}
			r, err := refresh(context.Background(), Spec{"v1", []string{id1, id2}}, false, f, func() time.Time {
				t.Fatal("clock called after failed preflight")
				return instant
			})
			if err == nil || r.Failure.Code != tc.code || r.ResourceGroups[0].Status != "pending" || r.ResourceGroups[1].Status != "failed" || r.ResourceGroups[1].Operation != "preflight" {
				t.Fatalf("bad preflight failure: %+v %v", r, err)
			}
			for _, g := range r.ResourceGroups {
				if g.Attempted || g.Updated || g.Verified {
					t.Fatalf("claimed mutation: %+v", g)
				}
			}
			data, _ := json.Marshal(r)
			if strings.Contains(string(data)+err.Error(), "SECRET") {
				t.Fatal("Azure error leaked")
			}
		})
	}
}

func TestPartialFailuresStopWithoutRollback(t *testing.T) {
	now := instant.UTC().Format(time.RFC3339Nano)
	for _, stage := range []string{"update", "reread", "timestamp", "id", "disappeared", "ambiguous", "deleting"} {
		t.Run(stage, func(t *testing.T) {
			write := update(id2, "createdAt")
			verify := show(id2, map[string]string{"createdAt": now, "owner": "changed"})
			code := "azure_cli_unavailable"
			switch stage {
			case "update":
				write.err = errors.New("SECRET update error")
			case "reread":
				verify.err = errors.New("SECRET reread error")
			case "timestamp":
				verify = show(id2, map[string]string{"createdAt": "old", "owner": "changed"})
				code = "created_at_mismatch"
			case "id":
				verify.data = show(id1, nil).data
				code = "resource_group_id_mismatch"
			case "disappeared":
				verify = show(id2, map[string]string{"createdAt": now})
				code = "unrelated_tag_disappeared"
			case "ambiguous":
				verify = show(id2, map[string]string{"createdAt": now, "CREATEDAT": now})
				code = "ambiguous_created_at"
			case "deleting":
				verify.data = []byte(strings.ReplaceAll(string(verify.data), "Succeeded", "Deleting"))
				code = "resource_group_deleting"
			}
			id3 := strings.Replace(id1, "customer-personal", "other", 1)
			calls := []call{show(id1, nil), show(id2, map[string]string{"owner": "original"}), show(id3, nil), update(id1, "createdAt"), show(id1, map[string]string{"createdAt": now}), write}
			if stage != "update" {
				calls = append(calls, verify)
			}
			f := &fakeExecutor{t: t, calls: calls}
			r, err := refresh(context.Background(), Spec{"v1", []string{id1, id2, id3}}, false, f, func() time.Time { return instant })
			if err == nil || r.Status != "failed" || r.Failure.Code != code || f.index != len(calls) {
				t.Fatalf("bad failure: %+v %v", r, err)
			}
			first, failed, pending := r.ResourceGroups[0], r.ResourceGroups[1], r.ResourceGroups[2]
			if !first.Verified || failed.Verified || !failed.Attempted || failed.Updated != (stage != "update") || failed.PossiblyUpdated != (stage == "update") || pending.Status != "pending" || pending.Attempted {
				t.Fatalf("misleading partial status: %+v", r)
			}
		})
	}
}

func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := refresh(ctx, Spec{"v1", []string{id1}}, false, &fakeExecutor{t: t}, time.Now)
	if !errors.Is(err, context.Canceled) || r.ResourceGroups[0].Attempted {
		t.Fatalf("ignored cancellation: %+v %v", r, err)
	}
}

// Run the test executable as a fake az, never the installed Azure CLI.
func TestMain(m *testing.M) {
	if os.Getenv("HCP_RETENTION_TEST_AZ") == "1" {
		switch os.Args[1] {
		case "fail":
			fmt.Fprint(os.Stderr, "SECRET authentication response")
			os.Exit(7)
		case "wait":
			time.Sleep(time.Minute)
		default:
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"env": os.Environ(), "args": os.Args[1:]})
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestAzureCLIIsolationAndErrors(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "az")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HOME", dir)
	t.Setenv("AZURE_CONFIG_DIR", "/must-not-use")
	t.Setenv("HCP_RETENTION_TEST_AZ", "1")
	args := []string{"inspect", "$(not-a-shell)", "a b"}
	data, err := (azureCLI{}).run(context.Background(), args...)
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Env, Args []string
	}
	if err := json.Unmarshal(data, &output); err != nil || !reflect.DeepEqual(args, output.Args) {
		t.Fatalf("arguments changed: %s %v", data, err)
	}
	count := 0
	for _, entry := range output.Env {
		if strings.HasPrefix(entry, "AZURE_CONFIG_DIR=") {
			count++
			if entry != "AZURE_CONFIG_DIR="+filepath.Join(dir, ".azure-redhat") {
				t.Fatalf("wrong account config: %s", entry)
			}
		}
	}
	if count != 1 || os.Getenv("AZURE_CONFIG_DIR") != "/must-not-use" {
		t.Fatal("config environment not isolated")
	}
	_, err = (azureCLI{}).run(context.Background(), "fail")
	failure := classify(err)
	if failure.Code != "azure_cli_failed" || failure.ExitCode == nil || *failure.ExitCode != 7 || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe error: %v %+v", err, failure)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = (azureCLI{}).run(ctx, "wait")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("subprocess deadline ignored: %v", err)
	}
	t.Setenv("HOME", "")
	_, err = (azureCLI{}).run(context.Background(), "inspect")
	if classify(err).Code != "home_directory_unavailable" {
		t.Fatalf("missing home accepted: %v", err)
	}
}

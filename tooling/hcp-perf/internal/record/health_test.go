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

package record

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

const healthManifestFixture = `{"spec":{"version":"v1","namespaces":["hcp"],"intervalSeconds":5,"durationSeconds":60,"enableWatch":true,"enableLogs":true,"outputDir":"SECRET-path","credentials":"SECRET-token"},"startedAt":"2026-01-01T00:00:00Z","limitations":"SECRET-text"}`
const healthCheckpointFixture = `{"type":"poll","atTime":"2026-01-01T00:00:05Z","durationMillis":12,"skippedTicks":2,"result":{"version":"v1","shutdown":"running","outputDir":"SECRET-path","polls":2,"samples":4,"apiErrors":1,"observationBytes":123,"streams":[{"namespace":"hcp","resource":"pods","samples":2,"pages":3,"entries":4,"apiErrors":1,"lastSuccess":"2026-01-01T00:00:04Z","message":"SECRET-body"}]}}`

func writeHealthFixture(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHealthRunningAndFinal(t *testing.T) {
	dir := t.TempDir()
	writeHealthFixture(t, dir, "manifest.json", healthManifestFixture)
	writeHealthFixture(t, dir, "checkpoints.ndjson", "invalid old record\n"+healthCheckpointFixture+"\n"+`{"partial":"SECRET`)
	for _, name := range []string{"watch.ndjson", "logs.ndjson", "pods.ndjson"} {
		writeHealthFixture(t, dir, name, "not JSON: SECRET raw body")
	}
	// Unknown credentials are never opened or followed, even as a dangling link.
	if err := os.Symlink("/missing/SECRET", filepath.Join(dir, "kubeconfig")); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(dir, "logs.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Health(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "unknown" || r.Final != nil || r.StreamSummary != nil || r.Checkpoint == nil || r.Checkpoint.Result.Polls != 2 || r.Checkpoint.Result.APIErrors != 1 || r.Checkpoint.Result.ObservationBytes != 123 || r.Checkpoint.SkippedTicks != 2 {
		t.Fatalf("incorrect running evidence: %+v", r)
	}
	if stream := r.Checkpoint.Result.Streams[0]; stream.LastSuccess == nil || stream.LastSuccess.Second() != 4 || stream.Pages != 3 || stream.Entries != 4 {
		t.Fatalf("missing per-stream progress: %+v", stream)
	}
	var found bool
	for _, file := range r.Files {
		if file.Path == "logs.ndjson" {
			found = true
			if file.Size != before.Size() || !file.ModTime.Equal(before.ModTime()) {
				t.Fatalf("incorrect log metadata: %+v", file)
			}
		}
	}
	if !found {
		t.Fatal("missing log metadata")
	}
	writeHealthFixture(t, dir, "summary.json", `{"version":"v1","shutdown":"error","apiErrors":3,"outputDir":"SECRET-path"}`)
	writeHealthFixture(t, dir, "stream-summary.json", `{"enableWatch":true,"enableLogs":true,"watch":{"required":4,"initialized":3,"bytes":70,"records":8,"errors":2,"gaps":1},"logs":{"bytes":90,"records":6,"gaps":2,"limitReached":true},"incomplete":true,"limitations":"SECRET-text","body":"SECRET-body"}`)
	r, err = Health(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "unknown" || r.Final == nil || r.Final.Shutdown != "error" || r.Final.APIErrors != 3 || r.StreamSummary == nil || r.StreamSummary.Watch.Gaps != 1 || r.StreamSummary.Logs.Gaps != 2 || !r.StreamSummary.Logs.LimitReached {
		t.Fatalf("missing terminal evidence: %+v", r)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("SECRET")) || bytes.Contains(data, []byte("outputDir")) || bytes.Contains(data, []byte("kubeconfig")) {
		t.Fatalf("health leaked private fields: %s", data)
	}
	after, err := os.Stat(filepath.Join(dir, "logs.ndjson"))
	if err != nil || !archiveUnchanged(before, after) {
		t.Fatalf("health modified evidence: %v", err)
	}
}

func TestHealthIncompleteFiles(t *testing.T) {
	for _, checkpoint := range []string{"", `{"partial":`, healthCheckpointFixture} {
		t.Run(checkpoint, func(t *testing.T) {
			dir := t.TempDir()
			writeHealthFixture(t, dir, "manifest.json", healthManifestFixture)
			writeHealthFixture(t, dir, "checkpoints.ndjson", checkpoint)
			writeHealthFixture(t, dir, "summary.json", `{"version":`)
			writeHealthFixture(t, dir, "stream-summary.json", "")
			r, err := Health(dir)
			if err != nil || r.Checkpoint != nil || r.Final != nil || r.StreamSummary != nil || len(r.Warnings) != 3 || r.Status != "unknown" {
				t.Fatalf("incomplete files must not imply health: %+v, %v", r, err)
			}
		})
	}
	dir := t.TempDir()
	writeHealthFixture(t, dir, "manifest.json", healthManifestFixture)
	r, err := Health(dir)
	if err != nil || r.Checkpoint != nil || r.Final != nil || len(r.Files) != 1 {
		t.Fatalf("manifest-only initialization: %+v, %v", r, err)
	}
}

func TestHealthRejectsUnsafeOrInvalidSources(t *testing.T) {
	for _, kind := range []string{"missing manifest", "null manifest", "invalid manifest", "unsupported version", "invalid namespaces", "oversized manifest", "oversized summary", "malformed checkpoint", "null checkpoint", "invalid type", "invalid resource", "invalid shutdown", "symlink root", "symlink manifest", "symlink logs", "hardlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if kind != "missing manifest" && kind != "symlink manifest" {
				writeHealthFixture(t, dir, "manifest.json", healthManifestFixture)
			}
			var err error
			switch kind {
			case "null manifest":
				writeHealthFixture(t, dir, "manifest.json", "null")
			case "invalid manifest":
				writeHealthFixture(t, dir, "manifest.json", `{"SECRET":`)
			case "unsupported version":
				writeHealthFixture(t, dir, "manifest.json", strings.Replace(healthManifestFixture, "v1", "SECRET", 1))
			case "invalid namespaces":
				writeHealthFixture(t, dir, "manifest.json", strings.Replace(healthManifestFixture, `"hcp"`, `"SECRET"`, 1))
			case "oversized manifest":
				writeHealthFixture(t, dir, "manifest.json", strings.Repeat("x", maxHealthReadBytes+1))
			case "oversized summary":
				writeHealthFixture(t, dir, "summary.json", strings.Repeat("x", maxHealthReadBytes+1))
			case "malformed checkpoint":
				writeHealthFixture(t, dir, "checkpoints.ndjson", healthCheckpointFixture+"\nSECRET invalid\n")
			case "null checkpoint":
				writeHealthFixture(t, dir, "checkpoints.ndjson", "null\n")
			case "invalid type", "invalid resource", "invalid shutdown":
				old := map[string]string{"invalid type": "poll", "invalid resource": "pods", "invalid shutdown": "running"}[kind]
				writeHealthFixture(t, dir, "checkpoints.ndjson", strings.Replace(healthCheckpointFixture, `"`+old+`"`, `"SECRET"`, 1)+"\n")
			case "symlink root":
				link := filepath.Join(t.TempDir(), "link")
				err = os.Symlink(dir, link)
				dir = link + "/"
			case "symlink manifest":
				err = os.Symlink("/missing/SECRET", filepath.Join(dir, "manifest.json"))
			case "symlink logs":
				err = os.Symlink("manifest.json", filepath.Join(dir, "logs.ndjson"))
			case "hardlink":
				err = os.Link(filepath.Join(dir, "manifest.json"), filepath.Join(dir, "logs.ndjson"))
			case "directory":
				err = os.Mkdir(filepath.Join(dir, "logs.ndjson"), 0700)
			case "fifo":
				err = syscall.Mkfifo(filepath.Join(dir, "checkpoints.ndjson"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Health(dir); err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), dir) {
				t.Fatalf("must reject safely: %v", err)
			}
		})
	}
}

func TestReadHealthFileBoundedTail(t *testing.T) {
	for _, tc := range []struct {
		name, suffix, want string
		fail               bool
	}{
		{"short", "\nlast\npartial", "last", false},
		{"long record", "\n" + strings.Repeat("a", 128<<10) + "\npartial", strings.Repeat("a", 128<<10), false},
		{"long suffix", "\nlast\n" + strings.Repeat("b", 128<<10), "last", false},
		{"at cap", "\n" + strings.Repeat("a", maxHealthReadBytes-2) + "\n", strings.Repeat("a", maxHealthReadBytes-2), false},
		{"over cap", "\n" + strings.Repeat("a", maxHealthReadBytes) + "\n", "", true},
		{"unterminated over cap", "\nlast\n" + strings.Repeat("b", maxHealthReadBytes), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "tail")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			// A sparse multi-GiB prefix ensures the reader seeks rather than scans.
			const prefix = 4 << 30
			if _, err := f.WriteAt([]byte(tc.suffix), prefix); err != nil {
				t.Fatal(err)
			}
			size := int64(prefix + len(tc.suffix))
			if _, err := f.WriteAt([]byte("\nnewer\n"), size); err != nil {
				t.Fatal(err)
			}
			data, err := readHealthFile(f, size, true)
			if (err != nil) != tc.fail || string(data) != tc.want {
				t.Fatalf("tail length=%d, error=%v, want length=%d fail=%v", len(data), err, len(tc.want), tc.fail)
			}
		})
	}
}

func TestHealthRecordedArchive(t *testing.T) {
	spec := testSpec(t)
	if _, err := Record(context.Background(), fake.NewClientset(), spec); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := WriteArchive(spec.OutputDir, &archive); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r := tar.NewReader(&archive)
	for {
		header, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		writeHealthFixture(t, dir, header.Name, string(data))
	}
	result, err := Health(dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Final == nil || result.Final.Shutdown != "duration" || result.Checkpoint == nil || result.Checkpoint.Type != "shutdown" || result.Final.Samples != 4 || result.Status != "unknown" || time.Since(result.ObservedAt) > time.Minute {
		t.Fatalf("incorrect exported recording health: %+v", result)
	}
}

func TestHealthLargeCheckpoint(t *testing.T) {
	dir := t.TempDir()
	var manifest HealthManifest
	manifest.Spec.Version = "v1"
	manifest.StartedAt = time.Now().UTC()
	checkpoint := Checkpoint{Type: "poll", AtTime: manifest.StartedAt, Result: Result{Version: "v1", Shutdown: "running"}}
	for i := range 100 {
		ns := strings.Repeat("n", 60) + string(rune('a'+i/26)) + string(rune('a'+i%26))
		manifest.Spec.Namespaces = append(manifest.Spec.Namespaces, ns)
		for _, resource := range resources {
			checkpoint.Result.Streams = append(checkpoint.Result.Streams, StreamResult{Namespace: ns, Resource: resource, Samples: 1, LastSuccess: &manifest.StartedAt})
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeHealthFixture(t, dir, "manifest.json", string(data))
	data, err = json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 64<<10 {
		t.Fatal("fixture must exceed initial tail window")
	}
	writeHealthFixture(t, dir, "checkpoints.ndjson", string(data)+"\npartial")
	result, err := Health(dir)
	if err != nil || result.Checkpoint == nil || len(result.Checkpoint.Result.Streams) != 400 {
		t.Fatalf("large checkpoint was not recovered: %v", err)
	}
}

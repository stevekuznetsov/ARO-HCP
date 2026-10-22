// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCardCLI(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "card.html")
	args := []string{"render", "--input", "docs/cards/small-v1.json", "--contention", "docs/cards/small-v1-contention.json", "--output", output}
	var metadata bytes.Buffer
	if err := runCard(args, &metadata); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions %o, want 0600", info.Mode().Perm())
	}
	var result struct {
		Output string `json:"output"`
		Bytes  int64  `json:"bytes"`
	}
	if err := json.Unmarshal(metadata.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Output != output || result.Bytes != info.Size() {
		t.Fatalf("incorrect metadata %+v for %d bytes", result, info.Size())
	}
	original, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	metadata.Reset()
	if err := runCard(args, &metadata); err == nil {
		t.Fatal("overwrote existing output")
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(original, after) || metadata.Len() != 0 {
		t.Fatal("existing output changed or success emitted")
	}
	if !bytes.HasPrefix(original, []byte("<!doctype html>")) {
		t.Fatal("not HTML")
	}
}

func TestCardCLIInvalidInputs(t *testing.T) {
	for _, args := range [][]string{nil, {"other"}, {"render"}, {"render", "--input", "missing", "--output", filepath.Join(t.TempDir(), "new.html")}, {"render", "--input", "docs/cards/small-v1.json", "--output", filepath.Join(t.TempDir(), "missing", "new.html")}, {"render", "--input", "docs/cards/small-v1.json", "--output", filepath.Join(t.TempDir(), "new.html"), "extra"}, {"render", "--input", "docs/cards/small-v1.json", "--contention", "missing", "--output", filepath.Join(t.TempDir(), "new.html")}} {
		var out bytes.Buffer
		if err := runCard(args, &out); err == nil {
			t.Errorf("accepted %v", args)
		}
		if out.Len() != 0 {
			t.Fatal("printed success for invalid input")
		}
	}
}

func TestCardCLIPortableInputsOnly(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "reviewed.json")
	output := filepath.Join(dir, "new.html")
	b, err := os.ReadFile("docs/cards/small-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	// All referenced artifacts can be absent: only the reviewed JSON is consumed.
	b = []byte(strings.ReplaceAll(string(b), "_artifacts/", "/does/not/exist/"))
	if err := os.WriteFile(input, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runCard([]string{"render", "--input", input, "--output", output}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.html")
	if err := os.Symlink(output, link); err != nil {
		t.Fatal(err)
	}
	if err := runCard([]string{"render", "--input", input, "--output", link}, &bytes.Buffer{}); err == nil {
		t.Fatal("followed existing output symlink")
	}
}

func TestCardCLIResources(t *testing.T) {
	for _, withContention := range []bool{false, true} {
		output := filepath.Join(t.TempDir(), "resources.html")
		args := []string{"render", "--input", "docs/cards/small-v1.json", "--resources", "docs/cards/small-v1-resources.json", "--output", output}
		if withContention {
			args = append(args, "--contention", "docs/cards/small-v1-contention.json")
		}
		var metadata bytes.Buffer
		if err := runCard(args, &metadata); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte("Complete container inventory (145 containers)")) || !bytes.Contains(b, []byte("3.86 CPU<br>14.104 GiB")) {
			t.Fatal("resource sidecar did not reach renderer")
		}
	}
	for _, payload := range []string{`{"schemaVersion":"hcp-perf.resources/v2"}`, "null"} {
		dir := t.TempDir()
		input, output := filepath.Join(dir, "invalid.json"), filepath.Join(dir, "output.html")
		if err := os.WriteFile(input, []byte(payload), 0600); err != nil {
			t.Fatal(err)
		}
		var metadata bytes.Buffer
		err := runCard([]string{"render", "--input", "docs/cards/small-v1.json", "--resources", input, "--output", output}, &metadata)
		if err == nil || !strings.Contains(err.Error(), "resources:") || metadata.Len() != 0 {
			t.Fatalf("invalid resources accepted or mislabeled: %v", err)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatal("created output for invalid resources")
		}
	}
}

func TestCardCLIExperience(t *testing.T) {
	for _, withOtherEvidence := range []bool{false, true} {
		output := filepath.Join(t.TempDir(), "experience.html")
		args := []string{"render", "--input", "docs/cards/small-v1.json", "--experience", "docs/cards/small-v1-experience.json", "--output", output}
		if withOtherEvidence {
			args = append(args, "--resources", "docs/cards/small-v1-resources.json", "--contention", "docs/cards/small-v1-contention.json")
		}
		var metadata bytes.Buffer
		if err := runCard(args, &metadata); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte(`id="experience-throughput"`)) || !bytes.Contains(b, []byte("17 cycles: 5 shared baseline")) || metadata.Len() == 0 {
			t.Fatal("experience did not reach renderer")
		}
		metadata.Reset()
		if err := runCard(args, &metadata); err == nil || metadata.Len() != 0 {
			t.Fatal("overwrote output or emitted success")
		}
	}
	for _, payload := range []string{`{"schemaVersion":"hcp-perf.experience/v2"}`, "null", "missing-file"} {
		dir := t.TempDir()
		input, output := filepath.Join(dir, "invalid.json"), filepath.Join(dir, "new.html")
		if payload != "missing-file" {
			if err := os.WriteFile(input, []byte(payload), 0600); err != nil {
				t.Fatal(err)
			}
		}
		var metadata bytes.Buffer
		err := runCard([]string{"render", "--input", "docs/cards/small-v1.json", "--experience", input, "--output", output}, &metadata)
		if err == nil || !strings.Contains(err.Error(), "experience:") || metadata.Len() != 0 {
			t.Fatalf("invalid experience accepted or mislabeled: %v", err)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatal("created output for invalid experience")
		}
	}
}

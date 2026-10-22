// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const ExportSchemaVersion = "hcp-perf.dashboard-export/v1"
const MaxSourceBytes int64 = 128 << 20

// ExportSpec is a private, reviewed allowlist. Dataset and RunMeta are public
// prose/assessment metadata; file paths and HCP identity are never copied out.
// Callers decoding JSON directly must resolve BaseDirectory against their input
// manifest's directory, not the process working directory.
type ExportSpec struct {
	SchemaVersion string     `json:"schemaVersion"`
	BaseDirectory string     `json:"baseDirectory"`
	Dataset       Dataset    `json:"dataset"`
	Runs          []RunInput `json:"runs"`
}

type InputRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	ID     string `json:"id"`
	Label  string `json:"label"`
}

type RunInput struct {
	SizeID       string     `json:"sizeID"`
	RunID        string     `json:"runID"`
	Directory    string     `json:"directory"`
	HCPNamespace string     `json:"hcpNamespace"`
	RunMeta      Run        `json:"runMeta"`
	Sources      RunSources `json:"sources"`
}

type RunSources struct {
	Spec            InputRef `json:"spec"`
	Results         InputRef `json:"results"`
	Outcome         InputRef `json:"outcome"`
	Requests        InputRef `json:"requests"`
	Events          InputRef `json:"events"`
	MetricsManifest InputRef `json:"metricsManifest"`
	// Empty CPU/CPUTime or Memory/MemoryTime pairs require corresponding
	// failed queries without response files in MetricsManifest.
	CPU            InputRef   `json:"cpu"`
	CPUTime        InputRef   `json:"cpuTime"`
	Memory         InputRef   `json:"memory"`
	MemoryTime     InputRef   `json:"memoryTime"`
	WorkingSetTime *InputRef  `json:"workingSetTime,omitempty"`
	RSSTime        *InputRef  `json:"rssTime,omitempty"`
	Inventories    []InputRef `json:"inventories"`
	Pods           InputRef   `json:"pods"`
	PodChunks      []InputRef `json:"podChunks,omitempty"`
	Deployments    InputRef   `json:"deployments"`
	StatefulSets   InputRef   `json:"statefulSets"`
	Evidence       []InputRef `json:"evidence,omitempty"`
}

func ReadExportSpec(path string) (ExportSpec, error) {
	var spec ExportSpec
	f, err := os.Open(path)
	if err != nil {
		return spec, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxInputBytes+1))
	if err != nil {
		return spec, err
	}
	if len(b) > MaxInputBytes {
		return spec, errors.New("export manifest exceeds size bound")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&spec); err != nil {
		return spec, err
	}
	if d.Decode(new(any)) != io.EOF {
		return spec, errors.New("trailing export manifest content")
	}
	if !filepath.IsAbs(spec.BaseDirectory) {
		spec.BaseDirectory = filepath.Join(filepath.Dir(path), spec.BaseDirectory)
	}
	spec.BaseDirectory, err = filepath.Abs(spec.BaseDirectory)
	return spec, err
}

type sourceReader struct {
	ctx     context.Context
	root    *os.Root
	sources map[string]Source
}

func (r *sourceReader) read(directory string, ref InputRef) ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	hash, err := hex.DecodeString(ref.SHA256)
	if err != nil || len(hash) != sha256.Size || ref.ID == "" || ref.Label == "" {
		return nil, errors.New("source requires public ID, label and SHA256")
	}
	if !filepath.IsLocal(ref.Path) || (directory != "" && !filepath.IsLocal(directory)) {
		return nil, errors.New("source paths must stay inside the private base directory")
	}
	path := filepath.Join(directory, ref.Path)
	info, err := r.root.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxSourceBytes {
		return nil, fmt.Errorf("source %s: not a bounded regular file", ref.ID)
	}
	f, err := r.root.Open(path)
	if err != nil {
		return nil, fmt.Errorf("source %s: cannot open declared file", ref.ID)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > MaxSourceBytes {
		return nil, fmt.Errorf("source %s: not a bounded regular file", ref.ID)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxSourceBytes+1))
	if err != nil || int64(len(b)) > MaxSourceBytes {
		return nil, fmt.Errorf("source %s: bounded read failed", ref.ID)
	}
	actual := sha256.Sum256(b)
	if !bytes.Equal(actual[:], hash) {
		return nil, fmt.Errorf("source %s: SHA256 mismatch", ref.ID)
	}
	s := Source{ID: ref.ID, Label: ref.Label, SHA256: strings.ToLower(ref.SHA256)}
	if previous, ok := r.sources[s.ID]; ok && previous != s {
		return nil, fmt.Errorf("source %s: conflicting citation", ref.ID)
	}
	r.sources[s.ID] = s
	return b, r.ctx.Err()
}

func decodeSource[T any](r *sourceReader, directory string, ref InputRef) (T, error) {
	var value T
	b, err := r.read(directory, ref)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(b, &value); err != nil {
		return value, fmt.Errorf("source %s: invalid JSON", ref.ID)
	}
	return value, nil
}

func eachJSON(b []byte, visit func(json.RawMessage) error) error {
	d := json.NewDecoder(bytes.NewReader(b))
	for {
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			if err == io.EOF {
				return nil
			}
			return errors.New("malformed NDJSON source")
		}
		if err := visit(raw); err != nil {
			return err
		}
	}
}

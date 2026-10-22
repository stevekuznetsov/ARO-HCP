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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const maxHealthReadBytes = 1 << 20

// HealthResult is a local, non-atomic observation, not a liveness probe. Status
// is always unknown: even a terminal summary cannot establish current health.
// Checkpoint and Final contain only allowlisted counters/timestamps and explicit
// namespace/resource names. Final and StreamSummary are absent until published;
// stream counters are final-only, not real-time. Files report last filesystem
// writes, not last successful capture or proof that every stream is progressing.
type HealthResult struct {
	Version       string             `json:"version"`
	Status        string             `json:"status"`
	ObservedAt    time.Time          `json:"observedAt"`
	Manifest      HealthManifest     `json:"manifest"`
	Checkpoint    *HealthCheckpoint  `json:"checkpoint,omitempty"`
	Final         *HealthSummary     `json:"final,omitempty"`
	StreamSummary *HealthLiveSummary `json:"streamSummary,omitempty"`
	Files         []HealthFile       `json:"files"`
	Warnings      []string           `json:"warnings,omitempty"`
	Limitations   string             `json:"limitations"`
}

// HealthManifest deliberately excludes outputDir and free-form limitations.
type HealthManifest struct {
	Spec struct {
		Version         string   `json:"version"`
		Namespaces      []string `json:"namespaces"`
		IntervalSeconds int64    `json:"intervalSeconds"`
		DurationSeconds int64    `json:"durationSeconds"`
		EnableWatch     bool     `json:"enableWatch"`
		EnableLogs      bool     `json:"enableLogs"`
	} `json:"spec"`
	StartedAt time.Time `json:"startedAt"`
}

// HealthCheckpoint describes the last newline-terminated checkpoint only.
// LastSuccess in each result stream is the last complete poll snapshot.
type HealthCheckpoint struct {
	Type           string        `json:"type"`
	AtTime         time.Time     `json:"atTime"`
	ScheduledAt    time.Time     `json:"scheduledAt"`
	PollStartedAt  time.Time     `json:"pollStartedAt"`
	PollFinishedAt time.Time     `json:"pollFinishedAt"`
	DurationMillis int64         `json:"durationMillis"`
	SkippedTicks   int64         `json:"skippedTicks"`
	Result         HealthSummary `json:"result"`
}

// HealthSummary excludes filesystem references and arbitrary diagnostic text.
// Shutdown is historical (running, duration, canceled, error), not pod status.
type HealthSummary struct {
	Version           string             `json:"version"`
	StartedAt         time.Time          `json:"startedAt"`
	EndedAt           time.Time          `json:"endedAt"`
	Polls             int64              `json:"polls"`
	Samples           int64              `json:"samples"`
	APIErrors         int64              `json:"apiErrors"`
	IncompleteStreams int64              `json:"incompleteStreams"`
	ObservationBytes  int64              `json:"observationBytes"`
	Shutdown          string             `json:"shutdown"`
	Streams           []StreamResult     `json:"streams"`
	Live              *HealthLiveSummary `json:"live,omitempty"`
}

// HealthLiveSummary contains final watch/log counters, never stream bodies.
type HealthLiveSummary struct {
	EnableWatch bool            `json:"enableWatch"`
	EnableLogs  bool            `json:"enableLogs"`
	Watch       LiveFileSummary `json:"watch"`
	Logs        LiveFileSummary `json:"logs"`
	Incomplete  bool            `json:"incomplete"`
}

// HealthFile names are fixed basenames; missing files are omitted. Size and
// ModTime are sampled independently of checkpoints and can advance asynchronously.
type HealthFile struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// Health reads only manifest.json, the tail of checkpoints.ndjson, summary.json
// and stream-summary.json. Each read is bounded to 1 MiB; checkpoint reads start
// with the last 64 KiB and expand backwards only to find the last complete line.
// An unterminated checkpoint suffix is ignored. Unpublished/partial final JSON
// is reported as unavailable with a warning; no final status is inferred.
// A valid public v1 manifest is required. Other files are only statted by a fixed
// top-level allowlist; credentials, raw logs and observation bodies are never
// parsed. Symlinks, special files and hardlinks at known names are rejected.
// Unknown files are ignored, and manifest paths are never followed. Errors do
// not include input contents or paths. Namespaces are retained, not a guarantee
// against secrets deliberately embedded in names. No API calls or writes occur.
// Reads can observe concurrent appends; bounded I/O is not a filesystem timeout.
func Health(dir string) (HealthResult, error) {
	result := HealthResult{
		Version: "v1", Status: "unknown", ObservedAt: time.Now().UTC(),
		Limitations: "Local non-atomic evidence only; current recorder and per-stream health are unknown. Checkpoints may be stale or delayed by API/filesystem I/O. File mtime is not last successful capture or durable storage confirmation. Watch/log counters are final-only; absent summaries do not prove the recorder is running. No live journal is available.",
	}
	info, err := os.Lstat(filepath.Clean(dir))
	if dir == "" || err != nil || !info.IsDir() {
		return result, errors.New("health input must be an existing directory, not a symlink")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return result, errors.New("cannot open health input directory")
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return result, errors.New("health input directory changed")
	}
	for _, name := range []string{"manifest.json", "checkpoints.ndjson", "summary.json", "stream-summary.json", "pods.ndjson", "events.ndjson", "deployments.ndjson", "statefulsets.ndjson", "watch.ndjson", "logs.ndjson"} {
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) && name != "manifest.json" {
			continue
		}
		if err != nil {
			return result, fmt.Errorf("cannot stat %s", name)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() < 0 {
			return result, fmt.Errorf("%s must be a singly-linked regular file", name)
		}
		var target any
		switch name {
		case "manifest.json":
			target = &result.Manifest
		case "checkpoints.ndjson":
			target = &result.Checkpoint
		case "summary.json":
			target = &result.Final
		case "stream-summary.json":
			target = &result.StreamSummary
		}
		if target != nil {
			// NOFOLLOW handles replacement symlinks; NONBLOCK prevents a replaced
			// FIFO from blocking before the opened file is verified.
			f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return result, fmt.Errorf("cannot open %s", name)
			}
			current, statErr := f.Stat()
			if statErr != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
				f.Close()
				return result, fmt.Errorf("%s changed before reading", name)
			}
			if stat, ok := current.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
				f.Close()
				return result, fmt.Errorf("%s must remain singly-linked", name)
			}
			data, readErr := readHealthFile(f, info.Size(), name == "checkpoints.ndjson")
			closeErr := f.Close()
			if readErr != nil || closeErr != nil {
				return result, fmt.Errorf("cannot read %s within 1 MiB bound", name)
			}
			if name != "manifest.json" && (len(data) == 0 || (name != "checkpoints.ndjson" && !json.Valid(data))) {
				result.Warnings = append(result.Warnings, name+": no complete record available")
			} else if bytes.Equal(bytes.TrimSpace(data), []byte("null")) || json.Unmarshal(data, target) != nil {
				return result, fmt.Errorf("invalid %s", name)
			}
		}
		result.Files = append(result.Files, HealthFile{Path: name, Size: info.Size(), ModTime: info.ModTime().UTC()})
		if name == "manifest.json" {
			if result.Manifest.Spec.Version != "v1" || len(result.Manifest.Spec.Namespaces) == 0 || result.Manifest.StartedAt.IsZero() {
				return result, errors.New("manifest.json requires a public v1 recording manifest")
			}
			seen := map[string]bool{}
			for _, ns := range result.Manifest.Spec.Namespaces {
				if len(validation.IsDNS1123Label(ns)) != 0 || seen[ns] {
					return result, errors.New("invalid manifest.json namespaces")
				}
				seen[ns] = true
			}
		}
	}
	var checkpointResult *HealthSummary
	if result.Checkpoint != nil {
		if result.Checkpoint.Type != "poll" && result.Checkpoint.Type != "shutdown" {
			return result, errors.New("invalid checkpoints.ndjson type")
		}
		checkpointResult = &result.Checkpoint.Result
	}
	for _, summary := range []*HealthSummary{checkpointResult, result.Final} {
		if summary == nil {
			continue
		}
		if summary.Version != "v1" || !slices.Contains([]string{"running", "duration", "canceled", "error"}, summary.Shutdown) {
			return result, errors.New("invalid recording summary version or shutdown")
		}
		for _, stream := range summary.Streams {
			if !slices.Contains(result.Manifest.Spec.Namespaces, stream.Namespace) || !slices.Contains(resources, stream.Resource) {
				return result, errors.New("invalid recording summary namespace or resource")
			}
		}
	}
	return result, nil
}

// ReadAt pins the end to the sampled size; appends cannot turn this into a scan.
func readHealthFile(f *os.File, size int64, tail bool) ([]byte, error) {
	if !tail {
		if size > maxHealthReadBytes {
			return nil, errors.New("metadata exceeds limit")
		}
		data := make([]byte, int(size))
		_, err := f.ReadAt(data, 0)
		return data, err
	}
	var data []byte
	offset := size
	for {
		n := min(offset, 64<<10, int64(maxHealthReadBytes-len(data)))
		chunk := make([]byte, int(n))
		offset -= n
		if _, err := f.ReadAt(chunk, offset); err != nil {
			return nil, err
		}
		data = append(chunk, data...)
		end := bytes.LastIndexByte(data, '\n')
		if end >= 0 {
			start := bytes.LastIndexByte(data[:end], '\n')
			if start >= 0 || offset == 0 {
				return data[start+1 : end], nil
			}
		}
		if offset == 0 {
			return nil, nil // Only an unterminated first record (or an empty file).
		}
		if len(data) == maxHealthReadBytes {
			return nil, io.ErrShortBuffer
		}
	}
}

// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package budget

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

func Hash(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }

func JSON(value any) ([]byte, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	return append(raw, '\n'), err
}

// Files includes the complete input receipt and resolved runtime policy; no
// external source paths are dereferenced or executed.
func Files(raw []byte, r Result) (map[string][]byte, error) {
	files := map[string][]byte{"input.json": raw}
	for name, value := range map[string]any{"candidate.json": r.Profile, "ledger.json": r.Ledger, "resolve.json": r.Resolved, "hypershift.json": r.HyperShift, "cost-input.json": r.CostSpec, "cost.json": r.Cost} {
		data, err := JSON(value)
		if err != nil {
			return nil, err
		}
		files[name] = data
	}
	return files, nil
}

// Store never overwrites. A manifest written last commits the output set;
// incomplete directories remain visible and cannot be mistaken for a success.
func Store(path string, files map[string][]byte, check bool) error {
	if path == "" {
		return fmt.Errorf("output path required")
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !parent.IsDir() {
		return fmt.Errorf("output parent must exist")
	}
	hashes := map[string]string{}
	names := make([]string, 0, len(files))
	for name, raw := range files {
		if name != filepath.Base(name) || name == "." || name == ".." || name == "output-hashes.json" {
			return fmt.Errorf("invalid output filename %q", name)
		}
		hashes[name] = Hash(raw)
		names = append(names, name)
	}
	manifest, err := JSON(hashes)
	if err != nil {
		return err
	}
	sort.Strings(names)
	names = append(names, "output-hashes.json")
	if check {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("check requires a real directory")
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(entries) != len(names) {
			return fmt.Errorf("output file set differs")
		}
	} else if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	for _, name := range names {
		raw := files[name]
		if name == "output-hashes.json" {
			raw = manifest
		}
		file := filepath.Join(path, name)
		if check {
			info, err := os.Lstat(file)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("not a regular output: %s", name)
			}
			saved, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			if !bytes.Equal(saved, raw) {
				return fmt.Errorf("output differs: %s", name)
			}
		} else {
			f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, writeErr := f.Write(raw)
			syncErr := f.Sync()
			closeErr := f.Close()
			if writeErr != nil {
				return writeErr
			}
			if syncErr != nil {
				return syncErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	if check {
		return nil
	}
	for _, dir := range []string{path, filepath.Dir(path)} {
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

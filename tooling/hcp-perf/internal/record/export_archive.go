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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"syscall"
	"time"
)

const (
	MaxArchiveFileBytes int64 = 8 << 30
	// Allow both 8 GiB live streams plus the 8 GiB polling budget and metadata.
	MaxArchiveBytes int64 = 32 << 30
)

var archiveNames = []string{
	"manifest.json", "summary.json", "checkpoints.ndjson", "pods.ndjson", "events.ndjson", "deployments.ndjson", "statefulsets.ndjson",
	"watch.ndjson", "logs.ndjson", "stream-summary.json",
}

func archiveNameAllowed(name string) bool {
	return slices.Contains(archiveNames, name)
}

func archiveRegular(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && stat.Nlink == 1 && info.Size() >= 0 && info.Size() <= MaxArchiveFileBytes
}

func archiveUnchanged(before, after os.FileInfo) bool {
	return os.SameFile(before, after) && before.Size() == after.Size() && before.Mode() == after.Mode() && before.ModTime().Equal(after.ModTime())
}

// WriteArchive emits only the recorder's known, top-level regular files as USTAR,
// using GNU headers for files too large for USTAR's octal size field.
// Missing files are allowed for interrupted recordings; unknown names, directories,
// symlinks and multiply-linked files fail closed. The source must be quiescent.
// Metadata checks detect ordinary concurrent changes, but are not a filesystem
// snapshot or protection against a hostile writer restoring timestamps.
// Contents are not sanitized: logs.ndjson contains raw logs and can contain
// secrets. Treat the archive as sensitive; do not publish it as a public artifact.
func WriteArchive(root string, out io.Writer) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("archive input must be a directory, not a symlink")
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	opened, err := dir.Stat(".")
	if err != nil || !archiveUnchanged(info, opened) {
		return errors.New("archive input directory changed")
	}
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	// One more than the allowlist bounds directory reads even for hostile input.
	entries, readErr := f.ReadDir(len(archiveNames) + 1)
	if readErr == io.EOF {
		readErr = nil
	}
	if err := errors.Join(readErr, f.Close()); err != nil {
		return err
	}
	if len(entries) == 0 {
		return errors.New("recording directory is empty")
	}
	if len(entries) > len(archiveNames) {
		return errors.New("recording directory contains too many entries")
	}
	files := make(map[string]os.FileInfo, len(entries))
	total := int64(1024) // Two terminating tar blocks.
	for _, entry := range entries {
		if !archiveNameAllowed(entry.Name()) {
			return errors.New("recording directory contains an unexpected filename")
		}
		info, err := dir.Lstat(entry.Name())
		if err != nil {
			return err
		}
		if !archiveRegular(info) {
			return fmt.Errorf("%s is not a singly-linked regular file within the size limit", entry.Name())
		}
		total += 512 + (info.Size()+511)/512*512
		if total > MaxArchiveBytes {
			return errors.New("archive exceeds total byte limit")
		}
		files[entry.Name()] = info
	}
	writer := tar.NewWriter(out)
	// A fixed order also makes identical recordings byte-for-byte reproducible.
	for _, name := range archiveNames {
		before, exists := files[name]
		if !exists {
			continue
		}
		// NOFOLLOW prevents a replacement symlink; NONBLOCK prevents a replaced
		// FIFO from blocking before fstat can reject it. Root confines resolution.
		f, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		current, statErr := f.Stat()
		if statErr != nil || !archiveRegular(current) || !archiveUnchanged(before, current) {
			f.Close()
			return fmt.Errorf("%s changed before archiving", name)
		}
		format := tar.FormatUSTAR
		if before.Size() >= 8<<30 {
			format = tar.FormatGNU
		}
		err = writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: before.Size(), Typeflag: tar.TypeReg, Format: format})
		if err == nil {
			_, err = io.CopyN(writer, f, before.Size())
		}
		after, statErr := f.Stat()
		err = errors.Join(err, statErr, f.Close())
		if err != nil {
			return err
		}
		if !archiveRegular(after) || !archiveUnchanged(before, after) {
			return fmt.Errorf("%s changed while archiving", name)
		}
	}
	for name, before := range files {
		after, err := dir.Lstat(name)
		if err != nil || !archiveRegular(after) || !archiveUnchanged(before, after) {
			return fmt.Errorf("%s changed while archiving", name)
		}
	}
	after, err := dir.Stat(".")
	if err != nil || !archiveUnchanged(opened, after) {
		return errors.New("recording directory changed while archiving")
	}
	return writer.Close()
}

// Wait holds the helper alive without busy polling or requiring a shell/image
// utility. The CLI's record hold command supplies the duration and signal context.
func Wait(ctx context.Context, duration time.Duration) error {
	if duration <= 0 || duration > 5*time.Minute {
		return errors.New("hold duration must be positive and at most 5m")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// validateExportArchive checks the received archive without extracting anything.
// Only the exact format emitted by WriteArchive is accepted, including its EOF.
func validateExportArchive(f *os.File, size int64) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := tar.NewReader(f)
	seen := map[string]bool{}
	expected := int64(1024)
	for {
		header, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("archive contains an invalid header")
		}
		format := tar.FormatUSTAR
		if header.Size >= 8<<30 {
			format = tar.FormatGNU
		}
		if !archiveNameAllowed(header.Name) || seen[header.Name] || header.Typeflag != tar.TypeReg || header.Linkname != "" || header.Format != format || header.Mode != 0600 || header.Size < 0 || header.Size > MaxArchiveFileBytes {
			return errors.New("archive contains an invalid entry")
		}
		seen[header.Name] = true
		expected += 512 + (header.Size+511)/512*512
		if expected > MaxArchiveBytes {
			return errors.New("archive exceeds total byte limit")
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			return errors.New("archive entry could not be read completely")
		}
	}
	if len(seen) == 0 || size != expected {
		return errors.New("archive is empty, truncated or has trailing data")
	}
	return nil
}

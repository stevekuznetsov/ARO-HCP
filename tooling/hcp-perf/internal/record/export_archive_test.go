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
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWriteArchive(t *testing.T) {
	dir := t.TempDir()
	names := []string{"manifest.json", "summary.json", "checkpoints.ndjson", "pods.ndjson", "events.ndjson", "deployments.ndjson", "statefulsets.ndjson", "watch.ndjson", "logs.ndjson", "stream-summary.json"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var first, second bytes.Buffer
	if err := WriteArchive(dir, &first); err != nil {
		t.Fatal(err)
	}
	if err := WriteArchive(dir, &second); err != nil || !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("archive not reproducible: %v", err)
	}
	reader := tar.NewReader(&first)
	for _, name := range names {
		header, err := reader.Next()
		if err != nil || header.Name != name || header.Typeflag != tar.TypeReg || header.Mode != 0600 || header.Format != tar.FormatUSTAR {
			t.Fatalf("invalid header: %+v, %v", header, err)
		}
		data, err := io.ReadAll(reader)
		if err != nil || string(data) != name+"\n" {
			t.Fatalf("archive data mismatch: %q, %v", data, err)
		}
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("unexpected extra entry: %v", err)
	}
}

func TestWriteArchiveRejectsUnsafeSources(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "directory", "fifo", "unknown", "backslash", "root symlink", "oversized file", "oversized total", "empty"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "pods.ndjson")
			outside := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(outside, []byte("SECRET"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch mode {
			case "symlink":
				err = os.Symlink(outside, file)
			case "hardlink":
				err = os.Link(outside, file)
			case "directory":
				err = os.Mkdir(file, 0700)
			case "fifo":
				err = syscall.Mkfifo(file, 0600)
			case "unknown":
				err = os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("SECRET"), 0600)
			case "backslash":
				err = os.WriteFile(filepath.Join(dir, `..\pods.ndjson`), []byte("SECRET"), 0600)
			case "root symlink":
				link := filepath.Join(t.TempDir(), "link")
				err = os.Symlink(dir, link)
				dir = link
			case "oversized file", "oversized total":
				for _, name := range []string{"pods.ndjson", "events.ndjson", "watch.ndjson", "logs.ndjson"} {
					path := filepath.Join(dir, name)
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
					size := MaxArchiveFileBytes
					if mode == "oversized file" {
						size++
					}
					if err := os.Truncate(path, size); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := WriteArchive(dir, &out); err == nil {
				t.Fatal("unsafe source accepted")
			}
			if out.Len() != 0 {
				t.Fatal("unsafe input was streamed before validation")
			}
		})
	}
	for _, name := range []string{"../pods.ndjson", "/pods.ndjson", "./pods.ndjson", "a/pods.ndjson", "C:\\pods.ndjson", "pods.ndjson\x00", "Pods.ndjson"} {
		if archiveNameAllowed(name) {
			t.Fatalf("unsafe archive name accepted: %q", name)
		}
	}
}

func TestWriteArchiveLiveStreamSafety(t *testing.T) {
	for _, name := range []string{"watch.ndjson", "logs.ndjson", "stream-summary.json"} {
		for _, kind := range []string{"symlink", "hardlink", "directory"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				outside := filepath.Join(t.TempDir(), "secret")
				if err := os.WriteFile(outside, []byte("SECRET"), 0600); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, name)
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(outside, path)
				case "hardlink":
					err = os.Link(outside, path)
				case "directory":
					err = os.Mkdir(path, 0700)
				}
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				if err := WriteArchive(dir, &out); err == nil || out.Len() != 0 {
					t.Fatal("unsafe live stream source accepted or emitted")
				}
			})
		}
	}
}

func TestWriteArchiveFullRecordingBudget(t *testing.T) {
	dir := t.TempDir()
	for name, size := range map[string]int64{
		"pods.ndjson": MaxObservationBytes, "watch.ndjson": MaxLiveStreamBytes, "logs.ndjson": MaxLiveStreamBytes,
		"manifest.json": 1, "summary.json": 1, "checkpoints.ndjson": 1, "stream-summary.json": 1,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		// Sparse files exercise actual copying at recorder limits without a large
		// allocation or writing GiBs of fixture data to disk.
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := &exportWriter{ctx: ctx, cancel: cancel, out: io.Discard, remaining: MaxArchiveBytes}
	if err := WriteArchive(dir, w); err != nil {
		t.Fatalf("permitted recording budget rejected: %v", err)
	}
	want := MaxObservationBytes + 2*MaxLiveStreamBytes + 7*512 + 4*512 + 1024
	if got := MaxArchiveBytes - w.remaining; got != want || got <= 2<<30 {
		t.Fatalf("archive truncated: wrote %d bytes, want %d", got, want)
	}
}

type archiveTestWriter func([]byte) (int, error)

func (f archiveTestWriter) Write(p []byte) (int, error) { return f(p) }

func TestWriteArchiveFailuresAndMutation(t *testing.T) {
	for _, mode := range []string{"header error", "chunk error", "close error", "append", "truncate", "replace", "symlink replace", "new file"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "pods.ndjson")
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 100000), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			sentinel := errors.New("output failed")
			err := WriteArchive(dir, archiveTestWriter(func(p []byte) (int, error) {
				calls++
				if (mode == "header error" && calls == 1) || (mode == "chunk error" && calls == 3) || (mode == "close error" && calls > 5) {
					return 0, sentinel
				}
				if calls == 2 {
					var err error
					switch mode {
					case "append":
						err = os.Truncate(path, 100001)
					case "truncate":
						err = os.Truncate(path, 1)
					case "replace", "symlink replace":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						if mode == "replace" {
							err = os.WriteFile(path, []byte("replacement"), 0600)
						} else {
							err = os.Symlink("/etc/passwd", path)
						}
					case "new file":
						err = os.WriteFile(filepath.Join(dir, "summary.json"), []byte("new"), 0600)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				return len(p), nil
			}))
			if err == nil {
				t.Fatal("archive accepted mutation or output failure")
			}
			if (mode == "header error" || mode == "chunk error" || mode == "close error") && !errors.Is(err, sentinel) {
				t.Fatalf("output error lost: %v", err)
			}
		})
	}
}

func TestValidateExportArchive(t *testing.T) {
	valid := exportArchiveFixture(t)
	for _, mode := range []string{"valid", "truncated", "no terminator", "trailing", "empty", "traversal", "symlink", "hardlink", "duplicate", "unknown", "oversized", "malformed header"} {
		t.Run(mode, func(t *testing.T) {
			data := valid
			switch mode {
			case "truncated":
				data = valid[:600]
			case "no terminator":
				data = valid[:len(valid)-1024]
			case "trailing":
				data = append(bytes.Clone(valid), 'x')
			case "empty":
				data = make([]byte, 1024)
			case "malformed header":
				data = bytes.Repeat([]byte("SECRET raw log"), 100)
			case "traversal", "symlink", "hardlink", "duplicate", "unknown", "oversized":
				var out bytes.Buffer
				w := tar.NewWriter(&out)
				h := &tar.Header{Name: "logs.ndjson", Mode: 0600, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
				switch mode {
				case "traversal":
					h.Name = "../logs.ndjson"
				case "symlink":
					h.Typeflag, h.Linkname = tar.TypeSymlink, "/etc/passwd"
				case "hardlink":
					h.Typeflag, h.Linkname = tar.TypeLink, "/etc/passwd"
				case "unknown":
					h.Name = "secret"
				case "oversized":
					h.Size = MaxArchiveFileBytes + 1
					h.Format = tar.FormatGNU
				}
				if err := w.WriteHeader(h); err != nil {
					t.Fatal(err)
				}
				if mode == "duplicate" {
					if err := w.WriteHeader(h); err != nil {
						t.Fatal(err)
					}
				}
				if err := w.Close(); err != nil && mode != "oversized" {
					t.Fatal(err)
				}
				data = out.Bytes()
			}
			f, err := os.CreateTemp(t.TempDir(), "archive")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			f, err = os.Open(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			err = validateExportArchive(f, int64(len(data)))
			if (err == nil) != (mode == "valid") {
				t.Fatalf("unexpected archive validation: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("archive validation exposed remote content: %v", err)
			}
		})
	}
}

func TestValidateExportArchiveLargeFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "archive")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var header bytes.Buffer
	if err := tar.NewWriter(&header).WriteHeader(&tar.Header{Name: "logs.ndjson", Mode: 0600, Size: 8 << 30, Typeflag: tar.TypeReg, Format: tar.FormatGNU}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(header.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(8<<30, io.SeekCurrent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	if err := validateExportArchive(f, int64(header.Len())+(8<<30)+1024); err != nil {
		t.Fatalf("exact 8 GiB file rejected: %v", err)
	}
}

func TestExportWriter(t *testing.T) {
	for _, mode := range []string{"limit", "canceled", "short", "error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			out := io.Writer(io.Discard)
			sentinel := errors.New("disk error")
			if mode == "short" {
				out = archiveTestWriter(func(p []byte) (int, error) { return len(p) - 1, nil })
			}
			if mode == "error" {
				out = archiveTestWriter(func([]byte) (int, error) { return 0, sentinel })
			}
			w := &exportWriter{ctx: ctx, cancel: cancel, out: out, remaining: 4}
			if mode == "canceled" {
				cancel()
			}
			if mode == "limit" {
				if n, err := w.Write([]byte("1234")); err != nil || n != 4 {
					t.Fatalf("exact limit rejected: %d, %v", n, err)
				}
			}
			if _, err := w.Write([]byte("1")); err == nil || ctx.Err() == nil {
				t.Fatalf("writer failed to abort context: %v", err)
			}
			if mode == "error" && !errors.Is(w.err, sentinel) {
				t.Fatal("disk error lost")
			}
			if n, err := w.Write([]byte("2")); n != 0 || err != w.err {
				t.Fatal("writer did not retain first error")
			}
		})
	}
}

func TestExportWait(t *testing.T) {
	for _, duration := range []time.Duration{0, -1, 5*time.Minute + 1} {
		if err := Wait(t.Context(), duration); err == nil {
			t.Fatal("invalid hold duration accepted")
		}
	}
	if err := Wait(t.Context(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Wait(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("hold cancellation lost: %v", err)
	}
}

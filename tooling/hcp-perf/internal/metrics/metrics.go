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

// Package metrics exports one bounded Prometheus range query batch and analyzes
// its local evidence. It is not a durable capture server or a capacity evaluator.
package metrics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const ManifestVersion = 1

const (
	StatusPending    = "pending"
	StatusSuccess    = "success"
	StatusIncomplete = "incomplete"
	StatusError      = "error"
)

type Query struct {
	Name string `json:"name"`
	Expr string `json:"expr"`
}

// QueryResult describes capture/data quality, never a capacity pass or fail.
// File and SHA256 are absent if no response could be saved.
type QueryResult struct {
	Query
	Status     string   `json:"status"`
	Error      string   `json:"error,omitempty"`
	HTTPStatus int      `json:"httpStatus,omitempty"`
	File       string   `json:"file,omitempty"`
	SHA256     string   `json:"sha256,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// Manifest intentionally contains neither endpoint nor authentication settings.
// Expressions and raw server responses are evidence and may contain sensitive
// application data; the capture directory must be kept private.
type Manifest struct {
	Version int           `json:"version"`
	Start   time.Time     `json:"start"`
	End     time.Time     `json:"end"`
	Step    time.Duration `json:"stepNanoseconds"`
	Queries []QueryResult `json:"queries"`
}

var queryName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validate(queries []Query, start, end time.Time, step time.Duration) error {
	if start.IsZero() || end.IsZero() || start.Year() < 1 || end.Year() > 9999 || end.Before(start) || step < time.Second || end.After(start.Add(time.Duration(1<<63-1))) {
		return errors.New("invalid time range or step (minimum step is one second)")
	}
	if step%time.Millisecond != 0 {
		return errors.New("step must be representable in whole milliseconds")
	}
	if len(queries) == 0 {
		return errors.New("at least one query is required")
	}
	seen := make(map[string]bool, len(queries))
	for _, q := range queries {
		if !queryName.MatchString(q.Name) || q.Name == "manifest" || seen[q.Name] || strings.TrimSpace(q.Expr) == "" {
			return fmt.Errorf("invalid or duplicate query name or empty expression: %q", q.Name)
		}
		seen[q.Name] = true
	}
	return nil
}

// Export creates dir exclusively (its parent must exist), then requests queries
// sequentially with a 60-second context deadline per request, including body
// reads. A nil client uses http.DefaultClient. Authentication, if needed, belongs
// in the caller's RoundTripper. The client/transport must honor request contexts.
// Each raw body is saved and synced before an atomic manifest replacement.
// Errors return a partial manifest; warnings/gaps have status "incomplete".
// Steps must be whole milliseconds; evaluation times use millisecond precision.
// No directory fsync or crash recovery is promised. Existing output is never reused.
func Export(ctx context.Context, client *http.Client, endpoint string, queries []Query, start, end time.Time, step time.Duration, dir string) (Manifest, error) {
	var m Manifest
	if err := validate(queries, start, end, step); err != nil {
		return m, err
	}
	u, err := url.Parse(endpoint)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(endpoint, "#") {
		return m, errors.New("endpoint must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/query_range"
	u.RawPath = ""
	if ctx == nil {
		return m, errors.New("context is required")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return m, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	m = Manifest{Version: ManifestVersion, Start: start.UTC(), End: end.UTC(), Step: step}
	for _, q := range queries {
		m.Queries = append(m.Queries, QueryResult{Query: q, Status: StatusPending})
	}
	if err := writeManifest(dir, m); err != nil {
		return m, err
	}
	var errs []error
	for i := range m.Queries {
		r := &m.Queries[i]
		requestURL := *u
		requestURL.RawQuery = url.Values{
			"query": {r.Expr}, "start": {m.Start.Format(time.RFC3339Nano)},
			"end": {m.End.Format(time.RFC3339Nano)}, "step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
		}.Encode()
		requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := capture(requestCtx, client, requestURL.String(), dir, r, m)
		cancel()
		if err != nil {
			r.Status = StatusError
			if r.Error == "" {
				r.Error = err.Error()
			}
			errs = append(errs, fmt.Errorf("query %s: %w", r.Name, err))
		}
		if err := writeManifest(dir, m); err != nil {
			return m, errors.Join(append(errs, err)...)
		}
	}
	return m, errors.Join(errs...)
}

func capture(ctx context.Context, client *http.Client, endpoint, dir string, r *QueryResult, m Manifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("cannot construct range request")
	}
	resp, err := client.Do(req)
	if err != nil {
		// Transport errors can include credentials added by a caller's transport.
		r.Error = "range request failed"
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New(r.Error)
	}
	defer resp.Body.Close()
	r.HTTPStatus = resp.StatusCode
	name := r.Name + ".json"
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, hash), resp.Body)
	syncErr := f.Sync()
	closeErr := f.Close()
	r.File, r.SHA256 = name, hex.EncodeToString(hash.Sum(nil))
	if copyErr != nil {
		r.Error = "reading or saving response failed"
		return errors.Join(errors.New(r.Error), ctx.Err(), syncErr, closeErr)
	}
	if err := errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	summary, err := summarize(body, m)
	r.Warnings = summary.Warnings
	if err != nil {
		return err
	}
	r.Status = summary.Status
	return nil
}

func writeManifest(dir string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(append(b, '\n'))
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "manifest.json"))
}

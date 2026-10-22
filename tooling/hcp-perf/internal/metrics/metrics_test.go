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
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var testStart = time.Unix(1700000000, 0).UTC()

const completeResponse = `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"__name__":"up","instance":"a"},"values":[[1700000000,"1"],[1700000001,"2"],[1700000002,"3"]]},{"metric":{"__name__":"up","instance":"b"},"values":[[1700000000,"4"],[1700000001,"5"],[1700000002,"6"]]}]}}`

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func exportTest(t *testing.T, body string, status int) (string, Manifest, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	dir := filepath.Join(t.TempDir(), "dump")
	m, err := Export(context.Background(), srv.Client(), srv.URL, []Query{{Name: "up", Expr: "up"}}, testStart, testStart.Add(2*time.Second), time.Second, dir)
	return dir, m, err
}

func loadManifest(t *testing.T, dir string) Manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestExportAnalyze(t *testing.T) {
	expr := `sum(rate(requests_total{label="a+b & c/d"}[5m]))`
	const token = "not-to-be-persisted"
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Query().Get("query"))
		if r.URL.Path != "/prometheus/api/v1/query_range" || r.URL.Query().Get("query") != expr || r.URL.Query().Get("step") != "1" || r.URL.Query().Get("start") != testStart.Format(time.RFC3339Nano) || r.URL.Query().Get("end") != testStart.Add(2*time.Second).Format(time.RFC3339Nano) {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("caller authentication not used")
		}
		_, _ = io.WriteString(w, completeResponse)
	}))
	defer srv.Close()
	client := srv.Client()
	transport := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 60*time.Second || time.Until(deadline) < 55*time.Second {
			t.Errorf("missing 60-second request bound: %v", deadline)
		}
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+token)
		return transport.RoundTrip(r)
	})
	dir := filepath.Join(t.TempDir(), "dump")
	queries := []Query{{Name: "first", Expr: expr}, {Name: "second", Expr: expr}}
	m, err := Export(context.Background(), client, srv.URL+"/prometheus/", queries, testStart, testStart.Add(2*time.Second), time.Second, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || !reflect.DeepEqual(m, loadManifest(t, dir)) {
		t.Fatal("requests or persisted manifest differ")
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory permissions: %v, %v", info, err)
	}
	for _, name := range []string{"manifest.json", "first.json", "second.json"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("file permissions: %v, %v", info, err)
		}
		b, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(b), token) || strings.Contains(string(b), srv.URL) {
			t.Fatalf("credentials/endpoint persisted or read failed: %v", err)
		}
		if name != "manifest.json" && string(b) != completeResponse {
			t.Fatal("raw response was changed")
		}
	}
	a, err := Analyze(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Queries) != 2 || len(a.Queries[0].Series) != 2 || a.Queries[0].Status != StatusSuccess {
		t.Fatalf("unexpected analysis: %+v", a)
	}
	for i, s := range a.Queries[0].Series {
		if s.Samples != 3 || s.ValidSamples != 3 || s.InvalidSamples != 0 || s.Expected != 3 || s.MissingApprox != 0 || s.Coverage != 1 || s.Min == nil || *s.Min != float64(1+3*i) || s.Max == nil || *s.Max != float64(3+3*i) || s.FirstTimestamp == nil || *s.FirstTimestamp != float64(testStart.Unix()) || s.LastTimestamp == nil || *s.LastTimestamp != float64(testStart.Unix()+2) || s.Labels["instance"] != []string{"a", "b"}[i] {
			t.Fatalf("unexpected series: %+v", s)
		}
	}
	if _, err := Export(context.Background(), client, srv.URL, queries, testStart, testStart, time.Second, dir); err == nil {
		t.Fatal("existing directory accepted")
	}
}

func TestExportValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		queries  []Query
		endpoint string
		start    time.Time
		end      time.Time
		step     time.Duration
	}{
		{name: "duplicate", queries: []Query{{"up", "up"}, {"up", "down"}}},
		{name: "traversal", queries: []Query{{"../up", "up"}}},
		{name: "absolute", queries: []Query{{"/up", "up"}}},
		{name: "backslash", queries: []Query{{`a\b`, "up"}}},
		{name: "reserved", queries: []Query{{"manifest", "up"}}},
		{name: "uppercase", queries: []Query{{"UP", "up"}}},
		{name: "long", queries: []Query{{strings.Repeat("a", 64), "up"}}},
		{name: "empty expression", queries: []Query{{"up", " \t"}}},
		{name: "no queries", queries: []Query{}},
		{name: "credentials", endpoint: "https://user:password@example.com"},
		{name: "query", endpoint: "https://example.com?token=secret"},
		{name: "empty query", endpoint: "https://example.com?"},
		{name: "fragment", endpoint: "https://example.com#secret"},
		{name: "empty fragment", endpoint: "https://example.com#"},
		{name: "relative", endpoint: "/prometheus"},
		{name: "scheme", endpoint: "file:///tmp/data"},
		{name: "host", endpoint: "http:///path"},
		{name: "bad URL", endpoint: "http://%"},
		{name: "short step", step: time.Millisecond},
		{name: "negative step", step: -time.Second},
		{name: "submillisecond step precision", step: time.Second + time.Nanosecond},
		{name: "reverse time", end: testStart.Add(-time.Second)},
		{name: "invalid year", end: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "overflow range", end: testStart.AddDate(300, 0, 0)},
		{name: "duration overflow by one", end: testStart.Add(time.Duration(1<<63 - 1)).Add(time.Nanosecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.queries == nil {
				tc.queries = []Query{{"up", "up"}}
			}
			if tc.endpoint == "" {
				tc.endpoint = "http://example.com"
			}
			if tc.start.IsZero() {
				tc.start = testStart
			}
			if tc.end.IsZero() {
				tc.end = testStart
			}
			if tc.step == 0 {
				tc.step = time.Second
			}
			dir := filepath.Join(t.TempDir(), "dump")
			if _, err := Export(context.Background(), nil, tc.endpoint, tc.queries, tc.start, tc.end, tc.step, dir); err == nil {
				t.Fatal("invalid input accepted")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("invalid input created output: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		start time.Time
		step  time.Duration
	}{{testStart, 0}, {time.Time{}, time.Second}} {
		_, err := Export(context.Background(), nil, "http://example.com", []Query{{"up", "up"}}, tc.start, testStart, tc.step, filepath.Join(t.TempDir(), "dump"))
		if err == nil {
			t.Fatal("zero time/step accepted")
		}
	}
	_, err := Export(context.Background(), nil, "http://example.com", []Query{{"up", "up"}}, testStart, testStart, time.Second, filepath.Join(t.TempDir(), "absent", "dump"))
	if err == nil {
		t.Fatal("missing parent accepted")
	}
}

func TestEvaluationGrid(t *testing.T) {
	for _, tc := range []struct {
		name       string
		start      time.Time
		window     time.Duration
		step       time.Duration
		values     string
		expected   int64
		valid      int
		invalid    int
		wantStatus string
	}{
		{
			name: "clustered samples do not fill grid", start: testStart,
			window: time.Minute, step: 30 * time.Second,
			values:   `[1700000000,"1"],[1700000001,"2"],[1700000002,"3"]`,
			expected: 3, valid: 1, invalid: 2, wantStatus: StatusError,
		},
		{
			name: "complete grid", start: testStart,
			window: time.Minute, step: 30 * time.Second,
			values:   `[1700000000,"1"],[1700000030,"2"],[1700000060,"3"]`,
			expected: 3, valid: 3, wantStatus: StatusSuccess,
		},
		{
			name: "missing middle position", start: testStart,
			window: time.Minute, step: 30 * time.Second,
			values:   `[1700000000,"1"],[1700000060,"3"]`,
			expected: 3, valid: 2, wantStatus: StatusIncomplete,
		},
		{
			name: "distinct timestamps at same position", start: testStart,
			window: time.Minute, step: 30 * time.Second,
			values:   `[1700000000,"1"],[1700000000.001,"2"],[1700000060,"3"]`,
			expected: 3, valid: 2, invalid: 1, wantStatus: StatusError,
		},
		{
			name: "millisecond tolerance at both boundaries", start: testStart,
			window: time.Minute, step: 30 * time.Second,
			values:   `[1699999999.999,"1"],[1700000030.001,"2"],[1700000060.001,"3"]`,
			expected: 3, valid: 3, wantStatus: StatusSuccess,
		},
		{
			name: "outside millisecond tolerance", start: testStart,
			window: time.Minute, step: 30 * time.Second,
			values:   `[1700000000,"1"],[1700000030.002,"2"],[1700000060,"3"]`,
			expected: 3, valid: 2, invalid: 1, wantStatus: StatusError,
		},
		{
			name: "submillisecond start and fractional step", start: testStart.Add(999 * time.Microsecond),
			window: 2002 * time.Millisecond, step: 1001 * time.Millisecond,
			values:   `[1700000000,"1"],[1700000001.001,"2"],[1700000002.002,"3"]`,
			expected: 3, valid: 3, wantStatus: StatusSuccess,
		},
		{
			name: "millisecond window rounding includes endpoint", start: testStart.Add(999 * time.Microsecond),
			window: time.Second - 999*time.Microsecond, step: time.Second,
			values:   `[1700000000,"1"],[1700000001,"2"]`,
			expected: 2, valid: 2, wantStatus: StatusSuccess,
		},
		{
			name: "nondivisible window", start: testStart,
			window: 59 * time.Second, step: 30 * time.Second,
			values:   `[1700000000,"1"],[1700000030,"2"],[1700000059,"3"]`,
			expected: 2, valid: 2, invalid: 1, wantStatus: StatusError,
		},
		{
			name: "largest window stays sparse", start: testStart,
			window: time.Duration(1<<63 - 1), step: time.Second,
			values:   `[1700000000,"1"]`,
			expected: 9223372037, valid: 1, wantStatus: StatusIncomplete,
		},
		{
			name: "large representable step", start: testStart,
			window: time.Duration(1<<63 - 1), step: 9223372036854 * time.Millisecond,
			values:   `[1700000000,"1"],[10923372036.854,"2"]`,
			expected: 2, valid: 2, wantStatus: StatusSuccess,
		},
		{
			name: "extreme timestamp rejected before integer conversion", start: testStart,
			window: time.Minute, step: 30 * time.Second,
			values:   `[1e308,"1"]`,
			expected: 3, invalid: 1, wantStatus: StatusError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"instance":"a"},"values":[` + tc.values + `]}]}}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			dir := filepath.Join(t.TempDir(), "dump")
			m, err := Export(context.Background(), srv.Client(), srv.URL, []Query{{"up", "up"}}, tc.start, tc.start.Add(tc.window), tc.step, dir)
			if (err != nil) != (tc.wantStatus == StatusError) || m.Queries[0].Status != tc.wantStatus {
				t.Fatalf("export status: %+v, %v", m, err)
			}
			a, err := Analyze(dir)
			if (err != nil) != (tc.wantStatus == StatusError) || a.Queries[0].Status != tc.wantStatus {
				t.Fatalf("analysis status: %+v, %v", a, err)
			}
			s := a.Queries[0].Series[0]
			if s.Expected != tc.expected || s.ValidSamples != tc.valid || s.InvalidSamples != tc.invalid || s.Samples != tc.valid+tc.invalid || s.MissingApprox != tc.expected-int64(tc.valid) || s.Coverage != float64(tc.valid)/float64(tc.expected) {
				t.Fatalf("incorrect grid coverage: %+v", s)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "up.json"))
			if err != nil || string(raw) != body {
				t.Fatalf("grid validation changed raw evidence: %s, %v", raw, err)
			}
		})
	}
}

func TestExportDoesNotOverwriteResponse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dump")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(filepath.Join(dir, "up.json"), []byte("existing evidence"), 0600); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, completeResponse)
	}))
	defer srv.Close()
	m, err := Export(context.Background(), srv.Client(), srv.URL, []Query{{"up", "up"}}, testStart, testStart.Add(2*time.Second), time.Second, dir)
	if err == nil || m.Queries[0].Status != StatusError || m.Queries[0].File != "" || !reflect.DeepEqual(m, loadManifest(t, dir)) {
		t.Fatalf("response collision not recorded: %+v %v", m, err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "up.json"))
	if err != nil || string(body) != "existing evidence" {
		t.Fatalf("existing response overwritten: %s %v", body, err)
	}
}

func TestExportResponseQuality(t *testing.T) {
	for _, tc := range []struct {
		name, body, status string
		httpStatus         int
		wantErr            bool
	}{
		{"HTTP error", `{"status":"error","error":"unavailable"}`, StatusError, 503, true},
		{"API error", `{"status":"error","error":"bad query"}`, StatusError, 200, true},
		{"malformed", `{"status":`, StatusError, 200, true},
		{"missing data", `{"status":"success"}`, StatusError, 200, true},
		{"empty data", `{"status":"success","data":{"resultType":"matrix","result":[]}}`, StatusError, 200, true},
		{"missing result", `{"status":"success","data":{"resultType":"matrix"}}`, StatusError, 200, true},
		{"wrong type", `{"status":"success","data":{"resultType":"vector","result":[]}}`, StatusError, 200, true},
		{"warnings", strings.Replace(completeResponse, `"status":"success"`, `"status":"success","warnings":["partial response"]`, 1), StatusIncomplete, 200, false},
		{"infos", strings.Replace(completeResponse, `"status":"success"`, `"status":"success","infos":["omitted histogram"]`, 1), StatusIncomplete, 200, false},
		{"gap", strings.Replace(completeResponse, `[1700000001,"2"],`, "", 1), StatusIncomplete, 200, false},
		{"nonfinite", strings.Replace(completeResponse, `"2"`, `"NaN"`, 1), StatusError, 200, true},
		{"infinite", strings.Replace(completeResponse, `"2"`, `"+Inf"`, 1), StatusError, 200, true},
		{"nonnumeric", strings.Replace(completeResponse, `"2"`, `"no"`, 1), StatusError, 200, true},
		{"null timestamp", strings.Replace(completeResponse, `1700000001`, `null`, 1), StatusError, 200, true},
		{"out of range", strings.Replace(completeResponse, `1700000001`, `1700000003`, 1), StatusError, 200, true},
		{"duplicate time", strings.Replace(completeResponse, `1700000001`, `1700000000`, 1), StatusError, 200, true},
		{"malformed sample", strings.Replace(completeResponse, `[1700000001,"2"]`, `[1700000001]`, 1), StatusError, 200, true},
		{"empty series", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"instance":"a"},"values":[]}]}}`, StatusError, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, m, err := exportTest(t, tc.body, tc.httpStatus)
			if (err != nil) != tc.wantErr || m.Queries[0].Status != tc.status {
				t.Fatalf("export: %+v, %v", m, err)
			}
			if !reflect.DeepEqual(m, loadManifest(t, dir)) || m.Queries[0].File != "up.json" || m.Queries[0].SHA256 == "" || m.Queries[0].HTTPStatus != tc.httpStatus {
				t.Fatalf("incomplete manifest: %+v", m)
			}
			a, err := Analyze(dir)
			if (err != nil) != tc.wantErr || a.Queries[0].Status != tc.status {
				t.Fatalf("analysis: %+v, %v", a, err)
			}
			if tc.status == StatusIncomplete && len(a.Queries[0].Warnings) == 0 {
				t.Fatal("warnings/gaps not reported")
			}
			if tc.name == "gap" {
				s := a.Queries[0].Series[0]
				if s.Samples != 2 || s.MissingApprox != 1 || s.Coverage != 2.0/3 || *s.Min != 1 {
					t.Fatalf("missing sample treated as zero or hidden: %+v", s)
				}
			}
			if tc.name == "empty series" && a.Queries[0].Series[0].Min != nil {
				t.Fatal("missing samples have numeric minimum")
			}
		})
	}
}

func TestExportCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":`)
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "dump")
	m, err := Export(ctx, srv.Client(), srv.URL, []Query{{"first", "up"}, {"second", "up"}}, testStart, testStart, time.Second, dir)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(m, loadManifest(t, dir)) {
		t.Fatalf("cancellation lost or manifest unreadable: %+v, %v", m, err)
	}
	for _, q := range m.Queries {
		if q.Status != StatusError || q.Error == "" {
			t.Fatalf("canceled query not marked failed: %+v", q)
		}
	}
	if _, err := Analyze(dir); err == nil {
		t.Fatal("canceled capture analyzed successfully")
	}
}

func TestExportTransportErrorDoesNotPersistSecrets(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("Bearer secret-value")
	})}
	dir := filepath.Join(t.TempDir(), "dump")
	m, err := Export(context.Background(), client, "http://example.com", []Query{{"up", "up"}}, testStart, testStart, time.Second, dir)
	if err == nil || strings.Contains(err.Error(), "secret-value") || strings.Contains(fmt.Sprint(loadManifest(t, dir)), "secret-value") || m.Queries[0].File != "" {
		t.Fatalf("transport error leaked or not recorded: %+v %v", m, err)
	}
}

func TestExportPersistsBeforeNextQuery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dump")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"status":"error"}`)
			return
		}
		m := loadManifest(t, dir)
		if m.Queries[0].Status != StatusError || m.Queries[0].File != "first.json" || m.Queries[1].Status != StatusPending {
			t.Errorf("first query not persisted before second: %+v", m)
		}
		_, _ = io.WriteString(w, completeResponse)
	}))
	defer srv.Close()
	m, err := Export(context.Background(), srv.Client(), srv.URL, []Query{{"first", "up"}, {"second", "up"}}, testStart, testStart.Add(2*time.Second), time.Second, dir)
	if err == nil || calls != 2 || m.Queries[1].Status != StatusSuccess {
		t.Fatalf("partial capture not continued: %+v %v", m, err)
	}
}

func TestAnalyzeRejectsTampering(t *testing.T) {
	for _, tc := range []string{"hash", "filename", "absolute", "symlink", "manifest symlink", "version", "duplicate", "bad hash", "pending", "malformed hashed body", "status error hashed body"} {
		t.Run(tc, func(t *testing.T) {
			dir, m, err := exportTest(t, completeResponse, 200)
			if err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "hash", "malformed hashed body", "status error hashed body":
				body := []byte("broken")
				if tc == "status error hashed body" {
					body = []byte(`{"status":"error","error":"bad"}`)
				}
				if err := os.WriteFile(filepath.Join(dir, "up.json"), body, 0600); err != nil {
					t.Fatal(err)
				}
				if tc != "hash" {
					h := sha256.Sum256(body)
					m.Queries[0].SHA256 = hex.EncodeToString(h[:])
				}
			case "filename":
				m.Queries[0].File = "../up.json"
			case "absolute":
				m.Queries[0].File = filepath.Join(dir, "up.json")
			case "symlink", "manifest symlink":
				name := "up.json"
				if tc == "manifest symlink" {
					name = "manifest.json"
				}
				path := filepath.Join(dir, name)
				outside := filepath.Join(t.TempDir(), "outside.json")
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "version":
				m.Version++
			case "duplicate":
				m.Queries = append(m.Queries, m.Queries[0])
			case "bad hash":
				m.Queries[0].SHA256 = "xyz"
			case "pending":
				m.Queries[0].Status = StatusPending
			}
			if tc != "manifest symlink" {
				if err := writeManifest(dir, m); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Analyze(dir); err == nil {
				t.Fatal("tampered capture accepted")
			} else if tc == "hash" && !strings.Contains(err.Error(), "hash mismatch") {
				t.Fatalf("hash was not verified before parsing: %v", err)
			}
		})
	}
}

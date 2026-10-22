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

package scrape

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckTargets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		targets   []Target
		gaps      []Gap
		wantError bool
	}{
		{"healthy", []Target{{ScrapePool: "expected", Health: "up", Labels: map[string]string{"job": "rewritten"}}}, nil, false},
		{"missing", nil, nil, true},
		{"wrong pool with right job label", []Target{{ScrapePool: "other", Health: "up", Labels: map[string]string{"job": "expected"}}}, nil, true},
		{"one replica down", []Target{{ScrapePool: "expected", Health: "up"}, {ScrapePool: "expected", Health: "down", LastError: "x509"}}, nil, true},
		{"up with error", []Target{{ScrapePool: "expected", Health: "up", LastError: "timeout"}}, nil, true},
		{"generation blocker", []Target{{ScrapePool: "expected", Health: "up"}}, []Gap{{Source: "monitor", Message: "unsupported auth"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/prefix/api/v1/targets" || r.URL.Query().Get("state") != "active" {
					t.Errorf("wrong request: %s", r.URL)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"activeTargets": tc.targets}})
			}))
			defer server.Close()
			b := Bundle{ExpectedJobs: []string{"expected"}, Gaps: tc.gaps}
			report, err := b.CheckTargets(t.Context(), server.URL+"/prefix")
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected result: %v %+v", err, report)
			}
			if tc.wantError && len(report.Gaps) == 0 {
				t.Fatal("failure without diagnostic gaps")
			}
		})
	}
}

func TestCheckTargetsAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"HTTP error", http.StatusForbidden, "forbidden"},
		{"malformed", http.StatusOK, "not json"},
		{"API error", http.StatusOK, `{"status":"error","error":"failed"}`},
		{"missing data", http.StatusOK, `{"status":"success"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.code); _, _ = w.Write([]byte(tc.body)) }))
			defer server.Close()
			if _, err := (Bundle{ExpectedJobs: []string{"expected"}}).CheckTargets(t.Context(), server.URL); err == nil {
				t.Fatal("expected API check failure")
			}
		})
	}
}

type authenticatedTransport struct{ base http.RoundTripper }

func (a authenticatedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer test-token")
	return a.base.RoundTrip(r)
}

func TestCheckTargetsWithClient(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("caller transport was not used")
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"activeTargets":[{"scrapePool":"expected","health":"up"}]}}`))
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = authenticatedTransport{base: client.Transport}
	b := Bundle{ExpectedJobs: []string{"expected"}}
	if _, err := b.CheckTargetsWithClient(t.Context(), client, server.URL); err != nil {
		t.Fatal(err)
	}
	if client.CheckRedirect != nil {
		t.Fatal("caller client was mutated")
	}
	if _, err := b.CheckTargetsWithClient(t.Context(), nil, server.URL); err == nil {
		t.Fatal("nil client accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := b.CheckTargetsWithClient(ctx, client, server.URL); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
}

func TestCheckTargetsWithClientRejectsRedirects(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		t.Error("caller redirect callback must not run")
		return nil
	}
	b := Bundle{ExpectedJobs: []string{"expected"}}
	if _, err := b.CheckTargetsWithClient(t.Context(), client, server.URL); err == nil || !strings.Contains(err.Error(), "302") || calls != 1 {
		t.Fatalf("redirect followed: %v calls=%d", err, calls)
	}
	if client.CheckRedirect == nil {
		t.Fatal("caller redirect policy mutated")
	}
}

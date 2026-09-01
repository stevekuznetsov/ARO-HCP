// Copyright 2025 Microsoft Corporation
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

// Package grafana provides a minimal client for Azure Managed Grafana that runs
// Prometheus instant queries through Grafana's unified query API. Each ARO-HCP
// cluster is exposed as a separate Prometheus (Azure Monitor Workspace)
// datasource inside a single Grafana instance.
//
// This is a lightly adapted copy of tooling/rightsize-requests/internal/grafana;
// the internal/ package cannot be imported across Go module boundaries.
package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// managedGrafanaScope is the well-known Azure Managed Grafana service
// application ID. A token for "<appID>/.default" authenticates to the Grafana
// HTTP API.
const managedGrafanaScope = "ce34e7e5-485f-4d76-964f-b3d2b16d1e4f/.default"

// Datasource is a subset of Grafana's datasource representation.
type Datasource struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Sample is one series returned by an instant query: its label set and value.
type Sample struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

// Series is one time series returned by a range query.
type Series struct {
	Labels map[string]string
	Times  []float64 // unix seconds
	Values []float64
}

// ArgMax returns the (time, value) of the series' maximum, or (0,0) if empty.
func (s Series) ArgMax() (t float64, v float64) {
	for i := range s.Values {
		if i == 0 || s.Values[i] > v {
			v = s.Values[i]
			t = s.Times[i]
		}
	}
	return t, v
}

// Client talks to a single Grafana instance.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a Client for the Grafana instance at grafanaURL, authenticating
// every request with a bearer token from cred scoped to Managed Grafana.
func New(grafanaURL string, cred azcore.TokenCredential) (*Client, error) {
	if grafanaURL == "" {
		return nil, fmt.Errorf("grafana URL is required")
	}
	if !strings.HasPrefix(grafanaURL, "http") {
		grafanaURL = "https://" + grafanaURL
	}
	return &Client{
		baseURL: strings.TrimRight(grafanaURL, "/"),
		http: &http.Client{
			Timeout:   180 * time.Second,
			Transport: &tokenTransport{cred: cred, next: http.DefaultTransport},
		},
	}, nil
}

// BaseURL returns the normalized Grafana base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// tokenTransport injects a fresh (azidentity caches internally) bearer token.
type tokenTransport struct {
	cred azcore.TokenCredential
	next http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.cred.GetToken(req.Context(), policy.TokenRequestOptions{
		Scopes: []string{managedGrafanaScope},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to acquire Grafana token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	return t.next.RoundTrip(req)
}

// ListDatasources returns all datasources configured in Grafana.
func (c *Client) ListDatasources(ctx context.Context) ([]Datasource, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/datasources", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list datasources: unexpected status %s", resp.Status)
	}
	var out []Datasource
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode datasources: %w", err)
	}
	return out, nil
}

// dsQueryRequest is the body for Grafana's unified /api/ds/query endpoint.
type dsQueryRequest struct {
	Queries []dsQuery `json:"queries"`
	From    string    `json:"from"`
	To      string    `json:"to"`
}

type dsQuery struct {
	RefID      string `json:"refId"`
	Datasource dsRef  `json:"datasource"`
	Expr       string `json:"expr"`
	Instant    bool   `json:"instant"`
	Range      bool   `json:"range,omitempty"`
	MaxDataPts int    `json:"maxDataPoints,omitempty"`
	IntervalMs int    `json:"intervalMs,omitempty"`
}

type dsRef struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

// dsQueryResponse models the dataframe response from /api/ds/query.
type dsQueryResponse struct {
	Results map[string]dsResult `json:"results"`
}

type dsResult struct {
	Status int    `json:"status"`
	Error  string `json:"error"`
	Frames []struct {
		Schema struct {
			Fields []struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"fields"`
		} `json:"schema"`
		Data struct {
			Values [][]any `json:"values"`
		} `json:"data"`
	} `json:"frames"`
}

// execute POSTs the query to /api/ds/query, retrying transient failures (rate
// limits, 5xx, dropped connections) with backoff. Cardinality errors ("query
// cost", "timeseries per metric limit") are returned immediately so the caller
// can split the query by namespace instead.
func (c *Client) execute(ctx context.Context, dsUID string, body dsQueryRequest) (dsResult, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return dsResult{}, err
	}
	var lastErr error
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff capped at 30s: 1,2,4,8,16,30,30...
			secs := 1 << uint(attempt-1)
			if secs > 30 {
				secs = 30
			}
			select {
			case <-ctx.Done():
				return dsResult{}, ctx.Err()
			case <-time.After(time.Duration(secs) * time.Second):
			}
		}
		res, err := c.executeOnce(ctx, dsUID, buf)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if ctx.Err() != nil || !isTransient(err) {
			return dsResult{}, err
		}
	}
	return dsResult{}, lastErr
}

func (c *Client) executeOnce(ctx context.Context, dsUID string, buf []byte) (dsResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/ds/query", bytes.NewReader(buf))
	if err != nil {
		return dsResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return dsResult{}, err // transport error (transient)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return dsResult{}, fmt.Errorf("query datasource %s: status %s: %s", dsUID, resp.Status, strings.TrimSpace(string(snippet)))
	}
	var dr dsQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return dsResult{}, fmt.Errorf("decode query response: %w", err)
	}
	res, ok := dr.Results["A"]
	if !ok {
		return dsResult{}, fmt.Errorf("query datasource %s: no result frame", dsUID)
	}
	if res.Status != 0 && res.Status != http.StatusOK {
		return dsResult{}, fmt.Errorf("query datasource %s: backend status %d: %s", dsUID, res.Status, res.Error)
	}
	return res, nil
}

// isTransient reports whether an error is worth retrying (rate limits, backend
// unavailability, dropped connections) versus a permanent cardinality/cost error
// that should instead be split into smaller queries.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// Cardinality/cost errors: do NOT retry (the caller halves the query).
	if strings.Contains(s, "timeseries per metric") || strings.Contains(s, "Query cost") ||
		strings.Contains(s, "exceeds max limit") {
		return false
	}
	if strings.Contains(s, "no such host") {
		return false
	}
	// Rate limits, backend unavailability, transport hiccups: retry.
	for _, m := range []string{
		"Too Many Requests", "429", "became unavailable", "connectionUnavailable",
		"status 500", "status 502", "status 503", "status 504",
		"connection reset", "EOF", "timeout", "deadline exceeded", "broken pipe",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// InstantQuery runs a PromQL instant query against the datasource identified by
// dsUID via Grafana's unified query API (/api/ds/query). This route drives the
// datasource's server-side Azure MSI authentication, which the legacy
// /api/datasources/proxy route does not do for Azure Monitor Workspace
// (Managed Prometheus) datasources.
func (c *Client) InstantQuery(ctx context.Context, dsUID, query string) ([]Sample, error) {
	body := dsQueryRequest{
		Queries: []dsQuery{{
			RefID:      "A",
			Datasource: dsRef{Type: "prometheus", UID: dsUID},
			Expr:       query,
			Instant:    true,
			MaxDataPts: 1,
		}},
		From: "now-1h",
		To:   "now",
	}
	res, err := c.execute(ctx, dsUID, body)
	if err != nil {
		return nil, err
	}

	var samples []Sample
	for _, fr := range res.Frames {
		// Find the value field: the last field, which carries the series labels.
		if len(fr.Schema.Fields) == 0 || len(fr.Data.Values) == 0 {
			continue
		}
		valueIdx := len(fr.Schema.Fields) - 1
		labels := fr.Schema.Fields[valueIdx].Labels
		col := fr.Data.Values[valueIdx]
		if len(col) == 0 {
			continue
		}
		v, ok := toFloat(col[len(col)-1])
		if !ok {
			continue
		}
		samples = append(samples, Sample{Labels: labels, Value: v})
	}
	return samples, nil
}

// RangeQuery runs a PromQL range query over [from,to] (e.g. "now-14d","now") at
// stepSeconds resolution, returning one Series per result series.
func (c *Client) RangeQuery(ctx context.Context, dsUID, query, from, to string, stepSeconds int) ([]Series, error) {
	if stepSeconds <= 0 {
		stepSeconds = 600
	}
	body := dsQueryRequest{
		Queries: []dsQuery{{
			RefID:      "A",
			Datasource: dsRef{Type: "prometheus", UID: dsUID},
			Expr:       query,
			Range:      true,
			IntervalMs: stepSeconds * 1000,
		}},
		From: from,
		To:   to,
	}
	res, err := c.execute(ctx, dsUID, body)
	if err != nil {
		return nil, err
	}

	var out []Series
	for _, fr := range res.Frames {
		if len(fr.Schema.Fields) < 2 || len(fr.Data.Values) < 2 {
			continue
		}
		// The time field is named "Time"/"time"; default to field 0.
		timeIdx := 0
		for i, f := range fr.Schema.Fields {
			if strings.EqualFold(f.Name, "time") {
				timeIdx = i
				break
			}
		}
		timeCol := fr.Data.Values[timeIdx]
		times := make([]float64, 0, len(timeCol))
		for _, tv := range timeCol {
			ms, _ := toFloat(tv)
			times = append(times, ms/1000.0) // ms -> seconds
		}
		for j := range fr.Schema.Fields {
			if j == timeIdx {
				continue
			}
			col := fr.Data.Values[j]
			vals := make([]float64, len(col))
			for i, cv := range col {
				vals[i], _ = toFloat(cv)
			}
			out = append(out, Series{Labels: fr.Schema.Fields[j].Labels, Times: times, Values: vals})
		}
	}
	return out, nil
}

// toFloat coerces a JSON-decoded numeric/string value to float64.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Target struct {
	ScrapePool       string            `json:"scrapePool"`
	ScrapeURL        string            `json:"scrapeUrl"`
	Health           string            `json:"health"`
	LastError        string            `json:"lastError"`
	Labels           map[string]string `json:"labels"`
	DiscoveredLabels map[string]string `json:"discoveredLabels"`
}

type TargetReport struct {
	Targets []Target `json:"targets"`
	Gaps    []Gap    `json:"gaps"`
}

// CheckTargets uses a default HTTP client with a 30-second timeout.
func (b Bundle) CheckTargets(ctx context.Context, endpoint string) (TargetReport, error) {
	return b.CheckTargetsWithClient(ctx, &http.Client{Timeout: 30 * time.Second}, endpoint)
}

// CheckTargetsWithClient checks the current active targets using the caller's
// transport (including authentication), timeout and cookie jar. It copies the
// client and disables redirects; it never mutates the supplied client.
// Every expected scrape pool needs at least one up target, and every active
// target in those pools must be healthy. ScrapePool, not the relabelable job
// label, identifies each job. Missing replicas and dropped targets cannot be
// inferred from this API snapshot. Generation gaps are carried forward.
func (b Bundle) CheckTargetsWithClient(ctx context.Context, client *http.Client, endpoint string) (TargetReport, error) {
	report := TargetReport{Targets: []Target{}, Gaps: append([]Gap{}, b.Gaps...)}
	if len(b.ExpectedJobs) == 0 {
		return report, fmt.Errorf("no expected jobs; generate a bundle first")
	}
	if client == nil {
		return report, fmt.Errorf("HTTP client is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return report, fmt.Errorf("endpoint must be an HTTP(S) Prometheus base URL without credentials, query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/targets"
	u.RawPath = ""
	u.RawQuery = "state=active"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return report, err
	}
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := copyClient.Do(req)
	if err != nil {
		return report, fmt.Errorf("get targets: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return report, fmt.Errorf("get targets: HTTP %d", resp.StatusCode)
	}
	var response struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ActiveTargets []Target `json:"activeTargets"`
		} `json:"data"`
	}
	const maxResponse = 32 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return report, fmt.Errorf("read targets: %w", err)
	}
	if len(data) > maxResponse {
		return report, fmt.Errorf("targets response exceeds %d bytes", maxResponse)
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return report, fmt.Errorf("decode targets: %w", err)
	}
	if response.Status != "success" {
		return report, fmt.Errorf("targets API status %q: %s", response.Status, response.Error)
	}
	up := map[string]int{}
	for _, name := range b.ExpectedJobs {
		up[name] = 0
	}
	for _, target := range response.Data.ActiveTargets {
		if _, expected := up[target.ScrapePool]; !expected {
			continue
		}
		report.Targets = append(report.Targets, target)
		if target.Health != "up" || target.LastError != "" {
			report.Gaps = append(report.Gaps, Gap{Source: target.ScrapePool, Message: fmt.Sprintf("target %s health=%s lastError=%s", target.ScrapeURL, target.Health, target.LastError)})
		} else {
			up[target.ScrapePool]++
		}
	}
	for _, name := range b.ExpectedJobs {
		if up[name] == 0 {
			report.Gaps = append(report.Gaps, Gap{Source: name, Message: "no active up targets in expected scrape pool"})
		}
	}
	if len(report.Gaps) > 0 {
		return report, fmt.Errorf("target check has %d blockers", len(report.Gaps))
	}
	return report, nil
}

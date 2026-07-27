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

// ci-alert-check is a throwaway CLI that finds PR CI e2e jobs which:
//   - failed overall
//   - had zero individual e2e test failures
//
// For qualifying runs it fetches the config (cluster names) and outputs
// a Kusto `| where cluster in (...)` clause plus time range for alert queries.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"

	"github.com/Azure/ARO-HCP/tooling/hcpctl/pkg/snapshot"
)

const (
	jobHistoryURL = "https://prow.ci.openshift.org/job-history/gs/test-platform-results/pr-logs/directory/pull-ci-Azure-ARO-HCP-main-e2e-parallel"
)

// prowBuild mirrors the JSON structure embedded in the Prow job-history page.
type prowBuild struct {
	SpyglassLink string `json:"SpyglassLink"`
	ID           string `json:"ID"`
	Started      string `json:"Started"`
	Duration     int64  `json:"Duration"` // nanoseconds
	Result       string `json:"Result"`
	Refs         struct {
		Org   string `json:"org"`
		Repo  string `json:"repo"`
		Pulls []struct {
			Number int    `json:"number"`
			Author string `json:"author"`
			SHA    string `json:"sha"`
			Title  string `json:"title"`
		} `json:"pulls"`
	} `json:"Refs"`
}

var allBuildsRe = regexp.MustCompile(`var allBuilds = (\[.*?\]);\s*\n`)

func fetchPage(ctx context.Context, buildID string) ([]prowBuild, string, error) {
	u := jobHistoryURL
	if buildID != "" {
		u += "?buildId=" + buildID
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	// Extract allBuilds JSON from script tag
	matches := allBuildsRe.FindSubmatch(body)
	if matches == nil {
		return nil, "", fmt.Errorf("could not find allBuilds in page response")
	}

	var builds []prowBuild
	if err := json.Unmarshal(matches[1], &builds); err != nil {
		return nil, "", fmt.Errorf("failed to parse allBuilds JSON: %w", err)
	}

	// Extract "Older Runs" pagination link buildId
	olderRe := regexp.MustCompile(`buildId=(\d+)">&lt;- Older Runs`)
	olderMatch := olderRe.FindSubmatch(body)
	var nextBuildID string
	if olderMatch != nil {
		nextBuildID = string(olderMatch[1])
	}

	return builds, nextBuildID, nil
}

// spyglassToGCSPrefix converts a SpyglassLink like
// /view/gs/test-platform-results/pr-logs/pull/Azure_ARO-HCP/5752/pull-ci-Azure-ARO-HCP-main-e2e-parallel/2081866440901660672
// into the GCS prefix: pr-logs/pull/Azure_ARO-HCP/5752/pull-ci-Azure-ARO-HCP-main-e2e-parallel/2081866440901660672
func spyglassToGCSPrefix(link string) string {
	const prefix = "/view/gs/test-platform-results/"
	if strings.HasPrefix(link, prefix) {
		return link[len(prefix):]
	}
	return link
}

type qualifyingRun struct {
	Build             prowBuild
	ServiceCluster    string
	ManagementCluster string
	StartTime         time.Time
	EndTime           time.Time
}

func main() {
	since := flag.Duration("since", 14*24*time.Hour, "How far back to look (default 2 weeks)")
	verbose := flag.Bool("v", false, "Verbose logging")
	flag.Parse()

	var logger logr.Logger
	if *verbose {
		stdr.SetVerbosity(2)
		logger = stdr.New(nil)
	} else {
		logger = stdr.New(nil)
		stdr.SetVerbosity(0)
	}
	ctx := logr.NewContext(context.Background(), logger)

	cutoff := time.Now().Add(-*since)

	fmt.Fprintf(os.Stderr, "Scanning e2e-parallel job history since %s...\n", cutoff.Format(time.RFC3339))

	// Phase 1: Paginate all job history to find FAILURE candidates
	var candidates []prowBuild
	var nextBuildID string
	totalScanned := 0

	for page := 0; ; page++ {
		builds, next, err := fetchPage(ctx, nextBuildID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR fetching page %d: %v\n", page, err)
			break
		}

		if len(builds) == 0 {
			break
		}

		reachedCutoff := false
		for _, b := range builds {
			totalScanned++

			started, err := time.Parse(time.RFC3339, b.Started)
			if err != nil {
				continue
			}

			if started.Before(cutoff) {
				reachedCutoff = true
				break
			}

			if b.Result == "FAILURE" {
				candidates = append(candidates, b)
			}
		}

		fmt.Fprintf(os.Stderr, "  page %d: %d builds scanned, %d failure candidates so far\n", page, totalScanned, len(candidates))

		if reachedCutoff || next == "" {
			break
		}
		nextBuildID = next
	}

	fmt.Fprintf(os.Stderr, "Pagination complete: %d total runs, %d failures to check\n\n", totalScanned, len(candidates))

	if len(candidates) == 0 {
		fmt.Fprintf(os.Stderr, "No failures found in time range.\n")
		return
	}

	// Phase 2: Check each candidate concurrently with a worker pool
	workers := runtime.NumCPU() * 2
	type result struct {
		idx int
		run *qualifyingRun // nil if not qualifying
	}

	jobs := make(chan int, len(candidates))
	results := make(chan result, len(candidates))

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				b := candidates[idx]
				started, _ := time.Parse(time.RFC3339, b.Started)

				gcsPrefix := spyglassToGCSPrefix(b.SpyglassLink)
				info := &snapshot.ProwJobInfo{
					URL:       fmt.Sprintf("https://prow.ci.openshift.org%s", b.SpyglassLink),
					JobName:   "pull-ci-Azure-ARO-HCP-main-e2e-parallel",
					ProwID:    b.ID,
					GCSPrefix: gcsPrefix,
				}

				// Fetch test results
				testResults, err := snapshot.FetchProwJobTestResults(ctx, info)
				if err != nil {
					// Job failed before tests ran — still a candidate
					testResults = nil
				}

				// Check if any test actually failed
				hasFailedTest := false
				for _, t := range testResults {
					if t.Failed {
						hasFailedTest = true
						break
					}
				}

				if hasFailedTest {
					results <- result{idx: idx, run: nil}
					continue
				}

				// Fetch config for cluster names
				run := &qualifyingRun{
					Build:     b,
					StartTime: started,
					EndTime:   started.Add(time.Duration(b.Duration)),
				}

				config, err := snapshot.FetchProwJobConfig(ctx, info, "")
				if err == nil {
					run.ServiceCluster = config.ServiceClusterName
					run.ManagementCluster = config.ManagementClusterName
				}

				results <- result{idx: idx, run: run}
			}
		}()
	}

	for i := range candidates {
		jobs <- i
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	qualifying := make([]*qualifyingRun, 0)
	checked := 0
	for r := range results {
		checked++
		if r.run != nil {
			qualifying = append(qualifying, r.run)
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s PR#%d (%s) -> QUALIFYING (svc=%s mgmt=%s)\n",
				checked, len(candidates), r.run.Build.ID,
				prNumber(r.run.Build), prAuthor(r.run.Build),
				r.run.ServiceCluster, r.run.ManagementCluster)
		} else {
			b := candidates[r.idx]
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s PR#%d (%s) -> has failed tests, skipping\n",
				checked, len(candidates), b.ID, prNumber(b), prAuthor(b))
		}
	}

	fmt.Fprintf(os.Stderr, "\n--- Summary ---\n")
	fmt.Fprintf(os.Stderr, "Total runs scanned: %d\n", totalScanned)
	fmt.Fprintf(os.Stderr, "Failures checked: %d\n", len(candidates))
	fmt.Fprintf(os.Stderr, "Qualifying (failed job, no test failures): %d\n", len(qualifying))

	if len(qualifying) == 0 {
		fmt.Fprintf(os.Stderr, "No qualifying runs found.\n")
		return
	}

	// Print qualifying runs
	fmt.Fprintf(os.Stderr, "\nQualifying runs:\n")
	for _, q := range qualifying {
		fmt.Fprintf(os.Stderr, "  %s  PR#%d (%s) by %s  [%s -> %s]  svc=%s mgmt=%s\n",
			q.Build.ID,
			prNumber(q.Build), prTitle(q.Build), prAuthor(q.Build),
			q.StartTime.Format("01-02 15:04"), q.EndTime.Format("01-02 15:04"),
			q.ServiceCluster, q.ManagementCluster,
		)
	}

	// Collect unique cluster names and time range
	clusters := map[string]bool{}
	var earliest, latest time.Time
	for _, q := range qualifying {
		if q.ServiceCluster != "" {
			clusters[q.ServiceCluster] = true
		}
		if q.ManagementCluster != "" {
			clusters[q.ManagementCluster] = true
		}
		if earliest.IsZero() || q.StartTime.Before(earliest) {
			earliest = q.StartTime
		}
		if q.EndTime.After(latest) {
			latest = q.EndTime
		}
	}

	// Output KQL clause
	fmt.Println()
	fmt.Println("// Kusto query fragment for alert investigation:")
	fmt.Println("// Covers", len(qualifying), "failed e2e jobs with no individual test failures")
	fmt.Printf("// Time range: %s to %s\n", earliest.Format(time.RFC3339), latest.Format(time.RFC3339))
	fmt.Println()

	if len(clusters) > 0 {
		clusterList := make([]string, 0, len(clusters))
		for c := range clusters {
			clusterList = append(clusterList, fmt.Sprintf("'%s'", c))
		}
		fmt.Printf("| where firedDateTime > datetime(%s) and firedDateTime < datetime(%s)\n",
			earliest.Format("2006-01-02T15:04:05Z"), latest.Format("2006-01-02T15:04:05Z"))
		fmt.Printf("| where cluster in (%s)\n", strings.Join(clusterList, ", "))
	} else {
		fmt.Println("// WARNING: no cluster names found in configs")
		fmt.Printf("| where firedDateTime > datetime(%s) and firedDateTime < datetime(%s)\n",
			earliest.Format("2006-01-02T15:04:05Z"), latest.Format("2006-01-02T15:04:05Z"))
	}
}

func prNumber(b prowBuild) int {
	if len(b.Refs.Pulls) > 0 {
		return b.Refs.Pulls[0].Number
	}
	return 0
}

func prAuthor(b prowBuild) string {
	if len(b.Refs.Pulls) > 0 {
		return b.Refs.Pulls[0].Author
	}
	return "unknown"
}

func prTitle(b prowBuild) string {
	if len(b.Refs.Pulls) > 0 {
		return b.Refs.Pulls[0].Title
	}
	return ""
}

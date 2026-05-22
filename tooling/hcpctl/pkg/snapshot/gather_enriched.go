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

package snapshot

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// GatherForTestOptions configures a data gathering run for a single test case.
// This is the library-level entry point used by both the CLI gather subcommand
// and downstream consumers (e.g. sdp-pipelines analysis controller).
type GatherForTestOptions struct {
	// ProwJobURL is the full URL to the Prow job (used to locate GCS artifacts).
	ProwJobURL string

	// TestName is the name of the failed test to gather data for.
	TestName string

	// OutputDir is the directory where the structured data should be written.
	OutputDir string

	// QueryTimeout is the timeout for individual Kusto queries.
	// A zero value defaults to 10 minutes.
	QueryTimeout time.Duration

	// Concurrency is the maximum number of concurrent Kusto queries.
	// A value of 0 defaults to 4 * runtime.NumCPU().
	Concurrency int
}

// GatherForTestResult contains metadata about what was gathered.
type GatherForTestResult struct {
	// ManifestPath is the path to the manifest.json file in the output directory.
	ManifestPath string

	// DataDir is the root of the structured data directory.
	DataDir string

	// Manifest is the parsed manifest from the snapshot library.
	Manifest *Manifest

	// VerificationReport contains pass/fail/skip results for each query.
	VerificationReport *VerificationReport

	// StartTime is the start of the time window for the test execution.
	StartTime time.Time

	// EndTime is the end of the time window for the test execution.
	EndTime time.Time

	// CleanupStartTime is the time at which the test's cleanup phase began.
	CleanupStartTime time.Time

	// TestSummaries contains metadata for all e2e tests (passing, failing,
	// skipped) from the same Prow job run.
	TestSummaries []TestSummary
}

// GatherForTest runs the full data gathering pipeline for a single test from
// a Prow job. It downloads Prow artifacts, queries Kusto for traces and state,
// writes test logs, sibling test metadata, and produces a structured data
// directory with a manifest.json index.
//
// The credential is used to authenticate to Azure Data Explorer; the specific
// cluster URI is determined at runtime from the Prow job config.
func GatherForTest(ctx context.Context, credential azcore.TokenCredential, opts GatherForTestOptions) (*GatherForTestResult, error) {
	// Parse the Prow URL to get structured job info.
	prowInfo, err := ParseProwURL(opts.ProwJobURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Prow URL %q: %w", opts.ProwJobURL, err)
	}

	// Fetch Prow job data from GCS (config and all test results).
	prowConfig, testResults, err := FetchProwJobData(ctx, prowInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Prow job data: %w", err)
	}

	// Find the specific test.
	var test *TestResult
	for i := range testResults {
		if testResults[i].Name == opts.TestName {
			test = &testResults[i]
			break
		}
	}
	if test == nil {
		return nil, fmt.Errorf("test %q not found in Prow job results (found %d tests)", opts.TestName, len(testResults))
	}

	// Write test logs.
	if err := WriteTestLogs(opts.OutputDir, test); err != nil {
		return nil, fmt.Errorf("failed to write test logs: %w", err)
	}

	// Build the Kusto endpoint.
	kustoEndpoint := fmt.Sprintf("https://%s.%s.kusto.windows.net", prowConfig.KustoName, prowConfig.Region)

	// Create a Kusto client.
	kcsb := azkustodata.NewConnectionStringBuilder(kustoEndpoint).
		WithTokenCredential(credential)
	kustoClient, err := azkustodata.New(kcsb)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kusto client for %q: %w", kustoEndpoint, err)
	}
	defer func() {
		if err := kustoClient.Close(); err != nil {
			slog.Warn("Failed to close Kusto client.", "error", err)
		}
	}()

	// Build the gather input with 5-minute padding.
	startTime := test.StartTime.Add(-5 * time.Minute)
	endTime := test.EndTime.Add(5 * time.Minute)

	queryTimeout := opts.QueryTimeout
	if queryTimeout == 0 {
		queryTimeout = 10 * time.Minute
	}

	input := GatherInput{
		ClusterURI:      kustoEndpoint,
		ServiceDatabase: prowConfig.ServiceDatabase,
		HCPDatabase:     prowConfig.HCPDatabase,
		ResourceGroup:   test.ResourceGroup,
		TimeWindow: TimeWindow{
			Start:           startTime,
			End:             endTime,
			SetupFinishTime: test.SetupFinishTime,
		},
		CleanupStartTime: test.CleanupStartTime,
		QueryTimeout:     queryTimeout,
		Concurrency:      opts.Concurrency,
		TestStartTime:    test.TestStartTime,
	}

	// Run the snapshot gatherer.
	gatherer := NewGatherer(kustoClient)
	manifest, report, err := gatherer.Gather(ctx, input, opts.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("snapshot gather failed: %w", err)
	}

	// Enrich manifest with test metadata.
	manifest.TestName = opts.TestName
	manifest.ProwJobURL = opts.ProwJobURL
	if err := WriteManifest(opts.OutputDir, manifest); err != nil {
		return nil, fmt.Errorf("failed to write manifest: %w", err)
	}

	// Write sibling test summaries.
	testSummaries := ConvertTestResults(testResults)
	if err := WriteSiblingTests(opts.OutputDir, testSummaries); err != nil {
		slog.Warn("Failed to write sibling_tests.json; continuing.", "error", err)
	}

	return &GatherForTestResult{
		ManifestPath:       fmt.Sprintf("%s/manifest.json", opts.OutputDir),
		DataDir:            opts.OutputDir,
		Manifest:           manifest,
		VerificationReport: report,
		StartTime:          test.StartTime,
		EndTime:            test.EndTime,
		CleanupStartTime:   test.CleanupStartTime,
		TestSummaries:      testSummaries,
	}, nil
}

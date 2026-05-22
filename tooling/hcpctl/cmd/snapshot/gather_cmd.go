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
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	snapshotpkg "github.com/Azure/ARO-HCP/tooling/hcpctl/pkg/snapshot"
)

// RawGatherOptions holds the unvalidated CLI options for the gather subcommand.
type RawGatherOptions struct {
	URL          string
	TestSelector string
	OutputDir    string
	QueryTimeout time.Duration
	Concurrency  int
}

func defaultGatherOptions() *RawGatherOptions {
	return &RawGatherOptions{
		QueryTimeout: 5 * time.Minute,
		OutputDir:    fmt.Sprintf("snapshot-%s", time.Now().Format("20060102-150405")),
	}
}

func bindGatherOptions(opts *RawGatherOptions, cmd *cobra.Command) error {
	cmd.Flags().StringVar(&opts.URL, "url", opts.URL, "Prow job URL (required)")
	cmd.Flags().StringVar(&opts.TestSelector, "test", opts.TestSelector, "Only gather data for the test whose name contains this substring (required)")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "Directory to write snapshot output")
	cmd.Flags().DurationVar(&opts.QueryTimeout, "query-timeout", opts.QueryTimeout, "Timeout for individual Kusto queries")
	cmd.Flags().IntVar(&opts.Concurrency, "concurrency", opts.Concurrency, "Maximum number of concurrent Kusto queries (0 = 4*NumCPU)")

	for _, flag := range []string{"url", "test"} {
		if err := cmd.MarkFlagRequired(flag); err != nil {
			return fmt.Errorf("failed to mark %s as required: %w", flag, err)
		}
	}
	return nil
}

type validatedGatherOptions struct {
	prowURL      string
	testSelector string
	outputDir    string
	queryTimeout time.Duration
	concurrency  int
}

func (o *RawGatherOptions) validate() (*validatedGatherOptions, error) {
	// Validate the Prow URL early.
	if _, err := snapshotpkg.ParseProwURL(o.URL); err != nil {
		return nil, fmt.Errorf("invalid --url: %w", err)
	}
	return &validatedGatherOptions{
		prowURL:      o.URL,
		testSelector: o.TestSelector,
		outputDir:    o.OutputDir,
		queryTimeout: o.QueryTimeout,
		concurrency:  o.Concurrency,
	}, nil
}

func (o *validatedGatherOptions) run(ctx context.Context) error {
	logger := logr.FromContextOrDiscard(ctx)

	// Resolve test name from substring selector by fetching Prow data.
	prowInfo, err := snapshotpkg.ParseProwURL(o.prowURL)
	if err != nil {
		return fmt.Errorf("failed to parse Prow URL: %w", err)
	}

	logger.Info("Fetching Prow job data to resolve test name",
		"job", prowInfo.JobName,
		"prowID", prowInfo.ProwID,
	)

	_, allTests, err := snapshotpkg.FetchProwJobData(ctx, prowInfo)
	if err != nil {
		return fmt.Errorf("failed to fetch Prow job data: %w", err)
	}

	// Find the target test by substring match.
	var matchedTestName string
	for _, t := range allTests {
		if strings.Contains(t.Name, o.testSelector) {
			matchedTestName = t.Name
			break
		}
	}
	if matchedTestName == "" {
		return fmt.Errorf("no test matches selector %q (found %d tests total)", o.testSelector, len(allTests))
	}

	logger.Info("Resolved test name", "selector", o.testSelector, "test", matchedTestName)

	// Build the output directory.
	sanitizedName := snapshotpkg.SanitizeTestName(matchedTestName)
	testOutputDir := filepath.Join(o.outputDir, prowInfo.JobName, prowInfo.ProwID, sanitizedName)

	// Create credential.
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return fmt.Errorf("failed to create Azure credential: %w", err)
	}

	// Delegate to the library function, which exercises the same code path
	// that downstream sdp-pipelines will use.
	result, err := snapshotpkg.GatherForTest(ctx, cred, snapshotpkg.GatherForTestOptions{
		ProwJobURL:   o.prowURL,
		TestName:     matchedTestName,
		OutputDir:    testOutputDir,
		QueryTimeout: o.queryTimeout,
		Concurrency:  o.concurrency,
	})
	if err != nil {
		return err
	}

	logger.Info("Gather complete",
		"test", matchedTestName,
		"outputDir", result.DataDir,
		"phases", len(result.Manifest.Phases),
	)

	return nil
}

func newGatherCommand() (*cobra.Command, error) {
	opts := defaultGatherOptions()
	cmd := &cobra.Command{
		Use:   "gather",
		Short: "Gather an enriched diagnostic snapshot for a single test",
		Long: `Gather a structured diagnostic snapshot for a single test from a Prow job,
including test logs, sibling test metadata, and Kusto query results.

This is the enriched version of from-prow-job: it targets a single test and
writes additional metadata (test/error.log, test/output.log, sibling_tests.json)
that the analyze subcommand requires.`,
		Example: `  # Gather enriched snapshot for a specific test
  hcpctl snapshot gather \
    --url https://prow.ci.openshift.org/view/gs/test-platform-results/logs/periodic-ci-Azure-ARO-HCP-main-aro-hcp-e2e-parallel/1234567890 \
    --test TestNodePoolCreation`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			validated, err := opts.validate()
			if err != nil {
				return err
			}
			return validated.run(cmd.Context())
		},
	}
	if err := bindGatherOptions(opts, cmd); err != nil {
		return nil, err
	}
	return cmd, nil
}

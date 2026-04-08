package failingtests

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"github.com/Azure/ARO-HCP/tooling/triage/clients/sippy"
	"github.com/Azure/ARO-HCP/tooling/triage/pkg/artifacts"
)

func DefaultFailingTestsOptions() *RawFailingTestsOptions {
	return &RawFailingTestsOptions{
		Since: "168h",
	}
}

func BindFailingTestsOptions(opts *RawFailingTestsOptions, cmd *cobra.Command) error {
	cmd.Flags().StringVar(&opts.Environment, "environment", opts.Environment, fmt.Sprintf("Sippy environment to query. Must be one of: %s", strings.Join(sippy.AllEnvironments, ", ")))
	cmd.Flags().StringVar(&opts.Since, "since", opts.Since, "How far back to look for failing tests, as a Go duration (e.g. 168h, 720h).")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "Directory to write per-job-run artifacts into.")
	return nil
}

// RawFailingTestsOptions holds input values as provided by CLI flags.
type RawFailingTestsOptions struct {
	Environment string
	Since       string
	OutputDir   string
}

// validatedFailingTestsOptions is a private wrapper that enforces a call of Validate() before Complete() can be invoked.
type validatedFailingTestsOptions struct {
	Environment string
	Duration    time.Duration
	OutputDir   string
}

type ValidatedFailingTestsOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*validatedFailingTestsOptions
}

// completedFailingTestsOptions is a private wrapper that enforces a call of Complete() before Run() can be invoked.
type completedFailingTestsOptions struct {
	sippyClient sippy.Client
	gcsClient   *storage.Client
	args        sippy.ListJobRunsArgs
	outputDir   string
}

type FailingTestsOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedFailingTestsOptions
}

func (o *RawFailingTestsOptions) Validate() (*ValidatedFailingTestsOptions, error) {
	if o.Environment == "" {
		return nil, fmt.Errorf("--environment is required")
	}
	validEnvironments := make(map[string]struct{}, len(sippy.AllEnvironments))
	for _, env := range sippy.AllEnvironments {
		validEnvironments[env] = struct{}{}
	}
	if _, ok := validEnvironments[o.Environment]; !ok {
		return nil, fmt.Errorf("invalid environment %q, must be one of: %s", o.Environment, strings.Join(sippy.AllEnvironments, ", "))
	}

	duration, err := time.ParseDuration(o.Since)
	if err != nil {
		return nil, fmt.Errorf("invalid --since value %q: %w", o.Since, err)
	}
	if duration <= 0 {
		return nil, fmt.Errorf("--since must be a positive duration, got %q", o.Since)
	}
	maxLookback := time.Duration(sippy.MaxLookbackDays) * 24 * time.Hour
	if duration > maxLookback {
		return nil, fmt.Errorf("--since %q exceeds the maximum lookback of %d days", o.Since, sippy.MaxLookbackDays)
	}

	if o.OutputDir == "" {
		return nil, fmt.Errorf("--output-dir is required")
	}

	return &ValidatedFailingTestsOptions{
		validatedFailingTestsOptions: &validatedFailingTestsOptions{
			Environment: o.Environment,
			Duration:    duration,
			OutputDir:   o.OutputDir,
		},
	}, nil
}

func (o *ValidatedFailingTestsOptions) Complete(ctx context.Context) (*FailingTestsOptions, error) {
	cutoff := time.Now().Add(-o.Duration)
	args := sippy.ListJobRunsArgs{
		Release: o.Environment,
		Filter: sippy.Filter{
			Items: []sippy.FilterItem{
				{
					ColumnField:   "timestamp",
					OperatorValue: ">",
					Value:         fmt.Sprintf("%d", cutoff.UnixMilli()),
				},
				{
					ColumnField:   "job",
					OperatorValue: "contains",
					Value:         "e2e-parallel",
				},
			},
			LinkOperator: "and",
		},
	}

	gcsClient, err := artifacts.NewGCSClient(ctx)
	if err != nil {
		return nil, err
	}

	return &FailingTestsOptions{
		completedFailingTestsOptions: &completedFailingTestsOptions{
			sippyClient: sippy.NewClient(),
			gcsClient:   gcsClient,
			args:        args,
			outputDir:   o.OutputDir,
		},
	}, nil
}

func (o *FailingTestsOptions) Run(ctx context.Context) error {
	logger := logr.FromContextOrDiscard(ctx)
	logger.V(1).Info("Querying Sippy for failing tests", "release", o.args.Release)

	response, err := o.sippyClient.ListJobRuns(ctx, o.args)
	if err != nil {
		return fmt.Errorf("failed to list job runs: %w", err)
	}

	logger.V(1).Info("Received job runs", "total", response.TotalRows)

	totalJobs := len(response.Rows)
	// testFailureCount tracks how many job runs each test failed in
	testFailureCount := make(map[string]int)
	var failingRuns []sippy.JobRun

	for _, run := range response.Rows {
		if len(run.FailedTestNames) == 0 {
			continue
		}
		failingRuns = append(failingRuns, run)
		for _, testName := range run.FailedTestNames {
			testFailureCount[testName]++
		}
	}

	// Print job failure summary
	if totalJobs == 0 {
		fmt.Println("No job runs found.")
		return nil
	}
	failRate := float64(len(failingRuns)) / float64(totalJobs) * 100
	fmt.Printf("Failed jobs: %d/%d (%.1f%%)\n\n", len(failingRuns), totalJobs, failRate)

	if len(testFailureCount) == 0 {
		fmt.Println("No failing tests found.")
		return nil
	}

	// Sort tests by failure count descending, then alphabetically for ties
	type testCount struct {
		name  string
		count int
	}
	tests := make([]testCount, 0, len(testFailureCount))
	for name, count := range testFailureCount {
		tests = append(tests, testCount{name: name, count: count})
	}
	sort.Slice(tests, func(i, j int) bool {
		if tests[i].count != tests[j].count {
			return tests[i].count > tests[j].count
		}
		return tests[i].name < tests[j].name
	})

	// Determine the width needed for the count column
	maxCount := tests[0].count
	countWidth := len(fmt.Sprintf("%d", maxCount))

	fmt.Println("Failing tests by frequency:")
	for _, tc := range tests {
		fmt.Printf("  %*d  %s\n", countWidth, tc.count, tc.name)
	}

	// Download artifacts for each failing job run
	fmt.Printf("\nDownloading artifacts to %s ...\n", o.outputDir)
	if err := downloadArtifacts(ctx, o.gcsClient, o.outputDir, failingRuns); err != nil {
		return err
	}

	return nil
}

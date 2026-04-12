package jobfailures

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/spf13/cobra"

	"github.com/Azure/ARO-HCP/tooling/triage/pkg/artifacts"
)

func DefaultJobFailuresOptions() *RawJobFailuresOptions {
	return &RawJobFailuresOptions{}
}

func BindJobFailuresOptions(opts *RawJobFailuresOptions, cmd *cobra.Command) error {
	cmd.Flags().StringVar(&opts.URL, "url", opts.URL, "Prow job URL (e.g. https://prow.ci.openshift.org/view/gs/test-platform-results/logs/<job>/<prow-id> or https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/pull/<org_repo>/<pr>/<job>/<prow-id>).")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "Directory to write per-job-run artifacts into.")
	return nil
}

// RawJobFailuresOptions holds input values as provided by CLI flags.
type RawJobFailuresOptions struct {
	URL       string
	OutputDir string
}

// validatedJobFailuresOptions is a private wrapper that enforces a call of Validate() before Complete() can be invoked.
type validatedJobFailuresOptions struct {
	JobName   string
	ProwID    string
	GCSPrefix string
	IsPR      bool
	OutputDir string
}

type ValidatedJobFailuresOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*validatedJobFailuresOptions
}

// completedJobFailuresOptions is a private wrapper that enforces a call of Complete() before Run() can be invoked.
type completedJobFailuresOptions struct {
	gcsClient *storage.Client
	jobName   string
	prowID    string
	gcsPrefix string
	isPR      bool
	outputDir string
}

type JobFailuresOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedJobFailuresOptions
}

func (o *RawJobFailuresOptions) Validate() (*ValidatedJobFailuresOptions, error) {
	if o.URL == "" {
		return nil, fmt.Errorf("--url is required")
	}
	if o.OutputDir == "" {
		return nil, fmt.Errorf("--output-dir is required")
	}

	jobName, prowID, gcsPrefix, isPR, err := parseProwURL(o.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid --url %q: %w", o.URL, err)
	}

	return &ValidatedJobFailuresOptions{
		validatedJobFailuresOptions: &validatedJobFailuresOptions{
			JobName:   jobName,
			ProwID:    prowID,
			GCSPrefix: gcsPrefix,
			IsPR:      isPR,
			OutputDir: o.OutputDir,
		},
	}, nil
}

func (o *ValidatedJobFailuresOptions) Complete(ctx context.Context) (*JobFailuresOptions, error) {
	gcsClient, err := artifacts.NewGCSClient(ctx)
	if err != nil {
		return nil, err
	}

	return &JobFailuresOptions{
		completedJobFailuresOptions: &completedJobFailuresOptions{
			gcsClient: gcsClient,
			jobName:   o.JobName,
			prowID:    o.ProwID,
			gcsPrefix: o.GCSPrefix,
			isPR:      o.IsPR,
			outputDir: o.OutputDir,
		},
	}, nil
}

func (o *JobFailuresOptions) Run(ctx context.Context) error {
	fmt.Printf("Downloading artifacts for job %s (prow ID %s) to %s ...\n", o.jobName, o.prowID, o.outputDir)
	if err := artifacts.DownloadRunArtifacts(ctx, o.gcsClient, o.outputDir, o.jobName, o.prowID, o.gcsPrefix, o.isPR); err != nil {
		return err
	}
	fmt.Println("Done.")
	return nil
}

// parseProwURL extracts the job name, Prow job ID, GCS prefix, and whether the URL is for
// a pull request (presubmit) job from a Prow job URL. It supports two URL formats:
//
// Periodic/postsubmit jobs:
//
//	https://prow.ci.openshift.org/view/gs/test-platform-results/logs/<job-name>/<prow-id>
//
// Pull request (presubmit) jobs:
//
//	https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/pull/<org_repo>/<pr-number>/<job-name>/<prow-id>
//
// The returned gcsPrefix is the path within the GCS bucket (everything after the bucket name),
// e.g. "logs/<job>/<id>" or "pr-logs/pull/<org_repo>/<pr>/<job>/<id>".
func parseProwURL(rawURL string) (jobName, prowID, gcsPrefix string, isPR bool, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", false, fmt.Errorf("failed to parse URL: %w", err)
	}

	// Split path into non-empty segments
	var segments []string
	for _, s := range strings.Split(u.Path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}

	// Find the "logs" or "pr-logs" segment and extract the job name and Prow ID.
	for i, seg := range segments {
		if seg == "pr-logs" {
			// pr-logs/pull/<org_repo>/<pr-number>/<job-name>/<prow-id>
			if i+5 >= len(segments) {
				return "", "", "", false, fmt.Errorf("URL path must contain pr-logs/pull/<org_repo>/<pr-number>/<job-name>/<prow-id>, got %q", u.Path)
			}
			if segments[i+1] != "pull" {
				return "", "", "", false, fmt.Errorf("expected \"pull\" after \"pr-logs\" in URL path, got %q", segments[i+1])
			}
			jobName = segments[i+4]
			prowID = segments[i+5]

			if _, err := strconv.ParseUint(prowID, 10, 64); err != nil {
				return "", "", "", false, fmt.Errorf("Prow ID %q is not a valid number", prowID)
			}

			gcsPrefix = strings.Join(segments[i:i+6], "/")
			return jobName, prowID, gcsPrefix, true, nil
		}
		if seg == "logs" {
			// logs/<job-name>/<prow-id>
			if i+2 >= len(segments) {
				return "", "", "", false, fmt.Errorf("URL path must contain logs/<job-name>/<prow-id>, got %q", u.Path)
			}
			jobName = segments[i+1]
			prowID = segments[i+2]

			if _, err := strconv.ParseUint(prowID, 10, 64); err != nil {
				return "", "", "", false, fmt.Errorf("Prow ID %q is not a valid number", prowID)
			}

			gcsPrefix = strings.Join(segments[i:i+3], "/")
			return jobName, prowID, gcsPrefix, false, nil
		}
	}

	return "", "", "", false, fmt.Errorf("URL path does not contain a \"logs\" or \"pr-logs\" segment: %q", u.Path)
}

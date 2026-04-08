package failingtests

import (
	"context"
	"fmt"

	"cloud.google.com/go/storage"
	"github.com/go-logr/logr"

	"github.com/Azure/ARO-HCP/tooling/triage/clients/sippy"
	"github.com/Azure/ARO-HCP/tooling/triage/pkg/artifacts"
)

func downloadArtifacts(ctx context.Context, gcsClient *storage.Client, outputDir string, runs []sippy.JobRun) error {
	logger := logr.FromContextOrDiscard(ctx)

	for _, run := range runs {
		prowID := fmt.Sprintf("%d", run.ProwID)
		if err := artifacts.DownloadRunArtifacts(ctx, gcsClient, outputDir, run.Job, prowID); err != nil {
			logger.Error(err, "Failed to download artifacts, skipping", "job", run.Job, "prowID", prowID)
			continue
		}
	}
	return nil
}

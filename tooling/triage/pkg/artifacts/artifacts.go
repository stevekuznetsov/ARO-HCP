package artifacts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/go-logr/logr"
	"github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"gopkg.in/yaml.v3"
)

const (
	GCSBucket  = "test-platform-results"
	configPath = "aro-hcp-write-config/artifacts/config.yaml"
)

// sourceConfig represents the fields we read from the full config.yaml.
type sourceConfig struct {
	Region string      `yaml:"region"`
	Kusto  sourceKusto `yaml:"kusto"`
}

type sourceKusto struct {
	KustoName                      string `yaml:"kustoName"`
	HostedControlPlaneLogsDatabase string `yaml:"hostedControlPlaneLogsDatabase"`
	ServiceLogsDatabase            string `yaml:"serviceLogsDatabase"`
}

// filteredConfig represents the subset of fields we write out.
type filteredConfig struct {
	Region string        `yaml:"region"`
	Kusto  filteredKusto `yaml:"kusto"`
}

type filteredKusto struct {
	Name                           string `yaml:"name"`
	HostedControlPlaneLogsDatabase string `yaml:"hostedControlPlaneLogsDatabase"`
	ServiceLogsDatabase            string `yaml:"serviceLogsDatabase"`
}

// NewGCSClient creates an unauthenticated GCS client for accessing public buckets.
func NewGCSClient(ctx context.Context) (*storage.Client, error) {
	gcsClient, err := storage.NewClient(ctx, option.WithoutAuthentication())
	if err != nil {
		return nil, fmt.Errorf("failed to create GCS client: %w", err)
	}
	return gcsClient, nil
}

// DownloadRunArtifacts downloads test artifacts for a single Prow job run from GCS
// and writes per-test failure data (output.log, error.log, metadata.json) to outputDir.
// The gcsPrefix is the path within the GCS bucket up to the Prow ID, e.g.:
//   - "logs/<job>/<prow-id>" for periodic/postsubmit jobs
//   - "pr-logs/pull/<org_repo>/<pr>/<job>/<prow-id>" for presubmit (PR) jobs
func DownloadRunArtifacts(ctx context.Context, gcsClient *storage.Client, outputDir, jobName, prowID, gcsPrefix string) error {
	logger := logr.FromContextOrDiscard(ctx)

	artifactDir, err := findArtifactDir(ctx, gcsClient, jobName, gcsPrefix)
	if err != nil {
		return fmt.Errorf("failed to find artifact directory: %w", err)
	}

	logger.V(1).Info("Found artifact directory", "job", jobName, "prowID", prowID, "dir", artifactDir)

	runDir := filepath.Join(outputDir, jobName, prowID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	artifactPrefix := fmt.Sprintf("%s/artifacts/%s", gcsPrefix, artifactDir)

	// Download and write filtered config.yaml
	configGCSPath := fmt.Sprintf("%s/%s", artifactPrefix, configPath)
	data, err := downloadObject(ctx, gcsClient, configGCSPath)
	if err != nil {
		return fmt.Errorf("failed to download config.yaml: %w", err)
	}

	filtered, err := filterConfig(data)
	if err != nil {
		return fmt.Errorf("failed to filter config.yaml: %w", err)
	}

	if err := os.WriteFile(filepath.Join(runDir, "config.yaml"), filtered, 0o644); err != nil {
		return fmt.Errorf("failed to write config.yaml: %w", err)
	}

	logger.V(1).Info("Wrote filtered config", "path", filepath.Join(runDir, "config.yaml"))

	// Find and download all extension test result files
	testResultsPrefix := fmt.Sprintf("%s/aro-hcp-test-persistent/artifacts/extension_test_result_e2e_", artifactPrefix)
	testResultFiles, err := listObjects(ctx, gcsClient, testResultsPrefix)
	if err != nil {
		return fmt.Errorf("failed to list test result files: %w", err)
	}
	if len(testResultFiles) == 0 {
		return fmt.Errorf("no extension_test_result_e2e_*.json files found under %s", testResultsPrefix)
	}

	var allResults extensiontests.ExtensionTestResults
	for _, objPath := range testResultFiles {
		data, err := downloadObject(ctx, gcsClient, objPath)
		if err != nil {
			logger.Error(err, "Failed to download test result file, skipping", "path", objPath)
			continue
		}
		var results extensiontests.ExtensionTestResults
		if err := json.Unmarshal(data, &results); err != nil {
			logger.Error(err, "Failed to parse test result file, skipping", "path", objPath)
			continue
		}
		allResults = append(allResults, results...)
	}

	// Save aggregated test results as tests.json
	aggregated, err := json.MarshalIndent(allResults, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal aggregated test results: %w", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "tests.json"), append(aggregated, '\n'), 0o644); err != nil {
		return fmt.Errorf("failed to write tests.json: %w", err)
	}

	// Write per-test metadata for failed tests only
	var written int
	for _, result := range allResults {
		if result.Result != extensiontests.ResultFailed {
			continue
		}
		testDir := filepath.Join(runDir, SanitizeTestName(result.Name))
		if err := os.MkdirAll(testDir, 0o755); err != nil {
			return fmt.Errorf("failed to create test directory for %q: %w", result.Name, err)
		}

		// Write the output log separately
		if err := os.WriteFile(filepath.Join(testDir, "output.log"), []byte(result.Output), 0o644); err != nil {
			return fmt.Errorf("failed to write output.log for %q: %w", result.Name, err)
		}

		// Write the error log separately
		if err := os.WriteFile(filepath.Join(testDir, "error.log"), []byte(result.Error), 0o644); err != nil {
			return fmt.Errorf("failed to write error.log for %q: %w", result.Name, err)
		}

		// Write metadata without the output and error fields
		result.Output = ""
		result.Error = ""
		metadata, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal metadata for %q: %w", result.Name, err)
		}
		if err := os.WriteFile(filepath.Join(testDir, "metadata.json"), append(metadata, '\n'), 0o644); err != nil {
			return fmt.Errorf("failed to write metadata.json for %q: %w", result.Name, err)
		}
		written++
	}

	logger.V(1).Info("Wrote test metadata", "path", runDir, "failed", written, "total", len(allResults))
	return nil
}

// findArtifactDir lists subdirectories under the artifacts/ prefix for a job run
// and returns the one whose name is a suffix of the job name. If multiple match,
// the longest (most specific) suffix wins.
func findArtifactDir(ctx context.Context, gcsClient *storage.Client, jobName, gcsPrefix string) (string, error) {
	prefix := fmt.Sprintf("%s/artifacts/", gcsPrefix)
	it := gcsClient.Bucket(GCSBucket).Objects(ctx, &storage.Query{
		Prefix:    prefix,
		Delimiter: "/",
	})

	var bestMatch string
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return "", fmt.Errorf("failed to list objects: %w", err)
		}
		if attrs.Prefix == "" {
			continue // not a directory
		}
		// attrs.Prefix looks like "logs/<job>/<id>/artifacts/<dir>/"
		dir := strings.TrimPrefix(attrs.Prefix, prefix)
		dir = strings.TrimSuffix(dir, "/")
		if strings.HasSuffix(jobName, dir) {
			if len(dir) > len(bestMatch) {
				bestMatch = dir
			}
		}
	}

	if bestMatch == "" {
		return "", fmt.Errorf("no artifact directory found matching a suffix of job name %q under %s", jobName, prefix)
	}
	return bestMatch, nil
}

// listObjects returns the full object names matching a given prefix in the bucket.
func listObjects(ctx context.Context, gcsClient *storage.Client, prefix string) ([]string, error) {
	it := gcsClient.Bucket(GCSBucket).Objects(ctx, &storage.Query{
		Prefix: prefix,
	})

	var objects []string
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}
		if attrs.Name != "" {
			objects = append(objects, attrs.Name)
		}
	}
	return objects, nil
}

func downloadObject(ctx context.Context, gcsClient *storage.Client, path string) ([]byte, error) {
	reader, err := gcsClient.Bucket(GCSBucket).Object(path).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open object %s: %w", path, err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read object %s: %w", path, err)
	}
	return data, nil
}

func filterConfig(data []byte) ([]byte, error) {
	var src sourceConfig
	if err := yaml.Unmarshal(data, &src); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	out := filteredConfig{
		Region: src.Region,
		Kusto: filteredKusto{
			Name:                           src.Kusto.KustoName,
			HostedControlPlaneLogsDatabase: src.Kusto.HostedControlPlaneLogsDatabase,
			ServiceLogsDatabase:            src.Kusto.ServiceLogsDatabase,
		},
	}

	result, err := yaml.Marshal(&out)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal filtered YAML: %w", err)
	}
	return result, nil
}

// SanitizeTestName replaces any character that is not alphanumeric, a dash,
// or an underscore with an underscore, producing a valid filesystem path component.
func SanitizeTestName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

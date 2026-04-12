package jobfailures

import (
	"testing"
)

func TestParseProwURL(t *testing.T) {
	tests := []struct {
		name          string
		url           string
		wantJob       string
		wantProwID    string
		wantGCSPrefix string
		wantIsPR      bool
		wantErrMsg    string
	}{
		{
			name:          "standard prow URL with gs prefix",
			url:           "https://prow.ci.openshift.org/view/gs/test-platform-results/logs/periodic-ci-Azure-ARO-HCP-main-aro-hcp-e2e-parallel/1234567890",
			wantJob:       "periodic-ci-Azure-ARO-HCP-main-aro-hcp-e2e-parallel",
			wantProwID:    "1234567890",
			wantGCSPrefix: "logs/periodic-ci-Azure-ARO-HCP-main-aro-hcp-e2e-parallel/1234567890",
			wantIsPR:      false,
		},
		{
			name:          "prow URL with gcs prefix",
			url:           "https://prow.ci.openshift.org/view/gcs/test-platform-results/logs/periodic-ci-Azure-ARO-HCP-main-aro-hcp-e2e-parallel/1234567890",
			wantJob:       "periodic-ci-Azure-ARO-HCP-main-aro-hcp-e2e-parallel",
			wantProwID:    "1234567890",
			wantGCSPrefix: "logs/periodic-ci-Azure-ARO-HCP-main-aro-hcp-e2e-parallel/1234567890",
			wantIsPR:      false,
		},
		{
			name:          "URL with trailing slash",
			url:           "https://prow.ci.openshift.org/view/gs/test-platform-results/logs/my-job/999/",
			wantJob:       "my-job",
			wantProwID:    "999",
			wantGCSPrefix: "logs/my-job/999",
			wantIsPR:      false,
		},
		{
			name:          "minimal URL with just logs path",
			url:           "https://example.com/logs/some-job/42",
			wantJob:       "some-job",
			wantProwID:    "42",
			wantGCSPrefix: "logs/some-job/42",
			wantIsPR:      false,
		},
		{
			name:          "PR job URL",
			url:           "https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/pull/Azure_ARO-HCP/4845/pull-ci-Azure-ARO-HCP-main-e2e-parallel/2043043812057550848",
			wantJob:       "pull-ci-Azure-ARO-HCP-main-e2e-parallel",
			wantProwID:    "2043043812057550848",
			wantGCSPrefix: "pr-logs/pull/Azure_ARO-HCP/4845/pull-ci-Azure-ARO-HCP-main-e2e-parallel/2043043812057550848",
			wantIsPR:      true,
		},
		{
			name:          "PR job URL with trailing slash",
			url:           "https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/pull/Azure_ARO-HCP/4845/pull-ci-Azure-ARO-HCP-main-e2e-parallel/2043043812057550848/",
			wantJob:       "pull-ci-Azure-ARO-HCP-main-e2e-parallel",
			wantProwID:    "2043043812057550848",
			wantGCSPrefix: "pr-logs/pull/Azure_ARO-HCP/4845/pull-ci-Azure-ARO-HCP-main-e2e-parallel/2043043812057550848",
			wantIsPR:      true,
		},
		{
			name:          "PR job URL with gcs prefix",
			url:           "https://prow.ci.openshift.org/view/gcs/test-platform-results/pr-logs/pull/Azure_ARO-HCP/100/pull-ci-Azure-ARO-HCP-main-e2e/555",
			wantJob:       "pull-ci-Azure-ARO-HCP-main-e2e",
			wantProwID:    "555",
			wantGCSPrefix: "pr-logs/pull/Azure_ARO-HCP/100/pull-ci-Azure-ARO-HCP-main-e2e/555",
			wantIsPR:      true,
		},
		{
			name:       "empty URL",
			url:        "",
			wantErrMsg: "does not contain a \"logs\" or \"pr-logs\" segment",
		},
		{
			name:       "URL without logs segment",
			url:        "https://prow.ci.openshift.org/view/gs/test-platform-results/other/my-job/123",
			wantErrMsg: "does not contain a \"logs\" or \"pr-logs\" segment",
		},
		{
			name:       "URL with logs but missing prow ID",
			url:        "https://prow.ci.openshift.org/view/gs/test-platform-results/logs/my-job",
			wantErrMsg: "must contain logs/<job-name>/<prow-id>",
		},
		{
			name:       "URL with logs but only job name, no ID",
			url:        "https://prow.ci.openshift.org/view/gs/test-platform-results/logs/my-job/",
			wantErrMsg: "must contain logs/<job-name>/<prow-id>",
		},
		{
			name:       "URL with non-numeric prow ID",
			url:        "https://prow.ci.openshift.org/view/gs/test-platform-results/logs/my-job/not-a-number",
			wantErrMsg: "not a valid number",
		},
		{
			name:       "URL with negative prow ID",
			url:        "https://prow.ci.openshift.org/view/gs/test-platform-results/logs/my-job/-1",
			wantErrMsg: "not a valid number",
		},
		{
			name:       "PR URL with missing segments",
			url:        "https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/pull/Azure_ARO-HCP/4845",
			wantErrMsg: "must contain pr-logs/pull/<org_repo>/<pr-number>/<job-name>/<prow-id>",
		},
		{
			name:       "PR URL with non-numeric prow ID",
			url:        "https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/pull/Azure_ARO-HCP/4845/my-job/not-a-number",
			wantErrMsg: "not a valid number",
		},
		{
			name:       "PR URL missing pull segment",
			url:        "https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/notpull/Azure_ARO-HCP/4845/my-job/123",
			wantErrMsg: "expected \"pull\" after \"pr-logs\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotJob, gotProwID, gotGCSPrefix, gotIsPR, err := parseProwURL(tt.url)

			if tt.wantErrMsg != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrMsg)
				}
				if !containsSubstring(err.Error(), tt.wantErrMsg) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErrMsg, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotJob != tt.wantJob {
				t.Errorf("job name: got %q, want %q", gotJob, tt.wantJob)
			}
			if gotProwID != tt.wantProwID {
				t.Errorf("prow ID: got %q, want %q", gotProwID, tt.wantProwID)
			}
			if gotGCSPrefix != tt.wantGCSPrefix {
				t.Errorf("GCS prefix: got %q, want %q", gotGCSPrefix, tt.wantGCSPrefix)
			}
			if gotIsPR != tt.wantIsPR {
				t.Errorf("isPR: got %v, want %v", gotIsPR, tt.wantIsPR)
			}
		})
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

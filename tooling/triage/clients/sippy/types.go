package sippy

// Sippy API types for interacting with https://sippy.dptools.openshift.org

// JobRunsResponse is the response structure from Sippy's /api/jobs/runs endpoint.
// When no pagination parameters are provided, all matching rows are returned.
type JobRunsResponse struct {
	Rows      []JobRun `json:"rows"`
	Page      int      `json:"page"`
	PageSize  int64    `json:"page_size"`
	TotalRows int64    `json:"total_rows"`
}

// JobRun represents a single Prow job run from Sippy.
type JobRun struct {
	ID                    int               `json:"id"`
	Variants              []string          `json:"variants"`
	Tags                  []string          `json:"tags"`
	TestGridURL           string            `json:"test_grid_url"`
	ProwID                uint              `json:"prow_id"`
	Job                   string            `json:"job"`
	Cluster               string            `json:"cluster"`
	URL                   string            `json:"url"`
	TestFlakes            int               `json:"test_flakes"`
	FlakedTestNames       []string          `json:"flaked_test_names"`
	TestFailures          int               `json:"test_failures"`
	FailedTestNames       []string          `json:"failed_test_names"`
	Failed                bool              `json:"failed"`
	InfrastructureFailure bool              `json:"infrastructure_failure"`
	KnownFailure          bool              `json:"known_failure"`
	Succeeded             bool              `json:"succeeded"`
	Timestamp             int64             `json:"timestamp"`
	OverallResult         JobOverallResult  `json:"overall_result"`
	PullRequestOrg        string            `json:"pull_request_org"`
	PullRequestRepo       string            `json:"pull_request_repo"`
	PullRequestLink       string            `json:"pull_request_link"`
	PullRequestSHA        string            `json:"pull_request_sha"`
	PullRequestAuthor     string            `json:"pull_request_author"`
	Annotations           map[string]string `json:"annotations"`
}

type JobOverallResult string

const (
	JobSucceeded             JobOverallResult = "S"
	JobRunning               JobOverallResult = "R"
	JobInfrastructureFailure JobOverallResult = "N"
	JobInstallFailure        JobOverallResult = "I"
	JobUpgradeFailure        JobOverallResult = "U"
	JobTestFailure           JobOverallResult = "F"
	JobFailureBeforeSetup    JobOverallResult = "n"
	JobAborted               JobOverallResult = "A"
	JobUnknown               JobOverallResult = "f"
)

const (
	HCPPublicInt  = "aro-integration"
	HCPPublicStg  = "aro-stage"
	HCPPublicProd = "aro-production"
)

var HCPEnvironments = []string{HCPPublicInt, HCPPublicStg, HCPPublicProd}

var AllEnvironments = HCPEnvironments

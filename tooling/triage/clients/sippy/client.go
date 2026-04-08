package sippy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/go-logr/logr"
)

const (
	// DefaultEndpoint is the Sippy API endpoint
	DefaultEndpoint = "https://sippy.dptools.openshift.org"
	// MaxLookbackDays is the maximum lookback period Sippy supports.
	// Sippy only retains 90 days of data, so querying beyond this returns no results.
	MaxLookbackDays = 90
	// jobRunsPath is the API path for listing job runs
	jobRunsPath = "api/jobs/runs"
)

type Client interface {
	// ListJobRuns fetches job runs from Sippy for a given release/environment.
	// The filter can be used to narrow results by job name patterns and timestamps.
	ListJobRuns(ctx context.Context, args ListJobRunsArgs) (*JobRunsResponse, error)
}

type ListJobRunsArgs struct {
	Release string // Release is the Sippy release name (see HCPEnvironments and ClassicEnvironments constants)
	Filter  Filter // Filter is the filter to apply to the query
}

type Filter struct {
	Items        []FilterItem `json:"items"`
	LinkOperator string       `json:"linkOperator,omitempty"`
}

type FilterItem struct {
	ColumnField   string `json:"columnField"`
	OperatorValue string `json:"operatorValue"`
	Value         string `json:"value"`
}

type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type client struct {
	endpoint string
	client   httpClient
}

var _ Client = (*client)(nil)

func NewClient() Client {
	return &client{
		endpoint: DefaultEndpoint,
		client:   http.DefaultClient,
	}
}

func (c *client) ListJobRuns(ctx context.Context, args ListJobRunsArgs) (*JobRunsResponse, error) {
	logger, err := logr.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get logger from context: %w", err)
	}

	filterJSON, err := json.Marshal(args.Filter)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal filter: %w", err)
	}

	// Build URL with query parameters.
	// Note: Sippy API returns all matching results when no pagination parameters (page, pageSize)
	// are specified, so we don't need to handle pagination.
	requestURL, err := url.Parse(c.endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse endpoint: %w", err)
	}
	requestURL.Path = jobRunsPath

	queryParams := requestURL.Query()
	queryParams.Set("release", args.Release)
	queryParams.Set("filter", string(filterJSON))
	requestURL.RawQuery = queryParams.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Error(err, "Failed to close response body.")
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &ResponseError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
			URL:        requestURL.String(),
		}
	}

	var jobRuns JobRunsResponse
	if err := json.Unmarshal(body, &jobRuns); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &jobRuns, nil
}

type ResponseError struct {
	StatusCode int
	Body       string
	URL        string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("Sippy API returned status %d for %s: %s", e.StatusCode, e.URL, e.Body)
}

package tracerequest

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/go-logr/logr"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/Azure/ARO-HCP/tooling/triage/pkg/kql"
)

//go:embed queries
var queriesFS embed.FS

// querySpec describes a single KQL query to execute and where to write results.
type querySpec struct {
	// component is the top-level directory name (e.g. "frontend").
	component string
	// queryName is the sub-directory name (e.g. "asyncOperationId").
	queryName string
	// templatePath is the path within the embedded FS (e.g. "queries/frontend/asyncOperationId/query.kql").
	templatePath string
	// database selects which database to query against: "service" or "hcp".
	database string
	// ready returns true when all data required by this query's template is available
	// in the templateData. Queries whose ready function returns false are skipped.
	// A nil ready function means the query has no prerequisites.
	ready func(templateData) bool
	// storeResult is called with the first column of every row in the query's result.
	// It stores the values into the appropriate field(s) on templateData so downstream
	// queries can reference them. A nil storeResult means the query's output is not
	// consumed by other queries.
	storeResult func(*templateData, []string)
}

// key returns the "component/queryName" identifier for this query.
func (q querySpec) key() string {
	return q.component + "/" + q.queryName
}

var queries = []querySpec{
	{
		component:    "frontend",
		queryName:    "resourceId",
		templatePath: "queries/frontend/resourceId/query.kql",
		database:     "service",
		storeResult: func(d *templateData, results []string) {
			d.ResourceID = results[0]
			parsed, err := azcorearm.ParseResourceID(results[0])
			if err != nil {
				return
			}
			d.ResourceGroup = parsed.ResourceGroupName
			d.ResourceType = parsed.ResourceType.String()
			d.ResourceName = parsed.Name
			d.ServiceProviderResourceType = serviceProviderResourceType(d.ResourceType)
		},
	},
	{
		component:    "frontend",
		queryName:    "asyncOperationId",
		templatePath: "queries/frontend/asyncOperationId/query.kql",
		database:     "service",
		storeResult: func(d *templateData, results []string) {
			d.AsyncOperationId = results[0]
		},
	},
	{
		component:    "frontend",
		queryName:    "asyncOperationPath",
		templatePath: "queries/frontend/asyncOperationPath/query.kql",
		database:     "service",
		storeResult: func(d *templateData, results []string) {
			d.AsyncOperationPath = results[0]
		},
	},
	{
		component:    "frontend",
		queryName:    "asyncOperationRequests",
		templatePath: "queries/frontend/asyncOperationRequests/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return d.AsyncOperationPath != ""
		},
	},
	{
		component:    "frontend",
		queryName:    "events",
		templatePath: "queries/frontend/events/query.kql",
		database:     "service",
	},
	{
		component:    "backend",
		queryName:    "asyncOperationState",
		templatePath: "queries/backend/asyncOperationState/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return d.AsyncOperationId != ""
		},
	},
	{
		component:    "backend",
		queryName:    "resourceState",
		templatePath: "queries/backend/resourceState/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return d.ResourceID != ""
		},
	},
	{
		component:    "backend",
		queryName:    "resourceControllerConditions",
		templatePath: "queries/backend/resourceControllerConditions/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return d.ResourceGroup != "" && d.ResourceType != ""
		},
	},
	{
		component:    "backend",
		queryName:    "serviceProviderState",
		templatePath: "queries/backend/serviceProviderState/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return d.ResourceGroup != "" && d.ServiceProviderResourceType != ""
		},
	},
	{
		component:    "backend",
		queryName:    "resourceInternalId",
		templatePath: "queries/backend/resourceInternalId/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return d.ResourceGroup != "" && d.ResourceType != ""
		},
		storeResult: func(d *templateData, results []string) {
			d.InternalID = results[0]
		},
	},
	{
		component:    "backend",
		queryName:    "events",
		templatePath: "queries/backend/events/query.kql",
		database:     "service",
	},
	{
		component:    "clustersService",
		queryName:    "cid",
		templatePath: "queries/clustersService/cid/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return d.ResourceID != ""
		},
		storeResult: func(d *templateData, results []string) {
			d.ClusterID = results[0]
		},
	},
	{
		component:    "clustersService",
		queryName:    "phases",
		templatePath: "queries/clustersService/phases/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			if d.ResourceID == "" {
				return false
			}
			rt := strings.ToLower(d.ResourceType)
			return rt == "microsoft.redhatopenshift/hcpopenshiftclusters" ||
				rt == "microsoft.redhatopenshift/hcpopenshiftclusters/nodepools"
		},
	},
	{
		component:    "clustersService",
		queryName:    "clusterState",
		templatePath: "queries/clustersService/clusterState/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return d.ResourceGroup != "" && strings.EqualFold(d.ResourceType, "microsoft.redhatopenshift/hcpopenshiftclusters")
		},
	},
	{
		component:    "clustersService",
		queryName:    "maestroBundles",
		templatePath: "queries/clustersService/maestroBundles/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return d.ResourceID != ""
		},
		storeResult: func(d *templateData, results []string) {
			d.BundleIDs = results
		},
	},
	{
		component:    "clustersService",
		queryName:    "events",
		templatePath: "queries/clustersService/events/query.kql",
		database:     "service",
	},
	{
		component:    "maestro",
		queryName:    "events",
		templatePath: "queries/maestro/events/query.kql",
		database:     "service",
	},
	{
		component:    "maestro",
		queryName:    "serverLogs",
		templatePath: "queries/maestro/serverLogs/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return len(d.BundleIDs) > 0
		},
	},
	{
		component:    "maestro",
		queryName:    "agentLogs",
		templatePath: "queries/maestro/agentLogs/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return len(d.BundleIDs) > 0
		},
	},
	{
		component:    "hypershift",
		queryName:    "pkiOperatorEvents",
		templatePath: "queries/hypershift/pkiOperatorEvents/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return d.ClusterID != "" && strings.EqualFold(d.ResourceType, "microsoft.redhatopenshift/hcpopenshiftclusters/requestadmincredential")
		},
	},
	{
		component:    "hypershift",
		queryName:    "hostedClusterConditions",
		templatePath: "queries/hypershift/hostedClusterConditions/query.kql",
		database:     "service",
		ready: func(d templateData) bool {
			return d.ResourceGroup != "" && d.ResourceName != "" && strings.EqualFold(d.ResourceType, "microsoft.redhatopenshift/hcpopenshiftclusters")
		},
	},
}

// templateData is the context available to KQL Go templates for trace-request queries.
type templateData struct {
	ClusterURI      string
	ServiceDatabase string
	HCPDatabase     string
	StartTime       time.Time
	EndTime         time.Time
	CorrelationID   string

	// The following fields are populated by the frontend/resourceId query, which
	// parses the ARM resource ID returned by the frontend.
	ResourceType  string
	ResourceGroup string
	ResourceName  string

	// AsyncOperationId is the ARM async operation resource ID (with the location segment
	// stripped), populated by the frontend/asyncOperationId query.
	AsyncOperationId string
	// AsyncOperationPath is the ARM async operation resource path, populated by the
	// frontend/asyncOperationPath query.
	AsyncOperationPath string
	// ResourceID is the ARM resource ID, populated by the frontend/resourceId query.
	ResourceID string
	// ServiceProviderResourceType is the sub-resource type for the service provider
	// resource corresponding to the input resource type. It is derived from ResourceType:
	//   - microsoft.redhatopenshift/hcpopenshiftclusters -> microsoft.redhatopenshift/hcpopenshiftclusters/serviceproviderclusters
	//   - microsoft.redhatopenshift/hcpopenshiftclusters/nodepools -> microsoft.redhatopenshift/hcpopenshiftclusters/nodepools/serviceprovidernodepools
	// Empty for resource types that have no service provider sub-resource.
	ServiceProviderResourceType string
	// InternalID is the internal (Cluster Service) resource ID, populated by the
	// backend/resourceInternalId query.
	InternalID string
	// BundleIDs is the list of Maestro manifest work bundle resource IDs, populated
	// by the clustersService/maestroBundles query.
	BundleIDs []string
	// ClusterID is the internal Cluster Service identifier for the cluster, populated
	// by the clustersService/cid query.
	ClusterID string
}

// serviceProviderResourceType maps an ARM resource type to its corresponding service
// provider sub-resource type. Returns empty string for resource types that have no
// service provider sub-resource.
func serviceProviderResourceType(resourceType string) string {
	switch strings.ToLower(resourceType) {
	case "microsoft.redhatopenshift/hcpopenshiftclusters":
		return "microsoft.redhatopenshift/hcpopenshiftclusters/serviceproviderclusters"
	case "microsoft.redhatopenshift/hcpopenshiftclusters/nodepools":
		return "microsoft.redhatopenshift/hcpopenshiftclusters/nodepools/serviceprovidernodepools"
	default:
		return ""
	}
}

// RawTraceRequestOptions holds input values as provided by CLI flags.
type RawTraceRequestOptions struct {
	ClusterName     string
	Region          string
	HCPDatabase     string
	ServiceDatabase string
	StartTime       string
	EndTime         string
	CorrelationID   string
	OutputDir       string
}

// validatedTraceRequestOptions is a private wrapper that enforces a call of Validate() before Complete() can be invoked.
type validatedTraceRequestOptions struct {
	clusterName     string
	region          string
	hcpDatabase     string
	serviceDatabase string
	startTime       time.Time
	endTime         time.Time
	correlationID   string
	outputDir       string
}

type ValidatedTraceRequestOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*validatedTraceRequestOptions
}

// completedTraceRequestOptions is a private wrapper that enforces a call of Complete() before Run() can be invoked.
type completedTraceRequestOptions struct {
	cluster         string
	hcpDatabase     string
	serviceDatabase string
	token           string
	outputDir       string
	templateData    templateData
}

type TraceRequestOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedTraceRequestOptions
}

func (o *RawTraceRequestOptions) Validate() (*ValidatedTraceRequestOptions, error) {
	if o.ClusterName == "" {
		return nil, fmt.Errorf("--cluster-name is required")
	}
	if o.Region == "" {
		return nil, fmt.Errorf("--region is required")
	}
	if o.HCPDatabase == "" {
		return nil, fmt.Errorf("--hcp-database is required")
	}
	if o.ServiceDatabase == "" {
		return nil, fmt.Errorf("--service-database is required")
	}
	if o.StartTime == "" {
		return nil, fmt.Errorf("--start-time is required")
	}
	if o.EndTime == "" {
		return nil, fmt.Errorf("--end-time is required")
	}
	if o.CorrelationID == "" {
		return nil, fmt.Errorf("--correlation-id is required")
	}
	if o.OutputDir == "" {
		return nil, fmt.Errorf("--output-dir is required")
	}

	startTime, err := time.Parse(time.RFC3339, o.StartTime)
	if err != nil {
		return nil, fmt.Errorf("invalid --start-time %q: must be RFC3339 format (e.g. 2024-01-15T10:30:00Z): %w", o.StartTime, err)
	}
	endTime, err := time.Parse(time.RFC3339, o.EndTime)
	if err != nil {
		return nil, fmt.Errorf("invalid --end-time %q: must be RFC3339 format (e.g. 2024-01-16T10:30:00Z): %w", o.EndTime, err)
	}
	if !startTime.Before(endTime) {
		return nil, fmt.Errorf("--start-time (%s) must be before --end-time (%s)", o.StartTime, o.EndTime)
	}

	return &ValidatedTraceRequestOptions{
		validatedTraceRequestOptions: &validatedTraceRequestOptions{
			clusterName:     o.ClusterName,
			region:          o.Region,
			hcpDatabase:     o.HCPDatabase,
			serviceDatabase: o.ServiceDatabase,
			startTime:       startTime,
			endTime:         endTime,
			correlationID:   o.CorrelationID,
			outputDir:       o.OutputDir,
		},
	}, nil
}

func (o *ValidatedTraceRequestOptions) Complete(ctx context.Context) (*TraceRequestOptions, error) {
	logger := logr.FromContextOrDiscard(ctx)

	// Acquire a bearer token for the Kusto cluster
	cluster := kql.KustoEndpoint(o.clusterName, o.region)
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure credential: %w", err)
	}

	scope := cluster + "/.default"
	tokenResponse, err := cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{scope},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to acquire token for scope %q: %w", scope, err)
	}
	logger.V(1).Info("Acquired bearer token", "scope", scope)

	return &TraceRequestOptions{
		completedTraceRequestOptions: &completedTraceRequestOptions{
			cluster:         cluster,
			hcpDatabase:     o.hcpDatabase,
			serviceDatabase: o.serviceDatabase,
			token:           tokenResponse.Token,
			outputDir:       o.outputDir,
			templateData: templateData{
				ClusterURI:      cluster,
				ServiceDatabase: o.serviceDatabase,
				HCPDatabase:     o.hcpDatabase,
				StartTime:       o.startTime,
				EndTime:         o.endTime,
				CorrelationID:   o.correlationID,
			},
		},
	}, nil
}

// kustoQueryRequest is the JSON payload for the Kusto /v2/rest/query endpoint.
type kustoQueryRequest struct {
	DB  string `json:"db"`
	CSL string `json:"csl"`
}

func (o *TraceRequestOptions) Run(ctx context.Context) error {
	logger := logr.FromContextOrDiscard(ctx)

	funcMap := template.FuncMap{
		"kqlDatetime": kql.KQLDatetime,
		"toLower":     strings.ToLower,
	}

	for _, q := range queries {
		// If this query has prerequisites, check that they are satisfied.
		if q.ready != nil && !q.ready(o.templateData) {
			logger.Info("Skipping query because prerequisites are not satisfied", "query", q.key())
			continue
		}

		// Render the template just before execution so that results from prior queries are available.
		renderedKQL, err := renderEmbeddedTemplate(funcMap, q.templatePath, o.templateData)
		if err != nil {
			return fmt.Errorf("query %s: %w", q.key(), err)
		}
		logger.V(1).Info("Rendered query", "query", q.key())

		result, err := o.runQuery(ctx, logger, q, renderedKQL)
		if err != nil {
			return fmt.Errorf("query %s failed: %w", q.key(), err)
		}

		// Store the result for downstream queries.
		if q.storeResult != nil && len(result) > 0 {
			q.storeResult(&o.templateData, result)
		}
	}

	return nil
}

func renderEmbeddedTemplate(funcMap template.FuncMap, templatePath string, data templateData) (string, error) {
	tmplBytes, err := queriesFS.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to read embedded query %s: %w", templatePath, err)
	}
	tmpl, err := template.New(templatePath).Funcs(funcMap).Parse(string(tmplBytes))
	if err != nil {
		return "", fmt.Errorf("failed to parse query template %s: %w", templatePath, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to render query template %s: %w", templatePath, err)
	}
	return buf.String(), nil
}

// runQuery executes a single KQL query, writes query.kql and output.log, and returns
// the first column of every data row (for use by dependent queries). If the query
// returns no data rows, the returned slice is nil.
func (o *TraceRequestOptions) runQuery(ctx context.Context, logger logr.Logger, q querySpec, renderedKQL string) ([]string, error) {
	outDir := filepath.Join(o.outputDir, q.component, q.queryName)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create output directory %s: %w", outDir, err)
	}

	// Write the rendered query
	queryFile := filepath.Join(outDir, "query.kql")
	if err := os.WriteFile(queryFile, []byte(renderedKQL), 0o644); err != nil {
		return nil, fmt.Errorf("failed to write query file %s: %w", queryFile, err)
	}
	logger.Info("Wrote query", "file", queryFile)

	// Determine which database to use
	db := o.serviceDatabase
	if q.database == "hcp" {
		db = o.hcpDatabase
	}

	// Execute the query against Kusto
	endpoint := o.cluster + "/v2/rest/query"
	logger.V(1).Info("Sending query to Kusto", "endpoint", endpoint, "database", db, "query", q.key())

	reqBody := kustoQueryRequest{
		DB:  db,
		CSL: renderedKQL,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Kusto query failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Read the full response so we can both write it to a file and parse it.
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Write raw query output
	outputFile := filepath.Join(outDir, "output.json")
	if err := os.WriteFile(outputFile, responseBody, 0o644); err != nil {
		return nil, fmt.Errorf("failed to write output file %s: %w", outputFile, err)
	}
	logger.Info("Wrote output", "file", outputFile)

	// Write pretty-printed markdown table
	columns, rows := kql.ParsePrimaryResult(responseBody)
	if len(columns) > 0 {
		mdFile := filepath.Join(outDir, "output.md")
		if err := os.WriteFile(mdFile, []byte(kql.RenderMarkdownTable(columns, rows)), 0o644); err != nil {
			return nil, fmt.Errorf("failed to write markdown file %s: %w", mdFile, err)
		}
		logger.Info("Wrote markdown", "file", mdFile)
	}

	// Extract the first column of every data row from the Kusto v2 response.
	results := extractFirstColumn(responseBody)
	if len(results) > 0 {
		logger.V(1).Info("Extracted results", "query", q.key(), "count", len(results))
	}
	return results, nil
}

// extractFirstColumn parses a Kusto v2 JSON response and returns the string value
// of the first cell in every row of the PrimaryResult DataTable frame.
// Returns nil if no data is found.
func extractFirstColumn(body []byte) []string {
	_, rows := kql.ParsePrimaryResult(body)
	var results []string
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		val := strings.TrimSpace(fmt.Sprintf("%v", row[0]))
		if val != "" {
			results = append(results, val)
		}
	}
	return results
}

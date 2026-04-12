package kustoquery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/go-logr/logr"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/Azure/ARO-HCP/tooling/triage/pkg/kql"
)

// completedKustoQueryOptions is a private wrapper that enforces a call of complete() before Run() can be invoked.
type completedKustoQueryOptions struct {
	cluster         string
	hcpDatabase     string
	serviceDatabase string
	renderedKQL     string
	token           string
}

type KustoQueryOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedKustoQueryOptions
}

func complete(ctx context.Context, validated *kql.ValidatedTemplateOptions) (*KustoQueryOptions, error) {
	logger := logr.FromContextOrDiscard(ctx)

	renderedKQL, err := validated.Render()
	if err != nil {
		return nil, err
	}
	logger.V(1).Info("Rendered KQL query", "kql", renderedKQL)

	// Acquire a bearer token for the Kusto cluster
	cluster := kql.KustoEndpoint(validated.ClusterName, validated.Region)
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

	return &KustoQueryOptions{
		completedKustoQueryOptions: &completedKustoQueryOptions{
			cluster:         cluster,
			hcpDatabase:     validated.HCPDatabase,
			serviceDatabase: validated.ServiceDatabase,
			renderedKQL:     renderedKQL,
			token:           tokenResponse.Token,
		},
	}, nil
}

// kustoQueryRequest is the JSON payload for the Kusto /v2/rest/query endpoint.
type kustoQueryRequest struct {
	DB  string `json:"db"`
	CSL string `json:"csl"`
}

func (o *KustoQueryOptions) Run(ctx context.Context) error {
	logger := logr.FromContextOrDiscard(ctx)

	endpoint := o.cluster + "/v2/rest/query"
	logger.V(1).Info("Sending query to Kusto", "endpoint", endpoint, "hcpDatabase", o.hcpDatabase, "serviceDatabase", o.serviceDatabase)

	reqBody := kustoQueryRequest{
		DB:  o.hcpDatabase,
		CSL: o.renderedKQL,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := kql.DoWithRetry(ctx, logger, req)
	if err != nil {
		return fmt.Errorf("failed to execute query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Kusto query failed with status %d: %s", resp.StatusCode, string(body))
	}

	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return fmt.Errorf("failed to write response: %w", err)
	}

	return nil
}

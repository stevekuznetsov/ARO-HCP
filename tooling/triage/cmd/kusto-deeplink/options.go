package kustodeeplink

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"

	"github.com/go-logr/logr"

	"github.com/Azure/ARO-HCP/tooling/triage/pkg/kql"
)

// completedDeeplinkOptions is a private wrapper that enforces a call of complete() before Run() can be invoked.
type completedDeeplinkOptions struct {
	deepLink string
	rawKQL   string
}

type DeeplinkOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedDeeplinkOptions
}

func complete(ctx context.Context, validated *kql.ValidatedTemplateOptions) (*DeeplinkOptions, error) {
	logger := logr.FromContextOrDiscard(ctx)

	renderedKQL, err := validated.Render()
	if err != nil {
		return nil, err
	}
	logger.V(1).Info("Rendered KQL query", "kql", renderedKQL)

	encoded, err := encodeKQL(renderedKQL)
	if err != nil {
		return nil, fmt.Errorf("failed to encode KQL query: %w", err)
	}

	deepLink := buildDeepLink(
		kql.KustoHost(validated.ClusterName, validated.Region),
		validated.HCPDatabase,
		encoded,
	)

	return &DeeplinkOptions{
		completedDeeplinkOptions: &completedDeeplinkOptions{
			deepLink: deepLink,
			rawKQL:   renderedKQL,
		},
	}, nil
}

func (o *DeeplinkOptions) Run(_ context.Context) error {
	fmt.Printf("Open in [ADX Web](%s)\n\n```kql\n%s\n```\n", o.deepLink, o.rawKQL)
	return nil
}

// encodeKQL compresses the KQL string with gzip and then base64-encodes the result,
// producing the encoding that Azure Data Explorer expects in deep-link query parameters.
func encodeKQL(query string) (string, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(query)); err != nil {
		return "", fmt.Errorf("gzip write failed: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", fmt.Errorf("gzip close failed: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// buildDeepLink constructs an Azure Data Explorer web UI deep-link URL.
// The format is:
//
//	https://dataexplorer.azure.com/clusters/<cluster-host>/databases/<database>?query=<encoded>
func buildDeepLink(clusterHost, database, encodedQuery string) string {
	u := &url.URL{
		Scheme: "https",
		Host:   "dataexplorer.azure.com",
		Path:   fmt.Sprintf("/clusters/%s/databases/%s", clusterHost, database),
	}
	q := u.Query()
	q.Set("query", encodedQuery)
	u.RawQuery = q.Encode()
	return u.String()
}

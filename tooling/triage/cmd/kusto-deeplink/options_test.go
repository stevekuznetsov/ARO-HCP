package kustodeeplink

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"strings"
	"testing"
)

func TestEncodeKQL(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{
			name:  "simple query",
			query: "StormEvents | take 10",
		},
		{
			name:  "empty query",
			query: "",
		},
		{
			name:  "complex query with special characters",
			query: "StormEvents\n| where StartTime >= datetime(2024-01-15T10:30:00Z)\n| where State == \"TEXAS\"\n| summarize count() by EventType\n| order by count_ desc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := encodeKQL(tt.query)
			if err != nil {
				t.Fatalf("encodeKQL() error: %v", err)
			}

			// Verify it's valid base64
			compressed, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("base64 decode failed: %v", err)
			}

			// Verify it decompresses back to the original query
			gz, err := gzip.NewReader(bytes.NewReader(compressed))
			if err != nil {
				t.Fatalf("gzip reader creation failed: %v", err)
			}
			defer gz.Close()

			decompressed, err := io.ReadAll(gz)
			if err != nil {
				t.Fatalf("gzip read failed: %v", err)
			}

			if string(decompressed) != tt.query {
				t.Errorf("round-trip mismatch:\n  got:  %q\n  want: %q", string(decompressed), tt.query)
			}
		})
	}
}

func TestBuildDeepLink(t *testing.T) {
	tests := []struct {
		name         string
		clusterHost  string
		database     string
		encodedQuery string
		wantPrefix   string
		wantContains []string
	}{
		{
			name:         "standard deep link",
			clusterHost:  "mycluster.westus3.kusto.windows.net",
			database:     "mydb",
			encodedQuery: "abc123==",
			wantPrefix:   "https://dataexplorer.azure.com/clusters/mycluster.westus3.kusto.windows.net/databases/mydb",
			wantContains: []string{"query=abc123"},
		},
		{
			name:         "encoded query with special base64 characters",
			clusterHost:  "cluster.eastus2.kusto.windows.net",
			database:     "logs",
			encodedQuery: "H4sI+AAAA/8=",
			wantPrefix:   "https://dataexplorer.azure.com/clusters/cluster.eastus2.kusto.windows.net/databases/logs",
			wantContains: []string{"query="},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildDeepLink(tt.clusterHost, tt.database, tt.encodedQuery)

			if !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("buildDeepLink() prefix mismatch:\n  got:  %q\n  want prefix: %q", got, tt.wantPrefix)
			}

			for _, substr := range tt.wantContains {
				if !strings.Contains(got, substr) {
					t.Errorf("buildDeepLink() missing substring %q in %q", substr, got)
				}
			}
		})
	}
}

func TestEncodeKQLRoundTrip(t *testing.T) {
	// End-to-end: encode a known query, build a deep link, verify the URL structure
	query := "StormEvents | where State == 'TEXAS' | take 5"

	encoded, err := encodeKQL(query)
	if err != nil {
		t.Fatalf("encodeKQL() error: %v", err)
	}

	link := buildDeepLink("mycluster.westus3.kusto.windows.net", "testdb", encoded)

	if !strings.HasPrefix(link, "https://dataexplorer.azure.com/") {
		t.Errorf("expected https://dataexplorer.azure.com/ prefix, got %q", link)
	}
	if !strings.Contains(link, "/clusters/mycluster.westus3.kusto.windows.net/") {
		t.Errorf("expected cluster host in path, got %q", link)
	}
	if !strings.Contains(link, "/databases/testdb") {
		t.Errorf("expected database in path, got %q", link)
	}
	if !strings.Contains(link, "query=") {
		t.Errorf("expected query parameter, got %q", link)
	}
}

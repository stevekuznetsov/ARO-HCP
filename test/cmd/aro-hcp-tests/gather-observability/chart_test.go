// Copyright 2025 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gatherobservability

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestParsePrometheusValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		input  []any
		wantTS int64
		wantV  float64
		wantOK bool
	}{
		{
			name:   "normal float64 timestamp and string value",
			input:  []any{float64(1700000000), "42.5"},
			wantTS: 1700000000,
			wantV:  42.5,
			wantOK: true,
		},
		{
			name:   "json.Number timestamp",
			input:  []any{json.Number("1700000000"), "100"},
			wantTS: 1700000000,
			wantV:  100,
			wantOK: true,
		},
		{
			name:   "string value parsed as float",
			input:  []any{float64(1700000000), "3.14"},
			wantTS: 1700000000,
			wantV:  3.14,
			wantOK: true,
		},
		{
			name:   "NaN value returns not ok",
			input:  []any{float64(1700000000), "NaN"},
			wantTS: 1700000000,
			wantV:  0,
			wantOK: false,
		},
		{
			name:   "+Inf value is capped to MaxFloat64",
			input:  []any{float64(1700000000), "+Inf"},
			wantTS: 1700000000,
			wantV:  math.MaxFloat64,
			wantOK: true,
		},
		{
			name:   "-Inf value is capped to -MaxFloat64",
			input:  []any{float64(1700000000), "-Inf"},
			wantTS: 1700000000,
			wantV:  -math.MaxFloat64,
			wantOK: true,
		},
		{
			name:   "zero value is valid",
			input:  []any{float64(1700000000), "0"},
			wantTS: 1700000000,
			wantV:  0,
			wantOK: true,
		},
		{
			name:   "float64 as value instead of string",
			input:  []any{float64(1700000000), float64(99.9)},
			wantTS: 1700000000,
			wantV:  99.9,
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts, val, ok := parsePrometheusValue(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ts != tt.wantTS {
				t.Errorf("ts = %d, want %d", ts, tt.wantTS)
			}
			if val != tt.wantV {
				t.Errorf("val = %v, want %v", val, tt.wantV)
			}
		})
	}
}

func TestMetricLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		metric map[string]string
		want   string
	}{
		{
			name:   "empty map returns value",
			metric: map[string]string{},
			want:   "value",
		},
		{
			name:   "nil map returns value",
			metric: nil,
			want:   "value",
		},
		{
			name:   "single label",
			metric: map[string]string{"pod": "my-pod"},
			want:   "pod=my-pod",
		},
		{
			name:   "multiple labels sorted alphabetically",
			metric: map[string]string{"namespace": "default", "container": "web", "pod": "app-1"},
			want:   "container=web, namespace=default, pod=app-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := metricLabel(tt.metric)
			if got != tt.want {
				t.Errorf("metricLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFindCommonLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		series []parsedSeries
		want   map[string]bool
	}{
		{
			name: "single series returns nil",
			series: []parsedSeries{
				{metric: map[string]string{"pod": "a", "ns": "default"}},
			},
			want: nil,
		},
		{
			name: "identical series - all labels common",
			series: []parsedSeries{
				{metric: map[string]string{"ns": "default", "job": "prom"}},
				{metric: map[string]string{"ns": "default", "job": "prom"}},
			},
			want: map[string]bool{"ns": true, "job": true},
		},
		{
			name: "partial overlap",
			series: []parsedSeries{
				{metric: map[string]string{"ns": "default", "pod": "a"}},
				{metric: map[string]string{"ns": "default", "pod": "b"}},
			},
			want: map[string]bool{"ns": true},
		},
		{
			name: "no common labels",
			series: []parsedSeries{
				{metric: map[string]string{"pod": "a"}},
				{metric: map[string]string{"pod": "b"}},
			},
			want: map[string]bool{},
		},
		{
			name: "key missing from second series is not common",
			series: []parsedSeries{
				{metric: map[string]string{"ns": "default", "extra": "val"}},
				{metric: map[string]string{"ns": "default"}},
			},
			want: map[string]bool{"ns": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := findCommonLabels(tt.series)
			if tt.want == nil {
				if got != nil {
					t.Errorf("findCommonLabels() = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("findCommonLabels() has %d entries, want %d", len(got), len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("findCommonLabels()[%q] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestCompactMetricLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		metric map[string]string
		common map[string]bool
		want   string
	}{
		{
			name:   "all labels are common - falls back to full label",
			metric: map[string]string{"ns": "default", "job": "prom"},
			common: map[string]bool{"ns": true, "job": true},
			want:   "job=prom, ns=default",
		},
		{
			name:   "single diff key shows only value",
			metric: map[string]string{"ns": "default", "pod": "my-pod"},
			common: map[string]bool{"ns": true},
			want:   "my-pod",
		},
		{
			name:   "multiple diff keys shows key=value pairs",
			metric: map[string]string{"ns": "default", "pod": "my-pod", "container": "web"},
			common: map[string]bool{"ns": true},
			want:   "container=web, pod=my-pod",
		},
		{
			name:   "no common labels with single key shows value",
			metric: map[string]string{"pod": "my-pod"},
			common: map[string]bool{},
			want:   "my-pod",
		},
		{
			name:   "nil common same as empty",
			metric: map[string]string{"pod": "a", "ns": "b"},
			common: nil,
			want:   "ns=b, pod=a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := compactMetricLabel(tt.metric, tt.common)
			if got != tt.want {
				t.Errorf("compactMetricLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractChartBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "normal body tags",
			input: "<html><head></head><body><div>chart</div></body></html>",
			want:  "<div>chart</div>",
		},
		{
			name:  "missing body tag returns full content",
			input: "<div>no body tags here</div>",
			want:  "<div>no body tags here</div>",
		},
		{
			name:  "empty body",
			input: "<html><body></body></html>",
			want:  "",
		},
		{
			name:  "body with multiple elements",
			input: "<html><body><div>one</div><script>two</script></body></html>",
			want:  "<div>one</div><script>two</script>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := string(extractChartBody([]byte(tt.input)))
			if got != tt.want {
				t.Errorf("extractChartBody() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeTitle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple lowercase",
			input: "hello",
			want:  "hello",
		},
		{
			name:  "uppercase converted to lowercase",
			input: "Hello World",
			want:  "hello-world",
		},
		{
			name:  "special characters replaced with dashes",
			input: "CPU Usage (%)",
			want:  "cpu-usage",
		},
		{
			name:  "consecutive dashes collapsed",
			input: "foo---bar",
			want:  "foo-bar",
		},
		{
			name:  "leading and trailing dashes trimmed",
			input: "  hello  ",
			want:  "hello",
		},
		{
			name:  "mixed special chars and numbers",
			input: "Node CPU: top 5 (avg)",
			want:  "node-cpu-top-5-avg",
		},
		{
			name:  "already clean",
			input: "clean-title-123",
			want:  "clean-title-123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeTitle(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeTitle(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveWorkspaceEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		spec        QuerySpec
		svcEndpoint string
		hcpEndpoint string
		want        string
	}{
		{
			name:        "hcp workspace returns hcp endpoint",
			spec:        QuerySpec{Workspace: "hcp"},
			svcEndpoint: "https://svc.prometheus.example.com",
			hcpEndpoint: "https://hcp.prometheus.example.com",
			want:        "https://hcp.prometheus.example.com",
		},
		{
			name:        "svc workspace returns svc endpoint",
			spec:        QuerySpec{Workspace: "svc"},
			svcEndpoint: "https://svc.prometheus.example.com",
			hcpEndpoint: "https://hcp.prometheus.example.com",
			want:        "https://svc.prometheus.example.com",
		},
		{
			name:        "unknown workspace defaults to svc endpoint",
			spec:        QuerySpec{Workspace: "other"},
			svcEndpoint: "https://svc.prometheus.example.com",
			hcpEndpoint: "https://hcp.prometheus.example.com",
			want:        "https://svc.prometheus.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveWorkspaceEndpoint(tt.spec, tt.svcEndpoint, tt.hcpEndpoint)
			if got != tt.want {
				t.Errorf("resolveWorkspaceEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadQueriesConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		yaml    string
		wantErr string
		check   func(t *testing.T, cfg *QueriesConfig)
	}{
		{
			name: "valid config with panels",
			yaml: `panels:
  - title: "CPU Panel"
    queries:
    - title: "CPU Usage"
      query: "rate(cpu_seconds_total[5m])"
      workspace: svc
      step: "30s"
    - title: "Memory"
      query: "process_resident_memory_bytes"
      workspace: hcp
`,
			check: func(t *testing.T, cfg *QueriesConfig) {
				if len(cfg.Panels) != 1 {
					t.Fatalf("expected 1 panel, got %d", len(cfg.Panels))
				}
				if cfg.Panels[0].Title != "CPU Panel" {
					t.Errorf("Panels[0].Title = %q, want %q", cfg.Panels[0].Title, "CPU Panel")
				}
				if len(cfg.Panels[0].Queries) != 2 {
					t.Fatalf("expected 2 queries, got %d", len(cfg.Panels[0].Queries))
				}
				if cfg.Panels[0].Queries[0].Step != "30s" {
					t.Errorf("Panels[0].Queries[0].Step = %q, want %q", cfg.Panels[0].Queries[0].Step, "30s")
				}
			},
		},
		{
			name: "missing panel title returns error",
			yaml: `panels:
  - queries:
    - title: "CPU"
      query: "up"
      workspace: svc
`,
			wantErr: "title is required",
		},
		{
			name: "empty queries returns error",
			yaml: `panels:
  - title: "Empty Panel"
    queries: []
`,
			wantErr: "at least one query is required",
		},
		{
			name: "missing query title returns error",
			yaml: `panels:
  - title: "Panel"
    queries:
    - query: "rate(cpu_seconds_total[5m])"
      workspace: svc
`,
			wantErr: "title is required",
		},
		{
			name: "missing query returns error",
			yaml: `panels:
  - title: "Panel"
    queries:
    - title: "CPU Usage"
      workspace: svc
`,
			wantErr: "query is required",
		},
		{
			name: "invalid workspace returns error",
			yaml: `panels:
  - title: "Panel"
    queries:
    - title: "CPU Usage"
      query: "rate(cpu_seconds_total[5m])"
      workspace: mgmt
`,
			wantErr: `workspace must be "svc" or "hcp"`,
		},
		{
			name: "step defaults to 60s when omitted",
			yaml: `panels:
  - title: "Panel"
    queries:
    - title: "CPU Usage"
      query: "rate(cpu_seconds_total[5m])"
      workspace: svc
`,
			check: func(t *testing.T, cfg *QueriesConfig) {
				if cfg.Panels[0].Queries[0].Step != "60s" {
					t.Errorf("Step = %q, want %q", cfg.Panels[0].Queries[0].Step, "60s")
				}
			},
		},
		{
			name: "step is preserved when provided",
			yaml: `panels:
  - title: "Panel"
    queries:
    - title: "CPU Usage"
      query: "rate(cpu_seconds_total[5m])"
      workspace: svc
      step: "15s"
`,
			check: func(t *testing.T, cfg *QueriesConfig) {
				if cfg.Panels[0].Queries[0].Step != "15s" {
					t.Errorf("Step = %q, want %q", cfg.Panels[0].Queries[0].Step, "15s")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := parseQueriesConfig([]byte(tt.yaml))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if got := err.Error(); !strings.Contains(got, tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", got, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestLoadQueriesConfigEmbedded(t *testing.T) {
	t.Parallel()
	cfg, err := loadQueriesConfig()
	if err != nil {
		t.Fatalf("embedded queries.yaml should parse without error: %v", err)
	}
	if len(cfg.Panels) == 0 {
		t.Fatal("embedded queries.yaml should contain at least one panel")
	}
	for _, p := range cfg.Panels {
		if len(p.Queries) == 0 {
			t.Errorf("panel %q should contain at least one query", p.Title)
		}
	}
}

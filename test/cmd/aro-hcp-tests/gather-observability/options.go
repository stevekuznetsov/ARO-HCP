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
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"k8s.io/utils/clock"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/alertsmanagement/armalertsmanagement"

	"github.com/Azure/ARO-HCP/internal/utils"
	"github.com/Azure/ARO-HCP/test/cmd/aro-hcp-tests/internal/testutil"
	"github.com/Azure/ARO-HCP/test/util/timing"
)

//go:embed artifacts/*.html.tmpl
var templatesFS embed.FS

func mustReadArtifact(name string) []byte {
	ret, err := templatesFS.ReadFile("artifacts/" + name)
	if err != nil {
		panic(fmt.Sprintf("failed to read embedded template %q: %v", name, err))
	}
	return ret
}

func DefaultOptions() *RawOptions {
	return &RawOptions{}
}

func BindOptions(opts *RawOptions, cmd *cobra.Command) error {
	cmd.Flags().StringVar(&opts.TimingInputDir, "timing-input", opts.TimingInputDir, "Path to the directory holding timing outputs from an end-to-end test run.")
	cmd.Flags().StringVar(&opts.OutputDir, "output", opts.OutputDir, "Path to the directory where artifacts will be written.")
	cmd.Flags().StringVar(&opts.RenderedConfig, "rendered-config", opts.RenderedConfig, "Path to the rendered configuration YAML file.")
	cmd.Flags().StringVar(&opts.SubscriptionID, "subscription-id", opts.SubscriptionID, "Azure subscription ID.")
	cmd.Flags().StringVar(&opts.StartTimeFallback, "start-time-fallback", opts.StartTimeFallback, "Optional RFC3339 time to use as start time fallback when steps and test timing are unavailable.")
	cmd.Flags().StringVar(&opts.SeverityThreshold, "severity-threshold", opts.SeverityThreshold, "Include alerts at this severity level or more critical (Sev0=critical .. Sev4=verbose). E.g. Sev2 includes Sev0, Sev1, Sev2. If not set, all severities are shown.")
	return nil
}

type RawOptions struct {
	TimingInputDir    string
	OutputDir         string
	RenderedConfig    string
	SubscriptionID    string
	StartTimeFallback string
	SeverityThreshold string
}

type validatedOptions struct {
	*RawOptions
	severityThreshold int // -1 means no filter; 0=Sev0 .. 4=Sev4
}

type ValidatedOptions struct {
	*validatedOptions
}

type completedOptions struct {
	OutputDir         string
	Scope             string // Azure resource scope: /subscriptions/{sub}/resourceGroups/{rg}
	SvcWorkspace      string
	HcpWorkspace      string
	TimeWindow        timing.TimeWindow
	Queries           *QueriesConfig
	SeverityThreshold int // -1 means no filter; 0=Sev0 .. 4=Sev4
	SvcPromEndpoint   string
	HcpPromEndpoint   string
	cred              azcore.TokenCredential
	knownIssues       []knownIssue
}

type Options struct {
	*completedOptions
}

func (o *RawOptions) Validate() (*ValidatedOptions, error) {
	for _, item := range []struct {
		flag  string
		name  string
		value *string
	}{
		{flag: "output", name: "output dir", value: &o.OutputDir},
		{flag: "rendered-config", name: "rendered config", value: &o.RenderedConfig},
		{flag: "subscription-id", name: "subscription ID", value: &o.SubscriptionID},
	} {
		if item.value == nil || *item.value == "" {
			return nil, fmt.Errorf("the %s must be provided with --%s", item.name, item.flag)
		}
	}
	minSev, err := ParseSeverityThreshold(o.SeverityThreshold)
	if err != nil {
		return nil, err
	}
	return &ValidatedOptions{
		validatedOptions: &validatedOptions{RawOptions: o, severityThreshold: minSev},
	}, nil
}

func (o *ValidatedOptions) Complete(ctx context.Context) (*Options, error) {
	logger, err := logr.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("logger not found in context: %w", err)
	}

	// Create output directory early so we fail fast on bad paths before
	// making expensive Azure API calls.
	if err := os.MkdirAll(o.OutputDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create output directory %s: %w", o.OutputDir, err)
	}

	cfg, err := testutil.LoadRenderedConfig(o.RenderedConfig)
	if err != nil {
		return nil, err
	}

	regionRG, err := testutil.ConfigGetString(cfg, "regionRG")
	if err != nil {
		return nil, fmt.Errorf("failed to get regionRG from config: %w", err)
	}
	svcWorkspace, err := testutil.ConfigGetString(cfg, "monitoring.svcWorkspaceName")
	if err != nil {
		return nil, fmt.Errorf("failed to get monitoring.svcWorkspaceName from config: %w", err)
	}
	hcpWorkspace, err := testutil.ConfigGetString(cfg, "monitoring.hcpWorkspaceName")
	if err != nil {
		return nil, fmt.Errorf("failed to get monitoring.hcpWorkspaceName from config: %w", err)
	}

	steps, err := timing.LoadSteps(ctx, o.TimingInputDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load steps: %w", err)
	}

	testTimingInfo, err := timing.LoadTestTimingInfo(ctx, o.TimingInputDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load test timing info: %w", err)
	}

	var startFallback *time.Time
	if o.StartTimeFallback != "" {
		t, err := time.Parse(time.RFC3339, o.StartTimeFallback)
		if err != nil {
			return nil, fmt.Errorf("failed to parse --start-time-fallback %q: %w", o.StartTimeFallback, err)
		}
		startFallback = &t
	}

	tw, err := timing.ComputeTimeWindow(ctx, clock.RealClock{}, steps, testTimingInfo, startFallback)
	if err != nil {
		return nil, fmt.Errorf("failed to compute time window: %w", err)
	}

	cred, err := azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{
		AdditionallyAllowedTenants:   []string{"*"},
		RequireAzureTokenCredentials: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure credential: %w", err)
	}

	scope := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s", o.SubscriptionID, regionRG)

	queries, err := loadQueriesConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load queries config: %w", err)
	}
	var totalQueries int
	for _, p := range queries.Panels {
		totalQueries += len(p.Queries)
	}
	logger.Info("loaded embedded queries config", "panels", len(queries.Panels), "queries", totalQueries)

	// Resolve Prometheus endpoints eagerly so we fail fast
	svcPromEndpoint, err := lookupPrometheusEndpoint(ctx, cred, o.SubscriptionID, regionRG, svcWorkspace)
	if err != nil {
		return nil, fmt.Errorf("failed to look up svc Prometheus endpoint for workspace %s in %s: %w", svcWorkspace, regionRG, err)
	}
	logger.Info("resolved svc Prometheus endpoint", "endpoint", svcPromEndpoint)

	hcpPromEndpoint, err := lookupPrometheusEndpoint(ctx, cred, o.SubscriptionID, regionRG, hcpWorkspace)
	if err != nil {
		return nil, fmt.Errorf("failed to look up hcp Prometheus endpoint for workspace %s in %s: %w", hcpWorkspace, regionRG, err)
	}
	logger.Info("resolved hcp Prometheus endpoint", "endpoint", hcpPromEndpoint)

	knownIssues, err := parseKnownIssues(defaultKnownIssuesData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse known issues config: %w", err)
	}
	logger.Info("loaded known issues config", "patterns", len(knownIssues))

	return &Options{completedOptions: &completedOptions{
		OutputDir:         o.OutputDir,
		Scope:             scope,
		SvcWorkspace:      svcWorkspace,
		HcpWorkspace:      hcpWorkspace,
		TimeWindow:        tw,
		Queries:           queries,
		SeverityThreshold: o.severityThreshold,
		SvcPromEndpoint:   svcPromEndpoint,
		HcpPromEndpoint:   hcpPromEndpoint,
		cred:              cred,
		knownIssues:       knownIssues,
	}}, nil
}

// alertsOutput is written to alerts.json and passed to the HTML template.
type alertsSummary struct {
	Total      int                                  `json:"total"`
	Known      int                                  `json:"known"`
	Unknown    int                                  `json:"unknown"`
	BySeverity map[armalertsmanagement.Severity]int `json:"bySeverity"`
}

type alertsOutput struct {
	Scope      string `json:"scope"`
	TimeWindow struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"timeWindow"`
	Summary alertsSummary `json:"summary"`
	Alerts  []alert       `json:"alerts"`
}

// Template helpers for the HTML template.
func (o alertsOutput) SeverityCounts() map[armalertsmanagement.Severity]int {
	return o.Summary.BySeverity
}
func (o alertsOutput) HasAlerts() bool        { return o.Summary.Total > 0 }
func (o alertsOutput) HasUnknownAlerts() bool { return o.Summary.Unknown > 0 }
func (o alertsOutput) KnownCount() int        { return o.Summary.Known }
func (o alertsOutput) UnknownCount() int      { return o.Summary.Unknown }

func (o Options) Run(ctx context.Context) error {
	logger, err := logr.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("logger not found in context: %w", err)
	}

	allAlerts, err := fetchAlerts(ctx, o.cred, o.Scope, o.TimeWindow.Start, o.TimeWindow.End)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to fetch alerts for scope %s, time window [%s to %s]: %w",
			o.Scope,
			o.TimeWindow.Start.Format(time.RFC3339), o.TimeWindow.End.Format(time.RFC3339), err))
	}

	alerts := filterAlertsBySeverity(allAlerts, o.SeverityThreshold)
	if o.SeverityThreshold >= 0 {
		logger.Info("filtered alerts by severity threshold", "threshold", fmt.Sprintf("Sev%d", o.SeverityThreshold), "before", len(allAlerts), "after", len(alerts))
	}

	alerts = classifyAlerts(alerts, o.knownIssues)

	// Build output used for both JSON and HTML
	severityCounts := map[armalertsmanagement.Severity]int{}
	var knownCount int
	for _, a := range alerts {
		severityCounts[a.Alert.Severity]++
		if a.Metadata.KnownIssue {
			knownCount++
		}
	}
	unknownCount := len(alerts) - knownCount

	logger.Info("classified alerts", "known", knownCount, "unknown", unknownCount)

	output := alertsOutput{
		Scope:  o.Scope,
		Alerts: alerts,
		Summary: alertsSummary{
			Total:      len(alerts),
			Known:      knownCount,
			Unknown:    unknownCount,
			BySeverity: severityCounts,
		},
	}
	output.TimeWindow.Start = o.TimeWindow.Start.UTC().Format(time.RFC3339)
	output.TimeWindow.End = o.TimeWindow.End.UTC().Format(time.RFC3339)

	jsonPath := filepath.Join(o.OutputDir, "alerts.json")
	jsonData, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to marshal alerts to JSON: %w", err))
	}
	if err := os.WriteFile(jsonPath, jsonData, 0644); err != nil {
		return utils.TrackError(fmt.Errorf("failed to write %s: %w", jsonPath, err))
	}
	logger.Info("wrote alert JSON artifact", "path", jsonPath, "alerts", len(alerts))

	// Render HTML artifact
	htmlPath := filepath.Join(o.OutputDir, "alerts-summary.html")
	if err := renderTemplate(htmlPath, output); err != nil {
		return utils.TrackError(fmt.Errorf("failed to render alerts HTML: %w", err))
	}
	logger.Info("wrote alert HTML artifact", "path", htmlPath)

	// Execute PromQL queries and render timeseries charts with alert overlays
	if o.Queries != nil {
		if err := o.runQueries(ctx); err != nil {
			return utils.TrackError(fmt.Errorf("PromQL query execution failed: %w", err))
		}
	}

	return nil
}

func (o Options) runQueries(ctx context.Context) error {
	logger, err := logr.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("logger not found in context: %w", err)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}

	for _, panel := range o.Queries.Panels {
		logger.Info("executing panel queries", "panel", panel.Title, "queries", len(panel.Queries))

		var panelCharts []chartData
		for _, q := range panel.Queries {
			endpoint := resolveWorkspaceEndpoint(q, o.SvcPromEndpoint, o.HcpPromEndpoint)

			logger.Info("executing PromQL query", "panel", panel.Title, "title", q.Title, "workspace", q.Workspace)

			var results []PrometheusResult
			var queryErr string
			resp, err := queryRange(ctx, httpClient, o.cred, endpoint, q.Query, o.TimeWindow.Start, o.TimeWindow.End, q.Step)
			if err != nil {
				logger.Error(err, "PromQL query failed", "title", q.Title)
				queryErr = err.Error()
			} else {
				results = resp.Data.Result
			}

			panelCharts = append(panelCharts, buildChartData(q.Title, q.Description, q.Query, queryErr, results, o.TimeWindow))
		}

		// filename must match the Spyglass HTML lens regex .*-summary.*\.html
		// so that Prow renders it inline in the job UI.
		fileName := fmt.Sprintf("panel-%s-summary.html", sanitizeTitle(panel.Title))
		panelPath := filepath.Join(o.OutputDir, fileName)

		pageData := panelPageData{Title: panel.Title, Charts: panelCharts}
		pageData.TimeWindow.Start = o.TimeWindow.Start.UTC().Format(time.RFC3339)
		pageData.TimeWindow.End = o.TimeWindow.End.UTC().Format(time.RFC3339)

		if err := renderPanel(panelPath, pageData); err != nil {
			logger.Error(err, "failed to render panel", "panel", panel.Title)
			continue
		}
		logger.Info("wrote panel", "path", panelPath, "charts", len(panelCharts))
	}
	return nil
}

// sanitizeTitle converts a title to a lowercase kebab-case string suitable for
// use in file names.
func sanitizeTitle(title string) string {
	title = strings.ToLower(title)
	title = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, title)
	// collapse multiple dashes
	for strings.Contains(title, "--") {
		title = strings.ReplaceAll(title, "--", "-")
	}
	return strings.Trim(title, "-")
}

func renderTemplate(outputPath string, data any) error {
	funcMap := template.FuncMap{
		"formatTime": func(t *time.Time) string {
			if t == nil {
				return "-"
			}
			return t.UTC().Format("2006-01-02 15:04:05")
		},
		"severityClass": severityCSSClass,
		"conditionClass": func(s string) string {
			switch s {
			case "Fired":
				return "condition-fired"
			case "Resolved":
				return "condition-resolved"
			default:
				return ""
			}
		},
		"label": func(labels map[string]string, key string) string {
			return labels[key]
		},
		"annotation": func(annotations map[string]string, key string) string {
			return annotations[key]
		},
		"relativeTime": func(windowStart string, t *time.Time) string {
			if t == nil {
				return ""
			}
			start, err := time.Parse(time.RFC3339, windowStart)
			if err != nil {
				return ""
			}
			minutes := int(t.Sub(start).Minutes())
			if minutes < 0 {
				return fmt.Sprintf("T%dm", minutes)
			}
			return fmt.Sprintf("T+%dm", minutes)
		},
	}

	tmplContent := mustReadArtifact("alerts.html.tmpl")
	tmpl, err := template.New("alerts").Funcs(funcMap).Parse(string(tmplContent))
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}
	if err := os.WriteFile(outputPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write %s: %w", outputPath, err)
	}
	return nil
}

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

// Package cmd wires the cluster-utilization CLI (collect, process, render, all).
package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/Azure/ARO-HCP/tooling/cluster-utilization/internal/collect"
	"github.com/Azure/ARO-HCP/tooling/cluster-utilization/internal/grafana"
	"github.com/Azure/ARO-HCP/tooling/cluster-utilization/internal/process"
	"github.com/Azure/ARO-HCP/tooling/cluster-utilization/internal/render"
)

// NewRootCommand builds the cluster-utilization command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "cluster-utilization",
		Short: "Report where ARO-HCP cluster compute goes (capacity, requests, usage)",
		Long: `cluster-utilization builds a "what we use our clusters for" report.

It runs in three stages that share an on-disk cache:

  collect  query every matching Grafana Prometheus datasource for node capacity,
           pod/container requests & limits, and sustained usage; write one raw
           cache file per cluster.
  process  read the cache offline, roll pods up into workloads (deployment/
           statefulset/daemonset/...), normalize similar namespaces (ocm-*,
           klusterlet-*), and emit a normalized analysis report.
  render   turn the report into a single self-contained HTML page.

The 'all' subcommand runs collect, then process, then render.

Authentication uses your ambient Azure credentials (az login / managed identity)
scoped to the Azure Managed Grafana service application.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newCollectCmd())
	root.AddCommand(newProcessCmd())
	root.AddCommand(newRenderCmd())
	root.AddCommand(newAllCmd())
	return root
}

type collectFlags struct {
	grafanaURL string
	opts       collect.Options
}

func addCollectFlags(cmd *cobra.Command, f *collectFlags) {
	cmd.Flags().StringVar(&f.grafanaURL, "grafana-url", "", "Azure Managed Grafana base URL (required)")
	cmd.Flags().StringVar(&f.opts.CacheDir, "cache-dir", ".cache", "directory for raw per-cluster cache files")
	cmd.Flags().StringVar(&f.opts.DatasourcePattern, "datasource-pattern", "^(services|hcps)-", "regexp; only query datasources whose uid matches")
	cmd.Flags().StringVar(&f.opts.Window, "window", "14d", "lookback window for the peak search and all-instance aggregation")
	cmd.Flags().StringVar(&f.opts.Step, "step", "5m", "subquery resolution step")
	cmd.Flags().Float64Var(&f.opts.Percentile, "percentile", 0.95, "per-pod OVER TIME statistic (0<p<1 => quantile; 0 or >=1 => raw max)")
	cmd.Flags().IntVar(&f.opts.NamespaceBatch, "namespace-batch", 20, "max namespaces per usage subquery (auto-halved on timeout; 0 = one batch)")
	cmd.Flags().IntVar(&f.opts.Concurrency, "concurrency", 1, "regions collected in parallel (1 = serial; keep low to avoid datasource rate limits)")
	cmd.Flags().DurationVar(&f.opts.DatasourceTimeout, "datasource-timeout", 45*time.Minute, "per-region time budget; a slow/unreachable region is abandoned")
	cmd.Flags().BoolVar(&f.opts.Refresh, "refresh", false, "re-query datasources even when a fresh cache file exists")
}

func newGrafanaClient(grafanaURL string) (*grafana.Client, error) {
	// The Azure SDK requires AZURE_TOKEN_CREDENTIALS to be set when we ask for
	// token-only credentials. Default it to the local dev chain (az login) so
	// running the tool interactively "just works".
	if os.Getenv("AZURE_TOKEN_CREDENTIALS") == "" {
		_ = os.Setenv("AZURE_TOKEN_CREDENTIALS", "dev")
	}
	cred, err := azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{
		RequireAzureTokenCredentials: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to obtain Azure credentials: %w", err)
	}
	return grafana.New(grafanaURL, cred)
}

func newCollectCmd() *cobra.Command {
	f := &collectFlags{}
	cmd := &cobra.Command{
		Use:   "collect",
		Short: "Query Grafana datasources and write raw per-cluster cache files",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			log := logr.FromContextOrDiscard(ctx)
			gc, err := newGrafanaClient(f.grafanaURL)
			if err != nil {
				return err
			}
			f.opts.GrafanaURL = gc.BaseURL()
			return collect.Run(ctx, log, gc, f.opts)
		},
	}
	addCollectFlags(cmd, f)
	_ = cmd.MarkFlagRequired("grafana-url")
	return cmd
}

func newProcessCmd() *cobra.Command {
	var opts process.Options
	cmd := &cobra.Command{
		Use:   "process",
		Short: "Read the cache, roll up workloads, normalize namespaces, emit report JSON",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := logr.FromContextOrDiscard(cmd.Context())
			return process.Run(log, opts)
		},
	}
	cmd.Flags().StringVar(&opts.CacheDir, "cache-dir", ".cache", "directory holding raw per-cluster cache files")
	cmd.Flags().StringVar(&opts.Output, "output", "report.json", "path to write the normalized report JSON")
	cmd.Flags().StringVar(&opts.NormalizeRules, "normalize-rules", "", "optional YAML file of namespace normalization rules (merged ahead of the built-in ocm-*/klusterlet-* defaults)")
	return cmd
}

func newRenderCmd() *cobra.Command {
	var opts render.Options
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render the report JSON into a self-contained HTML page",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := logr.FromContextOrDiscard(cmd.Context())
			return render.Run(log, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Input, "input", "report.json", "path to the normalized report JSON")
	cmd.Flags().StringVar(&opts.Output, "output", "report.html", "path to write the self-contained HTML page")
	return cmd
}

func newAllCmd() *cobra.Command {
	f := &collectFlags{}
	var reportJSON, reportHTML, normalizeRules string
	cmd := &cobra.Command{
		Use:   "all",
		Short: "Run collect, then process, then render",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			log := logr.FromContextOrDiscard(ctx)
			gc, err := newGrafanaClient(f.grafanaURL)
			if err != nil {
				return err
			}
			f.opts.GrafanaURL = gc.BaseURL()
			if err := collect.Run(ctx, log, gc, f.opts); err != nil {
				return err
			}
			if err := process.Run(log, process.Options{
				CacheDir: f.opts.CacheDir, Output: reportJSON, NormalizeRules: normalizeRules,
			}); err != nil {
				return err
			}
			return render.Run(log, render.Options{Input: reportJSON, Output: reportHTML})
		},
	}
	addCollectFlags(cmd, f)
	cmd.Flags().StringVar(&reportJSON, "report-json", "report.json", "intermediate report JSON path")
	cmd.Flags().StringVar(&reportHTML, "output", "report.html", "final self-contained HTML path")
	cmd.Flags().StringVar(&normalizeRules, "normalize-rules", "", "optional YAML file of namespace normalization rules")
	_ = cmd.MarkFlagRequired("grafana-url")
	return cmd
}

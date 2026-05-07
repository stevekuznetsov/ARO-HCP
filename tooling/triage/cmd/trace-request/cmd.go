// Copyright 2026 Microsoft Corporation
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

package tracerequest

import (
	"context"

	"github.com/spf13/cobra"
)

func NewCommand() (*cobra.Command, error) {
	opts := &RawTraceRequestOptions{}
	cmd := &cobra.Command{
		Use:           "trace-request",
		Short:         "Trace all data related to an ARM request through the system.",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return traceRequest(cmd.Context(), opts)
		},
	}
	bindTraceRequestOptions(opts, cmd)
	return cmd, nil
}

func bindTraceRequestOptions(opts *RawTraceRequestOptions, cmd *cobra.Command) {
	cmd.Flags().StringVar(&opts.ClusterName, "cluster-name", opts.ClusterName, "Kusto cluster name.")
	cmd.Flags().StringVar(&opts.Region, "region", opts.Region, "Azure region where the Kusto cluster is located (e.g. westus3).")
	cmd.Flags().StringVar(&opts.HCPDatabase, "hcp-database", opts.HCPDatabase, "Kusto database for hosted control plane logs.")
	cmd.Flags().StringVar(&opts.ServiceDatabase, "service-database", opts.ServiceDatabase, "Kusto database for service logs.")
	cmd.Flags().StringVar(&opts.StartTime, "start-time", opts.StartTime, "Query start time in RFC3339 format (e.g. 2024-01-15T10:30:00Z).")
	cmd.Flags().StringVar(&opts.EndTime, "end-time", opts.EndTime, "Query end time in RFC3339 format (e.g. 2024-01-16T10:30:00Z).")
	cmd.Flags().StringVar(&opts.CorrelationID, "correlation-id", opts.CorrelationID, "ARM correlation ID for the request to trace.")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "Directory to write query results into.")
}

func traceRequest(ctx context.Context, opts *RawTraceRequestOptions) error {
	validated, err := opts.Validate()
	if err != nil {
		return err
	}
	completed, err := validated.Complete(ctx)
	if err != nil {
		return err
	}
	return completed.Run(ctx)
}

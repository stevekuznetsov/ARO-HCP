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

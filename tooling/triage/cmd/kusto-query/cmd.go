package kustoquery

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/Azure/ARO-HCP/tooling/triage/pkg/kql"
)

func NewCommand() (*cobra.Command, error) {
	opts := &kql.RawTemplateOptions{}
	cmd := &cobra.Command{
		Use:           "kusto-query",
		Short:         "Run a templated KQL query against a Kusto cluster.",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return kustoQuery(cmd.Context(), opts)
		},
	}
	kql.BindTemplateOptions(opts, cmd)
	return cmd, nil
}

func kustoQuery(ctx context.Context, opts *kql.RawTemplateOptions) error {
	validated, err := opts.Validate()
	if err != nil {
		return err
	}
	completed, err := complete(ctx, validated)
	if err != nil {
		return err
	}
	return completed.Run(ctx)
}

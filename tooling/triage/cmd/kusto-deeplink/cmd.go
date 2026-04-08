package kustodeeplink

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/Azure/ARO-HCP/tooling/triage/pkg/kql"
)

func NewCommand() (*cobra.Command, error) {
	opts := &kql.RawTemplateOptions{}
	cmd := &cobra.Command{
		Use:           "kusto-deeplink",
		Short:         "Generate an Azure Data Explorer deep-link for a templated KQL query.",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return kustoDeeplink(cmd.Context(), opts)
		},
	}
	kql.BindTemplateOptions(opts, cmd)
	return cmd, nil
}

func kustoDeeplink(ctx context.Context, opts *kql.RawTemplateOptions) error {
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

package failingtests

import (
	"context"

	"github.com/spf13/cobra"
)

func NewCommand() (*cobra.Command, error) {
	opts := DefaultFailingTestsOptions()
	cmd := &cobra.Command{
		Use:           "failing-tests",
		Short:         "List failing tests for an environment over a time period.",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return failingTests(cmd.Context(), opts)
		},
	}
	if err := BindFailingTestsOptions(opts, cmd); err != nil {
		return nil, err
	}
	return cmd, nil
}

func failingTests(ctx context.Context, opts *RawFailingTestsOptions) error {
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

package jobfailures

import (
	"context"

	"github.com/spf13/cobra"
)

func NewCommand() (*cobra.Command, error) {
	opts := DefaultJobFailuresOptions()
	cmd := &cobra.Command{
		Use:           "job-failures",
		Short:         "Download failing test artifacts for a single Prow job run.",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return jobFailures(cmd.Context(), opts)
		},
	}
	if err := BindJobFailuresOptions(opts, cmd); err != nil {
		return nil, err
	}
	return cmd, nil
}

func jobFailures(ctx context.Context, opts *RawJobFailuresOptions) error {
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

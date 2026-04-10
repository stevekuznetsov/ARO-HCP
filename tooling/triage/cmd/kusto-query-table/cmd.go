package kustoquerytable

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/ARO-HCP/tooling/triage/pkg/kql"
)

func NewCommand() (*cobra.Command, error) {
	var inputFile string
	cmd := &cobra.Command{
		Use:           "kusto-query-table",
		Short:         "Render a Kusto v2 JSON response as a markdown table.",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(inputFile)
		},
	}
	cmd.Flags().StringVar(&inputFile, "input", "", "Path to a file containing the raw Kusto v2 JSON response.")
	return cmd, nil
}

func run(inputFile string) error {
	if inputFile == "" {
		return fmt.Errorf("--input is required")
	}
	data, err := os.ReadFile(inputFile)
	if err != nil {
		return fmt.Errorf("failed to read input file %q: %w", inputFile, err)
	}
	columns, rows := kql.ParsePrimaryResult(data)
	if len(columns) == 0 {
		return fmt.Errorf("no PrimaryResult data found in %q", inputFile)
	}
	fmt.Print(kql.RenderMarkdownTable(columns, rows))
	return nil
}

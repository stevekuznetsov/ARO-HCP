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

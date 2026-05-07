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

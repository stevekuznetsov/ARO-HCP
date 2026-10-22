// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dashboard

import (
	"errors"

	"github.com/spf13/cobra"
)

// NewCommand leaves root dispatch and error reporting to the existing CLI.
func NewCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "dashboard", Short: "Export and render offline performance dashboards",
		SilenceUsage: true, SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		Args:              cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("dashboard requires export, render or overview; use --help")
		},
	}
	for _, verb := range []struct {
		name, short string
		mode        CommandMode
	}{
		{"export", "Summarize explicitly reviewed private evidence into portable JSON", Export},
		{"render", "Render portable JSON as self-contained HTML without reading sources", Render},
		{"overview", "Combine reviewed portable datasets into an offline overview", Overview},
	} {
		raw := NewRawOptions(verb.mode)
		child := &cobra.Command{
			Use: verb.name, Short: verb.short, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				validated, err := raw.Validate(cmd.Context())
				if err != nil {
					return err
				}
				options, err := validated.Complete(cmd.Context())
				if err != nil {
					return err
				}
				return options.Run(cmd.Context())
			},
		}
		raw.BindOptions(child.Flags())
		command.AddCommand(child)
	}
	return command
}

// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iteration

import (
	"errors"

	"github.com/spf13/cobra"
)

// NewCommand bridges the existing CLI to Cobra without taking over root dispatch
// or error reporting. The caller supplies context, arguments and output writers.
func NewCommand() *cobra.Command {
	command := &cobra.Command{
		Use:           "iteration",
		Short:         "Manage durable, typed iterations",
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("iteration requires a subcommand; use --help")
		},
	}
	for _, verb := range []struct {
		name   string
		short  string
		mode   CommandMode
		hidden bool
	}{
		{name: "submit", short: "Validate, freeze and queue an iteration without dispatching it", mode: Submit},
		{name: "status", short: "Read one run, or list all runs when --id is omitted", mode: Status},
		{name: "list", short: "List runs from the local journal", mode: List},
		{name: "watch", short: "Observe a run until success, failure, stop or uncertainty", mode: Watch},
		{name: "stop", short: "Persist cancellation intent; does not roll back remote work", mode: Stop},
		{name: "reconcile", short: "Reconcile one run or all runs, potentially dispatching workers", mode: Reconcile},
		{name: "reconcile-operation", short: "Observe an accepted RP operation independently of automatic attempt limits", mode: ReconcileOperation},
		{name: "serve", short: "Continuously reconcile; shutdown does not stop workers", mode: Serve},
		{name: "worker", short: "Execute a controller-owned attempt (internal)", mode: Worker, hidden: true},
		{name: "retry-observation", short: "Replace only a startup observer with this executable; preserve remote operations", mode: RetryObservation},
	} {
		raw := NewRawOptions(verb.mode)
		child := &cobra.Command{
			Use:    verb.name,
			Short:  verb.short,
			Hidden: verb.hidden,
			Args:   cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				raw.Out = cmd.OutOrStdout()
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

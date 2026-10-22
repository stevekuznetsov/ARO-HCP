// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/scan"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

// runScan accepts arguments after "scan". Parent command routing is separate.
func runScan(args []string, out io.Writer) error {
	const usage = "usage: scan plan --profile FILE --input FILE"
	if len(args) == 0 || args[0] != "plan" {
		return fmt.Errorf("%s", usage)
	}
	flags := flag.NewFlagSet("scan plan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	input := flags.String("input", "", "scan specification JSON")
	profilePath := flags.String("profile", "", "existing sizing profile JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *input == "" || *profilePath == "" || flags.NArg() != 0 {
		return fmt.Errorf("%s", usage)
	}
	var spec scan.Spec
	if err := decodeFile(*input, &spec); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	var profile sizing.Profile
	if err := decodeFile(*profilePath, &profile); err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	result, err := scan.Plan(spec, profile)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

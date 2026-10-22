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

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/cost"
)

func runCost(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "estimate" {
		return fmt.Errorf("usage: cost estimate --input FILE")
	}
	flags := flag.NewFlagSet("cost estimate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	input := flags.String("input", "", "cost specification JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *input == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: cost estimate --input FILE")
	}
	var spec cost.Spec
	if err := decodeFile(*input, &spec); err != nil {
		return err
	}
	result, err := cost.Calculate(spec)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

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
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/card"
)

// runCard receives arguments after the parent "card" command, like runCost.
// The output path must be new; no source evidence is automatically read.
func runCard(args []string, out io.Writer) error {
	const usage = "usage: card render --input CARD_JSON [--contention CONTENTION_JSON] [--resources RESOURCES_JSON] [--experience FILE] --output NEW_HTML"
	if len(args) == 0 || args[0] != "render" {
		return fmt.Errorf("%s", usage)
	}
	f := flag.NewFlagSet("card render", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	input := f.String("input", "", "reviewed portable card JSON")
	contentionPath := f.String("contention", "", "reviewed portable contention sidecar JSON")
	resourcesPath := f.String("resources", "", "reviewed portable per-container resource sidecar JSON")
	experiencePath := f.String("experience", "", "reviewed portable client and canary experience JSON")
	output := f.String("output", "", "new standalone HTML file; parent must exist; never overwritten")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if *input == "" || *output == "" || f.NArg() != 0 {
		return fmt.Errorf("%s", usage)
	}
	read := func(path string, maxBytes int64) ([]byte, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > maxBytes {
			return nil, fmt.Errorf("%s must be a regular JSON file of at most %d bytes", path, maxBytes)
		}
		return io.ReadAll(io.LimitReader(file, maxBytes+1))
	}
	data, err := read(*input, card.MaxInputBytes)
	if err != nil {
		return err
	}
	c, err := card.Parse(data)
	if err != nil {
		return fmt.Errorf("input: %w", err)
	}
	var contention *card.Contention
	if *contentionPath != "" {
		data, err := read(*contentionPath, card.MaxInputBytes)
		if err != nil {
			return err
		}
		contention, err = card.ParseContention(data)
		if err != nil {
			return fmt.Errorf("contention: %w", err)
		}
	}
	var resources *card.Resources
	if *resourcesPath != "" {
		data, err := read(*resourcesPath, card.MaxResourcesBytes)
		if err != nil {
			return err
		}
		resources, err = card.ParseResources(data)
		if err != nil {
			return fmt.Errorf("resources: %w", err)
		}
	}
	var experience *card.Experience
	if *experiencePath != "" {
		data, err := read(*experiencePath, card.MaxExperienceBytes)
		if err != nil {
			return fmt.Errorf("experience: %w", err)
		}
		experience, err = card.ParseExperience(data)
		if err != nil {
			return fmt.Errorf("experience: %w", err)
		}
	}
	var html bytes.Buffer
	if err := card.RenderWithEvidence(&html, c, contention, resources, experience); err != nil {
		return err
	}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(html.Bytes())
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return json.NewEncoder(out).Encode(struct {
		Output string `json:"output"`
		Bytes  int    `json:"bytes"`
	}{Output: *output, Bytes: written})
}

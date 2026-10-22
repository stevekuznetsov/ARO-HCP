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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/provision"
)

func runRP(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 2 || args[0] != "rp" {
		return fmt.Errorf("rp requires create-cluster, create-nodepool, get, wait, delete, or credentials")
	}
	command := args[1]
	flags := flag.NewFlagSet("rp "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	input := flags.String("input", "", "RP connection JSON")
	resource := flags.String("resource", "", "resource reference JSON")
	output := flags.String("output", "", "new private artifacts directory")
	timeout := flags.Duration("timeout", time.Hour, "operation timeout, at most 24h")
	var payloadPath, credentialPath, operationURL string
	var confirmed bool
	switch command {
	case "create-cluster", "create-nodepool":
		flags.StringVar(&payloadPath, "payload", "", "raw resource JSON object")
	case "credentials":
		flags.StringVar(&credentialPath, "credentials-output", "", "new private admin kubeconfig file")
	case "delete":
		flags.BoolVar(&confirmed, "confirm-delete", false, "explicitly confirm deletion of this resource")
	case "get":
	case "wait":
		flags.StringVar(&operationURL, "operation-url", "", "existing RP operation status URL; no request is resubmitted")
		flags.StringVar(output, "outputdir", "", "new private artifacts directory (alias for --output)")
	default:
		return fmt.Errorf("unknown rp command %q", command)
	}
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *input == "" || *resource == "" || *output == "" {
		return fmt.Errorf("require --input, --resource, --output and no positional arguments")
	}
	if *timeout <= 0 || *timeout > 24*time.Hour {
		return fmt.Errorf("timeout must be positive and at most 24h")
	}
	if command == "delete" && !confirmed {
		return fmt.Errorf("delete requires --confirm-delete=true")
	}
	if command == "credentials" && credentialPath == "" {
		return fmt.Errorf("credentials requires --credentials-output")
	}
	if command == "wait" && operationURL == "" {
		return fmt.Errorf("wait requires --operation-url")
	}
	var config provision.Config
	if err := decodeFile(*input, &config); err != nil {
		return fmt.Errorf("RP config: %w", err)
	}
	var ref provision.Resource
	if err := decodeFile(*resource, &ref); err != nil {
		return fmt.Errorf("resource: %w", err)
	}
	var payload json.RawMessage
	if command == "create-cluster" || command == "create-nodepool" {
		if err := decodeFile(payloadPath, &payload); err != nil {
			return fmt.Errorf("payload: %w", err)
		}
		if !bytes.HasPrefix(bytes.TrimSpace(payload), []byte("{")) {
			return fmt.Errorf("payload must be a JSON object")
		}
	}
	// The package bounds requests and rejects redirects; no bearer transport is used.
	client, err := provision.New(config, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var result provision.Result
	switch command {
	case "create-cluster":
		result, err = client.CreateCluster(ctx, ref, payload, *output)
	case "create-nodepool":
		result, err = client.CreateNodePool(ctx, ref, payload, *output)
	case "get":
		result, err = client.Inspect(ctx, ref, *output)
	case "wait":
		result, err = client.Resume(ctx, ref, operationURL, *output)
	case "delete":
		result, err = client.Delete(ctx, ref, *output, confirmed)
	case "credentials":
		result, err = client.RequestAdminCredential(ctx, ref, *output, credentialPath)
	}
	return errors.Join(err, json.NewEncoder(out).Encode(result))
}

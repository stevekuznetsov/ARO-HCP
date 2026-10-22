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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/scrape"
)

// runScrape accepts the full command arguments, including "scrape" and its verb.
func runScrape(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("scrape requires a subcommand: generate or check")
	}
	if args[0] != "scrape" || (args[1] != "generate" && args[1] != "check") {
		return fmt.Errorf("unknown scrape command %q", args[:2])
	}
	f := flag.NewFlagSet("scrape "+args[1], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var input, kubeconfig, output, endpoint string
	defaultTimeout := time.Minute
	if args[1] == "generate" {
		f.StringVar(&input, "input", "", "scrape specification JSON")
		f.StringVar(&kubeconfig, "kubeconfig", "", "management kubeconfig; omitted uses in-cluster credentials")
		f.StringVar(&output, "output", "", "new directory for prometheus.yaml and bundle.json")
		defaultTimeout = 5 * time.Minute
	} else {
		f.StringVar(&input, "input", "", "generated bundle.json")
		f.StringVar(&endpoint, "endpoint", "", "Prometheus base URL")
	}
	timeout := f.Duration("timeout", defaultTimeout, "total operation timeout")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", f.Args())
	}
	if *timeout <= 0 || *timeout > 24*time.Hour {
		return fmt.Errorf("timeout must be positive and at most 24h")
	}
	if input == "" {
		return fmt.Errorf("--input is required")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var result any
	var operationErr error
	if args[1] == "generate" {
		if output == "" {
			return fmt.Errorf("--output is required")
		}
		var spec scrape.Spec
		if err := decodeFile(input, &spec); err != nil {
			return fmt.Errorf("scrape spec: %w", err)
		}
		var config *rest.Config
		var err error
		if kubeconfig == "" {
			config, err = rest.InClusterConfig()
		} else {
			config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		}
		if err != nil {
			return fmt.Errorf("load management credentials (supply --kubeconfig outside a cluster): %w", err)
		}
		config.Timeout = min(*timeout, 30*time.Second)
		config.UserAgent = "hcp-perf/v1"
		client, err := kubernetes.NewForConfig(config)
		if err != nil {
			return err
		}
		dynamicClient, err := dynamic.NewForConfig(config)
		if err != nil {
			return err
		}
		if err := os.Mkdir(output, 0700); err != nil {
			return fmt.Errorf("create bundle directory: %w", err)
		}
		bundle, err := scrape.Generate(ctx, client, dynamicClient, spec)
		operationErr = err
		if err != nil {
			// Persist API/validation failures as blockers so a later check cannot pass.
			bundle.Gaps = append(bundle.Gaps, scrape.Gap{Source: "generate", Message: err.Error()})
		}
		if len(bundle.Gaps) > 0 {
			operationErr = errors.Join(operationErr, fmt.Errorf("scrape bundle has %d blockers; incomplete bundle preserved in %s", len(bundle.Gaps), output))
		}
		data, err := json.MarshalIndent(bundle, "", "  ")
		operationErr = errors.Join(operationErr, err)
		if err == nil {
			// Keep partial evidence on failure; never replace an existing file or directory.
			for _, artifact := range []struct {
				name string
				data []byte
			}{{"bundle.json", append(data, '\n')}, {"prometheus.yaml", []byte(bundle.Config)}} {
				file, err := os.OpenFile(filepath.Join(output, artifact.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if err == nil {
					_, err = file.Write(artifact.data)
					err = errors.Join(err, file.Close())
				}
				if err != nil {
					operationErr = errors.Join(operationErr, fmt.Errorf("write %s: %w", artifact.name, err))
				}
			}
		}
		result = bundle
	} else {
		if endpoint == "" {
			return fmt.Errorf("--endpoint is required")
		}
		data, err := os.ReadFile(input)
		if err != nil {
			return err
		}
		// Bundles are output artifacts, not specs: partial output can contain nulls.
		var bundle scrape.Bundle
		if err := json.Unmarshal(data, &bundle); err != nil {
			return fmt.Errorf("scrape bundle: %w", err)
		}
		client := &http.Client{
			Timeout:       *timeout,
			Transport:     bearerTransport{token: os.Getenv("HCP_PERF_BEARER_TOKEN")},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		}
		result, operationErr = bundle.CheckTargetsWithClient(ctx, client, endpoint)
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return errors.Join(operationErr, encoder.Encode(result))
}

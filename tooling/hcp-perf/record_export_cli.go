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
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/record"
)

func runRecordExport(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 2 || args[0] != "record" {
		return fmt.Errorf("expected record export, archive or hold")
	}
	f := flag.NewFlagSet("record "+args[1], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	switch args[1] {
	case "archive":
		input := f.String("input", "", "quiescent recording directory containing only known recorder files")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *input == "" {
			return fmt.Errorf("record archive requires --input and no positional arguments")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Stdout is exclusively the tar stream, including on failure. The caller
		// reports errors on stderr; WriteArchive enforces the filename allowlist.
		return record.WriteArchive(*input, out)
	case "hold":
		duration := f.Duration("duration", 0, "helper lifetime, positive and at most 5m")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *duration <= 0 || *duration > 5*time.Minute {
			return fmt.Errorf("record hold requires --duration positive and at most 5m, and no positional arguments")
		}
		err := record.Wait(ctx, *duration)
		status := "completed"
		if err != nil {
			status = "failed"
		}
		return errors.Join(err, json.NewEncoder(out).Encode(map[string]string{"version": "v1", "status": status}))
	case "export":
		namespace := f.String("namespace", "", "recorder namespace")
		id := f.String("id", "", "recorder session ID")
		kubeconfig := f.String("kubeconfig", "", "explicit management kubeconfig; omitted uses in-cluster credentials")
		output := f.String("output", "", "new private evidence directory; parent must exist")
		timeout := f.Duration("timeout", 10*time.Minute, "operation timeout; cancellation still permits bounded helper cleanup")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return fmt.Errorf("unexpected positional arguments")
		}
		if *timeout <= 0 || *timeout > 24*time.Hour {
			return fmt.Errorf("timeout must be positive and at most 24h")
		}
		if len(validation.IsDNS1123Label(*namespace)) != 0 || len(validation.IsDNS1123Label(*id)) != 0 || len(*id) > 40 {
			return fmt.Errorf("namespace must be an explicit DNS label and id a DNS label of 1..40 characters")
		}
		if *output == "" {
			return fmt.Errorf("record export requires --output")
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
		config, err := recordRESTConfig(*kubeconfig)
		if err != nil {
			return err
		}
		client, err := kubernetes.NewForConfig(config)
		if err != nil {
			return fmt.Errorf("create record export client: %w", err)
		}
		// Export performs best-effort, UID-guarded helper cleanup with a separate
		// timeout even after cancellation, and never deletes the evidence PVC.
		result, err := record.Export(ctx, client, config, *namespace, *id, *output)
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return errors.Join(err, encoder.Encode(result))
	default:
		return fmt.Errorf("unknown record export command %q", args[1])
	}
}

// Export needs the same TLS/authentication configuration for both API and exec.
func recordRESTConfig(path string) (*rest.Config, error) {
	var config *rest.Config
	var err error
	if path == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", path)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot load record export credentials; supply a valid --kubeconfig outside a cluster")
	}
	config.Timeout = 30 * time.Second
	config.UserAgent = "hcp-perf/v1"
	return config, nil
}

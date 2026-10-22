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
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/requesttrace"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func runTurnover(ctx context.Context, args []string, out io.Writer) (retErr error) {
	if len(args) < 2 || args[0] != "turnover" || args[1] != "run" {
		return fmt.Errorf("expected turnover run")
	}
	f := flag.NewFlagSet("turnover run", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	input := f.String("input", "", "turnover specification JSON")
	credentialsDir := f.String("credentials-dir", "", "directory containing <namespace>.kubeconfig for each namespace identity, never administrator credentials")
	output := f.String("output", "", "new private evidence directory; parent must exist")
	requestLog := f.String("request-log", "", "optional new private HTTP NDJSON file (10 MiB cap); parent must exist")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *input == "" || *credentialsDir == "" || *output == "" {
		return fmt.Errorf("turnover run requires --input, --credentials-dir and --output")
	}
	var spec workload.TurnoverSpec
	if err := decodeFile(*input, &spec); err != nil {
		return fmt.Errorf("turnover input: %w", err)
	}
	trace, err := requesttrace.Open(*requestLog)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, trace.Close()) }()
	// RunTurnover validates the prefix before calling the factory and bounds each
	// request context to 30s (or the remaining run window, whichever is shorter).
	result, err := workload.RunTurnover(ctx, spec, workload.TurnoverOptions{
		OutputDir: *output,
		Factory: func(namespace string) (kubernetes.Interface, error) {
			return turnoverNamespaceClient(*credentialsDir, namespace, trace)
		},
	})
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return errors.Join(err, encoder.Encode(struct {
		workload.TurnoverResult
		Warnings []string `json:"warnings"`
	}{result, []string{
		"Each <namespace>.kubeconfig must use that namespace's benchmark identity, not an administrator. Loading a kubeconfig does not verify server-side identity or RBAC. No administrator or ambient credential fallback is used.",
		"Turnover events record application-level attempts. Optional --request-log records allowlisted HTTP attempts through response headers, not body completion or client limiter waits; namespace scope is not verified identity. Recording errors invalidate harness evidence, not HCP performance.",
	}}))
}

func turnoverNamespaceClient(dir, namespace string, trace *requesttrace.Log) (kubernetes.Interface, error) {
	if dir == "" || len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, fmt.Errorf("namespace credentials require a directory and a DNS-label namespace")
	}
	path := filepath.Join(dir, namespace+".kubeconfig")
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		// Parser errors can include credential file contents. Report only the
		// exact expected file and namespace, never the underlying payload.
		return nil, fmt.Errorf("cannot load credentials for namespace %q from %q; require a valid namespace-identity kubeconfig", namespace, path)
	}
	config.Timeout = 30 * time.Second
	config.UserAgent = "hcp-perf/v1"
	config.Wrap(trace.WrapTransport(namespace))
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("cannot create client for namespace %q from %q; check kubeconfig transport settings", namespace, path)
	}
	return client, nil
}

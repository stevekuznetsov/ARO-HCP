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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
)

func runAPILoad(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 2 || args[0] != "api-load" || args[1] != "run" {
		return fmt.Errorf("expected api-load run")
	}
	f := flag.NewFlagSet("api-load run", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	input := f.String("input", "", "API load specification JSON")
	credentialsDir := f.String("credentials-dir", "", "generated namespace ServiceAccount token <namespace>.kubeconfig files; no administrator or ambient fallback")
	output := f.String("output", "", "new private evidence directory; parent must exist")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *input == "" || *credentialsDir == "" || *output == "" {
		return fmt.Errorf("api-load run requires --input, --credentials-dir and --output, with no positional arguments")
	}
	var spec apiload.Spec
	if err := decodeFile(*input, &spec); err != nil {
		return fmt.Errorf("api-load input: %w", err)
	}
	result, err := apiload.Run(ctx, spec, apiload.Options{
		OutputDir: *output,
		Factory: func(ns string) (*rest.Config, error) {
			return apiLoadNamespaceConfig(*credentialsDir, ns)
		},
	})
	if result.Version == "" {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return errors.Join(err, enc.Encode(result))
}

func apiLoadNamespaceConfig(dir, namespace string) (*rest.Config, error) {
	if dir == "" || len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, fmt.Errorf("namespace credentials require a directory and DNS-label namespace")
	}
	raw, err := clientcmd.LoadFromFile(filepath.Join(dir, namespace+".kubeconfig"))
	if err != nil {
		// Kubeconfig parser errors may contain credentials. Never expose them.
		return nil, fmt.Errorf("cannot load namespace %q kubeconfig", namespace)
	}
	selected := raw.Contexts[raw.CurrentContext]
	if raw.CurrentContext == "" || selected == nil || selected.Namespace != namespace || selected.Cluster == "" || selected.AuthInfo == "" {
		return nil, fmt.Errorf("namespace %q kubeconfig requires an explicit current context with matching namespace, cluster and user", namespace)
	}
	cluster := raw.Clusters[selected.Cluster]
	if cluster == nil || cluster.Server == "" {
		return nil, fmt.Errorf("namespace %q kubeconfig requires an explicit cluster server", namespace)
	}
	server, err := url.Parse(cluster.Server)
	if err != nil || server.Scheme != "https" || server.Host == "" || server.User != nil || server.RawQuery != "" || server.Fragment != "" || server.RawPath != "" || cluster.InsecureSkipTLSVerify {
		return nil, fmt.Errorf("namespace %q kubeconfig requires a verified HTTPS server without userinfo, query, fragment, or escaped path", namespace)
	}
	for _, auth := range raw.AuthInfos {
		if auth != nil && (auth.Exec != nil || auth.AuthProvider != nil) {
			return nil, fmt.Errorf("namespace %q kubeconfig must not contain exec or auth-provider plugins", namespace)
		}
	}
	auth := raw.AuthInfos[selected.AuthInfo]
	if auth == nil || strings.TrimSpace(auth.Token) == "" || auth.TokenFile != "" ||
		auth.ClientCertificate != "" || len(auth.ClientCertificateData) != 0 || auth.ClientKey != "" || len(auth.ClientKeyData) != 0 ||
		auth.Username != "" || auth.Password != "" || auth.Impersonate != "" || auth.ImpersonateUID != "" || len(auth.ImpersonateGroups) != 0 || len(auth.ImpersonateUserExtra) != 0 {
		return nil, fmt.Errorf("namespace %q kubeconfig requires only an inline ServiceAccount token, without alternate authentication or impersonation", namespace)
	}
	// Direct configuration has no ambient loading rules, in-cluster fallback,
	// interactive reader, or credential persister. Never use the deferred loader.
	if err := clientcmd.ResolveLocalPaths(raw); err != nil {
		return nil, fmt.Errorf("cannot resolve namespace %q kubeconfig paths", namespace)
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(*raw, raw.CurrentContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot configure namespace %q kubeconfig", namespace)
	}
	return config, nil
}

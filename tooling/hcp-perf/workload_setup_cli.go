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
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/credentials"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

// runWorkloadSetup is a standalone entry point for the parent command dispatcher.
// Privileged setup never mints tokens; credentials is an explicit separate step.
func runWorkloadSetup(ctx context.Context, args []string, out io.Writer) error {
	return runWorkloadSetupWithClient(ctx, args, out, func(path string) (kubernetes.Interface, *rest.Config, error) {
		config, err := clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, nil, fmt.Errorf("load explicit guest admin kubeconfig")
		}
		config.Timeout = 30 * time.Second
		config.UserAgent = "hcp-perf/v1"
		client, err := kubernetes.NewForConfig(config)
		if err != nil {
			return nil, nil, fmt.Errorf("construct guest admin client")
		}
		return client, config, nil
	})
}

func runWorkloadSetupWithClient(ctx context.Context, args []string, out io.Writer, factory func(string) (kubernetes.Interface, *rest.Config, error)) error {
	if len(args) < 2 || args[0] != "workload" || (args[1] != "setup" && args[1] != "credentials") {
		return fmt.Errorf("expected workload setup or workload credentials")
	}
	command := args[1]
	f := flag.NewFlagSet("workload "+command, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	input := f.String("input", "", "workload setup specification JSON, matching the completed seed")
	kubeconfig := f.String("kubeconfig", "", "explicit guest admin kubeconfig; never used for measured workload traffic")
	timeout := f.Duration("timeout", 10*time.Minute, "operation timeout, positive and at most 10m")
	var output string
	var expiration time.Duration
	if command == "credentials" {
		f.StringVar(&output, "output", "", "new private canary kubeconfig file; parent must exist")
		f.DurationVar(&expiration, "expiration", 6*time.Hour, "requested token lifetime, whole seconds in [600s,24h]")
	}
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *input == "" || *kubeconfig == "" || *timeout <= 0 || *timeout > 10*time.Minute {
		return fmt.Errorf("require --input, --kubeconfig, timeout in (0,10m], and no positional arguments")
	}
	if command == "credentials" && (output == "" || expiration < 600*time.Second || expiration > 24*time.Hour || expiration%time.Second != 0) {
		return fmt.Errorf("credentials requires --output and expiration in whole seconds in [600s,24h]")
	}
	var spec workload.SetupSpec
	if err := decodeFile(*input, &spec); err != nil {
		return fmt.Errorf("workload setup input: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	client, config, err := factory(*kubeconfig)
	if err != nil {
		return fmt.Errorf("load explicit guest admin client")
	}
	if command == "setup" {
		result, err := workload.Configure(ctx, client, spec)
		if err != nil {
			// API errors can contain credentials. The safe partial inventory is
			// still emitted; never include server error bodies in CLI output.
			err = errors.Join(fmt.Errorf("workload setup failed; inspect created inventory and cluster before retrying"), ctx.Err())
		}
		return errors.Join(err, json.NewEncoder(out).Encode(result))
	}
	result, err := workloadCanaryCredentials(ctx, client, config, spec, output, expiration)
	return errors.Join(err, json.NewEncoder(out).Encode(result))
}

func workloadCanaryCredentials(ctx context.Context, client kubernetes.Interface, config *rest.Config, spec workload.SetupSpec, output string, expiration time.Duration) (result seedCredentialFile, err error) {
	if config == nil || client == nil || output == "" || expiration < 600*time.Second || expiration > 24*time.Hour || expiration%time.Second != 0 {
		return result, fmt.Errorf("require client, config, output and whole-second expiration in [600s,24h]")
	}
	if err := workload.VerifyCanaryIdentity(ctx, client, spec); err != nil {
		return result, errors.Join(fmt.Errorf("verify canary identity failed; complete workload setup first"), ctx.Err())
	}
	path, err := filepath.Abs(output)
	if err != nil {
		return result, err
	}
	// Reserve exclusively before minting a token, refusing symlinks and existing
	// files. On failure the empty/partial private file is retained, not retried.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	scoped, validUntil, err := credentials.MintCredentials(ctx, client, config, spec.CanaryNamespace(), workload.CanaryName, expiration)
	if err != nil {
		return result, err
	}
	guest := clientcmdapi.NewConfig()
	guest.Clusters["guest"] = &clientcmdapi.Cluster{Server: scoped.Host, CertificateAuthorityData: scoped.CAData, TLSServerName: scoped.ServerName, InsecureSkipTLSVerify: scoped.Insecure}
	guest.AuthInfos[workload.CanaryName] = &clientcmdapi.AuthInfo{Token: scoped.BearerToken}
	guest.Contexts[workload.CanaryName] = &clientcmdapi.Context{Cluster: "guest", AuthInfo: workload.CanaryName, Namespace: spec.CanaryNamespace()}
	guest.CurrentContext = workload.CanaryName
	data, err := clientcmd.Write(*guest)
	if err != nil {
		return result, fmt.Errorf("encode canary kubeconfig")
	}
	if _, err := file.Write(data); err != nil {
		return result, fmt.Errorf("write canary kubeconfig")
	}
	if err := file.Sync(); err != nil {
		return result, fmt.Errorf("sync canary kubeconfig")
	}
	return seedCredentialFile{Namespace: spec.CanaryNamespace(), Path: path, ValidUntil: validUntil}, nil
}

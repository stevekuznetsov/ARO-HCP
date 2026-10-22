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
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/credentials"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
)

func runSeed(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 2 || args[0] != "seed" {
		return fmt.Errorf("seed requires render, run, identities, or credentials")
	}
	command := args[1]
	flags := flag.NewFlagSet("seed "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	input := flags.String("input", "", "seed specification JSON")
	var output, kubeconfig, binary string
	var timeout, expiration time.Duration
	switch command {
	case "render", "run", "credentials":
		flags.StringVar(&output, "output", "", "new private output directory")
	case "identities":
	default:
		return fmt.Errorf("unknown seed command %q", command)
	}
	if command != "render" {
		flags.StringVar(&kubeconfig, "kubeconfig", "", "explicit guest admin kubeconfig")
		flags.DurationVar(&timeout, "timeout", 0, "operation timeout, at most 24h")
	}
	if command == "run" {
		flags.StringVar(&binary, "binary", "", "trusted executable path override; retains the input SHA-256 pin")
	}
	if command == "credentials" {
		flags.DurationVar(&expiration, "expiration", 6*time.Hour, "requested token lifetime, whole seconds in [600s,24h]")
	}
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *input == "" || (command != "identities" && output == "") {
		return fmt.Errorf("require --input, --output (except identities), and no positional arguments")
	}
	var flagErr error
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" && (timeout <= 0 || timeout > 24*time.Hour) {
			flagErr = fmt.Errorf("timeout must be positive and at most 24h")
		}
		if f.Name == "binary" && binary == "" {
			flagErr = fmt.Errorf("binary override must not be empty")
		}
	})
	if flagErr != nil {
		return flagErr
	}
	if command == "credentials" && (expiration < 600*time.Second || expiration > 24*time.Hour || expiration%time.Second != 0) {
		return fmt.Errorf("expiration must be whole seconds in [600s,24h]")
	}
	var spec seed.Spec
	if err := decodeFile(*input, &spec); err != nil {
		return fmt.Errorf("seed input: %w", err)
	}
	if binary != "" {
		spec.Binary.Path = binary
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if command != "render" && spec.Applications > 0 && kubeconfig == "" {
		return fmt.Errorf("require --kubeconfig for a nonempty seed")
	}
	if command != "render" {
		if timeout == 0 {
			timeout = 10 * time.Minute
			if command == "run" {
				timeout = time.Duration(spec.TimeoutSeconds) * time.Second
			}
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var result any
	var err error
	switch command {
	case "render":
		result, err = seed.Write(spec, output)
	case "run":
		result, err = seed.Run(ctx, spec, seed.Options{KubeconfigPath: kubeconfig, OutputDir: output})
	case "identities":
		var client kubernetes.Interface
		if spec.Applications > 0 {
			var clientErr error
			client, clientErr = managementClient(kubeconfig)
			if clientErr != nil {
				return fmt.Errorf("load guest admin client")
			}
		}
		err = seed.ConfigureIdentities(ctx, client, spec)
		if err != nil {
			// Kubernetes errors can contain server-supplied credential material.
			return errors.Join(fmt.Errorf("configure seed identities failed"), ctx.Err())
		}
		result = map[string]any{"version": "v1", "id": spec.ID, "namespaces": (spec.Applications + 19) / 20, "state": "completed"}
	case "credentials":
		result, err = seedCredentials(ctx, spec, kubeconfig, output, expiration)
	}
	return errors.Join(err, json.NewEncoder(out).Encode(result))
}

type seedCredentialFile struct {
	Namespace  string    `json:"namespace"`
	Path       string    `json:"path"`
	ValidUntil time.Time `json:"validUntil"`
}

type seedCredentialResult struct {
	Version           string               `json:"version"`
	ID                string               `json:"id"`
	ExpirationSeconds int64                `json:"expirationSeconds"`
	Files             []seedCredentialFile `json:"files"`
}

func seedCredentials(ctx context.Context, spec seed.Spec, kubeconfig, output string, expiration time.Duration) (result seedCredentialResult, err error) {
	if expiration < 600*time.Second || expiration > 24*time.Hour || expiration%time.Second != 0 {
		return result, fmt.Errorf("expiration must be whole seconds in [600s,24h]")
	}
	if err := spec.Validate(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return result, err
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return result, err
	}
	result = seedCredentialResult{Version: "v1", ID: spec.ID, ExpirationSeconds: int64(expiration / time.Second), Files: []seedCredentialFile{}}
	defer func() {
		data, marshalErr := json.MarshalIndent(result, "", "  ")
		err = errors.Join(err, marshalErr)
		if marshalErr == nil {
			err = errors.Join(err, writeSeedPrivate(filepath.Join(output, "credentials.json"), append(data, '\n')))
		}
	}()
	if spec.Applications == 0 {
		return result, nil
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return result, fmt.Errorf("load guest admin kubeconfig")
	}
	config.Timeout = 30 * time.Second
	config.UserAgent = "hcp-perf/v1"
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return result, fmt.Errorf("construct guest admin client")
	}
	for n := int64(0); n < (spec.Applications+19)/20; n++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		namespace := fmt.Sprintf("%s-%d", spec.NamespacePrefix, n)
		if err := readSeedIdentity(ctx, client, spec, n); err != nil {
			return result, errors.Join(fmt.Errorf("verify seed identity %s failed; run seed identities first", namespace), ctx.Err())
		}
		scoped, validUntil, err := credentials.MintCredentials(ctx, client, config, namespace, seed.IdentityName, expiration)
		if err != nil {
			return result, err
		}
		// Construct from an allowlist, never copy admin auth, plugins or certificates.
		guest := clientcmdapi.NewConfig()
		guest.Clusters["guest"] = &clientcmdapi.Cluster{Server: scoped.Host, CertificateAuthorityData: scoped.CAData, TLSServerName: scoped.ServerName, InsecureSkipTLSVerify: scoped.Insecure}
		guest.AuthInfos[seed.IdentityName] = &clientcmdapi.AuthInfo{Token: scoped.BearerToken}
		guest.Contexts["benchmark"] = &clientcmdapi.Context{Cluster: "guest", AuthInfo: seed.IdentityName, Namespace: namespace}
		guest.CurrentContext = "benchmark"
		data, err := clientcmd.Write(*guest)
		if err != nil {
			return result, fmt.Errorf("encode seed kubeconfig")
		}
		path := filepath.Join(output, namespace+".kubeconfig")
		if err := writeSeedPrivate(path, data); err != nil {
			return result, err
		}
		result.Files = append(result.Files, seedCredentialFile{Namespace: namespace, Path: path, ValidUntil: validUntil})
	}
	return result, nil
}

// Token issuance is separate from create-only identity setup and must not adopt
// a same-named account or a broadened role left by another actor.
func readSeedIdentity(ctx context.Context, client kubernetes.Interface, spec seed.Spec, n int64) error {
	return credentials.VerifySeedIdentity(ctx, client, spec, n)
}

func writeSeedPrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Sync(), f.Close())
}

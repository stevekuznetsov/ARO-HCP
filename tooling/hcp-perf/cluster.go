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
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/adversary"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/capture"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/cpuburn"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/record"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/requesttrace"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/scrape"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func runCluster(ctx context.Context, args []string, out io.Writer) (retErr error) {
	command := strings.Join(args[:2], " ")
	allowed, ok := map[string]string{
		"capture start": "input config scrape-bundle kubeconfig timeout wait", "capture status": "id kubeconfig timeout",
		"capture stop": "id kubeconfig timeout wait", "capture cleanup": "id delete-evidence kubeconfig timeout",
		"adversaries start": "input kubeconfig timeout", "adversaries status": "id namespace kubeconfig timeout",
		"adversaries discover": "input kubeconfig timeout",
		"adversaries stop":     "id namespace kubeconfig timeout", "cpu burn": "workers duration",
		"record start": "input kubeconfig timeout", "record status": "namespace id kubeconfig timeout",
		"record stop": "namespace id kubeconfig timeout", "record worker": "input kubeconfig",
		"canary run": "namespace deployment kubeconfig timeout ack-budget ready-budget cleanup-budget ack-expected ready-expected cleanup-expected request-log",
	}[command]
	if !ok {
		return fmt.Errorf("unknown command %q", command)
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	input := f.String("input", "", "specification JSON")
	config := f.String("config", "", "Prometheus configuration")
	scrapeBundle := f.String("scrape-bundle", "", "generated bundle including TLS mount requirements, instead of --config")
	kubeconfig := f.String("kubeconfig", "", "explicit management kubeconfig; omitted uses in-cluster credentials")
	id := f.String("id", "", "session or run ID")
	namespace := f.String("namespace", "", "adversary namespace")
	timeout := f.Duration("timeout", 5*time.Minute, "total operation timeout")
	wait := f.Bool("wait", true, "wait for capture readiness or shutdown")
	deleteEvidence := f.Bool("delete-evidence", false, "consent to namespace and persistent evidence deletion")
	workers := f.Int("workers", 0, "runnable CPU workers")
	duration := f.Duration("duration", 0, "self-enforced CPU workload lifetime")
	deployment := f.String("deployment", "", "existing zero-replica canary Deployment")
	ackBudget := f.Duration("ack-budget", 30*time.Second, "canary scale acknowledgement deadline")
	readyBudget := f.Duration("ready-budget", 120*time.Second, "canary readiness deadline including acknowledgement")
	cleanupBudget := f.Duration("cleanup-budget", 120*time.Second, "canary scale-down cleanup deadline")
	ackExpected := f.Duration("ack-expected", 0, "soft ACK expectation; overruns are measured, not canceled")
	readyExpected := f.Duration("ready-expected", 0, "soft readiness expectation including ACK")
	cleanupExpected := f.Duration("cleanup-expected", 0, "soft scale-down expectation")
	requestLog := f.String("request-log", "", "optional new private HTTP NDJSON file (10 MiB cap); header timing only, excludes client limiter waits")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	var flagErr error
	f.Visit(func(flag *flag.Flag) {
		if !strings.Contains(" "+allowed+" ", " "+flag.Name+" ") {
			flagErr = fmt.Errorf("--%s does not apply to %s", flag.Name, command)
		}
	})
	if flagErr != nil {
		return flagErr
	}
	if command == "cpu burn" {
		err := cpuburn.Run(ctx, *workers, *duration)
		status := "completed"
		if err != nil {
			status = "failed"
		}
		return errors.Join(err, json.NewEncoder(out).Encode(map[string]string{"version": "v1", "status": status}))
	}
	if command == "record worker" {
		var spec record.Spec
		if err := decodeFile(*input, &spec); err != nil {
			return err
		}
		if err := spec.Validate(); err != nil {
			return err
		}
		streamConfig, err := recordRESTConfig(*kubeconfig)
		if err != nil {
			return err
		}
		client, logClient, err := record.NewClients(streamConfig, spec)
		if err != nil {
			return err
		}
		result, err := record.RecordWithLogClient(ctx, client, logClient, spec)
		return errors.Join(err, json.NewEncoder(out).Encode(result))
	}
	if *timeout <= 0 || *timeout > 24*time.Hour {
		return fmt.Errorf("timeout must be positive and at most 24h")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var client kubernetes.Interface
	var err error
	if command == "canary run" {
		trace, openErr := requesttrace.Open(*requestLog)
		if openErr != nil {
			return openErr
		}
		defer func() { retErr = errors.Join(retErr, trace.Close()) }()
		var config *rest.Config
		config, err = recordRESTConfig(*kubeconfig)
		if err == nil {
			// LIST/WATCH and writes have their own contexts. A global HTTP
			// timeout would terminate otherwise healthy watch response bodies.
			config.Timeout = 0
			config.Wrap(trace.WrapTransport(*namespace))
			client, err = kubernetes.NewForConfig(config)
		}
	} else {
		client, err = managementClient(*kubeconfig)
	}
	if err != nil {
		return err
	}
	var result any
	switch command {
	case "capture start":
		var spec capture.Spec
		if err := decodeFile(*input, &spec); err != nil {
			return err
		}
		var data []byte
		if *scrapeBundle != "" {
			if !spec.KubernetesDiscovery {
				return fmt.Errorf("--scrape-bundle requires kubernetesDiscovery=true for discovery and metrics authorization")
			}
			if *config != "" || len(spec.TLSMounts) != 0 {
				return fmt.Errorf("--scrape-bundle cannot be combined with --config or spec TLS mounts")
			}
			encoded, readErr := os.ReadFile(*scrapeBundle)
			if readErr != nil {
				return readErr
			}
			var bundle scrape.Bundle
			if err := json.Unmarshal(encoded, &bundle); err != nil {
				return fmt.Errorf("scrape bundle: %w", err)
			}
			if len(bundle.Gaps) != 0 || bundle.Config == "" {
				return fmt.Errorf("scrape bundle is incomplete; resolve blockers before capture")
			}
			spec.TLSMounts = bundle.TLSMounts
			data = []byte(bundle.Config)
		} else {
			var readErr error
			data, readErr = os.ReadFile(*config)
			if readErr != nil {
				return readErr
			}
		}
		var status capture.SessionStatus
		status, err = capture.Start(ctx, client, spec, string(data))
		if err == nil && *wait {
			status, err = capture.WaitReady(ctx, client, spec.ID)
		}
		result = status
	case "capture status":
		result, err = capture.Status(ctx, client, *id)
	case "capture stop":
		var status capture.SessionStatus
		status, err = capture.Stop(ctx, client, *id)
		if err == nil && *wait {
			status, err = capture.WaitStopped(ctx, client, *id)
		}
		result = status
	case "capture cleanup":
		err = capture.Cleanup(ctx, client, *id, *deleteEvidence)
		result = map[string]any{"version": "v1", "id": *id, "deletionAccepted": err == nil}
	case "adversaries start", "adversaries discover":
		var spec adversary.RunSpec
		if err := decodeFile(*input, &spec); err != nil {
			return err
		}
		if command == "adversaries discover" {
			result, err = adversary.Discover(ctx, client, spec)
		} else {
			result, err = adversary.Start(ctx, client, spec)
		}
	case "adversaries status":
		result, err = adversary.Status(ctx, client, *namespace, *id)
	case "adversaries stop":
		result, err = adversary.Stop(ctx, client, *namespace, *id)
	case "record start":
		var spec record.SessionSpec
		if err := decodeFile(*input, &spec); err != nil {
			return err
		}
		result, err = record.Start(ctx, client, spec)
	case "record status":
		result, err = record.Status(ctx, client, *namespace, *id)
	case "record stop":
		result, err = record.Stop(ctx, client, *namespace, *id)
	case "canary run":
		result, err = workload.RunCanary(ctx, client, workload.Spec{
			Namespace: *namespace, Name: *deployment,
			Deadlines:    experiment.CanaryDeadlines{Ack: *ackBudget, Ready: *readyBudget, Cleanup: *cleanupBudget},
			Expectations: experiment.CanaryDeadlines{Ack: *ackExpected, Ready: *readyExpected, Cleanup: *cleanupExpected},
		})
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return errors.Join(err, encoder.Encode(result))
}

func managementClient(path string) (kubernetes.Interface, error) {
	var config *rest.Config
	var err error
	if path == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", path)
	}
	if err != nil {
		return nil, fmt.Errorf("load management credentials (supply --kubeconfig outside a cluster): %w", err)
	}
	config.Timeout = 30 * time.Second
	config.UserAgent = "hcp-perf/v1"
	return kubernetes.NewForConfig(config)
}

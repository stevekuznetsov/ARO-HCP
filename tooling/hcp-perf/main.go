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
	"os/signal"
	"strings"
	"syscall"
	"time"

	dashboardcmd "github.com/Azure/ARO-HCP/tooling/hcp-perf/cmd/dashboard"
	iterationcmd "github.com/Azure/ARO-HCP/tooling/hcp-perf/cmd/iteration"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/adversary"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"version": "v1", "status": "error", "error": err.Error()})
		os.Exit(1)
	}
}

const usage = `hcp-perf: local experiment primitives (no live case executor yet)

  sizing resolve --profile FILE
  sizing hypershift --profile FILE [--default-cpu-milli 10 --default-memory-mib 64]
  case plan --case FILE --profile FILE
  case schedule --case FILE --profile FILE
  case progress --input RUN_DIR
  environment refresh --input FILE [--dry-run]
  adversaries plan --input FILE
  adversaries discover --input FILE --kubeconfig FILE
  adversaries start --input FILE --kubeconfig FILE
  adversaries status|stop --namespace NAME --id ID --kubeconfig FILE
  capture start --input FILE --config FILE --kubeconfig FILE
  capture status|stop --id ID --kubeconfig FILE
  capture cleanup --id ID --delete-evidence --kubeconfig FILE
  record start --input FILE --kubeconfig FILE
  record status|stop --namespace NAME --id ID --kubeconfig FILE
  record worker --input FILE [--kubeconfig FILE]
  record health --input DIR
  canary run --namespace NAME --deployment NAME --kubeconfig GUEST_FILE
  rp create-cluster|create-nodepool|get|delete|credentials --input FILE --resource FILE --output DIR
  seed render|run|identities|credentials --input FILE
  turnover run --input FILE --credentials-dir DIR --output DIR
  api-load run --input FILE --credentials-dir DIR --output DIR
  workload setup|credentials --input FILE --kubeconfig GUEST_ADMIN_FILE
  scrape generate --input FILE --kubeconfig FILE --output DIR
  scrape check --input BUNDLE --endpoint URL
  record export --namespace NAME --id ID --kubeconfig FILE --output DIR
  cpu burn --workers N --duration 1h
  metrics export --endpoint URL --queries FILE --start RFC3339 --end RFC3339 --step 30s --output NEW_DIR
  metrics stream --endpoint URL --queries FILE --output NEW_DIR --duration 5m
  metrics backfill --endpoint URL --queries FILE --start RFC3339 --end RFC3339 --output NEW_DIR
  report render --input RUN_DIR --metrics EXPORT_DIR --output NEW_HTML
  card render --input CARD_JSON --contention CONTENTION_JSON --resources RESOURCES_JSON --experience EXPERIENCE_JSON --output NEW_HTML
  dashboard export --input PRIVATE_MANIFEST_JSON --output NEW_JSON
  dashboard render --input PORTABLE_JSON --output NEW_HTML
  assessment build --input RUN_DIR --metrics EXPORT_DIR --output NEW_JSON
  cost estimate --input FILE
  budget allocate|check --input FILE --output DIR [--prior-plan FILE --name NAME --kcm-cpu-milli N --catalog-cpu-milli N]
  iteration submit --input FILE --id ID [--root DIR]
  iteration status|list|watch|stop|reconcile|serve --help
  scan plan --profile FILE --input FILE
  analyze --input DIR

Inputs and outputs are JSON. Export optionally reads HCP_PERF_BEARER_TOKEN.
Sizing resolve emits experiment JSON; sizing hypershift emits an experimental CSC, never applies it.
`

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		_, err := io.WriteString(out, usage)
		return err
	}
	switch args[0] {
	case "dashboard":
		command := dashboardcmd.NewCommand()
		command.SetContext(ctx)
		command.SetOut(out)
		command.SetErr(out)
		command.SetArgs(args[1:])
		return command.Execute()
	case "budget":
		return runBudget(args[1:], out)
	case "iteration":
		command := iterationcmd.NewCommand()
		command.SetContext(ctx)
		command.SetOut(out)
		command.SetErr(out)
		command.SetArgs(args[1:])
		return command.Execute()
	case "card":
		return runCard(args[1:], out)
	case "api-load":
		return runAPILoad(ctx, args, out)
	case "cost":
		return runCost(args[1:], out)
	case "scan":
		return runScan(args[1:], out)
	case "assessment":
		return runAssessment(ctx, args, out)
	case "report":
		return runReport(ctx, args, out)
	case "metrics":
		if len(args) > 1 && args[1] == "backfill" {
			return runMetricsBackfill(ctx, args, out)
		}
		if len(args) > 1 && args[1] == "stream" {
			return runMetricsStream(ctx, args, out)
		}
	case "environment":
		return runRetention(ctx, args, out)
	case "case":
		if len(args) > 1 && args[1] == "progress" {
			return runProgress(ctx, args, out)
		}
	case "workload":
		return runWorkloadSetup(ctx, args, out)
	case "rp":
		return runRP(ctx, args, out)
	case "seed":
		return runSeed(ctx, args, out)
	case "turnover":
		return runTurnover(ctx, args, out)
	case "scrape":
		return runScrape(ctx, args, out)
	case "record":
		if len(args) > 1 && args[1] == "health" {
			return runRecordHealth(ctx, args, out)
		}
		if len(args) > 1 && (args[1] == "export" || args[1] == "archive" || args[1] == "hold") {
			return runRecordExport(ctx, args, out)
		}
	}
	if len(args) >= 2 && ((args[0] == "capture") || args[0] == "record" || args[0] == "canary" || (args[0] == "adversaries" && args[1] != "plan") || args[0] == "cpu") {
		return runCluster(ctx, args, out)
	}
	command := args[0]
	args = args[1:]
	if command != "analyze" {
		if len(args) == 0 {
			return fmt.Errorf("%s requires a subcommand; use --help", command)
		}
		command += " " + args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profilePath := flags.String("profile", "", "sizing profile JSON")
	defaultCPU := flags.Int64("default-cpu-milli", 10, "HyperShift fallback CPU request in millicores")
	defaultMemory := flags.Int64("default-memory-mib", 64, "HyperShift fallback memory request in MiB")
	casePath := flags.String("case", "", "case JSON")
	input := flags.String("input", "", "evidence directory")
	output := flags.String("output", "", "new evidence directory")
	endpoint := flags.String("endpoint", "", "Prometheus endpoint")
	queriesPath := flags.String("queries", "", "query list JSON")
	startText := flags.String("start", "", "RFC3339 start")
	endText := flags.String("end", "", "RFC3339 end")
	step := flags.Duration("step", 30*time.Second, "query step")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	// Reject irrelevant flags rather than silently accepting a typo in an agent command.
	allowed := map[string]string{
		"sizing resolve": "profile", "case plan": "profile case", "case schedule": "profile case",
		"sizing hypershift": "profile default-cpu-milli default-memory-mib",
		"metrics export":    "endpoint queries start end step output", "analyze": "input",
		"adversaries plan": "input",
	}
	list, ok := allowed[command]
	if !ok {
		return fmt.Errorf("unknown command %q; use --help", command)
	}
	var flagErr error
	flags.Visit(func(f *flag.Flag) {
		valid := false
		for _, name := range strings.Fields(list) {
			valid = valid || f.Name == name
		}
		if !valid {
			flagErr = fmt.Errorf("flag --%s does not apply to %s", f.Name, command)
		}
	})
	if flagErr != nil {
		return flagErr
	}
	var result any
	var operationErr error
	switch command {
	case "sizing resolve", "sizing hypershift", "case plan", "case schedule":
		var profile sizing.Profile
		if err := decodeFile(*profilePath, &profile); err != nil {
			return fmt.Errorf("profile: %w", err)
		}
		if command == "sizing resolve" || command == "sizing hypershift" {
			resolved, err := sizing.Resolve(profile)
			if err != nil {
				return err
			}
			result = resolved
			if command == "sizing hypershift" {
				result, err = sizing.ToHyperShift(resolved, sizing.Budget{CPUMilli: *defaultCPU, MemoryMiB: *defaultMemory})
				if err != nil {
					return err
				}
			}
		} else {
			var spec experiment.Spec
			if err := decodeFile(*casePath, &spec); err != nil {
				return fmt.Errorf("case: %w", err)
			}
			plan, err := experiment.Plan(spec, profile)
			if err != nil {
				return err
			}
			result = plan
			if command == "case schedule" {
				result, operationErr = experiment.RolloutsForPeriod(plan.Spec.Applications, plan.Spec.Seed,
					time.Duration(plan.Spec.SteadySeconds)*time.Second, time.Duration(plan.Spec.TurnoverPeriodSeconds)*time.Second)
			}
		}
	case "metrics export":
		start, err := time.Parse(time.RFC3339, *startText)
		if err != nil {
			return fmt.Errorf("start: %w", err)
		}
		end, err := time.Parse(time.RFC3339, *endText)
		if err != nil {
			return fmt.Errorf("end: %w", err)
		}
		var queries []metrics.Query
		if err := decodeFile(*queriesPath, &queries); err != nil {
			return fmt.Errorf("queries: %w", err)
		}
		client := &http.Client{Transport: bearerTransport{token: os.Getenv("HCP_PERF_BEARER_TOKEN")}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		result, operationErr = metrics.Export(ctx, client, *endpoint, queries, start, end, *step, *output)
	case "analyze":
		result, operationErr = metrics.Analyze(*input)
	case "adversaries plan":
		var spec adversary.Spec
		if err := decodeFile(*input, &spec); err != nil {
			return fmt.Errorf("node snapshot: %w", err)
		}
		result, operationErr = adversary.Plan(spec)
	}
	// Partial export/analysis results are useful even when the operation failed.
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return errors.Join(operationErr, err)
	}
	return operationErr
}

type bearerTransport struct{ token string }

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.token != "" {
		if r.URL.Scheme != "https" {
			return nil, fmt.Errorf("bearer authentication requires HTTPS")
		}
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

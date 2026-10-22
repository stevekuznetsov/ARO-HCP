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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaseRetentionPreflight(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "timeout", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required: %v", tool, err)
		}
	}
	const scriptPath = "scripts/run-case.sh"
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	refresh := strings.Index(script, "\nrun environment-refresh 6m ")
	gate := strings.Index(script, "\nrun environment-refresh-gate ")
	if refresh < 0 || gate <= refresh {
		t.Fatal("missing ordered refresh and verification gate")
	}
	for _, marker := range []string{"\nrun guest-nodes ", "\nobservation_started=true", "\nrecorder_attempted=true", "\nrun seed ", "\nrun workload-setup ", "\nrun canary-warmup "} {
		if strings.Index(script, marker) <= gate {
			t.Fatalf("%q must follow retention verification", marker)
		}
	}
	for _, tc := range []struct {
		name, filter string
		missing      bool
		exit         string
		pass         bool
		validation   bool
	}{
		{name: "verified", filter: ".", pass: true},
		{name: "verified-validation", filter: ".", pass: true, validation: true},
		{name: "blocked-validation", filter: `.status="failed"`, validation: true},
		{name: "case-insensitive-ids", filter: ".resourceGroups |= (reverse | map(.id |= ascii_upcase))", pass: true},
		{name: "missing-input", missing: true},
		{name: "malformed-input", missing: true},
		{name: "nonzero-even-with-verified-result", filter: ".", exit: "23"},
		{name: "timeout", filter: ".", exit: "124"},
		{name: "failed", filter: `.status="failed"`},
		{name: "dry-run", filter: `.dryRun=true`},
		{name: "planned", filter: `.status="planned" | .resourceGroups[].status="planned"`},
		{name: "partial", filter: `.resourceGroups[1].verified=false`},
		{name: "pending", filter: `.resourceGroups[1].status="pending"`},
		{name: "not-updated", filter: `.resourceGroups[1].updated=false`},
		{name: "not-attempted", filter: `.resourceGroups[1].attempted=false`},
		{name: "possibly-updated", filter: `.resourceGroups[1].possiblyUpdated=true`},
		{name: "failure", filter: `.failure={code:"failed"}`},
		{name: "group-failure", filter: `.resourceGroups[1].failure={code:"failed"}`},
		{name: "missing-group", filter: `.resourceGroups |= .[:1]`},
		{name: "wrong-group", filter: `.resourceGroups[1].id += "-other"`},
		{name: "duplicate-group", filter: `.resourceGroups += [.resourceGroups[0]]`},
		{name: "empty-groups", filter: `.resourceGroups=[]`},
		{name: "missing-groups", filter: `del(.resourceGroups)`},
		{name: "missing-result", filter: `empty`},
		{name: "multiple-results", filter: `., .`},
		{name: "malformed-result", filter: `"not a result"`},
	} {
		for _, contended := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/contended=%t", tc.name, contended), func(t *testing.T) {
				dir := t.TempDir()
				config := filepath.Join(dir, "config")
				bin := filepath.Join(dir, "bin")
				artifacts := filepath.Join(dir, "run")
				for _, path := range []string{config, bin} {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				write := func(path, content string, mode os.FileMode) {
					t.Helper()
					if err := os.WriteFile(path, []byte(content), mode); err != nil {
						t.Fatal(err)
					}
				}
				// No live CLI or Kubernetes command can be dispatched, including by traps.
				mock := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$TEST_DIR/calls"
case "$*" in
    --help) exit 0 ;;
    'workload setup') printf '%s\n' '{"error":"require --input, --kubeconfig"}' >&2; exit 1 ;;
    "environment refresh --input $TEST_DIR/run/inputs/retention.json --timeout 5m")
        [[ $AZURE_CONFIG_DIR == "$HOME/.azure-redhat" ]]
        cmp "$TEST_DIR/config/retention.json" "$TEST_DIR/run/inputs/retention.json"
        jq '.resourceGroups |= (unique_by(ascii_downcase) | map({id:.,status:"verified",attempted:true,updated:true,verified:true,possiblyUpdated:false})) | .status="verified" | .dryRun=false' "$TEST_DIR/run/inputs/retention.json" |
            jq "$TEST_FILTER"
        exit "$TEST_EXIT" ;;
    *) printf '%s\n' "$*" >>"$TEST_DIR/forbidden"; exit 91 ;;
esac
`
				perf := filepath.Join(bin, "hcp-perf")
				write(perf, mock, 0700)
				write(filepath.Join(bin, "kubectl"), "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >>\"$TEST_DIR/kubectl\"\nexit 91\n", 0700)
				write(filepath.Join(bin, "az"), "#!/usr/bin/env bash\nprintf forbidden >>\"$TEST_DIR/forbidden\"\nexit 91\n", 0700)
				retention := `{"version":"v1","resourceGroups":["/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/test-underlay","/subscriptions/00000000-0000-0000-0000-000000000002/resourceGroups/test-customer"]}`
				if !tc.missing {
					write(filepath.Join(config, "retention.json"), retention, 0600)
				}
				if tc.name == "malformed-input" {
					write(filepath.Join(config, "retention.json"), "{broken", 0600)
				}
				duration, recovery, recordDuration, adversaryDuration := 7200, 600, 21600, 10000
				if tc.validation {
					duration, recovery, recordDuration, adversaryDuration = 300, 30, 2220, 480
				}
				seed := fmt.Sprintf(`{"guestNodes":3,"applications":0,"id":"test","namespacePrefix":"apps","image":"image","timeoutSeconds":60,"durationSeconds":%d,"recoverySeconds":%d,"maxOutstanding":20,"binary":{"path":%q,"expectedSHA256":"%x"}}`, duration, recovery, perf, sha256.Sum256([]byte(mock)))
				for _, name := range []string{"seed", "setup", "turnover"} {
					write(filepath.Join(config, name+".json"), seed, 0600)
				}
				write(filepath.Join(config, "record.json"), fmt.Sprintf(`{"id":"record","namespace":"record","namespaces":["hcp"],"enableWatch":true,"durationSeconds":%d}`, recordDuration), 0600)
				write(filepath.Join(config, "captures.json"), `[{"id":"capture"}]`, 0600)
				write(filepath.Join(config, "adversaries.json"), fmt.Sprintf(`{"id":"adversary","namespace":"adversary","targetNamespace":"hcp","durationSeconds":%d}`, adversaryDuration), 0600)
				for _, name := range []string{"queries", "node-queries"} {
					write(filepath.Join(config, name+".json"), `[{"name":"up","expr":"up"},{"name":"scrape-time","expr":"timestamp(up)"},{"name":"adversary-cpu-rate","expr":"cpu"}]`, 0600)
				}
				bundle := filepath.Join(dir, "bundle.json")
				write(bundle, `{"gaps":[],"expectedJobs":["job"],"spec":{"targetNamespaces":["hcp"]}}`, 0600)
				args := []string{scriptPath, "--config-dir", config, "--artifacts-dir", artifacts}
				if tc.validation {
					args = append(args, "--validation", "--settle-seconds", "30")
				}
				if contended {
					args = append(args, "--contended")
				}
				exit := tc.exit
				if exit == "" {
					exit = "0"
				}
				cmd := exec.CommandContext(t.Context(), "bash", args...)
				cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + dir,
					"HCP_PERF=" + perf, "MGMT_KUBECONFIG=" + bundle, "GUEST_ADMIN_KUBECONFIG=" + bundle,
					"CAPTURE_BUNDLE=" + bundle, "HCP_NAMESPACE=hcp", "CAPTURE_ENDPOINT=http://unused.invalid",
					"NODE_METRICS_ENDPOINT=http://unused.invalid", "TEST_DIR=" + dir, "TEST_FILTER=" + tc.filter, "TEST_EXIT=" + exit}
				output, err := cmd.CombinedOutput()
				if err == nil {
					t.Fatalf("script must stop at retention failure or mock Kubernetes sentinel: %s", output)
				}
				if data, err := os.ReadFile(filepath.Join(dir, "forbidden")); !os.IsNotExist(err) {
					t.Fatalf("unexpected resource command: %s (%v)", data, err)
				}
				calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
				wantRefresh := 1
				if tc.missing {
					wantRefresh = 0
				}
				if strings.Count(string(calls), "environment refresh") != wantRefresh {
					t.Fatalf("unexpected refresh calls: %s; output: %s", calls, output)
				}
				kube, kubeErr := os.ReadFile(filepath.Join(dir, "kubectl"))
				if tc.pass {
					if kubeErr != nil || strings.Count(string(kube), "\n") != 1 || !strings.Contains(string(kube), "get nodes -o json") {
						t.Fatalf("verified refresh must reach first Kubernetes read only: %s (%v); %s", kube, kubeErr, output)
					}
					session, err := os.ReadFile(filepath.Join(artifacts, "inputs", "session.json"))
					var timing struct {
						Mode                                                                                                                           string
						SteadySeconds, RecoverySeconds, SettleSeconds, CanaryCadenceSeconds, BaselineCycles, ExpectedSteadyCanaries, ScheduledRollouts int
						Objective, CanaryPolicy, ExpectationsFile                                                                                      string
						ExpectationFlags                                                                                                               map[string]string
						HardDeadlineSeconds                                                                                                            map[string]int
						PreflightSeconds, CanaryTimeoutSeconds, CanaryCallBoundSeconds                                                                 int
					}
					wantMode, wantSettle, wantCadence, wantCanaries := "soak", 300, 300, 24
					if tc.validation {
						wantMode, wantSettle, wantCadence, wantCanaries = "validation", 30, 30, 10
					}
					if err != nil || json.Unmarshal(session, &timing) != nil || timing.Mode != wantMode || timing.SteadySeconds != duration || timing.RecoverySeconds != recovery || timing.SettleSeconds != wantSettle || timing.CanaryCadenceSeconds != wantCadence || timing.BaselineCycles != 5 || timing.ExpectedSteadyCanaries != wantCanaries || timing.ScheduledRollouts != 0 {
						t.Fatalf("unexpected persisted timing: %s (%v)", session, err)
					}
					if timing.Objective != "Observe and record progress and degradation, boundedhardstop" || timing.CanaryPolicy != "observe-degradation" || timing.ExpectationsFile != "commands/baseline-expectations.json" || timing.PreflightSeconds != 30 || timing.CanaryTimeoutSeconds != 300 || timing.CanaryCallBoundSeconds != 360 {
						t.Fatalf("unexpected canary policy: %s", session)
					}
					for stage, hard := range map[string]int{"ack": 30, "ready": 120, "cleanup": 120} {
						if timing.HardDeadlineSeconds[stage] != hard || timing.ExpectationFlags[stage] != "--"+stage+"-expected" {
							t.Fatalf("expectations must be separate from fixed hard deadlines: %s", session)
						}
					}
				} else if !os.IsNotExist(kubeErr) {
					t.Fatalf("blocked refresh reached Kubernetes: %s (%v)", kube, kubeErr)
				}
				if !tc.missing {
					copied := filepath.Join(artifacts, "inputs", "retention.json")
					data, err := os.ReadFile(copied)
					if err != nil || string(data) != retention {
						t.Fatalf("retention input not preserved: %s (%v)", data, err)
					}
					info, err := os.Stat(copied)
					if err != nil || info.Mode().Perm() != 0400 {
						t.Fatalf("retention copy must be read-only: %v (%v)", info, err)
					}
					hashes, err := os.ReadFile(filepath.Join(artifacts, "evidence", "inputs.sha256"))
					if err != nil || !strings.Contains(string(hashes), fmt.Sprintf("%x  %s", sha256.Sum256([]byte(retention)), copied)) {
						t.Fatalf("retention input hash missing: %s (%v)", hashes, err)
					}
				}
				outcome, err := os.ReadFile(filepath.Join(artifacts, "evidence", "outcome.json"))
				var result struct {
					ExitCode                                      int
					ExecutionStatus, ExperimentStatus, StopReason string
				}
				if err != nil || json.Unmarshal(outcome, &result) != nil || result.ExitCode == 0 {
					t.Fatalf("missing failed outcome: %s (%v)", outcome, err)
				}
				if result.ExecutionStatus != "harness_error" || result.ExperimentStatus != "unknown" {
					t.Fatalf("input/retention/dispatch failure is not an expectation violation: %s", outcome)
				}
				if tc.missing && result.StopReason != "input_error" {
					t.Fatalf("missing input misclassified: %s", outcome)
				}
			})
		}
	}
}

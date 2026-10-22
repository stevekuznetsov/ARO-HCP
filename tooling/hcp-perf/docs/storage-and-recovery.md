# Storage And Recovery

This is the maintained map of hcp-perf tools, their state, and recovery after a
workstation reboot or `/tmp` wipe. Paths below are relative to the
`ARO-HCP-perf-runner` repository root unless stated otherwise. Command contracts
remain in the [CLI README](../README.md), [case runner](../scripts/README.md), and
[campaign supervisor](../scripts/CAMPAIGN.md). The
[methodology](tshirt-methodology.md) explains how to interpret the evidence.

## Source And Evidence

| Location | Role |
| --- | --- |
| `tooling/hcp-perf/` | Maintained Go CLI, tests, examples, Python/shell tools, and docs. `docs/cards/` contains reviewed summaries and presentation inputs, not all raw evidence. |
| `docs/design/hcp-perf/` | Design history and deferred work; not a live execution inventory. |
| Separate `hypershift-perf-sizing` worktree | Experimental HyperShift schema, HO/CPO sizing projection and related image changes. These are not supplied by the ARO CLI or assumed present in released images. See [sizing](hypershift-sizing.md). |
| `_artifacts/<run-or-candidate>/` | Canonical private local store for individual inputs, frozen source/binaries, command receipts, raw evidence, credentials and derived results. Ignored by Git, not disposable build output. |

The [workspace map](tshirt-methodology.md#workspace-and-evidence-locations) records
the isolated worktrees and early histories: `perf-single-node`,
`perf-three-node-transition/attempt-6`, `perf-resource-locked-scan`, and
`tshirt-cards/small-v1`. Later histories include `medium-v1-20260910`,
`small-constrained-v1`, and `small-cost-v1` through `small-cost-v13` beneath
`_artifacts/`. Preserve their citations and hashes; do not rename or rewrite them
to fit a newer layout. Artifact launchers are historical execution records, not
portable supported entrypoints.

Maintained source does not mean committed source: inspect both worktrees for
dirty/untracked files. A clone or Git revision alone may omit the implementation
used by an experiment. Freeze the consumed source, executable and image digests;
do not edit a running script or its frozen bundle.

## Tool Map

Output paths are operator-selected unless noted. Keep durable outputs under a
private run root; many commands require a **new** output with an existing parent.

| Tool Group | Data And Location | Recovery Boundary |
| --- | --- | --- |
| `sizing resolve/hypershift`, `case plan/schedule`, `scan plan` | Profiles in `examples/`; save rendered JSON/CSC and schedules with reviewed inputs. | Offline regeneration is possible from exact inputs; rendering does not apply policy or prove startup safety. |
| `cost estimate`; `scripts/*offline_budgets.py`, `cost_allocate*.py` | Price assumptions, observations, allocations, source/output hashes and candidate profiles in private analysis/candidate roots. | Offline analysis, not live sizing. Cost v1-v13 scripts still read specific historical `_artifacts` dependencies; moving them into tooling has **not** made the chain portable or fully refactored. |
| Personal environment Make targets; `environment refresh` | Image overrides, rendered environment cache, pipeline step cache, exact RG retention inputs and receipts. | Cache is not proof of deployed state; retention tags are not a lease or cancellation of cleanup already underway. |
| `rp create-cluster/create-nodepool/get/wait/delete/credentials` | New private operation directory: `operation.json`, request events and permitted response artifacts; admin kubeconfig at explicit `--credentials-output`. | Accepted cloud operations continue independently. Use the saved operation URL with `rp wait`, not another create/delete. |
| `seed render/run/identities/credentials`, `workload setup/credentials` | Rendered kube-burner inputs, pinned binary identity, namespace/APF ownership UIDs, credential receipts and private kubeconfigs. | Guest fixtures remain remotely; setup is not an adopt/resume interface. Reconcile partial setup and expired credentials explicitly. |
| `canary run`, `turnover run`, `api-load run` | Incremental request/cadence/rollout evidence, results and outstanding-work records at selected paths. | Local generators stop with the host; already accepted Kubernetes mutations can still reconcile. Do not replay missed cadence slots or call missing outcomes successful. |
| `scrape generate/check`, `capture start/status/stop/cleanup` | Local scrape bundle; remote `hcp-perf-<capture-id>` namespace with immutable config and Prometheus PVC. | Prometheus continues after workstation loss until stopped or otherwise interrupted. Stop retains its PVC; cleanup can destroy evidence. Config/credential changes need a new session. |
| `metrics stream/export/backfill`, `scripts/record-guest-stats.sh` | Local manifests, hashes, raw query chunks and guest `observations.jsonl`. Streams use `manifest.json`, `journal.jsonl`, `chunk-NNNNNN/`. | Local collectors/forwards do not survive reboot as running processes. No stream resume; backfill retained windows separately, then start a new stream. |
| `record start/status/stop/health/export` | Independent remote Job/PVC; `/data/observations` holds snapshots, watches, acknowledged raw logs, checkpoints and result. Local export holds `archive.tar` and `manifest.json`. | Remote worker continues within its finite lifetime; no Job retry resumes interrupted recording. Export a terminal or acknowledged suspended writer only after verifying no nonterminal recorder Pods. |
| `adversaries discover/start/status/stop` | Immutable remote registration, node plan and CPU Jobs; save returned inventory locally. | Workstation reboot does **not** stop CPU load. Burners run until their duration or explicit stop; Job deadline is duration plus 60 seconds, with no retries. Verify stop remotely. |
| `scripts/run-case.sh`; `scripts/campaign.py` | Frozen inputs/source, command receipts, per-phase/arm evidence and independent execution/safety/capture outcomes. Layouts differ below. | Bounded orchestration, not infrastructure provisioning or automatic interrupted-load recovery. |
| `analyze`, `case progress`, `assessment build`, `report render`, `card render`; campaign `review` | Private JSON/HTML reviews with provenance; reviewed cards in `docs/cards/`. | Recompute into new outputs without rerunning load. HTML is a bounded preview, not the raw evidence. Campaign `review` writes `review.json` for terminal runs; use `recover-inspect` for read-only inspection. |

## Run Layouts

For new operator-organized runs, this is a suggested layout, **not a universal
schema or a directory tree every tool creates**:

```text
_artifacts/<new-run>/
  inputs/    reviewed specs, profiles, queries, retention and resource inventory
  bundle/    frozen source, binaries, resolved config and hashes
  private/   credentials and auth copies, never publication inputs
  commands/  argv, stdout, stderr, exit and operation receipts
  evidence/  metrics, recorder exports, snapshots, assessments and reports
  tmp/       disposable scratch on persistent storage when useful
```

`run-case.sh` actually creates `inputs/`, `private/`, `commands/`, `evidence/`;
its run/outcome files are `evidence/run.json` and `evidence/outcome.json`.
`case progress --input` takes that **run root**, not `evidence/`.

`campaign.py freeze` creates the new campaign root and `bundle/`, plus `build.log`.
The bundle includes `source/`, `campaign.py`, `hcp-perf`, `kube-burner`,
`config.json`, `provenance.json`, `sha256.json`, `campaign.service`, retained
queries/retention and `capture-ready-at-freeze.json`. Execution writes root-level
`started.json`, `status.json`, `outcome.json`; `commands/`, `arms/<name>/`,
`resolved/`, `retention/`, `capture-gates/`, `capture-guard/`, `safety/` and,
when applicable, `limits/`, `record-export/`, `metrics-final/`. Arm credentials
and command output are private even without a directory named `private`.
External capture handoffs, streams and kubeconfigs remain dependencies, not
automatically copied into this tree. Do not relocate a frozen campaign: its
configuration and service definition contain absolute paths.

## Durable Paths

New campaigns should set `durableStateRoot` to a persistent private directory.
Output must resolve beneath it. The opt-in check rejects output, root,
kubeconfigs, lock and observation dependencies resolving beneath `/tmp`,
`/var/tmp`, `/run` or `/dev/shm`, including symlinks. Omitting the field makes no
durability claim. It cannot detect every tmpfs mount, disk failure or external
kubeconfig certificate/token/exec-plugin dependency; verify the actual filesystem.

Supported filesystem references resolve against the **config file's parent**,
not the shell working directory. This includes predecessor run/snapshot/inventory
paths and partial-cleanup inventory. `~` expands; shell variables do not. URLs and
PromQL remain literal. Optional `pressureCPUQueryFile`/`pressureBusyQueryFile`
embed expression contents at freeze. Consumed non-secret inputs/source/burner
bytes are retained; credential files remain absolute references plus hashes.
The frozen capture-ready snapshot is provenance, never current readiness proof.
See the [complete path contract](../scripts/CAMPAIGN.md#durable-storage-and-reboot).

The current [root Makefile](../../../Makefile) defaults `PERSONAL_STATE_DIR` to
`<repo>/_artifacts/personal-dev`, with
`PERS_OVERRIDE_FILE=$(PERSONAL_STATE_DIR)/personal-dev-override.yaml`.
[Templatize](../../../setup-templatize-env.mk) and
[Clusters Service](../../../cluster-service/setup-env.mk) default `ENV_CACHE_DIR`
to `<repo>/_artifacts/env-cache`. Override both state directories per environment
incarnation using absolute paths without spaces; these recipes require private,
current-user-owned, non-symlink directories (0700) and publish outputs atomically.

`STEP_CACHE_DIR` is separate and still defaults to repository `.step-cache`.
Use a fresh incarnation-scoped cache for a rebuild and inspect the effective
cleanup invocation too. Durable default caches can be stale after cloud deletion;
neither a cache hit nor a new empty cache automatically proves live infrastructure
state. Never remove another checkout's cache as a reset shortcut.

`_artifacts/personal-reset-20260914/` records the scoped cleanup-then-rebuild
orchestration, with **no customer/HCP creation after rebuild**. Its actual layout
uses `source/`, `bin/`, `inputs/`, `receipts/`, `logs/`, `evidence/`, `timing/`,
`cache/steps/` and `tmp/`. It froze earlier source with `/tmp` references relocated
into that private `tmp/`; do not assume later Make changes altered that snapshot.
Read its private `ORCHESTRATION.md`, `status.json` and phase receipts for current
progress rather than treating this guide as a live worker-status record.

## Reboot Recovery

Durability means files can survive a reboot **on the same host/home filesystem**,
not that execution resumes. `/tmp` wiping should not remove durable dependencies;
it can still break an older run that referenced scratch paths. Disk/home loss
needs a separate backup. Cloud/RG/PVC cleanup is another failure domain and may
destroy remote data even while all local files survive. No off-machine backup is
configured by these tools; fsync, hashes and retained PVCs are not substitutes.

1. Preserve the interrupted run unchanged. Inspect its bundle and receipts first,
   then separately check local units and owned remote resources. Stored PIDs and
   `status.json` are not proof a process is alive.
2. Revalidate Azure/Kubernetes access and explicitly restore required forwards.
   Check remote capture, recorder and adversary status, Job deadlines, accepted RP
   operations and owned fixtures before authorizing new work.
3. Export remaining evidence before retention or cleanup removes it. Preserve
   partial chunks/logs and gaps; lack of a terminal result means unknown, not success.
4. Any authorized continuation of interrupted load uses new run/session IDs and a
   new freeze. Never delete `started.json`, overwrite `outcome.json`, remove phase
   receipts or discard locks merely to bypass a recovery gate.

From the repository root, campaign inspection is offline and read-only:

```bash
python3 -B tooling/hcp-perf/scripts/campaign.py recover-inspect --output "$RUN"
```

It checks available bundle hashes and dependency presence, not credential validity
or live systemd/cloud state. `started.json` without `outcome.json` reports
`started-without-outcome-interrupted-or-unknown`. It does not acquire/delete the
lock, renew access, resume Jobs or mutate infrastructure.

Campaign launch uses a **transient** user systemd unit with `Restart=no`.
`bundle/campaign.service` preserves a persistent definition with absolute working
directory/entrypoint, bounded runtime/stop time and no `Install/WantedBy` section;
it is not automatically installed or enabled. Only an intact, verified
**never-started** bundle may be launched after current readiness/lifetimes are
revalidated. If needed, an operator can manually install/link its saved definition
under a reviewed unique name, refuse replacement, reload and explicitly start it.
Do not enable destructive work at boot. There is no automatic restore command.

The reset differs: its definition is already at
`~/.config/systemd/user/hcp-perf-personal-reset-20260914.service`, with persistent
logs, `Restart=no`, and boot enablement disabled. Interrupted started phases fail
closed rather than replaying. Follow its private orchestration procedure and
reconcile accepted Azure operations before any manual repair/start; persistence
does not authorize cleanup or rebuild replay.

For an interrupted RP wait, preserve the original `operation.json` immediately:
the client records the accepted `operationURL` before polling. After restoring
the configured RP endpoint, associate the saved URL with the exact resource and
observe it into a **new** output directory:

```bash
"$HCP_PERF" rp wait --input "$RP_CONFIG" --resource "$RESOURCE" \
  --operation-url "$SAVED_OPERATION_URL" --output "$NEW_WAIT_OUTPUT"
```

This does not resubmit the mutation. If acceptance was ambiguous and no URL was
saved, inspect resource/operation state; do not infer failure and repeat creation.

`metrics stream` has **no resume** and cannot reuse its output directory. Restore
the source/forward, use `metrics backfill --start ... --end ...` into a new output
for still-retained windows, and start a new stream for future time. Backfill cannot
recover samples never scraped, expired Prometheus history or missing raw logs.
Remote CPU Jobs and recorders can continue while the workstation is down; inspect
and explicitly stop owned producers when authorized, then verify they stopped.
Prometheus capture has no automatic lifetime stop.

## Privacy And Gaps

`~/.azure-redhat`, `~/.kube` and private per-run credentials may survive reboot,
but tokens/certificates still expire. File existence or a matching hash is not
valid access. Frozen capture credential snapshots do not rotate automatically.
Do not put raw secrets in Git, command lines, public reports or inline scrape
configuration; raw logs and metrics labels also need private handling and review.
Use private directories/files (0700/0600 where appropriate) for auth copies.

Ordinary scratch need not be archived. `TMPDIR` can point to a private durable
`tmp/` for useful restart diagnostics, but not every tool honors it, and scratch
is not the canonical copy of credentials, operation URLs or evidence. Archive
required outputs explicitly before deleting scratch or cloud resources.

Remaining work includes automatic credential/forward renewal, a unified versioned
resource/dependency inventory, verified off-machine backup, and consolidation of
the ad hoc cost v1-v13 evidence chain into explicit portable inputs. Existing
offline tests cover bounded contracts, not end-to-end reboot, disk-loss or cloud
recovery. No recovery command here automatically authorizes destructive mutation.

# Durable Go Iterations

`hcp-perf iteration` is a Linux Go controller for a reviewed, bounded sequence of
typed actions. It calls existing Go libraries directly, not Python, shell commands,
or other `hcp-perf` CLI subprocesses. Detached workers run the frozen Go executable.
This is not yet a whole-workload or startup orchestration replacement.

## Offline Example

From `tooling/hcp-perf`, build a binary in an existing durable directory:

```sh
GOWORK=off go build -o "$HOME/.local/bin/hcp-perf" .
"$HOME/.local/bin/hcp-perf" iteration submit \
  --input examples/iteration-offline.json --id offline-demo
"$HOME/.local/bin/hcp-perf" iteration serve
```

In another terminal:

```sh
"$HOME/.local/bin/hcp-perf" iteration status --id offline-demo
"$HOME/.local/bin/hcp-perf" iteration watch --id offline-demo
"$HOME/.local/bin/hcp-perf" iteration list
```

The example performs only sizing and cost calculations. Its prices, capacity and
demand are fictional illustrations, not measurements or a qualified sizing policy.
The cost input is independent of the sizing result; the dependency demonstrates
ordering, not dynamic data passing. No command above contacts a cloud service.

## Commands

Every verb accepts `--root DIR`. The default is
`~/.local/state/hcp-perf/go-iterations`, separate from the discarded Python
prototype's database. IDs are 1-64 letters, digits, underscores or hyphens and must
start with a letter or digit. Positional arguments and irrelevant flags are errors.

| Command | Behavior |
|---|---|
| `submit --input FILE --id ID` | Strictly decode and validate the spec, freeze inputs and executable, claim the environment and queue work. Does not dispatch. |
| `status [--id ID]` | Read one run, or list all runs when the ID is omitted. |
| `list` | Read all runs in ID order. |
| `watch --id ID [--interval 1s]` | Emit newline-delimited JSON snapshots without reconciliation. Return on success, failure, stop, uncertainty or context cancellation. |
| `stop --id ID` | Persist cancellation intent and return the current journal state. Does not reconcile or delete remote resources. |
| `reconcile [--id ID]` | One reconciliation pass for one or all runs. May dispatch workers or observation-only recovery. |
| `serve [--interval 1s]` | Continuously reconcile all runs, starting with an immediate pass. |

The hidden internal command `worker --root ROOT --attempt TOKEN` is exclusively
for the controller's detached process factory. It calls `Work` using the exact
attempt token, not an action supplied on the command line.

`status`, `list` and `watch` do not call remote services or dispatch work. Opening
any command's controller may initialize the local SQLite database. Input/flag
errors are rejected before opening it. `watch` exits successfully only for
`SUCCEEDED`; `FAILED`, `STOPPED` and `BLOCKED` return an error after the snapshot.
The outer CLI reports errors once as JSON on stderr and exits 1. Context
cancellation also returns an error. Successful execution is not qualification.

Controller shutdown is not run cancellation. To stop a run, use
`iteration stop --id ID`; workers poll this durable bit and cancel their Go
library contexts. Keep a controller running or explicitly reconcile to finalize
undispatched actions. Stopping local observation cannot cancel an accepted RP
operation. Uncertain outcomes remain `BLOCKED` and retain the environment claim.

## Typed Inputs

The authoritative types and validation are in
[`internal/iteration/spec.go`](../internal/iteration/spec.go) and
[`actions.go`](../internal/iteration/actions.go). Top-level fields are:

- `version`: `v1`.
- `environment`: safe `id` and `incarnation` labels. Exclusion is keyed by **ID**,
  not incarnation; changing incarnation cannot bypass a blocked owner.
- `actions`: a nonempty ordered array of typed actions.

Every action has a unique safe `id`, `kind`, `recovery`, positive integer
`timeoutNanoseconds`, explicit `maxAttempts` in `[1,10]`, and exactly one parameter
family. Optional `dependsOn` entries contain `action` and `milestone`. References
must point to earlier actions. `completed` requires success; `started` means the
worker has claimed the action, not remote acceptance or readiness.

| Kind | Parameters | Required Recovery |
|---|---|---|
| `sizing-resolve` | `sizing.profile`, optional `sizing.hyperShiftDefaults` | `read-only` |
| `cost-estimate` | Inline `cost` spec | `read-only` |
| `rp-create-cluster`, `rp-create-node-pool` | `rp.config`, `rp.resource`, object `rp.payload` | `remote-mutation` |
| `rp-update-node-pool` | `rp.config`, exact node-pool `rp.resource`, full GET `rp.expectedResource`, integer `rp.replicas` | `remote-mutation` |
| `rp-inspect` | `rp.config`, `rp.resource` | `read-only` |
| `rp-wait` | `rp.config`, `rp.resource`, `rp.operationURL` | `read-only` |
| `rp-delete` | `rp.config`, `rp.resource`, `rp.confirmDelete: true` | `remote-mutation` |
| `metrics-export` | `metrics.endpoint`, `queries`, RFC3339 `start`/`end`, `stepNanoseconds` | `measurement` |
| `performance` | Inline `performance.Spec`; fresh ID/prefix, frozen startup inventory, guest credential reference and external capture | `measurement`, exactly one attempt |

See [Native Performance Action](PERFORMANCE.md) for its bounded execution contract,
capture prerequisites, finalization behavior and explicit validation gaps.

Submission accepts one JSON value up to 16 MiB, rejects unknown typed fields
including nested fields, and runs the core spec validator. RP payloads are opaque
ARM JSON objects, not iteration-schema structs. Their service-specific validation
remains with the RP adapter/service. There is no `argv`, generic command adapter,
script input map, executable path override, or shell expansion.

Inline inputs are frozen in `spec.json`. Relative cost evidence paths are resolved
against the input file's directory, including nested data-point evidence entries.
Evidence references are provenance only: their contents are neither loaded,
verified nor frozen. RP URLs and payload strings are not rewritten as local paths.
Startup and performance actions take explicit external kubeconfig references;
the RP credential action can create a private external guest kubeconfig. Do not
embed credentials in a spec. Review private payloads and artifacts before sharing.

## Durability And Recovery

### Population Image Bindings

For `performance.existingPopulation.imageBindings`, require a V7 or newer
binding-aware **serving controller**, as well as V7 or newer submission, explicit
reconciliation/finalization, and cleanup callers. Verify the actual running
`iteration serve` executable before submitting a rebound run; updating the CLI
or freezing a V7 worker alone is insufficient. Do not let old reconcilers handle
these runs.

V5/V6 strict submission rejects the new field, but their permissive journal
decoder drops it. Normal dispatch still uses the run's original hash-pinned
worker. However, old-controller finalization drops bindings from both the run
spec and the receipt, so equality can pass without validating them. It does not
fail closed on the unknown field. `iteration stop` only sets cancellation intent;
even a V7 stop caller does not select which serving controller performs readback.
Keep historical workers and receipts unchanged. See
[Explicit Catalog Image Rebinding](PERFORMANCE.md#explicit-catalog-image-rebinding).

### Node Pool Scale-Out

`rp-update-node-pool` is only a fixed-size scale-out action. Supply the complete
reviewed GET response in `expectedResource` (not a selected subset), with exact
resource ID, `Succeeded` provisioning state, current replicas, VM size, subnet and
version. Autoscaling must be absent or null; even an empty autoscaling object is
rejected. Desired `replicas` must exceed current replicas and be in `[1,200]`. The
200-node cap is deliberately conservative even for zoned pools. No `payload`,
arbitrary update body, scale-down, or same-count update is accepted. Existing
resource rejection in `rp-create-node-pool` is unchanged.

Immediately before the mutation, an exact resource GET must match **every field**
of the frozen snapshot. The only mutation is one merge PATCH with body
`{"properties":{"replicas":N}}`. This API has no HTTP ETag precondition: the
GET/PATCH sequence is **not atomic**. The operator must exclude all other writers,
including writers outside the local iteration environment claim.

Accepted operation metadata survives response-body timeout. Automatic recovery
and explicit `reconcile-operation` are GET-only and never repeat the PATCH. No
validated accepted receipt means uncertainty remains `BLOCKED`, not permission
to retry. After operation success, action-specific verification requires the exact
resource ID, desired replicas, `Succeeded`, and unchanged configuration including
VM, subnet and version. ARM `systemData`, provisioning state and absent/null
disabled-autoscaling representation may legitimately change. A wrong replica
count fails verification even if the operation reports success; the unresolved
action remains `BLOCKED`. This proves RP desired state, not guest Node readiness
or performance qualification; subsequent guest checks remain necessary.

Before submitting this new kind, promote the long-running `iteration serve`
controller to the newly tested binary as well as using it for submission/frozen
workers. An old serving controller cannot be assumed to validate or recover this
kind. Do not freeze or submit the combined scale/load iteration until the parent
combined review is complete.

Use a private persistent local filesystem supporting SQLite locks, flock and
fsync. The root must have no group/other permissions, normally mode 0700. SQLite
uses WAL and FULL synchronous mode. Each run reserves `ROOT/ID` and freezes the
Go binary as `worker`, inline `spec.json`, and an initial `manifest.json` before
publication. Per-attempt artifacts live below the run directory. SHA-256 checks
protect dispatch and worker execution from accidental input/binary modification.
Do not reuse IDs, even after an unsuccessful freeze leaves its reserved directory.

The default root is durable by convention, not a guarantee about the host's mounts.
An explicit `--root` is an operator storage decision: `/tmp`, `/var/tmp`, `/run`,
`/dev/shm`, ephemeral containers and temporary home directories are suitable only
for disposable offline tests, **not reboot-safe state**. Keep scratch builds and
test artifacts separate from maintained iteration state. All operators sharing an
environment must use the same root; exclusion is local, not fleet-wide.

The controller records dispatch intent before exec. An attempt lock and
transactional claim allow a restarted controller to dispatch the same unclaimed
token without executing the action twice. Workers record boot ID, PID and start
ticks; the controller never signals a saved PID. Detached workers can survive a
controller crash, but not machine reboot or cgroup-wide cleanup.

Lost read-only actions can retry within their attempt limit. Lost measurement
capture blocks rather than fabricating continuous coverage. RP mutations never
replay: an accepted operation receipt permits bounded observation-only resume in
a new attempt, followed by action-specific inspection. Creation requires an
existing Succeeded resource; deletion requires HTTP 404. Without sufficient
evidence the run stays `BLOCKED`. There is no force-unlock or operator API for
resolving uncertain claims. Investigate the remote outcome before reusing an
environment; do not delete its claim to get past an error.

Back up SQLite using a consistent database backup, not a copy of the database
alone while WAL is active. Do not prune active run directories. This is a new,
unshipped Go schema; there is no migration from the removed Python prototype.
See the [core contract](../internal/iteration/README.md) for recovery boundaries.

## Optional Manual Service

There is no service-install or activation command. An operator may review a user
unit like this, using an immutable, durably installed Go binary and absolute paths:

```ini
[Unit]
Description=HCP performance iteration controller

[Service]
Type=simple
UMask=0077
ExecStart="/persistent/bin/hcp-perf" iteration serve --root "/persistent/private/go-iterations"
Restart=on-failure
RestartSec=2
KillMode=process
TimeoutStopSec=10

[Install]
WantedBy=default.target
```

These literal example paths need no special escaping. For actual paths, apply
systemd syntax, not shell quoting: double-quote each argument, escape backslashes
and quotes, and double literal `%` specifiers and `$` characters in `ExecStart`.
Do not introduce a shell to interpret this command. Review and install the unit
manually without overwriting an existing service. Enabling it at boot and user
lingering are separate explicit operator decisions. Nothing here installs,
reloads, starts or enables a service.

`KillMode=process` intentionally leaves detached workers in the service cgroup
running when the controller stops. They retain resource usage and bounded
lifetimes. `systemctl stop` is not iteration cancellation. User logout policy may
still kill the cgroup; reboot loses all processes. Restarted reconciliation uses
durable recovery rather than replaying uncertain mutations.

## Not Ported

Recorder start/status/stop, workload setup, startup identity discovery, credential
retrieval, action-output wiring, and complete startup/campaign orchestration are
not supported iteration kinds. Existing [budget](BUDGET.md), [startup](STARTUP.md)
and [campaign](../scripts/CAMPAIGN.md) tools remain separate workflows; they were
not silently rewritten or wrapped by this controller. This bounded initial Go
controller must not be presented as full workload/startup automation.

# Direct API Load

`api-load` is an independent, bounded read-only client for existing guest-cluster
benchmark objects. It increases API request load without increasing Pod count,
rollout concurrency, creating objects, or modifying Deployment state. Run it
alongside turnover to complement rollout writes when rollout slots or the guest
Pod budget limit the workload generator before the API server.

This is an explicit synthetic recipe, not an observed or validated customer
traffic distribution. It cannot by itself establish representative customer
capacity or guarantee APF shedding before an API server crash.

## Invocation

The CLI routes `api-load` to its bounded worker and supplies SIGINT/SIGTERM
cancellation:

```sh
hcp-perf api-load run --input examples/api-load.json \
  --credentials-dir /private/benchmark-credentials \
  --output /private/results/api-load-640
```

Build any future executable to a **new path**. Do not overwrite the binary used
by live rollouts. This implementation does not start or stop those rollouts.
No live calls are needed for the unit tests; they use local TLS test servers.

The output directory must not exist and its parent must exist. It is created
with mode `0700`; `spec.json`, `requests.ndjson`, and `result.json` use `0600`.
Setup errors may leave a partial directory; always choose a new output path.

## Input

Inputs use strict JSON: unknown/miscased/duplicate fields, nulls, fractional
integers, missing required fields, and trailing values are rejected.

| Field | Contract |
| --- | --- |
| `version` | Required, `v1` |
| `durationSeconds` | Required integer, 1..3600; offered-load window |
| `ratePerSecond` | Required integer, 1..10000; logical offered operations/second |
| `maxInFlight` | Required integer, 1..256; fixed workers and bounded outstanding jobs |
| `requestTimeoutSeconds` | Required integer, 1..30; covers headers and response body |
| `seed` | Optional integer, default 42; explicit 0 is preserved |
| `namespacePrefix` | Optional DNS label; requires every namespace to start with `<prefix>-`; no discovery or expansion |
| `namespaces` | Required explicit unique DNS-label list, 1..1000 entries; each must have a target |
| `targets` | Required list, 1..1000 entries; selection uses seeded weights across the entire list |

`durationSeconds * ratePerSecond` must not exceed **1,000,000**. This caps request
records and scheduling work before any credentials are loaded or requests sent.
Weights are integers 1..1,000,000; they do not expand into repeated target arrays.

Each target requires `namespace`, `apiVersion`, `resource`, `verb`, and `weight`.
The namespace must be in `namespaces`. The initial allowlist is intentionally
aligned with the benchmark Role:

| API Version | Resource | Verb | Other Fields |
| --- | --- | --- | --- |
| `apps/v1` | `deployments` | `get` | Required exact DNS-subdomain `name`; no `limit` |
| `v1` | `pods` | `list` | No `name`; optional integer `limit` 0..10000, default 100 |
| `apps/v1` | `replicasets` | `list` | No `name`; optional integer `limit` 0..10000, default 100 |

Deployment GET permission is restricted to exact seeded names. The example uses
`hcp-perf-apps-0/app-000000`; substitute existing names from your setup. Seeded
Deployment indices are global (`app-000020` is in namespace suffix `-1`). There
is no existence preflight: 403/404 responses count as HTTP errors, not useful
successful load. ConfigMaps, arbitrary GVRs, PATCH, PUT, WATCH, subresources,
discovery, and all provisioning are rejected. The current benchmark Role has
Deployment get/update, not patch; APF matching rules are not RBAC grants.

LIST uses normal resourceVersion semantics (the parameter is absent), JSON
serialization, and a single page. It never follows `continue` or retries errors.
`limit: 0` explicitly asks the server for an unbounded list. Every response is
stream-consumed and discarded, capped at **32 MiB plus one detection byte**. A
body-limit error makes that request unsuccessful; the server may already have
spent memory serializing the response. Use bounded limits unless deliberately
testing large-list pressure. No response objects or raw body bytes are retained.

The example's 2:1:1 weights mean approximately 50% Deployment GET, 25% Pod LIST,
25% ReplicaSet LIST in one namespace. To spread offered load across six namespace
identities, explicitly add all six namespaces and repeat the same target weights
with each namespace's exact Deployment name. Equal total weight per namespace
gives statistical balance, not exact round-robin fairness. Fixed seed reproduces
offered target order; scheduling, response order, and dropped subsets depend on
runtime timing. There are no implicit label selectors.

## Credentials And Connections

Each namespace loads only `<credentials-dir>/<namespace>.kubeconfig`. No admin
kubeconfig flag, ambient kubeconfig, in-cluster fallback, or bearer-token
environment variable is used. Use **generated benchmark ServiceAccount token
kubeconfigs**, one per namespace, from `seed credentials`. The named file is
loaded directly without merging other files or consulting ambient contexts.
Its explicit current context must reference a cluster with a nonempty server,
an explicit user with a nonempty inline token, and a namespace exactly matching
the requested namespace. Missing or empty configuration is an error, even when
the process has in-cluster credentials or an ambient kubeconfig.

Exec and auth-provider plugins anywhere in the file are rejected before client
construction; no credential subprocess is started. The selected user must not
specify token files, client certificates/keys, basic authentication, or
impersonation. Tokens are static for the run, with no external refresh. Generate
tokens with sufficient lifetime before running; expiration is not repaired.
The files and server/CA settings must still be trusted. Namespace metadata and
an inline token do not prove ServiceAccount identity, ownership labels, or
server-side RBAC; do not substitute administrator tokens.

Only HTTPS with certificate verification is accepted. Authentication and CA
settings come from client-go; request paths are constructed only from validated
names and allowlisted API paths. Redirects are not followed. Normal keep-alive
and HTTP/2 multiplexing are retained; there is no deliberate connection churn or
API-server replica balancing. A sticky connection can concentrate load on one
replica. Observe per-replica metrics rather than assuming even distribution.

There is no client-go REST limiter, application retry, 429 retry, or pagination.
Normal underlying transport behavior can re-send a request. A
per-operation `httptrace.WroteRequest` hook counts fully written HTTP attempts,
including transport re-sends, separately from logical dispatch. Failed dials or
partial writes do not count as fully written attempts. This is client evidence,
not proof the API server accepted or processed the request.

## Scheduling And Evidence

The generator is open-loop. Monotonic elapsed time determines offered attempts.
Absolute deadlines are rounded up to nanoseconds rather than repeating a
truncated period (which drifts at rates such as 30 QPS). An early wake re-arms for
the same deadline instead of waiting an entire period. Each wake attempts nonblocking dispatch;
full worker slots are counted as `generatorLimited`, not queued indefinitely or
silently backpressured. Missed ticks cannot cause an unbounded catch-up burst:
only one quantum's budget may dispatch, with a minimum 1 ms quantum (up to 10 offers at
10,000 QPS). Disk stalls also occupy bounded slots and can limit the generator.

The normal duration ends scheduling, then outstanding operations drain for at
most their request timeout (apart from local IO). Caller cancellation stops
scheduling, cancels requests, joins all workers, and flushes evidence. The final
partial result is written and cancellation is returned to the CLI. No workers
or record writers are intentionally left running.

`requests.ndjson` has one record per dispatched logical operation, including
canceled jobs. Its sequence identifies the offered attempt; gaps are dispatch
drops. Target indices refer to `spec.json`. Dropped offers are counted globally
and per target, not written as individual records. Memory is bounded by worker
and channel limits plus a fixed latency histogram, not duration or response size.
The 64 KiB writer flushes and syncs once per second and on close, not per request.
A crash can lose the latest buffered evidence; a recording failure cancels the
run, returns an error, and invalidates harness evidence rather than proving HCP
failure. A blocked local filesystem can still delay shutdown.

`pacing.version=absolute-deadline-v1` identifies this runtime change. Its
`missedDeadline`, `slotsFull`, and `canceledOffers` sum to `generatorLimited`;
they never count as success. Wake count, early wakes, largest due batch and
maximum deadline lateness distinguish scheduler delay from a full dispatch
queue. Offer-sampled slot/job/event occupancy and maximum evidence flush+sync
and worker-to-collector wait expose disk backpressure (not exact continuous
queue peaks). Request `enqueued` and `queueDelayNanoseconds` separate dispatcher
delay from worker delay and HTTP/body latency. Request-accounting version,
rate/duration, worker/queue limits, timeouts and acceptance thresholds are unchanged.

Result counters distinguish `offered`, `dispatched`, `generatorLimited`,
`httpAttempts`, `completed`, `successful`, `rejected429`, `otherHTTPErrors`,
`transportErrors`, `timeouts`, `canceled`, and `bodyLimitErrors`. Every completed
run satisfies `offered = dispatched + generatorLimited` and
`completed = dispatched`. HTTP status counters and read-error counters may
overlap (for example, a 429 whose body times out). Success requires a 2xx response
and complete body consumption. HTTP failures do not themselves fail the CLI;
inspect the counters rather than interpreting exit zero as successful requests.

`intendedQPS` is the configured offered rate. `executedQPS` is fully written HTTP
attempts divided by the complete run interval, **including final drain**. Timestamps
include scheduling start/stop and final completion; per-request latency covers
headers **and body**, with header-only latency recorded separately. P50/P95 are
online nearest-rank millisecond upper-bound histogram estimates over all completed
operations, with a 30,001 ms overflow bucket. They are not success-only latency.

Records retain status codes and only validated UUID APF FlowSchema/PriorityLevel
UIDs, Audit-ID, and bounded numeric/HTTP-date Retry-After. Malformed values are
omitted. They never retain tokens, request headers, hosts, raw errors, or response
bodies. Map APF UIDs to configuration using separately collected authorized
evidence; this primitive performs no privileged discovery.

## Interpreting Pressure

Use a controlled rate ladder with a new spec/output for each run, leaving rollout
parameters unchanged. For example, 300 seconds at 640 or 1024 QPS remains below
the million-offer cap. These are recipes, not established safe rates. Monitor
canary health, APF queues/rejections, API latency, per-replica memory/CPU and
restarts with the existing independent collectors; stop on the experiment's
health guardrails. This primitive does not implement an automated safety stop.

High `generatorLimited`, low executed QPS, or 403/404-heavy traffic does not prove
KAS capacity. Separate client CPU/network/disk constraints from APF shedding.
429 plus validated APF IDs is useful request evidence, but correlate it with
server APF metrics before attributing rejection to a particular priority level.
Read load complements rollout writes; it does not model admission-heavy writes,
watch fanout, arbitrary customer object sizes, or balanced replica connections.

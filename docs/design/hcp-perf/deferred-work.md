# Deferred HCP Performance Work

## Scope Boundary

These are separate follow-on investigations, not hidden acceptance criteria for the [initial case runner](README.md). The initial work provides reproducible cases, composable adversaries, durable evidence, and offline analysis. It does not automatically choose a winning resource/APF configuration, establish a production capacity limit, or claim to reproduce observed customer workloads.

Each extension should reuse the same artifact and causal-timeline contracts, declare its changed independent variables, and keep diagnostics and missing evidence visible. A promising result should motivate another explicit case, not mutate the configuration during its scored interval.

## Campaign Optimization

Defer campaign scheduling, bracket/search strategies, increasing HCP or application population to find a boundary, and joint APF/resource optimization. First prove a fixed case produces useful, reproducible evidence and reliable failure classification. A future campaign can compose the primitives and compare complete bundles; it must distinguish insufficient demand, missing contention, worker bottlenecks, and missing streams from a capacity result.

Independent CPU and memory anchors and profile versions make controlled comparisons possible. Upper-size cases must test ratio validity, not assume a successful small profile scales linearly. Replication, noise handling, stopping criteria, and retention/cost policy need their own campaign design; a sophisticated scheduler or locking system is not part of the initial serial orchestrator.

## KAS Failover

A promising next test is loss of one **currently serving KAS**, with two separate variants: immediate replacement allowed, and that replica held unavailable for ten minutes. Record selection of the actual serving replica, fault acknowledgement, precise outage duration, replacement identity, and release/recovery.

Keep offered workload and CPU competitors active throughout the outage, unlike the initial recovery phase that stops new turnovers. Remaining KAS replicas should sustain useful work and protected progress without a growing backlog. Observe redistribution, controller/API behavior, and the returning replica rebuilding caches and rejoining service. Recovery is not complete merely because its Pod is Ready. The fault must remain scoped to one replica, have an independent release deadline, and preserve normal system identities and evidence collection. Define success thresholds and safety release before implementing the experiment.

## Memory Pressure and Hard Limits

Memory competitors are a separate experiment after footprint discovery. Design controlled pressure/reclaim cases that distinguish anonymous memory, file cache, kernel charge, reclaim stalls, Go runtime response, eviction, and OOM behavior. Do not use a memory competitor as the initial primary adversary or interpret a memory-request overrun as functional failure.

Validate a shipping policy with hard **memory request equals memory limit** separately from exploratory runs using a configurable safety multiplier such as three. Successful execution with a roomy safety limit does not demonstrate safety at the request-sized hard limit. Include startup, recovery, transient allocation peaks, and all helper/init/sidecar paths. CPU limits remain a separate issue; memory request-equals-limit is not a request to cap CPU or assert Guaranteed QoS.

## Dataset and State Limits

Vary dataset/cardinality independently of node and application turnover: object count and shape, etcd bytes, watch/cache population, and persistent state size can stress memory and recovery in different ways. Current application seeding fixes a reproducible population but is not a comprehensive dataset model.

Dataset limits, etcd/state recovery limits, their enforcement, and safe downsize behavior are **explicitly outside the original stability scope**. They need separate definitions of supported state, rejection/enforcement behavior, restore/recovery obligations, and downsize safety. Do not derive such limits from lightly populated e2e samples, one stable turnover run, or a chosen CPU/memory ratio.

## Runtime and APF Variants

Optional `GOMAXPROCS` variants may quantify the tradeoff between parallelism, CPU competition, latency, and memory/GC behavior. There is no default override and no CPU-request-derived value of one; the baseline preserves bursts. Record Go version and effective runtime settings when comparing variants.

Defer deep APF sensitivity analysis, queue/seat/concurrency searches, fairness across many identities, and storm profiles such as concentrated retries, watches/reconnects, or bursty mutations. The initial explicit reject-only benchmark class and bounded system/canary queues provide a controlled starting candidate, not a production recommendation. Future profiles are synthetic hypotheses until backed by observed customer workload evidence; none was agreed as representative of customer traffic here.

## When to Resume

Start follow-on work from a complete exported initial-case bundle and an explicit question. Choose one extension, declare unchanged controls, add its failure/recovery semantics, and state the evidence needed to distinguish a valid negative result from an invalid experiment. Preserve the initial runner's separation of execution, capture, analysis, and cleanup rather than expanding a single scored run into an adaptive campaign.

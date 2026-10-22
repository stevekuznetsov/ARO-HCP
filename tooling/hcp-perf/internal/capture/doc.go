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

// Package capture provisions standalone, durable Prometheus servers through a
// typed Kubernetes client. It does not require a Prometheus Operator.
//
// Start exclusively creates hcp-perf-<ID> and never adopts or replaces an existing
// namespace, even for an identical spec. Every successful create is recorded in
// the returned inventory; errors leave those resources intact for inspection.
// A failed or interrupted Start must be inspected with Status, not retried as an
// update. API timeouts can leave resources beyond the last confirmed inventory.
// The immutable ConfigMap stores the spec and separately supplied configuration.
// examples/capture.json contains a placeholder all-zero digest; replace it with
// a verified Prometheus server image digest before starting a real session.
//
// To materialize a generated scrape bundle, set Spec.TLSMounts = bundle.TLSMounts
// and call Start(ctx, client, spec, bundle.Config). The optional field uses
// []scrape.TLSMountRequirement. Generate the bundle for hcp-perf-<ID> with
// PrometheusServiceAccount="prometheus" and enable Spec.KubernetesDiscovery for
// its Kubernetes discovery/authenticated scrape jobs. Only
// cross-namespace CopyRequired mounts are accepted because Start creates a new
// namespace, never adopts an existing one. No CLI behavior is implemented here.
// TLS sources are read before namespace creation using GET of the exact named
// Secret/ConfigMap. Only selected nonempty keys are copied, one immutable object
// per key, preserving Secret versus ConfigMap source kind. Secret snapshots are
// Opaque; ConfigMap snapshots use binaryData. Caller credentials need those exact
// source GET permissions and destination create/get permissions. Prometheus RBAC
// gains no Secret/ConfigMap access. Duplicate source keys are deduplicated and
// shared source objects are read once. Limits are 64 requirements, 1 MiB per
// snapshot including its key, and 16 MiB aggregate values.
//
// Only the bundle's deterministic hashed destinations and read-only mounts under
// /etc/prometheus/tls/hcp-perf-tls-<hash>/value are accepted. Files use mode 0440,
// readable by fsGroup 2000 with Prometheus UID 1000; no service-account or config
// file path can be overwritten. The persisted spec contains references only.
// Status GETs only validated destination names and exports metadata, never source
// or destination credential bytes. TLSCredentialPolicy reports snapshot semantics.
// Snapshots are retained on Stop and destroyed with namespace cleanup; no source
// object is modified. Partial creation is retained, never overwritten on retry.
//
// Credential rotation and expiry monitoring are NOT automatic. PEM syntax,
// certificate/key matching, trust, validity dates, and run-length suitability must
// be verified by the caller; ensure certificates remain valid for the whole run.
// Updating source credentials does not refresh an existing capture session. This
// is a static credential baseline, not an ongoing rotation solution.
//
// Configuration must be a single YAML mapping with nonempty scrape_configs;
// JSON, as a subset of YAML, is also accepted. Structural validation is not
// Prometheus semantic validation. The selected Prometheus image validates its
// configuration at startup. No targets or metrics coverage are inferred. Ready
// means server readiness, never successful scrapes or complete metrics coverage.
//
// Configuration is stored verbatim in a ConfigMap, NOT a Secret. Do not supply
// inline tokens, passwords, private keys, or credential-bearing URLs. With
// kubernetesDiscovery=false, automatic token mounting is disabled and no RBAC is
// created or queried. Static unauthenticated targets work without these privileges.
// With kubernetesDiscovery=true, Kubernetes mounts a projected service-account
// token and CA using its standard admission mechanism. A session-unique ClusterRole
// and ClusterRoleBinding grant get/list/watch on nodes, services, endpoints, pods,
// and discovery.k8s.io endpointslices across ALL namespaces, plus get on
// nodes/metrics and non-resource GET /metrics, as required by generated scrape
// bundles. No nodes/proxy, secrets, writes, or other non-resource URLs are granted.
// The caller needs permission to create and bind these roles. Both cluster-scoped
// objects have the cluster-scoped Namespace as their controller owner; namespace
// deletion garbage-collects them. Stop retains these permissions until cleanup.
// Adversary Jobs must not use the Prometheus ServiceAccount.
//
// Supply kubernetes_sd_configs in the Prometheus YAML to use in-cluster discovery.
// Discovery authentication does not automatically authenticate scrape requests:
// configure authorization.credentials_file with
// /var/run/secrets/kubernetes.io/serviceaccount/token and appropriate tls_config
// per scrape job. The API server CA is mounted at
// /var/run/secrets/kubernetes.io/serviceaccount/ca.crt. Node addresses, kubelet
// serving-certificate trust, client authentication, token audience acceptance,
// network access, relabeling, and metrics coverage are NOT inferred or guaranteed.
// Do not disable TLS verification to work around incorrect trust configuration.
// Configuration is never
// included in status, errors, logs, or automatically exported evidence. Diagnostic
// messages are omitted because Kubernetes messages may contain configuration or
// credentials; reasons, condition states, and container exit codes are reported.
// API request errors expose only fixed classifications, never server-supplied
// error messages or arbitrary status reason strings.
//
// Start and Stop are nonblocking with respect to pod convergence. WaitReady and
// WaitStopped poll until the supplied context expires; callers should supply a
// deadline. Stop sets replicas to zero with UID/resourceVersion preconditions and
// retains the explicit PVC, ConfigMap, Service, and ServiceAccount. Only namespace
// and StatefulSet ownership reads precede scale-down; diagnostic read failures
// afterward return an error with stopping status, not a failed scale-down.
// Stopped means
// zero desired replicas and no nonterminal capture pods, including terminating
// pods, controlled by the current StatefulSet UID. Status lists all pods and scopes
// capture candidates by controller UID or the expected name prometheus-0. Missing
// or mismatched ownership labels/references on candidates produce an error rather
// than a ready/stopped result. PVC lists and the Service selector use capture-only
// labels. Unrelated adversary pods do not affect capture state. Event diagnostics
// cover the whole namespace. Labels are not an authorization boundary.
// There is intentionally no resume/update API. Cleanup requires deleteEvidence
// to be true, an owned StatefulSet with zero desired replicas, and no nonterminal
// pods ANYWHERE in the namespace, including adversaries and terminating pods.
// All Jobs in the namespace must also have Complete=True or Failed=True, or
// spec.suspend=true. A pending Job without pods blocks cleanup. Suspending a Job
// does not bypass the pod check; its nonterminal pods must finish terminating.
// Cleanup therefore requires namespace-wide list permission for both Jobs and pods.
// A partial Start without a StatefulSet needs operator-reviewed manual cleanup.
// These checks cover capture plus Jobs, not other controllers such as Deployments,
// other StatefulSets, DaemonSets, or CronJobs. Callers must quiesce those controllers
// and stop workload producers before cleanup: Job/pod checks and namespace deletion
// are not atomic and cannot prevent concurrent creation or resumption of workloads.
// Cleanup submits a preconditioned namespace deletion; success means deletion was
// accepted, not completed. This destroys adversary Jobs and all other namespace
// contents too. Namespace deletion destroys the PVC and
// may delete its disk according to the StorageClass reclaim policy. Export needed
// evidence before stopping (the HTTP server stops too), and before cleanup.
//
// Storage must name an existing class (for example managed-premium); its volume
// binding, disk capacity, retention, and backup policies remain the caller's
// responsibility. Retention is time based and does not guarantee the disk cannot
// fill. Pods require infra nodes, tolerate infra=true:NoSchedule, run as UID 1000
// with fsGroup 2000, and need admission policies that permit these settings.
// The HTTP Service is cluster-internal, without TLS/authentication; no ingress,
// port-forward, network isolation, admin API, or reload endpoint is created.
// Namespace ownership labels are safety checks, not an authorization boundary.
package capture

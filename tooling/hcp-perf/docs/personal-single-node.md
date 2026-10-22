# Personal Single-Node Evidence Run

This is a checkpointed operator workflow, not an infrastructure automation script.
Run one section at a time in an authorized **personal** environment. It uses the
implemented CLI primitives to create a fresh HCP with **one guest worker**, seed
200 application Pods, execute two hours of turnover, and export evidence. It does
not mean one management node or a single control-plane replica.

The initial result is an **as-deployed, quiet baseline**, not proof of the
smallest viable HCP, constrained-memory safety, CPU contention, or qualification
of a sizing profile. Stop at unmet gates; retain failures rather than relabeling
them as passing measurements. No Azure or Kubernetes deployment was performed
while writing this guide.

## 1. Select Checkout And Identity

Use the sibling `ARO-HCP-perf-runner` checkout, not the adjacent `ARO-HCP`
checkout. All repository `make` operations below use its root via `make -C`;
all `hcp-perf` commands run from its `tooling/hcp-perf` module directory.

```bash
cd /path/to/Azure/ARO-HCP-perf-runner/tooling/hcp-perf
export PERF_DIR="$PWD"
export REPO_ROOT="$(git rev-parse --show-toplevel)"
test "$PERF_DIR" = "$REPO_ROOT/tooling/hcp-perf"
git -C "$REPO_ROOT" status --short

export AZURE_CONFIG_DIR="$HOME/.azure-redhat"
export DEPLOY_ENV=pers
umask 077
set -o pipefail
set +x
date -u +%Y-%m-%dT%H:%M:%SZ
```

Use `AZURE_CONFIG_DIR="$HOME/.azure-redhat"` for **every** Azure command, including
commands invoked by make, credential helpers, and commands in other terminals.
Do not switch to a Microsoft-tenant login. The date available when this guide was
prepared was **2026-09-08 UTC**, not September 9. A prior report that no Steve
resource group was found is not a current inventory or proof of absence; there
was no live Azure recheck for this documentation change.

```bash
AZURE_CONFIG_DIR="$HOME/.azure-redhat" az account show \
  --query '{subscription:id,name:name,tenant:tenantId,user:user.name}' -o json
AZURE_CONFIG_DIR="$HOME/.azure-redhat" az group list \
  --query '[].{name:name,location:location}' -o table
```

**Checkpoint:** verify the Red Hat tenant and authorized subscription, review the
personal environment names from repository settings, and confirm ownership of
any existing resource groups. A name substring search alone is insufficient.
If authentication is absent, use the personal-dev account explicitly:

```bash
AZURE_CONFIG_DIR="$HOME/.azure-redhat" az login \
  --tenant 64dc69e4-d083-49fc-9569-ebece1dd1408 --use-device-code
AZURE_CONFIG_DIR="$HOME/.azure-redhat" az account set \
  --subscription 1d3378d3-5a3f-4712-85a1-2485495dfc4b
```

Do not delete or adopt existing infrastructure to make a run appear fresh. Follow
[personal-dev setup](../../../docs/personal-dev.md) and
[make options](../../../docs/make-options.md) for prerequisites, region/settings,
container tooling, quota, permissions, and cost. The environment is normally
swept after 48 hours; `PERSIST=true` extends retention to 15 days, not forever.

## 2. Build And Create The Personal Environment

This command creates/updates billable infrastructure and builds/pushes service
images from this checkout. Run it yourself only after reviewing the preceding
checkpoint. Do not use a cleanup target as an implicit setup step.

```bash
AZURE_CONFIG_DIR="$HOME/.azure-redhat" make -C "$REPO_ROOT" personal-dev-env DEPLOY_ENV=pers
export SVC_KUBECONFIG="$(AZURE_CONFIG_DIR="$HOME/.azure-redhat" make -s -C "$REPO_ROOT" infra.svc.aks.kubeconfigfile DEPLOY_ENV=pers)"
export MGMT_KUBECONFIG="$(AZURE_CONFIG_DIR="$HOME/.azure-redhat" make -s -C "$REPO_ROOT" infra.mgmt.aks.kubeconfigfile DEPLOY_ENV=pers)"
kubectl --kubeconfig "$SVC_KUBECONFIG" get nodes
kubectl --kubeconfig "$MGMT_KUBECONFIG" get nodes -L aro-hcp.azure.com/role,agentpool
kubectl --kubeconfig "$MGMT_KUBECONFIG" get storageclass

GOWORK=off go mod download
GOWORK=off go test -race ./...
GOWORK=off go run . --help
```

Go 1.25.7+, Linux, jq, curl, kubectl, and the pinned external kube-burner v1.17.3
executable are required for this workflow. `go.mod`/`go.sum` are authoritative;
the module uses Kubernetes/client-go and YAML dependencies. Avoid globally
exporting `GOWORK=off` into repository provisioning targets.

Prepare private local storage outside the source tree. All CLI `--output`
directories below must be **new**, with an existing parent. Preserve partial
attempts; use new paths for subsequent attempts rather than overwriting evidence.

```bash
export RUN_ID="p$(date -u +%Y%m%d%H%M%S)"
export RUN="$(mktemp -d "$HOME/hcp-perf-${RUN_ID}.XXXXXX")"
mkdir "$RUN/inputs" "$RUN/evidence" "$RUN/private"
export EARLY_ID="${RUN_ID}-early"
export FULL_ID="${RUN_ID}-full"
export EARLY_NS="hcp-perf-$EARLY_ID"
export FULL_NS="hcp-perf-$FULL_ID"
export SEED_ID="${RUN_ID}-seed"
export GUEST_KUBECONFIG="$RUN/private/guest-admin.kubeconfig"
export PRIVATE_CREDENTIALS_DIR="$RUN/private/benchmark"

git -C "$REPO_ROOT" rev-parse HEAD > "$RUN/evidence/source-revision.txt"
git -C "$REPO_ROOT" status --short > "$RUN/evidence/worktree-status.txt"
GOWORK=off go version > "$RUN/evidence/go-version.txt"
GOWORK=off go build -o "$RUN/private/hcp-perf" .
export HCP_PERF="$RUN/private/hcp-perf"
sha256sum "$HCP_PERF" > "$RUN/evidence/binary.sha256"
```

**Checkpoint:** approve real digest-pinned runner, Prometheus, and application
images, plus storage/resources for **two simultaneous captures** and a recorder
on infra nodes. Set `RUNNER_IMAGE`, `PROMETHEUS_IMAGE`, `APP_IMAGE`,
`STORAGE_CLASS`, `LOCATION`, `CUSTOMER_RG`, `CLUSTER_NAME`, and `WORKER_SKU` to
reviewed values. Set `KUBE_BURNER` and `KUBE_BURNER_SHA256` to the trusted
executable and its approved extracted-file hash. The example seed hash is for
Linux x86-64 v1.17.3, not its tarball. Image syntax validation does not establish
provenance or reject all-zero digests. Do not use the example all-zero pins.

```bash
: "${RUNNER_IMAGE:?approved registry/repository@sha256 digest}"
: "${PROMETHEUS_IMAGE:?approved Prometheus image digest}"
: "${APP_IMAGE:?approved application image digest, runnable under guest SCC}"
: "${STORAGE_CLASS:?verified management StorageClass}"
: "${LOCATION:?region matching the personal environment}"
: "${CUSTOMER_RG:?new uniquely named customer resource group}"
: "${CLUSTER_NAME:?new uniquely named HCP cluster}"
: "${WORKER_SKU:?approved one-worker VM SKU with sufficient capacity}"
: "${KUBE_BURNER:?absolute path to trusted v1.17.3 executable}"
: "${KUBE_BURNER_SHA256:?approved executable SHA-256}"
sha256sum "$KUBE_BURNER"
```

The app image must tolerate repeated five-replica rollouts and guest security
policy. The seed template sets no application readiness probe or command and
does not prove that its port-8080 Service serves useful traffic. Approve the
workload contract rather than silently substituting a different application.

## 3. Customer Infrastructure And Payload Gate

`hcp-perf rp` does **not** provision Azure customer infrastructure or construct
an ARM payload from Bicep outputs. It only submits your raw JSON resource object
to the personal RP. Do not pass an ARM deployment template or Bicep parameters
file as `--payload`.

The existing E2E helper is
`CreateClusterCustomerResources20251223` in
`test/util/framework/helpers_v20251223preview.go`. It deploys generated
`modules/customer-infra.json` at resource-group scope and
`modules/managed-identities.json` at subscription scope, then calls the
`PopulateClusterParamsFrom*Deployment20251223` helpers. It is a Go test-framework
helper, **not a hcp-perf command**. Running an arbitrary E2E test is not a safe
payload generator: it can provision its own HCP and register cleanup.

For a manual equivalent, compile those exact source modules without deploying
an HCP. This generates the same ARM template inputs used by E2E, in this run's
private input directory:

```bash
AZURE_CONFIG_DIR="$HOME/.azure-redhat" az bicep build \
  --file "$REPO_ROOT/test/e2e-setup/bicep/modules/customer-infra.bicep" \
  --outfile "$RUN/inputs/customer-infra.arm.json"
AZURE_CONFIG_DIR="$HOME/.azure-redhat" az bicep build \
  --file "$REPO_ROOT/test/e2e-setup/bicep/modules/managed-identities.bicep" \
  --outfile "$RUN/inputs/managed-identities.arm.json"

jq -n --arg cluster "$CLUSTER_NAME" \
  '{parameters:{clusterName:{value:$cluster},persistTagValue:{value:false}}}' \
  > "$RUN/inputs/customer-infra.parameters.json"
```

**Checkpoint:** review the compiled templates, network ranges, Key Vault exposure,
cleanup policy and required role-assignment privileges. Check resource-group
absence and create it only on a confirmed fresh name:

```bash
AZURE_CONFIG_DIR="$HOME/.azure-redhat" az group exists --name "$CUSTOMER_RG"
# Continue only if the preceding result is false and this name is approved.
AZURE_CONFIG_DIR="$HOME/.azure-redhat" az group create \
  --name "$CUSTOMER_RG" --location "$LOCATION" \
  --tags createdAt="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --output none
AZURE_CONFIG_DIR="$HOME/.azure-redhat" az deployment group create \
  --resource-group "$CUSTOMER_RG" --name "$RUN_ID-infra" \
  --template-file "$RUN/inputs/customer-infra.arm.json" \
  --parameters "@$RUN/inputs/customer-infra.parameters.json" \
  --query properties.outputs -o json > "$RUN/private/customer-infra.outputs.json"
```

Supply `IDENTITIES_JSON`, an approved JSON object containing **all 13 names**
required by the compiled `identities` parameter:
`clusterApiAzureMiName`, `controlPlaneMiName`, `cloudControllerManagerMiName`,
`ingressMiName`, `diskCsiDriverMiName`, `fileCsiDriverMiName`,
`imageRegistryMiName`, `cloudNetworkConfigMiName`, `kmsMiName`,
`dpDiskCsiDriverMiName`, `dpFileCsiDriverMiName`, `dpImageRegistryMiName`, and
`serviceManagedIdentityName`. For a fresh non-pooled run these are new names in
the customer RG. Do not borrow identities from a shared MSI pool. The following
explicitly selects `useMsiPool=false`; pooled mode needs the repository's
allocation workflow instead.

```bash
: "${IDENTITIES_JSON:?approved absolute path to the 13-name identity object}"
jq -n --arg rg "$CUSTOMER_RG" --arg cluster "$CLUSTER_NAME" \
  --slurpfile identities "$IDENTITIES_JSON" \
  --slurpfile infra "$RUN/private/customer-infra.outputs.json" '
  {parameters:{
    msiResourceGroupName:{value:$rg},clusterResourceGroupName:{value:$rg},
    clusterName:{value:$cluster},useMsiPool:{value:false},
    rbacScope:{value:"resourceGroup"},identities:{value:$identities[0]},
    nsgName:{value:$infra[0].nsgName.value},
    vnetName:{value:$infra[0].vnetName.value},
    subnetName:{value:$infra[0].vnetSubnetName.value},
    keyVaultName:{value:$infra[0].keyVaultName.value}}}' \
  > "$RUN/inputs/managed-identities.parameters.json"
AZURE_CONFIG_DIR="$HOME/.azure-redhat" az deployment sub create \
  --location "$LOCATION" --name "$RUN_ID-identities" \
  --template-file "$RUN/inputs/managed-identities.arm.json" \
  --parameters "@$RUN/inputs/managed-identities.parameters.json" \
  --query properties.outputs -o json > "$RUN/private/managed-identities.outputs.json"
```

**Required handoff:** supply `CLUSTER_PAYLOAD_FIXTURE` and
`NODEPOOL_PAYLOAD_FIXTURE`, absolute paths to reviewed **request resource JSON**
for `2025-12-23-preview`, generated from the same environment's existing E2E
parameter/build helpers or prepared against that API schema. There is no shipped
ready-to-deploy personal fixture in `examples/rp.json`; that file is connection
metadata only. Until these inputs exist, provisioning is blocked. Integration
test fixtures with fake identities/resource IDs are not deployable substitutes.

Review the full requirement set: location, available OpenShift version/channel,
unique managed RG name, network type and nonoverlapping CIDRs, API visibility and
reachability from the operator, image-registry settings, etcd encryption mode and
KMS key, operator and service identities and Azure roles. For the node pool,
review version/channel, SKU, OS disk type/size, availability zone, auto-repair,
and fixed replicas without autoscaling. `BuildHCPClusterFromParams20251223` and
`BuildNodePoolFromParams20251223` show the exact request shapes. Do not fabricate
version IDs, identity IDs, key versions, or network settings.

For fixtures using customer-managed KMS, bind the actual deployment outputs as
follows. Stop and review a different encryption contract instead of applying
this transformation to an incompatible fixture:

```bash
: "${CLUSTER_PAYLOAD_FIXTURE:?reviewed 2025-12-23-preview cluster request}"
: "${NODEPOOL_PAYLOAD_FIXTURE:?reviewed 2025-12-23-preview nodepool request}"
jq --arg location "$LOCATION" \
  --slurpfile infra "$RUN/private/customer-infra.outputs.json" \
  --slurpfile mi "$RUN/private/managed-identities.outputs.json" '
  .location=$location |
  .identity=$mi[0].identityValue.value |
  .properties.platform.networkSecurityGroupId=$infra[0].nsgID.value |
  .properties.platform.subnetId=$infra[0].vnetSubnetID.value |
  .properties.platform.vnetIntegrationSubnetId=$infra[0].vnetIntegrationSubnetID.value |
  .properties.platform.operatorsAuthentication.userAssignedIdentities=$mi[0].userAssignedIdentitiesValue.value |
  .properties.etcd.dataEncryption.customerManaged.kms.vaultName=$infra[0].keyVaultName.value |
  .properties.etcd.dataEncryption.customerManaged.kms.activeKey.name=$infra[0].etcdEncryptionKeyName.value |
  .properties.etcd.dataEncryption.customerManaged.kms.activeKey.version=$infra[0].etcdEncryptionKeyVersion.value' \
  "$CLUSTER_PAYLOAD_FIXTURE" > "$RUN/inputs/cluster-payload.json"
jq --arg location "$LOCATION" --arg sku "$WORKER_SKU" '
  .location=$location | .properties.platform.vmSize=$sku |
  .properties.replicas=1 | del(.properties.autoScaling)' \
  "$NODEPOOL_PAYLOAD_FIXTURE" > "$RUN/inputs/nodepool-payload.json"
```

These substitutions are not schema validation. Review fixtures for read-only
response fields and case-aliased keys; the RP primitive rejects duplicate/case-
aliased object keys. Hash reviewed inputs; keep payloads and raw Azure outputs
private. There must be no credentials embedded in the request bodies.

## 4. Start Early Worker Capture Before Creating The HCP

This session has a static generic configuration, independent of HCP namespace
names, with live discovery of **all management workers**. Do not generate an HCP
bundle yet: there are no HCP monitors to snapshot. Early cAdvisor collection can
record HCP container startup once those containers appear, but not startup
component metrics, pre-scrape spikes, or historical Kubernetes transitions.

```bash
jq --arg id "$EARLY_ID" --arg image "$PROMETHEUS_IMAGE" --arg sc "$STORAGE_CLASS" '
  .id=$id | .image=$image | .storageClass=$sc | .kubernetesDiscovery=true' \
  examples/capture.json > "$RUN/inputs/capture-early.json"
cat > "$RUN/inputs/prometheus-early.yaml" <<'YAML'
global:
  scrape_interval: 5s
  scrape_timeout: 5s
scrape_configs:
- job_name: prometheus
  static_configs:
  - targets: ["localhost:9090"]
- job_name: worker-cadvisor
  scheme: https
  metrics_path: /metrics/cadvisor
  kubernetes_sd_configs:
  - role: node
  authorization:
    type: Bearer
    credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token
  tls_config:
    ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
  relabel_configs:
  - source_labels: [__meta_kubernetes_node_label_aro_hcp_azure_com_role]
    regex: worker
    action: keep
  - source_labels: [__meta_kubernetes_node_name]
    target_label: node
YAML
```

**Trust preflight:** the CA above is the projected **API** CA. Before accepting
this configuration, establish whether it trusts each discovered kubelet serving
certificate and whether its SAN matches the discovered address. Verify direct
kubelet port reachability from infra, service-account token audience/authentication,
and `nodes/metrics` authorization. Check the approved kubelet CA against actual
serving certificates; API access alone does not validate kubelet TLS. If a
different CA is required, stop and prepare an exact selected-key `tlsMounts`
projection in the capture spec and use its in-Pod path in `ca_file`. A local CA
filename is not a mounted file. The shape and hashed destination are defined in
`internal/capture/tls.go` and `internal/scrape/tls.go`; do not disable verification
or patch the created StatefulSet to conceal a missing input.

Run the selected Prometheus version's `promtool check config` with referenced
files available in its validation environment. Structural CLI validation is not
Prometheus semantic validation. Then start and inspect:

```bash
"$HCP_PERF" capture start --input "$RUN/inputs/capture-early.json" \
  --config "$RUN/inputs/prometheus-early.yaml" --kubeconfig "$MGMT_KUBECONFIG" \
  --timeout 10m --wait=true > "$RUN/evidence/early-start.json"
"$HCP_PERF" capture status --id "$EARLY_ID" --kubeconfig "$MGMT_KUBECONFIG"
```

In a separate terminal with the same variables and Azure config export, keep this
loopback-only forward open:

```bash
export AZURE_CONFIG_DIR="$HOME/.azure-redhat"
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n "$EARLY_NS" \
  port-forward --address 127.0.0.1 service/prometheus 9090:9090
```

Back in the operator terminal:

```bash
unset HCP_PERF_BEARER_TOKEN
curl --fail --silent --show-error 'http://127.0.0.1:9090/api/v1/targets?state=active' \
  > "$RUN/evidence/early-targets.json"
jq -e '.status=="success" and
  ([.data.activeTargets[] | select(.scrapePool=="worker-cadvisor")] |
   length>0 and all(.[]; .health=="up" and .lastError==""))' \
  "$RUN/evidence/early-targets.json"
kubectl --kubeconfig "$MGMT_KUBECONFIG" get nodes \
  -l aro-hcp.azure.com/role=worker -o json \
  | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,allocatable:.status.allocatable}]' \
  > "$RUN/evidence/management-workers.json"
curl --fail --silent --show-error --get 'http://127.0.0.1:9090/api/v1/query' \
  --data-urlencode 'query=count by(node) (container_memory_usage_bytes{job="worker-cadvisor",container!=""})' \
  > "$RUN/evidence/early-cadvisor-series.json"
```

**Checkpoint:** compare the complete worker inventory with targets and nonempty
fresh container samples on every worker, including neighbors, not just the future
HCP's Pods. Confirm source timestamps advance over multiple scrapes. Fix auth/TLS
or missing targets before HCP creation; readiness of Prometheus is not this check.
Capture runs until stopped, irrespective of the start-command timeout. Failed
starts retain partial resources and cannot be resumed with another `start`.

```bash
export EARLY_START="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf '%s\n' "$EARLY_START" > "$RUN/evidence/early-window-start.txt"
```

## 5. Personal RP Creation And Namespace Discovery

Keep the frontend forward running in another terminal:

```bash
export AZURE_CONFIG_DIR="$HOME/.azure-redhat"
kubectl --kubeconfig "$SVC_KUBECONFIG" -n aro-hcp \
  port-forward --address 127.0.0.1 service/aro-hcp-frontend 8443:8443
```

Initialize the subscription in the **personal RP**, not ARM, after checking that
this is the intended newly provisioned frontend. This existing make target
writes registration; `hcp-perf rp` deliberately does not:

```bash
AZURE_CONFIG_DIR="$HOME/.azure-redhat" make -C "$REPO_ROOT" e2e-local/setup \
  FRONTEND_ADDRESS=http://localhost:8443 DEPLOY_ENV=pers
export SUBSCRIPTION_ID="$(AZURE_CONFIG_DIR="$HOME/.azure-redhat" az account show --query id -o tsv)"
export TENANT_ID="$(AZURE_CONFIG_DIR="$HOME/.azure-redhat" az account show --query tenantId -o tsv)"
export OBJECT_ID="$(AZURE_CONFIG_DIR="$HOME/.azure-redhat" az ad signed-in-user show --query id -o tsv)"
jq --arg sub "$SUBSCRIPTION_ID" --arg tenant "$TENANT_ID" --arg oid "$OBJECT_ID" '
  .subscriptionID=$sub | .tenantID=$tenant | .objectID=$oid' \
  examples/rp.json > "$RUN/inputs/rp.json"
jq -n --arg rg "$CUSTOMER_RG" --arg cluster "$CLUSTER_NAME" \
  '{resourceGroup:$rg,cluster:$cluster}' > "$RUN/inputs/cluster-ref.json"
jq '. + {nodePool:"worker"}' "$RUN/inputs/cluster-ref.json" > "$RUN/inputs/nodepool-ref.json"
export RESOURCE_ID="/subscriptions/$SUBSCRIPTION_ID/resourceGroups/$CUSTOMER_RG/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/$CLUSTER_NAME"

"$HCP_PERF" rp create-cluster --input "$RUN/inputs/rp.json" \
  --resource "$RUN/inputs/cluster-ref.json" --payload "$RUN/inputs/cluster-payload.json" \
  --output "$RUN/private/rp-create" --timeout 1h > "$RUN/evidence/rp-create-result.json"
```

This blocks while polling the RP. **During that call**, use another operator
terminal with the same variables to discover the HostedCluster as soon as it
appears. Match the actual ARM resource ID annotation, not a guessed `ocm-steve`
prefix or the first cluster returned:

```bash
export AZURE_CONFIG_DIR="$HOME/.azure-redhat"
kubectl --kubeconfig "$MGMT_KUBECONFIG" get hostedclusters -A -o json \
  | jq --arg rid "$RESOURCE_ID" '[.items[] |
    select((.metadata.annotations["azure.microsoft.com/hcp-cluster-azure-resource-id"] // "" | ascii_downcase) == ($rid | ascii_downcase)) |
    {namespace:.metadata.namespace,name:.metadata.name,uid:.metadata.uid,clusterID:.spec.clusterID}]' \
  > "$RUN/evidence/hostedcluster-identity.json"
jq -e 'length==1' "$RUN/evidence/hostedcluster-identity.json"
export HC_NS="$(jq -r '.[0].namespace' "$RUN/evidence/hostedcluster-identity.json")"
export HC_NAME="$(jq -r '.[0].name' "$RUN/evidence/hostedcluster-identity.json")"
kubectl --kubeconfig "$MGMT_KUBECONFIG" get hostedcontrolplanes -A \
  -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,CLUSTER_ID:.spec.clusterID
```

Repeat the discovery read manually if the cluster is not present. Select and
verify `HCP_NS`, the control-plane namespace belonging to this HC/cluster ID;
it is normally `${HC_NS}-${HC_NAME}`, but confirm the actual HostedControlPlane
and Pod ownership instead of relying on that naming convention alone. If the
annotation is absent or ambiguous, stop and resolve the RP/CS/HC mapping.

Start the recorder **now**, in the already-created early capture namespace:

```bash
: "${HCP_NS:?verified target HostedControlPlane namespace}"
jq --arg ns "$EARLY_NS" --arg hc "$HC_NS" --arg hcp "$HCP_NS" \
  --arg image "$RUNNER_IMAGE" --arg sc "$STORAGE_CLASS" '
  .namespace=$ns | .image=$image | .storageClass=$sc |
  .namespaces=([$hc,$hcp]|unique) | .durationSeconds=21600 |
  .enableWatch=true | .enableLogs=false | .rawLogs=false |
  .maxConcurrentLogs=256 | .maxStreamBytes=8589934592 |
  .cpuRequestMilli=500 | .memoryRequestMi=512 | .memoryLimitMi=2048 |
  .maxObservationBytes=8589934592 | .storageGi=32' examples/record.json > "$RUN/inputs/record.json"
"$HCP_PERF" record start --input "$RUN/inputs/record.json" \
  --kubeconfig "$MGMT_KUBECONFIG" --timeout 5m > "$RUN/evidence/record-start.json"
"$HCP_PERF" record status --namespace "$EARLY_NS" --id observations \
  --kubeconfig "$MGMT_KUBECONFIG" > "$RUN/evidence/record-status.json"
```

The recorder has no namespace regex, RP lookup, or future-prefix discovery. It
requires explicit namespaces and starts **after HC discovery** here; therefore
initial HCP objects/transitions/logs may already be lost. Listing current objects
does not reconstruct them. Waiting for `rp create-cluster` to finish before this
step makes that startup gap larger. This run cannot qualify complete startup
behavior, even if its early cAdvisor data is useful. Check six-hour recorder
duration against actual setup delays; if exhausted, use a new recorder ID and
record the gap, never assume a restart resumes the stream.

For raw logs, before creating the recorder, explicitly approve the sensitivity
and change **both** `enableLogs` and `rawLogs` to `true`. There is no separate
`--requireAck` flag: `rawLogs:true` is the required acknowledgement. Logs are
unsanitized, may contain secrets, and have best-effort restart/rotation coverage.
Watch output is sanitized but not lossless. Collection scopes do not narrow the
recorder's cluster-wide read/watch/log RBAC. Review that grant.

Keep the next-run input explicitly at `maxConcurrentLogs:256`; include all init,
regular and ephemeral containers plus previous-log requests in the estimate.
This leaves headroom for about 220 concurrent streams, not unlimited restarts.
The recorder requests 500m CPU/512 MiB memory with a 2 GiB memory limit on infra
nodes. Capacity skips and exhausted retries mean partial capture, not complete
evidence; later recovery does not erase an earlier gap. Clean terminal EOF is
verified against fresh Pod UID/restart status before completing the stream.
Do not retrofit new limits or classifications into previously captured archives.

After cluster create reports `finalState: Succeeded`, create the one-worker pool:

```bash
jq -e '.finalState=="Succeeded"' "$RUN/evidence/rp-create-result.json"
"$HCP_PERF" rp create-nodepool --input "$RUN/inputs/rp.json" \
  --resource "$RUN/inputs/nodepool-ref.json" --payload "$RUN/inputs/nodepool-payload.json" \
  --output "$RUN/private/rp-nodepool" --timeout 1h > "$RUN/evidence/rp-nodepool-result.json"
jq -e '.finalState=="Succeeded"' "$RUN/evidence/rp-nodepool-result.json"
"$HCP_PERF" rp credentials --input "$RUN/inputs/rp.json" \
  --resource "$RUN/inputs/cluster-ref.json" --output "$RUN/private/rp-credentials" \
  --credentials-output "$GUEST_KUBECONFIG" --timeout 10m \
  > "$RUN/evidence/rp-credential-receipt.json"
kubectl --kubeconfig "$GUEST_KUBECONFIG" get nodes
kubectl --kubeconfig "$GUEST_KUBECONFIG" wait --for=condition=Ready node --all --timeout=15m
kubectl --kubeconfig "$GUEST_KUBECONFIG" get nodes -o json \
  | jq -e '.items|length==1'
```

Keep the admin kubeconfig private and separate from workload identities. The RP
credential response body is never put in result JSON/artifacts by the primitive.
Timeouts do not roll back accepted operations. Inspect the saved operation URL
and use `rp get` with a new output directory instead of blind create retries.

## 6. Sizing And Full Scrape Gate

Record what actually runs before interpreting any results:

```bash
kubectl --kubeconfig "$MGMT_KUBECONFIG" get clustersizingconfiguration cluster -o json \
  > "$RUN/evidence/deployed-sizing.json"
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n "$HCP_NS" get pods -o json \
  | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,node:.spec.nodeName,
      containers:[(.spec.initContainers[]?,.spec.containers[]?) |
        {name,image,resources,runtimeEnv:[.env[]? |
          select(.name=="GOMEMLIMIT" or .name=="GOMAXPROCS")]}]}]' \
  > "$RUN/evidence/hcp-container-policy.json"
```

`sizing hypershift` now renders the experimental all-container policy described
in [Personal HyperShift Sizing](hypershift-sizing.md), including a candidate
profile and an explicit apply gate after custom HO/CPO readiness. It never
applies the artifact. `sizing resolve` is not a deployable CSC. A constrained case
still needs verified kind/workload/container/init coverage,
CPU requests without CPU limits, memory request/safety-limit policy, applicable
Go runtime settings, and verification of actual generated Pods.

The existing experimental-minimal path is `limitClusterSizes` in this checkout
(sometimes called `experimentalMinimal` in discussion): it selects `e2e_minimal`
with 100Mi memory/100m CPU **requests for seven entries**, inflated inflight
settings, and no general memory-limit/GOMEMLIMIT projection. Personal rendered
defaults enable it. It is not a complete smallest-size policy or evidence of
memory safety. Do not patch live Pods and call the result reconciler-supported.
Keep runs using the unchanged chart labeled `as-deployed-baseline`; a rendered
candidate alone does not establish live projection, coverage, or memory safety.

Now inspect actual component monitors and exporter availability:

```bash
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n "$HCP_NS" get servicemonitors,podmonitors
kubectl --kubeconfig "$MGMT_KUBECONFIG" get services -A \
  -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,PORTS:.spec.ports
kubectl --kubeconfig "$MGMT_KUBECONFIG" get daemonsets -A
```

**Exporter blocker:** `observability/prometheus/values-mgmt.yaml` disables
`nodeExporter`. Do not assume `prometheus/node-exporter:metrics` exists. An
authorized operator must supply/verify a worker-wide exporter Service with real
EndpointSlices, node metadata, and a named TCP metrics port. Set
`NODE_EXPORTER_NS`, `NODE_EXPORTER_SERVICE`, and `NODE_EXPORTER_PORT` only after
checking the actual Service and serving protocol. The generator's explicit
node-exporter job is **HTTP only**. If the actual exporter requires TLS/auth,
its ServiceMonitor does not magically configure that explicit job: stop for a
reviewed exporter/config solution or generator change. Never silently scrape a
TLS port over HTTP or disable certificate verification.

```bash
: "${NODE_EXPORTER_NS:?verified HTTP exporter namespace}"
: "${NODE_EXPORTER_SERVICE:?verified worker exporter Service}"
: "${NODE_EXPORTER_PORT:?actual named TCP metrics port}"
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n "$NODE_EXPORTER_NS" \
  get service "$NODE_EXPORTER_SERVICE" -o yaml
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n "$NODE_EXPORTER_NS" \
  get endpointslices -l "kubernetes.io/service-name=$NODE_EXPORTER_SERVICE"
jq --arg hcp "$HCP_NS" --arg ns "$FULL_NS" --arg cluster "$RUN_ID-mgmt" \
  --arg ens "$NODE_EXPORTER_NS" --arg svc "$NODE_EXPORTER_SERVICE" \
  --arg port "$NODE_EXPORTER_PORT" '
  .targetNamespaces=[$hcp] | del(.namespaceRegex) | .clusterID=$cluster |
  .prometheusNamespace=$ns | .prometheusServiceAccount="prometheus" |
  .nodeExporterServiceNamespace=$ens | .nodeExporterServiceName=$svc |
  .nodeExporterPort=$port' examples/scrape.json > "$RUN/inputs/scrape.json"
"$HCP_PERF" scrape generate --input "$RUN/inputs/scrape.json" \
  --kubeconfig "$MGMT_KUBECONFIG" --output "$RUN/evidence/scrape-full" --timeout 5m \
  > "$RUN/evidence/scrape-generation.json"
jq -e '.gaps|length==0' "$RUN/evidence/scrape-full/bundle.json"
```

Add other **verified monitor-bearing** namespaces to `targetNamespaces` if
needed. Every listed namespace without a monitor is a blocker. Inspect all
`sources`, `expectedJobs`, `rbac`, `tlsMounts`, and `gaps`; partial generation
returns nonzero and must not be treated as a complete bundle. Component TLS
references can be copied by capture; generator overrides such as
`componentCAFile`, `componentServerName`, `kubeletCAFile`, and
`kubeletServerName` refer to verified in-Pod trust/identity, not convenient ways
to ignore a bad certificate. Arbitrary local paths are not copied.

```bash
jq --arg id "$FULL_ID" '.id=$id' "$RUN/inputs/capture-early.json" \
  > "$RUN/inputs/capture-full.json"
"$HCP_PERF" capture start --input "$RUN/inputs/capture-full.json" \
  --scrape-bundle "$RUN/evidence/scrape-full/bundle.json" \
  --kubeconfig "$MGMT_KUBECONFIG" --timeout 10m --wait=true \
  > "$RUN/evidence/full-start.json"
```

If early capture required explicit spec `tlsMounts`, do not copy those into this
input: `--scrape-bundle` cannot be combined with spec mounts. Resolve the full
bundle's required trust projections separately before starting. Capture copies
only specified source keys into new immutable destination objects and grants
Prometheus no Secret-read permission. Approve source GET permissions, storage of
client keys, certificate lifetimes, and lack of automatic rotation. The actual
capture ServiceAccount is `prometheus`, not the scrape example's default
`hcp-perf`. Its namespace gets the HCP monitoring network-policy label.

Keep the early capture running. Start a second forward in another terminal:

```bash
export AZURE_CONFIG_DIR="$HOME/.azure-redhat"
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n "$FULL_NS" \
  port-forward --address 127.0.0.1 service/prometheus 9091:9090
```

Then check the new session:

```bash
"$HCP_PERF" scrape check --input "$RUN/evidence/scrape-full/bundle.json" \
  --endpoint http://127.0.0.1:9091 --timeout 1m > "$RUN/evidence/scrape-check-before.json"
export FULL_START="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf '%s\n' "$FULL_START" > "$RUN/evidence/full-window-start.txt"
```

**Checkpoint:** compare every required component replica, management worker,
container and exporter with actual fresh samples, not just one healthy target
per scrape pool. `scrape check` cannot detect missing replicas or prove metrics
coverage/freshness. Monitor creation/spec changes require regeneration and a
**new capture ID** because capture ConfigMaps are immutable. Kubernetes SD only
updates targets within the generated monitor snapshot. New full capture has no
pre-provision HCP component data; retain the early capture as a separate artifact
under the same run ID. Do not splice or zero-fill the startup gap.

## 7. Verify Worker Capacity And Seed

One guest node receives two namespaces, each with 20 applications and 100 Pods:
**40 five-replica applications, 200 app Pods total**, not 200 applications. At
the example 10m/16Mi requests that is 2 CPU and 3200Mi before system Pods.
RollingUpdate allows one surge Pod per outstanding app: with 20 outstanding,
budget at least 20 extra app Pods, plus terminating Pods, system services and
the canary. Requests are not usage limits; verify measured memory headroom,
kubelet allocatable Pod capacity, networking/IP capacity, and system reservations
for the chosen SKU. Do not reduce density or add workers and retain the same
case label. An undersized worker is a provisioning gate, not HCP-size evidence.

```bash
kubectl --kubeconfig "$GUEST_KUBECONFIG" describe nodes
kubectl --kubeconfig "$GUEST_KUBECONFIG" get pods -A -o wide
kubectl --kubeconfig "$GUEST_KUBECONFIG" top nodes
jq --arg id "$SEED_ID" --arg image "$APP_IMAGE" \
  --arg binary "$KUBE_BURNER" --arg hash "$KUBE_BURNER_SHA256" '
  .id=$id | .namespacePrefix=$id | .guestNodes=1 | .applications=40 | .image=$image |
  .binary.path=$binary | .binary.expectedSHA256=$hash' \
  examples/seed.json > "$RUN/inputs/seed.json"
"$HCP_PERF" seed render --input "$RUN/inputs/seed.json" \
  --output "$RUN/evidence/seed-render" > "$RUN/evidence/seed-render-result.json"
```

Review the rendered object/resource contract before running. No pre-pull occurs;
the upstream preloader is deliberately disabled because its namespace cleanup is
not safely scoped. Finish image pulls and warmup outside the scored window. A
successful kube-burner process is not a substitute for this readiness check:

```bash
"$HCP_PERF" seed run --input "$RUN/inputs/seed.json" \
  --kubeconfig "$GUEST_KUBECONFIG" --output "$RUN/private/seed-run" --timeout 1h \
  > "$RUN/evidence/seed-result.json"
kubectl --kubeconfig "$GUEST_KUBECONFIG" -n "$SEED_ID-0" \
  wait --for=condition=Available deployment --all --timeout=15m
kubectl --kubeconfig "$GUEST_KUBECONFIG" -n "$SEED_ID-1" \
  wait --for=condition=Available deployment --all --timeout=15m
kubectl --kubeconfig "$GUEST_KUBECONFIG" get deployments -A \
  -l "hcp-perf.azure.com/run-id=$SEED_ID" -o json \
  | jq -e '.items | length==40 and all(.[];
      .spec.replicas==5 and .status.observedGeneration==.metadata.generation and
      .status.updatedReplicas==5 and .status.readyReplicas==5 and .status.availableReplicas==5)'
kubectl --kubeconfig "$GUEST_KUBECONFIG" get pods -A \
  -l "hcp-perf.azure.com/run-id=$SEED_ID" -o json \
  | jq -e '.items | length==200 and all(.[];
      .metadata.deletionTimestamp==null and
      any(.status.conditions[]?; .type=="Ready" and .status=="True"))'
```

If `top` is unavailable, supply a separately verified guest memory measurement;
management-worker cAdvisor does not measure guest worker capacity. Stop for
Pending/evicted/OOM Pods, missing readiness, or insufficient memory headroom.

## 8. Namespace Identities And Manual APF

```bash
"$HCP_PERF" seed identities --input "$RUN/inputs/seed.json" \
  --kubeconfig "$GUEST_KUBECONFIG" --timeout 10m > "$RUN/evidence/seed-identities.json"
"$HCP_PERF" seed credentials --input "$RUN/inputs/seed.json" \
  --kubeconfig "$GUEST_KUBECONFIG" --output "$PRIVATE_CREDENTIALS_DIR" \
  --expiration 6h --timeout 10m > "$RUN/evidence/credential-metadata.json"
jq '{expirationSeconds,files:[.files[]|{namespace,validUntil}]}' \
  "$PRIVATE_CREDENTIALS_DIR/credentials.json"
```

These are per-namespace `benchmark` SA credentials with exact owned-Deployment
update and Pod/ReplicaSet read permissions. There is no admin fallback. The
issued lifetime can be shorter than requested; require actual `validUntil` to
cover warmup, two hours, recovery, and margin. Renew into a new private directory
before the run if needed. Never print kubeconfig contents, inline tokens, or
credential response bodies; do not use shell tracing, verbose HTTP traces, or
publish `$RUN/private`. The manifest contains expiry metadata, not token bodies.

**APF is not implemented by these CLI commands.** Install policy manually with
guest-admin kubectl after reviewing the API version, competing FlowSchemas and
their precedence. For this one-node example the following creates two explicit
Limited levels: benchmark uses Reject (no queue), and canary gets a separate
bounded share with no lending/borrowing. The numeric shares/precedence below are
an illustrative policy to review and freeze, not a measured optimal allocation.
Neither identity is `Exempt`; never run them as `system:masters`.

```bash
export CANARY_NS="${RUN_ID}-canary"
jq -n --arg prefix "$SEED_ID" --arg run "$RUN_ID" --arg canary "$CANARY_NS" '
  def level($name;$shares):
    {apiVersion:"flowcontrol.apiserver.k8s.io/v1",kind:"PriorityLevelConfiguration",
     metadata:{name:$name},spec:{type:"Limited",limited:{nominalConcurrencyShares:$shares,
       lendablePercent:0,borrowingLimitPercent:0,limitResponse:{type:"Reject"}}}};
  def flow($name;$precedence;$subjects;$rules):
    {apiVersion:"flowcontrol.apiserver.k8s.io/v1",kind:"FlowSchema",metadata:{name:$name},
     spec:{matchingPrecedence:$precedence,priorityLevelConfiguration:{name:$name},
       distinguisherMethod:{type:"ByUser"},rules:[{subjects:$subjects,resourceRules:$rules}]}};
  {apiVersion:"v1",kind:"List",items:[
    level($run+"-benchmark";1), level($run+"-canary";10),
    flow($run+"-benchmark";901;
      [range(0;2)|{kind:"ServiceAccount",serviceAccount:{namespace:($prefix+"-"+tostring),name:"benchmark"}}];
      [{verbs:["get","update","list","watch"],apiGroups:["","apps"],
        resources:["deployments","replicasets","pods"],namespaces:[$prefix+"-0",$prefix+"-1"],clusterScope:false}]),
    flow($run+"-canary";900;
      [{kind:"ServiceAccount",serviceAccount:{namespace:$canary,name:"canary"}}];
      [{verbs:["get","update","list"],apiGroups:["","apps"],
        resources:["deployments","deployments/scale","replicasets","pods"],namespaces:[$canary],clusterScope:false}])
  ]}' > "$RUN/inputs/apf.json"
kubectl --kubeconfig "$GUEST_KUBECONFIG" get flowschemas,prioritylevelconfigurations
kubectl --kubeconfig "$GUEST_KUBECONFIG" create --dry-run=server -f "$RUN/inputs/apf.json"
# Mutating checkpoint: run only after reviewing names, rules and precedence.
kubectl --kubeconfig "$GUEST_KUBECONFIG" create -f "$RUN/inputs/apf.json"
```

`create` deliberately refuses existing names; do not turn a collision into a
blind apply. APF matches requests, not RBAC grants; these objects grant no access
to the canary fixture. Supply `CANARY_FIXTURE` and `CANARY_GUEST_KUBECONFIG` via a
reviewed manual fixture/credential procedure before continuing:

- A new `CANARY_NS` namespace, `canary` SA with token automount disabled, and a
  `canary` Deployment converged at **zero** replicas using the approved warm image.
- A namespace Role/RoleBinding granting that SA get on the named Deployment,
  get/update on its `deployments/scale`, and list on ReplicaSets and Pods only.
- A private, short-lived token kubeconfig for that SA, trusted guest CA/server,
  no exec/admin fallback, and verified expiration covering the run. Token output
  must go directly to private files, not the terminal or command-line arguments.

```bash
: "${CANARY_FIXTURE:?reviewed namespace, zero-replica Deployment, SA and scoped RBAC manifest}"
# A server dry-run cannot validate namespaced objects in a not-yet-created namespace.
# Review the fixture locally, then create its Namespace before its namespaced objects.
kubectl --kubeconfig "$GUEST_KUBECONFIG" create -f "$CANARY_FIXTURE"
# Issue the private canary SA credential now, using the reviewed procedure above.
: "${CANARY_GUEST_KUBECONFIG:?private bounded canary identity kubeconfig}"
kubectl --kubeconfig "$CANARY_GUEST_KUBECONFIG" auth whoami
kubectl --kubeconfig "$PRIVATE_CREDENTIALS_DIR/$SEED_ID-0.kubeconfig" auth whoami
kubectl --kubeconfig "$PRIVATE_CREDENTIALS_DIR/$SEED_ID-1.kubeconfig" auth whoami
kubectl --kubeconfig "$CANARY_GUEST_KUBECONFIG" -n "$CANARY_NS" \
  auth can-i update deployments.apps/canary --subresource=scale
kubectl --kubeconfig "$PRIVATE_CREDENTIALS_DIR/$SEED_ID-0.kubeconfig" \
  -n "$SEED_ID-1" auth can-i update deployments.apps/app-000020
```

The last answer must be **no**. Verify exact server-side RBAC and APF
classification independently, including no dangling FlowSchemas and no
earlier-matching exemption. The primitives do not capture HTTP APF response
headers (`X-Kubernetes-PF-FlowSchema-UID` and
`X-Kubernetes-PF-PriorityLevel-UID`). Use reviewed header-only instrumentation
and APF metrics to prove actual classification; do not enable HTTP body/token
debugging. Until that evidence exists, protected-canary/APF qualification is
blocked. If policy/fixture cannot be supplied, do not call the baseline protected.

## 9. Baseline, Turnover And Optional Contention

Run five **consecutive**, nonoverlapping successful canary cycles after warmup,
saving each result and exit status. A failed cycle is a failure, not a sample to
discard. The small loop repeats only a CLI primitive, not infrastructure setup:

```bash
for cycle in 1 2 3 4 5; do
  if "$HCP_PERF" canary run --namespace "$CANARY_NS" --deployment canary \
    --kubeconfig "$CANARY_GUEST_KUBECONFIG" --timeout 5m \
    --ack-budget 30s --ready-budget 2m --cleanup-budget 2m \
    > "$RUN/evidence/canary-baseline-$cycle.json"; then
    printf '0\n' > "$RUN/evidence/canary-baseline-$cycle.exit"
  else
    printf '%s\n' "$?" > "$RUN/evidence/canary-baseline-$cycle.exit"
    break
  fi
done
```

Manually check all five exit files and successful stage completions. The library
deadline rule is `min(3 * maximum successful baseline duration, absolute budget)`
per stage; acknowledgement/ready/cleanup absolute budgets are 30s/120s/120s.
Readiness includes acknowledgement. There is no baseline-orchestration/deadline
CLI command: record the calculation and use the resulting explicit duration flags
for serial canaries every five minutes during steady state, without overlapping
cycles. A late/failed canary must be recorded rather than silently skipped.

For each manually scheduled steady-state cycle, set a new `CANARY_CYCLE` and the
three calculated Go-duration budget strings, then run this in the canary terminal.
Record the intended five-minute cadence separately from actual start times;
repeating this command immediately is not the same cadence:

```bash
: "${CANARY_CYCLE:?unique cycle number for this run}"
: "${ACK_BUDGET:?calculated acknowledgement duration, at most 30s}"
: "${READY_BUDGET:?calculated readiness duration, at most 120s}"
: "${CLEANUP_BUDGET:?calculated cleanup duration, at most 120s}"
"$HCP_PERF" canary run --namespace "$CANARY_NS" --deployment canary \
  --kubeconfig "$CANARY_GUEST_KUBECONFIG" --timeout 5m \
  --ack-budget "$ACK_BUDGET" --ready-budget "$READY_BUDGET" --cleanup-budget "$CLEANUP_BUDGET" \
  > "$RUN/evidence/canary-steady-$CANARY_CYCLE.json"
printf '%s\n' "$?" > "$RUN/evidence/canary-steady-$CANARY_CYCLE.exit"
```

The default workflow runs **no CPU adversaries**. Do not claim a CPU-minimum or
contention result from raw CPU usage under this quiet baseline. A separately
approved contended case may use [the adversary start/status/stop commands](../README.md#live-cpu-adversaries)
with the full capture namespace, exact target HCP namespace, runner digest,
explicit reserve/memory bounds and duration covering calibration, settle and the
intended steady window. Submission or Running status alone is not calibration.
Require all relevant adversaries Ready, verified container/node scrape coverage,
and measured contention on every current HCP worker with memory headroom before
starting the scored window. Placement changes require reassessment. Without
adversary readiness **and** scrape/sample checks, no contention/CPU-sizing claim
is valid. Stop adversaries and observe recovery separately; turnover does not
start, calibrate, or stop them.

Create a one-node schedule with the same seed/prefix. The illustrative profile
is used only to satisfy offline case-plan validation, never as applied policy:

```bash
jq '.name="personal-one-node-quiet" | .guestNodes=1 | .cpuTreatment="quiet"' \
  examples/case.json > "$RUN/inputs/case.json"
"$HCP_PERF" case schedule --case "$RUN/inputs/case.json" --profile examples/small.json \
  > "$RUN/evidence/intended-schedule.json"
jq --arg prefix "$SEED_ID" '.namespacePrefix=$prefix | .guestNodes=1 |
  .seed=42 | .durationSeconds=7200 | .recoverySeconds=600 | .maxOutstanding=20' \
  examples/turnover.json > "$RUN/inputs/turnover.json"
"$HCP_PERF" scrape check --input "$RUN/evidence/scrape-full/bundle.json" \
  --endpoint http://127.0.0.1:9091 > "$RUN/evidence/scrape-check-steady.json"
```

**Checkpoint:** all readiness/capacity/trust/identity gates passed, images warm,
stable settling interval recorded, canary baseline complete, no unaccounted
competitors or concurrent writers, recorder running, both captures healthy, and
credentials/certificates/storage valid through export. Record any qualification
blockers separately even if proceeding with a diagnostic baseline.

For the quiet baseline, observe at least the example's 300-second settling period
without new workload producers. Record its start/end and health; elapsed time
alone does not establish stability. Freeze and hash the reviewed input files:

```bash
date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/evidence/settle-start.txt"
sleep 300
date -u +%Y-%m-%dT%H:%M:%SZ > "$RUN/evidence/settle-end.txt"
sha256sum "$RUN/inputs/"*.json "$RUN/inputs/"*.yaml > "$RUN/evidence/inputs.sha256"
```

```bash
export STEADY_START="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
jq -n --arg id "$RUN_ID" --arg early "$EARLY_ID" --arg full "$FULL_ID" \
  --arg earlyStart "$EARLY_START" --arg fullStart "$FULL_START" \
  --arg steady "$STEADY_START" --arg rid "$RESOURCE_ID" --arg hc "$HC_NS" --arg hcp "$HCP_NS" \
  --arg sku "$WORKER_SKU" --arg runner "$RUNNER_IMAGE" --arg prom "$PROMETHEUS_IMAGE" --arg app "$APP_IMAGE" '
  {runID:$id,resourceID:$rid,guestNodes:1,workerSKU:$sku,seed:42,
   policy:"as-deployed-baseline",cpuTreatment:"quiet",startupQualified:false,
   captures:[{id:$early,start:$earlyStart},{id:$full,start:$fullStart}],
   steadyRequestedAt:$steady,hostedClusterNamespace:$hc,controlPlaneNamespace:$hcp,
   images:{runner:$runner,prometheus:$prom,application:$app},
   admissionSeconds:7200,recoverySeconds:600,maxOutstanding:20}' \
  > "$RUN/evidence/run.json"
"$HCP_PERF" turnover run --input "$RUN/inputs/turnover.json" \
  --credentials-dir "$PRIVATE_CREDENTIALS_DIR" --output "$RUN/evidence/turnover" \
  > "$RUN/evidence/turnover-result.json"
export WINDOW_END="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf '%s\n' "$WINDOW_END" > "$RUN/evidence/window-end.txt"
```

Keep the turnover terminal alive for up to **2h10m**, with canaries and health
checks in another terminal. The actual start/admission-end/finish timestamps in
`turnover/result.json` are authoritative; the shell timestamp is only a boundary
marker. There are 80 scheduled app rollouts, one app every 90 seconds in each
hour. Shortening the duration truncates admissions; it does not accelerate the
schedule. The configured ten-minute recovery admits no new work and remains
within the run even when no work is outstanding. Interruptions/failures can end
the process earlier; use the recorded timestamps, not the intended duration.

Inspect `scheduled`, `admitted`, `accepted`, `completed`, `failed`, `notStarted`,
`backlog`, and `outstanding`, plus per-intent timing in `events.ndjson` and
`result.json`. A zero exit is not proof that all scheduled work completed. There
is no automatic repair, rollback, resume, or repeat on an ambiguous write.

## 10. Export Before Stopping Capture

Record the final scrape check and export while both Prometheus servers remain
running. Source timestamps are essential: PromQL lookback can repeat old samples
and make step coverage look complete. This query set is a starting evidence set,
not a complete policy/capacity analyzer; review required component/APF metrics
against actual exposed names and add scoped queries before the run.

```bash
"$HCP_PERF" scrape check --input "$RUN/evidence/scrape-full/bundle.json" \
  --endpoint http://127.0.0.1:9091 > "$RUN/evidence/scrape-check-after.json"
cat > "$RUN/inputs/queries.json" <<'JSON'
[
  {"name":"up","expr":"up"},
  {"name":"scrape-time","expr":"timestamp(up)"},
  {"name":"scrape-samples","expr":"scrape_samples_scraped"},
  {"name":"container-memory","expr":"container_memory_usage_bytes{container!=\"\"}"},
  {"name":"container-memory-time","expr":"timestamp(container_memory_usage_bytes{container!=\"\"})"},
  {"name":"container-working-set","expr":"container_memory_working_set_bytes{container!=\"\"}"},
  {"name":"container-cpu-counter","expr":"container_cpu_usage_seconds_total{container!=\"\"}"},
  {"name":"node-memory-available","expr":"node_memory_MemAvailable_bytes"},
  {"name":"apiserver-requests","expr":"apiserver_request_total"},
  {"name":"apf-rejections","expr":"apiserver_flowcontrol_rejected_requests_total"}
]
JSON
"$HCP_PERF" metrics export --endpoint http://127.0.0.1:9090 \
  --queries "$RUN/inputs/queries.json" --start "$EARLY_START" --end "$WINDOW_END" \
  --step 5s --output "$RUN/evidence/metrics-early" > "$RUN/evidence/metrics-early-result.json"
"$HCP_PERF" metrics export --endpoint http://127.0.0.1:9091 \
  --queries "$RUN/inputs/queries.json" --start "$FULL_START" --end "$WINDOW_END" \
  --step 5s --output "$RUN/evidence/metrics-full" > "$RUN/evidence/metrics-full-result.json"
"$HCP_PERF" analyze --input "$RUN/evidence/metrics-early" > "$RUN/evidence/analysis-early.json"
"$HCP_PERF" analyze --input "$RUN/evidence/metrics-full" > "$RUN/evidence/analysis-full.json"
```

All-worker container selectors are intentional: preserve neighbor/adversary
evidence and filter offline by namespace, Pod UID/container/node identity. These
servers are dedicated to this run. Do not issue broad queries against shared
Prometheus without scoped selectors. The early session has no node-exporter or
component metrics, so those early query results are expected gaps, **not zeros**.
Export those gaps honestly or use a separately declared early query subset.
Queries have bounded per-query requests and export can fail for large responses;
use smaller explicit windows/query sets with new directories, recording their
boundaries. Preserve nonzero-exit partial manifests/responses.

The analyzer verifies metrics artifact hashes and per-series sample/step coverage.
It does not join recorder/turnover timelines, establish freshness by itself,
detect all missing replicas, implement cgroup budget checks, or issue a capacity
verdict. Missing/nonfinite samples, resets, OOMs, failed rollouts and collection
gaps require explicit analysis, not averaging away.

Stop the recorder only after the desired observation/recovery window:

```bash
"$HCP_PERF" record stop --namespace "$EARLY_NS" --id observations \
  --kubeconfig "$MGMT_KUBECONFIG" > "$RUN/evidence/record-stop.json"
"$HCP_PERF" record status --namespace "$EARLY_NS" --id observations \
  --kubeconfig "$MGMT_KUBECONFIG" > "$RUN/evidence/record-final-status.json"
```

Poll `record status` manually until writer Pods are gone and the Job is completed,
terminal Failed, or suspension is acknowledged. Failed recorders are exportable;
do not delete the Job/PVC to clear failure. Status is not the recording summary.

```bash
"$HCP_PERF" record export --namespace "$EARLY_NS" --id observations \
  --kubeconfig "$MGMT_KUBECONFIG" --output "$RUN/private/record-export" --timeout 10m \
  > "$RUN/evidence/record-export-result.json"
jq '{sha256,bytes,helperCleanupFailed,limitations}' "$RUN/private/record-export/manifest.json"
sha256sum "$RUN/private/record-export/archive.tar"
tar -tf "$RUN/private/record-export/archive.tar"
```

**Export checkpoint:** compare the archive hash with the manifest, inspect
allowlisted snapshots/checkpoints/summaries and watch/log gaps privately, and
check helper cleanup. The helper mounts the PVC read-only, uses the registered
runner digest, and executes the runner's archive primitive through exec/SPDY,
not `kubectl cp`/a shell in the scratch image. It requires Pod create/get/delete,
`pods/exec` authorization, working transport, infra capacity, volume reattachment,
and correct UID/GID/read permissions. These have implementation tests but were
not live-verified on this personal environment by this guide. A five-minute
helper bound still applies even with `--timeout 10m`.

Export never removes the PVC. An ordinary export error removes incomplete local
output; helper-cleanup failure retains it with a failure manifest. Preserve the
PVC and returned helper identity, investigate, and retry to a **new** output path
only after the helper/writer checks pass. A hash proves byte integrity, not
complete recording. Raw logs are not sanitized by export and can contain secrets.
Keep the archive private even if its filename looks like an ordinary artifact.

Once metrics and recorder evidence are verified and safely backed up:

```bash
"$HCP_PERF" capture stop --id "$FULL_ID" --kubeconfig "$MGMT_KUBECONFIG" --timeout 10m --wait=true
"$HCP_PERF" capture stop --id "$EARLY_ID" --kubeconfig "$MGMT_KUBECONFIG" --timeout 10m --wait=true
"$HCP_PERF" capture status --id "$FULL_ID" --kubeconfig "$MGMT_KUBECONFIG"
"$HCP_PERF" capture status --id "$EARLY_ID" --kubeconfig "$MGMT_KUBECONFIG"
```

Stopping capture removes HTTP access but retains PVCs/resources. Stop any optional
adversaries first and confirm stopped status. Never use namespace cleanup as a
way to stop active producers or as a substitute for export.

## 11. Optional Destructive Cleanup

This is a separate operator decision after evidence review and backup. There is
no trap, automated resource-group deletion, or failure-path cleanup in this guide.
`capture cleanup` destroys the capture namespace and its PVC evidence, including
the recorder PVC in the early namespace. It requires zero capture replicas, no
nonterminal Pods, and terminal/suspended Jobs; deletion accepted is not deletion
complete. Only run these commands if that destruction is explicitly intended:

```bash
"$HCP_PERF" capture cleanup --id "$FULL_ID" --delete-evidence --kubeconfig "$MGMT_KUBECONFIG"
"$HCP_PERF" capture cleanup --id "$EARLY_ID" --delete-evidence --kubeconfig "$MGMT_KUBECONFIG"
"$HCP_PERF" rp delete --input "$RUN/inputs/rp.json" --resource "$RUN/inputs/nodepool-ref.json" \
  --output "$RUN/private/rp-delete-nodepool" --confirm-delete --timeout 1h
"$HCP_PERF" rp delete --input "$RUN/inputs/rp.json" --resource "$RUN/inputs/cluster-ref.json" \
  --output "$RUN/private/rp-delete-cluster" --confirm-delete --timeout 1h
```

The RP delete primitive does not delete customer RGs or the personal environment.
Review ownership and the repository cleanup procedure separately after HCP
deletion completes. Do not remove lower-layer finalizers or Azure resources to
bypass the RP deletion chain. Keep the frontend forward available through RP
deletion and retain failure evidence.

## Remaining Blockers

- **Fresh provisioning inputs:** authorized personal settings/quota, customer
  infrastructure and identities/roles, and explicit API-correct payload fixtures
  are operator inputs. hcp-perf does not generate/provision these end to end.
- **Startup qualification:** explicit recorder namespaces are learned only after
  HC creation; generated HCP monitors exist only afterward. Two immutable capture
  sessions preserve early cAdvisor plus later component evidence, not a complete
  startup timeline. Dynamic future-namespace recording/config updates are absent.
- **Full scrape coverage:** real worker exporter availability and protocol, TLS
  trust/SANs, component monitor compatibility and credential lifetime, every
  replica/worker/container, and fresh samples require live verification. The
  explicit node-exporter generator job lacks TLS/auth configuration.
- **Constrained sizing:** the offline CSC bridge and initial fallback-complete
  candidate require custom HO/CPO deployment and live coverage/viability checks.
  `e2e_minimal` and an unqualified candidate do not satisfy this requirement.
- **APF/protected canaries:** no APF-install or baseline/cadence orchestrator CLI;
  manual policy/fixture/credentials and independent classification evidence are
  required. HTTP/APF receipts are not collected by turnover/canary primitives.
- **CPU qualification:** optional adversaries need approved bounds, readiness,
  live measured contention, complete scrapes and memory headroom; a quiet baseline
  cannot support a minimum-CPU claim.
- **Evidence assurance:** recorder PVC permissions/reattachment and exec/SPDY
  export need target-environment verification. Logs/watches have documented gaps;
  recorder timeline/cgroup analysis and an automatic scientific verdict are absent.
- **Orchestration:** remote full-case/campaign execution, credential rotation,
  automatic phase coordination/recovery and approved end-to-end cleanup remain
  outside these primitives. Human checkpoints are deliberate, not hidden automation.

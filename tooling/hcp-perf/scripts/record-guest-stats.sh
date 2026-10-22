#!/usr/bin/env bash
# Local, personal-experiment observer. Never invoke with shell tracing enabled.
set +x
set -euo pipefail
umask 077

if (( $# < 3 || $# > 4 )); then
    printf 'Usage: bash %s ABSOLUTE_KUBECONFIG NODE NEW_OUTPUT_DIR [DURATION_SECONDS=14400]\n' "$0" >&2
    exit 2
fi
kubeconfig=$1
node=$2
output=$3
duration=${4:-14400}
[[ $kubeconfig = /* && -f $kubeconfig && $output = /* && -d $(dirname "$output") ]]
[[ $node =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ && ${#node} -le 253 ]]
[[ $duration =~ ^[1-9][0-9]{0,4}$ ]] && (( duration <= 21600 ))
for executable in kubectl jq timeout sync date; do command -v "$executable" >/dev/null; done
export AZURE_CONFIG_DIR=${AZURE_CONFIG_DIR:-$HOME/.azure-redhat}
[[ $AZURE_CONFIG_DIR = "$HOME/.azure-redhat" ]]
mkdir -m 700 -- "$output"
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
deadline=$((SECONDS + duration))
child=
reason=error
samples=0
failures=0
bytes=0

finish() {
    local rc=$?
    trap - EXIT INT TERM
    if [[ -n $child ]]; then
        kill -TERM "$child" 2>/dev/null || true
        wait "$child" 2>/dev/null || true
    fi
    rm -f -- "$output/.response.json"
    jq -nc --arg endedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        --arg reason "$reason" --argjson exitCode "$rc" --argjson samples "$samples" \
        --argjson failures "$failures" \
        '{endedAt:$endedAt,reason:$reason,exitCode:$exitCode,samples:$samples,failures:$failures,qualified:false}' \
        > "$output/result.json"
    sync -f "$output/result.json"
    exit "$rc"
}
trap finish EXIT
trap 'reason=signal; exit 130' INT
trap 'reason=signal; exit 143' TERM
jq -nc --arg startedAt "$started" --arg node "$node" --arg kubeconfig "$kubeconfig" \
    --argjson pid "$$" --argjson durationSeconds "$duration" \
    '{version:1,startedAt:$startedAt,node:$node,kubeconfigReference:$kubeconfig,pid:$pid,
      durationSeconds:$durationSeconds,intervalSeconds:15,requestTimeoutSeconds:15,
      maxObservationBytes:1073741824,endpoint:("/api/v1/nodes/"+$node+"/proxy/stats/summary"),
      credentialsArchived:false,newRBAC:false,qualified:false}' > "$output/manifest.json"
sync -f "$output/manifest.json"
touch "$output/observations.jsonl"

while (( SECONDS < deadline )); do
    next=$((SECONDS + 15))
    remaining=$((deadline - SECONDS))
    bound=15
    (( remaining >= bound )) || bound=$remaining
    requested=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    # Discard free-form diagnostics: auth plugins/server errors may disclose credentials.
    timeout --signal=TERM --kill-after=2s "${bound}s" kubectl --kubeconfig "$kubeconfig" \
        --request-timeout=15s get --raw "/api/v1/nodes/$node/proxy/stats/summary" \
        > "$output/.response.json" 2>/dev/null &
    child=$!
    rc=0
    wait "$child" || rc=$?
    child=
    observed=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    line=
    if (( rc == 0 )); then
        # Allowlist identities and CPU/memory only; preserve absent values as null.
        line=$(jq -ce --arg requestedAt "$requested" --arg observedAt "$observed" --arg node "$node" '
            def resources: {startTime,cpu,memory};
            if .node.nodeName != $node or (.pods | type) != "array" then error("invalid summary") else
            {requestedAt:$requestedAt,observedAt:$observedAt,status:"success",node:$node,
             summary:{node:(.node | resources + {nodeName,systemContainers:[.systemContainers[]? | resources + {name}]}),
                      pods:[.pods[] | resources + {podRef:(.podRef | {name,namespace,uid}),
                            containers:[.containers[]? | resources + {name}]}]}} end
        ' "$output/.response.json" 2>/dev/null) || rc=65
    fi
    if (( rc != 0 )); then
        line=$(jq -nc --arg requestedAt "$requested" --arg observedAt "$observed" \
            --arg node "$node" --argjson exitCode "$rc" \
            '{requestedAt:$requestedAt,observedAt:$observedAt,node:$node,status:"error",exitCode:$exitCode}')
        failures=$((failures + 1))
    else
        samples=$((samples + 1))
    fi
    rm -f -- "$output/.response.json"
    bytes=$((bytes + ${#line} + 1))
    if (( bytes > 1073741824 )); then reason=byte-limit; exit 1; fi
    printf '%s\n' "$line" >> "$output/observations.jsonl"
    sync -f "$output/observations.jsonl"
    # Serial requests, no catch-up queue. The overall deadline also bounds sleeps.
    (( next <= deadline )) || next=$deadline
    if (( next > SECONDS )); then
        sleep "$((next - SECONDS))" &
        child=$!
        wait "$child"
        child=
    fi
done
reason=deadline
(( samples > 0 && failures == 0 ))

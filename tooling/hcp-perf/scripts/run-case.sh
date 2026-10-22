#!/usr/bin/env bash
# Experimental post-provisioning orchestration, not a campaign or sizing optimizer.
set +x
set -Eeuo pipefail
umask 077

usage() {
    printf '%s\n' \
        'Usage: run-case.sh --config-dir ABS_DIR --artifacts-dir NEW_ABS_DIR [--contended] [--validation]' \
        '       [--settle-seconds N] [--canary-cadence-seconds N]' \
        'Required environment: HCP_PERF MGMT_KUBECONFIG GUEST_ADMIN_KUBECONFIG HCP_NAMESPACE' \
        '                     CAPTURE_ENDPOINT NODE_METRICS_ENDPOINT CAPTURE_BUNDLE' \
        'Inputs: retention.json seed.json setup.json turnover.json record.json captures.json' \
        '        queries.json node-queries.json; adversaries.json only with --contended.' \
        'AZURE_CONFIG_DIR must be $HOME/.azure-redhat (defaulted only when unset).' \
        'Read scripts/README.md and review all settings before executing. No live dry-run.'
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

config_dir= artifacts_dir= contended=false mode=soak settle=300 cadence=
while (($#)); do
    case "$1" in
        --config-dir|--artifacts-dir|--settle-seconds|--canary-cadence-seconds)
            (($# >= 2)) || die "$1 requires a value"
            case "$1" in
                --config-dir) config_dir=$2 ;;
                --artifacts-dir) artifacts_dir=$2 ;;
                --settle-seconds) settle=$2 ;;
                --canary-cadence-seconds) cadence=$2 ;;
            esac
            shift 2 ;;
        --contended) contended=true; shift ;;
        --validation) mode=validation; shift ;;
        --help|-h) usage; exit 0 ;;
        *) usage >&2; die "unknown argument: $1" ;;
    esac
done
[[ $artifacts_dir == /* && ! -e $artifacts_dir && ! -L $artifacts_dir ]] || die '--artifacts-dir must be a new absolute path'
[[ -d $(dirname "$artifacts_dir") ]] || die 'artifacts parent must exist'
for tool in jq date sync; do
    command -v "$tool" >/dev/null || die "missing dependency: $tool"
done

mkdir "$artifacts_dir"
mkdir "$artifacts_dir/inputs" "$artifacts_dir/private" "$artifacts_dir/evidence" "$artifacts_dir/commands"
exec 3>&1 4>&2
exec >"$artifacts_dir/run.stdout" 2>"$artifacts_dir/run.stderr"
inputs=$artifacts_dir/inputs
evidence=$artifacts_dir/evidence
commands=$artifacts_dir/commands
active_pid= active_label= turnover_pid= recorder_attempted=false adversaries_attempted=false
observation_started=false record_id= record_namespace= adversary_id= adversary_namespace=
capture_start= pressure_start= recovery=600
steady_started=false slot=0 cycle=0 missed_canaries=0 completed_after_window=0 expected_canaries=0
phase=input failed_label= failed_class= stop_reason= workload_completed=false
capture_failed=0 shutdown_failed=0 ledger_failed=0

# Labels and typed results classify failures; stderr stays private and is never copied.
record_failure() {
    local label=$1 code=$2 class=harness_error
    case "$label" in
        workload-dispatch) class=expected_probe ;;
        validate-*) class=input_error ;;
        metrics-*|scrape-after|record-export|hcp-final|guest-final) class=capture_error; capture_failed=1 ;;
        record-stop|record-stopped-*|adversaries-stop|adversaries-stopped-*) class=shutdown_error; shutdown_failed=1 ;;
        guest-node-gate|hcp-gate|capture-gate-*) [[ $code != 1 ]] || class=preflight_not_viable ;;
        preflight-*|scrape-before) class=preflight_not_viable ;;
        turnover-gate) [[ $code != 1 ]] || class=expectation_violation ;;
        failure-recovery) stop_reason=harness_error ;;
        canary-steady-*)
            if [[ $code == 124 ]] || { [[ $code == 1 ]] && jq -e '
                .performance=="degraded" or .performance=="within_expectations" or
                (.ack.started != null and .ack.started != "0001-01-01T00:00:00Z")' "$commands/$label.json" >/dev/null 2>&1; }; then
                class=workload_aborted
            fi ;;
    esac
    if [[ $phase != finalization && $class != expected_probe ]]; then failed_label=$label; failed_class=$class; fi
    jq -cn --arg label "$label" --argjson code "$code" --arg class "$class" --arg phase "$phase" '
        {label:$label,exitCode:$code,classification:$class,phase:$phase,observedAt:(now|todateiso8601),
         stdoutFile:("commands/"+$label+".json"),stderrFile:("commands/"+$label+".stderr")}' >>"$evidence/failures.jsonl" &&
        sync -f "$evidence/failures.jsonl"
}

# Track the timeout process itself: GNU timeout forwards TERM to its process group.
# Never background shell functions that could outlive an untracked CLI child.
wait_local() {
    local pid=$1 rc signals
    while true; do
        rc=0 signals=${finalization_signals:-0}
        wait "$pid" || rc=$?
        # A trapped signal interrupts Bash wait, not the child. Reap its real
        # status even if it exited while the signal handler was recording evidence.
        ((signals != ${finalization_signals:-0})) || break
    done
    return "$rc"
}

run() {
    local label=$1 bound=$2 rc=0
    shift 2
    date -u +%Y-%m-%dT%H:%M:%SZ >"$commands/$label.started"
    active_label=$label
    timeout --kill-after=150s "$bound" "$@" >"$commands/$label.json" 2>"$commands/$label.stderr" &
    active_pid=$!
    wait_local "$active_pid" || rc=$?
    active_pid=
    printf '%s\n' "$rc" >"$commands/$label.exit"
    if ((rc)); then
        record_failure "$label" "$rc" || ledger_failed=1
        printf '%s failed (%s); see commands/%s.{json,stderr}\n' "$label" "$rc" "$label" >&4
    fi
    return "$rc"
}

stop_local() {
    local pid=$1 label=$2 deadline
    # Stopping a sibling after a failure must not replace the triggering label.
    local failed_label=$failed_label failed_class=$failed_class
    [[ -n $pid ]] || return 0
    kill -TERM "$pid" 2>/dev/null || true
    deadline=$((SECONDS + 150))
    while kill -0 "$pid" 2>/dev/null && ((SECONDS < deadline)); do sleep 1; done
    if kill -0 "$pid" 2>/dev/null; then
        printf '%s required SIGKILL; shutdown is incomplete\n' "$label" >>"$evidence/shutdown-errors.txt"
        kill -KILL -- "-$pid" 2>/dev/null || true
    fi
    local rc=0
    wait_local "$pid" || rc=$?
    printf '%s\n' "$rc" >"$commands/$label.exit"
    ((rc == 0)) || record_failure "$label" "$rc" || ledger_failed=1
}

# Poll observations only. Stop never retries a write or treats missing jobs as quiet.
poll_session() {
    local kind=$1 namespace=$2 id=$3 predicate=$4 label=$5 limit=$6
    local deadline=$((SECONDS + limit)) i=0
    while ((SECONDS < deadline)); do
        i=$((i + 1))
        run "$label-$i" 40s "$HCP_PERF" "$kind" status --namespace "$namespace" --id "$id" \
            --kubeconfig "$MGMT_KUBECONFIG" --timeout 30s || return 1
        if jq -e "$predicate" "$commands/$label-$i.json" >/dev/null; then return 0; fi
        sleep 5
    done
    printf '%s timed out; inspect commands/%s-*.json\n' "$label" "$label" >&2
    record_failure "$label-timeout" 124 || ledger_failed=1
    return 1
}

# Capture health is stricter than availability of naturally changing metric families.
# Analyze preserves raw completeness flags and validates hashes, grids and finite values.
require_metrics() {
    local name=$1 directory=$2 health=${3:-true}
    run "$name-analysis" 20m "$HCP_PERF" analyze --input "$directory" || return 1
    run "$name-gate" 10s jq -e --argjson health "$health" '
        def acceptable:
            length>0 and all(.[];
                (.status=="success" or .status=="incomplete") and (.error // "")=="" and
                (if .name=="up" or .name=="scrape-time" then .status=="success" else true end));
        (.manifest.queries|acceptable) and (.queries|acceptable) and
        (.queries|all(.[]; (.series|length)>0 and all(.series[]; .validSamples>0 and .invalidSamples==0))) and
        (if $health then (.queries|any(.[]; .name=="up")) and
            (.queries|any(.[]; .name=="scrape-time")) else true end)' \
        "$commands/$name-analysis.json" || return 1
    if ! $health; then return 0; fi
    # Evaluation timestamps alone hide PromQL lookback repeats. Compare each
    # timestamp(up) value with its evaluation time, never the later export time.
    run "$name-health" 30s jq -en --argjson maxAge "$SCRAPE_MAX_AGE_SECONDS" \
        --slurpfile up "$directory/up.json" --slurpfile time "$directory/scrape-time.json" '
        ($up[0].data.result|length)>0 and
        all($up[0].data.result[]; (.values|length)>0 and all(.values[]; (.[1]|tonumber)==1)) and
        ([$up[0].data.result[].metric|del(.__name__)]|sort)==
            ([$time[0].data.result[].metric|del(.__name__)]|sort) and
        all($time[0].data.result[]; (.values|length)>0 and
            all(.values[]; (.[0]-(.[1]|tonumber)) as $age | $age>=0 and $age<$maxAge))' || return 1
}

# Each unattempted slot is recorded exactly once, never retried or burst-admitted.
record_missed_slots() {
    local through=$1 reason=$2
    while ((slot < expected_canaries && slot * cadence <= through)); do
        jq -cn --argjson slot "$slot" --argjson offset "$((slot * cadence))" \
            --argjson epoch "$((steady_epoch + slot * cadence))" --arg reason "$reason" \
            --argjson afterWindow "$((SECONDS >= steady_seconds + duration))" \
            '{type:"missed-slot",slotIndex:$slot,canaryNumber:($slot+1),scheduledOffsetSeconds:$offset,
              scheduledAt:($epoch|todateiso8601),observedAt:(now|todateiso8601),reason:$reason,
              afterWindow:($afterWindow==1)}' >>"$evidence/canary-cadence.jsonl" || return 1
        slot=$((slot + 1))
        missed_canaries=$((missed_canaries + 1))
    done
    sync -f "$evidence/canary-cadence.jsonl"
}

record_finalization_signal() {
    finalization_interrupted=true
    finalization_signals=$((finalization_signals + 1))
    attempted_signal=$1
    printf 'WARNING: %s requested during finalization; continuing bounded cleanup/export (external supervisor hard cap still applies).\n' "$attempted_signal" >&2
    printf '{"type":"finalization-stop-requested","signal":"%s","observedAt":"%s"}\n' \
        "$attempted_signal" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$evidence/shutdown-events.jsonl"
}

finish() {
    local rc=$? cleanup_failed=0 end pressure_end
    local workload_rc=$rc
    local execution=completed reason=${stop_reason:-${failed_class:-harness_error}} workload_failed_label=$failed_label
    if ((workload_rc != 0)); then
        execution=harness_error
        case "$reason" in
            signal_INT|signal_TERM|signal_HUP|workload_aborted|preflight_not_viable) execution=aborted ;;
            expectation_violation) $workload_completed && execution=completed ;;
        esac
        [[ $phase != input || -n $stop_reason || -n $failed_class ]] || reason=input_error
    else reason=completed; fi
    phase=finalization
    local finalization_interrupted=false finalization_signals=0 attempted_signal=
    trap 'record_finalization_signal INT' INT
    trap 'record_finalization_signal TERM' TERM
    trap 'record_finalization_signal HUP' HUP
    trap - EXIT
    # Continue every evidence/stop attempt, even after a failed turnover or stop.
    # Per-command limits are not a total budget: the outer supervisor must enforce
    # the session hard cap (e.g. RuntimeMaxSec=45min with a bounded stop timeout).
    set +e
    # Watchers must stop workload safety assertions once this atomic marker exists;
    # already observed safety failures belong in their own evidence, not workload_rc.
    jq -n --argjson workloadExit "$workload_rc" \
        '{phase:"finalization",startedAt:(now|todateiso8601),workloadExitCode:$workloadExit}' \
        >"$evidence/finalization-started.json.tmp" &&
        sync -f "$evidence/finalization-started.json.tmp" &&
        mv "$evidence/finalization-started.json.tmp" "$evidence/finalization-started.json" && sync -f "$evidence" || cleanup_failed=1
    stop_local "$active_pid" "${active_label:-interrupted-command}"
    stop_local "$turnover_pid" turnover
    if $steady_started; then
        record_missed_slots "$duration" run-ended || cleanup_failed=1
    fi
    pressure_end=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    if $adversaries_attempted; then
        run adversaries-stop 70s "$HCP_PERF" adversaries stop --namespace "$adversary_namespace" \
            --id "$adversary_id" --kubeconfig "$MGMT_KUBECONFIG" --timeout 1m || cleanup_failed=1
        poll_session adversaries "$adversary_namespace" "$adversary_id" \
            '.phase=="stopped" and .nonterminalPods==0 and .submissionIncomplete==false' \
            adversaries-stopped 180 || cleanup_failed=1
    fi
    end=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    printf '%s\n' "$end" >"$evidence/window-end.txt"
    if $recorder_attempted; then
        run record-stop 70s "$HCP_PERF" record stop --namespace "$record_namespace" --id "$record_id" \
            --kubeconfig "$MGMT_KUBECONFIG" --timeout 1m || cleanup_failed=1
        poll_session record "$record_namespace" "$record_id" \
            '(.phase=="stopped" or .phase=="completed" or .phase=="failed") and .nonterminalPods==0' \
            record-stopped 180 || { cleanup_failed=1; shutdown_failed=1; }
        # Export is deliberately attempted even for a failed/partially stopped recorder.
        run record-export 12m "$HCP_PERF" record export --namespace "$record_namespace" --id "$record_id" \
            --kubeconfig "$MGMT_KUBECONFIG" --timeout 10m --output "$evidence/record" || cleanup_failed=1
    fi
    if $observation_started; then
        run hcp-final 40s kubectl --kubeconfig "$MGMT_KUBECONFIG" --request-timeout=30s \
            -n "$HCP_NAMESPACE" get pods -o json || cleanup_failed=1
        run guest-final 40s kubectl --kubeconfig "$GUEST_ADMIN_KUBECONFIG" --request-timeout=30s \
            get pods -A -o json || cleanup_failed=1
        run scrape-after 70s "$HCP_PERF" scrape check --input "$inputs/bundle.json" \
            --endpoint "$CAPTURE_ENDPOINT" --timeout 1m || cleanup_failed=1
        run metrics-capture 20m "$HCP_PERF" metrics export --endpoint "$CAPTURE_ENDPOINT" \
            --queries "$inputs/queries.json" --start "$capture_start" --end "$end" --step 5s \
            --output "$evidence/metrics-capture" || cleanup_failed=1
        require_metrics metrics-capture "$evidence/metrics-capture" || cleanup_failed=1
        run metrics-nodes 20m "$HCP_PERF" metrics export --endpoint "$NODE_METRICS_ENDPOINT" \
            --queries "$inputs/node-preflight-queries.json" --start "$capture_start" --end "$end" --step 5s \
            --output "$evidence/metrics-nodes" || cleanup_failed=1
        require_metrics metrics-nodes "$evidence/metrics-nodes" || cleanup_failed=1
        # Optional families are exported without treating absent series as zeros
        # or weakening the required preflight/export gates above.
        for kind in capture nodes; do
            local optional=optional-queries endpoint=$CAPTURE_ENDPOINT
            if [[ $kind == nodes ]]; then optional=optional-node-queries; endpoint=$NODE_METRICS_ENDPOINT; fi
            if [[ -f $inputs/$optional.json ]]; then
                run "metrics-optional-$kind" 20m "$HCP_PERF" metrics export --endpoint "$endpoint" \
                    --queries "$inputs/$optional.json" --start "$capture_start" --end "$end" --step 5s \
                    --output "$evidence/metrics-optional-$kind" || cleanup_failed=1
            fi
        done
        if [[ -n $pressure_start ]]; then
            run metrics-pressure 5m "$HCP_PERF" metrics export --endpoint "$NODE_METRICS_ENDPOINT" \
                --queries "$inputs/pressure-queries.json" --start "$pressure_start" --end "$pressure_end" --step 5s \
                --output "$evidence/metrics-pressure" || cleanup_failed=1
            require_metrics metrics-pressure "$evidence/metrics-pressure" false || cleanup_failed=1
        fi
    fi
    [[ ! -s $evidence/shutdown-errors.txt ]] || { cleanup_failed=1; shutdown_failed=1; }
    sha256sum "$inputs/"*.json >"$evidence/final-inputs.sha256" || cleanup_failed=1
    ((ledger_failed == 0)) || cleanup_failed=1
    ((rc != 0 || cleanup_failed == 0)) || rc=1
    # Count only real steady result files. Missing/malformed results are unknown,
    # not zero violations; baseline cycles are deliberately excluded.
    local performance=null canary_files=() file exits_complete=true
    for file in "$commands/"canary-steady-*.json; do
        [[ -f $file ]] || continue
        canary_files+=("$file")
        [[ -f ${file%.json}.exit && $(<"${file%.json}.exit") == 0 ]] || exits_complete=false
    done
    if $exits_complete && ((${#canary_files[@]} > 0 && ${#canary_files[@]} == cycle)); then
        performance=$(jq -es 'if length>0 and all(.[]; (.degraded|type)=="boolean" and
            (.performance=="degraded" or .performance=="within_expectations")) then
            {observed:length,degraded:([.[]|select(.degraded)]|length)} else null end' "${canary_files[@]}" 2>/dev/null) || performance=null
    fi
    jq -n --arg mode "$mode" --argjson exit "$rc" --argjson workloadExit "$workload_rc" --argjson cleanupFailed "$cleanup_failed" \
        --arg execution "$execution" --arg reason "$reason" --arg failedLabel "$workload_failed_label" \
        --argjson captureFailed "$capture_failed" --argjson shutdownFailed "$shutdown_failed" \
        --argjson observed "$observation_started" --argjson steady "$steady_started" --argjson performance "$performance" \
        --argjson expected "$expected_canaries" --argjson attempted "$cycle" --argjson missed "$missed_canaries" \
        --argjson afterWindow "$completed_after_window" --argjson interrupted "$finalization_interrupted" \
        '{mode:$mode,exitCode:$exit,workloadExitCode:$workloadExit,shutdownOrExportFailed:($cleanupFailed!=0),qualified:false,capturesLeftRunning:true,
          executionStatus:$execution,stopReason:$reason,failedLabel:(if $failedLabel=="" then null else $failedLabel end),
          captureStatus:(if $captureFailed!=0 then "incomplete" elif $observed then "complete" else "unknown" end),
          shutdownStatus:(if $shutdownFailed!=0 then "incomplete" else "complete" end),
          experimentStatus:(if $reason=="expectation_violation" or $reason=="workload_aborted" then "violations"
            elif $performance.degraded>0 then "degraded"
            elif $execution=="completed" and $steady and $missed==0 and $afterWindow==0 and
              $performance.observed==$expected and $expected>0 then "within_expectations" else "unknown" end),
          expectationViolations:{degradedCanaries:($performance.degraded // null),
            missedCanarySlots:(if $steady then $missed else null end),
            completedAfterWindow:(if $steady then $afterWindow else null end),safety:null},
          finalization_interrupted:$interrupted,
          expectedSteadyCanaries:$expected,attemptedSteadyCanaries:$attempted,missedSteadyCanaries:$missed,
          completedAfterWindow:$afterWindow,
          observationCoverage:(if $missed>0 or $afterWindow>0 or $exit!=0 then "incomplete" else "not_assessed" end)}' \
        >"$evidence/outcome.json.tmp" && sync -f "$evidence/outcome.json.tmp" &&
        mv "$evidence/outcome.json.tmp" "$evidence/outcome.json" && sync -f "$evidence" || {
            printf 'ERROR: durable outcome write failed; finalization evidence is incomplete.\n' >&4
            ((rc != 0)) || rc=1
        }
    printf 'Run exit %s. Private artifacts: %s. Captures and persistent resources retained.\n' "$rc" "$artifacts_dir" >&4
    exit "$rc"
}
trap finish EXIT
trap 'stop_reason=signal_INT; exit 130' INT
trap 'stop_reason=signal_TERM; exit 143' TERM
trap 'stop_reason=signal_HUP; exit 129' HUP

# Validate only after finalization is armed and a safe artifact destination exists.

[[ $settle =~ ^[1-9][0-9]{0,4}$ ]] && ((settle <= 86400)) || die '--settle-seconds must be in 1..86400'
[[ -z $cadence || $cadence =~ ^[1-9][0-9]{0,3}$ ]] || die '--canary-cadence-seconds must be a positive integer at most 7200'
[[ $config_dir == /* && -d $config_dir ]] || die '--config-dir must be an existing absolute directory'
for variable in HCP_PERF MGMT_KUBECONFIG GUEST_ADMIN_KUBECONFIG CAPTURE_BUNDLE; do
    [[ ${!variable:-} == /* && -f ${!variable} && -r ${!variable} ]] || die "$variable must name a readable absolute file"
done
[[ -x $HCP_PERF ]] || die 'HCP_PERF must be executable'
: "${HCP_NAMESPACE:?verified existing control-plane namespace required}"
: "${CAPTURE_ENDPOINT:?reachable dedicated HCP Prometheus URL required}"
: "${NODE_METRICS_ENDPOINT:?reachable dedicated node Prometheus URL required}"
SCRAPE_MAX_AGE_SECONDS=${SCRAPE_MAX_AGE_SECONDS:-45}
[[ $SCRAPE_MAX_AGE_SECONDS =~ ^[1-9][0-9]*$ ]] || die 'SCRAPE_MAX_AGE_SECONDS must be a positive integer'
[[ ${AZURE_CONFIG_DIR-${HOME}/.azure-redhat} == "$HOME/.azure-redhat" ]] || die 'refusing explicit AZURE_CONFIG_DIR other than $HOME/.azure-redhat'
export AZURE_CONFIG_DIR="$HOME/.azure-redhat"
for tool in kubectl timeout sha256sum sleep; do command -v "$tool" >/dev/null || die "missing dependency: $tool"; done

files=(retention seed setup turnover record captures queries node-queries)
$contended && files+=(adversaries)
for file in optional-queries optional-node-queries; do
    [[ ! -f $config_dir/$file.json ]] || files+=("$file")
done
for file in "${files[@]}"; do
    cp "$config_dir/$file.json" "$inputs/$file.json"
    run "validate-json-$file" 10s jq -e . "$inputs/$file.json"
done
chmod 0400 "$inputs/retention.json"
cp "$CAPTURE_BUNDLE" "$inputs/bundle.json"
jq -e . "$inputs/bundle.json" >/dev/null
sha256sum "$HCP_PERF" >"$evidence/hcp-perf.sha256"
sha256sum "$inputs/"*.json >"$evidence/inputs.sha256"
run cli-help 10s "$HCP_PERF" --help
# No args means the primitive rejects missing input before constructing any client.
if run workload-dispatch 10s "$HCP_PERF" workload setup; then die 'unexpected setup without input'; fi
jq -e '.error | contains("require --input, --kubeconfig")' "$commands/workload-dispatch.stderr" >/dev/null \
    || die 'HCP_PERF must wire workload setup/credentials into main.go before running this script'

# Refresh only the reviewed RG IDs, before observation or any test resources.
run environment-refresh 6m "$HCP_PERF" environment refresh --input "$inputs/retention.json" --timeout 5m
run environment-refresh-gate 10s jq -es --slurpfile spec "$inputs/retention.json" '
    length==1 and (.[0] |
        .version=="v1" and .status=="verified" and .dryRun==false and .failure==null and
        (.resourceGroups|type)=="array" and (.resourceGroups|length)>0 and
        all(.resourceGroups[]; .status=="verified" and .attempted==true and .updated==true and
            .verified==true and .possiblyUpdated==false and .failure==null) and
        ([.resourceGroups[].id|ascii_downcase]|sort)==
            ([$spec[0].resourceGroups[]|ascii_downcase]|unique|sort))' \
    "$commands/environment-refresh.json" || die 'retention refresh did not verify every explicit resource group; inspect commands/environment-refresh.*'

run validate-inputs 10s jq -en --slurpfile seed "$inputs/seed.json" --slurpfile setup "$inputs/setup.json" \
    --slurpfile turnover "$inputs/turnover.json" --slurpfile record "$inputs/record.json" --arg hcp "$HCP_NAMESPACE" \
    --arg mode "$mode" --argjson settle "$settle" '
    def integer: type=="number" and floor==.;
    $seed[0] as $s | $setup[0] as $w | $turnover[0] as $t | $record[0] as $r |
    ($s.guestNodes|type)=="number" and ($s.guestNodes|floor)==$s.guestNodes and
    $s.guestNodes>=1 and $s.guestNodes<=1000 and
    $s.guestNodes==$w.guestNodes and $s.guestNodes==$t.guestNodes and
    ($s.applications|type)=="number" and ($s.applications|floor)==$s.applications and
    $s.applications>=0 and $s.applications<=40000 and $s.applications*5<=$s.guestNodes*200 and
    $s.applications==$w.applications and $s.applications==$t.applications and
    $s.id==$w.id and $s.namespacePrefix==$w.namespacePrefix and $s.namespacePrefix==$t.namespacePrefix and
    $s.image==$w.image and ($s.timeoutSeconds|integer) and ($s.timeoutSeconds>=1 and $s.timeoutSeconds<=3600) and
    ($t.durationSeconds|integer) and ($t.recoverySeconds|integer) and
    (if $t|has("turnoverPeriodSeconds") then
        ($t.turnoverPeriodSeconds|integer) and $t.turnoverPeriodSeconds>=0 and $t.turnoverPeriodSeconds<=86400
     else true end) and
    (if $mode=="validation" then $t.durationSeconds>=30 and $t.durationSeconds<=1800 and
        $t.recoverySeconds>=1 and $t.recoverySeconds<=120
     else $t.durationSeconds==7200 and $t.recoverySeconds>=1 and $t.recoverySeconds<=600 end) and
    ($t.maxOutstanding>=1 and $t.maxOutstanding<=20 and ($t.maxOutstanding|floor)==$t.maxOutstanding) and
    ($r.namespaces|index($hcp)!=null) and $r.enableWatch==true and ($r.durationSeconds|integer) and
    $r.durationSeconds<=86400 and
    $r.durationSeconds>=(if $mode=="validation" then $t.durationSeconds+$t.recoverySeconds+$settle+$s.timeoutSeconds+1800 else 21600 end)' \
    || die 'seed/setup/turnover require matching explicit integer guestNodes and applications, at most 200 app Pods/node, and reviewed timing/identity settings'
nodes=$(jq -er .guestNodes "$inputs/seed.json")
applications=$(jq -er .applications "$inputs/seed.json")
namespace_count=$(((applications + 19) / 20))
max_outstanding=$(jq -er .maxOutstanding "$inputs/turnover.json")
seed_id=$(jq -er .id "$inputs/seed.json")
prefix=$(jq -er .namespacePrefix "$inputs/seed.json")
seed_timeout=$(jq -er .timeoutSeconds "$inputs/seed.json")
burner=$(jq -er .binary.path "$inputs/seed.json")
[[ $burner == /* && -x $burner ]] || die 'seed.binary.path must be an absolute executable path'
run burner-hash 10s sha256sum "$burner"
read -r burner_hash _ <"$commands/burner-hash.json"
[[ $burner_hash == "$(jq -er .binary.expectedSHA256 "$inputs/seed.json")" ]] || die 'seed binary SHA256 mismatch'
record_id=$(jq -er .id "$inputs/record.json")
record_namespace=$(jq -er .namespace "$inputs/record.json")
recovery=$(jq -er .recoverySeconds "$inputs/turnover.json")
duration=$(jq -er .durationSeconds "$inputs/turnover.json")
period=$(jq -er 'if (has("turnoverPeriodSeconds")|not) or .turnoverPeriodSeconds==0 then 3600 else .turnoverPeriodSeconds end' "$inputs/turnover.json")
if [[ -z $cadence ]]; then
    cadence=300
    if [[ $mode == validation ]]; then
        cadence=$((duration / 5))
        ((cadence <= 30)) || cadence=30
        ((cadence >= 5)) || cadence=5
    fi
fi
((cadence >= 5 && cadence <= duration)) || die 'canary cadence must be in 5..durationSeconds'
expected_canaries=$(((duration + cadence - 1) / cadence))
scheduled=$(((applications * duration + period - 1) / period))
((scheduled <= 80000)) || die 'schedule exceeds 80000 rollouts'
jq -n --arg mode "$mode" --argjson steady "$duration" --argjson recovery "$recovery" \
    --argjson settle "$settle" --argjson cadence "$cadence" --argjson canaries "$expected_canaries" --argjson scheduled "$scheduled" \
    --argjson nodes "$nodes" --argjson apps "$applications" --argjson contended "$contended" \
    --argjson period "$period" --argjson outstanding "$max_outstanding" \
    '{title:(($mode|ascii_upcase)+": \($nodes) guest nodes, \($apps*5) application Pods"),
      objective:"Observe and record progress and degradation, boundedhardstop",
      question:(if $mode=="validation" then "Do workload execution, watch-based canaries, capture, and export work end to end?" else "What progress and degradation occur under the recorded resource configuration during the measurement window?" end),
      guestNodes:$nodes,applications:$apps,cpuTreatment:(if $contended then "contended" else "quiet" end),
      measurementMode:"watch",mode:$mode,steadySeconds:$steady,recoverySeconds:$recovery,settleSeconds:$settle,
      canaryCadenceSeconds:$cadence,baselineCycles:5,expectedSteadyCanaries:$canaries,scheduledRollouts:$scheduled,
      turnoverPeriodSeconds:$period,nominalRolloutsPerHour:($apps*3600/$period),maxOutstanding:$outstanding,
      workloadRecipe:(if $period==3600 then "One full five-Pod replacement per application per hour"
        else "One full five-Pod replacement per application every \($period) seconds" end),
      canaryPolicy:"observe-degradation",expectationsFile:"commands/baseline-expectations.json",
      expectationFlags:{ack:"--ack-expected",ready:"--ready-expected",cleanup:"--cleanup-expected"},
      hardDeadlineSeconds:{ack:30,ready:120,cleanup:120},preflightSeconds:30,canaryTimeoutSeconds:300,canaryCallBoundSeconds:360}' >"$inputs/session.json"
chmod 0400 "$inputs/session.json"
sha256sum "$inputs/"*.json >"$evidence/inputs.sha256"
canary_namespace=hcp-perf-canary-$(jq -er .id "$inputs/setup.json")
run validate-bundle 10s jq -e --arg hcp "$HCP_NAMESPACE" \
    '(.gaps|length)==0 and (.expectedJobs|length)>0 and (.spec.targetNamespaces|index($hcp)!=null)' "$inputs/bundle.json"
run validate-captures 10s jq -e '
    type=="array" and length>0 and all(.[]; (.id|type)=="string" and (.id|length)>0) and
    ([.[].id]|length)==([.[].id]|unique|length)' "$inputs/captures.json"
for file in queries node-queries optional-queries optional-node-queries; do
    [[ -f $inputs/$file.json ]] || continue
    run "validate-$file" 10s jq -e '
        type=="array" and length>0 and all(.[]; ((.name // "")|test("^[a-z0-9][a-z0-9-]*$")) and
        (.expr|type)=="string" and (.expr|length)>0) and ([.[].name]|length)==([.[].name]|unique|length)' "$inputs/$file.json"
done
for file in queries node-queries; do
    run "validate-$file-health" 10s jq -e '
        (map(select(.name=="up"))[0].expr // "") as $up |
        ($up|test("^up(\\{[^{}]*\\})?$")) and
        any(.[]; .name=="scrape-time" and .expr==("timestamp("+$up+")"))' "$inputs/$file.json" \
        || die "$file.json requires named up and scrape-time queries: up (or up{...}) and timestamp of that identical selector"
done
jq -n --argjson maxAge "$SCRAPE_MAX_AGE_SECONDS" \
    '{scrapeMaxAgeSeconds:$maxAge,healthQueries:["up","scrape-time"],qualified:false}' >"$evidence/capture-health-policy.json"
cp "$inputs/node-queries.json" "$inputs/node-preflight-queries.json"
if $contended; then
    adversary_id=$(jq -er .id "$inputs/adversaries.json")
    adversary_namespace=$(jq -er .namespace "$inputs/adversaries.json")
    adversary_minimum=10000
    [[ $mode != validation ]] || adversary_minimum=$((duration + recovery + settle + 120))
    run validate-adversaries 10s jq -e --arg hcp "$HCP_NAMESPACE" --argjson minimum "$adversary_minimum" \
        '.targetNamespace==$hcp and (.durationSeconds|type)=="number" and (.durationSeconds|floor)==.durationSeconds and
        .durationSeconds>=$minimum and .durationSeconds<=86400' "$inputs/adversaries.json"
    # Presence of CPU samples is required; this does NOT establish contention.
    run validate-cpu-query 10s jq -e 'any(.[]; .name=="adversary-cpu-rate")' "$inputs/node-queries.json"
    jq '[.[]|select(.name!="adversary-cpu-rate")]' "$inputs/node-queries.json" >"$inputs/node-preflight-queries.json"
    run validate-node-preflight 10s jq -e 'length>0' "$inputs/node-preflight-queries.json"
fi

mgmt=(kubectl --kubeconfig "$MGMT_KUBECONFIG" --request-timeout=30s)
phase=preflight
guest=(kubectl --kubeconfig "$GUEST_ADMIN_KUBECONFIG" --request-timeout=30s)
run guest-nodes 40s "${guest[@]}" get nodes -o json
run guest-node-gate 10s jq -e --argjson nodes "$nodes" '.items|length==$nodes and all(.[];
    .metadata.deletionTimestamp==null and .spec.unschedulable!=true and
    any(.status.conditions[]; .type=="Ready" and .status=="True"))' "$commands/guest-nodes.json"
run guest-baseline 40s "${guest[@]}" get pods -A -o json
# Aggregate admission headroom only, not a per-node placement or resource proof.
run guest-capacity 10s jq -en --argjson apps "$applications" --argjson outstanding "$max_outstanding" \
    --slurpfile nodes "$commands/guest-nodes.json" --slurpfile pods "$commands/guest-baseline.json" '
    [$nodes[0].items[].metadata.name] as $names |
    [$nodes[0].items[].status.allocatable.pods | tonumber] as $slots |
    [$pods[0].items[] | select(.status.phase!="Succeeded" and .status.phase!="Failed") |
        select(.spec.nodeName as $node | $names | index($node)!=null)] as $assigned |
    {guestNodes:($names|length),allocatablePodSlots:($slots|add),assignedBaselinePods:($assigned|length),
     applicationPods:($apps*5),canaryPodSlots:1,rolloutSurgePodSlots:$outstanding} |
    .requiredPodSlots=(.assignedBaselinePods+.applicationPods+.canaryPodSlots+.rolloutSurgePodSlots) |
    if .requiredPodSlots<=.allocatablePodSlots then . else error("insufficient aggregate guest Pod slots for baseline, applications, canary and maxOutstanding surge") end'
run hcp-inventory 40s "${mgmt[@]}" -n "$HCP_NAMESPACE" get pods -o json
run hcp-gate 10s jq -e '.items|length>0 and all(.[];
    (.metadata.uid|type)=="string" and (.metadata.uid|length)>0 and
    .metadata.deletionTimestamp==null and
    (if .status.phase=="Succeeded" then true else .status.phase=="Running" and
        any(.status.conditions[]?; .type=="Ready" and .status=="True") end))' "$commands/hcp-inventory.json"
run record-absent 40s "${mgmt[@]}" -n "$record_namespace" get configmap "record-$record_id" --ignore-not-found -o json
[[ ! -s $commands/record-absent.json ]] || die 'recorder ID already exists; refusing to adopt or stop it'
if $contended; then
    run adversaries-absent 40s "${mgmt[@]}" -n "$adversary_namespace" get configmap "adversaries-$adversary_id" --ignore-not-found -o json
    [[ ! -s $commands/adversaries-absent.json ]] || die 'adversary ID already exists; refusing to adopt or stop it'
fi
mapfile -t capture_ids < <(jq -r '.[].id' "$inputs/captures.json")
for i in "${!capture_ids[@]}"; do
    run "capture-status-$i" 70s "$HCP_PERF" capture status --id "${capture_ids[$i]}" --kubeconfig "$MGMT_KUBECONFIG" --timeout 1m
    run "capture-gate-$i" 10s jq -e '.phase=="ready" and (.missing|length)==0' "$commands/capture-status-$i.json"
done
capture_start=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '%s\n' "$capture_start" >"$evidence/window-start.txt"
observation_started=true
run scrape-before 70s "$HCP_PERF" scrape check --input "$inputs/bundle.json" --endpoint "$CAPTURE_ENDPOINT" --timeout 1m

# Require capture health and finite samples, not lifetime-wide coverage of every
# API/GVR/container label set. Birth/death gaps remain incomplete in the evidence.
check_samples() {
    local name=$1 endpoint=$2 queries=$3 start end
    start=$(date -u -d '30 seconds ago' +%Y-%m-%dT%H:%M:%SZ)
    end=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    run "$name" 20m "$HCP_PERF" metrics export --endpoint "$endpoint" --queries "$queries" \
        --start "$start" --end "$end" --step 5s --output "$evidence/$name" || return 1
    require_metrics "$name" "$evidence/$name" "${4:-true}" || return 1
}
check_samples preflight-capture "$CAPTURE_ENDPOINT" "$inputs/queries.json"
check_samples preflight-nodes "$NODE_METRICS_ENDPOINT" "$inputs/node-preflight-queries.json"
jq -n --slurpfile session "$inputs/session.json" --argjson contended "$contended" --arg hcp "$HCP_NAMESPACE" \
    --argjson nodes "$nodes" --argjson apps "$applications" --argjson namespaces "$namespace_count" \
    '$session[0] + {experimental:true,postProvisioning:true,guestNodes:$nodes,applications:$apps,applicationPods:($apps*5),
      namespaceCount:$namespaces,canaryEnabled:true,hcpNamespace:$hcp,contendedRequested:$contended,
      qualified:false,limitations:["No startup coverage", "No automatic APF classification verification",
      "No pressure threshold or all-worker/replica freshness qualification", "No guest watch/log recorder",
      "No sizing or memory-safety verdict; review raw evidence"]}' >"$evidence/run.json"

recorder_attempted=true
phase=workload
run record-start 6m "$HCP_PERF" record start --input "$inputs/record.json" --kubeconfig "$MGMT_KUBECONFIG" --timeout 5m
# Require explicit readiness evidence too, so an older status CLI fails closed.
record_ready='.phase=="running" and .nonterminalPods>0 and
    any(.pods[]?; .phase=="Running" and .ready==true and .terminating==false and
        (.startTime|type)=="string" and .startTime!="" and .startTime!="0001-01-01T00:00:00Z")'
poll_session record "$record_namespace" "$record_id" "$record_ready" record-running 300
run seed 65m "$HCP_PERF" seed run --input "$inputs/seed.json" --kubeconfig "$GUEST_ADMIN_KUBECONFIG" \
    --timeout "${seed_timeout}s" --output "$artifacts_dir/private/seed"
for ((n=0; n<namespace_count; n++)); do
    run "seed-warmup-$n" 16m "${guest[@]}" -n "$prefix-$n" wait --for=condition=Available deployment --all --timeout=15m
done
run seed-deployments 40s "${guest[@]}" get deployments -A -l "hcp-perf.azure.com/run-id=$seed_id" -o json
run seed-deployment-gate 10s jq -e --argjson apps "$applications" '.items|type=="array" and length==$apps and all(.[];
    .spec.replicas==5 and .status.observedGeneration==.metadata.generation and
    .status.updatedReplicas==5 and .status.readyReplicas==5 and .status.availableReplicas==5)' "$commands/seed-deployments.json"
run seed-pods 40s "${guest[@]}" get pods -A -l "hcp-perf.azure.com/run-id=$seed_id" -o json
run seed-pod-gate 10s jq -e --argjson pods "$((applications * 5))" '.items|type=="array" and length==$pods and all(.[]; .metadata.deletionTimestamp==null and
    .status.phase=="Running" and any(.status.conditions[]?; .type=="Ready" and .status=="True"))' "$commands/seed-pods.json"
run guest-seeded-pods 40s "${guest[@]}" get pods -A -o json
# Count terminating Pods too. This admission gate cannot guarantee surge placement.
run preflight-seeded-slots 10s jq -en --argjson outstanding "$max_outstanding" \
    --slurpfile nodes "$commands/guest-nodes.json" --slurpfile pods "$commands/guest-seeded-pods.json" '
    [$nodes[0].items[] | . as $node |
        [$pods[0].items[] | select(.spec.nodeName==$node.metadata.name and
            .status.phase!="Succeeded" and .status.phase!="Failed")] as $assigned |
        {name:$node.metadata.name,uid:$node.metadata.uid,assigned:($assigned|length),
         terminating:([$assigned[]|select(.metadata.deletionTimestamp!=null)]|length),
         freeSlots:(($node.status.allocatable.pods|tonumber)-($assigned|length))}] |
    {nodes:.,aggregateFreeSlots:(map(.freeSlots)|add),requiredAggregate:($outstanding+1),
     minimumPerNode:2,schedulabilityGuaranteed:false} |
    if all(.nodes[]; .freeSlots>=2) and .aggregateFreeSlots>=.requiredAggregate then .
    else error("guest Pod-slot preflight failed: " + tojson) end'
run seed-identities 11m "$HCP_PERF" seed identities --input "$inputs/seed.json" --kubeconfig "$GUEST_ADMIN_KUBECONFIG" --timeout 10m
run seed-credentials 11m "$HCP_PERF" seed credentials --input "$inputs/seed.json" --kubeconfig "$GUEST_ADMIN_KUBECONFIG" \
    --timeout 10m --expiration 6h --output "$artifacts_dir/private/benchmark"
run workload-setup 11m "$HCP_PERF" workload setup --input "$inputs/setup.json" --kubeconfig "$GUEST_ADMIN_KUBECONFIG" --timeout 10m
run canary-credentials 11m "$HCP_PERF" workload credentials --input "$inputs/setup.json" --kubeconfig "$GUEST_ADMIN_KUBECONFIG" \
    --timeout 10m --expiration 6h --output "$artifacts_dir/private/canary.kubeconfig"

canary=("$HCP_PERF" canary run --namespace "$canary_namespace" --deployment canary \
    --kubeconfig "$artifacts_dir/private/canary.kubeconfig" --timeout 5m)
# One unscored image-pull cycle, then five consecutive baselines. Never retry a failure.
run canary-warmup 6m "${canary[@]}" --request-log "$commands/canary-warmup.requests.jsonl" --ack-budget 30s --ready-budget 2m --cleanup-budget 2m
for baseline_cycle in 1 2 3 4 5; do
    run "canary-baseline-$baseline_cycle" 6m "${canary[@]}" --request-log "$commands/canary-baseline-$baseline_cycle.requests.jsonl" --ack-budget 30s --ready-budget 2m --cleanup-budget 2m
done
run baseline-expectations 10s jq -es '
    if length!=5 or any(.[]; any(.ack,.ready,.cleanup;
      .durationNanoseconds<=0 or .completed=="0001-01-01T00:00:00Z" or .completed==null))
    then error("five successful complete cycles required") else
      {ack:([([.[].ack.durationNanoseconds]|max)*3,30000000000]|min),
       ready:([([.[].ready.durationNanoseconds]|max)*3,120000000000]|min),
       cleanup:([([.[].cleanup.durationNanoseconds]|max)*3,120000000000]|min)} end' \
    "$commands/"canary-baseline-{1,2,3,4,5}.json
ack=$(jq -er '.ack|tostring+"ns"' "$commands/baseline-expectations.json")
ready=$(jq -er '.ready|tostring+"ns"' "$commands/baseline-expectations.json")
cleanup=$(jq -er '.cleanup|tostring+"ns"' "$commands/baseline-expectations.json")

if $contended; then
    adversaries_attempted=true
    run adversaries-start 6m "$HCP_PERF" adversaries start --input "$inputs/adversaries.json" --kubeconfig "$MGMT_KUBECONFIG" --timeout 5m
    poll_session adversaries "$adversary_namespace" "$adversary_id" '
        .phase=="running" and .submissionIncomplete==false and .nonterminalPods>0 and
        (.jobs|length>0 and all(.[]; .phase=="running" and (.pods|length>0 and
        all(.[]; .phase=="Running" and .ready==true and .deleting==false))))' adversaries-running 300
fi
run settle "$((settle + 10))s" sleep "$settle"
if $contended; then
    jq '[.[]|select(.name=="adversary-cpu-rate")]' "$inputs/node-queries.json" >"$inputs/pressure-queries.json"
    check_samples pressure-observed "$NODE_METRICS_ENDPOINT" "$inputs/pressure-queries.json" false
    pressure_start=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    printf '%s\n' "$pressure_start" >"$evidence/pressure-start.txt"
fi
run scrape-steady 70s "$HCP_PERF" scrape check --input "$inputs/bundle.json" --endpoint "$CAPTURE_ENDPOINT" --timeout 1m
run record-steady 40s "$HCP_PERF" record status --namespace "$record_namespace" --id "$record_id" --kubeconfig "$MGMT_KUBECONFIG" --timeout 30s
run record-steady-gate 10s jq -e "$record_ready" "$commands/record-steady.json"
# TokenRequest timestamps may use the local RFC3339 offset rather than UTC Z.
run token-lifetime 10s jq -en --argjson namespaces "$namespace_count" \
    --argjson remaining "$((duration + recovery + 300))" \
    --slurpfile seed "$commands/seed-credentials.json" --slurpfile canary "$commands/canary-credentials.json" '
    def expiry:
        ([capture("^(?<date>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<zone>Z|[+-][0-9]{2}:[0-9]{2})$")][0] // error("invalid credential expiry")) |
        (.date+"Z"|fromdateiso8601) -
        (if .zone=="Z" then 0 else
            ((.zone[1:3]|tonumber)*3600+(.zone[4:6]|tonumber)*60) *
            (if .zone[0:1]=="-" then -1 else 1 end) end);
    ($seed[0].files|type)=="array" and ($seed[0].files|length)==$namespaces and all($seed[0].files[], $canary[0];
    (.validUntil|expiry)>now+$remaining)'

# The parent supervises both commands; canaries are serial and never detached.
# Start-to-start slots are fixed; overruns skip slots rather than catching up.
steady_seconds=$SECONDS
date -u +%Y-%m-%dT%H:%M:%SZ >"$evidence/steady-start.txt"
steady_epoch=$(date -u -d "$(<"$evidence/steady-start.txt")" +%s)
: >"$evidence/canary-cadence.jsonl"
steady_started=true
cp "$evidence/steady-start.txt" "$commands/turnover.started"
timeout --kill-after=150s "$((duration + recovery + 120))s" "$HCP_PERF" turnover run --input "$inputs/turnover.json" \
    --credentials-dir "$artifacts_dir/private/benchmark" --output "$evidence/turnover" \
    --request-log "$commands/turnover.requests.jsonl" \
    >"$commands/turnover.json" 2>"$commands/turnover.stderr" &
turnover_pid=$!
cycle=0 slot=0
while kill -0 "$turnover_pid" 2>/dev/null; do
    if ((slot < expected_canaries && SECONDS >= steady_seconds + slot * cadence && SECONDS < steady_seconds + duration)); then
        # If the supervisor itself stalled, do not admit a backlog of old slots.
        if ((SECONDS >= steady_seconds + (slot + 1) * cadence)); then
            record_missed_slots "$((SECONDS - steady_seconds))" supervisor-delay
            continue
        fi
        current_slot=$slot
        slot=$((slot + 1))
        cycle=$((cycle + 1))
        label=canary-steady-$((current_slot + 1))
        jq -cn --argjson slot "$current_slot" --argjson epoch "$((steady_epoch + current_slot * cadence))" --arg command "$label" \
            '{type:"cycle-started",slotIndex:$slot,canaryNumber:($slot+1),scheduledAt:($epoch|todateiso8601),
              startedAt:(now|todateiso8601),command:$command}' >>"$evidence/canary-cadence.jsonl"
        sync -f "$evidence/canary-cadence.jsonl"
        if ! run "$label" 6m "${canary[@]}" --request-log "$commands/$label.requests.jsonl" --ack-expected "$ack" --ready-expected "$ready" --cleanup-expected "$cleanup" \
            --ack-budget 30s --ready-budget 2m --cleanup-budget 2m; then
            stop_reason=${failed_class:-harness_error}
            printf '%s\n' "$((current_slot + 1))" >"$evidence/canary-failed.txt"
            stop_local "$turnover_pid" turnover
            turnover_pid=
            # Observe bounded recovery with existing CPU pressure, but admit no new load.
            run failure-recovery "$((recovery + 10))s" sleep "$recovery"
            die 'canary failed; turnover stopped and bounded recovery observed'
        fi
        after_window=$((SECONDS >= steady_seconds + duration))
        completed_after_window=$((completed_after_window + after_window))
        jq -cn --argjson slot "$current_slot" --arg command "$label" --argjson afterWindow "$after_window" \
            '{type:"cycle-completed",slotIndex:$slot,canaryNumber:($slot+1),command:$command,
              completedAt:(now|todateiso8601),afterWindow:($afterWindow==1),scored:($afterWindow==0),
              resultFile:("commands/"+$command+".json")}' >>"$evidence/canary-cadence.jsonl"
        record_missed_slots "$((SECONDS - steady_seconds))" cadence-overrun
    fi
    sleep 1
done
turnover_rc=0
wait "$turnover_pid" || turnover_rc=$?
turnover_pid=
printf '%s\n' "$turnover_rc" >"$commands/turnover.exit"
record_missed_slots "$duration" run-ended
if ((turnover_rc != 0)); then
    record_failure turnover "$turnover_rc" || ledger_failed=1
    run failure-recovery "$((recovery + 10))s" sleep "$recovery"
    die "turnover failed ($turnover_rc)"
fi
((SECONDS >= steady_seconds + duration)) || die 'turnover ended before the full steady window'
# The CLI may finish recovery early when no work remains, including zero apps.
remaining=$((steady_seconds + duration + recovery - SECONDS))
if ((remaining > 0)); then run recovery-window "$((remaining + 10))s" sleep "$remaining"; fi
workload_completed=true
run turnover-gate 10s jq -e --argjson scheduled "$scheduled" \
    'if all(.scheduled,.completed,.failed,.notStarted,.backlog,.outstanding; type=="number" and floor==. and .>=0)
     then .scheduled==$scheduled and .completed==$scheduled and .failed==0 and .notStarted==0 and .backlog==0 and .outstanding==0
     else error("invalid turnover counters") end' "$commands/turnover.json"

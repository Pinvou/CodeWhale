#!/usr/bin/env bash
set -euo pipefail
trap 'echo "session-lineage rollout command failed at line $LINENO" >&2' ERR

# Audited two-phase activation for Portal session-lineage logout.
# Evidence timestamps come from Kubernetes API object metadata, never the operator host.

action=${1:?phase1|phase2|rollback|binary-rollback-check}
namespace=${2:?namespace}
release=${3:?helm release}
repository=${4:?image repository}
tag=${5:?image tag}
digest=${6:?sha256:digest}
chart=${CHART_PATH:-deploy/helm/account-platform}
selector="app.kubernetes.io/instance=${release},app.kubernetes.io/component=token-portal"
deployment=
phase1_evidence="${release}-session-lineage-phase1"
activation_evidence="${release}-session-lineage-activation"
rollback_evidence="${release}-session-lineage-binary-rollback"
rollback_time_evidence="${release}-session-lineage-rollback-time"
lock_name="${release}-session-lineage-rollout-lock"
holder="${USER:-operator}-$$-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
lock_uid=
lock_resource_version=
restore_needed=false
restore_enabled=false
restore_replicas=0

[[ "$digest" == sha256:* ]] || { echo "digest must be sha256:<hex>" >&2; exit 2; }

kubectl() {
  local limit=45 request_timeout=30s
  [[ " $* " == *" wait "* ]] && { limit=330; request_timeout=300s; }
  command timeout --signal=TERM --kill-after=10 "$limit" kubectl --request-timeout="$request_timeout" "$@"
}

server_timestamp() {
  local probe="${release}-lineage-time-$$-$RANDOM" stamp
  kubectl -n "$namespace" create configmap "$probe" --from-literal=purpose=trusted-time-probe >/dev/null
  stamp=$(kubectl -n "$namespace" get configmap "$probe" -o jsonpath='{.metadata.creationTimestamp}')
  kubectl -n "$namespace" delete configmap "$probe" --wait=true >/dev/null
  printf '%s\n' "$stamp"
}
server_epoch() { node -e 'console.log(Math.floor(Date.parse(process.argv[1])/1000))' "$(server_timestamp)"; }

release_lock() {
  [[ -n "$lock_uid" ]] || return 0
  renew_lock
  [[ "$(kubectl -n "$namespace" get lease "$lock_name" -o jsonpath='{.metadata.uid}')" == "$lock_uid" ]]
  [[ "$(kubectl -n "$namespace" get lease "$lock_name" -o jsonpath='{.spec.holderIdentity}')" == "$holder" ]]
  [[ "$(kubectl -n "$namespace" get lease "$lock_name" -o jsonpath='{.metadata.resourceVersion}')" == "$lock_resource_version" ]]
  local proxy_log proxy_pid proxy_port body
  proxy_log=$(mktemp)
  kubectl proxy --port=0 --accept-hosts='^127\.0\.0\.1$' >"$proxy_log" 2>&1 & proxy_pid=$!
  proxy_port=
  for _ in {1..50}; do proxy_port=$(sed -nE 's/.*127\.0\.0\.1:([0-9]+).*/\1/p' "$proxy_log" | head -1); [[ -n "$proxy_port" ]] && break; sleep 0.1; done
  [[ -n "$proxy_port" ]] || { kill "$proxy_pid" 2>/dev/null || true; wait "$proxy_pid" 2>/dev/null || true; rm -f "$proxy_log"; return 1; }
  body="{\"apiVersion\":\"v1\",\"kind\":\"DeleteOptions\",\"preconditions\":{\"uid\":\"$lock_uid\",\"resourceVersion\":\"$lock_resource_version\"}}"
  if ! curl --fail --silent --show-error --connect-timeout 2 --max-time 10 -X DELETE -H 'Content-Type: application/json' --data "$body" "http://127.0.0.1:$proxy_port/apis/coordination.k8s.io/v1/namespaces/$namespace/leases/$lock_name" >/dev/null; then
    kill "$proxy_pid" 2>/dev/null || true; wait "$proxy_pid" 2>/dev/null || true; rm -f "$proxy_log"; return 1
  fi
  kill "$proxy_pid" 2>/dev/null || true; wait "$proxy_pid" 2>/dev/null || true; rm -f "$proxy_log"
  if kubectl -n "$namespace" get lease "$lock_name" >/dev/null 2>&1; then echo "failed to release rollout Lease $lock_name" >&2; return 1; fi
  lock_uid=
}

renew_lock() {
  [[ -n "$lock_uid" ]] || return 0
  local existing now updated
  existing=$(kubectl -n "$namespace" get lease "$lock_name" -o json)
  EXISTING="$existing" EXPECTED_UID="$lock_uid" EXPECTED_HOLDER="$holder" node -e 'const x=JSON.parse(process.env.EXISTING);if(x.metadata.uid!==process.env.EXPECTED_UID||x.spec.holderIdentity!==process.env.EXPECTED_HOLDER||!x.metadata.resourceVersion)process.exit(1)'
  now=$(server_timestamp)
  updated=$(EXISTING="$existing" NOW="$now" node -e 'const x=JSON.parse(process.env.EXISTING);delete x.metadata.managedFields;x.spec.renewTime=process.env.NOW;console.log(JSON.stringify(x))' | kubectl -n "$namespace" replace -f - -o json) || { echo "rollout Lease renewal CAS lost" >&2; return 1; }
  lock_resource_version=$(UPDATED="$updated" EXPECTED_UID="$lock_uid" EXPECTED_HOLDER="$holder" node -e 'const x=JSON.parse(process.env.UPDATED);if(x.metadata.uid!==process.env.EXPECTED_UID||x.spec.holderIdentity!==process.env.EXPECTED_HOLDER)process.exit(1);console.log(x.metadata.resourceVersion)')
}

cleanup() {
  local status=$?
  trap - EXIT
  if [[ "$status" -ne 0 && "$restore_needed" == true ]]; then
    echo "rollout failed; restoring replicas=$restore_replicas enabled=$restore_enabled" >&2
    renew_lock || { echo "compensation refused because Lease ownership was lost" >&2; restore_needed=false; }
  fi
  if [[ "$status" -ne 0 && "$restore_needed" == true ]]; then
    timeout --signal=TERM --kill-after=30 660 helm upgrade "$release" "$chart" -n "$namespace" --atomic --timeout 10m --reuse-values \
      --set "images.tokenPortal.repository=$repository" \
      --set "images.tokenPortal.tag=$tag@$digest" \
      --set "replicas.tokenPortal=$restore_replicas" \
      --set "portal.sessionLineageLogoutEnabled=$restore_enabled" || true
    renew_lock || echo "Lease renewal after compensation failed" >&2
  fi
  if ! release_lock; then echo "rollout Lease release failed" >&2; [[ "$status" -eq 0 ]] && status=1; fi
  exit "$status"
}
trap cleanup EXIT

acquire_lock() {
  local created now age existing renewed duration replacement
  now=$(server_timestamp)
  created=$(HOLDER="$holder" LOCK_NAME="$lock_name" NAMESPACE="$namespace" NOW="$now" node -e 'console.log(JSON.stringify({apiVersion:"coordination.k8s.io/v1",kind:"Lease",metadata:{name:process.env.LOCK_NAME,namespace:process.env.NAMESPACE},spec:{holderIdentity:process.env.HOLDER,leaseDurationSeconds:1800,acquireTime:process.env.NOW,renewTime:process.env.NOW}}))' | kubectl -n "$namespace" create -f - -o json 2>/dev/null) || true
  if [[ -z "$created" ]]; then
    existing=$(kubectl -n "$namespace" get lease "$lock_name" -o json)
    renewed=$(EXISTING="$existing" node -e 'const x=JSON.parse(process.env.EXISTING);console.log(x.spec.renewTime||x.metadata.creationTimestamp)')
    duration=$(EXISTING="$existing" node -e 'const x=JSON.parse(process.env.EXISTING);console.log(x.spec.leaseDurationSeconds||1800)')
    age=$(NOW="$now" RENEWED="$renewed" node -e 'console.log(Math.floor((Date.parse(process.env.NOW)-Date.parse(process.env.RENEWED))/1000))')
    (( age > duration )) || { echo "another rollout holds $lock_name (age=${age}s)" >&2; exit 1; }
    replacement=$(EXISTING="$existing" HOLDER="$holder" NOW="$now" node -e 'const x=JSON.parse(process.env.EXISTING);delete x.metadata.managedFields;x.spec.holderIdentity=process.env.HOLDER;x.spec.acquireTime=process.env.NOW;x.spec.renewTime=process.env.NOW;x.spec.leaseDurationSeconds=1800;x.spec.leaseTransitions=(x.spec.leaseTransitions||0)+1;console.log(JSON.stringify(x))')
    created=$(printf '%s\n' "$replacement" | kubectl -n "$namespace" replace -f - -o json) || { echo "rollout Lease CAS lost; retry later" >&2; exit 1; }
  fi
  lock_uid=$(node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).metadata.uid))' <<<"$created")
  lock_resource_version=$(node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).metadata.resourceVersion))' <<<"$created")
}

replicas() { kubectl -n "$namespace" get deployment "$deployment" -o jsonpath='{.spec.replicas}'; }

verify_pods() {
  local enabled=$1 expected=$2 check_digest=${3:-true} json rows count invalid forward_log forward_pid port status
  json=$(kubectl -n "$namespace" get pods -l "$selector" -o json)
  count=$(node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).items.length))' <<<"$json")
  [[ "$count" == "$expected" ]] || { echo "expected $expected labelled pods, found $count" >&2; return 1; }
  invalid=$(node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const x=JSON.parse(s).items.filter(p=>p.metadata.deletionTimestamp||!p.status.conditions?.some(c=>c.type==="Ready"&&c.status==="True"));console.log(x.map(p=>p.metadata.name).join(","))})' <<<"$json")
  [[ -z "$invalid" ]] || { echo "not Ready or terminating: $invalid" >&2; return 1; }
  rows=$(node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).items.map(p=>`${p.metadata.uid} ${p.status.containerStatuses?.find(c=>c.name==="token-portal")?.imageID||""} ${p.metadata.name}`).sort().join("\n")))' <<<"$json")
  if [[ "$check_digest" == true ]]; then while read -r _ image_id _; do [[ "$image_id" == *"$digest"* ]] || { echo "pod imageID does not contain $digest" >&2; return 1; }; done <<<"$rows"; fi
  while read -r _ _ pod; do
    renew_lock
    forward_log=$(mktemp)
    kubectl -n "$namespace" port-forward "pod/$pod" ":8080" --address=127.0.0.1 >"$forward_log" 2>&1 & forward_pid=$!
    port=
    for _ in {1..50}; do port=$(sed -nE 's/.*127\.0\.0\.1:([0-9]+).*/\1/p' "$forward_log" | head -1); [[ -n "$port" ]] && break; sleep 0.1; done
    if [[ -z "$port" ]]; then cat "$forward_log" >&2; kill "$forward_pid" 2>/dev/null || true; wait "$forward_pid" 2>/dev/null || true; rm -f "$forward_log"; return 1; fi
    if ! status=$(curl --fail --silent --show-error --connect-timeout 2 --max-time 5 "http://127.0.0.1:$port/health/session-lineage"); then
      kill "$forward_pid" 2>/dev/null || true; wait "$forward_pid" 2>/dev/null || true; rm -f "$forward_log"; return 1
    fi
    kill "$forward_pid" 2>/dev/null || true; wait "$forward_pid" 2>/dev/null || true; rm -f "$forward_log"
    if ! STATUS="$status" EXPECTED="$enabled" node -e 'const x=JSON.parse(process.env.STATUS);if(x.schema_version!==1||x.capable!==true||String(x.enabled)!==process.env.EXPECTED)process.exit(1)'; then return 1; fi
    renew_lock
  done <<<"$rows"
  printf '%s\n' "$rows"
}

pod_set_hash() { sha256sum | awk '{print $1}'; }

helm_apply() {
  local enabled=$1 count=$2
  renew_lock
  timeout --signal=TERM --kill-after=30 660 helm upgrade "$release" "$chart" -n "$namespace" --atomic --timeout 10m --reuse-values \
    --set "images.tokenPortal.repository=$repository" \
    --set "images.tokenPortal.tag=$tag@$digest" \
    --set "replicas.tokenPortal=$count" \
    --set "portal.sessionLineageLogoutEnabled=$enabled"
  renew_lock
}

scale_zero() {
  local remaining
  renew_lock
  kubectl -n "$namespace" scale deployment "$deployment" --replicas=0
  remaining=$(kubectl -n "$namespace" get pods -l "$selector" -o jsonpath='{.items[*].metadata.uid}')
  if [[ -n "$remaining" ]]; then kubectl -n "$namespace" wait --for=delete pod -l "$selector" --timeout=300s || true; fi
  remaining=$(kubectl -n "$namespace" get pods -l "$selector" -o jsonpath='{.items[*].metadata.uid}')
  [[ -z "$remaining" ]] || { echo "token-portal Pods remain after scale-to-zero" >&2; return 1; }
  renew_lock
}

deployment_flag_is_disabled() {
  local json
  json=$(kubectl -n "$namespace" get deployment "$deployment" -o json)
  DEPLOYMENT="$json" node -e 'const x=JSON.parse(process.env.DEPLOYMENT),c=x.spec.template.spec.containers.find(v=>v.name==="token-portal");if(!c)process.exit(1);const vars=(c.env||[]).filter(e=>e.name==="PORTAL_SESSION_LINEAGE_LOGOUT_ENABLED");if(vars.length===0)process.exit(0);const v=vars[0];if(vars.length!==1||Object.hasOwn(v,"valueFrom")||!Object.hasOwn(v,"value")||typeof v.value!=="string"||v.value!=="false")process.exit(1)'
}

evidence_value() { kubectl -n "$namespace" get configmap "$1" -o go-template --template "{{ index .data \"$2\" }}"; }

require_phase1_evidence() {
  local stamp born now not_before expected_hash current_hash expected_replicas completed_at recorded_not_before
  [[ "$(evidence_value "$phase1_evidence" phase1-digest)" == "$digest" ]]
  [[ "$(evidence_value "$phase1_evidence" capable)" == true ]]
  [[ "$(evidence_value "$phase1_evidence" enabled)" == false ]]
  completed_at=$(evidence_value "$phase1_evidence" completed-at)
  node -e 'if(!Number.isFinite(Date.parse(process.argv[1])))process.exit(1)' "$completed_at"
  recorded_not_before=$(evidence_value "$phase1_evidence" phase2-not-before)
  [[ "$recorded_not_before" =~ ^[0-9]+$ ]]
  expected_hash=$(evidence_value "$phase1_evidence" phase1-pod-set-sha256)
  expected_replicas=$(replicas)
  current_hash=$(verify_pods false "$expected_replicas" | pod_set_hash)
  [[ "$current_hash" == "$expected_hash" ]] || { echo "pod set changed since phase1" >&2; return 1; }
  stamp=$(kubectl -n "$namespace" get configmap "$phase1_evidence" -o jsonpath='{.metadata.creationTimestamp}')
  born=$(node -e 'console.log(Math.floor(Date.parse(process.argv[1])/1000))' "$stamp")
  now=$(server_epoch); not_before=$((born + 600))
  (( recorded_not_before >= $(node -e 'console.log(Math.floor(Date.parse(process.argv[1])/1000)+600)' "$completed_at") ))
  (( now >= not_before && now >= recorded_not_before )) || { echo "phase2-not-before=$not_before cluster-now=$now" >&2; return 1; }
}

acquire_lock
timeout --signal=TERM --kill-after=10 45 helm status "$release" -n "$namespace" >/dev/null
deployment=$(kubectl -n "$namespace" get deployment -l "$selector" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
[[ -n "$deployment" && "$deployment" != *$'\n'* ]] || { echo "expected exactly one token-portal deployment" >&2; exit 1; }
case "$action" in
  phase1)
    kubectl -n "$namespace" get configmap "$phase1_evidence" >/dev/null 2>&1 && { echo "$phase1_evidence already exists; archive/delete it explicitly for a new attempt" >&2; exit 1; }
    desired=$(replicas)
    deployment_flag_is_disabled || { echo "phase1 refused: session-lineage flag is not absent/false; run rollback first" >&2; exit 1; }
    helm_apply false "$desired"
    rows=$(verify_pods false "$desired")
    hash=$(printf '%s\n' "$rows" | pod_set_hash)
    completed_at=$(server_timestamp)
    completed_epoch=$(node -e 'console.log(Math.floor(Date.parse(process.argv[1])/1000))' "$completed_at")
    not_before=$((completed_epoch + 600))
    kubectl -n "$namespace" create configmap "$phase1_evidence" \
      --from-literal="phase1-digest=$digest" --from-literal="phase1-pod-set-sha256=$hash" \
      --from-literal="capable=true" --from-literal="enabled=false" --from-literal="completed-at=$completed_at" \
      --from-literal="phase2-not-before=$not_before" --dry-run=client -o json | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const x=JSON.parse(s);x.immutable=true;console.log(JSON.stringify(x))})' | kubectl -n "$namespace" create -f - >/dev/null
    ;;
  phase2)
    require_phase1_evidence
    restore_replicas=$(replicas); restore_enabled=false; restore_needed=true
    kubectl -n "$namespace" delete configmap "$rollback_evidence" "$rollback_time_evidence" --ignore-not-found --wait=true >/dev/null
    generation=$(kubectl -n "$namespace" get configmap "$phase1_evidence" -o jsonpath='{.metadata.uid}')
    scale_zero
    helm_apply true "$restore_replicas"
    verify_pods true "$restore_replicas" >/dev/null
    kubectl -n "$namespace" delete configmap "$activation_evidence" --ignore-not-found --wait=true >/dev/null
    kubectl -n "$namespace" create configmap "$activation_evidence" --from-literal="activation-generation=$generation" --from-literal="activation-digest=$digest" >/dev/null
    restore_needed=false
    ;;
  rollback)
    generation=$(evidence_value "$activation_evidence" activation-generation)
    [[ "$(evidence_value "$activation_evidence" activation-digest)" == "$digest" ]]
    restore_replicas=$(replicas); restore_enabled=true; restore_needed=true
    verify_pods true "$restore_replicas" >/dev/null
    scale_zero
    helm_apply false "$restore_replicas"
    verify_pods false "$restore_replicas" >/dev/null
    kubectl -n "$namespace" delete configmap "$rollback_evidence" "$rollback_time_evidence" --ignore-not-found --wait=true >/dev/null
    kubectl -n "$namespace" create configmap "$rollback_time_evidence" --from-literal="activation-generation=$generation" --dry-run=client -o json | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const x=JSON.parse(s);x.immutable=true;console.log(JSON.stringify(x))})' | kubectl -n "$namespace" create -f - >/dev/null
    stamp=$(kubectl -n "$namespace" get configmap "$rollback_time_evidence" -o jsonpath='{.metadata.creationTimestamp}')
    time_uid=$(kubectl -n "$namespace" get configmap "$rollback_time_evidence" -o jsonpath='{.metadata.uid}')
    not_before=$(node -e 'console.log(Math.floor(Date.parse(process.argv[1])/1000)+2592000)' "$stamp")
    kubectl -n "$namespace" create configmap "$rollback_evidence" --from-literal="activation-generation=$generation" --from-literal="activation-digest=$digest" --from-literal="time-source-uid=$time_uid" --from-literal="time-source-created-at=$stamp" --from-literal="binary-rollback-not-before=$not_before" --dry-run=client -o json | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const x=JSON.parse(s);x.immutable=true;console.log(JSON.stringify(x))})' | kubectl -n "$namespace" create -f - >/dev/null
    restore_needed=false
    ;;
  binary-rollback-check)
    generation=$(evidence_value "$activation_evidence" activation-generation)
    [[ "$(evidence_value "$activation_evidence" activation-digest)" == "$digest" ]]
    [[ "$(evidence_value "$rollback_evidence" activation-generation)" == "$generation" ]]
    [[ "$(evidence_value "$rollback_evidence" activation-digest)" == "$digest" ]]
    time_uid=$(evidence_value "$rollback_evidence" time-source-uid); [[ "$time_uid" == "$(kubectl -n "$namespace" get configmap "$rollback_time_evidence" -o jsonpath='{.metadata.uid}')" ]]
    stamp=$(evidence_value "$rollback_evidence" time-source-created-at); [[ "$stamp" == "$(kubectl -n "$namespace" get configmap "$rollback_time_evidence" -o jsonpath='{.metadata.creationTimestamp}')" ]]
    desired=$(replicas); verify_pods false "$desired" >/dev/null
    now=$(server_epoch); not_before=$(evidence_value "$rollback_evidence" binary-rollback-not-before); [[ "$not_before" =~ ^[0-9]+$ ]]
    [[ "$not_before" == "$(node -e 'console.log(Math.floor(Date.parse(process.argv[1])/1000)+2592000)' "$stamp")" ]]
    (( now >= not_before )) || { echo "binary rollback forbidden until $not_before (cluster now $now)" >&2; exit 1; }
    echo "binary rollback permitted for generation=$generation digest=$digest"
    ;;
  *) echo "unknown action: $action" >&2; exit 2 ;;
esac

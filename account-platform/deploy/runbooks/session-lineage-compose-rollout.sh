#!/usr/bin/env bash
set -euo pipefail
umask 077

action=${1:?phase1|phase2|rollback|binary-rollback-check}
image=${2:?repository@sha256:digest}
evidence_dir=${3:?evidence directory}
portal_port=${TOKEN_PORTAL_BIND_PORT:?set verified TOKEN_PORTAL_BIND_PORT}
base=deploy/production/compose.business.yaml
enabled=deploy/production/compose.session-lineage-enabled.yaml
[[ ! -L "$evidence_dir" ]] || { echo "evidence directory must not be a symlink" >&2; exit 2; }
mkdir -p "$evidence_dir"
chmod 700 "$evidence_dir"
[[ -O "$evidence_dir" && "$(stat -c '%a' "$evidence_dir")" == 700 ]] || { echo "evidence directory must be owner-only mode 700" >&2; exit 2; }
phase1="$evidence_dir/phase1"
activation="$evidence_dir/activation"
rollback="$evidence_dir/rollback"
lock="$evidence_dir/lock"
for evidence_path in "$phase1" "$activation" "$rollback" "$lock"; do [[ ! -L "$evidence_path" ]] || { echo "evidence path must not be a symlink: $evidence_path" >&2; exit 2; }; done
restore=false
restore_enabled=false
restore_image="$image"
restore_running=true
exec 9>"$lock"
flock -n 9 || { echo "another compose rollout is active" >&2; exit 1; }
[[ "$image" == *@sha256:* ]] || { echo "image must be digest pinned" >&2; exit 2; }

container_id() { TOKEN_PORTAL_IMAGE="$image" docker compose -f "$base" ps -q --all token-portal; }
inspect() { local cid; cid=$(container_id); [[ -n "$cid" ]]; docker inspect "$cid" --format '{{.Id}} {{.Config.Image}} {{.Image}} {{.State.StartedAt}} {{.State.Running}}'; }
verify_image() { local repos; repos=$(docker image inspect "$image" --format '{{json .RepoDigests}}'); REPOS="$repos" EXPECTED="$image" node -e 'const x=JSON.parse(process.env.REPOS);if(!x.includes(process.env.EXPECTED))process.exit(1)'; }
container_flag_is_disabled() { local cid env_json; cid=$(container_id); env_json=$(docker inspect "$cid" --format '{{json .Config.Env}}'); ENV_JSON="$env_json" node -e 'const x=JSON.parse(process.env.ENV_JSON),v=x.find(e=>e.startsWith("PORTAL_SESSION_LINEAGE_LOGOUT_ENABLED="));if(v&&v!=="PORTAL_SESSION_LINEAGE_LOGOUT_ENABLED=false")process.exit(1)'; }
probe() { curl --fail --silent --show-error --connect-timeout 2 --max-time 5 "http://127.0.0.1:$portal_port/health/session-lineage"; }
require_enabled() { EXPECTED=$1 STATUS=$(probe) node -e 'const x=JSON.parse(process.env.STATUS);if(x.capable!==true||String(x.enabled)!==process.env.EXPECTED)process.exit(1)'; }
daemon_epoch() {
  local cid stamp
  cid=$(docker create "$image")
  stamp=$(docker inspect "$cid" --format '{{.Created}}')
  docker rm "$cid" >/dev/null
  node -e 'console.log(Math.floor(Date.parse(process.argv[1])/1000))' "$stamp"
}
age_seconds() { node -e 'console.log(Number(process.argv[1])-Math.floor(Date.parse(process.argv[2])/1000))' "$(daemon_epoch)" "$1"; }
compose_up() {
  local target_image=$1 target_enabled=$2
  if [[ "$target_enabled" == true ]]; then
    TOKEN_PORTAL_IMAGE="$target_image" docker compose -f "$base" -f "$enabled" up -d --no-deps --force-recreate --wait --wait-timeout 120 token-portal
  else
    TOKEN_PORTAL_IMAGE="$target_image" docker compose -f "$base" up -d --no-deps --force-recreate --wait --wait-timeout 120 token-portal
  fi
}
safe_create_evidence() {
  local target=$1 content=$2 temporary
  [[ ! -e "$target" && ! -L "$target" ]] || { echo "evidence already exists or is unsafe: $target" >&2; return 1; }
  temporary=$(mktemp "$evidence_dir/.evidence.XXXXXX")
  printf '%s\n' "$content" >"$temporary"; chmod 600 "$temporary"
  if ! ln "$temporary" "$target"; then rm -f "$temporary"; return 1; fi
  rm -f "$temporary"
  [[ -f "$target" && ! -L "$target" && -O "$target" && "$(stat -c '%a' "$target")" == 600 && "$(<"$target")" == "$content" ]] || return 1
}
safe_replace_evidence() {
  local target=$1 content=$2 temporary
  [[ ! -e "$target" || ( -f "$target" && ! -L "$target" && -O "$target" ) ]] || { echo "unsafe evidence target: $target" >&2; return 1; }
  temporary=$(mktemp "$evidence_dir/.evidence.XXXXXX")
  printf '%s\n' "$content" >"$temporary"; chmod 600 "$temporary"; mv -f "$temporary" "$target"
  [[ -f "$target" && ! -L "$target" && -O "$target" && "$(stat -c '%a' "$target")" == 600 && "$(<"$target")" == "$content" ]]
}
cleanup() {
  local status=$?
  trap - EXIT
  if [[ "$status" -ne 0 && "$restore" == true ]]; then
    if [[ "$restore_enabled" == true ]]; then
      compose_up "$restore_image" true || { echo "failed to restore enabled baseline" >&2; exit "$status"; }
    else
      compose_up "$restore_image" false || { echo "failed to restore disabled baseline" >&2; exit "$status"; }
    fi
    [[ "$restore_running" == true ]] || TOKEN_PORTAL_IMAGE="$restore_image" docker compose -f "$base" stop token-portal || true
    if [[ "$restore_running" == true ]]; then require_enabled "$restore_enabled" || { echo "restored baseline did not become available" >&2; exit "$status"; }; fi
  fi
  exit "$status"
}
trap cleanup EXIT

case "$action" in
  phase1)
    [[ ! -e "$phase1" ]] || { echo "phase1 evidence exists; archive it explicitly" >&2; exit 1; }
    container_flag_is_disabled || { echo "phase1 refused: session-lineage flag is not absent/false; run rollback first" >&2; exit 1; }
    verify_image
    read -r _ _ restore_image _ restore_running <<<"$(inspect)"
    restore=true; restore_enabled=false
    compose_up "$image" false
    require_enabled false
    safe_create_evidence "$phase1" "$(inspect)"
    restore=false
    ;;
  phase2)
    verify_image
    read -r old_id old_image old_image_id old_started old_running <"$phase1"
    [[ "$old_image" == "$image" && "$old_running" == true ]] && (( $(age_seconds "$old_started") >= 600 ))
    [[ "$(inspect)" == "$old_id $old_image $old_image_id $old_started $old_running" ]] || { echo "container changed since phase1" >&2; exit 1; }
    require_enabled false
    rm -f "$rollback"
    restore=true; restore_enabled=false
    TOKEN_PORTAL_IMAGE="$image" docker compose -f "$base" stop token-portal
    compose_up "$image" true
    require_enabled true
    safe_replace_evidence "$activation" "$(inspect)"
    restore=false
    ;;
  rollback)
    verify_image
    read -r activation_id activation_image activation_image_id activation_started activation_running <"$activation"
    [[ "$activation_image" == "$image" && "$activation_running" == true ]]
    [[ "$(inspect)" == "$activation_id $activation_image $activation_image_id $activation_started $activation_running" ]] || { echo "rollback refused: active container does not match activation evidence" >&2; exit 1; }
    require_enabled true || { echo "rollback refused: session-lineage flag is not enabled" >&2; exit 1; }
    restore=true; restore_enabled=true
    TOKEN_PORTAL_IMAGE="$image" docker compose -f "$base" -f "$enabled" stop token-portal
    compose_up "$image" false
    require_enabled false
    read -r rollback_id rollback_image rollback_image_id rollback_started rollback_running <<<"$(inspect)"
    safe_replace_evidence "$rollback" "$activation_id $activation_image $activation_image_id $rollback_id $rollback_image $rollback_image_id $rollback_started $rollback_running"
    restore=false
    ;;
  binary-rollback-check)
    verify_image
    read -r activation_id activation_image activation_image_id _ activation_running <"$activation"
    read -r recorded_activation_id recorded_activation_image recorded_activation_image_id rollback_id rollback_image rollback_image_id rollback_started rollback_running <"$rollback"
    [[ "$recorded_activation_id" == "$activation_id" && "$recorded_activation_image" == "$activation_image" && "$recorded_activation_image_id" == "$activation_image_id" ]]
    [[ "$activation_image" == "$rollback_image" && "$rollback_image" == "$image" && "$rollback_running" == true ]]
    [[ "$(inspect)" == "$rollback_id $rollback_image $rollback_image_id $rollback_started $rollback_running" ]]
    require_enabled false
    (( $(age_seconds "$rollback_started") >= 2592000 )) || { echo "30-day binary rollback delay not elapsed" >&2; exit 1; }
    ;;
  *) echo "unknown action: $action" >&2; exit 2 ;;
esac

#!/usr/bin/env bash
# Source in the remote deployment shell. Never execute old code to discover its
# lock support, and never remove a guard as an automatic rollback action.
controller_lock_protocol_supported() {
  [[ -f "$1/relay-state-guard.js" ]] \
    && grep -Fxq 'export const RELAY_CONTROLLER_LOCK_PROTOCOL = 2;' "$1/relay-state-guard.js"
}

controller_lock_stop_service() {
  local load_state active_state
  load_state="$(systemctl show "$1" --property=LoadState --value)" || return
  if [[ "$load_state" == not-found ]]; then return 0; fi
  systemctl stop "$1" || return
  if [[ "${controller_lock_early_armed:-0}" == 1 ]]; then
    # A successful stop command alone does not prove the unit has stopped.
    active_state="$(systemctl show "$1" --property=ActiveState --value)" || return
    case "$active_state" in
      inactive|failed) controller_lock_early_stopped=1 ;;
      *) echo 'controller_lock_stop_unconfirmed: retain the stopped state for operator inspection' >&2; return 78 ;;
    esac
  fi
}

# Arm only while the existing code and unit configuration remain untouched.
# All other writers and restart sources must already be stopped by the operator.
controller_lock_early_arm() {
  local load_state active_state
  controller_lock_early_armed=0
  controller_lock_early_stopped=0
  controller_lock_early_was_active=0
  load_state="$(systemctl show "$1" --property=LoadState --value)" || return
  if [[ "$load_state" != not-found ]]; then
    active_state="$(systemctl show "$1" --property=ActiveState --value)" || return
    case "$active_state" in
      active) controller_lock_early_was_active=1 ;;
      inactive|failed) ;;
      *) echo 'controller_lock_service_state_unknown: do not begin upgrade' >&2; return 78 ;;
    esac
  fi
  controller_lock_early_armed=1
}

controller_lock_early_recover() {
  local service="$1" state_dir="$2" old_code="$3"
  controller_lock_early_armed=0
  if [[ "${controller_lock_early_was_active:-0}" != 1 || "${controller_lock_early_stopped:-0}" != 1 ]]; then
    echo 'controller_lock_early_recovery_skipped: prior or stopped state was not confirmed' >&2
    return 0
  fi
  if [[ -e "$state_dir/.pinvou-controller-state.guard" || -L "$state_dir/.pinvou-controller-state.guard" ]] \
    && ! controller_lock_protocol_supported "$old_code"; then
    echo 'controller_lock_offline_downgrade_required: service remains stopped' >&2
    return 78
  fi
  if ! systemctl start "$service" || ! systemctl is-active --quiet "$service"; then
    # A failed start can have partially activated a unit. Do not report it as
    # recovered; best-effort stop and leave its final state for inspection.
    systemctl stop "$service" || true
    echo 'controller_lock_early_recovery_unconfirmed: inspect service state before resuming writers' >&2
    return 78
  fi
  echo 'controller_lock_early_recovery_started: previous service is active; verify health before resuming other writers' >&2
}

controller_lock_prepare_upgrade() {
  local service="$1" state_dir="$2" old_code="$3" backup="$4" confirmed="$5" entry=""
  if [[ -d "$state_dir" ]]; then
    entry="$(find "$state_dir" -mindepth 1 -maxdepth 1 -print -quit)" || return
  fi
  if ! controller_lock_protocol_supported "$old_code" \
    && [[ -n "$entry" ]] \
    && [[ "$confirmed" != 1 ]]; then
    echo 'controller_lock_upgrade_confirmation_required: stop all CLI/gateway writers and automatic restart sources before retrying with PINVOU_CONFIRM_CONTROLLER_LOCK_UPGRADE=1' >&2
    return 78
  fi
  # An explicit stop cancels systemd Restart=always before changing any code.
  # The operator's upgrade confirmation covers independently launched writers.
  controller_lock_stop_service "$service" || return
  mkdir -p "$backup" || return
  chmod 700 "$backup" || return
  if [[ -d "$state_dir" ]]; then
    if [[ -e "$backup/controller-state" || -L "$backup/controller-state" ]]; then
      echo 'controller_lock_upgrade_archive_already_exists' >&2
      return 78
    fi
    # Precondition on every upgrade: the operator has stopped all other shared-
    # directory writers and restart sources; stopping this unit alone is not proof.
    # Only under that precondition is this an offline archive, never a live copy.
    cp -a -- "$state_dir" "$backup/controller-state" || return
  fi
}

controller_lock_before_rollback() {
  local service="$1" state_dir="$2" old_code="$3"
  controller_lock_stop_service "$service" || return
  if [[ -e "$state_dir/.pinvou-controller-state.guard" \
    || -L "$state_dir/.pinvou-controller-state.guard" ]]; then
    if ! controller_lock_protocol_supported "$old_code"; then
      echo 'controller_lock_offline_downgrade_required: service remains stopped; preserve state and complete the documented administrator offline downgrade' >&2
      return 78
    fi
  fi
}

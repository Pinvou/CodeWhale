# Sourced by production deploy and its public-check rollback.

relay_code_manifest() (
  cd -- "$1" || exit
  export LC_ALL=C
  local module
  local -a modules=()
  for module in *.js; do
    [[ -e "$module" || -L "$module" ]] || continue
    [[ -f "$module" && ! -L "$module" ]] || exit 1
    modules+=("$module")
  done
  printf 'pinvou-relay-js-manifest-v2 %s\n' "${#modules[@]}" || exit
  for module in "${modules[@]}"; do
    sha256sum -- "$module" || exit
  done
)

relay_code_verify() {
  local directory="$1" actual
  [[ -d "$directory" && ! -L "$directory" \
    && -f "$directory/.manifest.sha256" && ! -L "$directory/.manifest.sha256" ]] || return 1
  actual="$(relay_code_manifest "$directory")" || return
  # Compare the stored bytes directly. Bash command substitution would
  # discard a damaged manifest's trailing LF differences and embedded NULs.
  cmp -s -- "$directory/.manifest.sha256" - <<< "$actual"
}

relay_code_capture() (
  local source="$1" backup="$2" module
  umask 077
  mkdir -- "$backup" || return
  relay_code_manifest "$source" > "$backup/.manifest.sha256" || return
  for module in "$source"/*.js; do
    [[ -e "$module" || -L "$module" ]] || continue
    cp -a -- "$module" "$backup/" || return
  done
  relay_code_verify "$backup"
)

relay_code_stage() {
  local backup="$1" live="$2" stage
  stage="$(mktemp -d "$live/.relay-code-rollback.XXXXXXXX")" || return
  if ! cp -a -- "$backup" "$stage/incoming" \
    || ! relay_code_verify "$stage/incoming" \
    || ! mkdir -- "$stage/previous"; then
    rm -rf -- "$stage"
    echo 'relay_code_backup_invalid: current code was preserved' >&2
    return 1
  fi
  printf '%s\n' "$stage"
}

relay_code_replace() {
  local stage="$1" live="$2" module failed=0 recovery_failed=0
  local -a installed=()
  for module in "$live"/*.js; do
    [[ -e "$module" || -L "$module" ]] || continue
    if [[ ! -f "$module" && ! -L "$module" ]] \
      || ! mv -- "$module" "$stage/previous/"; then
      failed=1
      break
    fi
  done
  if (( ! failed )); then
    for module in "$stage/incoming"/*.js; do
      [[ -f "$module" ]] || continue
      if ! mv -- "$module" "$live/"; then
        failed=1
        break
      fi
      installed+=("${module##*/}")
    done
  fi
  if (( ! failed )); then
    if ! rm -rf -- "$stage"; then
      printf 'relay_code_cleanup_pending: remove retained staging after rollback at %s\n' "$stage" >&2
    fi
    return 0
  fi
  for module in "${installed[@]}"; do
    mv -- "$live/$module" "$stage/incoming/" || recovery_failed=1
  done
  for module in "$stage/previous"/*.js; do
    [[ -e "$module" || -L "$module" ]] || continue
    if [[ -e "$live/${module##*/}" || -L "$live/${module##*/}" ]]; then
      recovery_failed=1
      continue
    fi
    mv -- "$module" "$live/" || recovery_failed=1
  done
  if (( recovery_failed )); then
    printf 'relay_code_recovery_incomplete: preserved recovery files at %s\n' "$stage" >&2
  else
    if ! rm -rf -- "$stage"; then
      printf 'relay_code_cleanup_pending: remove retained staging at %s\n' "$stage" >&2
    fi
    echo 'relay_code_replace_failed: previous code restored' >&2
  fi
  return 1
}

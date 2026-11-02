#!/usr/bin/env bash
# Restore the indexed public submodule on a disposable Actions workspace.
set -euo pipefail
history=false
case "${1:-}" in
  '') test "$#" -eq 0 ;;
  --history) test "$#" -eq 1; history=true ;;
  *) echo 'usage: ci-prepare-codewhale.sh [--history]' >&2; exit 2 ;;
esac

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
workspace="$(cd "${GITHUB_WORKSPACE:?GITHUB_WORKSPACE is required}" && pwd -P)"
if [[ "${GITHUB_ACTIONS:-}" != true || "$root" != "$workspace" ]]; then
  echo 'CodeWhale preparation requires its own GitHub Actions workspace' >&2
  exit 1
fi
for name in GIT_DIR GIT_COMMON_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_SHALLOW_FILE GIT_CONFIG; do
  if [[ -n "${!name:-}" ]]; then
    echo 'Repository path overrides are not supported by CI preparation' >&2
    exit 1
  fi
done

owned_location() {
  local location="$1" parent="$1" owner="${2:-$root}" resolved
  if [[ -L "$location" && ! -d "$location" ]]; then
    echo 'Mutable Git metadata must not be a redirected file' >&2
    return 1
  fi
  while [[ ! -d "$parent" ]]; do parent="$(dirname "$parent")"; done
  resolved="$(cd "$parent" && pwd -P)"
  case "$resolved" in
    "$owner"|"$owner"/*) ;;
    *) echo 'Mutable Git metadata is outside its owning repository' >&2; return 1 ;;
  esac
}

owned_metadata() {
  local repository="$1" expected="$2" metadata entry option
  local -a git_command=(git -C "$repository")
  if [[ "${3:-}" == cached-module ]]; then
    git_command=(git --git-dir="$repository" --work-tree="$root")
  fi
  # Worktree identity does not prove ownership of HEAD, index or config.
  # Immutable object alternates remain permitted; their files are only read.
  for option in --git-dir --git-common-dir; do
    metadata="$("${git_command[@]}" rev-parse --path-format=absolute "$option")"
    owned_location "$metadata"
    metadata="$(cd "$metadata" && pwd -P)"
    if [[ "$metadata" != "$expected" ]]; then
      echo 'Mutable Git metadata belongs to a different repository' >&2
      return 1
    fi
    for entry in index config; do
      owned_location "$("${git_command[@]}" rev-parse --path-format=absolute --git-path "$entry")" "$metadata"
    done
  done
}

owned_checkout_metadata() {
  local expected="$root/.git/modules/CodeWhale"
  if [[ -d "$root/CodeWhale/.git" && ! -L "$root/CodeWhale/.git" ]]; then
    expected="$root/CodeWhale/.git"
  fi
  owned_metadata "$root/CodeWhale" "$expected"
}
if [[ -L "$root/CodeWhale" ]]; then
  echo 'CodeWhale must not be a redirected checkout' >&2
  exit 1
fi
if [[ "$(cd "$(git -C "$root" rev-parse --show-toplevel)" && pwd -P)" != "$root" ]]; then
  echo 'Actions workspace is not the parent repository root' >&2
  exit 1
fi
owned_metadata "$root" "$root/.git"
if [[ -e "$root/CodeWhale/.git" &&
      "$(cd "$(git -C "$root/CodeWhale" rev-parse --show-toplevel)" && pwd -P)" != "$root/CodeWhale" ]]; then
  echo 'Existing CodeWhale working tree is redirected' >&2
  exit 1
fi
if [[ -e "$root/CodeWhale/.git" ]]; then
  owned_checkout_metadata
fi
# A cached module may survive removal of its working tree. Check its owner
# before submodule update reuses that metadata, even without a child gitfile.
module_metadata="$root/.git/modules/CodeWhale"
if [[ -e "$module_metadata" || -L "$module_metadata" ]]; then
  owned_metadata "$module_metadata" "$module_metadata" cached-module
  if module_worktree="$(git --git-dir="$module_metadata" --work-tree="$root" config --path --get core.worktree)"; then
    case "$module_worktree" in
      /*|[A-Za-z]:/*) module_parent="$(cd "$(dirname "$module_worktree")" && pwd -P)" ;;
      *) module_parent="$(cd "$module_metadata/$(dirname "$module_worktree")" && pwd -P)" ;;
    esac
    if [[ "$module_parent/${module_worktree##*/}" != "$root/CodeWhale" ]]; then
      echo 'Cached CodeWhale metadata belongs to a different working tree' >&2
      exit 1
    fi
  else
    config_status=$?
    # A discovered child gitfile already proved this exact metadata owner.
    # Without that checkout, an absent optional key cannot identify its owner.
    if [[ "$config_status" != 1 || ! -f "$root/CodeWhale/.git" ]]; then
      echo 'Cached CodeWhale working tree identity cannot be established' >&2
      exit 1
    fi
  fi
fi
if [[ "$(git -C "$root" config -f .gitmodules --get submodule.CodeWhale.path)" != CodeWhale ||
      "$(git -C "$root" config -f .gitmodules --get submodule.CodeWhale.url)" != https://github.com/Pinvou/CodeWhale.git ]]; then
  echo 'CodeWhale public submodule configuration differs from the repository contract' >&2
  exit 1
fi
entry="$(git -C "$root" ls-files --stage -- CodeWhale)"
if [[ ! "$entry" =~ ^160000\ ([0-9a-f]{40})\ 0$'\t'CodeWhale$ ]]; then
  echo 'CodeWhale requires one nonconflicted indexed gitlink' >&2
  exit 1
fi
gitlink="${BASH_REMATCH[1]}"
git -C "$root" submodule sync -- CodeWhale
git -C "$root" submodule update --init --recursive --checkout --force -- CodeWhale
checkout="$(cd "$root/CodeWhale" && pwd -P)"
if [[ "$checkout" != "$root/CodeWhale" ]]; then
  echo 'CodeWhale checkout escaped the Actions workspace' >&2
  exit 1
fi
owned_checkout_metadata
if [[ "$history" == true && "$(git -C "$checkout" rev-parse --is-shallow-repository)" == true ]]; then
  git -C "$checkout" fetch --no-tags --unshallow origin "$gitlink"
fi
# The parent checkout action does not clean an existing submodule's files.
git -C "$checkout" clean -ffdx
test "$(git -C "$checkout" rev-parse HEAD)" = "$gitlink"
shallow="$(git -C "$checkout" rev-parse --is-shallow-repository)"
if [[ "$history" == true ]]; then
  test "$shallow" = false
fi
test -z "$(git -C "$checkout" status --porcelain --untracked-files=all)"
printf 'CODEWHALE_CI_READY head=%s shallow=%s clean=true\n' "$gitlink" "$shallow"

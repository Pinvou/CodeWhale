#!/usr/bin/env bash
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PINVOU_CODEWHALE_PATH="CodeWhale"
PINVOU_CODEWHALE_URL="https://github.com/Pinvou/CodeWhale.git"
PINVOU_CODEWHALE_BRANCH="pinvou3-clean"
PINVOU_CODEWHALE_TAG="pinvou-v0.9.12-r3"
# 过渡期豁免（fork-policy.md 第 0 节）：两次 rN 收口之间，父仓 gitlink 可沿受保护
# 维护分支领先不可变 tag 前进。TRANSITION_BASELINE 非空即启用过渡期断言：
# 默认发布检查要求 gitlink 等于公开维护分支头；CI 显式登记快照模式允许
# 维护分支前进，但必须证明固定 tag → 精确登记 pin → 公开维护头的祖先链。
# rN 收口时：把 TAG 更新为新 tag、删除 TRANSITION_BASELINE，恢复三方相等断言。
TRANSITION_BASELINE="61cb769be5b33abc64f64da4272f5b39a8b6c1fd"
# 2026-10-02 entered the transition window: the registered head advanced with
# CodeWhale#80 (the stable 1.99 toolchain compile fix) while the immutable r3
# tag stays pinned at its closure commit, so the three-way-equality assertion
# gives way to the transition assertions above.
MAX_ATTEMPTS=3
registered_snapshot=false

# Never let ambient credentials, URL rewrites or repository objects stand in
# for anonymously available public history. Remote commands use a fresh bare
# repository, including ls-remote (which otherwise reads the caller's config).
clean_git() {
  env -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG -u GIT_DIR -u GIT_WORK_TREE \
    -u GIT_COMMON_DIR -u GIT_INDEX_FILE -u GIT_ASKPASS -u SSH_ASKPASS -u GIT_SSL_NO_VERIFY \
    -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES \
    -u GIT_SHALLOW_FILE -u GIT_NAMESPACE -u GIT_REPLACE_REF_BASE \
    -u GIT_TEMPLATE_DIR GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
    GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_COUNT=0 GIT_TERMINAL_PROMPT=0 \
    git -c credential.helper= -c core.askPass= -c http.extraHeader= "$@"
}

if [[ $# -eq 1 && "$1" == "--registered-snapshot" ]]; then
  registered_snapshot=true
elif [[ $# -ne 0 ]]; then
  echo "unknown argument: $1" >&2
  exit 2
fi

actual_path="$(clean_git -C "$REPO" config -f .gitmodules --get submodule.CodeWhale.path)"
actual_url="$(clean_git -C "$REPO" config -f .gitmodules --get submodule.CodeWhale.url)"

if [[ "$actual_path" != "$PINVOU_CODEWHALE_PATH" ]]; then
  echo "错误：CodeWhale submodule path 应为 ${PINVOU_CODEWHALE_PATH}，实际为 $actual_path" >&2
  exit 1
fi

if [[ "$actual_url" != "$PINVOU_CODEWHALE_URL" ]]; then
  echo "错误：CodeWhale submodule 必须使用公开 URL $PINVOU_CODEWHALE_URL" >&2
  exit 1
fi

if clean_git -C "$REPO" config -f .gitmodules --get submodule.CodeWhale.branch >/dev/null; then
  echo "错误：.gitmodules 不得配置浮动的 submodule.CodeWhale.branch" >&2
  exit 1
fi

gitlink="$(
  clean_git -C "$REPO" ls-files --stage -- "$PINVOU_CODEWHALE_PATH" |
    awk '$1 == "160000" { print $2 }'
)"

if [[ ! "$gitlink" =~ ^[0-9a-f]{40}$ ]]; then
  echo "错误：无法从索引读取唯一的 CodeWhale gitlink commit" >&2
  exit 1
fi

scratch_root="$(cd "${TMPDIR:-/tmp}" && pwd -P)"
temporary_repository="$(mktemp -d "$scratch_root/pinvou-codewhale-check.XXXXXX")"
temporary_repository="$(cd "$temporary_repository" && pwd -P)"
if [[ "$temporary_repository" != "$scratch_root"/pinvou-codewhale-check.* ]]; then
  echo "错误：公开提交校验临时目录越界" >&2
  exit 1
fi
trap 'rm -rf -- "$temporary_repository"' EXIT
clean_git init --bare --template= "$temporary_repository" >/dev/null 2>&1
public_git() { clean_git -C "$temporary_repository" "$@"; }

remote_refs=""
for attempt in $(seq 1 "$MAX_ATTEMPTS"); do
  if remote_refs="$(
    public_git ls-remote "$PINVOU_CODEWHALE_URL" \
      "refs/heads/${PINVOU_CODEWHALE_BRANCH}" \
      "refs/tags/${PINVOU_CODEWHALE_TAG}" \
      "refs/tags/${PINVOU_CODEWHALE_TAG}^{}" \
      2>/dev/null
  )"; then
    break
  fi
  if [[ "$attempt" -eq "$MAX_ATTEMPTS" ]]; then
    echo "错误：${MAX_ATTEMPTS} 次尝试后仍无法读取公开 CodeWhale refs" >&2
    exit 1
  fi
  sleep "$attempt"
done

branch_target="$(
  printf '%s\n' "$remote_refs" |
    awk -v ref="refs/heads/${PINVOU_CODEWHALE_BRANCH}" '$2 == ref { print $1 }'
)"
tag_direct="$(
  printf '%s\n' "$remote_refs" |
    awk -v ref="refs/tags/${PINVOU_CODEWHALE_TAG}" '$2 == ref { print $1 }'
)"
tag_peeled="$(
  printf '%s\n' "$remote_refs" |
    awk -v ref="refs/tags/${PINVOU_CODEWHALE_TAG}^{}" '$2 == ref { print $1 }'
)"
tag_target="${tag_peeled:-$tag_direct}"

if [[ "$branch_target" != "$gitlink" && "$registered_snapshot" != true ]]; then
  echo "错误：${PINVOU_CODEWHALE_BRANCH} 为 ${branch_target:-<不存在>}，父仓 gitlink 为 $gitlink" >&2
  exit 1
fi

if [[ -n "$TRANSITION_BASELINE" ]]; then
  if [[ "$tag_target" != "$TRANSITION_BASELINE" ]]; then
    echo "错误：${PINVOU_CODEWHALE_TAG} 解引用为 ${tag_target:-<不存在>}，过渡期内应钉在收口 ${TRANSITION_BASELINE}；若这是新切的 rN tag，请把 TAG 常量更新为新 tag 名并清除 TRANSITION_BASELINE，不得移动已发布的不可变 tag" >&2
    exit 1
  fi
  if [[ "$tag_target" == "$gitlink" ]]; then
    echo "错误：gitlink 已追平 ${PINVOU_CODEWHALE_TAG} 收口，请切除新 tag 并清除 TRANSITION_BASELINE" >&2
    exit 1
  fi
  if [[ "$registered_snapshot" == true ]]; then
    registered_pin="$(sed -n 's/^EXPECTED_HEAD="\([0-9a-f]\{40\}\)"$/\1/p' "$REPO/scripts/fork-guard.sh")"
    if [[ ! "$registered_pin" =~ ^[0-9a-f]{40}$ || "$registered_pin" != "$gitlink" || ! "$branch_target" =~ ^[0-9a-f]{40}$ ]]; then
      echo "错误：冻结审核 gitlink 必须精确匹配登记 pin，且公开维护分支必须存在" >&2
      exit 1
    fi
    fetched=false
    for attempt in $(seq 1 "$MAX_ATTEMPTS"); do
      if public_git fetch --quiet --filter=tree:0 --no-tags "$PINVOU_CODEWHALE_URL" \
          "+refs/heads/${PINVOU_CODEWHALE_BRANCH}:refs/heads/maintenance" \
          "+refs/tags/${PINVOU_CODEWHALE_TAG}:refs/tags/baseline" "$gitlink"; then
        fetched=true
        break
      fi
      sleep "$attempt"
    done
    if [[ "$fetched" != true ]] || \
        [[ "$(public_git rev-parse 'refs/tags/baseline^{commit}' 2>/dev/null)" != "$TRANSITION_BASELINE" ]] || \
        [[ "$(public_git cat-file -t "$gitlink" 2>/dev/null)" != commit ]] || \
        ! public_git merge-base --is-ancestor "$TRANSITION_BASELINE" "$gitlink" || \
        ! public_git merge-base --is-ancestor "$gitlink" refs/heads/maintenance; then
      echo "错误：冻结审核 pin 无法证明公开可达、固定标签历史及维护分支包含关系" >&2
      exit 1
    fi
    echo "公开 CodeWhale 冻结审核校验通过：登记 gitlink ${gitlink} 位于公开维护历史；不可变标签仍钉在 ${tag_target}"
    exit 0
  fi
  echo "公开 CodeWhale 基线过渡期校验通过：${PINVOU_CODEWHALE_BRANCH} = gitlink = ${gitlink}；${PINVOU_CODEWHALE_TAG} 钉在收口 ${tag_target}"
  exit 0
fi

if [[ "$branch_target" != "$gitlink" ]]; then
  echo "错误：收口校验要求公开维护分支与父仓 gitlink 相等" >&2
  exit 1
fi

if [[ "$tag_target" != "$gitlink" ]]; then
  echo "错误：${PINVOU_CODEWHALE_TAG} 解引用为 ${tag_target:-<不存在>}，父仓 gitlink 为 $gitlink" >&2
  exit 1
fi

if [[ "$branch_target" != "$tag_target" ]]; then
  echo "错误：公开维护分支与不可变标签未指向同一 commit" >&2
  exit 1
fi

echo "公开 CodeWhale 基线校验通过：${PINVOU_CODEWHALE_BRANCH} = ${PINVOU_CODEWHALE_TAG} = $gitlink"

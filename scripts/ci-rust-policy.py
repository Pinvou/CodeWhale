#!/usr/bin/env python3
"""Resolve trusted Community backflow CI routing from the actual base/head diff."""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request
from fnmatch import fnmatchcase
from pathlib import Path
from typing import Any, Callable, NamedTuple


BACKFLOW_BRANCH = re.compile(r"^backflow/community-(\d+)$")
BACKFLOW_MARKER = re.compile(
    r"<!--\s*community-backflow:Pinvou/pinvou-agent#(\d+)\s*-->"
)
FULL_RUST_LABEL = "ci:full-rust"
ROUTING_MODES = {"ordinary", "full", "release", "portable_rust", "windows_full", "non_rust"}
SEMVER = re.compile(
    r"^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)"
    r"(?:-((?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)"
    r"(?:\.(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*))?"
    r"(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$"
)

RELEASE_VERSION_TARGETS = {
    "VERSION": ("plain", ()),
    "pinvou3-app/src-tauri/tauri.conf.json": ("json", ("version",)),
    "pinvou3-app/src-tauri/Cargo.toml": ("cargo", ("pinvou3-tauri",)),
    "pinvou-knowledge/Cargo.toml": ("cargo", ("pinvou-knowledge",)),
    "pinvou-knowledge/Cargo.lock": ("cargo-lock", ("pinvou-knowledge",)),
    "pinvou3-app/src-tauri/Cargo.lock": (
        "cargo-lock",
        ("pinvou-knowledge", "pinvou3-tauri"),
    ),
    "pinvou-cli/Cargo.lock": (
        "cargo-lock",
        ("pinvou-knowledge", "pinvou3-tauri"),
    ),
    "pinvou3-app/package.json": ("json", ("version",)),
    "pinvou3-app/package-lock.json": (
        "json",
        ("version", "packages", "", "version"),
    ),
}

WINDOWS_HIGH_RISK_PATHS = (
    "**/Cargo.toml",
    "**/Cargo.lock",
    "**/deny.toml",
    "**/build.rs",
    ".cargo/**",
    ".gitmodules",
    "CodeWhale",
    "**/rust-toolchain.toml",
    "pinvou3-app/src-tauri/src/platform/**",
    "pinvou3-app/src-tauri/src/**/platform/**",
    "pinvou3-app/src-tauri/src/app/commands/**",
    "pinvou3-app/src-tauri/src/features/codex_acp/**",
    "pinvou3-app/src-tauri/src/features/runtime_bundle/**",
    "pinvou3-app/src-tauri/src/features/updater/**",
    "pinvou3-app/src-tauri/capabilities/**",
    "pinvou3-app/src-tauri/config/platforms/windows/**",
    "pinvou3-app/src-tauri/packaging/windows/**",
    "pinvou3-app/scripts/tauri/**windows**",
    "private-runtimes/windows",
    # A backflow must never be able to lighten its own gate: edits to the CI
    # workflow, to this policy, or to non-version bundle config stay full.
    "**/tauri.conf.json",
    ".github/workflows/**",
    "scripts/ci-rust-policy.py",
)
WINDOWS_HIGH_RISK_PATH_PART = re.compile(
    r"(?:^|[/_.-])(?:windows|win32|filesystem|paths?|process|installer?|"
    r"permissions?|privilege|elevat(?:e|ed|ion)|native)(?:$|[/_.-])",
    re.IGNORECASE,
)
WINDOWS_HIGH_RISK_RUST = re.compile(
    r"(?:target_os\s*=\s*[\"']windows[\"']|cfg(?:_attr)?!?\s*\([^\n]*windows|"
    r"std::os::windows|windows_sys|windows::|winapi|extern\s+[\"']system[\"']|"
    r"CommandExt|creation_flags|CREATE_NO_WINDOW|\.arg\s*\(\s*[\"']/C[\"']|"
    r"#\s*\[link\s*\(|std::process|tokio::process|Command(?:::\w+)?|"
    r"std::fs|tokio::fs|std::path|PathBuf|set_permissions|canonicalize|"
    r"symlink|remove_file|rename\s*\()"
)


class FileChange(NamedTuple):
    path: str
    status: str
    before: bytes | None = None
    after: bytes | None = None
    patch: str = ""


class RoutingDecision(NamedTuple):
    trusted: bool
    mode: str
    reason: str

QUEUE_QUERY = """
query($owner: String!, $name: String!, $branch: String!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    mergeQueue(branch: $branch) {
      entries(first: 100, after: $cursor) {
        nodes {
          state
          baseCommit { oid }
          headCommit { oid }
          pullRequest {
            number
            baseRefName
            baseRepository { nameWithOwner }
            headRefName
            headRepository { nameWithOwner }
            body
            labels(first: 100) {
              totalCount
              nodes { name }
              pageInfo { hasNextPage endCursor }
            }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}
"""


def _labels(pr: dict[str, Any]) -> set[str]:
    labels = pr.get("labels") or []
    if isinstance(labels, dict):
        labels = labels.get("nodes") or []
    return {
        label.get("name", "") if isinstance(label, dict) else str(label)
        for label in labels
    }


def _head_repository(pr: dict[str, Any]) -> str:
    if "headRepository" in pr:
        return (pr.get("headRepository") or {}).get("nameWithOwner", "")
    return ((pr.get("head") or {}).get("repo") or {}).get("full_name", "")


def _head_ref(pr: dict[str, Any]) -> str:
    return pr.get("headRefName") or (pr.get("head") or {}).get("ref", "")


def _base_repository(pr: dict[str, Any]) -> str:
    if "baseRepository" in pr:
        return (pr.get("baseRepository") or {}).get("nameWithOwner", "")
    return ((pr.get("base") or {}).get("repo") or {}).get("full_name", "")


def _base_ref(pr: dict[str, Any]) -> str:
    return pr.get("baseRefName") or (pr.get("base") or {}).get("ref", "")


def trusted_backflow(pr: dict[str, Any], repository: str) -> tuple[bool, str]:
    if FULL_RUST_LABEL in _labels(pr):
        return False, f"{FULL_RUST_LABEL} requests the full Rust test"
    if _head_repository(pr) != repository:
        return False, "pull request is not from the Official repository"
    if _base_repository(pr) != repository:
        return False, "pull request base is not the Official repository"
    if _base_ref(pr) != "main":
        return False, "pull request base is not main"

    branch_match = BACKFLOW_BRANCH.fullmatch(_head_ref(pr))
    if not branch_match:
        return False, "head branch is not a Community backflow branch"

    markers = BACKFLOW_MARKER.findall(pr.get("body") or "")
    if len(markers) != 1 or markers[0] != branch_match.group(1):
        return False, "source marker does not uniquely match the backflow branch"

    return True, f"trusted Community backflow #{branch_match.group(1)}"


def _validate_queue_pull_request(pr: Any) -> dict[str, Any]:
    """Reject incomplete GraphQL PR snapshots before evaluating trust.

    A malformed or truncated queue entry is not evidence that a PR is an
    ordinary, non-trusted contribution.  In particular, silently treating a
    missing ``pullRequest`` or a partial labels connection as ordinary could
    skip the Windows policy when the omitted data contained ``ci:full-rust``.
    """

    if not isinstance(pr, dict):
        raise RuntimeError("merge queue entry pull request is missing")

    number = pr.get("number")
    if not isinstance(number, int) or isinstance(number, bool) or number <= 0:
        raise RuntimeError("merge queue pull request number is missing")

    for field in ("baseRefName", "headRefName"):
        if not isinstance(pr.get(field), str) or not pr[field]:
            raise RuntimeError(f"merge queue pull request {field} is missing")
    if "body" not in pr or not isinstance(pr["body"], str):
        raise RuntimeError("merge queue pull request body is missing")

    for field in ("baseRepository", "headRepository"):
        repository = pr.get(field)
        if not isinstance(repository, dict) or not isinstance(
            repository.get("nameWithOwner"), str
        ) or not repository["nameWithOwner"]:
            raise RuntimeError(f"merge queue pull request {field} is missing")

    labels = pr.get("labels")
    if not isinstance(labels, dict):
        raise RuntimeError("merge queue pull request labels are missing")
    nodes = labels.get("nodes")
    total_count = labels.get("totalCount")
    page_info = labels.get("pageInfo")
    if (
        not isinstance(nodes, list)
        or not isinstance(total_count, int)
        or isinstance(total_count, bool)
        or total_count < 0
        or not isinstance(page_info, dict)
        or not isinstance(page_info.get("hasNextPage"), bool)
        or "endCursor" not in page_info
        or (
            page_info["endCursor"] is not None
            and not isinstance(page_info["endCursor"], str)
        )
    ):
        raise RuntimeError("merge queue pull request labels connection is incomplete")
    if page_info["hasNextPage"] or total_count != len(nodes):
        raise RuntimeError("merge queue pull request labels were truncated")
    if any(
        not isinstance(label, dict)
        or not isinstance(label.get("name"), str)
        or not label["name"]
        for label in nodes
    ):
        raise RuntimeError("merge queue pull request label data is incomplete")
    return pr


def _parse_queue_page(result: Any) -> tuple[list[dict[str, Any]], bool, str | None]:
    """Validate one GraphQL queue page without coercing malformed values.

    GraphQL normally supplies these non-null connection fields, but API or
    schema failures can return partial data.  Every layer is policy input, so
    ``or {}``/``or []`` fallbacks would incorrectly turn missing evidence into
    a trusted empty value.
    """

    if not isinstance(result, dict):
        raise RuntimeError("merge queue GraphQL result is not an object")
    errors = result.get("errors")
    if errors is not None:
        if not isinstance(errors, list) or errors:
            raise RuntimeError("merge queue GraphQL result contains errors")

    value: Any = result
    for field in ("data", "repository", "mergeQueue", "entries"):
        if field not in value or not isinstance(value[field], dict):
            raise RuntimeError(f"merge queue GraphQL {field} object is missing")
        value = value[field]
    entries = value

    if "nodes" not in entries or not isinstance(entries["nodes"], list):
        raise RuntimeError("merge queue GraphQL nodes list is missing")
    if "pageInfo" not in entries or not isinstance(entries["pageInfo"], dict):
        raise RuntimeError("merge queue GraphQL pageInfo object is missing")
    page_info = entries["pageInfo"]
    if (
        "hasNextPage" not in page_info
        or not isinstance(page_info["hasNextPage"], bool)
        or "endCursor" not in page_info
        or (
            page_info["endCursor"] is not None
            and not isinstance(page_info["endCursor"], str)
        )
    ):
        raise RuntimeError("merge queue GraphQL pageInfo is incomplete")
    if page_info["hasNextPage"] and not page_info["endCursor"]:
        raise RuntimeError("merge queue pagination cursor is missing")

    nodes: list[dict[str, Any]] = []
    for raw_entry in entries["nodes"]:
        if not isinstance(raw_entry, dict):
            raise RuntimeError("merge queue entry is not an object")
        state = raw_entry.get("state")
        if not isinstance(state, str) or state not in {
            "QUEUED",
            "AWAITING_CHECKS",
            "MERGEABLE",
            "UNMERGEABLE",
            "LOCKED",
        }:
            raise RuntimeError("merge queue entry state is missing or unknown")
        _validate_queue_pull_request(raw_entry.get("pullRequest"))
        # GitHub exposes not-yet-built waiting entries with nullable commits.
        # They cannot contribute a link to this event's commit chain. Exclude
        # only the explicit QUEUED + two null fields shape, never missing fields,
        # partial commits, or another state. The event head must still trace all
        # the way to its base through the remaining fully validated entries.
        if (
            state == "QUEUED"
            and "baseCommit" in raw_entry
            and raw_entry["baseCommit"] is None
            and "headCommit" in raw_entry
            and raw_entry["headCommit"] is None
        ):
            continue
        for commit_field in ("baseCommit", "headCommit"):
            commit = raw_entry.get(commit_field)
            if (
                not isinstance(commit, dict)
                or not isinstance(commit.get("oid"), str)
                or not commit["oid"]
            ):
                raise RuntimeError(
                    f"merge queue entry {commit_field} is missing or incomplete"
                )
        nodes.append(raw_entry)

    return nodes, page_info["hasNextPage"], page_info["endCursor"]


def _git_bytes(arguments: list[str], repository_root: Path) -> bytes:
    result = subprocess.run(
        ["git", *arguments],
        cwd=repository_root,
        check=False,
        capture_output=True,
    )
    if result.returncode != 0:
        message = result.stderr.decode("utf-8", errors="replace").strip()
        raise RuntimeError(f"git {' '.join(arguments[:2])} failed: {message}")
    return result.stdout


def inspect_git_diff(
    base_sha: str, head_sha: str, repository_root: Path | None = None
) -> list[FileChange]:
    """Read the exact base/head tree diff without trusting PR titles or labels."""

    root = repository_root or Path.cwd()
    for name, sha in (("base", base_sha), ("head", head_sha)):
        if not re.fullmatch(r"[0-9a-fA-F]{40,64}", sha or ""):
            raise ValueError(f"{name} SHA is invalid")
        _git_bytes(["cat-file", "-e", f"{sha}^{{commit}}"], root)

    fields = _git_bytes(
        ["diff", "--name-status", "-z", "--no-renames", base_sha, head_sha], root
    ).split(b"\0")
    if fields and fields[-1] == b"":
        fields.pop()
    if len(fields) % 2:
        raise RuntimeError("git diff returned an incomplete name/status record")

    changes: list[FileChange] = []
    for index in range(0, len(fields), 2):
        status = fields[index].decode("ascii", errors="strict")
        path = fields[index + 1].decode("utf-8", errors="strict").replace("\\", "/")
        if status not in {"A", "D", "M", "T"} or not path:
            raise RuntimeError(f"unsupported diff record: {status} {path!r}")

        before = None
        after = None
        if path in RELEASE_VERSION_TARGETS or path.endswith(".rs"):
            if status != "A":
                before = _git_bytes(["show", f"{base_sha}:{path}"], root)
            if status != "D":
                after = _git_bytes(["show", f"{head_sha}:{path}"], root)

        patch = ""
        if path.endswith(".rs"):
            patch = _git_bytes(
                [
                    "diff",
                    "--no-ext-diff",
                    "--unified=0",
                    base_sha,
                    head_sha,
                    "--",
                    path,
                ],
                root,
            ).decode("utf-8", errors="strict")
        changes.append(FileChange(path, status, before, after, patch))
    return changes


def _replace_json_version(
    content: bytes, expected_version: str, fields: tuple[str, ...]
) -> Any:
    data = json.loads(content.decode("utf-8"))
    targets = (fields,) if fields != ("version", "packages", "", "version") else (
        ("version",),
        ("packages", "", "version"),
    )
    for target in targets:
        cursor = data
        for key in target[:-1]:
            cursor = cursor[key]
        if cursor[target[-1]] != expected_version:
            raise ValueError(f"JSON version field {'.'.join(target)} is inconsistent")
        cursor[target[-1]] = "__PINVOU_VERSION__"
    return data


def _replace_cargo_package_version(content: bytes, expected_version: str) -> str:
    text = content.decode("utf-8")
    lines = text.splitlines(keepends=True)
    in_package = False
    replaced = 0
    for index, line in enumerate(lines):
        stripped = line.strip()
        if stripped.startswith("["):
            in_package = stripped == "[package]"
            continue
        match = re.match(r'^(\s*version\s*=\s*")([^"]+)(".*)$', line)
        if in_package and match:
            if match.group(2) != expected_version:
                raise ValueError("Cargo [package] version is inconsistent")
            lines[index] = (
                f"{line[:match.start(2)]}__PINVOU_VERSION__{line[match.end(2):]}"
            )
            replaced += 1
            in_package = False
    if replaced != 1:
        raise ValueError(f"expected one Cargo [package] version, found {replaced}")
    return "".join(lines)


def _replace_lock_versions(
    content: bytes, expected_version: str, package_names: tuple[str, ...]
) -> str:
    lines = content.decode("utf-8").splitlines(keepends=True)
    found: dict[str, int] = {name: 0 for name in package_names}
    start = 0
    while start < len(lines):
        if lines[start].strip() != "[[package]]":
            start += 1
            continue
        end = start + 1
        while end < len(lines) and lines[end].strip() != "[[package]]":
            end += 1
        package_name = None
        version_index = None
        for index in range(start + 1, end):
            name_match = re.match(r'^name\s*=\s*"([^"]+)"', lines[index])
            version_match = re.match(r'^version\s*=\s*"([^"]+)"', lines[index])
            if name_match:
                package_name = name_match.group(1)
            if version_match and version_index is None:
                version_index = index
        if package_name in found:
            if version_index is None:
                raise ValueError(f"{package_name} has no version in Cargo.lock")
            match = re.match(r'^(version\s*=\s*")([^"]+)(".*)$', lines[version_index])
            if not match or match.group(2) != expected_version:
                raise ValueError(f"{package_name} version is inconsistent in Cargo.lock")
            lines[version_index] = (
                f"{lines[version_index][:match.start(2)]}__PINVOU_VERSION__"
                f"{lines[version_index][match.end(2):]}"
            )
            found[package_name] += 1
        start = end
    invalid = {name: count for name, count in found.items() if count != 1}
    if invalid:
        raise ValueError(f"Cargo.lock package version occurrences are invalid: {invalid}")
    return "".join(lines)


def pure_release_change(changes: list[FileChange]) -> tuple[bool, str]:
    """Accept only the exact mechanical output managed by sync-version.mjs."""

    paths = {change.path for change in changes}
    expected_paths = set(RELEASE_VERSION_TARGETS)
    if paths != expected_paths:
        return False, "changed paths are not exactly the sync-version target set"
    if any(change.status != "M" or change.before is None or change.after is None for change in changes):
        return False, "version targets must be modified in place"

    by_path = {change.path: change for change in changes}
    base_version = by_path["VERSION"].before.decode("utf-8").strip()
    head_version = by_path["VERSION"].after.decode("utf-8").strip()
    if (
        not SEMVER.fullmatch(base_version)
        or not SEMVER.fullmatch(head_version)
        or base_version == head_version
    ):
        return False, "VERSION is not a changed SemVer value"
    if by_path["VERSION"].before.replace(
        base_version.encode("utf-8"), head_version.encode("utf-8"), 1
    ) != by_path["VERSION"].after:
        return False, "VERSION contains non-version content changes"

    try:
        for path, (kind, fields) in RELEASE_VERSION_TARGETS.items():
            if path == "VERSION":
                continue
            change = by_path[path]
            if kind == "json":
                before = _replace_json_version(change.before, base_version, fields)
                after = _replace_json_version(change.after, head_version, fields)
            elif kind == "cargo":
                before = _replace_cargo_package_version(change.before, base_version)
                after = _replace_cargo_package_version(change.after, head_version)
            elif kind == "cargo-lock":
                before = _replace_lock_versions(change.before, base_version, fields)
                after = _replace_lock_versions(change.after, head_version, fields)
            else:
                raise ValueError(f"unknown version target kind: {kind}")
            if before != after:
                return False, f"{path} contains changes beyond managed version fields"
    except (KeyError, TypeError, UnicodeDecodeError, ValueError, json.JSONDecodeError) as error:
        return False, f"version target content is invalid: {error}"
    return True, f"mechanical sync-version change {base_version} -> {head_version}"


def windows_high_risk(changes: list[FileChange]) -> tuple[bool, str]:
    for change in changes:
        path = change.path
        if any(fnmatchcase(path, pattern) for pattern in WINDOWS_HIGH_RISK_PATHS):
            return True, f"Windows high-risk path changed: {path}"
        if WINDOWS_HIGH_RISK_PATH_PART.search(path):
            return True, f"Windows/path/process/install/permission path changed: {path}"
        if path.endswith((".ps1", ".cmd", ".bat", ".wxs", ".nsi")):
            return True, f"Windows-native file changed: {path}"
        if path.endswith(".rs"):
            complete_source = b"\n".join(
                content for content in (change.before, change.after) if content is not None
            ).decode("utf-8", errors="strict")
            if WINDOWS_HIGH_RISK_RUST.search(complete_source):
                return True, f"Windows/path/process/native Rust context changed: {path}"
    return False, "no Windows high-risk path or changed Rust semantics"


def classify_trusted_diff(changes: list[FileChange]) -> tuple[str, str]:
    if not changes:
        raise ValueError("base/head diff is empty")
    release, release_reason = pure_release_change(changes)
    if release:
        return "release", release_reason
    high_risk, high_risk_reason = windows_high_risk(changes)
    if high_risk:
        return "windows_full", high_risk_reason
    if any(change.path.endswith(".rs") for change in changes):
        return "portable_rust", "trusted cross-platform Rust diff"
    return "non_rust", "trusted non-Rust diff"


def _graphql_page(
    *,
    token: str,
    endpoint: str,
    owner: str,
    name: str,
    branch: str,
    cursor: str | None,
) -> dict[str, Any]:
    payload = json.dumps(
        {
            "query": QUEUE_QUERY,
            "variables": {
                "owner": owner,
                "name": name,
                "branch": branch,
                "cursor": cursor,
            },
        }
    ).encode("utf-8")
    request = urllib.request.Request(
        endpoint,
        data=payload,
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
            "User-Agent": "pinvou-ci-rust-policy",
        },
    )
    with urllib.request.urlopen(request, timeout=15) as response:
        result = json.load(response)
    if not isinstance(result, dict):
        raise RuntimeError("GraphQL response is not an object")
    if result.get("errors"):
        raise RuntimeError(result["errors"][0].get("message", "GraphQL query failed"))
    return result


def resolve_merge_group(
    event: dict[str, Any],
    repository: str,
    token: str,
    fetch_page: Callable[..., dict[str, Any]] = _graphql_page,
    endpoint: str = "https://api.github.com/graphql",
) -> tuple[bool, str]:
    if not isinstance(event, dict):
        raise RuntimeError("workflow event is not an object")
    merge_group = event.get("merge_group") or {}
    if not isinstance(merge_group, dict):
        raise RuntimeError("merge group event is not an object")
    head_sha = merge_group.get("head_sha", "")
    base_ref = merge_group.get("base_ref", "")
    base_sha = merge_group.get("base_sha", "")
    if (
        not head_sha
        or not base_sha
        or base_ref != "refs/heads/main"
        or "/" not in repository
    ):
        raise RuntimeError("merge group identity is incomplete or not based on main")
    if not token:
        raise RuntimeError("GitHub token is unavailable")

    owner, name = repository.split("/", 1)
    branch = base_ref.removeprefix("refs/heads/")
    cursor = None
    seen_cursors: set[str] = set()
    queue_entries: list[dict[str, Any]] = []

    while True:
        result = fetch_page(
            token=token,
            endpoint=endpoint,
            owner=owner,
            name=name,
            branch=branch,
            cursor=cursor,
        )
        nodes, has_next_page, end_cursor = _parse_queue_page(result)
        queue_entries.extend(nodes)

        if not has_next_page:
            break
        if end_cursor in seen_cursors:
            raise RuntimeError("merge queue pagination cursor repeated")
        cursor = end_cursor
        seen_cursors.add(cursor)

    # Each MergeQueueEntry proves one link in the cumulative merge-group commit
    # chain: baseCommit -> headCommit and the PR that produced that link. Walking
    # backwards from the event head to its event base yields the complete group;
    # matching only the cumulative head would validate the final PR and silently
    # miss earlier PRs in a multi-entry group.
    by_head: dict[str, list[dict[str, Any]]] = {}
    for entry in queue_entries:
        oid = entry["headCommit"]["oid"]
        by_head.setdefault(oid, []).append(entry)

    matching_entries: list[dict[str, Any]] = []
    cursor_sha = head_sha
    visited: set[str] = set()
    while cursor_sha != base_sha:
        if cursor_sha in visited:
            raise RuntimeError("merge queue entry chain contains a cycle")
        visited.add(cursor_sha)
        candidates = by_head.get(cursor_sha) or []
        if len(candidates) != 1:
            raise RuntimeError(
                "merge group cannot be mapped to one complete queue entry chain"
            )
        entry = candidates[0]
        previous = entry["baseCommit"]["oid"]
        matching_entries.append(entry)
        cursor_sha = previous
        if len(matching_entries) > len(queue_entries):
            raise RuntimeError("merge queue entry chain exceeds the queue snapshot")

    if not matching_entries:
        raise RuntimeError("merge group contains no pull requests")

    for entry in matching_entries:
        pr = _validate_queue_pull_request(entry.get("pullRequest"))
        if FULL_RUST_LABEL in _labels(pr):
            raise RuntimeError(
                f"queue entry #{pr['number']} requests {FULL_RUST_LABEL}"
            )
        trusted, reason = trusted_backflow(pr, repository)
        if not trusted:
            return False, f"queue entry #{pr.get('number', '?')}: {reason}"

    numbers = ", ".join(
        f"#{(entry.get('pullRequest') or {}).get('number', '?')}"
        for entry in reversed(matching_entries)
    )
    return True, f"merge group contains only trusted backflows: {numbers}"


def resolve_event(
    event: dict[str, Any],
    repository: str,
    token: str = "",
    fetch_page: Callable[..., dict[str, Any]] = _graphql_page,
    endpoint: str = "https://api.github.com/graphql",
) -> tuple[bool, str]:
    if not isinstance(event, dict):
        raise RuntimeError("workflow event is not an object")
    event_name = os.environ.get("GITHUB_EVENT_NAME", "")
    if not event_name:
        event_name = "merge_group" if "merge_group" in event else "pull_request"
    if event_name == "pull_request":
        return trusted_backflow(event.get("pull_request") or {}, repository)
    if event_name == "merge_group":
        return resolve_merge_group(
            event, repository, token, fetch_page=fetch_page, endpoint=endpoint
        )
    return False, f"{event_name or 'unknown'} events require the full Rust policy"


def _event_diff_range(event: dict[str, Any], event_name: str) -> tuple[str, str]:
    if event_name == "pull_request":
        pull = event.get("pull_request") or {}
        return (pull.get("base") or {}).get("sha", ""), (pull.get("head") or {}).get(
            "sha", ""
        )
    if event_name == "merge_group":
        group = event.get("merge_group") or {}
        return group.get("base_sha", ""), group.get("head_sha", "")
    return "", ""


def resolve_routing_event(
    event: dict[str, Any],
    repository: str,
    token: str = "",
    fetch_page: Callable[..., dict[str, Any]] = _graphql_page,
    inspect_diff: Callable[[str, str], list[FileChange]] = inspect_git_diff,
    endpoint: str = "https://api.github.com/graphql",
) -> RoutingDecision:
    try:
        trusted, trust_reason = resolve_event(
            event,
            repository,
            token,
            fetch_page=fetch_page,
            endpoint=endpoint,
        )
    except (
        AttributeError,
        OSError,
        TypeError,
        ValueError,
        RuntimeError,
        urllib.error.URLError,
    ) as error:
        return RoutingDecision(False, "full", f"trust resolution failed closed: {error}")
    if not trusted:
        # A completely resolved but non-trusted PR keeps the pre-existing ordinary
        # path filters. "full" is reserved for policy/API/diff resolution failures
        # that must force Windows even when rust_full would otherwise be false.
        return RoutingDecision(False, "ordinary", trust_reason)

    event_name = os.environ.get("GITHUB_EVENT_NAME", "")
    if not event_name:
        event_name = "merge_group" if "merge_group" in event else "pull_request"
    base_sha, head_sha = _event_diff_range(event, event_name)
    try:
        changes = inspect_diff(base_sha, head_sha)
        mode, route_reason = classify_trusted_diff(changes)
    except (AttributeError, OSError, TypeError, UnicodeError, ValueError, RuntimeError) as error:
        return RoutingDecision(False, "full", f"diff routing failed closed: {error}")
    return RoutingDecision(True, mode, f"{trust_reason}; {route_reason}")


def _emit(decision: RoutingDecision) -> None:
    if decision.mode not in ROUTING_MODES:
        decision = RoutingDecision(
            False,
            "full",
            f"policy emitted unknown mode {decision.mode!r}; failed closed",
        )
    value = str(decision.trusted).lower()
    pure_release = str(decision.trusted and decision.mode == "release").lower()
    print(f"trusted_backflow={value}: {decision.reason}")
    print(f"backflow_mode={decision.mode}")
    output = os.environ.get("GITHUB_OUTPUT")
    if output:
        safe_reason = decision.reason.replace("\r", " ").replace("\n", " ")
        with Path(output).open("a", encoding="utf-8") as stream:
            stream.write(
                f"trusted_backflow={value}\n"
                f"backflow_mode={decision.mode}\n"
                f"pure_release_backflow={pure_release}\n"
                f"reason={safe_reason}\n"
            )


def main() -> int:
    try:
        event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text("utf-8"))
        decision = resolve_routing_event(
            event,
            os.environ.get("GITHUB_REPOSITORY", ""),
            os.environ.get("GITHUB_TOKEN", ""),
            endpoint=os.environ.get(
                "GITHUB_GRAPHQL_URL", "https://api.github.com/graphql"
            ),
        )
    except (
        AttributeError,
        KeyError,
        OSError,
        TypeError,
        ValueError,
        RuntimeError,
        urllib.error.URLError,
    ) as error:
        decision = RoutingDecision(
            False, "full", f"policy resolution failed closed: {error}"
        )
        print(f"::warning::{decision.reason}", file=sys.stderr)
    _emit(decision)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

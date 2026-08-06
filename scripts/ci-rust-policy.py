#!/usr/bin/env python3
"""Resolve whether a trusted Community backflow may skip the full Rust test."""

from __future__ import annotations

import json
import os
import re
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Callable


BACKFLOW_BRANCH = re.compile(r"^backflow/community-(\d+)$")
BACKFLOW_MARKER = re.compile(
    r"<!--\s*community-backflow:Pinvou/pinvou-agent#(\d+)\s*-->"
)
FULL_RUST_LABEL = "ci:full-rust"

QUEUE_QUERY = """
query($owner: String!, $name: String!, $branch: String!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    mergeQueue(branch: $branch) {
      entries(first: 100, after: $cursor) {
        nodes {
          headCommit { oid }
          pullRequest {
            number
            headRefName
            headRepository { nameWithOwner }
            body
            labels(first: 100) { nodes { name } }
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


def trusted_backflow(pr: dict[str, Any], repository: str) -> tuple[bool, str]:
    if FULL_RUST_LABEL in _labels(pr):
        return False, f"{FULL_RUST_LABEL} requests the full Rust test"
    if _head_repository(pr) != repository:
        return False, "pull request is not from the Official repository"

    branch_match = BACKFLOW_BRANCH.fullmatch(_head_ref(pr))
    if not branch_match:
        return False, "head branch is not a Community backflow branch"

    markers = BACKFLOW_MARKER.findall(pr.get("body") or "")
    if len(markers) != 1 or markers[0] != branch_match.group(1):
        return False, "source marker does not uniquely match the backflow branch"

    return True, f"trusted Community backflow #{branch_match.group(1)}"


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
    merge_group = event.get("merge_group") or {}
    head_sha = merge_group.get("head_sha", "")
    base_ref = merge_group.get("base_ref", "")
    if not head_sha or not base_ref or "/" not in repository:
        return False, "merge group identity is incomplete"
    if not token:
        return False, "GitHub token is unavailable"

    owner, name = repository.split("/", 1)
    branch = base_ref.removeprefix("refs/heads/")
    cursor = None
    matching_entries: list[dict[str, Any]] = []

    while True:
        result = fetch_page(
            token=token,
            endpoint=endpoint,
            owner=owner,
            name=name,
            branch=branch,
            cursor=cursor,
        )
        entries = (
            (((result.get("data") or {}).get("repository") or {}).get("mergeQueue") or {})
            .get("entries")
            or {}
        )
        for entry in entries.get("nodes") or []:
            if ((entry.get("headCommit") or {}).get("oid")) == head_sha:
                matching_entries.append(entry)

        page_info = entries.get("pageInfo") or {}
        if not page_info.get("hasNextPage"):
            break
        cursor = page_info.get("endCursor")
        if not cursor:
            return False, "merge queue pagination cursor is missing"

    if not matching_entries:
        return False, "merge group is not present in the current queue"

    for entry in matching_entries:
        pr = entry.get("pullRequest") or {}
        trusted, reason = trusted_backflow(pr, repository)
        if not trusted:
            return False, f"queue entry #{pr.get('number', '?')}: {reason}"

    numbers = ", ".join(
        f"#{(entry.get('pullRequest') or {}).get('number', '?')}"
        for entry in matching_entries
    )
    return True, f"merge group contains only trusted backflows: {numbers}"


def resolve_event(
    event: dict[str, Any],
    repository: str,
    token: str = "",
    fetch_page: Callable[..., dict[str, Any]] = _graphql_page,
    endpoint: str = "https://api.github.com/graphql",
) -> tuple[bool, str]:
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


def _emit(trusted: bool, reason: str) -> None:
    value = str(trusted).lower()
    print(f"trusted_backflow={value}: {reason}")
    output = os.environ.get("GITHUB_OUTPUT")
    if output:
        safe_reason = reason.replace("\r", " ").replace("\n", " ")
        with Path(output).open("a", encoding="utf-8") as stream:
            stream.write(f"trusted_backflow={value}\nreason={safe_reason}\n")


def main() -> int:
    try:
        event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text("utf-8"))
        trusted, reason = resolve_event(
            event,
            os.environ.get("GITHUB_REPOSITORY", ""),
            os.environ.get("GITHUB_TOKEN", ""),
            endpoint=os.environ.get(
                "GITHUB_GRAPHQL_URL", "https://api.github.com/graphql"
            ),
        )
    except (KeyError, OSError, ValueError, RuntimeError, urllib.error.URLError) as error:
        trusted, reason = False, f"policy resolution failed closed: {error}"
        print(f"::warning::{reason}", file=sys.stderr)
    _emit(trusted, reason)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

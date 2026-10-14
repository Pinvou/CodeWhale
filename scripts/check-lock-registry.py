#!/usr/bin/env python3
"""Lockfile registry-hygiene guard (PR #1518).

Two failure classes are pinned here:

1. `resolved` URLs pointing at registry.npmmirror.com — the mirror is slow
   and a cross-border single point; `npm ci` consumes lock URLs verbatim,
   so every mirror URL is a latency/flake dependency on every build. The
   two repo locks that are consumed by CI (team-relay-hub,
   codex-bridge-runtime) carried 134 such URLs in total before the #1518
   cleanup; this keeps them from creeping back in.

2. Missing `libc` fields on the @img/* platform packages in
   team-relay-hub/package-lock.json — npm 10's arborist does not serialize
   `libc` (pkgMetaKeys gained the key only in npm 11), so a single local
   `npm install` on node 22 silently strips the filtering fields; without
   them every Linux environment installs BOTH the glibc and the musl
   native binaries. The count is pinned so the strip turns red here
   instead of being discovered as install-bloat later.
"""

import json
import os
import sys

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXPECTED_TEAM_RELAY_HUB_LIBC_ENTRIES = 16


def iter_lock_files():
    for root, dirs, files in os.walk(REPO_ROOT):
        dirs[:] = [
            d for d in dirs if d not in ("node_modules", ".git", "target", "dist")
        ]
        for name in files:
            if name == "package-lock.json":
                yield os.path.join(root, name)


def main():
    failures = []
    lock_files = sorted(iter_lock_files())
    if not lock_files:
        failures.append("no package-lock.json found — guard ran against the wrong tree?")
    hub_libc_count = None
    for path in lock_files:
        rel = os.path.relpath(path, REPO_ROOT)
        try:
            with open(path, encoding="utf-8") as handle:
                lock = json.load(handle)
        except (OSError, ValueError) as exc:
            failures.append(f"{rel}: unreadable lockfile: {exc}")
            continue
        mirror_urls = sorted(
            f"{pkg}: {entry.get('resolved')}"
            for pkg, entry in lock.get("packages", {}).items()
            if isinstance(entry, dict)
            and "registry.npmmirror.com" in (entry.get("resolved") or "")
        )
        if mirror_urls:
            failures.append(
                f"{rel}: {len(mirror_urls)} resolved URLs point at "
                f"registry.npmmirror.com (must be registry.npmjs.org):\n  "
                + "\n  ".join(mirror_urls)
            )
        if rel == os.path.join("team-relay-hub", "package-lock.json"):
            hub_libc_count = sum(
                1
                for entry in lock.get("packages", {}).values()
                if isinstance(entry, dict) and "libc" in entry
            )
    if hub_libc_count is None:
        failures.append(
            "team-relay-hub/package-lock.json not found — guard ran against the wrong tree?"
        )
    elif hub_libc_count != EXPECTED_TEAM_RELAY_HUB_LIBC_ENTRIES:
        failures.append(
            f"team-relay-hub/package-lock.json: expected "
            f"{EXPECTED_TEAM_RELAY_HUB_LIBC_ENTRIES} @img/* libc fields, found "
            f"{hub_libc_count} — a lock rewrite on npm 10 (arborist does not "
            f"serialize libc there) strips them; regenerate with npm 11+ or "
            f"restore the fields"
        )
    if failures:
        print("check-lock-registry: FAILED", file=sys.stderr)
        for failure in failures:
            print(f"\n{failure}", file=sys.stderr)
        return 1
    print(
        f"check-lock-registry: ok — {len(lock_files)} lockfiles mirror-free, "
        f"team-relay-hub libc fields intact ({hub_libc_count})"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Lockfile registry-hygiene guard (PR #1518).

Two failure classes are pinned here:

1. `resolved` URLs pointing at registry.npmmirror.com — the mirror is slow
   and a cross-border single point; `npm ci` consumes lock URLs verbatim,
   so every mirror URL is a latency/flake dependency on every build. The
   two repo locks that are consumed by CI (team-relay-hub,
   codex-bridge-runtime) carried 134 such URLs in total before the #1518
   cleanup; this keeps them from creeping back in.

2. Missing or wrong `libc` fields on the @img/* platform packages in
   team-relay-hub/package-lock.json — npm 10's arborist does not serialize
   `libc` (pkgMetaKeys gained the key only in npm 11), so a single local
   `npm install` on node 22 silently strips the filtering fields; without
   them every Linux environment installs BOTH the glibc and the musl
   native binaries. The exact pkg→libc mapping is pinned (round-6 review:
   a bare count survives a glibc/musl value swap, which installs the
   wrong-ABI binary), so both the strip and the swap turn red here instead
   of being discovered as install-bloat or odd crashes later. If the @img
   platform set legitimately changes, update EXPECTED_TEAM_RELAY_HUB_LIBC.

3. Lockfile format downgrades — the mirror scan walks the v3 `packages`
   map; a lock rewritten at lockfileVersion != 3 (an npm-old regen) would
   otherwise skip every check silently. Version 3 is pinned.

Coverage note: the CI fast-gate checks out with `submodules: false` and runs
this guard BEFORE the CodeWhale submodule init, so it sees the repo's own
locks only (5 today); a locally initialized tree scans the submodule's locks
too (12). The npm ci consumers this guard protects are both repo-owned, so
the CI coverage is the one that matters.
"""

import json
import os
import sys

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# team-relay-hub's @img/* platform packages and their serialized `libc`
# values (registry metadata for sharp 0.35.5 / @img/libvips 1.3.4). Keys are
# the package suffix starting at "@img/" so both v3 keys
# ("node_modules/@img/…") and bare "@img/…" keys map onto one constant.
EXPECTED_TEAM_RELAY_HUB_LIBC = {
    "@img/sharp-linux-arm": ["glibc"],
    "@img/sharp-linux-arm64": ["glibc"],
    "@img/sharp-linux-ppc64": ["glibc"],
    "@img/sharp-linux-riscv64": ["glibc"],
    "@img/sharp-linux-s390x": ["glibc"],
    "@img/sharp-linux-x64": ["glibc"],
    "@img/sharp-linuxmusl-arm64": ["musl"],
    "@img/sharp-linuxmusl-x64": ["musl"],
    "@img/sharp-libvips-linux-arm": ["glibc"],
    "@img/sharp-libvips-linux-arm64": ["glibc"],
    "@img/sharp-libvips-linux-ppc64": ["glibc"],
    "@img/sharp-libvips-linux-riscv64": ["glibc"],
    "@img/sharp-libvips-linux-s390x": ["glibc"],
    "@img/sharp-libvips-linux-x64": ["glibc"],
    "@img/sharp-libvips-linuxmusl-arm64": ["musl"],
    "@img/sharp-libvips-linuxmusl-x64": ["musl"],
}


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
    hub_libc = None
    for path in lock_files:
        rel = os.path.relpath(path, REPO_ROOT)
        try:
            with open(path, encoding="utf-8") as handle:
                lock = json.load(handle)
        except (OSError, ValueError) as exc:
            failures.append(f"{rel}: unreadable lockfile: {exc}")
            continue
        version = lock.get("lockfileVersion")
        if version != 3:
            failures.append(
                f"{rel}: unsupported lockfileVersion {version!r} (expected 3) — "
                "the mirror scan walks the v3 `packages` map, so a "
                "format-downgraded lock would skip every check silently; "
                "regenerate the lock with a current npm"
            )
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
            hub_libc = {}
            for pkg, entry in lock.get("packages", {}).items():
                if not isinstance(entry, dict) or "libc" not in entry:
                    continue
                marker = pkg.find("@img/")
                if marker < 0:
                    continue
                hub_libc[pkg[marker:]] = entry["libc"]
    if hub_libc is None:
        failures.append(
            "team-relay-hub/package-lock.json not found — guard ran against the wrong tree?"
        )
    else:
        expected = EXPECTED_TEAM_RELAY_HUB_LIBC
        for pkg in sorted(set(expected) - set(hub_libc)):
            failures.append(
                f"team-relay-hub/package-lock.json: @img/* libc field stripped: "
                f"{pkg} is missing (npm 10 arborist does not serialize libc — "
                f"regenerate with npm 11+ or restore the fields)"
            )
        for pkg in sorted(set(hub_libc) - set(expected)):
            failures.append(
                f"team-relay-hub/package-lock.json: unexpected @img/* libc "
                f"package {pkg} (libc={hub_libc[pkg]}) — if the @img platform "
                f"set legitimately changed, update EXPECTED_TEAM_RELAY_HUB_LIBC"
            )
        for pkg in sorted(set(expected) & set(hub_libc)):
            if hub_libc[pkg] != expected[pkg]:
                failures.append(
                    f"team-relay-hub/package-lock.json: @img/* libc value drift: "
                    f"{pkg} libc={hub_libc[pkg]}, expected {expected[pkg]} — a "
                    f"swapped value installs the wrong-ABI binary; restore the "
                    f"registry metadata (or update EXPECTED_TEAM_RELAY_HUB_LIBC "
                    f"if the platform set legitimately changed)"
                )
    if failures:
        print("check-lock-registry: FAILED", file=sys.stderr)
        for failure in failures:
            print(f"\n{failure}", file=sys.stderr)
        return 1
    print(
        f"check-lock-registry: ok — {len(lock_files)} lockfiles mirror-free "
        f"(lockfileVersion 3), team-relay-hub @img/* libc mapping intact "
        f"({len(hub_libc)} pkgs)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())

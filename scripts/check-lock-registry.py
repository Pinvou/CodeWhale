#!/usr/bin/env python3
"""Lockfile registry-hygiene guard (PR #1518).

Two failure classes are pinned here:

1. `resolved` URLs pointing at a registry mirror — mirrors are slow and
   cross-border single points; `npm ci` consumes lock URLs verbatim, so
   every mirror URL is a latency/flake dependency on every build. The
   CI-consumed repo locks carried npmmirror URLs before the #1518
   cleanup (14 in team-relay-hub alone at the cleanup commit's parent;
   the PR diff clears exactly those 14 — the one remaining codex-bridge
   URL was cleaned on main itself before the round-22 rebase); the
   blocklist covers npmmirror plus
   the other common CN mirrors (round-11 review: matching only
   registry.npmmirror.com let the same lock-regen creep back in via a
   taobao/tencent/huawei/cnpm mirror in a contributor's npmrc). Matching
   is case-insensitive — hosts serialize lowercase in practice, but
   nothing forces it. Scope honesty (round-20 review): this is a
   BLOCKLIST, not an official-registry whitelist — a non-https or
   lookalike-domain form (http://registry.npmjs.org,
   registry.npmjs.org.evil.example) or a mirror host outside the marker
   list is not caught; the failure message below says the same.

2. Missing or wrong `libc` fields on the @img/* platform packages in
   team-relay-hub/package-lock.json — npm's arborist did not serialize
   `libc` before npm 11.11.0 (the serializer fix landed there; every
   npm 11.0–11.10 still strips the fields, so "npm 11+" is NOT enough —
   round-12 review), so a single local `npm install` on node 22 with an
   in-range npm silently strips the filtering fields; without them every
   Linux environment installs BOTH the glibc and the musl native
   binaries. The exact pkg→libc mapping is pinned (round-6 review: a bare
   count survives a glibc/musl value swap, which installs the wrong-ABI
   binary), so both the strip and the swap turn red here instead of being
   discovered as install-bloat or odd crashes later. If the @img platform
   set legitimately changes, update EXPECTED_TEAM_RELAY_HUB_LIBC.
   The pinned table only sees packages it lists, so a strip on a platform
   package that a later sharp bump ADDS would slip past it (round-21
   review); a structural rule closes that: every @img/* package whose
   `os` is `["linux"]` must carry `libc` — npm never strips `os`, only
   `libc`, so the pair "linux without libc" is the strip signature
   regardless of what the table knows.

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


# Registry mirror hosts banned from `resolved` URLs. npm copies the
# registry a lock was generated against verbatim, so any of these means a
# contributor-side mirror config crept back into the committed lock. The
# bare `npmmirror.com` marker (round-12 review) also covers the non-host
# tarball form `https://npmmirror.com/mirrors/...` that some tools write;
# the aliyun/ustc/163/tuna entries close the same class for the other
# common CN mirrors.
MIRROR_HOST_MARKERS = (
    "npmmirror.com",
    "npm.taobao.org",  # legacy taobao mirror (redirects to npmmirror)
    "mirrors.cloud.tencent.com",
    "mirrors.tencent.com",
    "mirrors.huaweicloud.com",
    "registry.cnpmjs.org",
    "mirrors.aliyun.com",
    "mirrors.ustc.edu.cn",
    "mirrors.163.com",
    "mirrors.tuna.tsinghua.edu.cn",
)


def iter_lock_files():
    for root, dirs, files in os.walk(REPO_ROOT):
        dirs[:] = [
            d for d in dirs if d not in ("node_modules", ".git", "target", "dist")
        ]
        for name in files:
            if name == "package-lock.json":
                yield os.path.join(root, name)


def _targets_linux(os_field):
    """True when a package's `os` field names linux. npm writes a list of
    lowercase platform names, but the guard normalizes anyway (round-23:
    the old `== ["linux"]` equality let `["linux", "darwin"]`, a bare
    "linux" string, or a capitalized name slip past the libc strip
    signature)."""
    if isinstance(os_field, str):
        os_field = [os_field]
    if not isinstance(os_field, list):
        return False
    return any(isinstance(name, str) and name.strip().lower() == "linux" for name in os_field)


def main():
    failures = []
    lock_files = sorted(iter_lock_files())
    if not lock_files:
        failures.append("no package-lock.json found — guard ran against the wrong tree?")
    hub_libc = None
    hub_linux_without_libc = []
    hub_seen = False
    for path in lock_files:
        rel = os.path.relpath(path, REPO_ROOT)
        if rel == os.path.join("team-relay-hub", "package-lock.json"):
            hub_seen = True
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
        # Round-13 review: a v3 lock whose `packages` map is missing or
        # empty made the mirror scan pass silently (no real npm produces
        # that shape — even a dependency-free lock carries the "" root
        # entry), so treat it as corruption rather than a clean scan.
        if version == 3 and not lock.get("packages"):
            failures.append(
                f"{rel}: lockfileVersion 3 but the `packages` map is "
                "missing or empty — the mirror scan would pass silently "
                "over nothing; regenerate the lock with npm (the root "
                '"" entry must exist)'
            )
        mirror_urls = []
        for pkg, entry in lock.get("packages", {}).items():
            if not isinstance(entry, dict):
                continue
            resolved = entry.get("resolved")
            if resolved is None:
                continue
            if not isinstance(resolved, str):
                # round-23: a non-string `resolved` used to crash the scan
                # with a traceback (still non-zero, but unreadable) — npm
                # never writes one, so report it as corruption instead.
                failures.append(
                    f"{rel}: {pkg}: `resolved` is not a string "
                    f"({type(resolved).__name__}) — corrupt lock entry"
                )
                continue
            if any(marker in resolved.lower() for marker in MIRROR_HOST_MARKERS):
                mirror_urls.append(f"{pkg}: {resolved}")
        mirror_urls.sort()
        if mirror_urls:
            failures.append(
                f"{rel}: {len(mirror_urls)} resolved URLs point at a "
                f"known npm registry mirror (this guard is a mirror "
                f"blocklist, not an official-registry whitelist — it "
                f"catches the marker hosts below on any resolved URL):\n  "
                + "\n  ".join(mirror_urls)
            )
        if rel == os.path.join("team-relay-hub", "package-lock.json"):
            hub_libc = {}
            for pkg, entry in lock.get("packages", {}).items():
                if not isinstance(entry, dict):
                    continue
                marker = pkg.find("@img/")
                if marker < 0:
                    continue
                table_key = pkg[marker:]
                if "libc" in entry:
                    if table_key in hub_libc and hub_libc[table_key] != entry["libc"]:
                        # round-23: duplicate table entries (a nested
                        # node_modules copy) disagreeing on libc used to be
                        # last-write-wins — the wrong-ABI copy could hide
                        failures.append(
                            f"team-relay-hub/package-lock.json: {pkg} carries "
                            f"libc={entry['libc']} but another copy of "
                            f"{table_key} carries libc={hub_libc[table_key]} "
                            f"— duplicate entries must agree"
                        )
                    hub_libc.setdefault(table_key, entry["libc"])
                elif _targets_linux(entry.get("os")):
                    hub_linux_without_libc.append((pkg, table_key))
    if hub_libc is None:
        # Round-13 review: distinguish "absent" from "present but
        # unreadable" — the unreadable path above already reported the
        # real error, and the stale not-found message on top of it sent
        # the reader hunting for a file that exists.
        if not hub_seen:
            failures.append(
                "team-relay-hub/package-lock.json not found — guard ran against the wrong tree?"
            )
        else:
            failures.append(
                "team-relay-hub/package-lock.json could not be parsed — "
                "@img/* libc mapping unchecked (see the unreadable-lockfile "
                "error above)"
            )
    else:
        expected = EXPECTED_TEAM_RELAY_HUB_LIBC
        for pkg in sorted(set(expected) - set(hub_libc)):
            failures.append(
                f"team-relay-hub/package-lock.json: @img/* libc field stripped: "
                f"{pkg} is missing (npm arborist below 11.11.0 does not "
                f"serialize libc — regenerate with npm >= 11.11 or restore "
                f"the fields)"
            )
        for pkg in sorted(set(hub_libc) - set(expected)):
            failures.append(
                f"team-relay-hub/package-lock.json: unexpected @img/* libc "
                f"package {pkg} (libc={hub_libc[pkg]}) — if the @img platform "
                f"set legitimately changed, update EXPECTED_TEAM_RELAY_HUB_LIBC"
            )
        # Packages the table already lists are reported as "stripped" above;
        # this catches the strip on a linux platform package the table has
        # never heard of (a sharp bump that adds one), where the set
        # difference alone stays silent.
        # Round-23: keyed by the FULL package path — a nested duplicate
        # (`node_modules/sharp/node_modules/@img/...`) stripped of libc used
        # to be shadowed by its top-level copy and skipped silently. A table
        # package whose EVERY copy lost libc is already reported as
        # "stripped" above; everything else lands here.
        for pkg, table_key in sorted(hub_linux_without_libc):
            if table_key in expected and table_key not in hub_libc:
                continue
            failures.append(
                f"team-relay-hub/package-lock.json: linux @img/* package {pkg} "
                f"carries no libc field (a linux os entry without libc is the "
                f"npm < 11.11 strip signature) — regenerate with npm >= 11.11 "
                f"or restore the field"
                + (
                    ""
                    if table_key in expected
                    else ", then add the package to EXPECTED_TEAM_RELAY_HUB_LIBC"
                )
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

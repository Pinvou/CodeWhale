#!/usr/bin/env python3
"""Test-registry guard: fail on duplicate test registrations or missing pins.

A duplicated `#[test]` attribute registers one function twice while silently
stripping the attribute from its neighbor — both halves happened in PR #455
(round 26 and again in round 28), and raw run totals cannot catch the pair
because the inflation and the deflation cancel. Against
`cargo test --lib -- --list` this guard asserts:

  1. every test path is registered at most once (duplicate = inflated run);
  2. every consent-critical pin in REQUIRED_PINS is present exactly once
     (absent = a stolen attribute silenced the pin). Pins are FULL libtest
     paths: matching only the function's last segment would let a same-named
     test planted in a different module satisfy a pin whose real test lost
     its attribute there (the substitution hole). The cost is that moving a
     pinned test to another module turns the guard red — loud and
     one-line-to-fix, which is the direction a pin should fail. Extend
     REQUIRED_PINS when a new consent/rollback-critical pin lands.

Platform caveat (review #455 round-30 m10): two REQUIRED_PINS are
`#[cfg(unix)]`-gated (they need an unreadable-file fixture), so a local run
only misses them on Windows — `cfg(unix)` holds on macOS too (12/12 verified
there); the CI rust-test leg that wires this guard is Linux, where every
configuration resolves. The registry scope is the pinvou3-app/src-tauri
crate; covering the other test manifests (e.g. pinvou-cli) is a registered
follow-up.

Run it from anywhere (paths are anchored to this script's repo location);
it reuses cargo's artifacts, and CI wires it into the linux rust-test leg
right after `cargo test --lib --no-run`.
"""

import collections
import os
import re
import subprocess
import sys

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MANIFEST = os.path.join(REPO_ROOT, "pinvou3-app", "src-tauri", "Cargo.toml")

# Consent/rollback-critical regression pins, as FULL libtest paths (what
# `cargo test --lib -- --list` prints). A missing entry here means the pin's
# `#[test]` attribute was lost to an edit — the exact failure this guard
# exists for (PR #455 rounds 26 and 28). Full paths close the substitution
# hole: a same-named test in another module cannot satisfy a pin. Moving a
# pinned test to another module fails loud with the new location; update the
# path in that (rare, deliberate) case.
REQUIRED_PINS = [
    # commands/tests.rs: the install-path consent rollback pair (round-33
    # minor 6, review #455) — exactly the consent/rollback class this guard
    # exists to protect from attribute theft.
    "app::commands::tests::install_validation_failure_rollback_leaves_no_consent_rows",
    "app::commands::tests::install_consent_sync_persist_failure_is_honest_and_keeps_pack",
    # scope.rs: exact-cleanup hijack pin (round-26 MAJOR 1) and the
    # state_changed hot-refresh gate pin (round-28 MINOR 3) — the pair whose
    # attributes were swapped by the round-28 insertion.
    "features::marketplace::scope::tests::exact_cleanup_never_reowns_absent_dir_id_onto_foreign_claim",
    "features::marketplace::scope::tests::enable_packages_state_changed_tracks_persisted_delta",
    # marketplace/mod.rs: secrets resync keeps the previous registry on
    # unreadable state, and (round-30 MAJOR) a faulting mid-rebuild keyring
    # read discards the partial rebuild wholesale instead of leaving a
    # half-rebuilt registry.
    "features::marketplace::tests::secret_values_resync_unreadable_registry_keeps_previous",
    "features::marketplace::tests::secret_values_resync_keyring_fault_keeps_previous_registry",
    # plugin_import.rs: rebaseline keeps the restore point through a supply
    # failure rollback.
    "features::marketplace::plugin_import::tests::unified_import_keeps_backup_through_supply_failure_rollback",
    # recycle_bin.rs: consent-gate collision pin, secrets-restore pin, and
    # the supply-failure rollback pin.
    "features::marketplace::recycle_bin::tests::restore_consent_gate_never_reowns_collided_pack_id",
    "features::marketplace::recycle_bin::tests::restore_secrets_pack_into_uninitialized_scope_persists_consent",
    "features::marketplace::recycle_bin::tests::restore_mcp_supply_failure_rolls_back_to_recycle_bin",
    # marketplace/mod.rs (round-34): a legacy scope file that exists but
    # cannot be consumed must fail closed over the DenyAll fallback, never
    # migrate as "nothing was disabled" while the wide signal initializes
    # plain fully-on.
    "features::marketplace::tests::corrupt_legacy_file_fails_closed_not_implicitly_all_on",
    "features::marketplace::tests::schema_invalid_legacy_file_fails_closed",
]


def list_tests():
    # Must mirror the CI rust-test leg's feature set exactly (`cargo test
    # --lib --no-default-features --locked` in pr-check.yml): the leg builds
    # that configuration right before this guard runs, so matching it reuses
    # the just-built artifacts instead of recompiling the whole lib. Upstream
    # lists default features + benchmark-hooks because its own legs build
    # that way; this fork's memory-constrained leg does not, and no
    # REQUIRED_PIN is feature-gated (two are #[cfg(unix)]-gated, which is
    # expected to resolve on the Linux CI where this guard is wired).
    cmd = [
        "cargo",
        "test",
        "--manifest-path",
        MANIFEST,
        "--lib",
        "--no-default-features",
        "--locked",
        "--",
        "--list",
        "--format",
        "terse",
    ]
    env = dict(os.environ, CARGO_TERM_COLOR="never")
    result = subprocess.run(cmd, capture_output=True, text=True, env=env)
    if result.returncode != 0:
        sys.stderr.write(result.stderr)
        raise SystemExit("test-registry-guard: cargo test --list failed")
    paths = []
    for line in result.stdout.splitlines():
        if line.endswith(": test"):
            paths.append(line[: -len(": test")])
    if not paths:
        raise SystemExit("test-registry-guard: parsed zero tests — output format changed?")
    return paths


_FN_RE = re.compile(
    r"^(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:unsafe\s+)?"
    r"fn\s+([A-Za-z_][A-Za-z0-9_]*)"
)
_IGNORE_RE = re.compile(r"\bignore\b")


def _outside_strings(line, in_str):
    """Strip double-quoted string contents from `line` (backslash escapes
    honoured), carrying the in-string state across lines. Returns
    (text, in_str)."""
    out = []
    k = 0
    while k < len(line):
        ch = line[k]
        if in_str:
            if ch == "\\":
                k += 2
                continue
            if ch == '"':
                in_str = False
        elif ch == '"':
            in_str = True
        else:
            out.append(ch)
        k += 1
    return "".join(out), in_str


def _parse_attribute(lines, i):
    """Consume one `#[...]` attribute entry starting at line `i` (whose
    stripped text starts with `#[`); the entry may span lines (rustfmt-
    wrapped `#[cfg_attr(...)]`). Bracket depth is counted OUTSIDE string
    literals, so `#[ignore = "a[b]"]` terminates correctly and a
    `#[cfg_attr(feature = "ignore-me", ...)]` is judged by its tokens, not
    its prose. Returns (end_idx_exclusive, ignore_line_idx_or_None) — the
    reported location is the `ignore`-bearing line itself, not the block
    top."""
    depth = 0
    in_str = False
    ignore_line = None
    j = i
    n = len(lines)
    while j < n:
        outside, in_str = _outside_strings(lines[j], in_str)
        if ignore_line is None and _IGNORE_RE.search(outside):
            ignore_line = j
        depth += outside.count("[") - outside.count("]")
        if depth <= 0:
            return j + 1, ignore_line
        j += 1
    return n, ignore_line


def _fn_after(lines, idx):
    """If the first non-blank line at/after `idx` declares a function,
    return (fn_name, line_idx), else (None, None). Blank lines do not break
    attachment; a comment line does — an `#[ignore]` mentioned in a comment
    must not smear onto the next function. `pub`/`async`/`unsafe` prefixes,
    generics (`fn g<T: Clone>()`) and rustfmt-wrapped signatures
    (`fn long(` with the parens on the following lines) all resolve; the
    paren must open on the name's line.
    """
    j = idx
    n = len(lines)
    while j < n and not lines[j].strip():
        j += 1
    if j >= n:
        return None, None
    stripped = lines[j].lstrip()
    if stripped.startswith("//"):
        return None, None
    match = _FN_RE.match(stripped)
    if match is None or "(" not in stripped:
        return None, None
    return match.group(1), j


def ignored_source_pins():
    """Round-35 minor 10 (review #455): libtest's stable `--list` output cannot
    distinguish an #[ignore]d test from a live one, so scan the crate sources:
    any `fn <name>` whose directly attached attribute run contains an
    `ignore` token (covers `#[ignore]`, `#[ignore = "..."]`,
    `#[cfg_attr(..., ignore)]` in single- and multi-line form) is a
    silenced test.

    The scan is attribute-run based (round-6 review): starting from each
    `#[` line, whole bracket-balanced attribute entries are consumed (so
    wrapped cfg_attr forms resolve), and the run attaches to the function
    declared on the next non-blank, non-comment line. A doc (`///`) or
    plain comment terminates the run, so a comment that merely mentions
    `#[ignore` cannot smear onto this one. The map is keyed by bare
    function name to ALL locations; pin attribution is module-aware (see
    main), so a same-named ignored test in an unrelated module does not
    shadow or frame the pin.
    """
    src_root = os.path.join(os.path.dirname(MANIFEST), "src")
    hits = {}
    for root, dirs, files in os.walk(src_root):
        dirs.sort()
        for name in sorted(files):
            if not name.endswith(".rs"):
                continue
            path = os.path.join(root, name)
            try:
                with open(path, encoding="utf-8", errors="replace") as fh:
                    lines = fh.read().splitlines()
            except OSError:
                continue
            i = 0
            n = len(lines)
            while i < n:
                if not lines[i].lstrip().startswith("#["):
                    i += 1
                    continue
                run = []
                j = i
                while j < n:
                    stripped = lines[j].lstrip()
                    if not stripped:
                        j += 1  # blank lines are transparent within the run
                        continue
                    if not stripped.startswith("#["):
                        break
                    j, ignore_line = _parse_attribute(lines, j)
                    if ignore_line is not None:
                        run.append(ignore_line)
                fn_name, fn_idx = _fn_after(lines, j)
                if run and fn_name is not None:
                    hits.setdefault(fn_name, []).extend(
                        f"{path}:{line + 1}" for line in run
                    )
                i = fn_idx + 1 if fn_idx is not None else max(j, i + 1)
    return hits


def _candidate_pin_files(pin):
    """Source files that could directly contain the pinned test, derived
    from its libtest module path under the standard layouts: the test's
    parent module either lives in its own file (`a/b/tests.rs`,
    `a/b/tests/mod.rs`) or is declared inline in the grandparent's module —
    both its file form (`a/b.rs`) and its directory form (`a/b/mod.rs`).
    (Round-7 review: the grandparent FILE form was documented but missing
    from the list, which emptied the candidates for pins whose `tests` sits
    inline in a file module like `marketplace/scope.rs` and silently pushed
    them onto the bare-name fallback — reopening the cross-module
    false-positive hole for those pins.) Returns only files that exist; an
    empty result (unconventional layout) makes callers fall back to the
    bare-name match — fail closed."""
    parts = pin.split("::")
    parent, leaf = parts[:-1], parts[-1]
    src = os.path.join(os.path.dirname(MANIFEST), "src")
    if not parent:
        candidates = [
            os.path.join(src, leaf + ".rs"),
            os.path.join(src, leaf, "mod.rs"),
        ]
    else:
        base = os.path.join(src, *parent)
        candidates = [
            os.path.join(base, leaf + ".rs"),
            os.path.join(base, leaf, "mod.rs"),
            base + ".rs",
            os.path.join(base, "mod.rs"),
            os.path.dirname(base) + ".rs",
            os.path.join(os.path.dirname(base), "mod.rs"),
        ]
    return [c for c in candidates if os.path.isfile(c)]


def _pin_ignored_locations(pin, ignored_src):
    """Locations where the pin's own function is #[ignore]d, attributed
    module-awarely (round-6 review): the pin's module path maps to the
    standard files that could host it (`_candidate_pin_files`), and only an
    ignored fn in one of those files flags the pin — a same-named ignored
    test in an unrelated module stays silent. When the layout admits no
    candidate file the bare-name match applies to every hit: fail closed.
    Kept a pure function so the attribution is unit-testable without cargo
    (review #1518: the cross-module same-name positive control must be
    pinned by a committed regression, not a one-off manual run)."""
    seg_hits = ignored_src.get(pin.rsplit("::", 1)[-1])
    if not seg_hits:
        return []
    candidates = _candidate_pin_files(pin)
    if candidates:
        return [loc for loc in seg_hits if loc.rsplit(":", 1)[0] in candidates]
    return seg_hits


def main():
    if len(sys.argv) > 1:
        raise SystemExit(
            f"test-registry-guard: takes no arguments (got {sys.argv[1:]!r})"
        )
    paths = list_tests()
    failures = []

    counts = collections.Counter(paths)
    duplicates = sorted(p for p, n in counts.items() if n > 1)
    if duplicates:
        failures.append(
            "duplicate test registrations (a duplicated #[test] attribute "
            "silences a neighbouring pin):\n"
            + "\n".join(f"  {p} x{counts[p]}" for p in duplicates)
        )

    # Pin matching: FULL libtest path equality (see REQUIRED_PINS). A missing
    # pin whose last segment still appears exactly once in the registry gets
    # its own message — that shape is a module move, not a stolen attribute,
    # and naming the new location makes the fix a one-liner. A pin silenced
    # by #[ignore] (stable libtest --list cannot flag ignored tests) is
    # caught by the source-level scan below; `_pin_ignored_locations` does
    # the module-aware attribution.
    ignored_src = ignored_source_pins()
    by_segment = collections.defaultdict(list)
    for p in paths:
        by_segment[p.rsplit("::", 1)[-1]].append(p)
    for pin in REQUIRED_PINS:
        if pin in counts and counts[pin] == 1:
            matched = _pin_ignored_locations(pin, ignored_src)
            if matched:
                failures.append(
                    f"required pin {pin} is #[ignore]d at "
                    + ", ".join(matched)
                )
            continue
        if pin in counts:
            failures.append(f"required pin registered more than once: {pin} x{counts[pin]}")
            continue
        segment = pin.rsplit("::", 1)[-1]
        relocated = by_segment.get(segment, [])
        if relocated:
            failures.append(
                f"required pin {pin} not in the registry, but the function name "
                f"appears at {relocated} — the test moved modules; update "
                f"REQUIRED_PINS (or restore it there)"
            )
        else:
            failures.append(f"required pin absent from the registry (stolen #[test] attribute?): {pin}")

    if failures:
        print("test-registry-guard: FAILED", file=sys.stderr)
        for failure in failures:
            print(f"\n{failure}", file=sys.stderr)
        return 1

    print(f"test-registry-guard: ok — {len(paths)} unique test paths, "
          f"{len(REQUIRED_PINS)} required pins present")
    return 0


if __name__ == "__main__":
    sys.exit(main())

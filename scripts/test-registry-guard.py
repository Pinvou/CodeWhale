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

Static-scan boundary (review #1518 round-11, narrowed round-12): a pin's
ignore status is read from lexically-written attributes in lib-graph source
files. An `#[ignore]` arriving through code generation — `macro_rules!`,
`include!`, a proc-macro — is invisible to the source side, and so is a
pin moved outside the graph (integration `tests/`, bin targets). The
list-presence half still demands the registered path, so relocation
fails loud; but a *generated* `#[ignore]` on a pin that stays listed
would stay green — relocating a pin into generated code is tampering,
not an edit. The scanner follows `#[path]` wiring both at file top level
and inside inline modules (round-12: resolved per the rustc rule — the
declaring file's directory at top level, the externals' child directory
plus the inline-module components inside one), so both legal wirings are
scanned, and `main` fails CLOSED on a registered pin whose source it
cannot attribute (unmodelled wiring, macro-generated test, or
raw-identifier `mod r#type;`) instead of trusting the registry entry.
Still unmodelled (a REQUIRED_PIN there hits the fail-closed net, not a
silent skip): `#[path = "dir"]` on the inline module itself, and
`#[ignore]` written on the same line as its inline-module opener.

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
# How far past the name line a rustfmt-wrapped signature may put its
# parameter `(` (`fn g<\n    T: LoooongBound,\n    U: Other,\n>(") —
# generous, since generic lists only wrap one parameter per line; the scan
# still stops at the first `{` or `;`, so the lookahead cannot jump into a
# foreign item.
_FN_PAREN_LOOKAHEAD = 64
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
    return (fn_name, line_idx), else (None, None). `lines` is the lexed
    text from `_strip_code`: comments (line/block/doc) and string contents
    are already blanked, so they read as blanks and never break attachment
    — real Rust binding: an attribute stays attached to the next item no
    matter what comments sit between, and a pseudo-fn inside a comment
    cannot be found. `pub`/`async`/`unsafe` prefixes and generics
    (`fn g<T: Clone>()`) resolve; rustfmt-wrapped signatures resolve too —
    when a long generic parameter list pushes the parameter paren onto a
    later line (`fn g<\\n    T: Bound,\\n>(`), the scan accepts the `(` at
    the first stop before a `{` or `;` (round-12 review: requiring the
    paren on the name's line silently dropped the hit on exactly the shape
    rustfmt emits for long bound lists, with no fail-closed fallback).
    """
    j = idx
    n = len(lines)
    while j < n and not lines[j].strip():
        j += 1
    if j >= n:
        return None, None
    stripped = lines[j].lstrip()
    match = _FN_RE.match(stripped)
    if match is None:
        return None, None
    if "(" not in stripped:
        k = j + 1
        while k < n and k - j <= _FN_PAREN_LOOKAHEAD:
            text = lines[k]
            stops = [c for c in (text.find("("), text.find("{"), text.find(";")) if c != -1]
            if not stops:
                k += 1
                continue
            if min(stops) == text.find("("):
                return match.group(1), j
            return None, None
        return None, None
    return match.group(1), j


_MOD_TOKEN_RE = re.compile(
    r"\bmod\s+([A-Za-z_][A-Za-z0-9_]*)\s*(\{)?|\{|\}"
)
_PATH_ATTR_HEAD_RE = re.compile(r"#\[\s*path\s*=")


def _string_value_after(lines, li, col):
    """First string-literal payload in the raw source at/after (li, col),
    or None. Lexes raw lines with the same construct awareness as
    `_strip_code` — line and nested block comments are transparent — so
    only a string token the lexer vouches for is read (round-10 review:
    the old raw first-quote scan wired `#[path = /* "decoy.rs" */ ...]`
    to the comment's quote, and the real file silently left the lib
    graph). The scan starts at an attribute head, before its value, so no
    string can be open on entry. Escape sequences are not decoded (repo
    wiring paths are plain literals; an escaped form just resolves to a
    nonexistent file, and cargo — not this guard — is the authority on
    whether a wired file must exist, e.g. `#[cfg]`-gated platform
    wiring)."""
    block_depth = 0
    while li < len(lines):
        line = lines[li]
        k = col
        col = 0
        n = len(line)
        while k < n:
            ch = line[k]
            nxt = line[k + 1 : k + 2]
            if block_depth > 0:
                if ch == "/" and nxt == "*":
                    block_depth += 1
                    k += 2
                    continue
                if ch == "*" and nxt == "/":
                    block_depth -= 1
                    k += 2
                    continue
                k += 1
                continue
            if ch == "/" and nxt == "/":
                break  # rest of the line is comment
            if ch == "/" and nxt == "*":
                block_depth += 1
                k += 2
                continue
            if ch == '"':
                j = k + 1
                while j < n:
                    if line[j] == "\\":
                        j += 2
                        continue
                    if line[j] == '"':
                        return line[k + 1 : j]
                    j += 1
                return None  # unterminated: refuse rather than guess
            if ch in "rb" and not (
                line[k - 1 : k].isalnum() or line[k - 1 : k] == "_"
            ):
                j = k + (ch == "b")
                if j < n and line[j] == "r":
                    j += 1
                    hashes = j
                    while j < n and line[j] == "#":
                        j += 1
                    hashes = j - hashes
                    if j < n and line[j] == '"':
                        close = line.find('"' + "#" * hashes, j + 1)
                        return line[j + 1 : close] if close != -1 else None
            k += 1
        # the value may sit on a line below the attribute head, or behind
        # comments that consumed the rest of this one — without the advance
        # the outer loop would re-scan the same line forever
        li += 1
    return None


def _strip_code(line, state):
    """Blank (with spaces, positions preserved) everything that is not
    structural Rust code: line comments, nested block comments, double-
    quoted strings (backslash escapes honoured — the escaped character is
    blanked together with the backslash, so no string-interior character
    survives to masquerade as code; round-10 review: a leftover escaped
    quote read as an unclosed string and swallowed the attributed fn),
    raw strings (`r"..."`, `r#"..."#`, may span lines), char literals
    (including `'\\u{7d}'`, whose braces must not count toward nesting),
    and lifetime/label ticks (`'a`).
    `state` is `[in_str, block_depth, raw_hashes]`, carried across lines by
    the caller. Stripped text feeds brace tracking and `mod` detection, so a
    brace inside a string/comment/char literal must not skew the module
    stack."""
    out = list(line)
    in_str, block_depth, raw_hashes = state
    k = 0
    n = len(line)
    while k < n:
        if raw_hashes is not None:
            if (
                line[k] == '"'
                and line[k + 1 : k + 1 + raw_hashes] == "#" * raw_hashes
            ):
                out[k : k + 1 + raw_hashes] = " " * (1 + raw_hashes)
                k += 1 + raw_hashes
                raw_hashes = None
                continue
            out[k] = " "
            k += 1
            continue
        if in_str:
            out[k] = " "
            if line[k] == "\\":
                # blank the escaped character too: leaving it would put a
                # string-interior `"` into the stripped text, which the
                # attribute parser re-lexes as an unclosed string (it would
                # swallow the fn the run attaches to and drop a real hit)
                if k + 1 < n:
                    out[k + 1] = " "
                k += 2
                continue
            if line[k] == '"':
                in_str = False
            k += 1
            continue
        if block_depth > 0:
            out[k] = " "
            if line[k] == "/" and line[k + 1 : k + 2] == "*":
                block_depth += 1
                k += 2
                continue
            if line[k] == "*" and line[k + 1 : k + 2] == "/":
                block_depth -= 1
                k += 2
                continue
            k += 1
            continue
        ch = line[k]
        nxt = line[k + 1 : k + 2]
        if ch == "/" and nxt == "/":
            for t in range(k, n):
                out[t] = " "
            break
        if ch == "/" and nxt == "*":
            block_depth += 1
            out[k] = out[k + 1] = " "
            k += 2
            continue
        if ch == '"':
            in_str = True
            out[k] = " "
            k += 1
            continue
        if ch in "rb" and not (
            line[k - 1 : k].isalnum() or line[k - 1 : k] == "_"
        ):
            j = k + (ch == "b")
            if j < n and line[j] == "r":
                j += 1
                hashes = j
                while j < n and line[j] == "#":
                    j += 1
                hashes = j - hashes
                if j < n and line[j] == '"':
                    # blank the opening quote as well: a surviving `"` in
                    # the stripped text lexes as a plain-string opener and
                    # the attribute parser swallows everything behind it
                    out[k : j + 1] = " " * (j - k + 1)
                    raw_hashes = hashes
                    k = j + 1
                    continue
        if ch == "'":
            if nxt == "\\":
                # escape char literal: consume through the closing tick so
                # `'\u{7d}'` contributes no braces
                out[k] = " "
                k += 2
                while k < n and line[k] != "'":
                    out[k] = " "
                    k += 1
                if k < n:
                    out[k] = " "
                    k += 1
                continue
            if k + 2 < n and line[k + 2] == "'":
                out[k] = out[k + 1] = out[k + 2] = " "
                k += 3
                continue
            if nxt.isalnum() or nxt == "_":
                # lifetime or loop label (`'static`, `'outer:`), not a literal
                out[k] = " "
                k += 1
                while k < n and (line[k].isalnum() or line[k] == "_"):
                    out[k] = " "
                    k += 1
                continue
            out[k] = " "
            k += 1
            continue
        k += 1
    state[:] = [in_str, block_depth, raw_hashes]
    return "".join(out)


def _module_stacks(lines):
    """Per-line inline-module stacks plus the file's external `mod` names.

    Walks the stripped source (`_strip_code`) tracking inline
    `mod <name> { ... }` nesting: `stacks[i]` is the tuple of module names
    whose opening brace is on an earlier line and still open at line `i` —
    the inline-module half of a hit's full libtest path (review #1518
    round 6: attribution by file alone let same-named ignored fns in
    sibling inline modules of the SAME file frame a live pin). A
    `mod <name>` whose brace opens on the next line resolves by lookahead;
    rustfmt puts an `fn` on its own line, which is the layout the snapshots
    assume. External `mod <name>;` declarations (children in their own
    files) are returned for the module-graph walk as (name, enclosing
    inline-module stack) pairs — the stack decides the directory the child
    file resolves against.

    Returns (stacks, externals, clean, stripped). `clean` is False when the
    file ends with unbalanced braces/comments/strings: the simplified lexer
    cannot be trusted there, and the caller must fail closed on the file's
    hits instead of attributing them by path. `stripped` is the per-line
    lexed text — same indices as the input — and the caller must run the
    attribute scan on it (round-9 review: attachment decisions made on raw
    lines let a comment line between `#[ignore]` and its fn drop a real
    hit, and let a pseudo `#[ignore]`/fn inside a block comment fabricate
    one)."""
    stacks = [()] * len(lines)
    stripped_lines = []
    stack = []
    depth = 0
    state = [False, 0, None]
    pending = None
    externals = []
    clean = True
    for idx, raw in enumerate(lines):
        stacks[idx] = tuple(name for name, _ in stack)
        text = _strip_code(raw, state)
        stripped_lines.append(text)
        start = 0
        if pending is not None:
            first = 0
            while first < len(text) and text[first].isspace():
                first += 1
            if first < len(text):
                name, pending = pending, None
                if text[first] == "{":
                    depth += 1
                    stack.append((name, depth))
                    start = first + 1
                else:
                    start = first
        for tok in _MOD_TOKEN_RE.finditer(text, start):
            token = tok.group(0)
            if token == "{":
                depth += 1
            elif token == "}":
                depth -= 1
                if depth < 0:
                    clean = False
                    depth = 0
                while stack and stack[-1][1] > depth:
                    stack.pop()
            elif tok.group(2):
                depth += 1
                stack.append((tok.group(1), depth))
            else:
                rest = text[tok.end() :].strip()
                if rest == "":
                    pending = tok.group(1)
                elif rest.startswith(";"):
                    # external child; its inline-module stack decides the
                    # directory it resolves against (a `mod x;` nested in
                    # inline `mod s {}` lives under `s/`). Snap the stack
                    # NOW, not the line-start snapshot: braces opened
                    # earlier on this same line (`mod s { mod x; }`, legal
                    # but never emitted by rustfmt) already sit in `stack`,
                    # and the line-start tuple would resolve `x` one
                    # directory too high (round-12 review: stray files
                    # there could alias hits into wrong paths)
                    externals.append(
                        (tok.group(1), tuple(name for name, _ in stack))
                    )
    in_str, block_depth, raw_hashes = state
    if depth != 0 or block_depth != 0 or in_str or raw_hashes is not None:
        # an in_str left set means a string never closed on its line: every
        # following line was swallowed as string content, so hits downstream
        # are invisible — the file's parse cannot be trusted (same
        # fail-closed class as unbalanced braces/comments; round-10)
        clean = False
    return stacks, externals, clean, stripped_lines


def _path_wired_mods(stripped, lines, stacks, path):
    """`#[path = "..."] mod <name>;` pairs whose attribute run attaches to
    the declaration. Run delimitation is lexed (`stripped` from
    `_strip_code`): blanks and comments are transparent within the run and
    commented-out attributes/declarations are invisible — Rust attachment,
    in both the "comment breaks the wiring" and "pseudo attribute inside a
    block comment fabricates one" directions (round-9 review). The VALUE is
    read only from a string token `_string_value_after` vouches for in the
    raw block (the attribute head's position maps 1:1 because the lexer
    blanks with positions preserved); an attribute head whose value cannot
    be reliably parsed raises instead of silently skipping the wired module
    (round-10 review). Attributes BETWEEN the path entry and the `mod`
    declaration — the idiomatic `#[path = "..."]` + `#[cfg(unix)]`
    platform-wiring shape — no longer break the pairing: the run is
    consumed entry by entry and a seen path value stays pending until the
    declaration (or a non-attribute item) ends the run (round-12 review:
    the old single-entry pairing silently dropped exactly that wiring,
    while the `#[cfg]`-first order worked). Wirings are modelled wherever
    they appear: the returned inline-module stack (empty at file top
    level) tells the caller which resolution base rustc uses — round-12
    review closed the silent skip of `mod outer { #[path = "real.rs"]
    mod routed; }` (rustc loads `src/outer/real.rs`; the file previously
    never entered the scan and an `#[ignore]` there was invisible). A
    `#[path]` on the inline module itself (`#[path = "dir"] mod inner {
    ... }`) is still not followed (children resolve under the attribute
    directory — unmodelled; a REQUIRED_PIN there fails closed via the
    source-attribution net in `main`).
    Returns [(mod_name, path_value, inline_module_stack)]."""
    wired = []
    pending_value = None
    i = 0
    n = len(stripped)
    while i < n:
        if not stripped[i].lstrip().startswith("#["):
            if pending_value is not None:
                # the run ended on a non-attribute item without a `mod`
                # declaration: rustc would attach the path attribute to
                # that item instead — not a wiring this guard models
                pending_value = None
            i += 1
            continue
        end, _ = _parse_attribute(stripped, i)
        joined = "\n".join(stripped[i:end])
        head = _PATH_ATTR_HEAD_RE.search(joined)
        if head is not None:
            nl = joined.rfind("\n", 0, head.end())
            value = _string_value_after(
                lines[i:end],
                joined.count("\n", 0, head.end()),
                head.end() if nl == -1 else head.end() - nl - 1,
            )
            if value is None:
                raise SystemExit(
                    "test-registry-guard: cannot read the #[path] value "
                    f"at {path}:{i + 1} — refusing to silently skip the "
                    "wired module"
                )
            pending_value = value
        j = end
        while j < n and not stripped[j].strip():
            j += 1
        if j >= n:
            break
        if stripped[j].lstrip().startswith("#["):
            # another attribute entry in the same run: keep any pending
            # path value and pair it with the declaration after it
            i = j
            continue
        decl = stripped[j]
        tok = _MOD_TOKEN_RE.search(decl)
        if (
            pending_value is not None
            and tok is not None
            and tok.group(1) is not None
            and not tok.group(2)
            and decl[tok.end() :].strip().startswith(";")
        ):
            wired.append((tok.group(1), pending_value, tuple(stacks[j])))
            pending_value = None
            i = j + 1
            continue
        pending_value = None
        i = end
    return wired


def _scan_graph():
    """Walk the lib module graph from `src/lib.rs` and return
    (by_path, unattributable, attributable):

    - `by_path`: full libtest path -> `file:line` locations of every fn
      whose attached attribute run contains an `ignore` token;
    - `unattributable`: bare fn name -> locations for hits in files the
      lexer could not confidently parse (fail closed);
    - `attributable`: full libtest paths of every fn carrying ANY attached
      attribute entry — the proof set `main`'s source-attribution net uses
      (round-12 review): every normally-declared test fn carries at least
      its `#[test]`/`#[tokio::test]` attribute, so a registered pin absent
      from this set has its source outside the scanned graph and an
      `#[ignore]` there would be invisible.

    Scoping: the walk is rooted at `src/lib.rs` and follows external
    `mod <name>;` declarations — both conventional (`a/b.rs` +
    `a/b/<name>.rs` layouts) and `#[path = "..."]` wiring (at file top
    level relative to the declaring file's directory; inside inline
    modules relative to the externals' child directory plus the
    inline-module components, per the rustc rule — round-12), so it covers
    exactly the lib crate that `cargo test --lib --list` reports — bin-only
    files (`main.rs`, undeclared strays) are never scanned and cannot alias
    a pin's path. Hits are keyed by their FULL libtest module path: the
    declaring file's module path from the graph plus the inline-module
    stack at the function's line.

    The scan is attribute-run based (round-6 review): starting from each
    `#[` line, whole bracket-balanced attribute entries are consumed (so
    wrapped cfg_attr forms resolve), and the run attaches to the function
    declared on the next non-blank line. The whole scan runs on the LEXED
    text from `_strip_code` (round-9 review): comments — line, block, and
    doc — are blanked before attachment is decided, matching real Rust
    binding semantics in both directions. A comment cannot fabricate a hit
    (a `#[ignore]` mentioned or commented out is invisible), and a comment
    cannot hide one: a `#[ignore]` whose fn follows after a comment line
    (or a `///` doc comment, itself an attribute) stays attached and is
    still flagged, exactly as rustc keeps the attribute bound."""
    src_root = os.path.join(os.path.dirname(MANIFEST), "src")
    lib_root = os.path.join(src_root, "lib.rs")
    if not os.path.isfile(lib_root):
        raise SystemExit(
            "test-registry-guard: src/lib.rs not found — the guard scopes "
            "its scan to the lib module graph rooted there"
        )
    by_path = {}
    unattributable = {}
    attributable = set()
    visited = set()
    queue = [(lib_root, [])]
    while queue:
        path, base = queue.pop(0)
        real = os.path.realpath(path)
        if real in visited:
            continue
        visited.add(real)
        try:
            with open(path, encoding="utf-8", errors="replace") as fh:
                lines = fh.read().splitlines()
        except OSError as err:
            raise SystemExit(
                f"test-registry-guard: cannot read lib source {path}: {err}"
            )
        stacks, externals, clean, stripped = _module_stacks(lines)
        i = 0
        n = len(stripped)
        while i < n:
            if not stripped[i].lstrip().startswith("#["):
                i += 1
                continue
            run = []
            entries = 0
            j = i
            while j < n:
                lexed = stripped[j].lstrip()
                if not lexed:
                    # blanks and comments (blanked by the lexer) are both
                    # transparent within the run — rustc attachment semantics
                    j += 1
                    continue
                if not lexed.startswith("#["):
                    break
                j, ignore_line = _parse_attribute(stripped, j)
                entries += 1
                if ignore_line is not None:
                    run.append(ignore_line)
            fn_name, fn_idx = _fn_after(stripped, j)
            if entries and fn_name is not None:
                if clean:
                    full = "::".join(
                        list(base) + list(stacks[fn_idx]) + [fn_name]
                    )
                    if run:
                        locations = [f"{path}:{line + 1}" for line in run]
                        by_path.setdefault(full, []).extend(locations)
                    attributable.add(full)
                else:
                    locations = [f"{path}:{line + 1}" for line in run]
                    unattributable.setdefault(fn_name, []).extend(locations)
            i = fn_idx + 1 if fn_idx is not None else max(j, i + 1)
        # Child files of external `mod <name>;` declarations (Rust 2018):
        # the crate roots keep children beside themselves (`mod app;` in
        # lib.rs → src/app.rs), `mod.rs` keeps them in its own directory,
        # and any other file `a/b.rs` opens the `a/b/` directory.
        fname = os.path.basename(path)
        if fname in ("lib.rs", "main.rs", "mod.rs"):
            child_dir = os.path.dirname(path)
        else:
            child_dir = os.path.join(os.path.dirname(path), fname[:-3])
        for name, estack in externals:
            mod_dir = os.path.join(child_dir, *estack)
            for candidate in (
                os.path.join(mod_dir, name + ".rs"),
                os.path.join(mod_dir, name, "mod.rs"),
            ):
                if os.path.isfile(candidate):
                    queue.append((candidate, base + list(estack) + [name]))
                    break
        # `#[path = "..."]` wiring (12 real sites, e.g. file_ingest.rs).
        # rustc resolution, round-12 (verified against the Rust Reference
        # and rustc itself): a wiring OUTSIDE any inline module resolves
        # against the declaring file's own directory; inside inline
        # modules it resolves against the same base the conventional
        # externals use (dirname for mod-rs roots, dirname/<file-stem>
        # for other files) with the inline-module components appended as
        # directories — exactly `child_dir + estack`. The conventional
        # candidates above do not exist for these, so both resolutions
        # coexist without double visits (the graph dedups by realpath).
        file_dir = os.path.dirname(path)
        for name, rel, estack in _path_wired_mods(stripped, lines, stacks, path):
            if estack:
                candidate = os.path.normpath(
                    os.path.join(child_dir, *estack, rel)
                )
                mod_path = base + list(estack)
            else:
                candidate = os.path.normpath(os.path.join(file_dir, rel))
                mod_path = base
            if os.path.isfile(candidate):
                queue.append((candidate, mod_path + [name]))
    return by_path, unattributable, attributable


def ignored_source_pins():
    """`#[ignore]` source hits, as (by_path, unattributable) — round-35
    minor 10 (review #455): libtest's stable `--list` output cannot
    distinguish an #[ignore]d test from a live one, so scan the crate
    sources: any `fn <name>` whose directly attached attribute run contains
    an `ignore` token (covers `#[ignore]`, `#[ignore = "..."]`,
    `#[cfg_attr(..., ignore)]` in single- and multi-line form) is a
    silenced test. Thin wrapper over `_scan_graph` (which additionally
    returns the all-attributable-paths proof set main's net uses), keeping
    the two-value shape the behavior suite unpacks."""
    by_path, unattributable, _ = _scan_graph()
    return by_path, unattributable


def _pin_ignored_locations(pin, by_path, unattributable):
    """Locations where the pin's own test function is #[ignore]d, attributed
    by FULL libtest module path (review #1518 round 6): the scan resolves
    each hit's inline-module stack inside the lib module graph, so a
    same-named ignored fn in a sibling module — even within the same file —
    keys a different path and cannot frame a live pin. Only two inputs can
    flag a pin: an exact full-path match, or a same-leaf hit in a file whose
    structure the lexer could not confidently parse (fail closed — the old
    file-level filter's fallback, narrowed to genuinely unattributable
    hits). Kept a pure function so the attribution is unit-testable without
    cargo (review #1518: the cross-module same-name positive controls must
    be pinned by committed regressions, not one-off manual runs)."""
    matched = list(by_path.get(pin, []))
    matched.extend(unattributable.get(pin.rsplit("::", 1)[-1], []))
    return matched


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
    # the full-module-path attribution (inline-module stacks inside the lib
    # module graph), with the fail-closed leaf-net for unparseable files.
    by_path, unattributable, attributable = _scan_graph()
    by_segment = collections.defaultdict(list)
    for p in paths:
        by_segment[p.rsplit("::", 1)[-1]].append(p)
    for pin in REQUIRED_PINS:
        if pin in counts and counts[pin] == 1:
            matched = _pin_ignored_locations(pin, by_path, unattributable)
            if matched:
                failures.append(
                    f"required pin {pin} is #[ignore]d at "
                    + ", ".join(matched)
                )
                continue
            if (
                pin not in attributable
                and pin.rsplit("::", 1)[-1] not in unattributable
            ):
                # Fail closed (round-12 review): a listed pin whose fn the
                # source scan cannot attribute is a pin whose #[ignore]
                # would be invisible too — exactly the false-green shape
                # (every normally-declared test fn carries at least its
                # `#[test]` attribute, so it is attributable). Refuse to
                # trust the registry entry until the scanner models
                # whatever wiring hides the source.
                failures.append(
                    f"required pin {pin} is registered but its source "
                    "location is outside the scanned lib module graph "
                    "(unmodelled #[path] wiring, macro-generated test, or "
                    "raw-identifier mod) — an #[ignore] there would go "
                    "undetected; extend the scanner before trusting it"
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

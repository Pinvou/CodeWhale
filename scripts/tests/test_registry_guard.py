"""Behavior regression for scripts/test-registry-guard.py.

Review #1518 (rounds 4-6) demonstrated that the guard's pin attribution was
the fragile half: the full-path pin can be satisfied by a same-named test in
another module, and an unrelated module's `#[ignore] fn` can frame a
properly registered pin as silenced. The fixes are pinned here as committed
regressions — the review explicitly asked for the cross-module and
same-file cross-inline-module same-name positive controls to live in the
tree, not in one-off manual runs:

  - REQUIRED_PINS use full libtest paths (rounds 4-5);
  - `#[ignore]` hits are attributed by FULL libtest module path: the scan
    walks the lib module graph from `src/lib.rs` and resolves each hit's
    inline-module stack, so a same-named ignored fn in a sibling module —
    even within the same file — keys a different path (round 6);
  - files the simplified lexer cannot confidently parse fail closed on
    their hits' bare function names (round 6 fallback, narrowed);
  - the whole attribute scan runs on the lexed text: comments cannot
    fabricate a hit (block-comment pseudo `#[ignore]`/fn) and cannot hide
    one (a comment between `#[ignore]` and its fn keeps the binding, as
    rustc does) — both round-9 review directions are pinned below;
  - string escapes are erased whole (round-10 review: a leftover escaped
    quote read as an unclosed string and swallowed the fn), and the
    `#[path]` value is read only from a string token the lexer vouches
    for — a quoted path inside the attribute's comment cannot derail the
    wiring, and an unparseable value raises instead of silently skipping
    the wired module;
  - the value reader advances across lines (round-11: a value wrapped to
    the next line, or behind a comment, re-scanned one line forever) and
    the stripper erases a raw string's opening quote together with its
    prefix (round-11: the surviving quote lexed as a plain-string opener
    and swallowed the attribute run — an `#[ignore]` behind a raw-string
    doc attribute passed the gate, and raw-string `#[path]` values never
    wired end-to-end);
  - round-15 review: cfg-mutex inline twins of one module name with
    different `#[path]` directories no longer collapse onto a single
    (enclosing stack, name) slot — a conflicting repeat fails closed
    naming both declarations, instead of the later twin silently
    shadowing the platform-relevant real source; and a non-mod-rs file's
    own-`#[path]` inline module resolves against the FILE's directory
    with the pending file-stem cleared (the earlier fixture here pinned
    the stem-relative location rustc never loads — corrected in
    Round14InlineRedirectTests);
  - round-18 review: the explicit-`#[path]` exclusion from conventional
    probing is per declaration — (module name, declaration line) — so a
    cfg-mutex plain twin of a wired declaration keeps its conventional
    scan (name-keyed exclusion silenced exactly the branch rustc loads
    on the other host, green-lighting a pin `#[ignore]`d there);
  - round-19 review: the visited scan context carries the directory
    ownership alongside (realpath, module path) — a cfg-mutex wired/plain
    twin pair reaching one file under one module path differs only in
    ownership (children beside the file vs under `<stem>/`), and the
    FIFO race must not drop the branch rustc loads on the other host;
    and a declaration line hosting more same-name declarations than the
    `#[path]` run can wire is refused (ambiguous identity) instead of
    the keyed exclusion silencing the plain twin's probe;
  - round-20 review: hand-packed (rustfmt::skip) layouts the run parser
    cannot bind token-wise — a path run closing on the declaration's own
    line, a path value wrapped in `#[cfg_attr(...)]`, a path head
    mid-line after code, and a mod keyword whose name/terminator wraps —
    fail closed instead of letting a conventional same-name file
    fabricate the pin's attribution (each was a silent green: the run
    parser advanced past the packed declaration, no wired entry existed,
    so the round-19 refusal and the per-declaration exclusion never
    engaged); spacing between `#` and `[` and a leading UTF-8 BOM are
    modelled as ordinary wiring instead of silently dropping it;
  - main()'s duplicate-#[test] detection — the guard's original purpose —
    is pinned against a mocked `cargo test --list` registry.

`main()` shells out to cargo, so these tests exercise the pure filesystem
half: the source scanner (`ignored_source_pins`) and the pin attribution
(`_pin_ignored_locations`) against a synthetic crate tree, with the guard
module's MANIFEST pointed at it. Fixtures declare their module graph from
`lib.rs` the way the real crate does — the graph walk scopes the scan, so
undeclared files (bin roots, strays) must be invisible to it.
"""

from __future__ import annotations

import importlib.util
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "test-registry-guard.py"
SPEC = importlib.util.spec_from_file_location("test_registry_guard", SCRIPT)
assert SPEC and SPEC.loader
guard = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(guard)


def _write(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")


class GuardFixture(unittest.TestCase):
    """Builds a synthetic src-tauri crate tree and points the guard's
    MANIFEST at it, so the scanner and pin attribution run without cargo."""

    def _crate_root(self, files: dict[str, str]) -> Path:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        _write(root / "pinvou3-app" / "src-tauri" / "Cargo.toml", "")
        for rel, text in files.items():
            _write(root / "pinvou3-app" / "src-tauri" / "src" / rel, text)
        return root

    def _scan(self, root: Path):
        with patch.object(
            guard, "MANIFEST", str(root / "pinvou3-app/src-tauri/Cargo.toml")
        ):
            return guard.ignored_source_pins()

    def _ignored_sources(self, files: dict[str, str]):
        return self._scan(self._crate_root(files))

    def _src_path(self, root: Path, rel: str) -> str:
        return str(root / "pinvou3-app" / "src-tauri" / "src" / rel)


_LIVE_PIN = (
    "#[cfg(test)]\n"
    "mod tests {\n"
    "    #[test]\n"
    "    fn critical_pin() {\n"
    "        assert!(true);\n"
    "    }\n"
    "}\n"
)
_IGNORED_PIN = (
    "#[cfg(test)]\n"
    "mod tests {\n"
    "    #[test]\n"
    "    #[ignore = \"slow in CI\"]\n"
    "    fn critical_pin() {\n"
    "        assert!(true);\n"
    "    }\n"
    "}\n"
)
# The round-6 review reproduction: BOTH the live pin and a same-named
# sibling #[ignore]d fn live in ONE file, in sibling inline modules.
_SIBLING_TREE = {
    "lib.rs": "#[cfg(test)]\nmod real;\n",
    "real/mod.rs": "#[cfg(test)]\nmod module;\n",
    "real/module.rs": (
        "#[cfg(test)]\n"
        "mod tests {\n"
        "    #[test]\n"
        "    fn critical_pin() {\n"
        "        assert!(true);\n"
        "    }\n"
        "}\n"
        "\n"
        "#[cfg(test)]\n"
        "mod unrelated {\n"
        "    #[test]\n"
        "    #[ignore]\n"
        "    fn critical_pin() {\n"
        "        assert!(true);\n"
        "    }\n"
        "}\n"
    ),
}
_PIN = "real::module::tests::critical_pin"


class PinAttributionTests(GuardFixture):
    """Review #1518 rounds 4-6: the #[ignore] scan must attribute hits by the
    pin's full libtest module path — bare names (round 4) and source-file
    paths (round 5) both let a same-named ignored fn frame a live pin."""

    def test_sibling_inline_module_same_file_ignored_does_not_frame_pin(self):
        # The round-6 review reproduction, verbatim: `mod tests` and a
        # sibling `mod unrelated` live in the SAME file, both containing a
        # `critical_pin`; only the sibling's is #[ignore]d. The live pin
        # must stay silent, and the sibling hit must be keyed under its own
        # full path — proving attribution, not mere absence.
        root = self._crate_root(_SIBLING_TREE)
        by_path, unattributable = self._scan(root)
        self.assertEqual(
            guard._pin_ignored_locations(_PIN, by_path, unattributable),
            [],
        )
        self.assertEqual(sorted(by_path), ["real::module::unrelated::critical_pin"])
        self.assertEqual(
            [loc.rsplit(":", 1)[0] for loc in by_path["real::module::unrelated::critical_pin"]],
            [self._src_path(root, "real/module.rs")],
        )
        self.assertEqual(unattributable, {})

    def test_pin_inline_module_ignored_is_flagged_at_ignore_line(self):
        # The detection itself: the same layout with the ignore on the pin's
        # own function flags the pin, reporting the ignore-bearing line.
        root = self._crate_root(
            {
                "lib.rs": "#[cfg(test)]\nmod real;\n",
                "real/mod.rs": "#[cfg(test)]\nmod module;\n",
                "real/module.rs": _IGNORED_PIN,
            }
        )
        by_path, unattributable = self._scan(root)
        self.assertEqual(
            guard._pin_ignored_locations(_PIN, by_path, unattributable),
            [self._src_path(root, "real/module.rs") + ":4"],
        )

    def test_registered_pin_with_no_ignored_sources_stays_silent(self):
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": "#[cfg(test)]\nmod real;\n",
                "real/mod.rs": "#[cfg(test)]\nmod module;\n",
                "real/module.rs": _LIVE_PIN,
            }
        )
        self.assertEqual(
            guard._pin_ignored_locations(_PIN, by_path, unattributable),
            [],
        )

    def test_unparseable_file_hits_fail_closed_on_leaf_match(self):
        # A file the lexer cannot trust (unbalanced brace) has its hits
        # keyed by bare function name; any pin sharing the leaf is flagged —
        # the fail-closed fallback, narrowed to genuinely unattributable
        # hits. (Round 5's fallback keyed EVERY clean file this way; the
        # round-6 fix narrows it to files whose module stack is unknown.)
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": "#[cfg(test)]\nmod broken;\n",
                "broken.rs": (
                    "#[test]\n"
                    "#[ignore]\n"
                    "fn critical_pin() {\n"
                    "    assert!(true);\n"
                    "}\n"
                    "}\n"  # unbalanced: parse cannot be trusted
                ),
            }
        )
        self.assertNotEqual(
            guard._pin_ignored_locations(
                "nowhere::defined::critical_pin", by_path, unattributable
            ),
            [],
        )
        # Round-13: pin the fail-closed HALF explicitly — the hit must be
        # visible ONLY through the leaf net, never through a trusted full
        # path (otherwise a path-shape accident could satisfy a pin without
        # the unparseable-file suspicion).
        self.assertEqual(by_path, {})

    def test_bin_root_and_unreached_files_are_never_scanned(self):
        # The scan is scoped to the lib module graph from lib.rs: the bin
        # root and undeclared strays are not lib test sources, so a
        # same-named ignored fn there can neither alias a pin's path nor
        # trip the leaf fallback.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": _LIVE_PIN,
                "main.rs": _IGNORED_PIN,
                "stray.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(by_path, {})
        self.assertEqual(unattributable, {})


class ModulePathTests(GuardFixture):
    """The graph walk + inline-module stack must reproduce libtest paths for
    each real layout a REQUIRED_PIN uses."""

    def test_external_mod_chain_attributes_child_file(self):
        # app::commands::tests::pin — `tests` is an external child module of
        # commands.rs (the REQUIRED_PINS shape for commands); the child file
        # itself holds the fns, no `mod tests` wrapper.
        by_path, _ = self._ignored_sources(
            {
                "lib.rs": "mod app;\n",
                "app.rs": "pub mod commands;\n",
                "app/commands.rs": "#[cfg(test)]\nmod tests;\n",
                "app/commands/tests.rs": (
                    "#[test]\n"
                    "#[ignore = \"slow in CI\"]\n"
                    "fn critical_pin() {\n"
                    "    assert!(true);\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(
            list(by_path), ["app::commands::tests::critical_pin"]
        )

    def test_grandparent_file_layout_attributes_inline_tests(self):
        # features::marketplace::scope::tests::pin — `tests` is inline in
        # the file module scope.rs (the round-7 grandparent-file form), with
        # a sibling inline module carrying a same-named ignored fn: the pin
        # stays silent while the sibling is keyed apart.
        by_path, _ = self._ignored_sources(
            {
                "lib.rs": "mod features;\n",
                "features/mod.rs": "pub mod marketplace;\n",
                "features/marketplace/mod.rs": "pub mod scope;\n",
                "features/marketplace/scope.rs": (
                    "#[cfg(test)]\n"
                    "mod tests {\n"
                    "    #[test]\n"
                    "    fn critical_pin() {\n"
                    "        assert!(true);\n"
                    "    }\n"
                    "}\n"
                    "\n"
                    "#[cfg(test)]\n"
                    "mod stale_cache {\n"
                    "    #[test]\n"
                    "    #[ignore]\n"
                    "    fn critical_pin() {\n"
                    "        assert!(true);\n"
                    "    }\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(
            sorted(by_path),
            ["features::marketplace::scope::stale_cache::critical_pin"],
        )

    def test_path_attribute_wires_external_file(self):
        # `#[path = "..."] mod name;` wiring (12 real sites, e.g.
        # file_ingest.rs → visual_preview.rs): the wired file joins the lib
        # graph under the MOD name, and its inline test modules attribute
        # through it. The conventional candidate does not exist, so the
        # wiring is the only path in — deleting it would strand the file
        # (the old scanner's os.walk saw it; the graph must not lose it).
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": "mod features;\n",
                "features/mod.rs": "pub mod files;\n",
                "features/files/mod.rs": "pub mod file_ingest;\n",
                "features/files/file_ingest.rs": (
                    "#[path = \"visual_preview.rs\"]\nmod visual_preview;\n"
                ),
                "features/files/visual_preview.rs": (
                    "#[cfg(test)]\n"
                    "mod visual_preview_smoke {\n"
                    "    #[test]\n"
                    "    #[ignore = \"需要 libreoffice\"]\n"
                    "    fn renders_both_paths() {\n"
                    "    }\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(unattributable, {})
        self.assertEqual(
            list(by_path),
            [
                "features::files::file_ingest::visual_preview::"
                "visual_preview_smoke::renders_both_paths"
            ],
        )

    def test_path_value_behind_quote_bearing_comment_is_wired(self):
        # Round-10 review reproduction (comment 4292115820), positive
        # control: the first `"` after the attribute head sits INSIDE a
        # block comment, and the old raw first-quote scan wired the module
        # to the comment's decoy path — the real file silently left the lib
        # graph. The value must be read only from a string token the lexer
        # vouches for. The target deliberately does NOT match the mod name
        # (no conventional src/<name>.rs candidate), so the `#[path]`
        # wiring is the only route in — a decoy read cannot be masked by
        # the conventional resolution.
        root = self._crate_root(
            {
                "lib.rs": (
                    "#[path = /* \"decoy.rs\" */ \"real_target.rs\"]\n"
                    "mod wired;\n"
                ),
                "real_target.rs": _IGNORED_PIN,
            }
        )
        by_path, unattributable = self._scan(root)
        self.assertEqual(sorted(by_path), ["wired::tests::critical_pin"])
        self.assertEqual(unattributable, {})

    def test_unparseable_path_value_fails_closed_not_silently_skipped(self):
        # Round-10 review, the no-silent-skip half: a `#[path]` head whose
        # value cannot be reliably parsed must abort the guard loudly — the
        # old behavior quietly left the module out of the scan, which is
        # exactly the fail-open direction for a pin silenced in that file.
        # Round-13: pin the MESSAGE too — any SystemExit from any source
        # satisfied the bare assertRaises, so a regression that aborted for
        # the wrong reason (or a refactored raise site) passed unnoticed.
        with self.assertRaises(SystemExit) as ctx:
            self._ignored_sources(
                {
                    "lib.rs": (
                        "#[path = \"unterminated.rs\n"
                        "mod wired;\n"
                    ),
                    "unterminated.rs": _IGNORED_PIN,
                }
            )
        self.assertIn("cannot read the #[path] value", str(ctx.exception))

    def test_path_attribute_inside_inline_module_is_wired(self):
        # Round-12 review (Jensen P2, comment 4297148007), positive control:
        # `mod outer { #[path = "real.rs"] mod routed; }` is legal Rust and
        # rustc loads `src/outer/real.rs` as `outer::routed` (resolution
        # verified against rustc: the declaring mod-rs root's directory plus
        # the inline-module components). The pre-round-12 scanner silently
        # left the file out of the graph, so an #[ignore] on a REQUIRED_PIN
        # there passed the gate — the guard now follows the wiring. The
        # target deliberately does NOT match the mod name, so the wiring is
        # the only route in.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "mod outer {\n"
                    "    #[path = \"real.rs\"]\n"
                    "    mod routed;\n"
                    "}\n"
                ),
                "outer/real.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(sorted(by_path), ["outer::routed::tests::critical_pin"])
        self.assertEqual(unattributable, {})

    def test_inline_module_path_wiring_in_non_mod_rs_file_is_wired(self):
        # Round-12, the non-mod-rs half of the rustc rule: in `src/a/b.rs`
        # (module a::b), an inline module's #[path] child resolves against
        # `src/a/b/` plus the inline-module components — rustc verified:
        # `mod s { #[path = "x.rs"] mod m; }` loads `src/a/b/s/x.rs`.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": "mod a;\n",
                "a.rs": "pub mod b;\n",
                "a/b.rs": (
                    "pub mod s {\n"
                    "    #[path = \"real.rs\"]\n"
                    "    pub mod m;\n"
                    "}\n"
                ),
                "a/b/s/real.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(
            sorted(by_path), ["a::b::s::m::tests::critical_pin"]
        )
        self.assertEqual(unattributable, {})

    def test_attribute_between_path_and_declaration_does_not_break_wiring(self):
        # Round-12 review: the idiomatic platform-wiring shape puts a cfg
        # attribute between the path attribute and the `mod` declaration
        # (`#[path = "..."]` + `#[cfg(unix)]`). The old single-entry pairing
        # silently dropped exactly this wiring while the `#[cfg]`-first
        # order worked; a path value stays pending across the run's
        # attribute entries until the declaration (or a non-attribute item)
        # ends it.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "#[path = \"wired/real_target.rs\"]\n"
                    "#[cfg(unix)]\n"
                    "mod pinned_mod;\n"
                ),
                "wired/real_target.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(sorted(by_path), ["pinned_mod::tests::critical_pin"])
        self.assertEqual(unattributable, {})

    def test_path_run_ended_by_non_attribute_item_drops_pending_value(self):
        # The pending value must not leak past the run: rustc attaches the
        # path attribute to the next item, and a `fn` there means the
        # "wiring" never was one — no module may enter the graph through it
        # (the stray file stays unscanned; a pin living there is caught by
        # main()'s source-attribution net, not by phantom scanning).
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "#[path = \"stray_target.rs\"]\n"
                    "fn not_a_mod() {}\n"
                ),
                "stray_target.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(by_path, {})
        self.assertEqual(unattributable, {})

    def test_next_line_brace_inline_module_is_attributed(self):
        # rustfmt permits the brace on its own line; the lookahead must
        # still push the module before any hit inside it.
        by_path, _ = self._ignored_sources(
            {
                "lib.rs": (
                    "#[cfg(test)]\n"
                    "mod tests\n"
                    "{\n"
                    "    #[test]\n"
                    "    #[ignore]\n"
                    "    fn critical_pin() {\n"
                    "    }\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(list(by_path), ["tests::critical_pin"])

    def test_nested_inline_modules_disambiguate_same_name(self):
        # Same file, same fn name at two different inline depths: each full
        # path keys exactly its own location.
        by_path, _ = self._ignored_sources(
            {
                "lib.rs": (
                    "mod a {\n"
                    "    #[test]\n"
                    "    #[ignore]\n"
                    "    fn critical_pin() {\n"
                    "    }\n"
                    "}\n"
                    "\n"
                    "mod b {\n"
                    "    mod tests {\n"
                    "        #[test]\n"
                    "        #[ignore]\n"
                    "        fn critical_pin() {\n"
                    "        }\n"
                    "    }\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(
            sorted(by_path),
            ["a::critical_pin", "b::tests::critical_pin"],
        )
        self.assertEqual(len(by_path["a::critical_pin"]), 1)
        self.assertEqual(len(by_path["b::tests::critical_pin"]), 1)


class LexerRobustnessTests(GuardFixture):
    """Braces inside non-code constructs must not skew the module stack, and
    constructs the lexer cannot trust must be reported as unclean."""

    def test_char_literals_comments_and_strings_do_not_skew_paths(self):
        # Everything below appears in the real crate: `s.find('{')` char
        # literals (connector_cli.rs), `// }` line comments, `/* { */` block
        # comments, brace-y strings, lifetimes and loop labels. None may
        # shift the inline-module stack of what follows.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "fn lexer_hostile<'a>(s: &str) -> Option<usize> {\n"
                    "    let open = s.find('{')?;\n"  # char literal brace
                    "    let close = s.rfind('}');\n"
                    "    // comment with a stray } brace\n"
                    "    /* block comment { with braces } */\n"
                    "    let label = 'outer: {\n"
                    "        let msg = \"}} not code\";\n"
                    "        open + msg.len()\n"
                    "    };\n"
                    "    Some(open)\n"
                    "}\n"
                    "\n"
                    "#[cfg(test)]\n"
                    "mod tests {\n"
                    "    #[test]\n"
                    "    #[ignore]\n"
                    "    fn critical_pin() {\n"
                    "    }\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(unattributable, {})
        self.assertEqual(list(by_path), ["tests::critical_pin"])

    def test_raw_string_and_nested_block_comment_are_transparent(self):
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "const PATTERN: &str = r#\"mod fake {\n"
                    "    fn critical_pin() {}\n"
                    "}\"#;\n"
                    "\n"
                    "/* outer /* nested { */ still outer } */\n"
                    "#[cfg(test)]\n"
                    "mod tests {\n"
                    "    #[test]\n"
                    "    #[ignore]\n"
                    "    fn critical_pin() {\n"
                    "    }\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(unattributable, {})
        self.assertEqual(list(by_path), ["tests::critical_pin"])

    def test_unterminated_string_makes_whole_file_fail_closed(self):
        # Round-10 same-class hardening of the lexer-trust check: a string
        # that never closes used to leave the file "clean" while every
        # following line was swallowed as string content — a well-formed
        # hit before it surfaced as a trusted full path. Untrusted parse
        # must fail closed on the bare fn name instead.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "#[test]\n"
                    "#[ignore]\n"
                    "fn critical_pin() {\n"
                    "}\n"
                    "\n"
                    "let tail = \"this string never closes\n"
                ),
            }
        )
        self.assertEqual(by_path, {})
        self.assertIn("critical_pin", unattributable)


class ScannerParsingTests(GuardFixture):
    """Attribute-run parsing forms the round-6 rewrite claims to cover,
    each pinned so it cannot regress unnoticed."""

    def _ignored(self, text: str) -> dict:
        by_path, _ = self._ignored_sources({"lib.rs": text})
        return by_path

    def test_multi_line_cfg_attr_ignore_is_detected(self):
        ignored = self._ignored(
            "#[cfg_attr(\n"
            "    feature = \"heavy\",\n"
            "    ignore\n"
            ")]\n"
            "fn wrapped_cfg() {\n"
            "}\n"
        )
        self.assertIn("wrapped_cfg", ignored)

    def test_pub_async_unsafe_and_generic_signatures_are_detected(self):
        ignored = self._ignored(
            "#[test]\n"
            "#[ignore]\n"
            "pub async fn skipped_async() {\n"
            "}\n"
            "\n"
            "#[test]\n"
            "#[ignore]\n"
            "pub unsafe fn skipped_unsafe() {\n"
            "}\n"
            "\n"
            "#[test]\n"
            "#[ignore]\n"
            "fn skipped_generic<T: Clone>(value: T) {\n"
            "}\n"
        )
        for name in ("skipped_async", "skipped_unsafe", "skipped_generic"):
            self.assertIn(name, ignored)

    def test_rustfmt_wrapped_signature_is_detected(self):
        ignored = self._ignored(
            "#[test]\n"
            "#[ignore]\n"
            "fn skipped_wrapped(\n"
            "    first: usize,\n"
            "    second: usize,\n"
            ") {\n"
            "}\n"
        )
        self.assertIn("skipped_wrapped", ignored)

    def test_rustfmt_wrapped_generic_signature_is_detected(self):
        # Round-12 review: rustfmt wraps a long generic parameter list so
        # the parameter `(` opens on a later line (`fn g<\n    T: ...,\n>(
        # `). Requiring the paren on the name's line silently dropped the
        # hit with no fail-closed fallback; the scan must follow the
        # signature to its paren.
        ignored = self._ignored(
            "#[test]\n"
            "#[ignore = \"wrapped generics\"]\n"
            "fn long_name_with_generics<\n"
            "    T: Clone + Send + Sync + Static,\n"
            ">(\n"
            "    value: T,\n"
            ") {\n"
            "}\n"
        )
        self.assertIn("long_name_with_generics", ignored)

    def test_wrapped_signature_stops_at_body_brace(self):
        # The lookahead must not jump across a body: a `fn`-looking line
        # whose `{` (or `;`) comes before any `(` is not a function this
        # run attaches to — no hit may be fabricated for it (rustc rejects
        # the shape outright, so a missed hit cannot go live either).
        ignored = self._ignored(
            "#[test]\n"
            "#[ignore]\n"
            "fn not_a_signature<\n"
            "    T: Clone,\n"
            "> where T: Static {\n"
            "    let x = 1;\n"
            "}\n"
        )
        self.assertEqual(ignored, {})

    def test_ignore_inside_string_literal_is_not_a_hit(self):
        # `ignore` is judged by tokens outside strings: a cfg_attr whose
        # string argument merely contains the word must not flag the
        # function, while a bracketed ignore reason still terminates the
        # attribute correctly.
        ignored = self._ignored(
            "#[cfg_attr(feature = \"ignore-me\", expect(unused))]\n"
            "fn string_word_only() {\n"
            "}\n"
            "\n"
            "#[ignore = \"kept for a[b] manual runs\"]\n"
            "fn bracketed_reason() {\n"
            "}\n"
        )
        self.assertIn("bracketed_reason", ignored)
        self.assertNotIn("string_word_only", ignored)

    def test_comment_mentioning_ignore_does_not_smear_onto_next_fn(self):
        ignored = self._ignored(
            "// disabled on CI: #[ignore was considered here\n"
            "fn commented_neighbor() {\n"
            "}\n"
            "\n"
            "#[test]\n"
            "// decided against #[ignore for this one\n"
            "fn commented_inside_run() {\n"
            "}\n"
        )
        self.assertNotIn("commented_neighbor", ignored)
        self.assertNotIn("commented_inside_run", ignored)

    def test_comment_between_ignore_and_fn_does_not_hide_hit(self):
        # Round-9 review reproduction, positive control (comment 4291331727):
        # Rust comments do NOT sever an attribute from its item, so a pin
        # silenced behind comment lines — after `#[ignore]` or mid-run —
        # must still be flagged. The pre-round-9 scanner read raw lines and
        # dropped the run at the first comment, so a disabled critical
        # regression passed the gate on a mock libtest manifest.
        root = self._crate_root(
            {
                "lib.rs": (
                    "#[test]\n"
                    "#[ignore]\n"
                    "// temporary quarantine — restore before next release\n"
                    "fn critical_pin() {\n"
                    "}\n"
                    "\n"
                    "#[test]\n"
                    "// mid-run note\n"
                    "#[ignore = \"flaky, see #999\"]\n"
                    "fn second_pin() {\n"
                    "}\n"
                ),
            }
        )
        by_path, unattributable = self._scan(root)
        src = self._src_path(root, "lib.rs")
        self.assertEqual(
            by_path,
            {"critical_pin": [f"{src}:2"], "second_pin": [f"{src}:9"]},
        )
        self.assertEqual(unattributable, {})

    def test_escaped_quote_in_ignore_reason_is_detected(self):
        # Round-10 review reproduction (comment 4292115811), positive
        # control: the `\"` escape used to leave its escaped quote in the
        # lexed text, which the attribute parser re-read as an unclosed
        # string and swallowed the fn — a silenced pin passed the gate
        # (exit 0 on a mock libtest manifest). The escaped character must
        # be blanked together with the backslash.
        root = self._crate_root(
            {
                "lib.rs": (
                    "#[test]\n"
                    "#[ignore = \"one quote: \\\"\"]\n"
                    "fn critical_pin() {\n"
                    "}\n"
                ),
            }
        )
        by_path, unattributable = self._scan(root)
        self.assertEqual(
            by_path,
            {"critical_pin": [self._src_path(root, "lib.rs") + ":2"]},
        )
        self.assertEqual(unattributable, {})

    def test_block_comment_pseudo_ignore_and_fn_do_not_frame_live_test(self):
        # Round-9 review reproduction, negative control: a block comment
        # containing `#[ignore]` and a same-named pseudo function must not be
        # attributed to the real live test defined after it — the pre-round-9
        # scanner read the commented text as an attribute run bound to the
        # live fn and failed main() on a healthy tree.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "/*\n"
                    "#[ignore]\n"
                    "fn critical_pin() {}\n"
                    "*/\n"
                    "#[test]\n"
                    "fn critical_pin() {\n"
                    "}\n"
                ),
            }
        )
        self.assertEqual(by_path, {})
        self.assertEqual(unattributable, {})

    def test_commented_out_attribute_and_path_wiring_are_invisible(self):
        # Same round-9 root cause on the graph-walk side: a commented-out
        # `#[path]` wiring must not pull a file into the scan, and a comment
        # between a live `#[path]` attribute and its `mod` declaration must
        # not break the wiring (rustc keeps them attached). The live target
        # deliberately does not match its mod name, so the `#[path]` wiring
        # is the only route in and the test cannot pass via conventional
        # resolution.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "// #[path = \"ghost.rs\"]\n"
                    "// mod ghost;\n"
                    "\n"
                    "#[path = \"wired_target.rs\"]\n"
                    "// wired through a comment\n"
                    "mod wired;\n"
                ),
                "ghost.rs": _IGNORED_PIN,
                "wired_target.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(sorted(by_path), ["wired::tests::critical_pin"])
        self.assertEqual(unattributable, {})


class Round11LexerBoundaryTests(GuardFixture):
    """Review #1518 round-11: the value reader must always terminate (a
    multi-line `#[path]` value used to spin the scan forever), and the
    stripper must erase a raw string's opening quote together with its
    prefix (its survival lexed as a plain-string opener and swallowed
    everything behind it)."""

    def test_path_value_on_following_line_is_wired(self):
        # `#[path =` with the value on the next line behind a comment: the
        # first line yields no string, so the scan must advance — the
        # pre-round-11 reader re-scanned the same line forever and hung the
        # guard process.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "#[path =\n"
                    "// value wrapped to the next line\n"
                    "\"wrapped_target.rs\"]\n"
                    "mod wrapped;\n"
                ),
                "wrapped_target.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(sorted(by_path), ["wrapped::tests::critical_pin"])
        self.assertEqual(unattributable, {})

    def test_path_value_behind_multiline_block_comment_is_wired(self):
        # Same termination property through the block-comment branch: the
        # comment consumes the rest of the attribute line(s) and the value
        # follows once it closes.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "#[path = /* block comment\n"
                    "   spanning lines */ \"block_target.rs\"]\n"
                    "mod blocked;\n"
                ),
                "block_target.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(sorted(by_path), ["blocked::tests::critical_pin"])
        self.assertEqual(unattributable, {})

    def test_raw_string_attribute_does_not_hide_following_ignore(self):
        # The raw-string branch used to blank the body but leave the
        # opening quote in the stripped text; the attribute parser read it
        # as a plain-string opener and swallowed the rest of the run — a
        # pin silenced behind a `#[doc = r#"..."#]` passed the gate. The
        # reported location is the ignore-bearing line itself.
        root = self._crate_root(
            {
                "lib.rs": (
                    "#[doc = r#\"raw doc with [brackets] and \" quotes\"#]\n"
                    "#[test]\n"
                    "#[ignore]\n"
                    "fn critical_pin() {\n"
                    "}\n"
                ),
            }
        )
        by_path, unattributable = self._scan(root)
        self.assertEqual(
            by_path,
            {"critical_pin": [self._src_path(root, "lib.rs") + ":3"]},
        )
        self.assertEqual(unattributable, {})

    def test_raw_string_path_value_is_wired(self):
        # The documented raw-string form must wire end-to-end: the value
        # reader reads the raw payload, and the erased opening quote lets
        # the attribute parser find the declaration, so the file joins the
        # lib graph under its mod name.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "#[path = r#\"raw_target.rs\"#]\n"
                    "mod rawwired;\n"
                ),
                "raw_target.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(sorted(by_path), ["rawwired::tests::critical_pin"])
        self.assertEqual(unattributable, {})


class DuplicateRegistrationTests(unittest.TestCase):
    """main()'s duplicate-#[test] detection — the guard's original purpose
    (review #455 rounds 26/28) — previously had no committed regression:
    these run the real main() against a mocked `cargo test --list`
    registry, so the duplicate half is covered without cargo too. Round-12
    adds the composition layer: main()'s headline "pin is #[ignore]d"
    failure path, the relocated/absent message branches, the argv guard,
    the cargo invocation contract, and the source-attribution net."""

    def _run_main(self, paths, source="", required=None, files=None):
        import contextlib
        import io
        import sys

        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            _write(root / "pinvou3-app" / "src-tauri" / "Cargo.toml", "")
            _write(root / "pinvou3-app" / "src-tauri" / "src" / "lib.rs", source)
            for rel, text in (files or {}).items():
                _write(root / "pinvou3-app" / "src-tauri" / "src" / rel, text)
            stderr = io.StringIO()
            pins = (
                list(guard.REQUIRED_PINS) if required is None else list(required)
            )
            with patch.object(
                guard, "MANIFEST", str(root / "pinvou3-app/src-tauri/Cargo.toml")
            ), patch.object(
                guard, "list_tests", return_value=list(paths)
            ), patch.object(
                guard, "REQUIRED_PINS", pins
            ), patch.object(
                sys, "argv", ["test-registry-guard"]
            ), contextlib.redirect_stderr(stderr):
                code = guard.main()
        return code, stderr.getvalue()

    def test_same_path_registered_twice_fails(self):
        # The failure the guard exists for: one path registered twice means
        # a duplicated attribute silenced a neighbouring test while
        # inflating the run count.
        code, err = self._run_main(["a::tests::dup", "a::tests::dup"])
        self.assertEqual(code, 1)
        self.assertIn("duplicate test registrations", err)
        self.assertIn("a::tests::dup x2", err)

    def test_required_pin_registered_more_than_once_has_dedicated_message(self):
        # Round-13: main()'s pin-specific "registered more than once"
        # branch was the only main() branch with no committed regression —
        # deleting it degraded the message to the generic duplicates line
        # while every test stayed green. Pin the dedicated message so the
        # branch is load-bearing.
        code, err = self._run_main(
            ["a::tests::dup", "a::tests::dup"],
            source=(
                "#[cfg(test)]\n"
                "mod tests {\n"
                "    #[test]\n"
                "    fn dup() {}\n"
                "}\n"
            ),
            required=["a::tests::dup"],
        )
        self.assertEqual(code, 1)
        self.assertIn(
            "required pin registered more than once: a::tests::dup x2", err
        )

    def test_same_name_in_distinct_modules_is_not_a_duplicate(self):
        # Full-path keying: two modules may each carry a same-named test —
        # libtest registers both separately. With the required pin present
        # AND its source attributable (round-12: the registry half alone no
        # longer satisfies main() — the fixture declares the real fns) the
        # guard must pass (the bare-name era would have red here).
        source = (
            "mod a {\n"
            "    mod tests {\n"
            "        #[test]\n"
            "        fn dup() {}\n"
            "    }\n"
            "}\n"
            "mod b {\n"
            "    mod tests {\n"
            "        #[test]\n"
            "        fn dup() {}\n"
            "    }\n"
            "}\n"
        )
        code, err = self._run_main(
            ["a::tests::dup", "b::tests::dup"],
            source=source,
            required=["a::tests::dup"],
        )
        self.assertEqual(code, 0)
        self.assertNotIn("duplicate", err)

    def test_main_reports_ignored_pin_end_to_end(self):
        # Round-12 composition net (fresh review L3): the scan half
        # (ignored_source_pins) and the attribution half
        # (_pin_ignored_locations) were each covered in isolation, but
        # main()'s headline failure path — the reason the guard exists —
        # was never exercised end to end. A wiring regression inside
        # main() must not pass silently.
        source = (
            "mod a {\n"
            "    mod tests {\n"
            "        #[test]\n"
            '        #[ignore = "quarantined"]\n'
            "        fn critical_pin() {}\n"
            "    }\n"
            "}\n"
        )
        code, err = self._run_main(
            ["a::tests::critical_pin"],
            source=source,
            required=["a::tests::critical_pin"],
        )
        self.assertEqual(code, 1)
        self.assertIn(
            "required pin a::tests::critical_pin is #[ignore]d at", err
        )
        self.assertIn("lib.rs:4", err)

    def test_main_reports_relocated_pin_with_new_location(self):
        # A pin missing from the registry whose leaf still appears exactly
        # once must name the new location (module move, not theft).
        source = (
            "mod moved {\n"
            "    mod tests {\n"
            "        #[test]\n"
            "        fn critical_pin() {}\n"
            "    }\n"
            "}\n"
        )
        code, err = self._run_main(
            ["moved::tests::critical_pin"],
            source=source,
            required=["old::tests::critical_pin"],
        )
        self.assertEqual(code, 1)
        self.assertIn("moved modules", err)
        self.assertIn("moved::tests::critical_pin", err)

    def test_main_reports_absent_pin_as_stolen_attribute(self):
        code, err = self._run_main([], required=["old::tests::critical_pin"])
        self.assertEqual(code, 1)
        self.assertIn("absent from the registry", err)
        self.assertIn("old::tests::critical_pin", err)

    def test_main_fails_closed_on_registered_pin_with_unscannable_source(self):
        # Round-12 source-attribution net: a listed pin whose declaring
        # file the lib module graph never reaches (here the wired target
        # does not exist on disk) would hide any #[ignore] — the guard
        # refuses to trust the registry entry instead of passing.
        source = "#[path = \"nowhere/real.rs\"]\nmod wired;\n"
        code, err = self._run_main(
            ["wired::tests::critical_pin"],
            source=source,
            required=["wired::tests::critical_pin"],
        )
        self.assertEqual(code, 1)
        self.assertIn("outside the scanned lib module graph", err)
        self.assertIn("wired::tests::critical_pin", err)

    def test_main_rejects_arguments(self):
        import sys

        with patch.object(sys, "argv", ["test-registry-guard", "--flag"]):
            with self.assertRaises(SystemExit) as ctx:
                guard.main()
        self.assertIn("takes no arguments", str(ctx.exception))

    def test_list_tests_invokes_locked_no_default_features_cargo(self):
        # Round-12: the cargo invocation is a contract — same manifest,
        # --no-default-features and --locked as the build step right before
        # it (artifact reuse), terse listing, color pinned off.
        captured = {}

        class FakeResult:
            returncode = 0
            stdout = ""
            stderr = ""

        def fake_run(cmd, capture_output, text, env):
            captured["cmd"] = cmd
            captured["env"] = env
            return FakeResult()

        with tempfile.TemporaryDirectory() as td:
            manifest = str(
                Path(td) / "pinvou3-app" / "src-tauri" / "Cargo.toml"
            )
            with patch.object(guard, "MANIFEST", manifest), patch.object(
                guard.subprocess, "run", side_effect=fake_run
            ):
                with self.assertRaises(SystemExit):
                    # zero parsed tests -> fail closed AFTER the call
                    guard.list_tests()
        self.assertEqual(captured["cmd"][:2], ["cargo", "test"])
        self.assertIn("--locked", captured["cmd"])
        self.assertIn("--no-default-features", captured["cmd"])
        self.assertEqual(captured["env"]["CARGO_TERM_COLOR"], "never")
        # Round-13: the registry scope and the artifact-reuse contract ride
        # on two more argv details the old assertions did not pin — `--lib`
        # (dropping it silently mixes integration-test targets into the
        # registry) and the manifest path matching the MANIFEST the guard
        # was pointed at (a swapped manifest would list a different crate's
        # tests while every other assertion here stays green).
        self.assertIn("--lib", captured["cmd"])
        self.assertIn(str(Path(manifest)), captured["cmd"])


class SameLineInlineModuleTests(GuardFixture):
    """Round-12: an external `mod x;` sharing its line with the inline
    module that opens there (`mod s { mod x; }`) must resolve against the
    stack WITH `s` — the line-start snapshot resolved it one directory too
    high, where a stray file could alias hits into wrong paths."""

    def test_same_line_inline_external_resolves_inside_module(self):
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": "mod a;\n",
                "a.rs": "mod s { mod child; }\n",
                "a/s/child.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(sorted(by_path), ["a::s::child::tests::critical_pin"])
        self.assertEqual(unattributable, {})

    def test_stray_file_at_wrong_level_is_not_scanned(self):
        # The stray file the old stack snapshot WOULD have resolved (`a/child.rs`)
        # is never compiled by rustc and must stay invisible to the graph.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": "mod a;\n",
                "a.rs": "mod s { mod child; }\n",
                "a/child.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(by_path, {})
        self.assertEqual(unattributable, {})


class Round13HardeningTests(GuardFixture):
    """Round-13 review: branch-level anchors the earlier rounds left
    unpinned — the EOF-unclean lexer states other than an unterminated
    plain string, the `;`-stop of the rustfmt-signature lookahead, the
    hashless raw-string `#[path]` value form, qualifier prefixes beyond
    `pub/async/unsafe`, wired-file child ownership (the round-13
    `<stem>/` model was corrected to rustc's mod.rs-like `#[path]`
    semantics in round-16), and the loud
    abort when `src/lib.rs` is missing."""

    def test_hit_before_unterminated_block_comment_fails_closed(self):
        # The hit is recorded BEFORE the opener, so the leaf-net route is
        # exercised: an unclean-at-EOF file (block_depth stuck > 0) must
        # keep its hits untrusted — attributed to the bare name only, never
        # to a full libtest path (the in_str variant was pinned in round
        # 10; this is the block-comment sibling of that branch).
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": "#[cfg(test)]\nmod broken;\n",
                "broken.rs": (
                    "#[test]\n"
                    "#[ignore]\n"
                    "fn critical_pin() {}\n"
                    "/* never closed\n"
                ),
            }
        )
        self.assertEqual(by_path, {})
        self.assertNotEqual(unattributable.get("critical_pin"), [])

    def test_hit_before_unterminated_raw_string_fails_closed(self):
        # Same contract for the third EOF-unclean state (raw_hashes stuck
        # non-None): the recorded hit must land in the leaf net, not the
        # trusted path map.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": "#[cfg(test)]\nmod broken;\n",
                "broken.rs": (
                    "#[test]\n"
                    "#[ignore]\n"
                    "fn critical_pin() {}\n"
                    'const S: &str = r#"never closed\n'
                ),
            }
        )
        self.assertEqual(by_path, {})
        self.assertNotEqual(unattributable.get("critical_pin"), [])

    def test_signature_stopped_by_semicolon_is_not_attached(self):
        # A wrapped signature whose first structural stop is `;` (trait
        # method shape) must not capture the hit into a foreign fn — the
        # run stays unattached, and main()'s source-attribution net fails
        # closed on a pin in that position.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "#[test]\n"
                    "#[ignore]\n"
                    "fn trait_shaped<T>\n"
                    "where\n"
                    "    T: LoooongBound;\n"
                    "#[test]\n"
                    "fn later() {}\n"
                )
            }
        )
        self.assertEqual(by_path, {})
        self.assertEqual(unattributable, {})

    def test_hashless_raw_string_path_value_is_wired(self):
        # Round-11 pinned the `r#"..."#` form end-to-end; the hashless
        # `r"..."` form takes the same value-reader branch with zero
        # hashes and must wire identically (round-13: the branch had no
        # committed anchor).
        root = self._crate_root(
            {
                "lib.rs": '#[path = r"real_target.rs"]\nmod rawwired;\n',
                "real_target.rs": _IGNORED_PIN,
            }
        )
        by_path, unattributable = self._scan(root)
        self.assertEqual(sorted(by_path), ["rawwired::tests::critical_pin"])
        self.assertEqual(unattributable, {})

    def test_const_fn_qualifier_is_attributable(self):
        # Round-13 widened the fn-line regex to the full qualifier set
        # (rustc's canonical order is pub/default/const/async/unsafe/
        # extern — `#[test] pub const fn` is legal and compiles, verified
        # against rustc). The old prefix chain silently dropped `const fn`
        # on the floor, making such a pin fail closed instead of scanned.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "#[cfg(test)]\n"
                    "mod tests {\n"
                    "    #[test]\n"
                    "    #[ignore = \"slow in CI\"]\n"
                    "    pub const fn critical_pin() {}\n"
                    "}\n"
                )
            }
        )
        self.assertEqual(
            sorted(by_path), ["tests::critical_pin"]
        )
        self.assertEqual(unattributable, {})

    def test_escaped_char_literal_does_not_offset_attribution(self):
        # `'\u{7d}'` is the docstring's named char-literal case: its braces
        # must not count toward the inline-module stack, or the pin's path
        # gains a phantom component and the attribution misses.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "const BRACE: char = '\\u{7d}';\n"
                    "#[cfg(test)]\n"
                    "mod tests {\n"
                    "    #[test]\n"
                    "    #[ignore = \"slow in CI\"]\n"
                    "    fn critical_pin() {}\n"
                    "}\n"
                )
            }
        )
        self.assertEqual(sorted(by_path), ["tests::critical_pin"])
        self.assertEqual(unattributable, {})

    def test_wired_file_children_resolve_beside_the_file(self):
        # Round-13 pinned the wrong rustc model here (children under the
        # `<stem>/` directory); round-16 corrected it with rustc 1.99
        # probes both ways: mod_file_path treats every `#[path]` file as
        # mod.rs-like (DirOwnership Owned{relative: None}), so
        # `#[path = "main.rs"] mod entry;` + `mod inner;` loads the
        # SIBLING src/inner.rs — the stem-directory file `main/inner.rs`
        # is not a lib source at all, and the live twin sitting there
        # must not satisfy the scan. The location is compared EXACTLY
        # against `_src_path` (round-17 review): a hardcoded-slash
        # `endswith` rejected the scanner's legitimate backslash output
        # on Windows and the `"/main/"` decoy exclusion went blind to it.
        files = {
            "lib.rs": '#[path = "main.rs"]\nmod entry;\n',
            "main.rs": "mod inner;\n",
            "inner.rs": _IGNORED_PIN,
            "main/inner.rs": _LIVE_PIN,
        }
        root = self._crate_root(files)
        by_path, unattributable = self._scan(root)
        self.assertEqual(sorted(by_path), ["entry::inner::tests::critical_pin"])
        self.assertEqual(unattributable, {})
        self.assertEqual(
            by_path["entry::inner::tests::critical_pin"],
            [self._src_path(root, "inner.rs") + ":4"],
        )

    def test_wired_file_stem_directory_is_not_a_lib_source(self):
        # Reverse control for the corrected rule: an #[ignore] at the
        # stem-directory twin (the unwired leftover the round-13 scanner
        # actually read) cannot frame the pin while the real sibling
        # source is live — that decoy never enters the scan.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": '#[path = "main.rs"]\nmod entry;\n',
                "main.rs": "mod inner;\n",
                "inner.rs": _LIVE_PIN,
                "main/inner.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(by_path, {})
        self.assertEqual(unattributable, {})

    def test_missing_lib_rs_aborts_loudly(self):
        # The scan scopes itself to src/lib.rs; when the manifest points at
        # a crate without one the guard must abort instead of silently
        # scanning nothing (every pin would fail the attribution net with a
        # misleading message).
        with self.assertRaises(SystemExit) as ctx:
            self._ignored_sources({"main.rs": _IGNORED_PIN})
        self.assertIn("src/lib.rs not found", str(ctx.exception))


class Round14InlineRedirectTests(GuardFixture):
    """Round-14 review P2 (comment 4303787402): a `#[path]` on the inline
    module itself redirects the directory everything nested inside resolves
    against (the Rust Reference's thread_files form). The scanner dropped
    that attribute, resolved nested wirings against the module NAME's
    directory, and — when an unwired leftover file existed at that
    unredirected location with the same full module path — attributed the
    pin to the WRONG physical file: a silenced real source passed as
    attributable and main() returned 0 (the review's four-fixture matrix,
    fixture 3). The fix models the redirect per rustc; every test here
    plants a decoy at the unredirected location so the model — not the
    fail-closed net — is what decides the outcome."""

    _LIB = (
        '#[path = "real_dir"]\n'
        "mod outer {\n"
        '    #[path = "real.rs"]\n'
        "    mod routed;\n"
        "}\n"
    )
    _PIN = "outer::routed::tests::critical_pin"
    # Reuse DuplicateRegistrationTests' main() runner (its crate-tree
    # writer takes extra files now) without inheriting its test methods.
    _run_main = DuplicateRegistrationTests._run_main

    def test_redirected_real_source_is_scanned_not_the_decoy(self):
        # The review reproduction, verbatim: the real source lives in
        # src/real_dir/ (the redirected directory) and carries the
        # #[ignore]; the leftover src/outer/real.rs holds a live fn of the
        # same full path. The pin must be flagged AT THE REAL FILE — the
        # decoy must neither silence the hit nor satisfy attribution.
        root = self._crate_root(
            {
                "lib.rs": self._LIB,
                "real_dir/real.rs": _IGNORED_PIN,
                "outer/real.rs": _LIVE_PIN,
            }
        )
        by_path, unattributable = self._scan(root)
        self.assertEqual(sorted(by_path), [self._PIN])
        self.assertEqual(
            [
                loc.rsplit(":", 1)[0]
                for loc in by_path[self._PIN]
            ],
            [self._src_path(root, "real_dir/real.rs")],
        )
        self.assertEqual(
            guard._pin_ignored_locations(self._PIN, by_path, unattributable),
            [self._src_path(root, "real_dir/real.rs") + ":4"],
        )
        self.assertEqual(unattributable, {})

    def test_decoy_location_is_outside_the_graph_entirely(self):
        # The reverse half: with the real (redirected) source live, the
        # unwired leftover is NOT a lib source — an #[ignore] planted
        # there must not frame the pin. Pre-fix the scanner scanned the
        # leftover INSTEAD of the real file and keyed its hits under the
        # pin's path.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": self._LIB,
                "real_dir/real.rs": _LIVE_PIN,
                "outer/real.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(by_path, {})
        self.assertEqual(unattributable, {})

    def test_conventional_children_of_redirected_inline_module_resolve(self):
        # The redirect applies to conventional children too: rustc loads
        # src/real_dir/routed.rs for `#[path = "real_dir"] mod outer {
        # mod routed; }` — not src/outer/routed.rs.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    '#[path = "real_dir"]\n'
                    "mod outer {\n"
                    "    mod routed;\n"
                    "}\n"
                ),
                "real_dir/routed.rs": _IGNORED_PIN,
                "outer/routed.rs": _LIVE_PIN,
            }
        )
        self.assertEqual(sorted(by_path), [self._PIN])
        self.assertEqual(unattributable, {})

    def test_nested_redirects_compose(self):
        # Each level's attribute replaces its own component: rustc loads
        # src/d1/d2/real.rs for `#[path = "d1"] mod a { #[path = "d2"]
        # mod b { #[path = "real.rs"] mod c; } }`, and the libtest path
        # keeps the module names (a::b::c::…).
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    '#[path = "d1"]\n'
                    "mod a {\n"
                    '    #[path = "d2"]\n'
                    "    mod b {\n"
                    '        #[path = "real.rs"]\n'
                    "        mod c;\n"
                    "    }\n"
                    "}\n"
                ),
                "d1/d2/real.rs": _IGNORED_PIN,
                "a/b/real.rs": _LIVE_PIN,
            }
        )
        self.assertEqual(sorted(by_path), ["a::b::c::tests::critical_pin"])
        self.assertEqual(unattributable, {})

    def test_non_mod_rs_redirect_resolves_against_the_file_directory(self):
        # Round-15 review P2 (comment 4306952266), correcting this test's
        # own round-14 expectation: rustc resolves an inline module's OWN
        # #[path] against the declaring module's directory and CLEARS the
        # pending file-stem (rustc 1.99 probes: `#[path = "redir"] mod s
        # { #[path = "real.rs"] mod m; }` in src/a/b.rs loads
        # src/a/redir/real.rs — the old body pinned the stem-relative
        # src/a/b/redir/real.rs, a file rustc never loads). Both wrong
        # locations are planted as LIVE decoys so the model — not the
        # fail-closed net — decides.
        root = self._crate_root(
            {
                "lib.rs": "mod a;\n",
                "a.rs": "pub mod b;\n",
                "a/b.rs": (
                    '#[path = "redir"]\n'
                    "pub mod s {\n"
                    '    #[path = "real.rs"]\n'
                    "    pub mod m;\n"
                    "}\n"
                ),
                "a/redir/real.rs": _IGNORED_PIN,
                "a/b/redir/real.rs": _LIVE_PIN,
                "a/b/s/real.rs": _LIVE_PIN,
            }
        )
        by_path, unattributable = self._scan(root)
        pin = "a::b::s::m::tests::critical_pin"
        self.assertEqual(sorted(by_path), [pin])
        self.assertEqual(
            [loc.rsplit(":", 1)[0] for loc in by_path[pin]],
            [self._src_path(root, "a/redir/real.rs")],
        )
        self.assertEqual(unattributable, {})

    def test_main_red_on_redirected_ignored_pin_despite_decoy(self):
        # End-to-end over the review's false-green fixture: main() must
        # fail on the #[ignore]-at-the-real-file message — not pass, and
        # not the fail-closed "outside the scanned lib module graph"
        # message (the redirect is modelled now, the real source IS
        # reached).
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files={
                "real_dir/real.rs": _IGNORED_PIN,
                "outer/real.rs": _LIVE_PIN,
            },
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("real_dir") / "real.rs") + ":4", err)
        self.assertNotIn("outside the scanned lib module graph", err)

    def test_main_green_when_redirected_real_source_is_live(self):
        # The corrected direction of the review's fixture 3: a live real
        # source is properly attributed through the redirect, and the
        # decoy's #[ignore] — outside the graph — cannot frame it.
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files={
                "real_dir/real.rs": _LIVE_PIN,
                "outer/real.rs": _IGNORED_PIN,
            },
        )
        self.assertEqual(code, 0)
        self.assertNotIn("is #[ignore]d", err)


class Round15CfgAmbiguityTests(GuardFixture):
    """Round-15 review P2 (comment 4306952265): two cfg-mutex inline
    declarations of one module name with different `#[path]` redirects
    shared the single (enclosing_stack, name) slot and the LATER one
    silently replaced the earlier — a Linux-relevant ignored pin resolved
    against the Windows twin's directory and main() returned 0 (the
    review's four-fixture matrix, fixture 1: unix-first order). rustc
    strips cfg-mismatched items before module resolution, so which twin
    survives depends on the host platform, which the guard cannot
    evaluate: a conflicting repeat now fails closed (`_record_inline_decl`),
    in both declaration orders, for a plain-vs-redirected twin too, and
    even when both twins agree on the ignore status — the ambiguity itself
    is the failure. Same-directory twins stay legal."""

    _LIB_UNIX_FIRST = (
        "#[cfg(unix)]\n"
        '#[path = "unix_dir"]\n'
        "mod outer {\n"
        "    mod routed;\n"
        "}\n"
        "#[cfg(windows)]\n"
        '#[path = "windows_dir"]\n'
        "mod outer {\n"
        "    mod routed;\n"
        "}\n"
    )
    _PIN = "outer::routed::tests::critical_pin"
    # Reuse DuplicateRegistrationTests' main() runner without inheriting
    # its test methods (same pattern as Round14InlineRedirectTests).
    _run_main = DuplicateRegistrationTests._run_main

    def test_conflicting_redirect_twins_fail_closed(self):
        # The review reproduction, verbatim order (unix first): the real
        # unix_dir source carries the #[ignore], the windows twin is live.
        # Pre-fix the windows entry replaced the unix one, the scanner
        # read only windows_dir, and the silenced pin passed (the review's
        # 0). The file is refused now; the refusal names both directories.
        with self.assertRaises(SystemExit) as ctx:
            self._ignored_sources(
                {
                    "lib.rs": self._LIB_UNIX_FIRST,
                    "unix_dir/routed.rs": _IGNORED_PIN,
                    "windows_dir/routed.rs": _LIVE_PIN,
                }
            )
        msg = str(ctx.exception)
        self.assertIn("ambiguous inline module", msg)
        self.assertIn("'unix_dir' vs 'windows_dir'", msg)
        self.assertIn("distinct names", msg)

    def test_conflict_fails_closed_in_reverse_declaration_order(self):
        # The map may not keep whichever twin is declared last: the
        # windows-first order (the review's fixture 2 — pre-fix it
        # "passed" only because the shadowing happened to point at the
        # ignored file) must abort too.
        with self.assertRaises(SystemExit) as ctx:
            self._ignored_sources(
                {
                    "lib.rs": (
                        "#[cfg(windows)]\n"
                        '#[path = "windows_dir"]\n'
                        "mod outer {\n"
                        "    mod routed;\n"
                        "}\n"
                        "#[cfg(unix)]\n"
                        '#[path = "unix_dir"]\n'
                        "mod outer {\n"
                        "    mod routed;\n"
                        "}\n"
                    ),
                    "unix_dir/routed.rs": _IGNORED_PIN,
                    "windows_dir/routed.rs": _LIVE_PIN,
                }
            )
        self.assertIn("'windows_dir' vs 'unix_dir'", str(ctx.exception))

    def test_ambiguous_node_fails_closed_even_when_both_twins_agree(self):
        # Both-live and both-ignored change the verdict, not the
        # attribution problem: the guard cannot know which file holds the
        # pin's source, so the conflict stays a refusal instead of a
        # platform-dependent guess — fail closed in every direction.
        for unix_file, windows_file in (
            (_LIVE_PIN, _LIVE_PIN),
            (_IGNORED_PIN, _IGNORED_PIN),
        ):
            with self.assertRaises(SystemExit) as ctx:
                self._ignored_sources(
                    {
                        "lib.rs": self._LIB_UNIX_FIRST,
                        "unix_dir/routed.rs": unix_file,
                        "windows_dir/routed.rs": windows_file,
                    }
                )
            self.assertIn("ambiguous inline module", str(ctx.exception))

    def test_redirected_twin_conflicts_with_plain_name_module(self):
        # A #[path] twin vs a plain cfg-mutex twin of the same name: the
        # plain declaration's effective directory is the module NAME (the
        # fallback _dir_components applies), so 'unix_dir' vs 'outer' is a
        # genuine conflict — the plain side is recorded too, not only
        # path-carrying declarations.
        with self.assertRaises(SystemExit) as ctx:
            self._ignored_sources(
                {
                    "lib.rs": (
                        "#[cfg(unix)]\n"
                        '#[path = "unix_dir"]\n'
                        "mod outer {\n"
                        "    mod routed;\n"
                        "}\n"
                        "#[cfg(windows)]\n"
                        "mod outer {\n"
                        "    mod routed;\n"
                        "}\n"
                    ),
                    "unix_dir/routed.rs": _IGNORED_PIN,
                    "outer/routed.rs": _LIVE_PIN,
                }
            )
        self.assertIn("'unix_dir' vs 'outer'", str(ctx.exception))

    def test_identical_redirect_twins_are_not_ambiguous(self):
        # The same directory twice resolves identically — legal Rust, the
        # scan proceeds and the pin is attributed at the shared file.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "#[cfg(unix)]\n"
                    '#[path = "shared_dir"]\n'
                    "mod outer {\n"
                    "    mod routed;\n"
                    "}\n"
                    "#[cfg(windows)]\n"
                    '#[path = "shared_dir"]\n'
                    "mod outer {\n"
                    "    mod routed;\n"
                    "}\n"
                ),
                "shared_dir/routed.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(sorted(by_path), [self._PIN])
        self.assertEqual(unattributable, {})

    def test_main_fails_closed_on_ambiguous_redirect(self):
        # End to end: the ambiguity aborts the guard run itself — main()
        # exits with the refusal, it never reaches a verdict derived from
        # whichever twin the old map happened to keep.
        with self.assertRaises(SystemExit) as ctx:
            self._run_main(
                [self._PIN],
                source=self._LIB_UNIX_FIRST,
                required=[self._PIN],
                files={
                    "unix_dir/routed.rs": _IGNORED_PIN,
                    "windows_dir/routed.rs": _LIVE_PIN,
                },
            )
        self.assertIn("ambiguous inline module", str(ctx.exception))


class Round15NonModRsOwnershipTests(GuardFixture):
    """Round-15 review P2 (comment 4306952266): in a non-mod-rs file the
    pending file-stem directory stayed the base the own-#[path] inline
    module's directory was appended after, while rustc resolves an inline
    module's own path against the declaring module's directory and CLEARS
    the pending stem — `#[path = "redir"] mod s { #[path = "real.rs"]
    mod m; }` in src/a/b.rs loads src/a/redir/real.rs; the scanner read
    src/a/b/redir/real.rs, a file rustc never loads (rustc 1.99 probes,
    both directions). The corrected round-14 fixture lives in
    Round14InlineRedirectTests; these pin the remaining shapes: the
    reverse decoy direction, external children under the redirected
    module, composition after a plain inline has consumed the stem, and
    the end-to-end red."""

    _LIB = (
        '#[path = "redir"]\n'
        "pub mod s {\n"
        '    #[path = "real.rs"]\n'
        "    pub mod m;\n"
        "}\n"
    )
    _PIN = "a::b::s::m::tests::critical_pin"
    _run_main = DuplicateRegistrationTests._run_main

    def _tree(self, real, stem_relative):
        return {
            "lib.rs": "mod a;\n",
            "a.rs": "pub mod b;\n",
            "a/b.rs": self._LIB,
            "a/redir/real.rs": real,
            "a/b/redir/real.rs": stem_relative,
        }

    def test_stem_relative_decoy_cannot_frame_live_pin(self):
        # Reverse half: with the real (file-dir-relative) source live, an
        # #[ignore] at the old stem-relative location is NOT a lib source
        # — it must not frame the pin. Pre-fix the scanner read exactly
        # that decoy and keyed its hit under the pin's path.
        by_path, unattributable = self._ignored_sources(
            self._tree(real=_LIVE_PIN, stem_relative=_IGNORED_PIN)
        )
        self.assertEqual(by_path, {})
        self.assertEqual(unattributable, {})

    def test_external_child_of_own_path_inline_uses_file_dir(self):
        # Conventional children under the redirected inline module move
        # with it: `#[path = "redir"] mod s { mod m; }` in a/b.rs loads
        # src/a/redir/m.rs — the stem is cleared for the whole subtree,
        # and neither the stem-relative nor the name-relative twin counts.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": "mod a;\n",
                "a.rs": "pub mod b;\n",
                "a/b.rs": (
                    '#[path = "redir"]\n'
                    "pub mod s {\n"
                    "    pub mod m;\n"
                    "}\n"
                ),
                "a/redir/m.rs": _IGNORED_PIN,
                "a/b/redir/m.rs": _LIVE_PIN,
                "a/b/s/m.rs": _LIVE_PIN,
            }
        )
        self.assertEqual(sorted(by_path), [self._PIN])
        self.assertEqual(unattributable, {})

    def test_own_path_after_plain_inline_composes_after_consumed_stem(self):
        # A plain first inline component consumes the stem
        # (src/b/outer/…); an own-#[path] module BELOW it composes after
        # that consumed base — the base must only drop to the file
        # directory when the FIRST component itself carries the override
        # (rustc probe: `mod outer { #[path = "redir"] mod s { ... } }` in
        # src/b.rs loads src/b/outer/redir/real.rs, not src/outer/…).
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": "mod a;\n",
                "a.rs": "pub mod b;\n",
                "a/b.rs": (
                    "pub mod outer {\n"
                    '    #[path = "redir"]\n'
                    "    pub mod s {\n"
                    '        #[path = "real.rs"]\n'
                    "        pub mod m;\n"
                    "    }\n"
                    "}\n"
                ),
                "a/b/outer/redir/real.rs": _IGNORED_PIN,
                "a/outer/redir/real.rs": _LIVE_PIN,
                "a/b/outer/s/real.rs": _LIVE_PIN,
            }
        )
        self.assertEqual(
            sorted(by_path),
            ["a::b::outer::s::m::tests::critical_pin"],
        )
        self.assertEqual(unattributable, {})

    def test_main_red_on_non_mod_rs_redirected_ignored_pin(self):
        # End to end over the review's false-green fixture: the real
        # (file-dir-relative) source carries the #[ignore], the
        # stem-relative twin the pre-fix scanner actually read is live —
        # main() must fail with the ignore verdict AT THE REAL FILE.
        code, err = self._run_main(
            [self._PIN],
            source="mod a;\n",
            required=[self._PIN],
            files={
                "a.rs": "pub mod b;\n",
                "a/b.rs": self._LIB,
                "a/redir/real.rs": _IGNORED_PIN,
                "a/b/redir/real.rs": _LIVE_PIN,
            },
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("a") / "redir" / "real.rs") + ":4", err)
        self.assertNotIn("outside the scanned lib module graph", err)


class Round16SameTextDifferentBaseTests(GuardFixture):
    """Round-16 review P2 (comment 4307717045): identical directory TEXT
    does not imply an identical resolution base. In src/a/b.rs the
    cfg-mutex pair plain `#[cfg(unix)] mod outer { … }` (the plain first
    component consumes the pending stem → src/a/b/outer/) and
    `#[cfg(windows)] #[path = "outer"] mod outer { … }` (the attribute
    clears the stem → src/a/outer/) both carry the text "outer", and the
    round-15 record read that as one same-directory repeat — the global
    override then pointed BOTH at src/a/outer, so an ignored pin in the
    real src/a/b/outer/routed.rs passed while only the live twin file was
    read (the review's four-control: 0 / 1 / 0). rustc 1.99 probes pin
    both bases. The record now carries the physical base and a differing
    base aborts even when the texts match — in both declaration orders
    and even when the twins agree on the ignore status, while the
    mod-rs-root same-text pair (review: the old same-directory positive
    control holds only there) and deeper twins that share one base stay
    legal."""

    _LIB_UNIX_FIRST = (
        "#[cfg(unix)]\n"
        "mod outer {\n"
        "    mod routed;\n"
        "}\n"
        "#[cfg(windows)]\n"
        '#[path = "outer"]\n'
        "mod outer {\n"
        "    mod routed;\n"
        "}\n"
    )
    _PIN = "a::b::outer::routed::tests::critical_pin"
    _run_main = DuplicateRegistrationTests._run_main

    def _files(self, plain_file, redirect_file, b_lib=None):
        return {
            "lib.rs": "mod a;\n",
            "a.rs": "pub mod b;\n",
            "a/b.rs": b_lib if b_lib is not None else self._LIB_UNIX_FIRST,
            "a/b/outer/routed.rs": plain_file,
            "a/outer/routed.rs": redirect_file,
        }

    def test_review_false_green_fails_closed(self):
        # The review reproduction: the real stem-base source (the file a
        # unix host compiles) carries the #[ignore], the cleared-stem
        # twin is live — pre-fix both read src/a/outer/routed.rs and the
        # silenced pin passed. The physical-base mismatch aborts now,
        # naming both directories.
        with self.assertRaises(SystemExit) as ctx:
            self._ignored_sources(self._files(_IGNORED_PIN, _LIVE_PIN))
        msg = str(ctx.exception)
        self.assertIn("ambiguous inline module", msg)
        self.assertIn(str(Path("a") / "b" / "outer"), msg)
        self.assertIn(str(Path("a") / "outer"), msg)
        self.assertIn("distinct names", msg)

    def test_conflict_fails_closed_in_reverse_declaration_order(self):
        # The redirect twin declared first must not pin the model either:
        # the plain declaration's stem-consumed base differs from the
        # attribute's cleared base no matter which comes first.
        lib = (
            "#[cfg(windows)]\n"
            '#[path = "outer"]\n'
            "mod outer {\n"
            "    mod routed;\n"
            "}\n"
            "#[cfg(unix)]\n"
            "mod outer {\n"
            "    mod routed;\n"
            "}\n"
        )
        with self.assertRaises(SystemExit) as ctx:
            self._ignored_sources(
                self._files(_IGNORED_PIN, _LIVE_PIN, b_lib=lib)
            )
        self.assertIn("ambiguous inline module", str(ctx.exception))

    def test_ambiguity_fails_closed_when_twins_agree(self):
        # Both-live and both-ignored only change the verdict, not the
        # attribution: which physical file holds the pin's source is
        # platform-dependent, so the node stays refused either way.
        for plain_file, redirect_file in (
            (_LIVE_PIN, _LIVE_PIN),
            (_IGNORED_PIN, _IGNORED_PIN),
        ):
            with self.assertRaises(SystemExit) as ctx:
                self._ignored_sources(self._files(plain_file, redirect_file))
            self.assertIn("ambiguous inline module", str(ctx.exception))

    def test_mod_rs_root_plain_and_path_same_text_stay_legal(self):
        # The boundary the review names explicitly: at a mod-rs root the
        # plain and the #[path] twin share one base (child_dir ==
        # file_dir), so the same text IS the same directory — legal
        # Rust, attributed at the shared file. This is exactly the shape
        # that must NOT be extrapolated to non-mod-rs files.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": self._LIB_UNIX_FIRST,
                "outer/routed.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(
            sorted(by_path), ["outer::routed::tests::critical_pin"]
        )
        self.assertEqual(unattributable, {})

    def test_deeper_twins_share_base_and_stay_legal(self):
        # Below the first component the stem is already consumed, so a
        # plain `mod s` and a same-named `#[path = "s"] mod s` twin
        # resolve identically (rustc probe: both to
        # src/a/b/outer/s/routed.rs) — the physical-base record must not
        # turn that agreement into a false clash.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": "mod a;\n",
                "a.rs": "pub mod b;\n",
                "a/b.rs": (
                    "pub mod outer {\n"
                    "    #[cfg(unix)]\n"
                    "    pub mod s {\n"
                    "        pub mod routed;\n"
                    "    }\n"
                    "    #[cfg(windows)]\n"
                    '    #[path = "s"]\n'
                    "    pub mod s {\n"
                    "        pub mod routed;\n"
                    "    }\n"
                    "}\n"
                ),
                "a/b/outer/s/routed.rs": _IGNORED_PIN,
                "a/outer/s/routed.rs": _LIVE_PIN,
                "a/b/outer/routed.rs": _LIVE_PIN,
            }
        )
        self.assertEqual(
            sorted(by_path),
            ["a::b::outer::s::routed::tests::critical_pin"],
        )
        self.assertEqual(unattributable, {})

    def test_main_fails_closed_on_same_text_different_base(self):
        # End to end: the review's false-green fixture aborts the guard
        # run itself instead of reaching a verdict read off whichever
        # file the old text-keyed model happened to point at.
        with self.assertRaises(SystemExit) as ctx:
            self._run_main(
                [self._PIN],
                source="mod a;\n",
                required=[self._PIN],
                files={
                    "a.rs": "pub mod b;\n",
                    "a/b.rs": self._LIB_UNIX_FIRST,
                    "a/b/outer/routed.rs": _IGNORED_PIN,
                    "a/outer/routed.rs": _LIVE_PIN,
                },
            )
        self.assertIn("ambiguous inline module", str(ctx.exception))


class Round16PathFileOwnershipTests(GuardFixture):
    """Round-16 review P2 (comment 4307717048): a file loaded through
    `#[path]` wiring keeps mod.rs-like directory ownership — rustc's
    mod_file_path returns DirOwnership Owned{relative: None} for every
    #[path] load, so `lib.rs: #[path = "shim.rs"] mod entry;` with
    `shim.rs: mod inner;` compiles src/inner.rs (rustc 1.99 probes, both
    directions), NOT src/shim/inner.rs. The scanner queued wired files as
    ordinary non-mod-rs modules and opened `<stem>/`, reading an unwired
    leftover file while the real sibling source stayed unscanned: an
    ignored pin in src/inner.rs passed because the live same-named pin in
    src/shim/inner.rs was attributed instead (the review's four-control:
    0 / 1 / 0). The queue now tags #[path] loads; their conventional
    children resolve beside the file, in plain inline modules, and under
    own-#[path] inline overrides alike."""

    _LIB = '#[path = "shim.rs"]\nmod entry;\n'
    _SHIM = "mod inner;\n"
    _PIN = "entry::inner::tests::critical_pin"
    _run_main = DuplicateRegistrationTests._run_main

    def test_real_sibling_source_is_scanned_not_the_stem_twin(self):
        # The review's fixture 1: the real sibling source carries the
        # #[ignore], the stem-directory twin is live — the scan must
        # attribute the pin AT src/inner.rs (pre-fix it read exactly the
        # unwired shim/inner.rs and passed). Exact `_src_path` equality
        # instead of a slash-suffixed endswith (round-17 review: the
        # hardcoded separator rejected the scanner's backslash output on
        # Windows, and the `"/shim/"` exclusion missed that form too).
        files = {
            "lib.rs": self._LIB,
            "shim.rs": self._SHIM,
            "inner.rs": _IGNORED_PIN,
            "shim/inner.rs": _LIVE_PIN,
        }
        root = self._crate_root(files)
        by_path, unattributable = self._scan(root)
        self.assertEqual(sorted(by_path), [self._PIN])
        self.assertEqual(unattributable, {})
        self.assertEqual(
            by_path[self._PIN],
            [self._src_path(root, "inner.rs") + ":4"],
        )

    def test_stem_twin_decoy_cannot_frame_live_pin(self):
        # Reverse half: with the real sibling source live, the #[ignore]
        # in the unwired stem-directory leftover must not frame the pin —
        # that file is not a lib source and never enters the scan.
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": self._LIB,
                "shim.rs": self._SHIM,
                "inner.rs": _LIVE_PIN,
                "shim/inner.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(by_path, {})
        self.assertEqual(unattributable, {})

    def test_plain_inline_module_in_path_owned_file_uses_file_dir(self):
        # Ownership covers every child shape: a plain inline module in
        # the wired file consumes no stem — `mod outer { mod routed; }`
        # in shim.rs loads src/outer/routed.rs (rustc probe), so the
        # shim/outer/ twin is a decoy, not the pin's source. Exact
        # `_src_path` equality (round-17 review) — the slash-substring
        # decoy exclusion was vacuous against Windows backslash output.
        files = {
            "lib.rs": self._LIB,
            "shim.rs": "mod outer {\n    mod routed;\n}\n",
            "outer/routed.rs": _IGNORED_PIN,
            "shim/outer/routed.rs": _LIVE_PIN,
        }
        root = self._crate_root(files)
        by_path, unattributable = self._scan(root)
        self.assertEqual(
            sorted(by_path),
            ["entry::outer::routed::tests::critical_pin"],
        )
        self.assertEqual(unattributable, {})
        self.assertEqual(
            by_path["entry::outer::routed::tests::critical_pin"],
            [self._src_path(root, "outer/routed.rs") + ":4"],
        )

    def test_own_path_inline_in_path_owned_file_uses_file_dir(self):
        # An own-#[path] inline module inside the wired file composes
        # against the file's directory too (rustc probe:
        # `#[path = "redir"] mod s { … }` in shim.rs loads
        # src/redir/real.rs): neither the stem-relative nor the
        # module-name-relative twin is a lib source. Exact `_src_path`
        # equality (round-17 review) — same Windows-separator blindness
        # as the sibling tests.
        files = {
            "lib.rs": self._LIB,
            "shim.rs": (
                '#[path = "redir"]\n'
                "mod s {\n"
                '    #[path = "real.rs"]\n'
                "    mod m;\n"
                "}\n"
            ),
            "redir/real.rs": _IGNORED_PIN,
            "shim/redir/real.rs": _LIVE_PIN,
            "shim/s/real.rs": _LIVE_PIN,
        }
        root = self._crate_root(files)
        by_path, unattributable = self._scan(root)
        self.assertEqual(
            sorted(by_path), ["entry::s::m::tests::critical_pin"]
        )
        self.assertEqual(unattributable, {})
        self.assertEqual(
            by_path["entry::s::m::tests::critical_pin"],
            [self._src_path(root, "redir/real.rs") + ":4"],
        )

    def test_main_red_locates_real_sibling_source(self):
        # End to end over the review's false-green fixture: main() fails
        # with the ignore verdict AT THE REAL sibling file, and the
        # unwired stem-directory decoy appears nowhere.
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files={
                "shim.rs": self._SHIM,
                "inner.rs": _IGNORED_PIN,
                "shim/inner.rs": _LIVE_PIN,
            },
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("inner.rs")) + ":4", err)
        self.assertNotIn(str(Path("shim") / "inner.rs"), err)
        self.assertNotIn("outside the scanned lib module graph", err)


class Round17ExplicitPathOwnershipTests(GuardFixture):
    """Round-17 review P2 (comment 4308210510): a `#[path]` declaration
    whose module name matches its target's basename also matched the
    externals' conventional candidates — `lib.rs:
    #[path = "shared.rs"] mod shared;` found src/shared.rs through both
    loops, the externals enqueue won the FIFO race with path_loaded=False,
    and the realpath-only visited set dropped the correct wired entry, so
    the scan read the stem-directory decoy (the review's four-control ran
    0/1/0/1 instead of 1/0/0/1, no cfg needed). One physical file wired
    under two module names additionally lost every context after the
    first (rustc 1.99 probe: alpha::inner::t AND beta::inner::t are both
    libtest paths), and module cycles now fail closed the way rustc's
    loader does ("circular modules"). Round-17 review P2 (comment
    4308210515) reworked the round-16 location assertions onto exact
    `_src_path` equality: the hardcoded-slash `endswith` rejected the
    scanner's legitimate backslash output on Windows (2 of the reviewer's
    93 items failed) and the `"/main/"`/`"/shim/"` decoy exclusions went
    vacuous against it — exact platform-native equality on both sides
    keeps the assertions honest under BOTH separators."""

    _LIB = '#[path = "shared.rs"]\nmod shared;\n'
    _SHIM = "mod inner;\n"
    _PIN = "shared::inner::tests::critical_pin"
    _run_main = DuplicateRegistrationTests._run_main

    def test_explicit_path_declaration_wins_over_conventional_twin(self):
        # The review's fixture: real src/inner.rs carries the #[ignore],
        # the stem-directory decoy src/shared/inner.rs is live — main
        # must fail AT THE REAL source (pre-fix the decoy satisfied the
        # scan and the guard returned 0).
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files={
                "shared.rs": self._SHIM,
                "inner.rs": _IGNORED_PIN,
                "shared/inner.rs": _LIVE_PIN,
            },
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("inner.rs")) + ":4", err)
        self.assertNotIn(str(Path("shared") / "inner.rs"), err)
        self.assertNotIn("outside the scanned lib module graph", err)

    def test_explicit_path_reverse_control_decoy_cannot_frame(self):
        # Reverse half of the four-control: the real sibling source is
        # live, the decoy carries the #[ignore] — the guard must pass
        # (pre-fix it returned 1 off the wrongly-scanned decoy).
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files={
                "shared.rs": self._SHIM,
                "inner.rs": _LIVE_PIN,
                "shared/inner.rs": _IGNORED_PIN,
            },
        )
        self.assertEqual(code, 0)
        self.assertNotIn(str(Path("shared") / "inner.rs"), err)

    def test_one_file_two_module_paths_both_contexts_scanned(self):
        # The same physical file wired under two module names is two
        # libtest paths (rustc 1.99 probe): the scan must attribute BOTH
        # contexts — the realpath-only visited set silently dropped the
        # second, pushing its pin into the fail-closed outside-graph
        # branch.
        lib = (
            '#[path = "shared.rs"]\nmod alpha;\n'
            '#[path = "shared.rs"]\nmod beta;\n'
        )
        files = {
            "lib.rs": lib,
            "shared.rs": self._SHIM,
            "inner.rs": _IGNORED_PIN,
        }
        root = self._crate_root(files)
        with patch.object(
            guard, "MANIFEST", str(root / "pinvou3-app/src-tauri/Cargo.toml")
        ):
            by_path, unattributable, attributable = guard._scan_graph()
        self.assertEqual(unattributable, {})
        self.assertEqual(
            sorted(attributable),
            [
                "alpha::inner::tests::critical_pin",
                "beta::inner::tests::critical_pin",
            ],
        )
        expected_loc = [self._src_path(root, "inner.rs") + ":4"]
        self.assertEqual(by_path["alpha::inner::tests::critical_pin"], expected_loc)
        self.assertEqual(by_path["beta::inner::tests::critical_pin"], expected_loc)
        # End to end: the second context's pin is judged on the REAL
        # source, not reported outside the graph.
        code, err = self._run_main(
            ["beta::inner::tests::critical_pin"],
            source=lib,
            required=["beta::inner::tests::critical_pin"],
            files={
                "shared.rs": self._SHIM,
                "inner.rs": _IGNORED_PIN,
            },
        )
        self.assertEqual(code, 1)
        self.assertIn(
            "required pin beta::inner::tests::critical_pin is #[ignore]d at",
            err,
        )
        self.assertNotIn("outside the scanned lib module graph", err)

    def test_circular_module_graph_fails_closed(self):
        # rustc refuses a #[path] module cycle ("circular modules:
        # src/a.rs -> src/a.rs", rustc 1.99 probe); the scan mirrors that
        # refusal instead of walking an unbounded context tree.
        files = {
            "lib.rs": '#[path = "a.rs"]\nmod a;\n',
            "a.rs": '#[path = "a.rs"]\nmod b;\n',
        }
        root = self._crate_root(files)
        with patch.object(
            guard, "MANIFEST", str(root / "pinvou3-app/src-tauri/Cargo.toml")
        ):
            with self.assertRaises(SystemExit) as ctx:
                guard.ignored_source_pins()
        self.assertIn("circular modules", str(ctx.exception))


class Round18CfgTwinExternalTests(GuardFixture):
    """Round-18 review P2 (comment 4309910884): the wired exclusion was
    keyed by (enclosing stack, module NAME), so a cfg-mutex plain twin of
    a wired declaration was excluded too —
    `#[cfg(unix)] #[path = "unix.rs"] mod shared;` beside
    `#[cfg(windows)] mod shared;` silenced the Windows branch's
    conventional probe, and the required pin `#[ignore]`d in the file
    rustc actually loads there scanned green (the review's four-control
    ran 0/1/0/1). The exclusion is now per declaration — (module name,
    declaration line) — so only the declaration carrying the `#[path]`
    attribute loses its conventional probe; the twin keeps it. Both
    branches of the cfg split are scanned, which is deliberately
    conservative: an `#[ignore]` on any candidate source of a required
    pin fails, whichever host resolution loads it."""

    _LIB = (
        "#[cfg(unix)]\n"
        '#[path = "unix.rs"]\n'
        "mod shared;\n"
        "\n"
        "#[cfg(windows)]\n"
        "mod shared;\n"
    )
    _PIN = "shared::tests::critical_pin"
    _run_main = DuplicateRegistrationTests._run_main

    def test_cfg_twin_plain_branch_is_scanned(self):
        # The review's false green: the Windows-loaded conventional file
        # carries the #[ignore], unix.rs is live — main must fail AT THE
        # REAL Windows source (pre-fix the plain twin was name-excluded
        # with the wired declaration and never scanned → return 0).
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files={"unix.rs": _LIVE_PIN, "shared.rs": _IGNORED_PIN},
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("shared.rs")) + ":4", err)
        self.assertNotIn("unix.rs", err)
        self.assertNotIn("outside the scanned lib module graph", err)

    def test_cfg_twin_reverse_control_wired_branch_ignored_fails(self):
        # Reverse half of the four-control: the wired unix.rs carries the
        # #[ignore] — the guard must fail at the wired source.
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files={"unix.rs": _IGNORED_PIN, "shared.rs": _LIVE_PIN},
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("unix.rs")) + ":4", err)

    def test_cfg_twin_both_live_pass(self):
        # Both branches live: the required pin is registered and its
        # sources carry no #[ignore] — the guard must pass.
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files={"unix.rs": _LIVE_PIN, "shared.rs": _LIVE_PIN},
        )
        self.assertEqual(code, 0)
        self.assertNotIn("is #[ignore]d", err)

    def test_cfg_twin_both_ignored_fail(self):
        # Both branches ignored: red regardless of which host loads which.
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files={"unix.rs": _IGNORED_PIN, "shared.rs": _IGNORED_PIN},
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)

    def test_wired_declaration_itself_still_excluded_from_conventional(self):
        # The round-17 ownership rule must survive the identity keying:
        # with NO plain twin, the wired declaration alone is excluded and
        # the stem-directory decoy at the conventional spot is NOT a lib
        # source — an #[ignore] planted there must not frame the pin.
        code, err = self._run_main(
            [self._PIN],
            source='#[path = "real.rs"]\nmod shared;\n',
            required=[self._PIN],
            files={
                "real.rs": _LIVE_PIN,
                "shared.rs": _IGNORED_PIN,
            },
        )
        self.assertEqual(code, 0)
        self.assertNotIn(str(Path("shared.rs")), err)


class Round19OwnershipContextTests(GuardFixture):
    """Round-19 review P2 (review of head 49efdd36, 2026-10-22): the
    visited set keyed (realpath, module path) dropped the ownership half —
    `#[cfg(unix)] #[path = "shared.rs"] mod shared;` beside
    `#[cfg(windows)] mod shared;` both reach src/shared.rs under the SAME
    module path, the plain enqueue wins the FIFO race (the externals loop
    precedes the wired loop), and the wired context is discarded: its
    mod.rs-like ownership resolves `mod inner;` to src/inner.rs while the
    surviving plain context opens `shared/` — so the pin's real ignored
    source src/inner.rs never entered the scan and the live stem child
    src/shared/inner.rs answered for it (the review's four-control ran
    0/1/0/1). The context now carries the ownership tag and both branches
    are scanned; any candidate source's #[ignore] fails the pin — the
    conservative direction the round-18 cfg-twin fix established."""

    _LIB = (
        "#[cfg(unix)]\n"
        '#[path = "shared.rs"]\n'
        "mod shared;\n"
        "\n"
        "#[cfg(windows)]\n"
        "mod shared;\n"
    )
    _SWAPPED = (
        "#[cfg(windows)]\n"
        "mod shared;\n"
        "\n"
        "#[cfg(unix)]\n"
        '#[path = "shared.rs"]\n'
        "mod shared;\n"
    )
    _PIN = "shared::inner::tests::critical_pin"
    _run_main = DuplicateRegistrationTests._run_main

    def _files(self, real, stem):
        return {
            "lib.rs": self._LIB,
            "shared.rs": "mod inner;\n",
            "inner.rs": real,
            "shared/inner.rs": stem,
        }

    def test_wired_child_ignored_fails_at_real_sibling_source(self):
        # The review's false green: src/inner.rs (the wired context's
        # child — the file rustc loads on unix) carries the #[ignore],
        # the stem child src/shared/inner.rs is live. The scan must
        # attribute the pin AT src/inner.rs and main must fail there
        # (pre-fix the wired context was visited-dropped → return 0).
        files = self._files(_IGNORED_PIN, _LIVE_PIN)
        root = self._crate_root(files)
        by_path, unattributable = self._scan(root)
        self.assertEqual(unattributable, {})
        self.assertEqual(
            by_path[self._PIN], [self._src_path(root, "inner.rs") + ":4"]
        )
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files=files,
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("inner.rs")) + ":4", err)
        self.assertNotIn(str(Path("shared") / "inner.rs"), err)
        self.assertNotIn("outside the scanned lib module graph", err)

    def test_plain_child_ignored_fails_at_stem_source(self):
        # Reverse half of the four-control: the stem child (the plain
        # context's child — the file rustc loads on windows) carries the
        # #[ignore], the wired sibling is live — main must fail AT the
        # stem source (pre-fix this control passed with the wired context
        # alone doing the scanning; the fixed scan keeps both).
        files = self._files(_LIVE_PIN, _IGNORED_PIN)
        root = self._crate_root(files)
        by_path, unattributable = self._scan(root)
        self.assertEqual(unattributable, {})
        self.assertEqual(
            by_path[self._PIN],
            [self._src_path(root, "shared/inner.rs") + ":4"],
        )
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files=files,
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("shared") / "inner.rs") + ":4", err)
        self.assertNotIn("outside the scanned lib module graph", err)

    def test_both_live_pass(self):
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files=self._files(_LIVE_PIN, _LIVE_PIN),
        )
        self.assertEqual(code, 0)
        self.assertNotIn("is #[ignore]d", err)

    def test_both_ignored_fail(self):
        code, err = self._run_main(
            [self._PIN],
            source=self._LIB,
            required=[self._PIN],
            files=self._files(_IGNORED_PIN, _IGNORED_PIN),
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("inner.rs")) + ":4", err)
        self.assertIn(str(Path("shared") / "inner.rs") + ":4", err)

    def test_swapped_declaration_order_same_outcome(self):
        # The pre-fix visit was FIFO luck, so the fix must not depend on
        # enqueue order either: with the twin declarations swapped in the
        # source, the wired child's #[ignore] still fails at src/inner.rs.
        code, err = self._run_main(
            [self._PIN],
            source=self._SWAPPED,
            required=[self._PIN],
            files=self._files(_IGNORED_PIN, _LIVE_PIN),
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("inner.rs")) + ":4", err)
        self.assertNotIn(str(Path("shared") / "inner.rs"), err)


class Round19DeclLineAmbiguityTests(GuardFixture):
    """Round-19 review P2 (review of head 49efdd36, 2026-10-22): the wired
    exclusion keyed (module name, declaration LINE) is only a sound
    declaration identity while every same-line same-name declaration is
    wired — a `#[rustfmt::skip]` line hosting
    `mod shared; #[cfg(windows)] mod shared;` behind the run shares ONE
    (name, line) across both, the keyed exclusion silenced the plain
    twin's conventional probe as well, and a required pin `#[ignore]`d in
    the Windows source passed (the review's wired-live/plain-ignored
    control returned 0). The lexer cannot tell which token the run wired,
    so a line with more same-name declarations than the run can wire now
    fails closed instead of excluding by a guessed identity (the
    round-15 ambiguity precedent); rustfmt's own layout puts each
    declaration on its line and never trips the refusal."""

    _LIB = (
        "#[rustfmt::skip]\n"
        "mod wrapper {\n"
        '#[cfg(unix)] #[path = "unix.rs"]\n'
        "mod shared; #[cfg(windows)] mod shared;\n"
        "}\n"
    )
    _SPLIT = (
        "#[rustfmt::skip]\n"
        "mod wrapper {\n"
        '#[cfg(unix)] #[path = "unix.rs"]\n'
        "mod shared;\n"
        "#[cfg(windows)] mod shared;\n"
        "}\n"
    )
    _PIN = "wrapper::shared::tests::critical_pin"
    _run_main = DuplicateRegistrationTests._run_main

    def test_same_line_twin_fails_closed(self):
        # The refusal is about the unresolvable identity, not the pin
        # state: it must fire with the plain source ignored AND with both
        # branches live — a silent green there would leave the
        # exclusion's reach unproven.
        for files in (
            {"wrapper/unix.rs": _LIVE_PIN, "wrapper/shared.rs": _IGNORED_PIN},
            {"wrapper/unix.rs": _LIVE_PIN, "wrapper/shared.rs": _LIVE_PIN},
        ):
            with self.assertRaises(SystemExit) as ctx:
                self._run_main(
                    [self._PIN],
                    source=self._LIB,
                    required=[self._PIN],
                    files=files,
                )
            message = str(ctx.exception)
            self.assertIn("ambiguous mod declarations", message)
            self.assertIn("lib.rs:4", message)
            self.assertIn("module 'shared'", message)
            self.assertIn("put each mod on its own line", message)

    def test_split_line_twin_scans_both_branches(self):
        # Positive control: the identical modules split onto separate
        # lines keep the round-18 per-declaration exclusion — both
        # branches probed, any #[ignore] red, both live green.
        code, err = self._run_main(
            [self._PIN],
            source=self._SPLIT,
            required=[self._PIN],
            files={"wrapper/unix.rs": _LIVE_PIN, "wrapper/shared.rs": _IGNORED_PIN},
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("wrapper") / "shared.rs") + ":4", err)

        code, err = self._run_main(
            [self._PIN],
            source=self._SPLIT,
            required=[self._PIN],
            files={"wrapper/unix.rs": _IGNORED_PIN, "wrapper/shared.rs": _LIVE_PIN},
        )
        self.assertEqual(code, 1)
        self.assertIn(str(Path("wrapper") / "unix.rs") + ":4", err)

        code, err = self._run_main(
            [self._PIN],
            source=self._SPLIT,
            required=[self._PIN],
            files={"wrapper/unix.rs": _LIVE_PIN, "wrapper/shared.rs": _LIVE_PIN},
        )
        self.assertEqual(code, 0)
        self.assertNotIn("is #[ignore]d", err)


class Round20PackedWiringTests(GuardFixture):
    """Round-20 review (head 8b7945154c, 2026-10-23): the run parser
    advanced past a `#[path]` entry whose line also carries the `mod`
    declaration, leaving no wired entry — the round-19 refusal (keyed on
    wired entries) and the round-18 per-declaration exclusion never
    engaged, and a same-named conventional file stood in for the wired
    source (maintainer control: packed 0/1/0/1 vs split 1/0/0/1). The
    same substitution defeated every unbindable packed layout, so all of
    them now fail closed; the two legal forms the old probes silently
    dropped — whitespace between `#` and `[`, a leading UTF-8 BOM — are
    modelled as ordinary wiring instead."""

    _PIN = "app::commands::tests::critical_pin"
    _run_main = DuplicateRegistrationTests._run_main

    _PACKED = '#[path = "real.rs"] mod commands;\n'
    _PACKED_SKIP = (
        "#[rustfmt::skip]\n"
        "mod wrapper { #[path = \"real.rs\"] mod commands; }\n"
    )
    _CFG_ATTR = '#[cfg_attr(unix, path = "real.rs")]\nmod commands;\n'
    _MID_LINE = 'mod app { #[path = "real.rs"] mod commands; }\n'
    _SPLIT_KEYWORD = "mod app {\n#[path = \"real.rs\"]\nmod\ncommands;\n}\n"
    _HASH_SPACE = 'mod app {\n# [path = "real.rs"]\nmod commands;\n}\n'
    _BOM = "mod app {\n#[path = \"real.rs\"]\nmod commands;\n}\n"

    def _files(self, real, decoy):
        return {"app/real.rs": real, "app/commands.rs": decoy}

    def test_packed_same_line_fails_closed(self):
        # The refusal is about the unbindable layout, not the pin state:
        # it must fire with the wired source ignored (decoy live — the
        # silent green the maintainer reproduced) and with everything
        # live, because the wiring is dropped either way.
        for real, decoy in ((_IGNORED_PIN, _LIVE_PIN), (_LIVE_PIN, _LIVE_PIN)):
            with self.assertRaises(SystemExit) as ctx:
                self._run_main(
                    [self._PIN],
                    source=self._PACKED,
                    required=[self._PIN],
                    files=self._files(real, decoy),
                )
            message = str(ctx.exception)
            self.assertIn("shares that line with a module declaration", message)
            self.assertIn("lib.rs:1", message)
            self.assertIn("put the mod on its own line", message)

    def test_rustfmt_skip_wrapper_packed_fails_closed(self):
        # The maintainer's control shape, as rustfmt lays it out:
        # `#[rustfmt::skip]` on its own line keeps the run branch off the
        # packed line, so the mid-line head refusal is the net that fires
        # (a packed run on the wrapper's own line hits the shared-line
        # refusal instead — both are fail-closed).
        with self.assertRaises(SystemExit) as ctx:
            self._run_main(
                [self._PIN],
                source=self._PACKED_SKIP,
                required=[self._PIN],
                files=self._files(_IGNORED_PIN, _LIVE_PIN),
            )
        message = str(ctx.exception)
        self.assertIn("appears mid-line after code", message)
        self.assertIn("lib.rs:2", message)

    def test_cfg_attr_wrapped_path_fails_closed(self):
        # The predicate cannot be decided statically, and which branch
        # (attribute target vs conventional spot) loads is exactly the
        # attribution the scan needs — refuse instead of guessing.
        for real, decoy in ((_IGNORED_PIN, _LIVE_PIN), (_LIVE_PIN, _IGNORED_PIN)):
            with self.assertRaises(SystemExit) as ctx:
                self._run_main(
                    [self._PIN],
                    source=self._CFG_ATTR,
                    required=[self._PIN],
                    files=self._files(real, decoy),
                )
            message = str(ctx.exception)
            self.assertIn("wrapped in cfg_attr", message)
            self.assertIn("lib.rs:1", message)

    def test_mid_line_path_head_fails_closed(self):
        # A path head after code on the same line is never rustfmt output
        # and cannot be bound to its declaration token-wise.
        with self.assertRaises(SystemExit) as ctx:
            self._run_main(
                [self._PIN],
                source=self._MID_LINE,
                required=[self._PIN],
                files=self._files(_IGNORED_PIN, _LIVE_PIN),
            )
        message = str(ctx.exception)
        self.assertIn("appears mid-line after code", message)
        self.assertIn("lib.rs:1", message)

    def test_split_keyword_fails_closed(self):
        # `mod` alone on the run's next line (name/terminator wrapped):
        # the binding cannot be vouched for, so refuse instead of letting
        # the conventional candidate answer.
        with self.assertRaises(SystemExit) as ctx:
            self._run_main(
                [self._PIN],
                source=self._SPLIT_KEYWORD,
                required=[self._PIN],
                files=self._files(_IGNORED_PIN, _LIVE_PIN),
            )
        message = str(ctx.exception)
        self.assertIn("does not share the keyword's line", message)
        self.assertIn("lib.rs:3", message)

    def test_hash_space_form_is_wired(self):
        # Positive/negative controls for the spacing-tolerant head: the
        # wired target is the module's only source, a same-named decoy at
        # the conventional spot never substitutes (round-17 exclusion),
        # and an #[ignore] in the wired source is red at its own line.
        code, err = self._run_main(
            [self._PIN],
            source=self._HASH_SPACE,
            required=[self._PIN],
            files=self._files(_IGNORED_PIN, _LIVE_PIN),
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {self._PIN} is #[ignore]d at", err)
        self.assertIn(str(Path("app") / "real.rs") + ":4", err)

        code, err = self._run_main(
            [self._PIN],
            source=self._HASH_SPACE,
            required=[self._PIN],
            files=self._files(_LIVE_PIN, _IGNORED_PIN),
        )
        self.assertEqual(code, 0)
        self.assertNotIn("is #[ignore]d", err)

        code, err = self._run_main(
            [self._PIN],
            source=self._HASH_SPACE,
            required=[self._PIN],
            files=self._files(_LIVE_PIN, _LIVE_PIN),
        )
        self.assertEqual(code, 0)

    def test_bom_prefix_keeps_top_level_wiring(self):
        # rustc accepts a leading BOM; the old read left it on line 1 and
        # the attribute probe silently dropped top-of-file wiring. The BOM
        # sits directly on the wiring line here: the wired source is
        # scanned, its #[ignore] red at its own line, and a live source
        # green.
        top_pin = "commands::tests::critical_pin"
        bom_lib = '\ufeff#[path = "real.rs"]\nmod commands;\n'
        files = {"real.rs": _IGNORED_PIN, "commands.rs": _LIVE_PIN}
        code, err = self._run_main(
            [top_pin], source=bom_lib, required=[top_pin], files=files,
        )
        self.assertEqual(code, 1)
        self.assertIn(f"required pin {top_pin} is #[ignore]d at", err)
        self.assertIn("real.rs:4", err)

        files = {"real.rs": _LIVE_PIN, "commands.rs": _LIVE_PIN}
        code, err = self._run_main(
            [top_pin], source=bom_lib, required=[top_pin], files=files,
        )
        self.assertEqual(code, 0)


class Round21TerseParseContractTests(unittest.TestCase):
    """The registry half of the guard rides on two things no earlier test
    pinned (round-21 review mutations m9/m9b survived the whole suite):
    the `-- --list --format terse` argv tail, and the parse loop that keeps
    only `<path>: test` lines. Every main()-level test replaces list_tests
    wholesale and the one subprocess test feeds empty stdout, so a parser
    that accepted any non-empty line, or an argv that dropped `--format
    terse` (letting libtest's summary line and `: benchmark` entries into
    the registry), stayed green here while CI's real cargo would diverge.
    """

    def _list(self, stdout):
        class FakeResult:
            returncode = 0
            stderr = ""

        FakeResult.stdout = stdout
        captured = {}

        def fake_run(cmd, capture_output, text, env):
            captured["cmd"] = cmd
            return FakeResult()

        with patch.object(guard.subprocess, "run", side_effect=fake_run):
            paths = guard.list_tests()
        return paths, captured["cmd"]

    def test_argv_tail_is_exactly_list_format_terse(self):
        _, cmd = self._list("a::b: test\n")
        self.assertIn("--", cmd)
        self.assertEqual(cmd[cmd.index("--") + 1 :], ["--list", "--format", "terse"])

    def test_parses_terse_test_lines_verbatim(self):
        paths, _ = self._list("alpha::beta::t1: test\nalpha::t2: test\n")
        self.assertEqual(paths, ["alpha::beta::t1", "alpha::t2"])

    def test_only_test_lines_enter_the_registry(self):
        # Shape of the non-terse (pretty) listing: benchmarks, a blank
        # line and libtest's "N tests, M benchmarks" summary. None of them
        # may become a registry path — a parser that keeps any non-empty
        # line would let the summary pass as a phantom test and let a
        # benchmark masquerade as a registered test.
        paths, _ = self._list(
            "alpha::beta::t1: test\n"
            "alpha::bench_x: benchmark\n"
            "\n"
            "1 test, 1 benchmark\n"
        )
        self.assertEqual(paths, ["alpha::beta::t1"])

    def test_raw_identifier_paths_are_kept_as_libtest_prints_them(self):
        # libtest keeps the r# prefix on raw-identifier module segments;
        # the registry side must not normalize it away (the source scanner
        # cannot produce such a path, so a REQUIRED_PIN there hits the
        # attribution fail-closed net instead of matching a stripped key).
        paths, _ = self._list("r4::r#async::t: test\n")
        self.assertEqual(paths, ["r4::r#async::t"])


if __name__ == "__main__":
    unittest.main()

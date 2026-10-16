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
    the wired module.

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
        with self.assertRaises(SystemExit):
            self._ignored_sources(
                {
                    "lib.rs": (
                        "#[path = \"unterminated.rs\n"
                        "mod wired;\n"
                    ),
                    "unterminated.rs": _IGNORED_PIN,
                }
            )

    def test_path_attribute_inside_inline_module_is_not_wired(self):
        # Out of scope by design: a path attribute nested in an inline
        # module resolves against nested module directories; wiring it
        # against the file's directory would attribute wrong paths. The
        # declaration is left unwired (its hits would only matter if a pin
        # lived there, which none does).
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "mod outer {\n"
                    "    #[path = \"inner.rs\"]\n"
                    "    mod inner;\n"
                    "}\n"
                ),
                "inner.rs": _IGNORED_PIN,
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
        # not break the wiring (rustc keeps them attached).
        by_path, unattributable = self._ignored_sources(
            {
                "lib.rs": (
                    "// #[path = \"ghost.rs\"]\n"
                    "// mod ghost;\n"
                    "\n"
                    "#[path = \"wired.rs\"]\n"
                    "// wired through a comment\n"
                    "mod wired;\n"
                ),
                "ghost.rs": _IGNORED_PIN,
                "wired.rs": _IGNORED_PIN,
            }
        )
        self.assertEqual(sorted(by_path), ["wired::tests::critical_pin"])
        self.assertEqual(unattributable, {})


if __name__ == "__main__":
    unittest.main()

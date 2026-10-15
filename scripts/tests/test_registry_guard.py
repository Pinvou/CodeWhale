"""Behavior regression for scripts/test-registry-guard.py.

Review #1518 (rounds 4-6) demonstrated that the guard's pin attribution was
the fragile half: the full-path pin can be satisfied by a same-named test in
another module, and an unrelated module's `#[ignore] fn` can frame a
properly registered pin as silenced. The fixes (full libtest paths in
REQUIRED_PINS, module-aware ignore attribution with a fail-closed fallback)
are pinned here as committed regressions — the review explicitly asked for
the cross-module same-name positive control to live in the tree, not in a
one-off manual run.

`main()` shells out to cargo, so these tests exercise the pure filesystem
half: the source scanner (`ignored_source_pins`) and the pin attribution
(`_pin_ignored_locations` / `_candidate_pin_files`) against a synthetic
crate tree, with the guard module's MANIFEST pointed at it.
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

    def _ignored_sources(self, files: dict[str, str]) -> dict:
        root = self._crate_root(files)
        manifest = str(root / "pinvou3-app" / "src-tauri" / "Cargo.toml")
        with patch.object(guard, "MANIFEST", manifest):
            return guard.ignored_source_pins()

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


class PinAttributionTests(GuardFixture):
    """Review #1518 rounds 4-6: the #[ignore] scan must attribute hits by the
    pin's full module path, not its bare function name."""

    def test_unrelated_module_same_name_ignored_does_not_frame_registered_pin(self):
        # The review reproduction: `real::module::critical_pin` is
        # registered and live, while an UNRELATED module carries a same-named
        # #[ignore]d fn. The pin must stay silent — attribution maps the
        # pin's module path to its own candidate files only.
        root = self._crate_root(
            {"real/module.rs": _LIVE_PIN, "other.rs": _IGNORED_PIN}
        )
        with patch.object(
            guard, "MANIFEST", str(root / "pinvou3-app/src-tauri/Cargo.toml")
        ):
            ignored = guard.ignored_source_pins()
            self.assertEqual(
                [loc.rsplit(":", 1)[0] for loc in ignored["critical_pin"]],
                [self._src_path(root, "other.rs")],
            )
            self.assertEqual(
                guard._pin_ignored_locations(
                    "real::module::critical_pin", ignored
                ),
                [],
            )

    def test_pin_module_file_ignored_is_flagged_at_ignore_line(self):
        # The detection itself: the same layout with the ignore on the pin's
        # own function flags the pin, reporting the ignore-bearing line.
        root = self._crate_root({"real/module.rs": _IGNORED_PIN})
        with patch.object(
            guard, "MANIFEST", str(root / "pinvou3-app/src-tauri/Cargo.toml")
        ):
            self.assertEqual(
                guard._pin_ignored_locations(
                    "real::module::critical_pin", guard.ignored_source_pins()
                ),
                [self._src_path(root, "real/module.rs") + ":4"],
            )

    def test_missing_pin_module_layout_falls_back_fail_closed(self):
        # No candidate file exists for the pin's module path (unconventional
        # layout): the bare-name match applies to every hit instead of
        # silently passing — the guard must fail loud, not blind.
        ignored = self._ignored_sources(
            {
                "stray.rs": (
                    "#[test]\n"
                    "#[ignore]\n"
                    "fn critical_pin() {\n"
                    "    assert!(true);\n"
                    "}\n"
                ),
            }
        )
        self.assertNotEqual(
            guard._pin_ignored_locations(
                "nowhere::defined::critical_pin", ignored
            ),
            [],
        )

    def test_registered_pin_with_no_ignored_sources_stays_silent(self):
        ignored = self._ignored_sources({"real/module.rs": _LIVE_PIN})
        self.assertEqual(
            guard._pin_ignored_locations(
                "real::module::critical_pin", ignored
            ),
            [],
        )


class ScannerParsingTests(GuardFixture):
    """Attribute-run parsing forms the round-6 rewrite claims to cover,
    each pinned so it cannot regress unnoticed."""

    def _ignored(self, text: str) -> dict:
        return self._ignored_sources({"lib.rs": text})

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


class CandidateLayoutTests(GuardFixture):
    def _candidates(self, files: dict[str, str], pin: str) -> list:
        root = self._crate_root(files)
        with patch.object(
            guard, "MANIFEST", str(root / "pinvou3-app/src-tauri/Cargo.toml")
        ):
            return guard._candidate_pin_files(pin)

    def test_inline_tests_rs_layout_resolves(self):
        # app::commands::tests::pin — the REQUIRED_PINS shape: the pin's
        # parent `tests` module is declared inline in commands.rs.
        got = self._candidates({"app/commands.rs": ""}, "app::commands::tests::pin")
        self.assertEqual(len(got), 1)
        self.assertTrue(got[0].endswith("/app/commands.rs"))

    def test_own_file_and_mod_rs_layouts_resolve(self):
        got = self._candidates({"a/b/pin.rs": ""}, "a::b::pin")
        self.assertEqual(len(got), 1)
        self.assertTrue(got[0].endswith("/a/b/pin.rs"))
        got = self._candidates({"c/d/mod.rs": ""}, "c::d::pin")
        self.assertEqual(len(got), 1)
        self.assertTrue(got[0].endswith("/c/d/mod.rs"))

    def test_unconventional_layout_yields_empty_for_fail_closed_fallback(self):
        self.assertEqual(self._candidates({}, "x::y::pin"), [])


if __name__ == "__main__":
    unittest.main()

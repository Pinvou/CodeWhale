import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from collections import Counter
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPT_PATH = REPO_ROOT / "scripts" / "architecture-guard.py"


def load_guard_module():
    spec = importlib.util.spec_from_file_location("architecture_guard", SCRIPT_PATH)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


class ArchitectureGuardUnitTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.guard = load_guard_module()

    def _migration_fixture_reference(self):
        # Immutable integration target used for this migration calibration.
        # It includes frozen main's builtin references; never use a moving ref.
        # generated_from tracks older resources, not the migration target.
        reference = "0841a2d04dcd9aff9eba57c29525a8854694090b"
        self.assertEqual(
            reference,
            subprocess.check_output(
                ["git", "rev-parse", "--verify", f"{reference}^{{commit}}"],
                cwd=REPO_ROOT,
                text=True,
            ).strip(),
        )
        return reference

    def test_count_baseline_detects_both_reduction_and_increase(self):
        failures, progress = self.guard.compare_counts(
            "sample", {"legacy": 2}, {"legacy": 3}
        )
        self.assertEqual([], failures)
        self.assertTrue(progress)

        failures, _ = self.guard.compare_counts(
            "sample", {"legacy": 4, "new": 1}, {"legacy": 3}
        )
        self.assertEqual(2, len(failures))
        self.assertTrue(self.guard.should_fail([], progress))
        self.assertTrue(self.guard.should_fail(failures, []))
        self.assertFalse(self.guard.should_fail([], []))

    def test_cycle_baseline_allows_shrinking_but_not_expansion(self):
        baseline = [["assistant", "files", "knowledge", "remote_control"]]
        self.assertTrue(self.guard.cycle_allowed(["assistant", "knowledge"], baseline))
        self.assertFalse(
            self.guard.cycle_allowed(["assistant", "knowledge", "scheduled"], baseline)
        )
        self.assertFalse(self.guard.cycle_allowed(["assistant"], baseline))

    def test_baseline_ratchet_allows_tightening_but_rejects_new_allowance(self):
        previous = {
            "schema_version": 1,
            "rules": {"rule": {"old": 3}},
            "rust_feature_cycles": [["a", "b", "c"]],
        }
        tighter = {
            "schema_version": 1,
            "rules": {"rule": {"old": 2}},
            "rust_feature_cycles": [["a", "b"]],
        }
        self.assertEqual([], self.guard.compare_baseline_ratchet(tighter, previous))

        wider = {
            "schema_version": 1,
            "rules": {"rule": {"old": 4, "new": 1}},
            "rust_feature_cycles": [["a", "b", "c", "d"]],
        }
        failures = self.guard.compare_baseline_ratchet(wider, previous)
        self.assertEqual(3, len(failures))

    def test_frontend_scanner_rejects_feature_to_app_and_native_tauri(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            feature = root / "pinvou3-app/src/features/chat/view.jsx"
            app = root / "pinvou3-app/src/app/helper.js"
            platform = root / "pinvou3-app/src/platform/tauri/client.js"
            feature.parent.mkdir(parents=True)
            app.parent.mkdir(parents=True)
            platform.parent.mkdir(parents=True)
            feature.write_text(
                'import {\n  helper\n} from "../../app/helper.js";\n'
                "window.__TAURI__.core.invoke('x');\n"
                "const os = navigator.platform;\n",
                encoding="utf-8",
            )
            app.write_text("export default {};\n", encoding="utf-8")
            platform.write_text("window.__TAURI__.core.invoke('allowed');\n", encoding="utf-8")

            rules = self.guard.scan_frontend(root)
            self.assertEqual(1, sum(rules["frontend_feature_imports_app"].values()))
            self.assertEqual(1, sum(rules["frontend_tauri_global_outside_platform"].values()))
            self.assertEqual(1, sum(rules["frontend_user_agent_platform_detection"].values()))

    def test_rust_scanner_finds_upward_dependency_cycle_and_platform_leaks(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            feature_a = rust_root / "features/a/mod.rs"
            feature_b = rust_root / "features/b/mod.rs"
            platform = rust_root / "features/a/platform/windows.rs"
            platform_service = rust_root / "platform/service.rs"
            app_bridge = rust_root / "app/bridge/mod.rs"
            for path in [feature_a, feature_b, platform, platform_service, app_bridge]:
                path.parent.mkdir(parents=True, exist_ok=True)
            app_bridge.write_text("pub fn notify() {}\n", encoding="utf-8")
            (rust_root / "lib.rs").write_text(
                '#[path = "app/bridge/mod.rs"]\nmod bridge;\n'
                '#[path = "features/a/mod.rs"]\nmod feature_a;\n'
                '#[path = "features/b/mod.rs"]\nmod feature_b;\n'
                'fn handler() { tauri::generate_handler![commands::ok, features::a::leak,]; }\n',
                encoding="utf-8",
            )
            feature_a.write_text(
                '#[cfg(target_os = "windows")]\n'
                '#[tauri::command]\n'
                '#[cfg(any(windows, target_arch = "aarch64"))]\n'
                '#[cfg_attr(all(unix, target_family = "unix"), allow(dead_code))]\n'
                'fn cfg_macro() { if cfg!(target_env = "msvc") {} }\n'
                "use crate::{bridge, feature_b};\n"
                'fn leaked_platform_detail() { Command::new("powershell.exe"); }\n'
                "fn upward() { bridge::notify(); feature_b::run(); }\n",
                encoding="utf-8",
            )
            feature_b.write_text(
                "fn backward() { crate::feature_a::run(); }\n", encoding="utf-8"
            )
            platform.write_text(
                '#[cfg(target_os = "windows")]\n'
                'fn allowed() { Command::new("xdg-open"); }\n',
                encoding="utf-8",
            )
            platform_service.write_text(
                "fn reverse() { crate::feature_a::run(); }\n", encoding="utf-8"
            )

            rules, cycles = self.guard.scan_rust(root)
            self.assertEqual(1, rules["rust_feature_depends_on_app"]["a->bridge"])
            self.assertEqual(
                1,
                rules["rust_platform_depends_on_upper_layer"][
                    "pinvou3-app/src-tauri/src/platform/service.rs->features::a"
                ],
            )
            self.assertEqual(
                1,
                rules["rust_tauri_handler_outside_app"][
                    "pinvou3-app/src-tauri/src/lib.rs:features::a::leak"
                ],
            )
            self.assertEqual([["a", "b"]], cycles)
            self.assertEqual(1, rules["rust_cyclic_feature_dependencies"]["a->b"])
            self.assertEqual(1, rules["rust_cyclic_feature_dependencies"]["b->a"])
            self.assertEqual(
                6,
                rules["rust_target_cfg_outside_adapter"][
                    "pinvou3-app/src-tauri/src/features/a/mod.rs"
                ],
            )
            self.assertNotIn(
                "pinvou3-app/src-tauri/src/features/a/platform/windows.rs",
                rules["rust_target_cfg_outside_adapter"],
            )
            self.assertEqual(
                1,
                rules["rust_platform_details_outside_adapter"][
                    "pinvou3-app/src-tauri/src/features/a/mod.rs"
                ],
            )
            self.assertNotIn(
                "pinvou3-app/src-tauri/src/features/a/platform/windows.rs",
                rules["rust_platform_details_outside_adapter"],
            )
            self.assertEqual(
                1,
                rules["rust_tauri_commands_outside_app"][
                    "pinvou3-app/src-tauri/src/features/a/mod.rs"
                ],
            )

    def test_crate_root_aliases_resolve_across_grouped_and_nested_use_trees(self):
        cases = [
            (
                "use crate::app as host; host::commands::run();",
                [("app", "commands")],
            ),
            (
                "use crate::features as domains; domains::sessions::run();",
                [("features", "sessions")],
            ),
            (
                "use crate::{app as host, features as domains};\n"
                "host::commands::run(); domains::sessions::run();",
                [("app", "commands"), ("features", "sessions")],
            ),
            (
                "use crate::{app::{self as host}, features::{self as domains}};\n"
                "host::commands::run(); domains::sessions::run();",
                [("app", "commands"), ("features", "sessions")],
            ),
            (
                "use {crate::app as host, crate::features as domains};\n"
                "use domains::{sessions as local, assistant};\n"
                "host::commands::run();",
                [("features", "sessions"), ("features", "assistant"), ("app", "commands")],
            ),
            (
                "use crate as root; root::app::commands::run();",
                [("app", "commands")],
            ),
            (
                "use crate::app as r#host; r#host::commands::run();",
                [("app", "commands")],
            ),
            (
                "crate::r#app::r#commands::run(); crate::r#features::r#sessions::run();",
                [("app", "commands"), ("features", "sessions")],
            ),
            (
                "extern crate self as own; own::app::commands::run();",
                [("app", "commands")],
            ),
            (
                "use crate::app as host; use self::host as h; h::commands::run();",
                [("app", "commands")],
            ),
            (
                "use crate::app as host; use self::{host as h}; h::commands::run();",
                [("app", "commands")],
            ),
            (
                "use crate::features as domains; use self::domains as upper; "
                "upper::sessions::run();",
                [("features", "sessions")],
            ),
            (
                "use crate::features as domains; use domains::*;",
                [("features", "*")],
            ),
            (
                "use crate::features::{sessions};",
                [("features", "sessions")],
            ),
            (
                "use crate::features as domains; use domains::{sessions};",
                [("features", "sessions")],
            ),
        ]
        for source, expected in cases:
            with self.subTest(source=source):
                references = self.guard.crate_references(source, {})
                self.assertEqual(
                    expected,
                    [ref for ref in references if ref[1] != self.guard.ROOT_MODULE],
                )

        self.assertEqual(
            [],
            self.guard.crate_references(
                '// use crate::app as host;\nconst NOTE: &str = "host::commands";', {}
            ),
        )

    def test_root_aliases_cannot_hide_feature_to_app_or_feature_cycles(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            remote = rust_root / "features/remote_control/mod.rs"
            sessions = rust_root / "features/sessions/mod.rs"
            remote.parent.mkdir(parents=True)
            sessions.parent.mkdir(parents=True)
            remote.write_text(
                "use crate::{app as host, features as domains};\n"
                "fn run() { host::commands::run(); domains::sessions::run(); }\n",
                encoding="utf-8",
            )
            sessions.write_text(
                "use crate::features as domains;\n"
                "fn run() { domains::remote_control::run(); }\n",
                encoding="utf-8",
            )
            rules, cycles = self.guard.scan_rust(root)
            self.assertEqual(
                1, rules["rust_feature_depends_on_app"]["remote_control->commands"]
            )
            self.assertEqual(
                1, rules["rust_feature_depends_on_app"]["remote_control-><root>"]
            )
            self.assertEqual([["remote_control", "sessions"]], cycles)
            self.assertEqual(
                1, rules["rust_cyclic_feature_dependencies"]["remote_control->sessions"]
            )
            self.assertEqual(
                1, rules["rust_cyclic_feature_dependencies"]["sessions->remote_control"]
            )
            failures, _ = self.guard.compare(
                {"schema_version": 1, "rules": rules, "rust_feature_cycles": cycles},
                {"schema_version": 1, "rules": {}, "rust_feature_cycles": []},
            )
            self.assertTrue(any("rust_feature_depends_on_app" in item for item in failures))
            self.assertTrue(any("rust_feature_cycles" in item for item in failures))

            remote.write_text(
                "use crate::features as domains; use domains::*;\n",
                encoding="utf-8",
            )
            rules, _ = self.guard.scan_rust(root)
            self.assertGreaterEqual(
                rules["rust_crate_root_glob_imports"][
                    "pinvou3-app/src-tauri/src/features/remote_control/mod.rs"
                ],
                1,
            )

            for app_ref in [
                "crate::r#app::commands::run();",
                "extern crate self as own; own::app::commands::run();",
                "use crate::app as host; use self::host as h; h::commands::run();",
                "use crate::app as host; use self::{host as h}; h::commands::run();",
            ]:
                with self.subTest(app_ref=app_ref):
                    remote.write_text(app_ref, encoding="utf-8")
                    rules, _ = self.guard.scan_rust(root)
                    self.assertEqual(
                        1, rules["rust_feature_depends_on_app"]["remote_control->commands"]
                    )

            remote.write_text(
                "use crate::features as domains; use self::domains as d; "
                "d::sessions::run();",
                encoding="utf-8",
            )
            rules, cycles = self.guard.scan_rust(root)
            self.assertEqual([["remote_control", "sessions"]], cycles)

    def test_grouped_feature_cycle_debt_tracks_reference_semantics(self):
        baseline = json.loads(
            (REPO_ROOT / "scripts/architecture-baseline.json").read_text(encoding="utf-8")
        )
        reference = self._migration_fixture_reference()
        self.assertNotIn("rust_grouped_feature_import_sites", baseline["rules"])
        self.assertEqual(
            self.guard.RUST_DEPENDENCY_MIGRATION_ANCHOR,
            baseline["migration_anchors"][self.guard.RUST_DEPENDENCY_MIGRATION],
        )

        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            assistant = root / "pinvou3-app/src-tauri/src/features/assistant/mod.rs"
            knowledge = root / "pinvou3-app/src-tauri/src/features/knowledge/mod.rs"
            assistant.parent.mkdir(parents=True)
            knowledge.parent.mkdir(parents=True)
            declaration = "use crate::features::{knowledge};\n"
            assistant.write_text(declaration, encoding="utf-8")
            knowledge.write_text("use crate::features::{assistant};\n", encoding="utf-8")

            def scanned_state():
                rules, cycles = self.guard.scan_rust(root)
                return {
                    "schema_version": 1,
                    "rules": {name: dict(counts) for name, counts in rules.items()},
                    "rust_feature_cycles": cycles,
                }

            original = scanned_state()
            self.assertEqual([["assistant", "knowledge"]], original["rust_feature_cycles"])
            self.assertEqual(
                {"assistant->knowledge": 1, "knowledge->assistant": 1},
                original["rules"]["rust_cyclic_feature_dependencies"],
            )

            assistant.write_text(
                "// unrelated header\n\nuse std::collections::BTreeMap;\n"
                "const N: usize = 1;\nstruct Marker;\n" + declaration,
                encoding="utf-8",
            )
            self.assertEqual(([], []), self.guard.compare(scanned_state(), original))

            assistant.write_text(
                "const N: usize = 1;\n" + declaration + "struct Marker;\n",
                encoding="utf-8",
            )
            self.assertEqual(([], []), self.guard.compare(scanned_state(), original))

            assistant.write_text(
                "use crate::features as domains; use domains::{knowledge};\n",
                encoding="utf-8",
            )
            failures, _ = self.guard.compare(scanned_state(), original)
            self.assertTrue(any("rust_crate_root_glob_imports" in item for item in failures))

            assistant.write_text(declaration * 2, encoding="utf-8")
            failures, _ = self.guard.compare(scanned_state(), original)
            self.assertTrue(any("assistant->knowledge" in item for item in failures))

            assistant.write_text(declaration, encoding="utf-8")
            extra = assistant.parent / "extra.rs"
            extra.write_text(declaration, encoding="utf-8")
            failures, _ = self.guard.compare(scanned_state(), original)
            self.assertTrue(any("assistant->knowledge" in item for item in failures))
            extra.unlink()

            assistant.write_text("use crate::features::{sessions};\n", encoding="utf-8")
            failures, progress = self.guard.compare(scanned_state(), original)
            self.assertTrue(self.guard.should_fail(failures, progress))
            self.assertTrue(any("assistant->knowledge" in item for item in progress))

            assistant.write_text(
                "// use crate::features::{knowledge};\n"
                'const SAMPLE: &str = r#"use crate::features::{knowledge};"#;\n'
                + declaration,
                encoding="utf-8",
            )
            self.assertEqual(([], []), self.guard.compare(scanned_state(), original))

        previous = json.loads(
            subprocess.check_output(
                ["git", "show", f"{reference}:scripts/architecture-baseline.json"],
                cwd=REPO_ROOT,
                text=True,
            )
        )
        anchor = self.guard.RUST_DEPENDENCY_MIGRATION_ANCHOR
        self.assertTrue(self.guard.migration_anchor_is_ancestor(REPO_ROOT, anchor))
        merge_state, anchor_state, target_state = (
            self.guard.migration_semantic_states_from_git(REPO_ROOT, anchor, reference)
        )
        migration_limits, migration_cycles = self.guard.merged_migration_limits(
            merge_state, anchor_state, target_state
        )
        self.assertEqual(
            dict(migration_limits["rust_cyclic_feature_dependencies"]),
            baseline["rules"]["rust_cyclic_feature_dependencies"],
        )
        self.assertEqual(
            dict(migration_limits["rust_platform_depends_on_upper_layer"]),
            baseline["rules"]["rust_platform_depends_on_upper_layer"],
        )
        self.assertEqual(migration_cycles, baseline["rust_feature_cycles"])
        self.assertEqual([], self.guard.compare_baseline_ratchet(
            baseline, previous, merge_state, anchor_state, target_state
        ))

        # These extra allowances are valid only when the target actually owns
        # frozen main's builtin references. The old 57/47 calibration must
        # still reject a candidate claiming 58/50; no global exemption is added.
        stale_target_edges = Counter(target_state[2])
        stale_target_edges[("assistant", "marketplace")] -= 1
        stale_target_edges[("runtime_bundle", "marketplace")] -= 3
        stale_failures = self.guard.compare_baseline_ratchet(
            baseline, previous, merge_state, anchor_state,
            (target_state[0], target_state[1], stale_target_edges),
        )
        for edge in ("assistant->marketplace", "runtime_bundle->marketplace"):
            self.assertTrue(any(edge in failure for failure in stale_failures))

        expanded = json.loads(json.dumps(baseline))
        expanded["rules"]["rust_cyclic_feature_dependencies"]["assistant->knowledge"] += 1
        self.assertTrue(self.guard.compare_baseline_ratchet(
            expanded, previous, merge_state, anchor_state, target_state
        ))
        expanded = json.loads(json.dumps(baseline))
        expanded["rust_feature_cycles"].append(["assistant", "sessions"])
        self.assertTrue(self.guard.compare_baseline_ratchet(
            expanded, previous, merge_state, anchor_state, target_state
        ))

    def test_baseline_rule_presence_and_cycle_migration_are_monotonic(self):
        current = json.loads(
            (REPO_ROOT / "scripts/architecture-baseline.json").read_text(encoding="utf-8")
        )
        # Freeze the same calibration fixture; future-main behavior is covered
        # by test_migration_history_uses_three_way_feature_edge_counts.
        reference = self._migration_fixture_reference()
        host_rule = "rust_remote_control_host_api"
        cycle_rule = "rust_cyclic_feature_dependencies"

        host_missing = json.loads(json.dumps(current))
        del host_missing["rules"][host_rule]
        self.assertEqual([], self.guard.compare_baseline_ratchet(current, host_missing))

        host_empty = json.loads(json.dumps(current))
        host_empty["rules"][host_rule] = {}
        self.assertTrue(
            any(host_rule in item for item in self.guard.compare_baseline_ratchet(current, host_empty))
        )

        cycle_empty = json.loads(json.dumps(current))
        cycle_empty["rules"][cycle_rule] = {}
        cycle_empty["rust_feature_cycles"] = []
        self.assertTrue(
            any(cycle_rule in item for item in self.guard.compare_baseline_ratchet(current, cycle_empty))
        )

        # The one-time migration is tied to relevant debt and the marker,
        # independent of generated_from and unrelated future-main counters.
        unmarked_empty = json.loads(json.dumps(cycle_empty))
        del unmarked_empty["migrations"]
        del unmarked_empty["migration_anchors"]
        unmarked_empty["generated_from"] = "future-main-unrelated-commit"
        unmarked_empty["rules"]["common_platform_binaries"] = {"old": 1}
        self.assertTrue(self.guard.compare_baseline_ratchet(current, unmarked_empty))
        merge_state, anchor_state, target_state = (
            self.guard.migration_semantic_states_from_git(
                REPO_ROOT, self.guard.RUST_DEPENDENCY_MIGRATION_ANCHOR, reference
            )
        )
        self.assertTrue(self.guard.compare_baseline_ratchet(
            current, unmarked_empty, merge_state
        ))
        self.assertEqual([], self.guard.compare_baseline_ratchet(
            current, unmarked_empty, merge_state, anchor_state, target_state
        ))
        reduced_edges = Counter(target_state[2])
        reduced_edges[("assistant", "remote_control")] -= 1
        self.assertTrue(self.guard.compare_baseline_ratchet(
            current, unmarked_empty, merge_state, anchor_state,
            (target_state[0], target_state[1], reduced_edges),
        ))
        self.assertTrue(
            self.guard.compare_baseline_ratchet(
                current, unmarked_empty, merge_state, anchor_state, ({}, [], Counter())
            )
        )
        removed_marker = json.loads(json.dumps(cycle_empty))
        del removed_marker["migrations"]
        self.assertTrue(
            any("marker removed" in item for item in self.guard.compare_baseline_ratchet(
                removed_marker, current
            ))
        )
        changed_anchor = json.loads(json.dumps(current))
        changed_anchor["migration_anchors"][self.guard.RUST_DEPENDENCY_MIGRATION] = "0" * 40
        self.assertTrue(
            any("anchor changed" in item for item in self.guard.compare_baseline_ratchet(
                changed_anchor, current
            ))
        )
        self.assertFalse(self.guard.migration_anchor_is_ancestor(REPO_ROOT, "0" * 40))

    def test_cycle_migration_requires_real_edges_in_target_source(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src/features"
            assistant = rust_root / "assistant/mod.rs"
            knowledge = rust_root / "knowledge/mod.rs"
            for path in (assistant, knowledge):
                path.parent.mkdir(parents=True)
            assistant.write_text("use crate::features::{knowledge};\n", encoding="utf-8")
            knowledge.write_text("use crate::features::{assistant};\n", encoding="utf-8")
            subprocess.run(["git", "init", "-q"], cwd=root, check=True)
            subprocess.run(["git", "add", "."], cwd=root, check=True)
            subprocess.run(
                ["git", "-c", "user.name=Guard Test", "-c", "user.email=guard@example.invalid",
                 "commit", "-qm", "old source"],
                cwd=root,
                check=True,
            )
            old_ref = subprocess.check_output(
                ["git", "rev-parse", "HEAD"], cwd=root, text=True
            ).strip()
            previous = {
                "schema_version": 1,
                "rules": {"rust_cyclic_feature_dependencies": {}},
                "rust_feature_cycles": [],
            }
            candidate = {
                "schema_version": 1,
                "migrations": [self.guard.RUST_DEPENDENCY_MIGRATION],
                "migration_anchors": {
                    self.guard.RUST_DEPENDENCY_MIGRATION:
                    self.guard.RUST_DEPENDENCY_MIGRATION_ANCHOR,
                },
                "rules": {"rust_cyclic_feature_dependencies": {
                    "assistant->knowledge": 1,
                    "knowledge->assistant": 1,
                }},
                "rust_feature_cycles": [["assistant", "knowledge"]],
            }
            old_evidence = self.guard.rust_state_from_git(root, old_ref)
            self.assertEqual(
                Counter({"assistant->knowledge": 1, "knowledge->assistant": 1}),
                old_evidence[0]["rust_cyclic_feature_dependencies"],
            )
            self.assertEqual(
                [], self.guard.compare_baseline_ratchet(
                    candidate, previous, old_evidence, old_evidence, old_evidence
                )
            )
            self.assertTrue(self.guard.compare_baseline_ratchet(candidate, previous))

            unrelated = rust_root / "other/mod.rs"
            unrelated.parent.mkdir(parents=True)
            unrelated.write_text("pub fn marker() {}\n", encoding="utf-8")
            subprocess.run(["git", "add", "."], cwd=root, check=True)
            subprocess.run(
                ["git", "-c", "user.name=Guard Test", "-c", "user.email=guard@example.invalid",
                 "commit", "-qm", "unrelated source"],
                cwd=root,
                check=True,
            )
            unrelated_state = self.guard.rust_state_from_git(root, "HEAD")
            self.assertEqual([], self.guard.compare_baseline_ratchet(
                candidate, previous, old_evidence, old_evidence, unrelated_state
            ))

            assistant.write_text("pub fn run() {}\n", encoding="utf-8")
            knowledge.write_text("pub fn run() {}\n", encoding="utf-8")
            subprocess.run(["git", "add", "."], cwd=root, check=True)
            subprocess.run(
                ["git", "-c", "user.name=Guard Test", "-c", "user.email=guard@example.invalid",
                 "commit", "-qm", "cleared source"],
                cwd=root,
                check=True,
            )
            cleared_evidence = self.guard.rust_state_from_git(root, "HEAD")
            self.assertFalse(cleared_evidence[0]["rust_cyclic_feature_dependencies"])
            self.assertTrue(
                self.guard.compare_baseline_ratchet(
                    candidate, previous, old_evidence, old_evidence, cleared_evidence
                )
            )

    def test_migration_history_uses_three_way_feature_edge_counts(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            feature_root = root / "pinvou3-app/src-tauri/src/features"
            assistant = feature_root / "assistant/mod.rs"
            knowledge = feature_root / "knowledge/mod.rs"
            for path in (assistant, knowledge):
                path.parent.mkdir(parents=True)
            subprocess.run(["git", "init", "-q"], cwd=root, check=True)

            def commit(message):
                subprocess.run(["git", "add", "."], cwd=root, check=True)
                subprocess.run(
                    ["git", "-c", "user.name=Guard Test", "-c", "user.email=guard@example.invalid",
                     "commit", "-qm", message],
                    cwd=root,
                    check=True,
                )
                return subprocess.check_output(
                    ["git", "rev-parse", "HEAD"], cwd=root, text=True
                ).strip()

            forward = "use crate::features::{knowledge};\n"
            backward = "use crate::features::{assistant};\n"
            assistant.write_text(forward * 5, encoding="utf-8")
            knowledge.write_text(backward, encoding="utf-8")
            old_main = commit("common ancestor with five references")
            assistant.write_text(forward * 9, encoding="utf-8")
            knowledge.write_text(backward * 2, encoding="utf-8")
            anchor = commit("reviewed task anchor")
            previous = {
                "schema_version": 1,
                "rules": {"rust_cyclic_feature_dependencies": {}},
                "rust_feature_cycles": [],
            }
            candidate_template = {
                "schema_version": 1,
                "migrations": [self.guard.RUST_DEPENDENCY_MIGRATION],
                "migration_anchors": {
                    self.guard.RUST_DEPENDENCY_MIGRATION:
                    self.guard.RUST_DEPENDENCY_MIGRATION_ANCHOR,
                },
                "rules": {"rust_cyclic_feature_dependencies": {}},
                "rust_feature_cycles": [["assistant", "knowledge"]],
            }
            def check(target_ref, allowed_forward, allowed_backward):
                merge, task_anchor, target = self.guard.migration_semantic_states_from_git(
                    root, anchor, target_ref
                )
                limits, cycles = self.guard.merged_migration_limits(merge, task_anchor, target)
                self.assertEqual(allowed_forward, limits["rust_cyclic_feature_dependencies"]["assistant->knowledge"])
                self.assertEqual(allowed_backward, limits["rust_cyclic_feature_dependencies"]["knowledge->assistant"])
                self.assertEqual([["assistant", "knowledge"]], cycles)
                candidate = json.loads(json.dumps(candidate_template))
                candidate["rules"]["rust_cyclic_feature_dependencies"] = {
                    "assistant->knowledge": allowed_forward,
                    "knowledge->assistant": allowed_backward,
                }
                self.assertEqual([], self.guard.compare_baseline_ratchet(
                    candidate, previous, merge, task_anchor, target
                ))
                candidate["rules"]["rust_cyclic_feature_dependencies"]["assistant->knowledge"] += 1
                self.assertTrue(self.guard.compare_baseline_ratchet(
                    candidate, previous, merge, task_anchor, target
                ))

            # T=M: branch-reviewed net changes (+4, +1) remain eligible.
            check(old_main, 9, 2)

            subprocess.run(["git", "checkout", "-qb", "future", old_main], cwd=root, check=True)
            unrelated = feature_root / "other/mod.rs"
            unrelated.parent.mkdir(parents=True)
            unrelated.write_text("pub fn other() {}\n", encoding="utf-8")
            future_unrelated = commit("future main unrelated change")
            subprocess.run(["git", "checkout", "--detach", "-q", anchor], cwd=root, check=True)
            check(future_unrelated, 9, 2)

            subprocess.run(["git", "checkout", "-q", "future"], cwd=root, check=True)
            assistant.write_text(forward * 3, encoding="utf-8")
            future_partial = commit("future main cleared two references")
            subprocess.run(["git", "checkout", "--detach", "-q", anchor], cwd=root, check=True)
            check(future_partial, 7, 2)

            subprocess.run(["git", "checkout", "-q", "future"], cwd=root, check=True)
            assistant.write_text("pub fn run() {}\n", encoding="utf-8")
            knowledge.write_text("pub fn run() {}\n", encoding="utf-8")
            future_cleared = commit("future main cleared all debt")
            subprocess.run(["git", "checkout", "--detach", "-q", anchor], cwd=root, check=True)
            check(future_cleared, 4, 1)

            subprocess.run(["git", "checkout", "-q", "future"], cwd=root, check=True)
            assistant.write_text(forward * 7, encoding="utf-8")
            knowledge.write_text(backward, encoding="utf-8")
            future_added = commit("future main added references")
            subprocess.run(["git", "checkout", "--detach", "-q", anchor], cwd=root, check=True)
            check(future_added, 11, 2)

            # A branch-side reduction is a negative net delta, not fresh allowance.
            subprocess.run(["git", "checkout", "-qb", "reduced", old_main], cwd=root, check=True)
            assistant.write_text(forward * 3, encoding="utf-8")
            reduced_anchor_ref = commit("reviewed branch reduced references")
            merge, task_anchor, target = self.guard.migration_semantic_states_from_git(
                root, reduced_anchor_ref, future_added
            )
            limits, _ = self.guard.merged_migration_limits(merge, task_anchor, target)
            self.assertEqual(5, limits["rust_cyclic_feature_dependencies"]["assistant->knowledge"])

            subprocess.run(["git", "checkout", "--detach", "-q", old_main], cwd=root, check=True)
            with self.assertRaisesRegex(ValueError, "not a candidate HEAD ancestor"):
                self.guard.migration_semantic_states_from_git(root, anchor, "HEAD")

    def test_super_module_paths_and_underscore_imports_are_checked_in_scan(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            sources = {
                "features/alpha/mod.rs": (
                    "use super::{r#beta as _};\n"
                    "use crate::{app::commands as _};\n"
                ),
                "features/alpha/nested.rs": (
                    "use super::super::{beta as _};\n"
                    "use super::sibling as _;\n"
                    "super::sibling::run();\n"
                    "use crate::app::{commands as _};\n"
                ),
                "features/alpha/deep/mod.rs": "use super::super::beta as _;\n",
                "features/alpha/deep/leaf.rs": (
                    "fn run() { super::super::super::beta::run(); }\n"
                ),
                "features/beta/mod.rs": "use super::alpha as _;\n",
            }
            for name, code in sources.items():
                path = rust_root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(code, encoding="utf-8")
            self.assertEqual(
                ("features", "alpha", "deep"),
                self.guard.rust_module_path(rust_root / "features/alpha/deep/mod.rs", rust_root),
            )
            self.assertEqual(
                ("features", "alpha", "nested"),
                self.guard.rust_module_path(rust_root / "features/alpha/nested.rs", rust_root),
            )
            rules, cycles = self.guard.scan_rust(root)
            self.assertEqual(2, rules["rust_feature_depends_on_app"]["alpha->commands"])
            self.assertEqual([["alpha", "beta"]], cycles)
            self.assertEqual(4, rules["rust_cyclic_feature_dependencies"]["alpha->beta"])
            self.assertEqual(1, rules["rust_cyclic_feature_dependencies"]["beta->alpha"])
            self.assertNotIn("alpha->sibling", rules["rust_cyclic_feature_dependencies"])
            failures, _ = self.guard.compare(
                {"schema_version": 1, "rules": rules, "rust_feature_cycles": cycles},
                {"schema_version": 1, "rules": {}, "rust_feature_cycles": []},
            )
            self.assertTrue(any("rust_feature_depends_on_app" in item for item in failures))
            self.assertTrue(any("rust_feature_cycles" in item for item in failures))

    def test_root_module_imports_count_without_local_use(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            feature = rust_root / "features/alpha/mod.rs"
            platform = rust_root / "platform/service.rs"
            feature.parent.mkdir(parents=True)
            platform.parent.mkdir(parents=True)
            feature.write_text(
                "use crate::{app as host, features as domains, platform as os};\n"
                "mod child { fn run() { super::host::commands::run(); } }\n",
                encoding="utf-8",
            )
            platform.write_text(
                "use crate::app as host; use crate::features as domains;\n",
                encoding="utf-8",
            )
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(1, rules["rust_feature_depends_on_app"]["alpha-><root>"])
            self.assertEqual(
                1,
                rules["rust_crate_root_glob_imports"][
                    "pinvou3-app/src-tauri/src/features/alpha/mod.rs"
                ],
            )
            self.assertEqual(
                1,
                rules["rust_platform_depends_on_upper_layer"][
                    "pinvou3-app/src-tauri/src/platform/service.rs->app::<root>"
                ],
            )
            self.assertEqual(
                1,
                rules["rust_platform_depends_on_upper_layer"][
                    "pinvou3-app/src-tauri/src/platform/service.rs->features::<root>"
                ],
            )
            failures, _ = self.guard.compare(
                {"schema_version": 1, "rules": rules, "rust_feature_cycles": []},
                {"schema_version": 1, "rules": {}, "rust_feature_cycles": []},
            )
            self.assertTrue(any("rust_feature_depends_on_app" in item for item in failures))
            self.assertTrue(any("rust_crate_root_glob_imports" in item for item in failures))

    def test_path_modules_resolve_super_from_declared_module_not_disk_layout(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            sources = {
                "features/alpha/mod.rs": "mod engine;\n",
                "features/alpha/engine.rs": '#[path = "child.rs"] mod child;\n',
                "features/alpha/child.rs": (
                    '#[path = "grandchild.rs"] mod grandchild;\n'
                    "use super::super::super::beta as _;\n"
                    "use super::super::local as _;\n"
                ),
                "features/alpha/grandchild.rs": "use super::super::super::super::beta as _;\n",
                "features/beta/mod.rs": "use super::alpha as _;\n",
            }
            for name, code in sources.items():
                path = rust_root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(code, encoding="utf-8")
            files = list(rust_root.rglob("*.rs"))
            paths, ambiguous, _ = self.guard.rust_module_paths(files, rust_root)
            self.assertFalse(ambiguous)
            self.assertEqual(
                ("features", "alpha", "engine", "child"),
                paths[(rust_root / "features/alpha/child.rs").resolve()],
            )
            self.assertEqual(
                ("features", "alpha", "engine", "child", "grandchild"),
                paths[(rust_root / "features/alpha/grandchild.rs").resolve()],
            )
            rules, cycles = self.guard.scan_rust(root)
            self.assertEqual([["alpha", "beta"]], cycles)
            self.assertEqual(2, rules["rust_cyclic_feature_dependencies"]["alpha->beta"])
            self.assertEqual(1, rules["rust_cyclic_feature_dependencies"]["beta->alpha"])
            self.assertFalse(rules["rust_module_path_ambiguous"])

    def test_super_glob_only_fails_when_scope_resolves_to_crate_layer_root(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            feature = rust_root / "features/alpha/mod.rs"
            tests = rust_root / "features/alpha_tests.rs"
            feature.parent.mkdir(parents=True)
            feature.write_text(
                "use super::*; use super::{*};\n"
                "#[cfg(test)] mod tests {\n"
                "  use super::*;\n"
                '  include!(concat!(env!("CARGO_MANIFEST_DIR"), "/src/features/alpha_tests.rs"));\n'
                "}\n",
                encoding="utf-8",
            )
            tests.write_text("use super::*;\n", encoding="utf-8")
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(
                2,
                rules["rust_crate_root_glob_imports"][
                    "pinvou3-app/src-tauri/src/features/alpha/mod.rs"
                ],
            )
            self.assertNotIn(
                "pinvou3-app/src-tauri/src/features/alpha_tests.rs",
                rules["rust_crate_root_glob_imports"],
            )
            failures, _ = self.guard.compare(
                {"schema_version": 1, "rules": rules, "rust_feature_cycles": []},
                {"schema_version": 1, "rules": {}, "rust_feature_cycles": []},
            )
            self.assertTrue(any("rust_crate_root_glob_imports" in item for item in failures))

    def test_unresolved_inline_path_module_fails_closed(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            parent = rust_root / "features/alpha/mod.rs"
            child = rust_root / "features/alpha/child.rs"
            parent.parent.mkdir(parents=True)
            parent.write_text(
                'mod nested { #[path = "child.rs"] mod child; }\n',
                encoding="utf-8",
            )
            child.write_text("use super::*;\n", encoding="utf-8")
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(
                1,
                rules["rust_module_path_ambiguous"][
                    "pinvou3-app/src-tauri/src/features/alpha/mod.rs"
                ],
            )

    def test_cfg_attr_paths_scan_all_branches_and_reject_unknown_targets(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            parent = rust_root / "features/alpha/mod.rs"
            win = rust_root / "features/alpha/win.rs"
            unix = rust_root / "features/alpha/unix.rs"
            beta = rust_root / "features/beta/mod.rs"
            for path in (parent, win, unix, beta):
                path.parent.mkdir(parents=True, exist_ok=True)
            parent.write_text(
                '#[cfg_attr(windows, path = r"win.rs")]\n'
                '#[cfg_attr(not(windows), path = "unix.rs")]\n'
                'mod child;\n',
                encoding="utf-8",
            )
            win.write_text("use super::super::beta as _;\n", encoding="utf-8")
            unix.write_text("use super::super::beta as _;\n", encoding="utf-8")
            beta.write_text("use super::alpha as _;\n", encoding="utf-8")
            rules, cycles = self.guard.scan_rust(root)
            self.assertFalse(rules["rust_module_path_ambiguous"])
            self.assertEqual([["alpha", "beta"]], cycles)
            self.assertEqual(2, rules["rust_cyclic_feature_dependencies"]["alpha->beta"])

            parent.write_text(
                '#[cfg_attr(windows, path = r"win.rs", path = "../../outside.rs")]\n'
                'mod child;\n',
                encoding="utf-8",
            )
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(
                1,
                rules["rust_module_path_ambiguous"][
                    "pinvou3-app/src-tauri/src/features/alpha/mod.rs"
                ],
            )

    def test_raw_path_root_alias_and_raw_include_suffix_are_resolved(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            lib = rust_root / "lib.rs"
            app = rust_root / "app/mod.rs"
            feature = rust_root / "features/alpha/mod.rs"
            tests = rust_root / "features/alpha_tests.rs"
            for path in (lib, app, feature, tests):
                path.parent.mkdir(parents=True, exist_ok=True)
            lib.write_text('#[path = r"app/mod.rs"] mod host;\n', encoding="utf-8")
            app.write_text("pub mod commands;\n", encoding="utf-8")
            feature.write_text(
                "fn run() { crate::host::commands::run(); }\n"
                "#[cfg(test)] mod tests {\n"
                '  include!(concat!(env!("CARGO_MANIFEST_DIR"), r#"/src/features/alpha_tests.rs"#));\n'
                "}\n",
                encoding="utf-8",
            )
            tests.write_text("use super::*;\n", encoding="utf-8")
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(1, rules["rust_feature_depends_on_app"]["alpha->commands"])
            self.assertFalse(rules["rust_module_path_ambiguous"])
            self.assertFalse(rules["rust_crate_root_glob_imports"])

            feature.write_text('include!(r"../../../../outside.rs");\n', encoding="utf-8")
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(
                1,
                rules["rust_module_path_ambiguous"][
                    "pinvou3-app/src-tauri/src/features/alpha/mod.rs"
                ],
            )
            feature.write_text('include!(env!("DYNAMIC_RUST"));\n', encoding="utf-8")
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(
                1,
                rules["rust_module_path_ambiguous"][
                    "pinvou3-app/src-tauri/src/features/alpha/mod.rs"
                ],
            )

    def test_included_file_uses_declared_feature_owner_not_disk_parent(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            alpha = rust_root / "features/alpha/mod.rs"
            beta = rust_root / "features/beta/mod.rs"
            helper = rust_root / "features/beta/helper.rs"
            gamma = rust_root / "features/gamma/mod.rs"
            for path in (alpha, beta, helper, gamma):
                path.parent.mkdir(parents=True, exist_ok=True)
            alpha.write_text(
                'mod included { include!("../beta/helper.rs"); }\n',
                encoding="utf-8",
            )
            helper.write_text("use super::super::beta as _;\n", encoding="utf-8")
            beta.write_text("use crate::features::alpha as _;\n", encoding="utf-8")
            gamma.write_text("use crate::app::commands as _;\n", encoding="utf-8")
            rules, cycles = self.guard.scan_rust(root)
            self.assertEqual([["alpha", "beta"]], cycles)
            self.assertEqual(1, rules["rust_cyclic_feature_dependencies"]["alpha->beta"])
            self.assertEqual(1, rules["rust_cyclic_feature_dependencies"]["beta->alpha"])
            self.assertEqual(1, rules["rust_feature_depends_on_app"]["gamma->commands"])
            self.assertNotIn("beta->beta", rules["rust_cyclic_feature_dependencies"])

    def test_crate_root_visible_use_aliases_are_scanned_across_files(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            lib = rust_root / "lib.rs"
            app = rust_root / "app/mod.rs"
            alpha = rust_root / "features/alpha/mod.rs"
            beta = rust_root / "features/beta/mod.rs"
            for path in (lib, app, alpha, beta):
                path.parent.mkdir(parents=True, exist_ok=True)
            app.write_text("pub mod commands;\n", encoding="utf-8")
            alpha.write_text(
                "fn run() { crate::host::commands::run(); crate::domains::beta::run(); }\n",
                encoding="utf-8",
            )
            beta.write_text("use crate::features::alpha as _;\n", encoding="utf-8")
            variants = (
                "pub use crate::{app as host, features as domains};\n",
                "pub(crate) use crate::{app as host, features as domains};\n",
                "pub(crate) use crate::app as host; pub(crate) use crate::features as domains;\n",
                "pub(super) use crate::{app as host, features as domains};\n",
                "pub(in crate) use crate::{app as host, features as domains};\n",
                '#[cfg(feature = "x")] pub(crate) use crate::{r#app as r#host, r#features as domains};\n',
            )
            for declaration in variants:
                with self.subTest(declaration=declaration):
                    lib.write_text("mod app; mod features;\n" + declaration, encoding="utf-8")
                    rules, cycles = self.guard.scan_rust(root)
                    self.assertEqual(1, rules["rust_feature_depends_on_app"]["alpha->commands"])
                    self.assertEqual([["alpha", "beta"]], cycles)
                    self.assertEqual(
                        1, rules["rust_cyclic_feature_dependencies"]["alpha->beta"]
                    )

    def test_private_root_uses_and_extern_self_alias_reach_descendants(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            lib = rust_root / "lib.rs"
            app = rust_root / "app/mod.rs"
            feature = rust_root / "features/alpha/mod.rs"
            for path in (lib, app, feature):
                path.parent.mkdir(parents=True, exist_ok=True)
            app.write_text("pub mod commands;\n", encoding="utf-8")
            feature.write_text("fn run() { crate::host::commands::run(); }\n", encoding="utf-8")
            variants = (
                "use app as host;\n",
                "pub(crate) use app as host;\n",
                "use {app as host};\n",
                "use {r#app as r#host};\n",
            )
            for declaration in variants:
                with self.subTest(declaration=declaration):
                    lib.write_text("mod app;\n" + declaration, encoding="utf-8")
                    rules, _ = self.guard.scan_rust(root)
                    self.assertEqual(1, rules["rust_feature_depends_on_app"]["alpha->commands"])
                    self.assertFalse(rules["rust_crate_root_alias_ambiguous"])

            lib.write_text("mod app; extern crate self as own;\n", encoding="utf-8")
            feature.write_text(
                "fn run() { crate::own::app::commands::run(); }\n",
                encoding="utf-8",
            )
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(1, rules["rust_feature_depends_on_app"]["alpha->commands"])

    def test_root_alias_collector_ignores_uninvoked_macro_and_nested_uses(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            lib = rust_root / "lib.rs"
            app = rust_root / "app/mod.rs"
            feature = rust_root / "features/alpha/mod.rs"
            for path in (lib, app, feature):
                path.parent.mkdir(parents=True, exist_ok=True)
            lib.write_text(
                '#[path = "app/mod.rs"] mod host;\n'
                'macro_rules! shadow { () => { use crate::features::knowledge as host; }; }\n'
                'fn private_use() { use crate::features::knowledge as host; }\n'
                'mod nested { use crate::features::knowledge as host; }\n',
                encoding="utf-8",
            )
            app.write_text("pub mod commands;\n", encoding="utf-8")
            feature.write_text("fn run() { crate::host::commands::run(); }\n", encoding="utf-8")
            rules, cycles = self.guard.scan_rust(root)
            self.assertEqual(1, rules["rust_feature_depends_on_app"]["alpha->commands"])
            self.assertFalse(rules["rust_crate_root_alias_ambiguous"])
            self.assertFalse(cycles)

            lib.write_text(
                lib.read_text(encoding="utf-8") + "use host as forwarded;\n",
                encoding="utf-8",
            )
            feature.write_text(
                "fn run() { crate::forwarded::commands::run(); }\n",
                encoding="utf-8",
            )
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(1, rules["rust_feature_depends_on_app"]["alpha->commands"])

            lib.write_text(lib.read_text(encoding="utf-8") + "shadow!();\n", encoding="utf-8")
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(
                1,
                rules["rust_crate_root_alias_ambiguous"][
                    "pinvou3-app/src-tauri/src/lib.rs"
                ],
            )
    def test_rust_lexer_keeps_character_and_raw_literal_delimiters_out_of_items(self):
        tokens = self.guard.rust_syntax_tokens(
            "const OPEN: char = '{'; const CLOSE: u8 = b'}'; "
            "const ESC: char = '\\u{10_FFFF}'; const QUOTE: char = '\\''; "
            'const RAW: &str = r#"include!(\\"fake.rs\\") {"#; '
            'const BYTES: &[u8] = br#"}"#; include!("real.rs");'
        )
        self.assertNotIn("{", tokens)
        self.assertNotIn("}", tokens)
        self.assertEqual(1, tokens.count("include"))
        self.assertEqual(7, tokens.count("<literal>"))
        self.assertEqual(("'", "a"), self.guard.rust_syntax_tokens("'a"))

        canonical = "\n".join(self.guard.SESSION_APPLICATION_EXPORTS) + "\n"
        self.assertTrue(
            self.guard.session_application_export_surface_is_frozen(
                canonical + "const OPEN: char = '{'; const CLOSE: u8 = b'}';\n"
            )
        )
        self.assertFalse(
            self.guard.session_application_export_surface_is_frozen(
                canonical + "const OPEN: char = '{'; include!(\"extra_exports.rs\");\n"
            )
        )

    def test_platform_rule_exceptions_require_header_marker_and_reason(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            allowed = rust_root / "features/allowed/mod.rs"
            rejected = rust_root / "features/rejected/mod.rs"
            late = rust_root / "features/late/mod.rs"
            allowed.parent.mkdir(parents=True)
            rejected.parent.mkdir(parents=True)
            late.parent.mkdir(parents=True)
            allowed.write_text(
                "// architecture-guard: allow-target-cfg -- dependency is Windows-only\n"
                "// architecture-guard: allow-platform-detail -- parses external command\n"
                '#[cfg(target_os = "windows")]\n'
                'fn run() { Command::new("powershell.exe"); }\n',
                encoding="utf-8",
            )
            late.write_text(
                "// header filler\n" * 20
                + "// architecture-guard: allow-target-cfg -- too late\n"
                + "// architecture-guard: allow-platform-detail -- too late\n"
                + '#[cfg(target_os = "windows")]\n'
                + 'fn run() { Command::new("powershell.exe"); }\n',
                encoding="utf-8",
            )
            rejected.write_text(
                "// architecture-guard: allow-target-cfg\n"
                "// architecture-guard: allow-platform-detail --\n"
                '#[cfg(target_os = "windows")]\n'
                'fn run() { Command::new("powershell.exe"); }\n',
                encoding="utf-8",
            )

            rules, _ = self.guard.scan_rust(root)

            self.assertNotIn(
                "pinvou3-app/src-tauri/src/features/allowed/mod.rs",
                rules["rust_target_cfg_outside_adapter"],
            )
            self.assertNotIn(
                "pinvou3-app/src-tauri/src/features/allowed/mod.rs",
                rules["rust_platform_details_outside_adapter"],
            )
            self.assertEqual(
                1,
                rules["rust_target_cfg_outside_adapter"][
                    "pinvou3-app/src-tauri/src/features/rejected/mod.rs"
                ],
            )
            self.assertEqual(
                1,
                rules["rust_platform_details_outside_adapter"][
                    "pinvou3-app/src-tauri/src/features/rejected/mod.rs"
                ],
            )
            self.assertEqual(
                1,
                rules["rust_target_cfg_outside_adapter"][
                    "pinvou3-app/src-tauri/src/features/late/mod.rs"
                ],
            )
            self.assertEqual(
                1,
                rules["rust_platform_details_outside_adapter"][
                    "pinvou3-app/src-tauri/src/features/late/mod.rs"
                ],
            )

    def test_application_boundaries_are_enumerated_and_reject_tauri_apis(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            plain = rust_root / "features/sessions/application/plain.rs"
            nested = rust_root / "features/sessions/application/nested/leak.rs"
            alias = rust_root / "features/sessions/application/alias.rs"
            grouped = rust_root / "features/sessions/application/grouped.rs"
            remote_ports = rust_root / "features/remote_control/controller/ports.rs"
            for path in [plain, nested, alias, grouped, remote_ports]:
                path.parent.mkdir(parents=True, exist_ok=True)
            plain.write_text(
                "pub trait Boundary { fn run(&self); }\n", encoding="utf-8"
            )
            nested.write_text(
                "use tauri::AppHandle;\n"
                "fn project(app: &AppHandle) { crate::platform::app_events::forward(app); }\n",
                encoding="utf-8",
            )
            remote_ports.write_text(
                "fn leak(app: tauri::AppHandle) { tauri::Emitter::emit(&app, \"x\", ()); }\n",
                encoding="utf-8",
            )
            alias.write_text("use tauri as host;\n", encoding="utf-8")
            grouped.write_text(
                "use crate::platform::{app_events as events};\n",
                encoding="utf-8",
            )

            rules, _ = self.guard.scan_rust(root)

            self.assertNotIn(
                "pinvou3-app/src-tauri/src/features/sessions/application/plain.rs",
                rules["rust_tauri_api_in_application"],
            )
            self.assertEqual(
                2,
                rules["rust_tauri_api_in_application"][
                    "pinvou3-app/src-tauri/src/features/sessions/application/nested/leak.rs"
                ],
            )
            self.assertEqual(
                2,
                rules["rust_tauri_api_in_application"][
                    "pinvou3-app/src-tauri/src/features/sessions/application/alias.rs"
                ],
            )
            self.assertEqual(
                2,
                rules["rust_tauri_api_in_application"][
                    "pinvou3-app/src-tauri/src/features/sessions/application/grouped.rs"
                ],
            )
            self.assertEqual(
                2,
                rules["rust_remote_control_host_api"][
                    "pinvou3-app/src-tauri/src/features/remote_control/controller/ports.rs:tauri_path"
                ],
            )

    def test_remote_control_host_api_rule_covers_non_port_files_and_ratchets(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            remote = (
                root
                / "pinvou3-app/src-tauri/src/features/remote_control/controller/new.rs"
            )
            remote.parent.mkdir(parents=True)
            remote.write_text(
                "use tauri::{AppHandle, Emitter};\n"
                "fn notify(app: &AppHandle) {\n"
                "  app.emit(\"event\", ());\n"
                "  crate::platform::app_events::forward(app);\n"
                "}\n",
                encoding="utf-8",
            )
            relative = "pinvou3-app/src-tauri/src/features/remote_control/controller/new.rs"
            rules, cycles = self.guard.scan_rust(root)
            self.assertEqual(1, rules["rust_remote_control_host_api"][f"{relative}:tauri_path"])
            self.assertEqual(1, rules["rust_remote_control_host_api"][f"{relative}:host_emit"])
            self.assertEqual(1, rules["rust_remote_control_host_api"][f"{relative}:app_events"])
            failures, _ = self.guard.compare(
                {"schema_version": 1, "rules": rules, "rust_feature_cycles": cycles},
                {"schema_version": 1, "rules": {}, "rust_feature_cycles": []},
            )
            self.assertTrue(any("rust_remote_control_host_api" in item for item in failures))

    def test_host_api_alias_glob_and_generic_emit_syntax_cannot_hide_calls(self):
        cases = [
            ("use {tauri as host};\n", "tauri_import_risk"),
            ("use {tauri};\n", "tauri_import_risk"),
            ("use tauri::{self as host};\n", "tauri_import_risk"),
            ("use tauri::{prelude as host};\n", "tauri_import_risk"),
            ("use tauri::*;\n", "tauri_import_risk"),
            ("use crate::platform as host;\n", "app_events_import_risk"),
            ("use crate::platform::{self as host};\n", "app_events_import_risk"),
            ("use crate::platform::{app_events as events};\n", "app_events_import_risk"),
            ("use crate::platform::app_events::*;\n", "app_events_import_risk"),
            (
                "use tauri::Emitter;\n"
                "fn send(app: &tauri::AppHandle) {\n"
                " app.emit::<String>(\"x\", String::new());\n"
                " app.emit_to::<String>(\"main\", \"x\", String::new());\n"
                "}\n",
                "host_emit",
            ),
        ]
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            remote = root / "pinvou3-app/src-tauri/src/features/remote_control/alias.rs"
            remote.parent.mkdir(parents=True)
            key = "pinvou3-app/src-tauri/src/features/remote_control/alias.rs"
            for source, mode in cases:
                with self.subTest(source=source):
                    remote.write_text(source, encoding="utf-8")
                    rules, cycles = self.guard.scan_rust(root)
                    self.assertGreater(
                        rules["rust_remote_control_host_api"][f"{key}:{mode}"], 0
                    )
                    failures, _ = self.guard.compare(
                        {"schema_version": 1, "rules": rules, "rust_feature_cycles": cycles},
                        {"schema_version": 1, "rules": {}, "rust_feature_cycles": []},
                    )
                    self.assertTrue(
                        any("rust_remote_control_host_api" in item for item in failures)
                    )

        # This feature also has an unrelated transport sink named `emit`.
        self.assertNotIn(
            "host_emit", self.guard.remote_control_host_api_counts("sink.emit(event);")
        )

    def test_new_remote_host_api_baseline_has_only_exact_legacy_allowances(self):
        baseline = json.loads(
            (REPO_ROOT / "scripts/architecture-baseline.json").read_text(encoding="utf-8")
        )
        legacy = baseline["rules"]["rust_remote_control_host_api"]
        self.assertEqual(self.guard.REMOTE_CONTROL_HOST_API_INITIAL_CEILING, legacy)
        old_baseline = json.loads(json.dumps(baseline))
        old_baseline["rules"].pop("rust_remote_control_host_api")
        self.assertEqual(
            [], self.guard.compare_baseline_ratchet(baseline, old_baseline)
        )

        expanded = json.loads(json.dumps(baseline))
        expanded["rules"]["rust_remote_control_host_api"]["new.rs:tauri_path"] = 1
        self.assertTrue(self.guard.compare_baseline_ratchet(expanded, old_baseline))
        expanded = json.loads(json.dumps(baseline))
        first = next(iter(legacy))
        expanded["rules"]["rust_remote_control_host_api"][first] += 1
        self.assertTrue(self.guard.compare_baseline_ratchet(expanded, old_baseline))

    def test_session_application_central_exports_are_frozen(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            module = (
                root
                / "pinvou3-app/src-tauri/src/features/sessions/application/mod.rs"
            )
            module.parent.mkdir(parents=True)
            module.write_text(
                "\n".join(self.guard.SESSION_APPLICATION_EXPORTS) + "\n",
                encoding="utf-8",
            )
            relative = "pinvou3-app/src-tauri/src/features/sessions/application/mod.rs"

            rules, _ = self.guard.scan_rust(root)
            self.assertNotIn(relative, rules["rust_session_application_export_drift"])

            canonical = module.read_text(encoding="utf-8")
            with_attributes = canonical.replace(
                "pub(crate) mod admission;",
                "// pub(crate) mod fake;\n"
                "pub /* scoped */ (crate)\nmod admission;",
            )
            module.write_text(with_attributes + 'const NOTE: &str = r#"pub mod fake;"#;\n', encoding="utf-8")
            rules, _ = self.guard.scan_rust(root)
            self.assertNotIn(relative, rules["rust_session_application_export_drift"])

            for addition in [
                "pub\n(crate)\nmod extra;\n",
                "pub(crate) mod sync_remote_journal;\n",
                "#[cfg(test)]\n// comment before export\n"
                "pub(crate)\nuse admission::{AdmissionUseCase};\n",
            ]:
                module.write_text(with_attributes + addition, encoding="utf-8")
                rules, cycles = self.guard.scan_rust(root)
                self.assertEqual(
                    1, rules["rust_session_application_export_drift"][relative]
                )
                failures, _ = self.guard.compare(
                    {"schema_version": 1, "rules": rules, "rust_feature_cycles": cycles},
                    {"schema_version": 1, "rules": {}, "rust_feature_cycles": []},
                )
                self.assertTrue(
                    any("rust_session_application_export_drift" in item for item in failures)
                )

            module.write_text(
                canonical + "pub(crate) use admission::*;\n",
                encoding="utf-8",
            )
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(
                1, rules["rust_session_application_export_drift"][relative]
            )

            for addition in [
                '#[cfg(any())]\npub(crate) mod admission;',
                '#[cfg(test)]\npub(crate) mod admission;',
                '#[cfg_attr(test, path = "other.rs")]\npub(crate) mod admission;',
            ]:
                with self.subTest(attribute=addition):
                    module.write_text(
                        canonical.replace("pub(crate) mod admission;", addition),
                        encoding="utf-8",
                    )
                    rules, _ = self.guard.scan_rust(root)
                    self.assertEqual(
                        1, rules["rust_session_application_export_drift"][relative]
                    )

            for addition in [
                'include!("extra_exports.rs");\n',
                'cfg_if! { if #[cfg(any())] { pub(crate) mod extra; } }\n',
                'macro_rules! exports { () => { pub(crate) mod extra; } }\n',
            ]:
                with self.subTest(expansion=addition):
                    module.write_text(canonical + addition, encoding="utf-8")
                    rules, _ = self.guard.scan_rust(root)
                    self.assertEqual(
                        1, rules["rust_session_application_export_drift"][relative]
                    )

            module.write_text(
                canonical + '#[cfg(test)]\nmod tests { macro_rules! local { () => {} } }\n',
                encoding="utf-8",
            )
            rules, _ = self.guard.scan_rust(root)
            self.assertNotIn(relative, rules["rust_session_application_export_drift"])

            module.write_text("pub(crate) mod admission;\n", encoding="utf-8")
            rules, _ = self.guard.scan_rust(root)
            self.assertEqual(
                1, rules["rust_session_application_export_drift"][relative]
            )

    def test_rust_scanner_rejects_external_group_kill_spawn(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            platform = rust_root / "platform/process.rs"
            platform.parent.mkdir(parents=True)
            platform.write_text(
                'fn run() {\n'
                '    Command::new("kill");\n'
                '    Command::new("/usr/bin/kill");\n'
                '    Path::new("kill");\n'
                '    connector_cli_command(cmd, "kill");\n'
                '    Command::new("taskkill");\n'
                '}\n',
                encoding="utf-8",
            )

            rules, _ = self.guard.scan_rust(root)

            self.assertEqual(
                7,
                rules["rust_external_group_kill_spawn"][
                    "pinvou3-app/src-tauri/src/platform/process.rs"
                ],
            )

    def test_rust_scanner_rejects_kill_in_stub_shaped_file(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            rust_root = root / "pinvou3-app/src-tauri/src"
            stub = rust_root / "platform/os/unsupported.rs"
            stub.parent.mkdir(parents=True)
            stub.write_text(
                "pub fn kill_pid_tree(pid: u32) {\n"
                "    #[cfg(unix)]\n"
                "    { let _ = pid; }\n"
                "    #[cfg(not(unix))]\n"
                '    { Command::new("kill").args(["-9", "-42"]).output(); }\n'
                "}\n",
                encoding="utf-8",
            )

            rules, _ = self.guard.scan_rust(root)

            self.assertEqual(
                2,
                rules["rust_external_group_kill_spawn"][
                    "pinvou3-app/src-tauri/src/platform/os/unsupported.rs"
                ],
            )

    def test_guard_does_not_track_file_line_counts(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            frontend = root / "pinvou3-app/src/features/sample/view.jsx"
            rust = root / "pinvou3-app/src-tauri/src/features/sample/mod.rs"
            frontend.parent.mkdir(parents=True)
            rust.parent.mkdir(parents=True)
            frontend.write_text("const value = 1;\n" * 2000, encoding="utf-8")
            rust.write_text("fn value() {}\n" * 2000, encoding="utf-8")

            frontend_rules = self.guard.scan_frontend(root)
            rust_rules, _ = self.guard.scan_rust(root)

            self.assertFalse(any("line" in rule for rule in frontend_rules))
            self.assertFalse(any("line" in rule for rule in rust_rules))

    def test_resource_scanner_rejects_platform_binaries_in_common(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            common = root / "pinvou3-app/src-tauri/resources/common/bundle/bin"
            common.mkdir(parents=True)
            (common / "tool.exe").write_bytes(b"MZ\x00\x00")
            (common / "tool").write_bytes(b"\x7fELF")
            generated = (
                root
                / "pinvou3-app/src-tauri/resources/common/generated/node_modules/tool"
            )
            generated.parent.mkdir(parents=True)
            generated.write_bytes(b"\x7fELF")
            platform = root / "pinvou3-app/src-tauri/resources/platforms/linux/x86_64"
            platform.mkdir(parents=True)
            (platform / "allowed").write_bytes(b"\x7fELF")

            rules = self.guard.scan_resources(root)

            self.assertEqual(2, sum(rules["common_platform_binaries"].values()))


class ArchitectureGuardRepositoryTests(unittest.TestCase):
    def test_checked_in_baseline_passes(self):
        result = subprocess.run(
            [sys.executable, str(SCRIPT_PATH)],
            cwd=REPO_ROOT,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            check=False,
        )
        self.assertEqual(0, result.returncode, result.stdout)
        self.assertIn("architecture guard passed", result.stdout)

    def test_baseline_is_valid_json_with_expected_schema(self):
        baseline = json.loads(
            (REPO_ROOT / "scripts/architecture-baseline.json").read_text(encoding="utf-8")
        )
        self.assertEqual(1, baseline["schema_version"])
        self.assertIn("rules", baseline)
        self.assertIn("rust_feature_cycles", baseline)

if __name__ == "__main__":
    unittest.main()

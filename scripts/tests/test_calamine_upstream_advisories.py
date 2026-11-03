import copy
import importlib.util
import json
from pathlib import Path
import re
import subprocess
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "calamine_advisories", ROOT / "scripts/calamine-upstream-advisories.py"
)
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


def metadata():
    identity = f"{CHECK.GIT_SOURCE}#calamine@0.38.0"
    return {
        "packages": [
            {"id": "root", "name": "consumer", "version": "1.0.0", "source": None,
             "dependencies": [{"name": "calamine", "source": CHECK.GIT_SOURCE,
                               "req": "^0.38", "optional": False, "features": ["dates"]}]},
            {"id": identity, "name": "calamine", "version": "0.38.0",
             "source": f"{CHECK.GIT_SOURCE}#{CHECK.REVISION}", "dependencies": []},
        ],
        "workspace_members": ["root"], "workspace_default_members": ["root"],
        "resolve": {"root": "root", "nodes": [
            {"id": "root", "dependencies": [identity], "features": [],
             "deps": [{"name": "renamed_calamine", "pkg": identity,
                       "dep_kinds": [{"kind": None, "target": None}]}]},
            {"id": identity, "dependencies": [], "deps": [], "features": ["dates"]},
        ]},
    }


class ProjectionTests(unittest.TestCase):
    def test_only_package_identity_and_parent_source_change(self):
        original = metadata()
        before = copy.deepcopy(original)
        projected = CHECK.project_upstream(original)
        upstream = f"{CHECK.REGISTRY}#calamine@0.38.0"
        expected = copy.deepcopy(original)
        expected["packages"][0]["dependencies"][0]["source"] = CHECK.REGISTRY
        expected["packages"][1]["source"] = CHECK.REGISTRY
        expected["packages"][1]["id"] = upstream
        expected["resolve"]["nodes"][0]["dependencies"] = [upstream]
        expected["resolve"]["nodes"][0]["deps"][0]["pkg"] = upstream
        expected["resolve"]["nodes"][1]["id"] = upstream
        self.assertEqual(projected, expected)
        self.assertEqual(original, before)

    def test_unknown_source_revision_missing_or_duplicate_fail_closed(self):
        for source in [None, CHECK.REGISTRY, CHECK.GIT_SOURCE + "#" + "0" * 40,
                       CHECK.GIT_SOURCE.replace("zhuowp", "other") + "#" + CHECK.REVISION]:
            with self.subTest(source=source):
                value = metadata()
                value["packages"][1]["source"] = source
                with self.assertRaises(ValueError):
                    CHECK.project_upstream(value)
        for count in [0, 2]:
            with self.subTest(count=count):
                value = metadata()
                value["packages"] = [value["packages"][0]] + [value["packages"][1]] * count
                with self.assertRaises(ValueError):
                    CHECK.project_upstream(value)

    def test_inconsistent_id_node_or_parent_source_fail_closed(self):
        mutations = [
            lambda m: m["packages"][1].update(id="wrong"),
            lambda m: m["packages"][0]["dependencies"][0].update(source=CHECK.REGISTRY),
            lambda m: m["packages"][0]["dependencies"].clear(),
            lambda m: m["resolve"]["nodes"].pop(),
            lambda m: m["resolve"]["nodes"].append(copy.deepcopy(m["resolve"]["nodes"][1])),
            lambda m: m["workspace_members"].append(m["packages"][1]["id"]),
        ]
        for mutation in mutations:
            value = metadata()
            mutation(value)
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                CHECK.project_upstream(value)

    def test_version_is_preserved_for_upstream_matching(self):
        value = metadata()
        old = value["packages"][1]["id"]
        value = json.loads(json.dumps(value).replace(old, old.replace("0.38.0", "0.37.0")))
        value["packages"][1]["version"] = "0.37.0"
        value["packages"][0]["dependencies"][0]["req"] = "^0.37"
        projected = CHECK.project_upstream(value)
        self.assertEqual(projected["packages"][1]["version"], "0.37.0")
        self.assertEqual(projected["packages"][1]["id"], f"{CHECK.REGISTRY}#calamine@0.37.0")


class CommandTests(unittest.TestCase):
    def test_locked_all_feature_graph_and_advisories_only_use_temporary_file(self):
        temporary_paths = []

        def run(command, **kwargs):
            self.assertEqual(kwargs["cwd"], CHECK.ROOT)
            self.assertTrue(kwargs["check"])
            self.assertIn("--locked", command)
            self.assertIn("--all-features", command)
            if command[1] == "metadata":
                return subprocess.CompletedProcess(command, 0, json.dumps(metadata()))
            self.assertEqual(command[:2], ["cargo", "deny"])
            self.assertEqual(command[-2:], ["check", "advisories"])
            path = Path(command[command.index("--metadata-path") + 1])
            temporary_paths.append(path)
            projected = json.loads(path.read_text(encoding="utf-8"))
            self.assertEqual(projected["packages"][1]["source"], CHECK.REGISTRY)
            return subprocess.CompletedProcess(command, 0)

        with mock.patch.object(CHECK.subprocess, "run", side_effect=run) as command:
            self.assertEqual(CHECK.main(["--manifest-path", CHECK.MANIFESTS[0]]), 0)
            self.assertEqual(command.call_count, 2)
        self.assertTrue(temporary_paths)
        self.assertFalse(temporary_paths[0].exists())

    def test_metadata_and_deny_failures_propagate(self):
        success = subprocess.CompletedProcess([], 0, json.dumps(metadata()))
        for responses in [
            [subprocess.CalledProcessError(7, ["cargo", "metadata"])],
            [success, subprocess.CalledProcessError(9, ["cargo", "deny"])],
        ]:
            with self.subTest(responses=responses), mock.patch.object(
                CHECK.subprocess, "run", side_effect=responses
            ):
                self.assertEqual(CHECK.main(["--manifest-path", CHECK.MANIFESTS[0]]),
                                 responses[-1].returncode)

    def test_invalid_metadata_fails_before_scan(self):
        result = subprocess.CompletedProcess([], 0, "{}")
        with mock.patch.object(CHECK.subprocess, "run", return_value=result) as command:
            self.assertEqual(CHECK.main(["--manifest-path", CHECK.MANIFESTS[0]]), 1)
            self.assertEqual(command.call_count, 1)


class WorkflowTests(unittest.TestCase):
    def test_existing_four_category_scans_and_two_conditional_supplements(self):
        workflow = (ROOT / ".github/workflows/pr-check.yml").read_text(encoding="utf-8")
        for job, manifest, condition, ordinary in [
            ("knowledge-rust", CHECK.MANIFESTS[0], "needs.changes.outputs.knowledge_dependencies == 'true'",
             "cargo deny --locked --manifest-path pinvou-knowledge/Cargo.toml --config pinvou3-app/src-tauri/deny.toml check advisories licenses bans sources"),
            ("rust-lint", CHECK.MANIFESTS[1], "env.RUN_HEAVY_RUST_CHECKS == 'true' && needs.changes.outputs.rust_dependencies == 'true'",
             "cargo deny --locked check advisories licenses bans sources"),
        ]:
            block = re.search(rf"^  {job}:\n(.*?)(?=^  [a-zA-Z0-9_-]+:|\Z)", workflow, re.M | re.S)[1]
            supplement = next(step for step in block.split("\n      - ")
                              if step.startswith("name: calamine upstream advisories\n"))
            self.assertIn("if: ${{ " + condition + " }}", supplement)
            self.assertIn("shell: bash", supplement)
            self.assertIn("working-directory: .", supplement)
            self.assertIn(f"python3 scripts/calamine-upstream-advisories.py --manifest-path {manifest}", supplement)
            self.assertNotIn("continue-on-error", supplement)
            self.assertLess(block.index(ordinary), block.index("name: calamine upstream advisories"))
        self.assertEqual(workflow.count("run: python3 scripts/calamine-upstream-advisories.py"), 2)
        for group in ["rust_dependencies", "knowledge_rust", "knowledge_dependencies"]:
            block = re.search(rf"^            {group}:\n(.*?)(?=^            \w+:|\Z)", workflow, re.M | re.S)[1]
            for path in ["scripts/calamine-upstream-advisories.py", "scripts/tests/test_calamine_upstream_advisories.py"]:
                self.assertIn(f"- '{path}'", block)


if __name__ == "__main__":
    unittest.main()

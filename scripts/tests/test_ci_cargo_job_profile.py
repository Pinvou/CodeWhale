import re
import os
import importlib.util
import subprocess
from unittest import mock
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[2]
OVERRIDE = "--config 'profile.dev.package.codewhale-tui.opt-level=0'"


class CargoJobProfileTests(unittest.TestCase):
    def test_linux_memory_override_is_scoped_to_all_four_test_processes(self):
        workflow = (ROOT / ".github/workflows/pr-check.yml").read_text(encoding="utf-8")
        job = workflow.split("\n  rust-test:", 1)[1].split("\n  cli-test:", 1)[0]
        commands = re.findall(r"(?m)^ +(?:run: )?(?:/usr/bin/time -v )?cargo (.+)$", job)
        test_commands = [command for command in commands if re.search(r"(?:^| )test(?: |$)", command)]
        self.assertEqual(len(test_commands), 4)
        for command in test_commands:
            self.assertTrue(command.startswith(OVERRIDE + " test"), command)
        self.assertEqual(sum("--no-run" in command for command in test_commands), 2)
        self.assertEqual(sum("--lib" in command for command in test_commands), 3)
        self.assertIn("--test behavior_telemetry", job)
        self.assertNotIn('cat >> "$config"', job)
        self.assertNotIn('config="${CARGO_HOME:-$HOME/.cargo}/config.toml"', job)
        self.assertNotIn("[profile.dev.package.codewhale-tui]", job)


    def test_registry_guard_gets_the_same_process_override_in_its_step(self):
        workflow = (ROOT / ".github/workflows/pr-check.yml").read_text(encoding="utf-8")
        job = workflow.split("\n  rust-test:", 1)[1].split("\n  cli-test:", 1)[0]
        run = job.index("        run: python3 scripts/test-registry-guard.py")
        step = job[job.rfind("      - name:", 0, run):run]
        self.assertIn('PINVOU_CI_CODEWHALE_DEV_O0: "1"', step)


class RegistryGuardProfileTests(unittest.TestCase):
    def guard(self):
        spec = importlib.util.spec_from_file_location("ci_profile_registry_probe", ROOT / "scripts/test-registry-guard.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def invoke(self, env):
        guard = self.guard()
        result = subprocess.CompletedProcess([], 0, "fixture::registered: test\n", "")
        with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(guard.subprocess, "run", return_value=result) as run:
            self.assertEqual(guard.list_tests(), ["fixture::registered"])
        return run.call_args.args[0]

    def test_opt_in_has_same_override_and_keeps_registry_flags(self):
        command = self.invoke({"PINVOU_CI_CODEWHALE_DEV_O0": "1"})
        self.assertEqual(command[:4], ["cargo", "--config", "profile.dev.package.codewhale-tui.opt-level=0", "test"])
        self.assertEqual(command[4:], ["--manifest-path", str(ROOT / "pinvou3-app/src-tauri/Cargo.toml"), "--lib", "--no-default-features", "--features", "benchmark-hooks", "--locked", "--", "--list", "--format", "terse"])

    def test_unset_preserves_original_registry_invocation(self):
        command = self.invoke({})
        self.assertEqual(command[:2], ["cargo", "test"])
        self.assertNotIn("--config", command)

    def test_invalid_override_never_dispatches(self):
        for value in ("", "0", "true", "profile.dev.opt-level=3"):
            with self.subTest(value=value):
                guard = self.guard()
                with mock.patch.dict(os.environ, {"PINVOU_CI_CODEWHALE_DEV_O0": value}, clear=True), mock.patch.object(guard.subprocess, "run") as run:
                    with self.assertRaises(SystemExit):
                        guard.list_tests()
                    run.assert_not_called()


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""Generated SGLang lifecycle scripts preserve literal deployment paths offline.

All Docker, GPU, network and sleep commands are test executables. No listener,
container, service, package install or model inference is started.
"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SERVER = Path(__file__).resolve().parents[1] / "src-tauri/resources/common/bundle/mcp-servers/megacube_admin_server.py"
spec = importlib.util.spec_from_file_location("megacube_generated_env", SERVER)
server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(server)

PATHS = ["with spaces", "$archive", "`touch should-not-exist`", r"back\slash",
         "single'quote", 'embedded"quote', 'trailing"', 'two-trailing""']

STUB = r'''import json, os, sys
from pathlib import Path
name = Path(sys.argv[0]).name
args = sys.argv[1:]
with open(os.environ["COMMAND_LOG"], "a") as log:
    log.write(json.dumps([name, *args]) + "\n")
if name == "docker":
    state = Path(os.environ["RUNNING_MARKER"])
    if args[0] == "ps" and state.exists():
        print(os.environ["CONTAINER"])
    elif args[0] == "run" and "-d" in args:
        state.touch()
        print("offline-container-handle")
    elif args[0] in ("rm", "stop"):
        state.unlink(missing_ok=True)
elif name == "nvidia-smi":
    print("Offline GPU fixture")
elif name == "curl" and args[-1].endswith("/v1/models"):
    print('{"data":[{"id":"fixture-NVFP4"}]}')
'''


class GeneratedEnvironmentTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.commands = self.root / "commands"
        self.commands.mkdir()
        self.log = self.root / "commands.jsonl"
        self.log.write_text("")
        for name in ("docker", "nvidia-smi", "nvidia-ctk", "curl", "sleep"):
            program = self.commands / name
            program.write_text("#!" + sys.executable + "\n" + STUB)
            program.chmod(0o755)
        self.env = {**os.environ, "PATH": str(self.commands) + os.pathsep + os.environ["PATH"],
                    "COMMAND_LOG": str(self.log), "RUNNING_MARKER": str(self.root / "running"),
                    "CONTAINER": "sglang-fixture", "PYTHONDONTWRITEBYTECODE": "1"}
        self.env.pop("archive", None)
        self.weights = b"offline tiny weight"
        self.card = self.root / "card"
        self.card.mkdir()
        files = server._render_sglang_card_files({
            "repo_id": "test/fixture-NVFP4", "model_name": "fixture", "weights_bytes": len(self.weights),
            "lfs_files": [{"name": "model.safetensors", "size": len(self.weights),
                           "sha256": hashlib.sha256(self.weights).hexdigest()}],
            "small_files": ["config.json"], "quantization": "NVFP4"}, "fixture")
        for name, data in files.items():
            path = self.card / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
            if name.endswith(".sh"):
                path.chmod(0o755)

    def run_script(self, script, *args, extra=None):
        result = subprocess.run(["bash", str(self.card / script), *args],
                                env={**self.env, **(extra or {})}, capture_output=True,
                                text=True, timeout=10, cwd=self.root)
        self.assertEqual(result.returncode, 0, result.stdout + "\n" + result.stderr)
        return result

    def deploy(self, name):
        directory = self.root / name
        directory.mkdir(exist_ok=True)
        (directory / "model.safetensors").write_bytes(self.weights)
        (directory / "config.json").write_text("{}")
        self.run_script("vendor/deploy.sh", extra={"MODEL_DIR": str(directory), "PORT": "30173",
                                                  "SKIP_DOCKER_INSTALL": "1"})
        self.assertIn('MODEL_DIR="' + str(directory) + '"', (self.card / "vendor/.deploy-env").read_text())
        return directory

    def test_generated_vendor_lifecycle_round_trips_literal_paths(self):
        for name in PATHS:
            with self.subTest(path=name):
                directory = self.deploy(name)
                self.log.write_text("")
                self.run_script("vendor/start.sh")
                calls = [json.loads(line) for line in self.log.read_text().splitlines()]
                launches = [call for call in calls if call[:3] == ["docker", "run", "-d"]]
                self.assertEqual(len(launches), 1)
                self.assertIn(str(directory) + ":/model:ro", launches[0])
                self.assertIn("30173:30000", launches[0])
                self.run_script("vendor/stop.sh")
                self.run_script("vendor/uninstall.sh", "--keep-model", "--yes")
                self.assertTrue((directory / "model.safetensors").is_file())
                self.assertFalse((self.root / "should-not-exist").exists())

    def test_outer_common_port_and_uninstall_path_keep_the_same_literal_format(self):
        for name in PATHS:
            with self.subTest(path=name):
                directory = self.deploy(name)
                # This runs the actual common.sh PORT fallback; the explicit
                # environment must not hide a broken .deploy-env reader.
                result = subprocess.run(["bash", "-eu", "-c", '. "$1"; printf "%s" "$PORT"',
                                         "fixture", str(self.card / "common.sh")],
                                        env=self.env, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, "30173")
                sentinel = self.root / "unrelated-keep"
                sentinel.write_text("preserved")
                self.run_script("uninstall.sh", extra={"LMM_PURGE_WEIGHTS": "1"})
                self.assertFalse(directory.exists())
                self.assertEqual(sentinel.read_text(), "preserved")

    def test_record_commands_are_not_evaluated(self):
        # This string is a literal pathname. Even the backtick content must
        # remain bytes in the mount argument and never run as a command.
        directory = self.deploy("`touch should-not-exist`")
        for script in ("vendor/start.sh", "vendor/stop.sh"):
            self.run_script(script)
        self.assertTrue(directory.exists())
        self.assertFalse((self.root / "should-not-exist").exists())


if __name__ == "__main__":
    unittest.main()

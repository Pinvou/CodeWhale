#!/usr/bin/env python3
"""Opt-in real store-host loading of a prepared native fixture card.

PINVOU_MEGACUBE_TEST_BINARY=/absolute/pinvou3-tauri python3 -B this_file.py
No model server is started. This proves the package/executor contract, not Tachyon inference.
"""
import hashlib
import http.client
import json
import os
from pathlib import Path
import platform
import socket
import sqlite3
import subprocess
import shlex
import tempfile
import time
import unittest


@unittest.skipUnless(os.environ.get("PINVOU_MEGACUBE_TEST_BINARY") and platform.machine() == "aarch64",
                     "opt in with a built aarch64 desktop binary; no real Tachyon assets required")
class NativeLoaderTest(unittest.TestCase):
    def test_complete_card_loads_and_missing_uninstall_is_rejected(self):
        with tempfile.TemporaryDirectory(prefix="pinvou-native-loader-") as temporary:
            root = Path(temporary)
            home, profile = root / "home", root / "p"
            home.mkdir()
            store = profile / "model-store"
            cards = store / "cards"
            cards.mkdir(parents=True)
            weights = b"native-loader-fixture\n" * 64
            candidate = home / "models/imported"
            candidate.mkdir(parents=True)
            (candidate / "fixture.bin").write_bytes(weights)
            binary = b"#!/bin/sh\n# fixture only: help succeeds; not an inference engine\nexit 0\n"
            # Independent, non-Tachyon fixture: the MCP no longer generates
            # native engine installers. Keep exercising the real store loader,
            # all six entries and refreshed post-install configuration.
            for card_id in ("native-complete", "native-missing"):
                directory = cards / card_id
                directory.mkdir()
                common = ("CARD_DIR=\"$(cd -- \"$(dirname -- \"${BASH_SOURCE[0]}\")\" && pwd)\"\n"
                          + "ENGINE_DIR=\"$CARD_DIR/engine\"\n"
                          + "MODEL_DIR=\"$HOME/models/" + card_id + "\"\n"
                          + "ARTIFACT=fixture.bin\nMODEL_ID=fixture-native\nPORT=0\n"
                          + "ENGINE_BIN_SHA256=uninstalled\n")
                files = {"common.sh": common,
                    "card.json": json.dumps({"id": card_id, "name": "Native loader fixture",
                                              "engine_name": "fixture-native", "size_bytes": len(weights),
                                              "package_files": ["common.sh", "weights.sha256"]}),
                    "weights.sha256": hashlib.sha256(weights).hexdigest() + "  fixture.bin\n",
                    "install_engine.sh": '#!/bin/bash\nset -eu\n. "$(dirname "$0")/common.sh"\n'
                        + 'if [ "${LMM_PROBE_ONLY:-0}" = 1 ]; then [ -x "$ENGINE_DIR/fixture-engine" ]; exit; fi\n'
                        + 'mkdir -p "$ENGINE_DIR"\nprintf %s ' + shlex.quote(binary.decode()) + ' > "$ENGINE_DIR/fixture-engine"\n'
                        + 'chmod +x "$ENGINE_DIR/fixture-engine"\necho ENGINE_BIN_SHA256='
                        + shlex.quote(hashlib.sha256(binary).hexdigest()) + ' >> "$CARD_DIR/common.sh"\n',
                    "download_model.sh": '#!/bin/bash\nset -eu\n. "$(dirname "$0")/common.sh"\n'
                        + 'mkdir -p "$MODEL_DIR"\ncp -- ' + shlex.quote(str(candidate / "fixture.bin")) + ' "$MODEL_DIR/$ARTIFACT"\n'
                        + '(cd "$MODEL_DIR"; sha256sum -c "$CARD_DIR/weights.sha256")\n',
                    "deploy.sh": '#!/bin/bash\nset -eu\n. "$(dirname "$0")/common.sh"\n'
                        + 'printf "PORT=0\\nMODEL_ID=fixture-native\\n" > "$CARD_DIR/.deploy-env"\n',
                    "enable.sh": "#!/bin/sh\nexit 0\n", "stop.sh": "#!/bin/sh\nexit 0\n",
                    "uninstall.sh": "#!/bin/sh\nexit 0\n"}
                for name, content in files.items():
                    path = directory / name
                    path.write_text(content)
                    if name.endswith(".sh"):
                        path.chmod(0o755)
                self.assertTrue((directory / "uninstall.sh").is_file())
            (cards / "native-missing/uninstall.sh").unlink()
            env = {**os.environ, "HOME": str(home), "PINVOU3_HOME": str(profile),
                   "CMSD_SOCKET": str(root / "absent.sock"),
                   "XDG_DATA_HOME": str(root / "data"), "XDG_STATE_HOME": str(root / "state"),
                   "XDG_CONFIG_HOME": str(root / "config"), "XDG_CACHE_HOME": str(root / "cache")}
            env.pop("BASH_ENV", None)
            socket_path = store / "service.sock"

            def request(path, payload=None):
                with socket.socket(socket.AF_UNIX) as connection:
                    connection.settimeout(10)
                    connection.connect(str(socket_path))
                    body = json.dumps(payload).encode() if payload is not None else b""
                    method = "POST" if payload is not None else "GET"
                    connection.sendall(("%s %s HTTP/1.1\r\nHost: local\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n" % (method, path, len(body))).encode() + body)
                    response = http.client.HTTPResponse(connection)
                    response.begin()
                    value = json.loads(response.read())
                    self.assertEqual(response.status, 200, value)
                    return value

            run_id = None
            with (root / "host.log").open("w+") as log:
                host = subprocess.Popen([os.environ["PINVOU_MEGACUBE_TEST_BINARY"], "--model-store-host"],
                                        env=env, stdout=log, stderr=subprocess.STDOUT)
                try:
                    deadline = time.monotonic() + 15
                    while not socket_path.exists() and host.poll() is None and time.monotonic() < deadline:
                        time.sleep(.1)
                    self.assertTrue(socket_path.exists(), "store host failed to bind")
                    run_id = request("/v1/install_local", {"card_id": "native-complete"})["run_id"]
                    deadline = time.monotonic() + 120
                    while time.monotonic() < deadline:
                        task = request("/v1/tasks/" + run_id)
                        if task["status"] != "running":
                            break
                        time.sleep(.25)
                    self.assertEqual(task["status"], "succeeded", task)
                    installed = task
                    run_id = request("/v1/install_local", {"card_id": "native-missing"})["run_id"]
                    deadline = time.monotonic() + 15
                    while time.monotonic() < deadline:
                        rejected = request("/v1/tasks/" + run_id)
                        if rejected["status"] != "running":
                            break
                        time.sleep(.1)
                    self.assertEqual(rejected["status"], "failed", rejected)
                    self.assertIn("uninstall.sh", rejected["error"])
                    with sqlite3.connect("file:%s?mode=ro" % (store / "models.db"), uri=True) as database:
                        row = database.execute("SELECT status,scripts_content_json,assets_content_json FROM store_models WHERE card_id=?", ("native-complete",)).fetchone()
                    self.assertEqual(row[0], "deployed")
                    scripts, assets = json.loads(row[1]), json.loads(row[2])
                    self.assertIn("uninstall", scripts)
                    self.assertIn(hashlib.sha256(binary).hexdigest(), assets["common.sh"]["content"])
                    self.assertTrue((cards / "native-complete/.deploy-env").is_file())
                    report = {"executor": "real_model_store_host", "engine": "tiny_shell_fixture_not_Tachyon",
                              "native_inference_tested": False, "complete_card_status": installed["status"],
                              "missing_uninstall_status": rejected["status"], "stored_uninstall": True,
                              "post_install_configuration_preserved": True, "source_preserved": (candidate / "fixture.bin").read_bytes() == weights}
                    if os.environ.get("PINVOU_MEGACUBE_ACCEPTANCE_OUTPUT"):
                        output = Path(os.environ["PINVOU_MEGACUBE_ACCEPTANCE_OUTPUT"])
                        output.parent.mkdir(parents=True, exist_ok=True)
                        output.write_text(json.dumps(report, ensure_ascii=False, indent=2))
                finally:
                    if run_id and host.poll() is None:
                        try:
                            request("/v1/tasks/" + run_id + "/cancel", {})
                        except Exception:
                            pass
                    host.terminate()
                    try:
                        host.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        host.kill()
                        host.wait(timeout=5)


if __name__ == "__main__":
    unittest.main(verbosity=2)

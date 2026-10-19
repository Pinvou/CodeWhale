#!/usr/bin/env python3
"""Validate a model-card native_engine_process.py without model weights or a GPU.

Usage: python3 scripts/test-native-engine-launch-receipt.py /path/to/controller.py
The controller comes from the card publisher; no network or user runtime is used.
"""

import importlib.util
import hashlib
import json
import os
import re
from pathlib import Path
import signal
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock


CONTROLLER_PATH = Path(sys.argv.pop(1)).resolve()


class NativeLaunchReceiptTests(unittest.TestCase):
    def test_rust_repair_matches_the_real_published_controller_and_fixed_result(self):
        rust = (Path(__file__).resolve().parents[1] / "pinvou3-app/src-tauri/src/features/"
                "model_store/platform/linux/native_card_compat.rs").read_text(encoding="utf-8")
        old = re.search(r'const OLD: &str = r#"(.*?)"#;', rust, re.S).group(1)
        new = re.search(r'const NEW: &str = r#"(.*?)"#;', rust, re.S).group(1)
        broken_hash = re.search(r'pub const BROKEN: &str = "([a-f0-9]{64})";', rust).group(1)
        fixed_hash = re.search(r'pub const FIXED: &str = "([a-f0-9]{64})";', rust).group(1)
        fixed = CONTROLLER_PATH.read_text(encoding="utf-8")
        self.assertEqual(fixed.count(new), 1)
        published = fixed.replace(new, old, 1)
        self.assertEqual(hashlib.sha256(published.encode()).hexdigest(), broken_hash)
        self.assertEqual(published.count(old), 1)
        self.assertEqual(hashlib.sha256(published.replace(old, new, 1).encode()).hexdigest(), fixed_hash)

    def setUp(self):
        spec = importlib.util.spec_from_file_location("card_controller", CONTROLLER_PATH)
        self.controller = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.controller)
        self.temporary = tempfile.TemporaryDirectory(prefix="pinvou-launch-receipt-test-")
        self.addCleanup(self.temporary.cleanup)
        root = Path(self.temporary.name)
        self.weights = root / "fake-model.py"
        self.weights.write_text("import time\ntime.sleep(30)\n", encoding="utf-8")
        executable = Path(sys.executable).resolve()
        metadata = executable.stat()
        self.target = {
            "executable": str(executable),
            "inode": [metadata.st_dev, metadata.st_ino],
            "weights": str(self.weights),
            "port": 18279,
            "model": "pinvou-isolated-test-only",
        }
        self.command = [str(executable), str(self.weights), "--port", "18279",
                        "--model-id", self.target["model"]]
        self.runtime = root / "runtime"
        self.runtime.mkdir(mode=0o700)
        self.instance = self.controller.Instance(str(self.runtime), self.target)
        self.addCleanup(self.instance.close)
        self.process = {"pid": 123456, "start_ticks": 123, "uid": os.getuid()}

    def test_delayed_proc_identity_records_only_the_direct_child(self):
        child = SimpleNamespace(pid=self.process["pid"], poll=mock.Mock(return_value=None))
        with mock.patch.object(self.controller, "discover", return_value=[]), \
                mock.patch.object(self.controller.subprocess, "Popen", return_value=child), \
                mock.patch.object(self.controller, "identity", side_effect=[None, None, self.process]) as identity, \
                mock.patch.object(self.controller.time, "sleep"):
            self.assertEqual(self.instance.launch(self.command, "", ""), self.process)
        self.assertEqual(identity.call_args_list, [mock.call(child.pid, self.target)] * 3)
        receipt = json.loads((self.runtime / "serve.identity.json").read_text())
        self.assertEqual(receipt["process"], self.process)
        self.assertEqual(receipt["target"], self.target)

    def test_exited_child_does_not_get_a_receipt(self):
        child = SimpleNamespace(pid=self.process["pid"], poll=mock.Mock(return_value=1))
        with mock.patch.object(self.controller, "discover", return_value=[]), \
                mock.patch.object(self.controller.subprocess, "Popen", return_value=child), \
                mock.patch.object(self.controller, "identity", return_value=None):
            with self.assertRaisesRegex(self.controller.UnsafeState, "已退出"):
                self.instance.launch(self.command, "", "")
        self.assertFalse((self.runtime / "serve.identity.json").exists())

    def test_identity_timeout_keeps_the_child_unowned_and_unsignalled(self):
        child = SimpleNamespace(pid=self.process["pid"], poll=mock.Mock(return_value=None))
        with mock.patch.object(self.controller, "discover", return_value=[]), \
                mock.patch.object(self.controller.subprocess, "Popen", return_value=child), \
                mock.patch.object(self.controller, "identity", return_value=None), \
                mock.patch.object(self.controller.time, "monotonic", side_effect=[0, 3]), \
                mock.patch.object(self.controller.signal, "pidfd_send_signal") as send:
            with self.assertRaisesRegex(self.controller.UnsafeState, str(child.pid)):
                self.instance.launch(self.command, "", "")
            send.assert_not_called()
        self.assertFalse((self.runtime / "serve.identity.json").exists())

    def test_existing_unrecorded_process_is_neither_adopted_nor_stopped(self):
        with mock.patch.object(self.controller, "discover", return_value=[self.process]), \
                mock.patch.object(self.controller.subprocess, "Popen") as spawn, \
                mock.patch.object(self.controller.signal, "pidfd_send_signal") as send:
            with self.assertRaises(self.controller.UnsafeState):
                self.instance.launch(self.command, "", "")
            with self.assertRaises(self.controller.UnsafeState):
                self.instance.stop()
            spawn.assert_not_called()
            send.assert_not_called()
        self.assertFalse((self.runtime / "serve.identity.json").exists())

    def test_200_real_child_launch_receipt_and_stop_cycles(self):
        original_spawn = self.controller.subprocess.Popen
        children = []

        def spawn(*args, **kwargs):
            child = original_spawn(*args, **kwargs)
            children.append(child)
            return child

        def discover_test_children(target):
            # Keep the inventory inside this test. Unrelated host processes can
            # have deliberately unreadable /proc state or very long argv during
            # compilation; identity/receipts/pidfd signalling remain real.
            return [process for child in children
                    if (process := self.controller.identity(child.pid, target)) is not None]

        with mock.patch.object(self.controller.subprocess, "Popen", side_effect=spawn), \
                mock.patch.object(self.controller, "discover", side_effect=discover_test_children):
            for _ in range(200):
                try:
                    process = self.instance.launch(self.command, "", "")
                    self.assertEqual(process["pid"], children[-1].pid)
                    self.assertEqual(self.instance.owned(), process)
                    self.instance.stop(grace=2)
                    self.assertIsNone(self.instance.owned())
                finally:
                    # Only our own unreaped Popen child is eligible for cleanup.
                    for child in children:
                        if child.poll() is None:
                            fd = os.pidfd_open(child.pid)
                            try:
                                signal.pidfd_send_signal(fd, signal.SIGTERM)
                            finally:
                                os.close(fd)
                        child.wait(timeout=3)
                    children.clear()


if __name__ == "__main__":
    unittest.main(verbosity=2)

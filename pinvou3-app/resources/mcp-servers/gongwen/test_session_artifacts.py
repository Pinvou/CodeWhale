import json
import os
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path

import docx


PACKAGE = Path(__file__).resolve().parent
RUNNER = (
    PACKAGE.parents[2]
    / "src-tauri/resources/common/bundle/mcp-servers/python_dependency_runner.py"
)
REQUEST = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {
        "name": "make_gongwen",
        "arguments": {
            "filename": "session-fixture",
            "文种": "通知",
            "标题": "关于测试的通知",
            "主送机关": "测试组：",
            "正文": "请核实会话归属。",
            "成文日期": "2026年11月26日",
            "发文机关": "测试办公室",
        },
    },
}


def generate(artifacts, fallback, filename=REQUEST["params"]["arguments"]["filename"]):
    request = {
        **REQUEST,
        "params": {
            **REQUEST["params"],
            "arguments": {**REQUEST["params"]["arguments"], "filename": filename},
        },
    }
    response = subprocess.run(
        [
            sys.executable,
            "-I",
            "-S",
            "-B",
            str(RUNNER),
            str(Path(docx.__file__).resolve().parent.parent),
            str(PACKAGE / "server.py"),
            f"--session-artifacts-dir={artifacts}",
        ],
        input=json.dumps(request) + "\n",
        env={**os.environ, "PINVOU3_SESSION_ARTIFACTS": str(fallback)},
        encoding="utf-8",
        capture_output=True,
        check=True,
        timeout=30,
    )
    result = json.loads(response.stdout)
    return json.loads(result["result"]["content"][0]["text"])


class GongwenSessionArtifactsTest(unittest.TestCase):
    def test_managed_connector_publishes_only_in_its_own_session(self):
        with tempfile.TemporaryDirectory() as root:
            sessions = Path(root) / "sessions"
            first = sessions / "controlled-a" / "artifacts"
            second = sessions / "controlled-b" / "artifacts"
            fallback = sessions / "default" / "artifacts"
            destinations = [first, second]
            for directory in destinations:
                directory.parent.mkdir(parents=True)

            for destination in destinations:
                result = generate(destination, fallback)
                self.assertTrue(result["ok"], result)
                published = destination / "session-fixture.docx"
                self.assertEqual(Path(result["path"]), published)
                self.assertEqual(list(destination.iterdir()), [published])
                with zipfile.ZipFile(published) as document:
                    self.assertIn("请核实会话归属。", document.read("word/document.xml").decode())

            self.assertFalse(fallback.exists())

    def test_hidden_and_office_lock_names_are_discoverable(self):
        with tempfile.TemporaryDirectory() as root:
            artifacts = Path(root) / "controlled" / "artifacts"
            fallback = Path(root) / "default"
            for filename in [".draft", "~$draft"]:
                result = generate(artifacts, fallback, filename)
                self.assertTrue(result["ok"], result)
                published = artifacts / f"公文-{filename}.docx"
                self.assertEqual(Path(result["path"]), published)
                self.assertTrue(published.is_file())
            self.assertFalse(fallback.exists())

    def test_managed_connector_does_not_fall_back_when_session_is_unavailable(self):
        with tempfile.TemporaryDirectory() as root:
            blocked = Path(root) / "unavailable"
            blocked.write_text("blocked directory", encoding="utf-8")
            result = generate(blocked / "artifacts", Path(root) / "default")
            self.assertFalse(result["ok"], result)
            self.assertNotIn("path", result)
            self.assertEqual(list(Path(root).iterdir()), [blocked])


if __name__ == "__main__":
    unittest.main()

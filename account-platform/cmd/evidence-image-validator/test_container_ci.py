"""Local orchestration checks; these do not start Docker or validate Linux."""
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, call, patch

spec = importlib.util.spec_from_file_location("container_ci", Path(__file__).with_name("container_ci.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ContainerCIUnitTest(unittest.TestCase):
    def test_volume_mode_is_set_before_ownership_transfer(self):
        with patch.object(module, "docker") as docker:
            module.initialize_volume("init", ["--cap-drop", "ALL"], "volume", "image")
            args = docker.call_args.args
            self.assertIn("--cap-drop", args)
            self.assertIn("CHOWN", args)
            self.assertNotIn("FOWNER", args)
            self.assertEqual(args[-1], "chmod 0750 /run/evidence && chown 65533:65533 /run/evidence")

    def test_negative_uid_is_surrounded_by_live_positive_requests(self):
        events = Mock()
        with patch.object(module, "client") as client, patch.object(module, "worker_ready") as ready:
            events.attach_mock(client, "client")
            events.attach_mock(ready, "ready")
            names = ["init", "worker", "before", "negative", "restart", "check", "after"]
            module.check_peer_access(names, "owner", "image", "volume", Path("/test"))
            self.assertEqual(events.mock_calls, [
                call.client("before", "owner", "image", "volume", Path("/test"), "65532:65533", "allowed"),
                call.ready("worker"),
                call.client("negative", "owner", "image", "volume", Path("/test"), "65531:65533", "wrong-caller"),
                call.ready("worker"),
                call.client("after", "owner", "image", "volume", Path("/test"), "65532:65533", "allowed"),
            ])

    def test_cleanup_requires_matching_owner(self):
        for label, expect_remove in [("our-token", True), ("other-token", False), ("", False)]:
            with self.subTest(label=label), patch.object(module, "docker") as docker:
                docker.return_value = SimpleNamespace(returncode=0, stdout=label + "\n")
                module.owned_cleanup("container", "our-container", "our-token")
                self.assertEqual(docker.call_count, 2 if expect_remove else 1)
                if expect_remove:
                    self.assertEqual(docker.call_args.args, ("container", "rm", "-f", "our-container"))

    def test_missing_or_skipped_client_case_fails(self):
        for text in ("", "--- SKIP: TestEvidenceValidatorContainer"):
            with self.subTest(text=text), patch.object(module, "docker", return_value=SimpleNamespace(stdout=text)):
                with self.assertRaises(RuntimeError):
                    module.client("name", "owner", "image", "volume", Path("/test"), "65532:65533", "allowed")

    def test_expected_client_cases_are_required(self):
        names = ("", "/png", "/progressive.jpg", "/animated.png", "/pdf/plain-compressed.pdf",
                 "/pdf/truncated.pdf", "/pdf/user-password.pdf")
        output = "\n".join("--- PASS: TestEvidenceValidatorContainer" + name for name in names)
        with patch.object(module, "docker", return_value=SimpleNamespace(stdout=output)):
            module.client("name", "owner", "image", "volume", Path("/test"), "65532:65533", "allowed")
        with patch.object(module, "docker", return_value=SimpleNamespace(stdout=output.replace(names[-1], "/missing.pdf"))):
            with self.assertRaises(RuntimeError):
                module.client("name", "owner", "image", "volume", Path("/test"), "65532:65533", "allowed")


if __name__ == "__main__":
    unittest.main()

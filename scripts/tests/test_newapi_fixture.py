"""Offline guard tests. Real Docker acceptance requires explicit opt-in."""

import copy
import importlib.util
import io
from contextlib import redirect_stdout
from pathlib import Path
import unittest
from unittest.mock import patch
from types import SimpleNamespace

SPEC = importlib.util.spec_from_file_location("newapi_fixture", Path(__file__).resolve().parents[1] / "account-platform-newapi-fixture.py")
fixture = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(fixture)


class NewAPIFixtureTests(unittest.TestCase):
    def setUp(self):
        self.image = {
            "Os": "linux", "Architecture": "amd64", "Config": {"Labels": {
                "org.opencontainers.image.revision": fixture.REVISION,
                "org.opencontainers.image.version": "v1.0.0-rc.26",
                "org.opencontainers.image.source": "https://github.com/QuantumNous/new-api",
            }},
        }

    def test_approved_digest_and_labels(self):
        fixture.validate_image("calciumion/new-api@" + fixture.INDEX, self.image)
        fixture.validate_image("mirror.example.com/calciumion/new-api@" + fixture.INDEX, self.image)
        for reference in ["calciumion/new-api:latest", "calciumion/new-api:v1.0.0-rc.26", "x@sha256:" + "0" * 64]:
            with self.assertRaisesRegex(RuntimeError, "newapi_digest_mismatch"):
                fixture.validate_image(reference, self.image)

    def test_mismatched_platform_or_provenance_rejected(self):
        for key, value in [("Os", "windows"), ("Architecture", "arm64")]:
            image = copy.deepcopy(self.image)
            image[key] = value
            with self.assertRaisesRegex(RuntimeError, "newapi_platform_mismatch"):
                fixture.validate_image("x@" + fixture.INDEX, image)
        for key in self.image["Config"]["Labels"]:
            image = copy.deepcopy(self.image)
            image["Config"]["Labels"][key] = "incorrect"
            with self.assertRaisesRegex(RuntimeError, "newapi_provenance_mismatch"):
                fixture.validate_image("x@" + fixture.INDEX, image)

    def test_cleanup_refuses_foreign_resource_and_continues_own(self):
        resources = [("network", "network-id"), ("container", "foreign-container")]
        def metadata(kind, target):
            return {"Labels": {fixture.LABEL: "own"}} if kind == "network" else {"Config": {"Labels": {fixture.LABEL: "other"}}}
        with patch.object(fixture, "inspect", side_effect=metadata), patch.object(fixture, "docker") as docker:
            with self.assertRaisesRegex(RuntimeError, "fixture_cleanup_failed"):
                fixture.cleanup(resources, "own")
            docker.assert_called_once_with("network", "rm", "network-id")

    def test_cleanup_uses_exact_ids_and_reverse_order(self):
        with patch.object(fixture, "inspect", side_effect=[{"Config": {"Labels": {fixture.LABEL: "own"}}}, {"Labels": {fixture.LABEL: "own"}}]), patch.object(fixture, "docker") as docker:
            fixture.cleanup([("network", "network-id"), ("container", "container-id")], "own")
            self.assertEqual(docker.call_args_list[0].args, ("container", "rm", "--force", "container-id"))
            self.assertEqual(docker.call_args_list[1].args, ("network", "rm", "network-id"))

    def test_no_redirect_with_credentials(self):
        with self.assertRaisesRegex(RuntimeError, "fixture_http_redirect_rejected"):
            fixture.NoRedirect().redirect_request(None, None, 302, "", {}, "https://foreign.example.com")

    def test_docker_failure_does_not_print_or_put_payload_in_arguments(self):
        output = io.StringIO()
        with patch.object(fixture.subprocess, "run", return_value=SimpleNamespace(returncode=1, stdout="private response", stderr="private diagnostic")) as run, redirect_stdout(output):
            with self.assertRaisesRegex(RuntimeError, "^docker_create_failed$"):
                fixture.docker("create", "--env-file", "/dev/stdin", payload="ephemeral-secret")
            self.assertNotIn("ephemeral-secret", run.call_args.args[0])
            self.assertEqual(run.call_args.kwargs["input"], "ephemeral-secret")
        self.assertEqual(output.getvalue(), "")


if __name__ == "__main__":
    unittest.main()

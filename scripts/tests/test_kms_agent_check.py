import copy
from datetime import datetime, timezone
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "account-platform-kms-agent-check.py"
SPEC = importlib.util.spec_from_file_location("kms_agent_check", SCRIPT)
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class KMSAgentCheckTests(unittest.TestCase):
    def test_renewal_uses_real_rfc3339_field_and_checks_time_bounds(self):
        now = datetime.now(timezone.utc).timestamp()
        good = {"creation_time": int(now) - 200,
                "last_renewal": datetime.fromtimestamp(now-2, timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")}
        CHECK.validate_renewal(good)
        CHECK.validate_renewal({**good, "last_renewal": good["last_renewal"].replace("Z", "123Z")})
        for value in (None, 0, "not-a-time", "2020-01-01T00:00:00Z", "2999-01-01T00:00:00Z", "2026-01-01T00:00:00"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                CHECK.validate_renewal({**good, "last_renewal": value})

    @unittest.skipUnless(os.name == "posix" and hasattr(os, "geteuid") and os.geteuid() == 0,
                         "requires Linux operator for actual UID/permissions")
    def test_actual_sink_permissions_links_and_bounded_token_read(self):
        with tempfile.TemporaryDirectory(prefix="pinvou-agent-check-test-") as directory:
            root = Path(directory).resolve()
            os.chmod(root, 0o700)
            os.chown(root, 65532, 65532)
            token = root / "token"
            token.write_bytes(b"test-only-token-value-not-a-real-secret")
            os.chmod(token, 0o600)
            os.chown(token, 65532, 65532)
            self.assertEqual(CHECK.read_token(token), "test-only-token-value-not-a-real-secret")
            link = root / "link"
            link.symlink_to(token)
            with self.assertRaises(ValueError):
                CHECK.read_token(link)
            os.chmod(token, 0o644)
            with self.assertRaises(ValueError):
                CHECK.read_token(token)
            os.chmod(token, 0o600)
            os.chown(token, 0, 0)
            with self.assertRaises(ValueError):
                CHECK.read_token(token)
            os.chown(token, 65532, 65532)
            for data in (b"x"*4097, b"test-token-must-not-contain-newlines\n", b"short"):
                token.write_bytes(data)
                with self.assertRaises(ValueError):
                    CHECK.read_token(token)

    def test_lease_requires_exact_role_and_bounded_live_renewable_token(self):
        role = "pinvou-token-gateway"
        good = dict(policies=[role], type="service", renewable=True, orphan=True,
                    ttl=200, explicit_max_ttl=900, path="auth/approle/login", meta={"role_name": role})
        CHECK.validate_lease(good, role)
        CHECK.validate_lease({**good, "identity_policies": []}, role)
        for value in (None, {}, "", False, 0, ["fixture-extra-policy"]):
            with self.subTest(identity_policies=value), self.assertRaises(ValueError):
                CHECK.validate_lease({**good, "identity_policies": value}, role)
        for field, value in (("policies", [role, "default"]), ("policies", ["pinvou-kms-backup"]),
                             ("ttl", 0), ("ttl", 301), ("explicit_max_ttl", 0), ("renewable", False),
                             ("orphan", False), ("type", "batch"), ("meta", {"role_name": "other"})):
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                CHECK.validate_lease({**good, field: value}, role)

    def test_actual_container_contract_rejects_cross_role_and_unsafe_mounts(self):
        role = "pinvou-token-gateway"
        root, trust = Path("/example/agents"), Path("/example/trust")
        good = {
            "State": {"Running": True},
            "Config": {"Image": CHECK.IMAGE, "User": "65532:65532", "Entrypoint": ["/usr/bin/bao"],
                       "Cmd": ["agent", "-config=/bao/config/agent.hcl"], "Env": ["PATH=/usr/bin"]},
            "HostConfig": {"ReadonlyRootfs": True, "Privileged": False, "CapDrop": ["ALL"], "CapAdd": [],
                           "SecurityOpt": ["no-new-privileges:true"], "PortBindings": {}, "Memory": 128*1024*1024,
                           "NanoCpus": 500000000, "PidsLimit": 64, "Ulimits": [{"Name": "core", "Soft": 0, "Hard": 0}],
                           "RestartPolicy": {"Name": "unless-stopped"}, "LogConfig": {"Type": "local"},
                           "Tmpfs": {path: "rw,nosuid,nodev,noexec" for path in ("/tmp", "/openbao/file", "/openbao/logs")}},
            "NetworkSettings": {"Networks": {"private-kms": {}}},
            "Mounts": [{"Type": "bind", "Source": str(src), "Destination": dst, "RW": rw} for src, dst, rw in
                       ((root / "config", "/bao/config", False), (trust, "/bao/trust", False),
                        (root / "bootstrap" / role, "/bao/bootstrap", False),
                        (CHECK.SINK_ROOT / role, "/bao/token", True))],
        }
        validate = lambda data: CHECK.validate_container(data, role, "private-kms", root, trust)
        validate(good)
        for field, value in (("ReadonlyRootfs", False), ("Privileged", True), ("CapAdd", ["IPC_LOCK"]),
                             ("PortBindings", {"8200/tcp": [{}]}), ("Memory", 0), ("PidsLimit", 0), ("Ulimits", [])):
            bad = copy.deepcopy(good)
            bad["HostConfig"][field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                validate(bad)
        for index, key, value in ((2, "RW", True), (3, "Source", "/other-role/token"), (0, "Type", "volume")):
            bad = copy.deepcopy(good)
            bad["Mounts"][index][key] = value
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                validate(bad)
        bad = copy.deepcopy(good)
        bad["NetworkSettings"]["Networks"]["public"] = {}
        with self.assertRaises(ValueError):
            validate(bad)

    def test_permission_probe_never_issues_privileged_mutations(self):
        class FakeAPI:
            def __init__(self):
                self.calls = []
                self.plaintext = None

            def call(self, path, token, body=None, expected=200, snapshot=False):
                self.calls.append((path, copy.deepcopy(body), expected, snapshot))
                if expected != 200 or snapshot:
                    return None
                if path.startswith("transit/encrypt/"):
                    self.plaintext = body["plaintext"]
                    return {"ciphertext": "test-only"}
                return {"plaintext": self.plaintext}

        for role in CHECK.ROLES:
            api = FakeAPI()
            CHECK.verify_permissions(api, role, "test-token-not-a-real-secret")
            self.assertTrue(any(status == 403 for _, _, status, _ in api.calls))
            for path, body, _, _ in api.calls:
                self.assertNotIn("rotate", path)
                self.assertNotIn("revoke", path)
                if body is not None:
                    self.assertTrue(path.startswith(("transit/encrypt/", "transit/decrypt/")))
            self.assertEqual(any(snapshot for _, _, _, snapshot in api.calls), role == "pinvou-kms-backup")

    def test_public_http_or_proxy_style_endpoint_is_rejected_before_ca_read(self):
        for endpoint in ("http://127.0.0.1:8200", "https://user:pass@127.0.0.1:8200",
                         "https://127.0.0.1:8200/path", "https://8.8.8.8:8200", "https://127.0.0.1:8200?x=y"):
            with self.subTest(endpoint=endpoint), self.assertRaises(ValueError):
                CHECK.API(endpoint, Path("missing-ca.crt"))


if __name__ == "__main__":
    unittest.main()

import copy
import importlib.util
from pathlib import Path
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "account-platform-kms-audit-reopen.py"
SPEC = importlib.util.spec_from_file_location("audit_reopen", SCRIPT)
MOD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MOD)


class AuditReopenTests(unittest.TestCase):
    def test_only_exact_kms_container_may_receive_signal(self):
        root = Path("/example")
        good = {"Id": "a"*64, "State": {"Running": True},
                "Config": {"Image": MOD.IMAGE, "User": "65532:65532",
                           "Entrypoint": ["/usr/bin/bao"], "Cmd": ["server", "-config=/bao/config/server.hcl"],
                           "Labels": {"com.docker.compose.project": "pinvou-security", "com.docker.compose.service": "openbao"}},
                "Mounts": [{"Destination": "/bao/audit", "Source": str(root / "openbao/audit"), "Type": "bind", "RW": True}]}
        self.assertEqual(MOD.validate_target(good, root), "a"*64)
        for field, value in (("Image", "unapproved"), ("User", "root"), ("Cmd", ["agent"]),
                             ("Labels", {"com.docker.compose.project": "other", "com.docker.compose.service": "openbao"})):
            bad = copy.deepcopy(good)
            bad["Config"][field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                MOD.validate_target(bad, root)
        bad = copy.deepcopy(good)
        bad["Mounts"][0]["Source"] = str(root / "other")
        with self.assertRaises(ValueError):
            MOD.validate_target(bad, root)


if __name__ == "__main__":
    unittest.main()

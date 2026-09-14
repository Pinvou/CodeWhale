"""Fixed first-install secrets use only independently generated test recipients."""
import hashlib
import importlib.util
import json
import os
import re
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("dependency_passwords", Path(__file__).resolve().parents[1] / "account-platform-dependency-passwords.py")
PREPARE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PREPARE)


class PasswordTests(unittest.TestCase):
    def test_fixed_identity_set(self):
        self.assertEqual(set(PREPARE.NAMES), {"POSTGRES_ADMIN_PASSWORD", "ACCOUNT_DB_PASSWORD", "BILLING_DB_PASSWORD", "TOKEN_DB_PASSWORD", "ACCOUNT_MIGRATION_DB_PASSWORD", "BILLING_MIGRATION_DB_PASSWORD", "TOKEN_MIGRATION_DB_PASSWORD", "TOKEN_POLICY_DB_PASSWORD", "NEWAPI_DB_PASSWORD", "KEYCLOAK_DB_PASSWORD", "PORTAL_REDIS_PASSWORD", "IDENTITY_REDIS_PASSWORD", "NEWAPI_REDIS_PASSWORD"})
        with self.assertRaises(ValueError):
            json.loads('{"age_binary":"a","age_binary":"b"}', object_pairs_hook=PREPARE.closed_object)

    def test_actual_generator_emits_all_thirteen_distinct_values(self):
        values = PREPARE.generate_password_values()
        self.assertEqual(set(values), set(PREPARE.NAMES))
        self.assertEqual(len(values), 13)
        self.assertEqual(len(set(values.values())), 13)
        self.assertTrue(all(re.fullmatch('[0-9a-f]{64}', value) for value in values.values()))

    @unittest.skipUnless(os.name == "posix" and os.environ.get("PINVOU_TEST_DEPENDENCY_BIN_DIRECTORY"), "explicit isolated Linux fixture")
    def test_actual_encrypted_first_and_recovery(self):
        bins = Path(os.environ["PINVOU_TEST_DEPENDENCY_BIN_DIRECTORY"])
        with tempfile.TemporaryDirectory(prefix="pinvou-password-fixture-") as raw:
            root = Path(raw)
            root.chmod(0o700)

            def run(args):
                result = subprocess.run(list(map(str, args)), capture_output=True, timeout=20)
                self.assertEqual(result.returncode, 0, "temporary age command failed")
                return result.stdout

            run([bins / "age-keygen", "-o", root / "identity"])
            recipient = run([bins / "age-keygen", "-y", root / "identity"]).strip()
            (root / "recipient").write_bytes(recipient)
            (root / "recipient").chmod(0o600)
            base = {"recipient_file": str(root / "recipient"), "recipient_sha256": hashlib.sha256(recipient).hexdigest(),
                    "age_binary": str(bins / "age"), "age_sha256": hashlib.sha256((bins / "age").read_bytes()).hexdigest(), "output_directory": str(root / "prepared")}
            config = root / "config.json"

            def config_write(value):
                config.write_text(json.dumps(value))
                config.chmod(0o600)

            config_write(base)
            digest = PREPARE.prepare(config)
            directory = root / "prepared"
            self.assertEqual(set(p.name for p in directory.iterdir()), set(PREPARE.NAMES) | {"passwords.age", "manifest.json", "READY"})
            manifest = json.loads((directory / "manifest.json").read_bytes())
            self.assertEqual(hashlib.sha256((directory / "manifest.json").read_bytes()).hexdigest(), digest)
            self.assertEqual((directory / "READY").read_text(), "DEPENDENCY_PASSWORDS_READY_V1\n" + digest + "\n")
            self.assertEqual(set(manifest["files"]), set(PREPARE.NAMES) | {"passwords.age"})
            envelope = json.loads(run([bins / "age", "-d", "-i", root / "identity", directory / "passwords.age"]))
            self.assertEqual(envelope["version"], 1)
            self.assertEqual(envelope["purpose"], PREPARE.PURPOSE)
            self.assertEqual(envelope["recipient_sha256"], base["recipient_sha256"])
            self.assertEqual(set(envelope["passwords"]), set(PREPARE.NAMES))
            self.assertEqual(len(set(envelope["passwords"].values())), 13)
            for name, value in envelope["passwords"].items():
                self.assertTrue(re.fullmatch('[0-9a-f]{64}', value), "test secret has invalid shape")
                self.assertTrue((directory / name).read_bytes() == value.encode(), "secret file differs from authenticated test envelope")
                self.assertTrue(value.encode() not in (directory / "passwords.age").read_bytes(), "test ciphertext contains plaintext canary")
            for name, expected in manifest["files"].items():
                self.assertEqual(hashlib.sha256((directory / name).read_bytes()).hexdigest(), expected)
                self.assertEqual((directory / name).stat().st_mode & 0o777, 0o600)
            before = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in directory.iterdir()}
            with self.assertRaises(FileExistsError):
                PREPARE.prepare(config)
            self.assertEqual({p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in directory.iterdir()}, before)
            for field, value in (("recipient_sha256", "0" * 64), ("age_sha256", "0" * 64), ("unexpected", "value")):
                config_write({**base, "output_directory": str(root / "invalid"), field: value})
                with self.assertRaises(ValueError):
                    PREPARE.prepare(config)
                self.assertFalse((root / "invalid").exists())
            config_write({**base, "output_directory": str(root / "invalid")})
            config.chmod(0o640)
            with self.assertRaises(ValueError):
                PREPARE.prepare(config)
            config.chmod(0o600)
            (root / "recipient").chmod(0o666)
            with self.assertRaises(ValueError):
                PREPARE.prepare(config)
            (root / "recipient").chmod(0o600)
            os.symlink(root / "recipient", root / "recipient-link")
            config_write({**base, "recipient_file": str(root / "recipient-link"), "output_directory": str(root / "invalid")})
            with self.assertRaises(ValueError):
                PREPARE.prepare(config)
            self.assertFalse((root / "invalid").exists())
            (root / "unsafe-parent").mkdir(mode=0o755)
            (root / "unsafe-parent").chmod(0o755)
            config_write({**base, "output_directory": str(root / "unsafe-parent/output")})
            with self.assertRaises(ValueError):
                PREPARE.prepare(config)
            self.assertFalse((root / "unsafe-parent/output").exists())
            config_write({**base, "output_directory": str(root / "encryption-failed")})
            with patch.object(PREPARE.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, b"", b"not printed")):
                with self.assertRaises(ValueError):
                    PREPARE.prepare(config)
            self.assertEqual(list((root / "encryption-failed").iterdir()), [])
            with self.assertRaises(FileExistsError):
                PREPARE.prepare(config)
            config_write({**base, "output_directory": str(root / "persistence-failed")})
            with patch.object(PREPARE, "write_new", side_effect=OSError("test-only persistence failure")):
                with self.assertRaises(OSError):
                    PREPARE.prepare(config)
            self.assertEqual(list((root / "persistence-failed").iterdir()), [])
            print("DEPENDENCY_PASSWORD_ENCRYPTED_FIRST_RECOVERY_FAILURE_PASS", flush=True)


if __name__ == "__main__":
    unittest.main()

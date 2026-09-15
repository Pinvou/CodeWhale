"""Only fresh temporary fixture identities and output directories are accepted."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("projection", Path(__file__).resolve().parents[1] / "account-platform-project-dependencies.py")
PROJECT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROJECT)


class ProjectionTests(unittest.TestCase):
    def test_fixed_template_hashes(self):
        root = Path(__file__).resolve().parents[2] / "account-platform/deploy/production"
        for name, expected in PROJECT.TEMPLATES.items():
            self.assertEqual(hashlib.sha256((root / name).read_bytes().replace(b"\r\n", b"\n")).hexdigest(), expected)

    def test_postgres_bootstrap_mapping_is_explicit_and_complete(self):
        expected = {"ACCOUNT_DB_PASSWORD", "BILLING_DB_PASSWORD", "TOKEN_DB_PASSWORD",
                    "ACCOUNT_MIGRATION_DB_PASSWORD", "BILLING_MIGRATION_DB_PASSWORD",
                    "ACCOUNT_APPROVAL_WRITER_DB_PASSWORD", "ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD",
                    "TOKEN_MIGRATION_DB_PASSWORD", "TOKEN_POLICY_DB_PASSWORD",
                    "NEWAPI_DB_PASSWORD", "KEYCLOAK_DB_PASSWORD"}
        self.assertEqual(set(PROJECT.POSTGRES_BOOTSTRAP_PASSWORD_NAMES), expected)
        password_bytes = {name: name.encode() for name in PROJECT.PASSWORD.NAMES}
        projected = PROJECT.postgres_bootstrap_files(password_bytes)
        expected_paths = {"postgres/bootstrap/" + name for name in expected}
        self.assertEqual(set(projected), expected_paths)
        self.assertEqual(projected["postgres/bootstrap/ACCOUNT_MIGRATION_DB_PASSWORD"],
                         (b"ACCOUNT_MIGRATION_DB_PASSWORD", 999))
        self.assertEqual(projected["postgres/bootstrap/KEYCLOAK_DB_PASSWORD"],
                         (b"KEYCLOAK_DB_PASSWORD", 999))

    @unittest.skipUnless(os.name == "posix" and os.geteuid() == 0 and os.environ.get("PINVOU_TEST_DEPENDENCY_BIN_DIRECTORY"), "explicit isolated root Linux fixture")
    def test_fixed_projection_and_failures(self):
        bins = Path(os.environ["PINVOU_TEST_DEPENDENCY_BIN_DIRECTORY"])
        with tempfile.TemporaryDirectory(prefix="pinvou-projection-fixture-") as raw:
            root = Path(raw)
            source = root / "materials"
            source.mkdir(mode=0o700)

            def run(args):
                result = subprocess.run(list(map(str, args)), capture_output=True, timeout=20)
                self.assertEqual(result.returncode, 0, "temporary fixture command failed")
                return result.stdout

            def private(file, body):
                file.write_bytes(body)
                file.chmod(0o600)

            run([bins / "age-keygen", "-o", root / "identity"])
            recipient = run([bins / "age-keygen", "-y", root / "identity"]).strip()
            private(root / "recipient", recipient)
            fingerprint = hashlib.sha256(recipient).hexdigest()
            for name, binary in (("pki", "infrastructure-pki"), ("nats", "nats-identities")):
                config = root / (name + ".json")
                private(config, json.dumps({"recipient_file": str(root / "recipient"), "recipient_sha256": fingerprint,
                                           "output_directory": str(source / name)}).encode())
                run([bins / binary, "-config", config])
            config = root / "passwords.json"
            private(config, json.dumps({"recipient_file": str(root / "recipient"), "recipient_sha256": fingerprint,
                                       "age_binary": str(bins / "age"), "age_sha256": hashlib.sha256((bins / "age").read_bytes()).hexdigest(),
                                       "output_directory": str(source / "passwords")}).encode())
            password_digest = PROJECT.PASSWORD.prepare(config)
            public = source / "nats/public-keys.json"
            run([bins / "event-resources", "-render-authorization", "-public-keys", public, "-public-keys-sha256",
                 hashlib.sha256(public.read_bytes()).hexdigest(), "-output", source / "nats-authorization.conf"])
            template_root = root / "templates"
            template_root.mkdir(mode=0o700)
            for name in PROJECT.TEMPLATES:
                private(template_root / name, (Path(__file__).resolve().parents[2] / "account-platform/deploy/production" / name).read_bytes())
            config = root / "project.json"
            base = {"material_directory": str(source), "output_directory": str(root / "output"), "template_directory": str(template_root),
                    "recipient_sha256": fingerprint, "password_manifest_sha256": password_digest,
                    "pki_manifest_sha256": hashlib.sha256((source / "pki/manifest.json").read_bytes()).hexdigest(),
                    "nats_manifest_sha256": hashlib.sha256((source / "nats/manifest.json").read_bytes()).hexdigest(),
                    "nats_acl_sha256": hashlib.sha256((source / "nats-authorization.conf").read_bytes()).hexdigest()}

            def configure(value):
                private(config, json.dumps(value).encode())

            def source_state():
                return {p.relative_to(source).as_posix(): (p.stat().st_uid, p.stat().st_gid, p.stat().st_mode,
                        hashlib.sha256(p.read_bytes()).hexdigest() if p.is_file() else None) for p in source.rglob("*")}

            before = source_state()
            configure(base)
            digest = PROJECT.project(config)
            output = root / "output"
            manifest = json.loads((output / "manifest.json").read_bytes())
            self.assertEqual(hashlib.sha256((output / "manifest.json").read_bytes()).hexdigest(), digest)
            self.assertEqual((output / "READY").read_text(), "DEPENDENCY_PROJECTION_READY_V1\n" + digest + "\n")
            expected_files = {f"{s}/tls/server.{x}" for s in ("postgres", "redis", "newapi-redis", "nats") for x in ("crt", "key")}
            db_names = {"ACCOUNT_DB_PASSWORD", "BILLING_DB_PASSWORD", "TOKEN_DB_PASSWORD", "ACCOUNT_MIGRATION_DB_PASSWORD", "BILLING_MIGRATION_DB_PASSWORD", "TOKEN_MIGRATION_DB_PASSWORD", "TOKEN_POLICY_DB_PASSWORD", "NEWAPI_DB_PASSWORD", "KEYCLOAK_DB_PASSWORD"}
            expected_files |= {"postgres/bootstrap/" + n for n in db_names} | {"postgres/admin/password", "postgres/trust/ca.crt", "trust/ca.crt", "nats/authorization/authorization.conf", "redis/authorization/users.acl", "newapi-redis/authorization/users.acl"}
            self.assertEqual(set(manifest["files"]), expected_files)
            for name, metadata in manifest["files"].items():
                path = output / name
                self.assertEqual(hashlib.sha256(path.read_bytes()).hexdigest(), metadata["sha256"])
                expected_uid = 999 if name.startswith("postgres/") else 65532
                self.assertEqual((metadata["uid"], metadata["gid"], path.stat().st_uid, path.stat().st_gid, path.stat().st_mode & 0o777), (expected_uid, expected_uid, expected_uid, expected_uid, 0o600))
            for service in ("postgres", "redis", "newapi-redis", "nats"):
                for suffix in ("crt", "key"):
                    self.assertTrue((output / f"{service}/tls/server.{suffix}").read_bytes() == (source / f"pki/{service}/server.{suffix}").read_bytes(), "wrong leaf projection")
                self.assertEqual(list((output / service / "data").iterdir()), [])
            for name in ("trust/ca.crt", "postgres/trust/ca.crt"):
                self.assertTrue((output / name).read_bytes() == (source / "pki/ca.crt").read_bytes(), "wrong public root projection")
            for user, service in (("PORTAL", "redis"), ("IDENTITY", "redis"), ("NEWAPI", "newapi-redis")):
                secret = (source / "passwords" / (user + "_REDIS_PASSWORD")).read_bytes()
                acl = (output / service / "authorization/users.acl").read_bytes()
                self.assertTrue(hashlib.sha256(secret).hexdigest().encode() in acl and secret not in acl, "cache hash projection mismatch")
            for name in db_names:
                self.assertTrue((output / "postgres/bootstrap" / name).read_bytes() == (source / "passwords" / name).read_bytes(), "wrong database password mapping")
            self.assertEqual(source_state(), before)
            with self.assertRaises(FileExistsError):
                PROJECT.project(config)
            for field in ("pki_manifest_sha256", "nats_manifest_sha256", "password_manifest_sha256", "nats_acl_sha256", "recipient_sha256"):
                configure({**base, "output_directory": str(root / "invalid"), field: "0" * 64})
                with self.assertRaises(ValueError):
                    PROJECT.project(config)
                self.assertFalse((root / "invalid").exists())
            configure({**base, "output_directory": str(root / "invalid")})
            template = template_root / "redis-accounts.acl.template"
            original = template.read_bytes()
            private(template, original + b"user forbidden on nopass +@all\n")
            with self.assertRaises(ValueError):
                PROJECT.project(config)
            private(template, original)
            config.chmod(0o640)
            with self.assertRaises(ValueError):
                PROJECT.project(config)
            config.chmod(0o600)
            link = root / "linked"
            os.symlink(output, link)
            configure({**base, "output_directory": str(link)})
            with self.assertRaises(FileExistsError):
                PROJECT.project(config)
            configure({**base, "output_directory": str(root / "partial")})
            with patch.object(PROJECT, "write_file", side_effect=OSError("fixture persistence failure")):
                with self.assertRaises(OSError):
                    PROJECT.project(config)
            self.assertTrue((root / "partial").is_dir())
            self.assertFalse((root / "partial/READY").exists())
            with self.assertRaises(FileExistsError):
                PROJECT.project(config)
            self.assertEqual(source_state(), before)
            print("DEPENDENCY_FIXED_PROJECTION_SOURCE_PRESERVATION_FAILURE_PASS", flush=True)


if __name__ == "__main__":
    unittest.main()

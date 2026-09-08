"""No production inputs; the optional Linux case generates its own age identity."""
import hashlib
import base64
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("dependency_check", Path(__file__).resolve().parents[1] / "account-platform-dependency-bundle-check.py")
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class BundleCheckTests(unittest.TestCase):
    def test_duplicate_json_is_rejected(self):
        with self.assertRaises(ValueError):
            json.loads('{"files":{"a":"1","a":"2"}}', object_pairs_hook=CHECK.closed_object)

    @unittest.skipUnless(os.name == "posix", "POSIX deployment ownership check")
    def test_partial_bundle_refused(self):
        with tempfile.TemporaryDirectory(prefix="pinvou-bundle-negative-") as raw:
            directory = Path(raw)
            with self.assertRaises(ValueError):
                CHECK.manifest(directory, "0" * 64, 2, "purpose", {"missing"}, set(), "MARKER")

    @unittest.skipUnless(os.name == "posix" and os.environ.get("PINVOU_TEST_DEPENDENCY_BIN_DIRECTORY"), "explicit isolated Linux fixture")
    def test_actual_generated_bundles_and_tamper(self):
        bins = Path(os.environ["PINVOU_TEST_DEPENDENCY_BIN_DIRECTORY"])
        with tempfile.TemporaryDirectory(prefix="pinvou-bundle-fixture-") as raw:
            root = Path(raw)
            root.chmod(0o700)
            import subprocess

            def run(args):
                result = subprocess.run(list(map(str, args)), capture_output=True, timeout=20)
                self.assertEqual(result.returncode, 0, "fixture command failed")
                return result.stdout

            run([bins / "age-keygen", "-o", root / "identity"])
            recipient = run([bins / "age-keygen", "-y", root / "identity"]).strip()
            (root / "recipient").write_bytes(recipient)
            (root / "recipient").chmod(0o600)
            fingerprint = hashlib.sha256(recipient).hexdigest()
            for output, binary in (("pki", "infrastructure-pki"), ("nats", "nats-identities")):
                config = root / (output + ".json")
                config.write_text(json.dumps({"recipient_file": str(root / "recipient"), "recipient_sha256": fingerprint, "output_directory": str(root / output)}))
                config.chmod(0o600)
                run([bins / binary, "-config", config])
            verify = lambda: CHECK.verify(root / "pki", root / "nats", fingerprint)
            result = verify()
            self.assertEqual(len(result), 3)
            for file in (root / "pki/READY", root / "nats/public-keys.json", root / "pki/redis/server.key"):
                original = file.read_bytes()
                file.write_bytes(original + b"tamper")
                with self.assertRaises(ValueError):
                    verify()
                file.write_bytes(original)
            extra = root / "pki/extra"
            extra.write_text("unexpected")
            extra.chmod(0o600)
            with self.assertRaises(ValueError):
                verify()
            extra.unlink()
            key = root / "pki/redis/server.key"
            key.chmod(0o640)
            with self.assertRaises(ValueError):
                verify()
            key.chmod(0o600)
            os.symlink(key, root / "pki/link")
            with self.assertRaises(ValueError):
                verify()
            (root / "pki/link").unlink()
            self.assertEqual(verify(), result)
            # This identity was created above, exclusively for this fixture. Never
            # accept a production identity/root package as an integration input.
            envelope = json.loads(run([bins / "age", "-d", "-i", root / "identity", root / "pki/ca-private.age"]))
            self.assertEqual(envelope["purpose"], "pinvou-platform-internal-ca")
            (root / "fixture-ca.der").write_bytes(base64.b64decode(envelope["private_pkcs8"]))
            (root / "fixture-ca.der").chmod(0o600)
            run(["openssl", "pkey", "-inform", "DER", "-in", root / "fixture-ca.der", "-out", root / "fixture-ca.pem"])
            (root / "fixture-ca.pem").chmod(0o600)
            run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-days", "30", "-subj", "/CN=Other test root", "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign", "-keyout", root / "other.key", "-out", root / "other.crt"])
            run(["openssl", "req", "-new", "-key", key, "-subj", "/CN=redis", "-out", root / "leaf.csr"])
            meta, ready = root / "pki/manifest.json", root / "pki/READY"

            def semantic_rejection(changes):
                originals = {file: file.read_bytes() for file in (*changes, meta, ready)}
                try:
                    for file, body in changes.items():
                        file.write_bytes(body)
                    document = json.loads(meta.read_bytes())
                    for file in changes:
                        document["files"][file.relative_to(root / "pki").as_posix()] = hashlib.sha256(file.read_bytes()).hexdigest()
                    meta.write_text(json.dumps(document))
                    ready.write_text("INFRASTRUCTURE_PKI_READY_V2\n" + hashlib.sha256(meta.read_bytes()).hexdigest() + "\n")
                    with self.assertRaises(ValueError):
                        verify()
                finally:
                    for file, body in originals.items():
                        file.write_bytes(body)
                self.assertEqual(verify(), result)

            cert = root / "pki/redis/server.crt"
            semantic_rejection({key: (root / "pki/newapi/server.key").read_bytes()})
            semantic_rejection({root / "pki/ca.crt": (root / "pki/ca.crt").read_bytes() + (root / "other.crt").read_bytes()})
            semantic_rejection({cert: cert.read_bytes() + (root / "other.crt").read_bytes()})
            for name, eku, days, ca_flag, wrong_root in (("wrong", "serverAuth", "30", "FALSE", False),
                                                       ("redis", "clientAuth", "30", "FALSE", False),
                                                       ("redis", "serverAuth", "1", "FALSE", False),
                                                       ("redis", "serverAuth", "30", "TRUE", False),
                                                       ("redis", "serverAuth", "30", "FALSE", True)):
                (root / "leaf.ext").write_text(f"subjectAltName=DNS:{name}\nextendedKeyUsage={eku}\nbasicConstraints=critical,CA:{ca_flag}\n")
                run(["openssl", "x509", "-req", "-in", root / "leaf.csr", "-CA", root / ("other.crt" if wrong_root else "pki/ca.crt"),
                     "-CAkey", root / ("other.key" if wrong_root else "fixture-ca.pem"), "-set_serial", "123", "-days", days, "-extfile", root / "leaf.ext", "-out", root / "bad.crt"])
                semantic_rejection({cert: (root / "bad.crt").read_bytes()})
            print("DEPENDENCY_BUNDLE_GENERATION_VALIDATION_TAMPER_PASS", flush=True)


if __name__ == "__main__":
    unittest.main()

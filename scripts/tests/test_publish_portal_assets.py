import hashlib
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "publish-portal-assets.py"


class PortalAssetPublisherTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.origin = self.root / "origin"
        (self.origin / "assets").mkdir(parents=True)
        self.old = self.release("old", {"account-abcdefgh.js": b"old", "common-abcd1234.js": b"shared"})
        self.new = self.release("new", {"account-ijklmnop.js": b"new", "common-abcd1234.js": b"shared"})

    def release(self, name, assets):
        path = self.root / name
        (path / "assets").mkdir(parents=True)
        for filename, body in assets.items():
            (path / "assets" / filename).write_bytes(body)
        (path / "admin.html").write_bytes(b"never publish HTML to the asset origin")
        return path

    def publish(self, release, *, check=False):
        args = [sys.executable, SCRIPT, release, self.origin]
        if check:
            args.append("--check")
        return subprocess.run(args, capture_output=True, text=True, check=False)

    def test_old_and_new_assets_coexist_and_survive_rollback(self):
        self.assertNotEqual(self.publish(self.old, check=True).returncode, 0)
        self.assertEqual(list((self.origin / "assets").iterdir()), [])
        for release in [self.old, self.new, self.old]:
            result = self.publish(release)
            self.assertEqual(result.returncode, 0, result.stderr)
        files = sorted(p.name for p in (self.origin / "assets").iterdir())
        self.assertEqual(files, ["account-abcdefgh.js", "account-ijklmnop.js", "common-abcd1234.js"])
        self.assertEqual(self.publish(self.old, check=True).returncode, 0)
        self.assertEqual(self.publish(self.new, check=True).returncode, 0)
        self.assertFalse((self.origin / "admin.html").exists())

    def test_existing_immutable_asset_is_not_overwritten(self):
        self.assertEqual(self.publish(self.old).returncode, 0)
        duplicate = self.release("collision", {"account-abcdefgh.js": b"bad"})
        original = self.origin / "assets" / "account-abcdefgh.js"
        before = hashlib.sha256(original.read_bytes()).digest()
        for check in (False, True):
            result = self.publish(duplicate, check=check)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("different bytes", result.stderr)
            self.assertEqual(hashlib.sha256(original.read_bytes()).digest(), before)

    def test_rejected_release_does_not_publish_nonasset_files(self):
        invalid = self.release("invalid", {"private.txt": b"do not publish"})
        result = self.publish(invalid)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not a regular, hashed Portal asset", result.stderr)
        self.assertEqual(list((self.origin / "assets").iterdir()), [])

    def test_partial_publish_keeps_complete_files_and_retries(self):
        self.assertEqual(self.publish(self.old).returncode, 0)
        release = self.release("partial", {
            "a-new-abcdefgh.js": b"complete",
            "common-abcd1234.js": b"collision",
        })
        result = self.publish(release)
        self.assertNotEqual(result.returncode, 0)
        shared = self.origin / "assets"
        self.assertEqual((shared / "a-new-abcdefgh.js").read_bytes(), b"complete")
        self.assertEqual((shared / "common-abcd1234.js").read_bytes(), b"shared")
        self.assertFalse(any(path.name.startswith(".portal-") for path in shared.iterdir()))

        corrected = self.release("corrected", {
            "a-new-abcdefgh.js": b"complete",
            "common-abcd1234.js": b"shared",
        })
        self.assertEqual(self.publish(corrected).returncode, 0)
        self.assertEqual(self.publish(corrected, check=True).returncode, 0)

    def test_does_not_create_an_unprovisioned_asset_directory(self):
        (self.origin / "assets").rmdir()
        self.assertNotEqual(self.publish(self.old).returncode, 0)
        self.assertEqual(list(self.origin.iterdir()), [])

    def test_concurrent_equal_publications_are_idempotent(self):
        args = [sys.executable, SCRIPT, self.old, self.origin]
        with subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE) as peer:
            local = self.publish(self.old)
            _, errors = peer.communicate(timeout=10)
        self.assertEqual(peer.returncode, 0, errors.decode())
        self.assertEqual(local.returncode, 0, local.stderr)
        self.assertEqual(self.publish(self.old, check=True).returncode, 0)

    @unittest.skipIf(os.name == "nt", "creating symlinks on Windows may require privileges")
    def test_rejects_source_and_origin_links(self):
        payload = self.old / "assets" / "account-abcdefgh.js"
        (self.old / "assets" / "not-linked-abcdefgh.js").symlink_to(payload)
        self.assertNotEqual(self.publish(self.old).returncode, 0)
        (self.origin / "assets").rmdir()
        (self.origin / "assets").symlink_to(self.new / "assets", target_is_directory=True)
        self.assertNotEqual(self.publish(self.new).returncode, 0)


if __name__ == "__main__":
    unittest.main()

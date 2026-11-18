import concurrent.futures
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
import urllib.error
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/lib"))
from ci_frontend_cache import Store  # noqa: E402

SPEC = importlib.util.spec_from_file_location("frontend_bundle", ROOT / "scripts/ci-frontend-bundle.py")
BUNDLE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BUNDLE)
COMMIT = "1" * 40


class StoreTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.store = Store(self.temporary.name, reserve=0)
        self.body = b"immutable frontend bundle"
        self.digest = hashlib.sha256(self.body).hexdigest()

    def publish(self, commit=COMMIT):
        return self.store.put(commit, self.digest, io.BytesIO(self.body), len(self.body))

    def test_correct_commit_and_digest_are_required_for_hit(self):
        self.publish()
        self.assertIsNone(self.store.open("2" * 40, self.digest))
        stream, size = self.store.open(COMMIT, self.digest)
        with stream:
            self.assertEqual(stream.read(), self.body)
        self.assertEqual(size, len(self.body))

    def test_bad_digest_never_becomes_visible(self):
        with self.assertRaises(ValueError):
            self.store.put(COMMIT, "0" * 64, io.BytesIO(self.body), len(self.body))
        self.assertEqual(list(self.store.objects.iterdir()), [])
        self.assertEqual(list(self.store.temporary.iterdir()), [])

    def test_partial_upload_never_becomes_visible(self):
        with self.assertRaises(ValueError):
            self.store.put(COMMIT, self.digest, io.BytesIO(self.body[:-1]), len(self.body))
        self.assertIsNone(self.store.open(COMMIT, self.digest))
        self.assertEqual(list(self.store.temporary.iterdir()), [])

    def test_corrupted_local_object_is_not_served(self):
        self.publish()
        (self.store.objects / self.digest).write_bytes(b"corrupted")
        self.assertIsNone(self.store.open(COMMIT, self.digest))

    def test_concurrent_publications_share_complete_immutable_bytes(self):
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            list(pool.map(lambda unused: self.publish(), range(8)))
        stream, _ = self.store.open(COMMIT, self.digest)
        with stream:
            self.assertEqual(stream.read(), self.body)
        self.assertEqual(len(list(self.store.objects.iterdir())), 1)
        self.assertEqual(list(self.store.temporary.iterdir()), [])

    def test_retention_keeps_new_entry_and_evicts_only_owned_cache_objects(self):
        self.publish()
        self.store.max_entries = 1
        second = b"new frontend bundle"
        digest = hashlib.sha256(second).hexdigest()
        self.store.put("2" * 40, digest, io.BytesIO(second), len(second))
        self.assertIsNone(self.store.open(COMMIT, self.digest))
        stream, _ = self.store.open("2" * 40, digest)
        stream.close()

    def test_disk_reserve_and_blob_cap_are_enforced(self):
        with patch("ci_frontend_cache.shutil.disk_usage", return_value=SimpleNamespace(free=0)):
            with self.assertRaises(OSError):
                self.publish()
        self.store.max_blob = 1
        with self.assertRaises(ValueError):
            self.publish()

    def test_path_traversal_is_rejected(self):
        for commit, digest in (("../" + COMMIT, self.digest), (COMMIT, "../" + self.digest)):
            with self.assertRaises(ValueError):
                self.store.open(commit, digest)


class BundleTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.workspace = self.root / "workspace"
        (self.workspace / "pinvou3-app").mkdir(parents=True)
        self.archive = self.root / "bundle.tar.gz"

    def archive_entries(self, entries):
        with tarfile.open(self.archive, "w:gz") as archive:
            for name, kind in entries:
                member = tarfile.TarInfo(name)
                if kind == "link":
                    member.type = tarfile.SYMTYPE
                    member.linkname = "/outside"
                    archive.addfile(member)
                else:
                    member.size = 2
                    archive.addfile(member, io.BytesIO(b"ok"))
        return hashlib.sha256(self.archive.read_bytes()).hexdigest()

    def unpack(self, digest):
        args = SimpleNamespace(commit=COMMIT, sha256=digest, workspace=self.workspace, bundle=self.archive)
        with patch.object(BUNDLE, "tested_commit", return_value=COMMIT), patch.object(BUNDLE.Path, "cwd", return_value=self.workspace / "pinvou3-app"):
            BUNDLE.unpack(args)

    def test_valid_bundle_extracts_independent_job_outputs(self):
        digest = self.archive_entries([("pinvou3-app/dist/index.html", "file"), ("remote-control-relay/web/dist/index.html", "file")])
        self.unpack(digest)
        self.assertEqual((self.workspace / "pinvou3-app/dist/index.html").read_text(), "ok")

    def test_wrong_checksum_preserves_existing_output(self):
        self.archive_entries([("pinvou3-app/dist/index.html", "file")])
        target = self.workspace / "pinvou3-app/dist"
        target.mkdir()
        (target / "index.html").write_text("previous")
        with self.assertRaises(ValueError):
            self.unpack("0" * 64)
        self.assertEqual((target / "index.html").read_text(), "previous")

    def test_unsafe_archive_and_missing_entries_are_rejected_before_publication(self):
        for name, kind in (("../escape", "file"), ("/absolute", "file"), ("pinvou3-app/dist/link", "link"), ("other/index.html", "file")):
            with self.subTest(name=name):
                digest = self.archive_entries([(name, kind)])
                with self.assertRaises(ValueError):
                    self.unpack(digest)
                self.assertFalse((self.workspace / "pinvou3-app/dist").exists())

    def test_non_loopback_cache_configuration_is_rejected(self):
        for value in ("http://example.invalid", "http://user:secret@localhost", "http://localhost/path"):
            with self.subTest(value=value), patch.dict(os.environ, {"PINVOU_FRONTEND_CACHE_URL": value}):
                with self.assertRaises(ValueError):
                    BUNDLE.cache_url()

    def test_publication_without_host_cache_keeps_github_delivery(self):
        digest = self.archive_entries([("pinvou3-app/dist/index.html", "file")])
        output = self.root / "output"
        with patch.object(BUNDLE, "tested_commit", return_value=COMMIT), patch.dict(os.environ, {"PINVOU_FRONTEND_CACHE_URL": "", "GITHUB_OUTPUT": str(output)}):
            BUNDLE.publish(self.archive)
        self.assertIn("transport=github", output.read_text())
        self.assertIn("sha256=" + digest, output.read_text())

    def test_fetch_rejects_a_different_checked_out_commit(self):
        args = SimpleNamespace(commit="2" * 40, sha256="3" * 64, size=20, output=self.root / "download", transport="lan")
        with patch.object(BUNDLE, "tested_commit", return_value=COMMIT):
            with self.assertRaises(ValueError):
                BUNDLE.fetch(args)
        self.assertFalse(args.output.exists())

    def test_file_backed_publication_keeps_content_length_and_lan_receipt(self):
        digest = self.archive_entries([("pinvou3-app/dist/index.html", "file")])
        body = self.archive.read_bytes()
        output = self.root / "output"
        def request(request, timeout):
            self.assertEqual(request.get_header("Content-length"), str(len(body)))
            self.assertEqual(request.data.read(), body)
            return io.BytesIO(json.dumps({"commit": COMMIT, "sha256": digest, "replicas": 3}).encode())
        client = SimpleNamespace(open=request)
        with patch.object(BUNDLE, "tested_commit", return_value=COMMIT), patch.object(BUNDLE.urllib.request, "build_opener", return_value=client), patch.dict(os.environ, {"PINVOU_FRONTEND_CACHE_URL": "http://localhost:17898", "GITHUB_REPOSITORY": "fixture/repo", "GITHUB_OUTPUT": str(output)}):
            BUNDLE.publish(self.archive)
        self.assertIn("transport=lan", output.read_text())

    def test_cache_hash_mismatch_does_not_replace_existing_job_file(self):
        body = b"correct bundle"
        digest = hashlib.sha256(body).hexdigest()
        args = SimpleNamespace(commit=COMMIT, sha256=digest, size=len(body), output=self.root / "download", transport="lan")
        args.output.write_bytes(b"previous")
        response = io.BytesIO(b"x" * len(body))
        response.headers = {"X-Tested-Commit": COMMIT, "X-Content-SHA256": digest,
                            "Content-Length": str(len(body)), "X-Cache-Source": "local-hit"}
        client = SimpleNamespace(open=lambda *args, **kwargs: response)
        with patch.object(BUNDLE, "tested_commit", return_value=COMMIT), patch.object(BUNDLE.urllib.request, "build_opener", return_value=client), patch.dict(os.environ, {"PINVOU_FRONTEND_CACHE_URL": "http://localhost:17898", "GITHUB_REPOSITORY": "fixture/repo"}):
            with self.assertRaises(ValueError):
                BUNDLE.fetch(args)
        self.assertEqual(args.output.read_bytes(), b"previous")

    def test_github_authorization_is_not_forwarded_to_storage_redirect(self):
        body = b"archive"
        listing = io.BytesIO(json.dumps({"artifacts": [{"id": 1, "name": "frontend-setup-bundle", "expired": False, "size_in_bytes": len(body)}]}).encode())
        def api(request, timeout):
            self.assertEqual(request.get_header("Authorization"), "Bearer canary")
            if "/runs/" in request.full_url:
                return listing
            raise urllib.error.HTTPError(request.full_url, 302, "redirect", {"Location": "https://storage.example.invalid/object?signature=canary"}, None)
        def storage(request, timeout):
            self.assertIsNone(request.get_header("Authorization"))
            response = io.BytesIO(body)
            response.status = 200
            return response
        with patch.object(BUNDLE.urllib.request, "build_opener", return_value=SimpleNamespace(open=api)), patch.object(BUNDLE.urllib.request, "urlopen", side_effect=storage), patch.dict(os.environ, {"GITHUB_REPOSITORY": "fixture/repo", "GITHUB_RUN_ID": "123", "GITHUB_TOKEN": "canary", "GITHUB_API_URL": "https://api.github.com"}):
            BUNDLE.github_zip(self.root / "archive", BUNDLE.time.monotonic() + 10)
        self.assertEqual((self.root / "archive").read_bytes(), body)


if __name__ == "__main__":
    unittest.main()

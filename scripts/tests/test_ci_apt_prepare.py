import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location('ci_apt', Path(__file__).parents[1] / 'ci-apt-prepare.py')
APT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(APT)


class AptTrustTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.sources = self.root / 'etc/apt/sources.list.d'
        self.sources.mkdir(parents=True)

    def test_only_active_llvm_sources_are_considered(self):
        (self.sources / 'other.list').write_text('deb https://example.invalid/llvm jammy main\n# deb https://apt.llvm.org/jammy llvm-toolchain-jammy-18 main\n')
        self.assertEqual(APT.targets(self.root), [])
        (self.sources / 'llvm.list').write_text('deb https://apt.llvm.org/jammy llvm-toolchain-jammy-18 main\n')
        self.assertEqual(APT.targets(self.root), [self.root / 'etc/apt/trusted.gpg.d/pinvou-llvm.gpg'])

    def test_signed_by_scope_is_retained(self):
        (self.sources / 'llvm.list').write_text('deb [signed-by=/usr/share/keyrings/llvm.gpg] https://apt.llvm.org/jammy llvm-toolchain-jammy-18 main\n')
        self.assertEqual(APT.targets(self.root), [self.root / 'usr/share/keyrings/llvm.gpg'])

    def test_deb822_disabled_source_is_not_modified(self):
        (self.sources / 'llvm.sources').write_text('Types: deb\nURIs: https://apt.llvm.org/jammy\nSigned-By: /etc/apt/keyrings/llvm.asc\nEnabled: no\n')
        self.assertEqual(APT.targets(self.root), [])

    def test_unrecognized_key_scope_fails_before_writing(self):
        (self.sources / 'llvm.list').write_text('deb [signed-by=/tmp/untrusted-key.gpg] https://apt.llvm.org/jammy llvm-toolchain-jammy-18 main\n')
        with self.assertRaises(ValueError):
            APT.targets(self.root)

    def test_inline_comment_is_not_an_active_llvm_source(self):
        (self.sources / 'other.list').write_text('deb https://example.invalid jammy main # https://apt.llvm.org/jammy\n')
        self.assertEqual(APT.targets(self.root), [])

    def test_multiple_key_scopes_need_explicit_review(self):
        (self.sources / 'llvm.sources').write_text('Types: deb\nURIs: https://apt.llvm.org/jammy\nSigned-By: /etc/apt/keyrings/a.gpg /etc/apt/keyrings/b.gpg\n')
        with self.assertRaises(ValueError):
            APT.targets(self.root)

    def test_vendor_certificate_is_the_pinned_official_public_key(self):
        data = (Path(__file__).parents[1] / 'vendor/llvm-snapshot.asc').read_bytes()
        self.assertEqual(hashlib.sha256(data).hexdigest(), APT.KEY_SHA256)
        self.assertIn(b'BEGIN PGP PUBLIC KEY BLOCK', data)
        self.assertNotIn(b'PRIVATE KEY', data)

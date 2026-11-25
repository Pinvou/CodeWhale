"""No-network checks for retained and newly downloaded acceptance weights."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / 'scripts/acceptance/download_model.py'


class AcceptanceDownloadModelTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='cmsd-download-check-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.script = self.root / 'download_model.py'
        shutil.copyfile(SCRIPT, self.script)
        self.weights = self.root / 'models/qa-smollm2'
        self.weights.mkdir(parents=True)
        self.path = self.weights / 'model.safetensors'
        self.partial = self.weights / 'model.safetensors.part'
        self.expected = b'fixed public model content'
        self.item = {'rfilename': self.path.name, 'size': len(self.expected),
                     'lfs': {'sha256': hashlib.sha256(self.expected).hexdigest()}}
        self.download = self.root / 'curl-payload'
        self.marker = self.root / 'curl-called'
        tools = self.root / 'bin'
        tools.mkdir()
        curl = tools / 'curl'
        curl.write_text('#!/usr/bin/env python3\n'
                        'import os, pathlib, shutil, sys\n'
                        "pathlib.Path(os.environ['CMSD_TEST_CURL_CALLED']).write_text('called')\n"
                        "shutil.copyfile(os.environ['CMSD_TEST_CURL_PAYLOAD'], sys.argv[sys.argv.index('--output') + 1])\n")
        curl.chmod(0o700)
        self.environment = dict(os.environ, LMM_MODELS_DIR=str(self.root / 'models'),
                                PATH=str(tools) + os.pathsep + os.environ.get('PATH', ''),
                                CMSD_TEST_CURL_PAYLOAD=str(self.download),
                                CMSD_TEST_CURL_CALLED=str(self.marker))

    def run_script(self):
        source = {'repo': 'fixture/no-network', 'revision': 'fixed', 'files': [self.item]}
        (self.root / 'source.json').write_text(json.dumps(source))
        return subprocess.run([sys.executable, '-B', str(self.script)],
                              env=self.environment, stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, text=True, timeout=5)

    def assert_verified(self, result):
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn('Manifest sizes and declared LFS SHA-256 verified;', result.stdout)

    def assert_failed(self, result, message):
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn(message, result.stdout)
        self.assertNotIn('Manifest sizes and declared LFS SHA-256 verified;', result.stdout)

    def test_existing_correct_file_is_verified_without_download(self):
        self.path.write_bytes(self.expected)
        self.assert_verified(self.run_script())
        self.assertEqual(self.path.read_bytes(), self.expected)
        self.assertFalse(self.marker.exists())

    def test_existing_wrong_size_is_preserved_and_rejected(self):
        self.path.write_bytes(b'bad')
        self.assert_failed(self.run_script(), 'download size mismatch')
        self.assertEqual(self.path.read_bytes(), b'bad')
        self.assertFalse(self.marker.exists())

    def test_existing_same_size_wrong_digest_is_preserved_and_rejected(self):
        bad = b'x' * len(self.expected)
        self.path.write_bytes(bad)
        self.assert_failed(self.run_script(), 'download digest mismatch')
        self.assertEqual(self.path.read_bytes(), bad)
        self.assertFalse(self.marker.exists())

    def test_existing_without_lfs_still_requires_size(self):
        self.item.pop('lfs')
        self.path.write_bytes(b'x' * len(self.expected))
        self.assert_verified(self.run_script())
        self.path.write_bytes(b'bad')
        self.assert_failed(self.run_script(), 'download size mismatch')
        self.assertEqual(self.path.read_bytes(), b'bad')
        self.assertFalse(self.marker.exists())

    def test_new_correct_partial_is_verified_before_publication(self):
        self.download.write_bytes(self.expected)
        self.assert_verified(self.run_script())
        self.assertTrue(self.marker.exists())
        self.assertEqual(self.path.read_bytes(), self.expected)
        self.assertFalse(self.partial.exists())

    def test_new_wrong_size_partial_is_retained_without_publication(self):
        self.download.write_bytes(b'bad')
        self.assert_failed(self.run_script(), 'download size mismatch')
        self.assertTrue(self.marker.exists())
        self.assertFalse(self.path.exists())
        self.assertEqual(self.partial.read_bytes(), b'bad')

    def test_new_same_size_wrong_digest_partial_is_retained(self):
        bad = b'x' * len(self.expected)
        self.download.write_bytes(bad)
        self.assert_failed(self.run_script(), 'download digest mismatch')
        self.assertTrue(self.marker.exists())
        self.assertFalse(self.path.exists())
        self.assertEqual(self.partial.read_bytes(), bad)

    def test_new_without_lfs_requires_size_before_publication(self):
        self.item.pop('lfs')
        self.download.write_bytes(b'x' * len(self.expected))
        self.assert_verified(self.run_script())
        self.assertEqual(self.path.read_bytes(), self.download.read_bytes())
        self.assertFalse(self.partial.exists())


if __name__ == '__main__':
    unittest.main()

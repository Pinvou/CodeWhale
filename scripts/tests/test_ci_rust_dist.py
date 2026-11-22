import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import threading
import unittest
import urllib.error
import urllib.request

SPEC = importlib.util.spec_from_file_location('ci_rust_dist', Path(__file__).parents[1] / 'ci-rust-dist.py')
DIST = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DIST)


class RustDistributionTests(unittest.TestCase):
    def test_mirror_bytes_are_verified_against_an_independent_official_checksum(self):
        with tempfile.TemporaryDirectory() as directory:
            distribution, name, bodies, calls = self.fixture(directory)
            distribution.mirror = 'rsproxy'
            fetch = distribution.fetch
            def mirrored(url, target, expected=None, **options):
                if url.startswith('https://rsproxy.cn'):
                    self.assertEqual(expected, hashlib.sha256(bodies[name]).hexdigest())
                    self.assertEqual(options['attempts'], 1)
                    self.assertLessEqual(options['seconds'], 30)
                    calls.append(url)
                    url = DIST.ORIGIN + url.removeprefix('https://rsproxy.cn')
                return fetch(url, target, expected, **options)
            distribution.fetch = mirrored
            self.assertEqual(distribution.artifact(name).read_bytes(), bodies[name])
            self.assertEqual(calls[0], DIST.ORIGIN + name + '.sha256')
            self.assertIn('https://rsproxy.cn' + name, calls)

    def test_mirror_failure_falls_back_within_the_original_total_budget(self):
        with tempfile.TemporaryDirectory() as directory:
            distribution, name, bodies, calls = self.fixture(directory)
            distribution.mirror = 'rsproxy'
            fetch = distribution.fetch
            def unavailable(url, target, expected=None, **options):
                if url.startswith('https://rsproxy.cn'):
                    raise DIST.DOWNLOAD.DownloadError('fixture mirror unavailable')
                self.assertGreater(options['seconds'], 0)
                self.assertLessEqual(options['seconds'], 180)
                return fetch(url, target, expected, **options)
            distribution.fetch = unavailable
            self.assertEqual(distribution.artifact(name).read_bytes(), bodies[name])
            self.assertEqual(calls[-1], DIST.ORIGIN + name)

    def fixture(self, root):
        root = Path(root)
        (root / 'snapshot').mkdir()
        package = b'owned-rust-component-fixture'
        name = '/dist/2026-01-01/rustc-1.0.0-fixture.tar.xz'
        bodies = {name: package, name + '.sha256': hashlib.sha256(package).hexdigest().encode() + b'  fixture\n'}
        calls = []
        def transfer(url, target, seconds, tls12, max_bytes):
            self.assertTrue(tls12, 'only the official Rust origin uses its measured compatibility policy')
            calls.append(url)
            body = bodies[url.removeprefix(DIST.ORIGIN)]
            target.write_bytes(body)
            return {'exit': 0, 'http_code': 200}
        def fetch(url, target, expected=None, **options):
            return DIST.DOWNLOAD.fetch(url, target, expected, transfer=transfer, **options)
        return DIST.Distribution(root / 'snapshot', root / 'cache', fetch), name, bodies, calls

    def test_official_checksum_is_independent_and_warm_objects_avoid_repeated_upstream_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            distribution, name, bodies, calls = self.fixture(root)
            first = distribution.artifact(name)
            self.assertEqual(first.read_bytes(), bodies[name])
            self.assertEqual(calls, [DIST.ORIGIN + name + '.sha256', DIST.ORIGIN + name])
            self.assertEqual(distribution.artifact(name), first)
            self.assertEqual(len(calls), 2)
            (root / 'second-job').mkdir()
            second = DIST.Distribution(root / 'second-job', root / 'cache', distribution.fetch)
            self.assertEqual(second.artifact(name).read_bytes(), bodies[name])
            self.assertEqual(calls[-1], DIST.ORIGIN + name + '.sha256')
            self.assertEqual(len(calls), 3, 'the new job authenticates its checksum but reuses verified object bytes')

    def test_a_new_job_refreshes_mutable_metadata_while_one_job_keeps_a_consistent_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            distribution, _, bodies, calls = self.fixture(root)
            name = '/dist/channel-rust-stable.toml'
            bodies[name] = b'manifest_version="2"\n'
            bodies[name + '.sha256'] = hashlib.sha256(bodies[name]).hexdigest().encode()
            first = distribution.artifact(name).read_bytes()
            bodies[name] = b'manifest_version="2"\ndate="2026-01-02"\n'
            bodies[name + '.sha256'] = hashlib.sha256(bodies[name]).hexdigest().encode()
            self.assertEqual(distribution.artifact(name).read_bytes(), first)
            (root / 'second-job').mkdir()
            second = DIST.Distribution(root / 'second-job', root / 'cache', distribution.fetch)
            self.assertEqual(second.artifact(name).read_bytes(), bodies[name])

    def test_checksum_mismatch_never_publishes_a_component(self):
        with tempfile.TemporaryDirectory() as directory:
            distribution, name, bodies, _ = self.fixture(directory)
            bodies[name] = b'wrong-component'
            with self.assertRaises(DIST.DOWNLOAD.DownloadError):
                distribution.artifact(name)
            self.assertFalse(any(p.name == hashlib.sha256(bodies[name]).hexdigest()
                                 for p in distribution.cache.iterdir()))

    def test_invalid_requests_do_not_fetch_or_become_a_general_proxy(self):
        with tempfile.TemporaryDirectory() as directory:
            distribution, _, _, calls = self.fixture(directory)
            for name in ('https://example.invalid/dist/a.tar.xz', '/dist/../private.tar.xz',
                         '/dist/%2e%2e/private.tar.xz', '/dist/a.tar.xz?token=x',
                         '/dist//a.tar.xz', '/dist/a.exe', '/not-dist/a.tar.xz'):
                with self.subTest(name=name), self.assertRaises(ValueError):
                    distribution.artifact(name)
            self.assertEqual(calls, [])

    def test_malformed_checksum_is_rejected_before_component_download(self):
        with tempfile.TemporaryDirectory() as directory:
            distribution, name, bodies, calls = self.fixture(directory)
            bodies[name + '.sha256'] = b'<html>intercepted</html>'
            with self.assertRaises(ValueError):
                distribution.artifact(name)
            self.assertEqual(calls, [DIST.ORIGIN + name + '.sha256'])

    def test_snapshot_budget_bounds_temporary_component_copies_too(self):
        with tempfile.TemporaryDirectory() as directory:
            distribution, name, _, _ = self.fixture(directory)
            distribution.capacity = 80  # The valid checksum consumes 74 bytes.
            with self.assertRaises(DIST.DOWNLOAD.DownloadError):
                distribution.artifact(name)
            self.assertLessEqual(sum(p.stat().st_size for p in distribution.root.iterdir()), 80)

    def test_loopback_http_serves_exact_verified_bytes_and_checksum(self):
        with tempfile.TemporaryDirectory() as directory:
            distribution, name, bodies, calls = self.fixture(directory)
            server = DIST.Server(('127.0.0.1', 0), distribution)
            thread = threading.Thread(target=server.serve_forever)
            thread.start()
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            try:
                base = 'http://127.0.0.1:' + str(server.server_address[1])
                for suffix in ('.sha256', ''):
                    with opener.open(base + name + suffix, timeout=3) as response:
                        self.assertEqual(response.read(), bodies[name + suffix])
            finally:
                server.shutdown()
                server.server_close()
                thread.join(3)

    def test_owned_listener_starts_atomically_and_can_be_retired_without_any_origin_request(self):
        with tempfile.TemporaryDirectory() as directory:
            url, state, process = DIST.start(Path(directory) / 'cache', directory)
            try:
                self.assertTrue(url.startswith('http://127.0.0.1:'))
                self.assertEqual(json.loads(state.read_text())['pid'], process.pid)
                self.assertFalse((Path(directory) / 'cache').exists())
            finally:
                process.terminate()
                process.wait(timeout=5)


if __name__ == '__main__':
    unittest.main()

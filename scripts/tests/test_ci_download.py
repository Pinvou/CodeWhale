import contextlib
import hashlib
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location('ci_download', Path(__file__).parents[1] / 'ci-download.py')
DOWNLOAD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DOWNLOAD)
DATA = b'fixture archive bytes'
SHA = hashlib.sha256(DATA).hexdigest()
URL = 'https://downloads.example.invalid/archive?sig=must-not-appear'


class DownloadTests(unittest.TestCase):
    def test_windows_service_path_resolves_git_bundled_curl_without_shell_alias_or_path_mutation(self):
        with tempfile.TemporaryDirectory(prefix='installed Git ') as directory:
            root = Path(directory)
            git = root / 'cmd/git.exe'; git.parent.mkdir(); git.write_bytes(b'installed git')
            curl = root / 'ucrt64/bin/curl.exe'; curl.parent.mkdir(parents=True)
            curl.write_bytes(b'installed curl')
            environment = {'PATH':'retained-business-path'}
            resolved = DOWNLOAD.curl_executable(platform='nt',
                which=lambda command: str(git) if command == 'git' else None, environment=environment)
            self.assertEqual(Path(resolved), curl)
            self.assertEqual(environment, {'PATH':'retained-business-path'})
            self.assertEqual(DOWNLOAD.curl_executable(platform='nt',
                which=lambda command: 'preferred-curl' if command == 'curl' else self.fail('unneeded Git probe'),
                environment=environment), 'preferred-curl')
            curl.unlink()
            with self.assertRaises(DOWNLOAD.DownloadError):
                DOWNLOAD.curl_executable(platform='nt',
                    which=lambda command: str(git) if command == 'git' else None, environment={})

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.disk = patch.object(DOWNLOAD.shutil, 'disk_usage', return_value=SimpleNamespace(free=10 * 1024**3))
        self.disk.start()
        self.addCleanup(self.disk.stop)
        self.output = io.StringIO()
        self.capture = contextlib.redirect_stdout(self.output)
        self.capture.__enter__()
        self.addCleanup(self.capture.__exit__, None, None, None)

    @staticmethod
    def successful(url, target, seconds, tls12, limit):
        target.write_bytes(DATA)
        return {'exit': 0, 'http_code': 200}

    def fetch(self, **options):
        options.setdefault('transfer', self.successful)
        options.setdefault('cache', self.root / 'cache')
        DOWNLOAD.fetch(URL, self.root / 'result', SHA, **options)

    def test_verified_cache_hit_never_calls_network(self):
        self.fetch()
        self.fetch(transfer=lambda *a: self.fail('cache hit performed a download'))
        self.assertEqual((self.root / 'result').read_bytes(), DATA)
        self.assertIn('cache-hit', self.output.getvalue())
        self.assertNotIn('sig=', self.output.getvalue())
        self.assertNotIn('must-not-appear', self.output.getvalue())

    def test_corrupted_cache_is_not_consumed(self):
        self.fetch()
        (self.root / 'cache' / SHA).write_bytes(b'corrupted')
        self.fetch()
        self.assertEqual((self.root / 'cache' / SHA).read_bytes(), DATA)
        self.assertIn('cache-rejected', self.output.getvalue())

    def test_checksum_failure_never_replaces_existing_output_or_publishes(self):
        (self.root / 'result').write_bytes(b'previous output')
        def wrong(url, target, *args):
            target.write_bytes(b'wrong')
            return {'exit': 0}
        with self.assertRaisesRegex(DOWNLOAD.DownloadError, 'SHA-256 mismatch'):
            self.fetch(transfer=wrong)
        self.assertEqual((self.root / 'result').read_bytes(), b'previous output')
        self.assertFalse((self.root / 'cache' / SHA).exists())
        self.assertFalse(list(self.root.glob('.ci-download-*')))

    def test_mutable_metadata_never_enters_content_cache(self):
        DOWNLOAD.fetch(URL, self.root / 'metadata', cache=self.root / 'cache', transfer=self.successful)
        self.assertFalse((self.root / 'cache').exists())

    def test_http_504_retries_inside_one_remaining_budget(self):
        budgets = []
        def transfer(url, target, seconds, *args):
            budgets.append(seconds)
            if len(budgets) == 1:
                return {'exit': 22, 'http_code': 504}
            target.write_bytes(DATA)
            return {'exit': 0, 'http_code': 200}
        self.fetch(transfer=transfer, sleep=lambda _: None, seconds=5)
        self.assertEqual(len(budgets), 2)
        self.assertTrue(0 < budgets[1] <= budgets[0] <= 5)

    def test_connect_gateway_failure_retries_with_origin_000_inside_original_budget(self):
        for code in (22, 56):
            with self.subTest(curl=code):
                budgets = []
                def transfer(url, target, seconds, tls12, limit):
                    budgets.append((seconds, tls12))
                    if len(budgets) == 1:
                        return {'exit':code, 'http_code':0, 'http_connect':502,
                                'failure_stage':'proxy-connect'}
                    return self.successful(url, target, seconds, tls12, limit)
                # No cross-subcase cache hit may hide the transfer under test.
                self.fetch(transfer=transfer, cache=None, sleep=lambda _: None, seconds=5)
                self.assertEqual(len(budgets), 2)
                self.assertTrue(0 < budgets[1][0] <= budgets[0][0] <= 5)
                self.assertEqual([item[1]for item in budgets], [False, False])

    def test_connect_refusal_and_certificate_failures_never_retry(self):
        for code, proxy, origin in ((56,407,0), (22,403,0), (22,404,0),
                                    (56,501,0), (60,502,0), (77,503,0),
                                    (22,200,401), (22,200,404), (22,200,0)):
            with self.subTest(curl=code, proxy=proxy, origin=origin):
                calls=[]
                def denied(*args):
                    calls.append(True)
                    return {'exit':code, 'http_code':origin, 'http_connect':proxy}
                with self.assertRaises(DOWNLOAD.DownloadError):
                    self.fetch(transfer=denied, cache=None, sleep=lambda _: self.fail('terminal failure slept'))
                self.assertEqual(len(calls), 1)

    def test_real_curl_connect_status_is_separate_and_retries_are_bounded(self):
        try:
            DOWNLOAD.curl_executable()
        except DOWNLOAD.DownloadError:
            self.skipTest('native curl is required for CONNECT wire validation')
        for status, expected_calls in ((502,3), (503,3), (407,1)):
            with self.subTest(proxy=status):
                observations=[]
                class Proxy(http.server.BaseHTTPRequestHandler):
                    def log_message(self, *_):
                        pass
                    def do_CONNECT(self):
                        observations.append(self.command)
                        self.send_error(status)
                        self.close_connection=True
                server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Proxy)
                thread=threading.Thread(target=server.serve_forever,daemon=True)
                thread.start()
                proxy=f'http://127.0.0.1:{server.server_port}'
                try:
                    with patch.dict(os.environ, {'HTTPS_PROXY':proxy,'https_proxy':proxy,
                            'HTTP_PROXY':proxy,'http_proxy':proxy,'NO_PROXY':'','no_proxy':''}):
                        with self.assertRaises(DOWNLOAD.DownloadError):
                            self.fetch(cache=None, transfer=DOWNLOAD.curl_transfer,
                                       sleep=lambda _:None, seconds=5)
                finally:
                    server.shutdown();server.server_close();thread.join(timeout=2)
                self.assertEqual(len(observations),expected_calls)
                self.assertFalse((self.root/'result').exists())
                events=[json.loads(line.split(' ',1)[1])for line in self.output.getvalue().splitlines()]
                attempts=[event for event in events if event['phase']=='attempt']
                self.assertEqual(attempts[-1]['http_connect'],status)
                self.assertEqual(attempts[-1]['http_code'],0)
                self.assertEqual(attempts[-1]['failure_stage'],'proxy-connect')
                self.assertNotIn('must-not-appear',self.output.getvalue())
                self.output.seek(0);self.output.truncate()

    def test_certificate_authentication_and_missing_asset_fail_without_retry(self):
        for code, status in ((60, 0), (22, 401), (22, 403), (22, 404)):
            calls = []
            def denied(*args):
                calls.append(True)
                return {'exit': code, 'http_code': status}
            with self.assertRaises(DOWNLOAD.DownloadError):
                self.fetch(transfer=denied)
            self.assertEqual(len(calls), 1)

    def test_tls_fallback_keeps_same_budget_and_only_follows_handshake_failure(self):
        policies = []
        def transfer(url, target, seconds, tls12, limit):
            policies.append(tls12)
            if len(policies) == 1:
                return {'exit': 35, 'time_connect': .1, 'time_appconnect': 0}
            target.write_bytes(DATA)
            return {'exit': 0}
        self.fetch(transfer=transfer, sleep=lambda _: None)
        self.assertEqual(policies, [False, True])

    def test_capacity_and_busy_cache_keep_download_usable_without_eviction(self):
        self.fetch(capacity=1)
        self.assertEqual((self.root / 'result').read_bytes(), DATA)
        self.assertFalse((self.root / 'cache' / SHA).exists())
        with patch.object(DOWNLOAD, 'publication_lock', return_value=None):
            self.fetch()
        self.assertFalse((self.root / 'cache' / SHA).exists())

    def test_later_redirect_handshake_failure_is_not_hidden_by_earlier_tls_timing(self):
        policies = []
        def transfer(url, target, seconds, tls12, limit):
            policies.append(tls12)
            if len(policies) == 1:
                return {'exit': 28, 'time_connect': .1, 'time_appconnect': .2,
                        'num_redirects': 1, 'failure_stage': 'tls-handshake'}
            target.write_bytes(DATA)
            return {'exit': 0}
        self.fetch(transfer=transfer, sleep=lambda _: None)
        self.assertEqual(policies, [False, True])

    def test_redirect_error_classification_does_not_expose_the_error_uri(self):
        with patch.object(DOWNLOAD.subprocess, 'run', return_value=SimpleNamespace(
                returncode=28, stdout=b'{"time_appconnect":"0.2"}',
                stderr=b'curl: (28) SSL connection timeout https://secret.invalid/?credential=private')):
            receipt = DOWNLOAD.curl_transfer(URL, self.root / 'out', 5, False, 1024)
        self.assertEqual(receipt['failure_stage'], 'tls-handshake')
        self.assertNotIn('credential', json.dumps(receipt))

    def test_publication_lock_recovers_after_the_previous_owner_closes(self):
        cache = self.root / 'cache'
        cache.mkdir()
        first = DOWNLOAD.publication_lock(cache)
        self.assertIsNotNone(first)
        first.close()
        second = DOWNLOAD.publication_lock(cache)
        self.assertIsNotNone(second)
        second.close()

    def test_curl_preserves_certificate_checks_and_sanitizes_metrics(self):
        receipt = {'http_code': '000', 'time_connect': '0.2', 'time_appconnect': '0.0'}
        with patch.object(DOWNLOAD.subprocess, 'run', return_value=SimpleNamespace(
                returncode=35, stdout=json.dumps(receipt).encode(), stderr=b'secret URI')) as run:
            result = DOWNLOAD.curl_transfer(URL, self.root / 'out', 5, True, 1024)
        args = run.call_args.args[0]
        self.assertIn('--tlsv1.2', args)
        self.assertIn('--tls-max', args)
        self.assertIn('--disable', args)
        self.assertNotIn('--insecure', args)
        self.assertNotIn('-k', args)
        self.assertEqual(result['http_code'], 0)
        self.assertEqual(result['time_connect'], .2)
        self.assertNotIn('stderr', result)

    def test_rejects_insecure_or_credential_bearing_sources(self):
        for url in ('http://example.invalid/a', 'file:///a', 'https://user:password@example.invalid/a'):
            with self.assertRaises(DOWNLOAD.DownloadError):
                DOWNLOAD.fetch(url, self.root / 'out', transfer=lambda *a: self.fail('network called'))


if __name__ == '__main__':
    unittest.main()

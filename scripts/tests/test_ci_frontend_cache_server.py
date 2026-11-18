"""Real CLI, mTLS and filesystem tests; no deployed cache or external network."""
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
import os
from pathlib import Path
import queue
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
import urllib.parse

ROOT = Path(__file__).resolve().parents[2]
COMMIT = '1' * 40
REPOSITORY = 'fixture/frontend'


class CacheServerTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # The real service's 2 GiB reserve stays enabled. Use the physical
        # workspace, not a small /tmp tmpfs that cannot meet that prerequisite.
        cls.temporary = tempfile.TemporaryDirectory(prefix='.cache-server-test-', dir=ROOT)
        cls.addClassCleanup(cls.temporary.cleanup)
        cls.root = Path(cls.temporary.name)
        openssl = shutil.which('openssl')
        if not openssl and shutil.which('git'):
            candidate = Path(shutil.which('git')).resolve().parents[1] / 'usr/bin/openssl.exe'
            if candidate.is_file():
                openssl = str(candidate)
        if not openssl:
            raise RuntimeError('openssl is required for real cache mTLS integration')

        def command(*args):
            subprocess.run([openssl, *args], cwd=cls.root, check=True,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15)

        command('req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:prime256v1',
                '-nodes', '-keyout', 'ca.key', '-out', 'ca.pem', '-days', '1',
                '-subj', '/CN=cache-test-ca', '-addext', 'basicConstraints=critical,CA:TRUE',
                '-addext', 'keyUsage=critical,keyCertSign,cRLSign')
        command('req', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:prime256v1',
                '-nodes', '-keyout', 'service.key', '-out', 'service.csr', '-subj', '/CN=localhost')
        (cls.root / 'service.ext').write_text(
            'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n'
            'extendedKeyUsage=serverAuth,clientAuth\nsubjectAltName=DNS:localhost,IP:127.0.0.1\n',
            encoding='utf-8')
        command('x509', '-req', '-in', 'service.csr', '-CA', 'ca.pem', '-CAkey', 'ca.key',
                '-set_serial', '2', '-out', 'service.pem', '-days', '1', '-extfile', 'service.ext')

    def setUp(self):
        self.case = self.root / self._testMethodName
        self.case.mkdir()
        self.context = ssl.create_default_context(cafile=str(self.root / 'ca.pem'))
        self.context.load_cert_chain(str(self.root / 'service.pem'), str(self.root / 'service.key'))

    def start_server(self, name, peers=()):
        directory = self.case / name
        directory.mkdir()
        config = {'repository': REPOSITORY, 'root': str(directory / 'cache'),
                  'local_port': 0, 'peer_bind': '127.0.0.1', 'peer_port': 0,
                  'peers': list(peers), 'ca': str(self.root / 'ca.pem'),
                  'certificate': str(self.root / 'service.pem'),
                  'private_key': str(self.root / 'service.key'),
                  'max_bytes': 2 * 1024**2, 'max_entries': 32}
        path = directory / 'config.json'
        path.write_text(json.dumps(config), encoding='utf-8')
        child = subprocess.Popen([sys.executable, str(ROOT / 'scripts/ci-frontend-cache-server.py'),
                                  '--config', str(path)], stdin=subprocess.DEVNULL,
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

        def stop():
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=5)
            child.stdout.close()
            child.stderr.close()

        self.addCleanup(stop)
        ready = queue.Queue()
        reader = threading.Thread(target=lambda: ready.put(child.stdout.readline()), daemon=True)
        reader.start()
        try:
            event = json.loads(ready.get(timeout=15))
        except (queue.Empty, ValueError):
            self.fail('cache CLI did not publish its bound-port readiness event')
        self.assertEqual(event['event'], 'cache-ready')
        reader.join(timeout=1)
        return {'local': f'http://127.0.0.1:{event["local_port"]}',
                'peer': f'https://127.0.0.1:{event["peer_port"]}',
                'root': Path(config['root'])}

    def request(self, url, *, data=None, context=None):
        client = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                             urllib.request.HTTPSHandler(context=context or self.context))
        request = urllib.request.Request(url, data=data, headers={'X-Repository': REPOSITORY},
                                         method='PUT' if data is not None else 'GET')
        try:
            response = client.open(request, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.headers, response.read()

    def test_peer_requires_client_certificate_before_read_or_write(self):
        server = self.start_server('node')
        body = b'authenticated fixture'
        digest = hashlib.sha256(body).hexdigest()
        url = server['peer'] + '/v1/' + COMMIT + '/' + digest
        self.assertEqual(self.request(url, data=body)[0], 200, 'positive mTLS control')
        uncredentialed = ssl.create_default_context(cafile=str(self.root / 'ca.pem'))
        with self.assertRaises((OSError, urllib.error.URLError)):
            self.request(url, context=uncredentialed)
        with self.assertRaises((OSError, urllib.error.URLError)):
            self.request(url, data=body, context=uncredentialed)
        self.assertEqual(self.request(url)[2], body, 'failed handshakes must not break the next authenticated call')

    def test_peer_rejects_wrong_sha_without_publishing_any_object(self):
        server = self.start_server('node')
        status, _, _ = self.request(server['peer'] + '/v1/' + COMMIT + '/' + '0' * 64,
                                    data=b'bytes with a different SHA-256')
        self.assertEqual(status, 400)
        self.assertEqual(list((server['root'] / 'objects').iterdir()), [])
        self.assertEqual(list((server['root'] / 'refs').iterdir()), [])

    def test_incomplete_tls_handshake_does_not_block_authenticated_peer(self):
        server = self.start_server('node')
        address = urllib.parse.urlsplit(server['peer'])
        with socket.create_connection((address.hostname, address.port), timeout=2):
            # Keep one TCP connection open without speaking TLS. The actual
            # authenticated request must complete while that socket is open.
            body = b'legitimate peer while another handshake is incomplete'
            digest = hashlib.sha256(body).hexdigest()
            url = server['peer'] + '/v1/' + COMMIT + '/' + digest
            self.assertEqual(self.request(url, data=body)[0], 200)
            self.assertEqual(self.request(url)[2], body)
        self.assertEqual(self.request(url)[2], body, 'next request remains usable after cancellation')

    def test_local_miss_and_corruption_are_repaired_through_real_mtls_peer(self):
        first = self.start_server('first')
        second = self.start_server('second', [first['peer']])
        body = b'immutable replica fixture'
        digest = hashlib.sha256(body).hexdigest()
        suffix = '/v1/' + COMMIT + '/' + digest
        self.assertEqual(self.request(first['peer'] + suffix, data=body)[0], 200)
        for corruption in [False, True]:
            if corruption:
                (second['root'] / 'objects' / digest).write_bytes(b'corrupted storage')
            status, headers, received = self.request(second['local'] + suffix)
            self.assertEqual(status, 200)
            self.assertEqual(headers['X-Cache-Source'], 'peer-fill')
            self.assertEqual(headers['X-Tested-Commit'], COMMIT)
            self.assertEqual(headers['X-Content-SHA256'], digest)
            self.assertEqual(received, body)
            self.assertEqual(self.request(second['local'] + suffix)[1]['X-Cache-Source'], 'local-hit')

    def test_loopback_publication_replicates_verified_bytes_over_real_mtls(self):
        first = self.start_server('first')
        second = self.start_server('second', [first['peer']])
        body = b'file-backed peer replication from a real local publication'
        digest = hashlib.sha256(body).hexdigest()
        suffix = '/v1/' + COMMIT + '/' + digest
        status, _, receipt = self.request(second['local'] + suffix, data=body)
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(receipt), {'stored': True, 'replicas': 2, 'sha256': digest, 'commit': COMMIT})
        for node in [first, second]:
            status, headers, received = self.request(node['local'] + suffix)
            self.assertEqual((status, headers['X-Cache-Source'], received), (200, 'local-hit', body))

    @unittest.skipIf(os.name == 'nt', 'Linux service socket watchdog must run on its target OS')
    def test_peer_incomplete_headers_are_interrupted_without_publishing_then_recovers(self):
        body = b'healthy peer after an incomplete chunk header'
        digest = hashlib.sha256(body).hexdigest()
        release = threading.Event()
        calls = []
        class Peer(BaseHTTPRequestHandler):
            def do_GET(self):
                calls.append(self.path)
                if len(calls) == 1:
                    self.connection.sendall(b'HTTP/1.1 200 OK\r\nContent-Len')
                    release.wait(5)
                else:
                    self.send_response(200)
                    self.send_header('Content-Length', str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
            def log_message(self, *args):
                pass
        peer = ThreadingHTTPServer(('127.0.0.1', 0), Peer)
        peer.daemon_threads = True
        context = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH, cafile=str(self.root / 'ca.pem'))
        context.load_cert_chain(str(self.root / 'service.pem'), str(self.root / 'service.key'))
        context.verify_mode = ssl.CERT_REQUIRED
        peer.socket = context.wrap_socket(peer.socket, server_side=True)
        thread = threading.Thread(target=peer.serve_forever, daemon=True)
        thread.start()
        spec = importlib.util.spec_from_file_location('cache_server_slow_peer', ROOT / 'scripts/ci-frontend-cache-server.py')
        server = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(server)
        config = {'repository': REPOSITORY, 'root': str(self.case / 'cache'),
                  'peers': [f'https://127.0.0.1:{peer.server_port}'],
                  'ca': str(self.root / 'ca.pem'), 'certificate': str(self.root / 'service.pem'),
                  'private_key': str(self.root / 'service.key')}
        app = server.App(config)
        started = time.monotonic()
        try:
            self.assertFalse(app.fill(COMMIT, digest, deadline=time.monotonic() + 0.3))
            self.assertLess(time.monotonic() - started, 1.5)
            self.assertEqual(list(app.store.objects.iterdir()), [])
            self.assertTrue(app.fill(COMMIT, digest, deadline=time.monotonic() + 3))
            opened, _ = app.store.open(COMMIT, digest)
            with opened:
                self.assertEqual(opened.read(), body)
        finally:
            release.set()
            peer.shutdown()
            peer.server_close()
            thread.join(timeout=3)


if __name__ == '__main__':
    unittest.main()

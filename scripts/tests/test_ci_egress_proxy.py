import contextlib
import errno
import importlib.util
import io
import json
from pathlib import Path
import socket
import threading
import time
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location('ci_proxy', Path(__file__).parents[1] / 'ci-egress-proxy.py')
PROXY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROXY)


def read_all(connection):
    result = bytearray()
    while True:
        block = connection.recv(65536)
        if not block:
            return bytes(result)
        result.extend(block)


class ProxyTests(unittest.TestCase):
    def test_capacity_uses_soft_descriptor_budget_including_selector_and_header_reserve(self):
        self.assertEqual(PROXY.tunnel_capacity(1024, 1024), 240)
        self.assertEqual(PROXY.tunnel_capacity(1024, 8192), 1024)
        self.assertEqual(PROXY.tunnel_capacity(20, 8192), 20)
        with self.assertRaises(ValueError):
            PROXY.tunnel_capacity(1024, 64)

    def test_local_descriptor_exhaustion_is_preserved_without_trying_every_remote_address(self):
        with mock.patch.object(PROXY.socket, 'create_connection', side_effect=OSError(errno.EMFILE, 'private diagnostic')) as connect:
            with self.assertRaises(PROXY.UpstreamError) as raised:
                PROXY.connect_target('github.com', lambda _: ['140.82.113.3', '140.82.113.4'])
        self.assertEqual(connect.call_count, 1)
        self.assertEqual(raised.exception.errno, errno.EMFILE)
        self.assertEqual(raised.exception.stage, 'descriptor-capacity')
        self.assertNotIn('private diagnostic', str(raised.exception))

    def test_admission_covers_slow_headers_and_releases_capacity_when_client_disconnects(self):
        proxy = PROXY.Server(('127.0.0.1', 0), max_tunnels=1)
        thread = threading.Thread(target=proxy.serve_forever)
        thread.start()
        first = socket.create_connection(proxy.server_address, timeout=3)
        try:
            deadline = time.monotonic() + 3
            while proxy.slots.acquire(blocking=False):
                proxy.slots.release()
                self.assertLess(time.monotonic(), deadline)
                time.sleep(.01)
            with socket.create_connection(proxy.server_address, timeout=3) as second:
                second.sendall(b'GET /health HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n')
                headers = b''
                while b'\r\n\r\n' not in headers:
                    headers += second.recv(1)
                self.assertIn(b'503 Proxy capacity reached', headers)
            first.close()
            deadline = time.monotonic() + 3
            while not proxy.slots.acquire(blocking=False):
                self.assertLess(time.monotonic(), deadline)
                time.sleep(.01)
            proxy.slots.release()
            with socket.create_connection(proxy.server_address, timeout=3) as third:
                third.sendall(b'GET /health HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n')
                self.assertTrue(read_all(third).endswith(b'ok\n'))
        finally:
            first.close()
            proxy.shutdown()
            proxy.server_close()
            thread.join(5)

    def test_certificate_services_cover_observed_chain_without_becoming_an_open_proxy(self):
        for host in ('crl.globalsign.com', 'ocsp.globalsign.com', 'ocsp2.globalsign.com', 'secure.globalsign.com'):
            self.assertEqual(PROXY.pki_target('http://' + host + '/signed-object').hostname, host)
        for url in ('http://internal.invalid/a', 'http://127.0.0.1/a',
                    'http://user:password@crl.globalsign.com/a', 'http://crl.globalsign.com:443/a',
                    'https://crl.globalsign.com:80/a', 'http://crl.globalsign.com/a#fragment'):
            with self.assertRaises(ValueError):
                PROXY.pki_target(url)

    def test_pki_redirect_is_rejected_before_an_unapproved_fetch(self):
        handler = PROXY.PKIRedirect()
        request = PROXY.urllib.request.Request('http://crl.globalsign.com/signed.crl')
        with self.assertRaises(ValueError):
            handler.redirect_request(request, None, 302, 'redirect', {}, 'http://127.0.0.1/private')

    def test_half_closed_request_still_receives_complete_large_response(self):
        client, left = socket.socketpair()
        right, backend = socket.socketpair()
        sockets = [client, left, right, backend]
        for connection in sockets:
            connection.settimeout(5)
        result = {}
        request, response = b'q' * 300000, b'r' * 700000
        def forward():
            try:
                result['receipt'] = PROXY.relay(left, right, idle=5, buffer_limit=16384)
            finally:
                left.close()
                right.close()
        def serve():
            try:
                result['request'] = read_all(backend)
                backend.sendall(response)
                backend.shutdown(socket.SHUT_WR)
            finally:
                backend.close()
        relay_thread = threading.Thread(target=forward)
        server_thread = threading.Thread(target=serve)
        try:
            relay_thread.start()
            server_thread.start()
            client.sendall(request)
            client.shutdown(socket.SHUT_WR)
            self.assertEqual(read_all(client), response)
            relay_thread.join(5)
            server_thread.join(5)
            self.assertFalse(relay_thread.is_alive())
            self.assertFalse(server_thread.is_alive())
            self.assertEqual(result['request'], request)
            self.assertEqual(result['receipt'], {'client_bytes': len(request), 'upstream_bytes': len(response)})
        finally:
            for connection in sockets:
                connection.close()
            relay_thread.join(5)
            server_thread.join(5)

    def test_optimistic_client_bytes_are_not_lost_in_http_header_buffer(self):
        listener = socket.socket()
        listener.bind(('127.0.0.1', 0))
        listener.listen()
        def serve():
            connection, _ = listener.accept()
            with connection:
                connection.settimeout(5)
                data = connection.recv(65536)
                connection.sendall(data)
        backend = threading.Thread(target=serve)
        proxy = PROXY.Server(('127.0.0.1', 0), connector=lambda *_: socket.create_connection(listener.getsockname(), timeout=5))
        thread = threading.Thread(target=proxy.serve_forever)
        with contextlib.redirect_stdout(io.StringIO()):
            try:
                backend.start()
                thread.start()
                with socket.create_connection(proxy.server_address, timeout=5) as client:
                    client.sendall(b'CONNECT fixture.invalid:443 HTTP/1.1\r\nHost: fixture.invalid:443\r\n\r\nCLIENT_HELLO')
                    data = b''
                    while b'\r\n\r\n' not in data:
                        data += client.recv(1)
                    self.assertIn(b'200 Connection Established', data)
                    self.assertEqual(client.recv(65536), b'CLIENT_HELLO')
            finally:
                proxy.shutdown()
                proxy.server_close()
                listener.close()
                thread.join(5)
                backend.join(5)

    def test_concurrent_dns_queries_share_one_verified_lookup(self):
        class Response:
            def __enter__(self):
                return self
            def __exit__(self, *args):
                pass
            def read(self, limit):
                return json.dumps({'Status': 0, 'Answer': [{'type': 1, 'data': '140.82.113.3', 'TTL': 60}]}).encode()
        class Opener:
            calls = 0
            def open(self, *args, **kwargs):
                self.calls += 1
                time.sleep(.05)
                return Response()
        resolver = PROXY.Resolver()
        resolver.direct = Opener()
        values = []
        threads = [threading.Thread(target=lambda: values.append(resolver('github.com'))) for _ in range(20)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(5)
        self.assertEqual(values, [['140.82.113.3']] * 20)
        self.assertEqual(resolver.direct.calls, 1)

    def test_log_labels_do_not_expose_private_hosts_or_request_uris(self):
        self.assertEqual(PROXY.label('internal-service.invalid'), 'other')
        self.assertEqual(PROXY.label('10.0.0.1'), 'other')
        self.assertEqual(PROXY.label('productionresultssa16.blob.core.windows.net'), 'productionresultssa16.blob.core.windows.net')


if __name__ == '__main__':
    unittest.main()

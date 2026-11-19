"""Linux-target slow-stream tests, including HTTP chunk parsing and recovery."""
import hashlib
import http.client
import io
import os
from pathlib import Path
import socket
import sys
import tempfile
import time
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'lib'))
from ci_frontend_cache import Store
from ci_transfer_deadline import Deadline, DeadlineExceeded, command_deadline


@unittest.skipIf(os.name == 'nt', 'network interruption contract is exercised on Linux CI')
class TransferDeadlineTests(unittest.TestCase):
    def test_continuously_incomplete_http_chunk_header_is_interrupted_and_next_call_works(self):
        reader, writer = socket.socketpair()
        self.addCleanup(reader.close)
        self.addCleanup(writer.close)
        reader.settimeout(2)
        writer.sendall(b'HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4')
        response = http.client.HTTPResponse(reader)
        response.begin()
        started = time.monotonic()
        try:
            with self.assertRaises(DeadlineExceeded), command_deadline(time.monotonic() + 0.2):
                response.read(128 * 1024)
        finally:
            response.close()
        self.assertLess(time.monotonic() - started, 1.5)
        next_reader, next_writer = socket.socketpair()
        with next_reader, next_writer:
            next_writer.sendall(b'HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabc')
            with command_deadline(time.monotonic() + 1):
                with http.client.HTTPResponse(next_reader) as response:
                    response.begin()
                    self.assertEqual(response.read(), b'abc')

    def test_owned_socket_upload_expiry_does_not_publish_or_leak_reservation(self):
        with tempfile.TemporaryDirectory() as directory:
            store = Store(directory, reserve=0)
            body = b'abcd'
            digest = hashlib.sha256(body).hexdigest()
            reader, writer = socket.socketpair()
            self.addCleanup(reader.close)
            self.addCleanup(writer.close)
            reader.settimeout(2)
            writer.sendall(body[:1])
            started = time.monotonic()
            with reader.makefile('rb') as stream, Deadline(0.2) as budget:
                budget.watch(reader)
                with self.assertRaises((OSError, ValueError)):
                    store.put('1' * 40, digest, stream, len(body), deadline=budget.end)
            self.assertLess(time.monotonic() - started, 1.5)
            self.assertEqual(list(store.objects.iterdir()), [])
            self.assertEqual(list(store.temporary.iterdir()), [])
            self.assertEqual(store.pending_bytes, 0)
            store.put('1' * 40, digest, io.BytesIO(body), len(body))
            opened, _ = store.open('1' * 40, digest)
            with opened:
                self.assertEqual(opened.read(), body)


if __name__ == '__main__':
    unittest.main()

"""Acceptance-driver transport checks only; not daemon/model acceptance."""
import socket
import threading
import time
import unittest
from real_http_line import response_line


class RealHttpLineTests(unittest.TestCase):
    def test_actual_stream_fragments_form_one_complete_status(self):
        reader, writer = socket.socketpair()
        def send():
            with writer:
                writer.sendall(b'HTTP/1.1 '); time.sleep(.02); writer.sendall(b'400 Bad Request\r\nHeader: value\r\n')
        thread = threading.Thread(target=send); thread.start()
        with reader:
            reader.settimeout(.5)
            self.assertEqual(response_line(reader), b'HTTP/1.1 400 Bad Request')
            self.assertEqual(reader.gettimeout(), .5)
        thread.join(timeout=1)

    def test_eof_after_prefix_does_not_pass_status_assertion(self):
        reader, writer = socket.socketpair()
        with writer: writer.sendall(b'HTTP/1.1 400')
        with reader, self.assertRaises(EOFError): response_line(reader)

    def test_oversize_and_silent_peer_are_bounded(self):
        reader, writer = socket.socketpair()
        with writer: writer.sendall(b'x' * 8192)
        with reader, self.assertRaises(ValueError): response_line(reader)
        reader, writer = socket.socketpair()
        with reader, writer, self.assertRaises(TimeoutError): response_line(reader, timeout=.03)

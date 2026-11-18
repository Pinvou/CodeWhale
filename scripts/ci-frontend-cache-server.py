#!/usr/bin/env python3
"""Host-local frontend cache with mutually authenticated HTTPS peer replication."""
import argparse
import concurrent.futures
from contextlib import contextmanager
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import socket
import ssl
import sys
import threading
import time
import urllib.error
import urllib.request
import urllib.parse

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
from ci_frontend_cache import Store, identity  # noqa: E402
from ci_transfer_deadline import Deadline, DeadlineExceeded  # noqa: E402


class PeerConnection(http.client.HTTPSConnection):
    def __init__(self, host, port, context, budget, addresses):
        self.budget = budget
        super().__init__(host, port, context=context, timeout=budget.remaining(20))
        self._create_connection = lambda *args: self.connect_address(addresses)

    def connect_address(self, addresses):
        last_error = None
        for family, kind, protocol, _, address in addresses:
            self.budget.check()
            connection = socket.socket(family, kind, protocol)
            try:
                self.budget.watch(connection)
                connection.settimeout(self.budget.remaining(20))
                connection.connect(address)
                self.budget.check()
                return connection
            except OSError as error:
                last_error = error
                connection.close()
        raise last_error or OSError('no resolved peer address')

    def connect(self):
        # Direct peer connections never use a proxy/tunnel. Own both the raw
        # and TLS socket before entering a potentially slow handshake/parser.
        http.client.HTTPConnection.connect(self)
        self.budget.watch(self.sock)
        self.sock = self._context.wrap_socket(self.sock, server_hostname=self.host,
                                              do_handshake_on_connect=False)
        self.budget.watch(self.sock)
        self.sock.settimeout(self.budget.remaining(20))
        self.sock.do_handshake()
        self.budget.check()


class Server(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, handler, app, peer=False, tls_context=None):
        self.app, self.peer = app, peer
        self.tls_context = tls_context
        self.slots = threading.BoundedSemaphore(16)
        super().__init__(address, handler)

    def process_request(self, request, address):
        if not self.slots.acquire(blocking=False):
            request.close()
            return
        request.settimeout(45)
        try:
            super().process_request(request, address)
        except Exception:
            self.slots.release()
            raise

    def process_request_thread(self, request, address):
        connection = request
        try:
            # TLS belongs to an admitted worker, not the serial accept loop.
            # Unauthenticated connections share the same bounded slot budget.
            if self.tls_context is not None:
                connection = self.tls_context.wrap_socket(request, server_side=True,
                                                          do_handshake_on_connect=False)
                connection.settimeout(5)
                connection.do_handshake()
                connection.settimeout(45)
            self.finish_request(connection, address)
        except Exception:
            self.handle_error(connection, address)
        finally:
            try:
                self.shutdown_request(connection)
            finally:
                self.slots.release()

    def handle_error(self, request, client_address):
        # Never print peer URLs, headers or request payloads.
        print(json.dumps({"event": "request_error"}), flush=True)


class App:
    def __init__(self, config):
        self.repository = config["repository"]
        self.store = Store(config["root"], max_bytes=config.get("max_bytes", 20 * 1024**3),
                           max_entries=config.get("max_entries", 1000))
        self.peers = config.get("peers", [])
        self.peer_addresses = {}
        # Resolve operator-owned peer names once, before serving requests.
        # Runtime deadlines must not be stranded in an uninterruptible resolver.
        for peer in self.peers:
            parsed = self.peer_url(peer)
            self.peer_addresses[peer] = socket.getaddrinfo(parsed.hostname, parsed.port or 443,
                                                         type=socket.SOCK_STREAM)
        self.context = ssl.create_default_context(cafile=config["ca"])
        self.context.load_cert_chain(config["certificate"], config["private_key"])

    @staticmethod
    def peer_url(url):
        parsed = urllib.parse.urlsplit(url)
        if (parsed.scheme != 'https' or not parsed.hostname or parsed.username or parsed.password
                or parsed.path not in ('', '/') or parsed.query or parsed.fragment):
            raise ValueError('peers require HTTPS origins')
        return parsed

    @contextmanager
    def peer_request(self, url, method, commit, digest, stream=None, size=None, *, deadline=None):
        parsed = self.peer_url(url)
        headers = {"X-Repository": self.repository}
        if size is not None:
            headers["Content-Length"] = str(size)
        end = time.monotonic() + 20
        with Deadline(end=min(end, deadline) if deadline is not None else end) as budget:
            connection = PeerConnection(parsed.hostname, parsed.port, self.context, budget, self.peer_addresses[url])
            try:
                connection.request(method, '/v1/' + commit + '/' + digest, body=stream, headers=headers)
                with connection.getresponse() as response:
                    budget.check()
                    if response.status != 200:
                        raise ValueError('peer rejected bundle request')
                    response.transfer_deadline = budget.end
                    yield response
                budget.check()
            finally:
                connection.close()

    def fill(self, commit, digest, *, deadline=None):
        for peer in self.peers:
            try:
                with self.peer_request(peer, "GET", commit, digest, deadline=deadline) as response:
                    length = response.headers.get('Content-Length')
                    if length is None:
                        raise ValueError('peer response requires a fixed size')
                    size = int(length)
                    self.store.put(commit, digest, response, size, deadline=response.transfer_deadline)
                return True
            except (OSError, ValueError, http.client.HTTPException, DeadlineExceeded) as error:
                if isinstance(error, urllib.error.HTTPError):
                    error.close()
                continue
        return False

    def replicate(self, commit, digest, *, deadline=None):
        def push(peer):
            opened = self.store.open(commit, digest)
            if opened is None:
                return False
            stream, size = opened
            try:
                with stream, self.peer_request(peer, "PUT", commit, digest, stream, size, deadline=deadline) as response:
                    return response.status == 200
            except (OSError, ValueError, http.client.HTTPException, DeadlineExceeded) as error:
                if isinstance(error, urllib.error.HTTPError):
                    error.close()
                return False
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            return 1 + sum(pool.map(push, self.peers))


class Handler(BaseHTTPRequestHandler):
    def setup(self):
        super().setup()
        self.budget = Deadline(90)
        self.budget.watch(self.connection)
        self.budget.__enter__()

    def finish(self):
        try:
            super().finish()
        finally:
            self.budget.__exit__(None, None, None)

    def log_message(self, *args):
        pass

    def reply(self, status, value):
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(data)

    def bundle_identity(self):
        if self.headers.get("X-Repository") != self.server.app.repository:
            raise ValueError("repository mismatch")
        parts = self.path.split("/")
        if len(parts) != 4 or parts[1] != "v1":
            raise ValueError("invalid bundle path")
        identity(parts[2], parts[3])
        return parts[2], parts[3]

    def do_GET(self):
        if self.path == "/health":
            self.reply(200, {"ready": True, "schema": 1})
            return
        try:
            commit, digest = self.bundle_identity()
            opened = self.server.app.store.open(commit, digest)
            source = "local-hit"
            if opened is None and not self.server.peer and self.server.app.fill(commit, digest, deadline=self.budget.end):
                opened = self.server.app.store.open(commit, digest)
                source = "peer-fill"
            if opened is None:
                self.reply(404, {"error": "bundle_not_found"})
                return
            stream, size = opened
            with stream:
                self.send_response(200)
                self.send_header("Content-Length", str(size))
                self.send_header("X-Cache-Source", source)
                self.send_header("X-Tested-Commit", commit)
                self.send_header("X-Content-SHA256", digest)
                self.end_headers()
                for block in iter(lambda: stream.read(256 * 1024), b""):
                    self.wfile.write(block)
        except (OSError, ValueError, KeyError, DeadlineExceeded):
            self.reply(400, {"error": "invalid_bundle_request"})

    def do_PUT(self):
        try:
            commit, digest = self.bundle_identity()
            if self.headers.get("Transfer-Encoding"):
                raise ValueError("chunked uploads are not accepted")
            size = int(self.headers.get("Content-Length", "0"))
            self.server.app.store.put(commit, digest, self.rfile, size, deadline=self.budget.end)
            replicas = 1 if self.server.peer else self.server.app.replicate(commit, digest, deadline=self.budget.end)
            self.budget.check()
            self.reply(200, {"stored": True, "replicas": replicas, "sha256": digest, "commit": commit})
        except (OSError, ValueError, KeyError, DeadlineExceeded):
            self.reply(400, {"error": "invalid_bundle_upload"})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True)
    args = parser.parse_args()
    config = json.loads(Path(args.config).read_text())
    app = App(config)
    local = Server(("127.0.0.1", config["local_port"]), Handler, app)
    context = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH, cafile=config["ca"])
    context.load_cert_chain(config["certificate"], config["private_key"])
    context.verify_mode = ssl.CERT_REQUIRED
    peer = Server((config["peer_bind"], config["peer_port"]), Handler, app, peer=True, tls_context=context)
    threading.Thread(target=peer.serve_forever, daemon=True).start()
    print(json.dumps({"event": "cache-ready", "local_port": local.server_port,
                      "peer_port": peer.server_port}), flush=True)
    local.serve_forever()


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Host-local frontend cache with mutually authenticated HTTPS peer replication."""
import argparse
import concurrent.futures
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import socket
import ssl
import sys
import threading
import urllib.error
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
from ci_frontend_cache import Store, identity  # noqa: E402


class Server(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, handler, app, peer=False):
        self.app, self.peer = app, peer
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
        try:
            super().process_request_thread(request, address)
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
        self.context = ssl.create_default_context(cafile=config["ca"])
        self.context.load_cert_chain(config["certificate"], config["private_key"])
        self.client = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                                 urllib.request.HTTPSHandler(context=self.context))

    def peer_request(self, url, method, commit, digest, stream=None, size=None):
        if not url.startswith("https://"):
            raise ValueError("peers require HTTPS")
        headers = {"X-Repository": self.repository}
        if size is not None:
            headers["Content-Length"] = str(size)
        request = urllib.request.Request(url.rstrip("/") + "/v1/" + commit + "/" + digest,
                                         data=stream, headers=headers, method=method)
        return self.client.open(request, timeout=20)

    def fill(self, commit, digest):
        for peer in self.peers:
            try:
                with self.peer_request(peer, "GET", commit, digest) as response:
                    size = int(response.headers["Content-Length"])
                    self.store.put(commit, digest, response, size)
                return True
            except (OSError, ValueError, urllib.error.URLError) as error:
                if isinstance(error, urllib.error.HTTPError):
                    error.close()
                continue
        return False

    def replicate(self, commit, digest):
        def push(peer):
            opened = self.store.open(commit, digest)
            if opened is None:
                return False
            stream, size = opened
            try:
                with stream, self.peer_request(peer, "PUT", commit, digest, stream, size) as response:
                    return response.status == 200
            except (OSError, ValueError, urllib.error.URLError) as error:
                if isinstance(error, urllib.error.HTTPError):
                    error.close()
                return False
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            return 1 + sum(pool.map(push, self.peers))


class Handler(BaseHTTPRequestHandler):
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
            if opened is None and not self.server.peer and self.server.app.fill(commit, digest):
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
        except (OSError, ValueError, KeyError):
            self.reply(400, {"error": "invalid_bundle_request"})

    def do_PUT(self):
        try:
            commit, digest = self.bundle_identity()
            if self.headers.get("Transfer-Encoding"):
                raise ValueError("chunked uploads are not accepted")
            size = int(self.headers.get("Content-Length", "0"))
            self.server.app.store.put(commit, digest, self.rfile, size)
            replicas = 1 if self.server.peer else self.server.app.replicate(commit, digest)
            self.reply(200, {"stored": True, "replicas": replicas, "sha256": digest, "commit": commit})
        except (OSError, ValueError, KeyError):
            self.reply(400, {"error": "invalid_bundle_upload"})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True)
    args = parser.parse_args()
    config = json.loads(Path(args.config).read_text())
    app = App(config)
    local = Server(("127.0.0.1", config["local_port"]), Handler, app)
    peer = Server((config["peer_bind"], config["peer_port"]), Handler, app, peer=True)
    context = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH, cafile=config["ca"])
    context.load_cert_chain(config["certificate"], config["private_key"])
    context.verify_mode = ssl.CERT_REQUIRED
    peer.socket = context.wrap_socket(peer.socket, server_side=True)
    threading.Thread(target=peer.serve_forever, daemon=True).start()
    local.serve_forever()


if __name__ == "__main__":
    main()

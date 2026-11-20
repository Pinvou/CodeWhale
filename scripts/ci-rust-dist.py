#!/usr/bin/env python3
"""Job-owned Rustup distribution cache; authenticated origin, verified bytes."""
import argparse
import hashlib
import http.server
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import threading
import time

ORIGIN = 'https://static.rust-lang.org'
SPEC = importlib.util.spec_from_file_location('ci_download', Path(__file__).with_name('ci-download.py'))
DOWNLOAD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DOWNLOAD)


def distribution_path(value):
    if (not re.fullmatch(r'/dist/[A-Za-z0-9_./-]+', value)
            or any(part in ('', '.', '..') for part in value.split('/')[1:])
            or not value.endswith(('.toml', '.sha256', '.tar.xz', '.tar.gz', '.asc'))):
        raise ValueError('invalid Rust distribution path')
    return value


class Distribution:
    def __init__(self, root, cache, fetch=DOWNLOAD.fetch, capacity=DOWNLOAD.DEFAULT_CAPACITY):
        self.root, self.cache, self.fetch = Path(root), Path(cache), fetch
        self.capacity = capacity
        self.lock = threading.Lock()
        self.transfer_lock = threading.Lock()
        self.locks = {}

    def download(self, name, target, expected=None, **options):
        # Rustup may request components concurrently. Reserve snapshot bytes
        # serially so concurrent misses cannot each spend the whole budget.
        with self.transfer_lock:
            used = sum(p.stat().st_size for p in self.root.iterdir()
                       if p.is_file() and re.fullmatch('[0-9a-f]{64}', p.name))
            remaining = self.capacity - used
            if remaining <= 0:
                raise DOWNLOAD.DownloadError('Rust snapshot capacity reached')
            options['max_bytes'] = min(options.get('max_bytes', DOWNLOAD.MAX_OBJECT), remaining)
            self.fetch(ORIGIN + name, target, expected, force_tls12=True, **options)

    def artifact(self, name):
        name = distribution_path(name)
        with self.lock:
            if name not in self.locks:
                if len(self.locks) >= 256:
                    raise ValueError('Rust distribution request budget exhausted')
                self.locks[name] = threading.Lock()
            lock = self.locks[name]
        with lock:
            target = self.root / hashlib.sha256(name.encode()).hexdigest()
            if target.is_file():
                return target
            # Rustup reads a channel SHA before its manifest. Hold that fresh
            # authenticated snapshot consistently within this one job; never
            # reuse a mutable channel URL from another job's state directory.
            if name.endswith(('.sha256', '.asc')):
                self.download(name, target, seconds=45, max_bytes=64 * 1024)
                if name.endswith('.sha256'):
                    self.checksum(target)
                return target
            checksum = self.artifact(name + '.sha256')
            expected = self.checksum(checksum)
            self.download(name, target, expected, cache=self.cache,
                          seconds=180, max_bytes=16 * 1024**2 if name.endswith('.toml') else DOWNLOAD.MAX_OBJECT)
            return target

    @staticmethod
    def checksum(path):
        text = path.read_text(encoding='ascii').strip()
        if not re.fullmatch(r'[0-9a-f]{64}(?:[ \t]+[^\r\n]+)?', text):
            raise ValueError('invalid official Rust checksum')
        return text[:64]


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *_):
        pass

    def do_GET(self):
        sent = False
        try:
            path = self.server.distribution.artifact(self.path)
            self.send_response(200)
            self.send_header('Content-Length', str(path.stat().st_size))
            self.send_header('Connection', 'close')
            self.end_headers()
            sent = True
            if self.command != 'HEAD':
                with path.open('rb') as source:
                    while block := source.read(256 * 1024):
                        self.wfile.write(block)
        except (ValueError, OSError, DOWNLOAD.DownloadError) as error:
            # Do not include exception text, source URLs, headers or secrets.
            if not sent and not self.wfile.closed:
                try:
                    self.send_error(502, type(error).__name__)
                except OSError:
                    pass
        finally:
            self.close_connection = True

    do_HEAD = do_GET


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 16

    def __init__(self, address, distribution):
        self.distribution = distribution
        self.slots = threading.BoundedSemaphore(4)
        super().__init__(address, Handler)

    def process_request(self, request, address):
        if not self.slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, address)
        except Exception:
            self.slots.release()
            raise

    def process_request_thread(self, request, address):
        request.settimeout(190)
        try:
            super().process_request_thread(request, address)
        finally:
            self.slots.release()

    def handle_error(self, *_):
        pass


def start(cache, temporary):
    root = Path(tempfile.mkdtemp(prefix='rust-dist-', dir=temporary))
    state = root / 'ready.json'
    options = {'start_new_session': True} if os.name != 'nt' else {
        'creationflags': subprocess.CREATE_NO_WINDOW}
    child = subprocess.Popen([sys.executable, str(Path(__file__).resolve()),
                              '--serve', str(root), '--cache', str(cache)],
                             stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL, close_fds=True, **options)
    # Keep RUNNER_TRACKING_ID and the Windows Job Object: the Runner must retire
    # this child when its job ends. A hard lifetime is a second cleanup bound.
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if state.is_file():
            ready = json.loads(state.read_text())
            if ready['pid'] != child.pid or not 1024 <= ready['port'] < 65536:
                child.terminate()
                raise ValueError('invalid owned Rust distribution listener')
            return 'http://127.0.0.1:' + str(ready['port']), state, child
        if child.poll() is not None:
            raise RuntimeError('Rust distribution listener failed')
        time.sleep(.02)
    child.terminate()
    raise TimeoutError('Rust distribution listener startup failed')


def serve(root, cache):
    # Only this verified origin downloader bypasses stale public NO_PROXY
    # entries. Business tests and package installation routing are unchanged.
    if any(os.environ.get(k) for k in ('HTTPS_PROXY', 'https_proxy', 'HTTP_PROXY', 'http_proxy')):
        os.environ['NO_PROXY'] = os.environ['no_proxy'] = 'localhost,127.0.0.1,::1'
    server = Server(('127.0.0.1', 0), Distribution(root, cache))
    state = root / 'ready.json'
    temporary = root / 'ready.tmp'
    temporary.write_text(json.dumps({'pid': os.getpid(), 'port': server.server_address[1]}))
    os.replace(temporary, state)
    statistics = {'events': [], 'counts': {}}
    statistics_lock = threading.Lock()
    def record(phase, **fields):
        with statistics_lock:
            statistics['counts'][phase] = statistics['counts'].get(phase, 0) + 1
            statistics['events'] = (statistics['events'] + [{'phase': phase, **fields}])[-32:]
            try:
                temporary = root / 'statistics.tmp'
                temporary.write_text(json.dumps(statistics))
                os.replace(temporary, root / 'statistics.json')
            except OSError:
                pass  # Runner cleanup may already be retiring this job's temp files.
    DOWNLOAD.event = record
    def retire():
        deadline = time.monotonic() + 7200
        while time.monotonic() < deadline and state.is_file():
            time.sleep(1)
        server.shutdown()
    threading.Thread(target=retire, daemon=True).start()
    try:
        server.serve_forever(poll_interval=.25)
    finally:
        server.server_close()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--serve', required=True, type=Path)
    parser.add_argument('--cache', required=True, type=Path)
    args = parser.parse_args()
    serve(args.serve, args.cache)

#!/usr/bin/env python3
"""Loopback CI CONNECT proxy with bounded, half-close-safe TCP forwarding."""
import argparse
import errno
import http.server
import ipaddress
import json
import selectors
import socket
import threading
import time
import urllib.parse
import urllib.request
import uuid

GITHUB = ('github.com', 'githubusercontent.com', 'githubassets.com', 'github.io', 'ghcr.io')
PUBLIC = GITHUB + ('rust-lang.org', 'crates.io', 'rsproxy.cn', 'deepin.com', 'blob.core.windows.net',
                   'llvm.org', 'freedesktop.org', 'npmjs.org', 'nodejs.org', 'golang.org')

# The Windows PKI HTTP bridge imports this policy and the same relay. These
# are public signed certificate/revocation services, never arbitrary HTTP.
PKI_HOSTS = frozenset(('cdp.rapidssl.com', 'status.rapidssl.com', 'cacerts.rapidssl.com',
    'crl3.digicert.com', 'crl4.digicert.com', 'ocsp.digicert.com', 'cacerts.digicert.com',
    'crl.microsoft.com', 'www.microsoft.com', 'ctldl.windowsupdate.com', 'ocsp.msocsp.com',
    'crl.globalsign.com', 'ocsp.globalsign.com', 'ocsp2.globalsign.com', 'secure.globalsign.com'))


def pki_target(url):
    target = urllib.parse.urlsplit(url)
    if (target.scheme not in ('http', 'https') or target.hostname not in PKI_HOSTS
            or target.username or target.password or target.fragment
            or target.port not in (None, 80 if target.scheme == 'http' else 443)):
        raise ValueError('unapproved certificate-service destination')
    return target


class PKIRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, newurl):
        pki_target(newurl)  # Validate before following, not after fetching it.
        return super().redirect_request(request, fp, code, message, headers, newurl)


def label(host):
    return host if any(host == suffix or host.endswith('.' + suffix) for suffix in PUBLIC) else 'other'


def event(phase, **fields):
    print(json.dumps({'phase': phase, **fields}, sort_keys=True), flush=True)


def tunnel_capacity(requested=1024, soft_limit=None):
    """Reserve descriptors for accept/DNS/logging, not just active sockets."""
    if soft_limit is None:
        try:
            import resource
            soft_limit = resource.getrlimit(resource.RLIMIT_NOFILE)[0]
            if soft_limit == resource.RLIM_INFINITY:
                return requested
        except ImportError:  # The production proxy is Linux; portable tests also run on Windows.
            return requested
    capacity = min(requested, (soft_limit - 64) // 4)
    if capacity < 1:
        raise ValueError('proxy descriptor budget is insufficient')
    return capacity


class UpstreamError(OSError):
    def __init__(self, stage, original=None):
        super().__init__(getattr(original, 'errno', None), 'upstream unavailable')
        self.stage = stage


class Resolver:
    def __init__(self):
        self.cache = {}
        self.lock = threading.Lock()
        self.inflight = {}
        self.direct = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def __call__(self, host):
        if not any(host == suffix or host.endswith('.' + suffix) for suffix in GITHUB):
            return list(dict.fromkeys(a[4][0] for a in socket.getaddrinfo(host, 443, type=socket.SOCK_STREAM)))
        with self.lock:
            cached = self.cache.get(host)
            if cached and cached[0] > time.monotonic():
                return cached[1]
            if host not in self.inflight:
                if len(self.inflight) >= 64:
                    raise OSError('resolver capacity reached')
                ready = self.inflight[host] = threading.Event()
                owner = True
            else:
                ready = self.inflight[host]
                owner = False
        if not owner:
            if not ready.wait(13):
                raise TimeoutError('resolver wait exhausted')
            with self.lock:
                cached = self.cache.get(host)
                if cached and cached[0] > time.monotonic():
                    return cached[1]
            raise OSError('resolution unavailable')
        try:
            query = urllib.parse.urlencode({'name': host, 'type': 'A'})
            with self.direct.open('https://dns.alidns.com/resolve?' + query, timeout=12) as response:
                raw = response.read(128 * 1024 + 1)
                if len(raw) > 128 * 1024:
                    raise OSError('resolver response too large')
                data = json.loads(raw)
            records = [a for a in data.get('Answer', []) if a.get('type') == 1]
            values = list(dict.fromkeys(a['data'] for a in records if ipaddress.ip_address(a['data']).is_global))
            if data.get('Status') != 0 or not values:
                raise OSError('public resolution unavailable')
            ttl = min(300, max(20, min(int(a.get('TTL', 60)) for a in records)))
            with self.lock:
                if len(self.cache) >= 512:
                    self.cache = {k: v for k, v in self.cache.items() if v[0] > time.monotonic()}
                if len(self.cache) < 512:
                    self.cache[host] = (time.monotonic() + ttl, values)
            return values
        finally:
            with self.lock:
                self.inflight.pop(host, None)
                ready.set()


def connect_target(host, resolver):
    try:
        addresses = resolver(host)
    except OSError as error:
        raise UpstreamError('dns', error) from None
    deadline = time.monotonic() + 12
    last_error = None
    for address in addresses:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        try:
            connection = socket.create_connection((address, 443), timeout=min(4, remaining))
            connection.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
            return connection
        except OSError as error:
            last_error = error
            if error.errno in (errno.ENFILE, errno.EMFILE):
                raise UpstreamError('descriptor-capacity', error) from None
            continue
    raise UpstreamError('tcp-connect', last_error)


def relay(left, right, *, idle=300, buffer_limit=256 * 1024):
    """Forward both directions until drained; FIN is directional, not failure."""
    sockets = (left, right)
    other = {left: right, right: left}
    buffers = {s: bytearray() for s in sockets}
    readers = set(sockets)
    write_closed = set()
    received = {s: 0 for s in sockets}
    last = time.monotonic()
    for s in sockets:
        s.setblocking(False)
    with selectors.DefaultSelector() as selector:
        while readers or any(buffers.values()):
            for s in sockets:
                # Once a read side closes, drain its pending bytes before
                # forwarding FIN. The peer may still send a complete response.
                if other[s] not in readers and not buffers[s] and s not in write_closed:
                    try:
                        s.shutdown(socket.SHUT_WR)
                    except OSError:
                        pass
                    write_closed.add(s)
                flags = 0
                if s in readers and len(buffers[other[s]]) < buffer_limit:
                    flags |= selectors.EVENT_READ
                if buffers[s]:
                    flags |= selectors.EVENT_WRITE
                try:
                    selector.unregister(s)
                except KeyError:
                    pass
                if flags:
                    selector.register(s, flags)
            remaining = idle - (time.monotonic() - last)
            if remaining <= 0:
                raise TimeoutError('relay idle deadline')
            ready = selector.select(remaining)
            if not ready:
                raise TimeoutError('relay idle deadline')
            for key, flags in ready:
                s = key.fileobj
                if flags & selectors.EVENT_READ:
                    try:
                        packet = s.recv(min(65536, buffer_limit - len(buffers[other[s]])))
                    except BlockingIOError:
                        packet = None
                    if packet:
                        buffers[other[s]].extend(packet)
                        received[s] += len(packet)
                        last = time.monotonic()
                    elif packet == b'':
                        readers.discard(s)
                if flags & selectors.EVENT_WRITE and buffers[s]:
                    try:
                        sent = s.send(buffers[s])
                    except BlockingIOError:
                        sent = 0
                    if sent:
                        del buffers[s][:sent]
                        last = time.monotonic()
    return {'client_bytes': received[left], 'upstream_bytes': received[right]}


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    # Do not consume an optimistic ClientHello into a buffered HTTP reader.
    rbufsize = 0

    def setup(self):
        super().setup()
        self.connection.settimeout(15)

    def log_message(self, *_):
        pass

    def do_GET(self):
        if self.path != '/health':
            self.send_error(405, 'HTTPS CONNECT only')
            return
        self.send_response(200)
        self.send_header('Content-Length', '3')
        self.end_headers()
        self.wfile.write(b'ok\n')

    def do_CONNECT(self):
        upstream = None
        connected = False
        ident = uuid.uuid4().hex[:12]
        sampled = int(ident[-2:], 16) % 16 == 0
        started = time.monotonic()
        phase = 'parse'
        host = ''
        try:
            target = urllib.parse.urlsplit('//' + self.path)
            host = (target.hostname or '').lower().rstrip('.')
            if not host or target.port != 443 or target.username or target.path or target.query or target.fragment:
                self.send_error(403, 'Only HTTPS port 443 is allowed')
                return
            phase = 'resolve-connect'
            upstream = self.server.connector(host, self.server.resolver)
            self.send_response(200, 'Connection Established')
            self.end_headers()
            self.wfile.flush()
            connected = True
            if sampled:
                peer = upstream.getpeername()[0]
                event('connected', id=ident, host=label(host),
                      upstream=peer if ipaddress.ip_address(peer).is_global else 'other',
                      elapsed_ms=round((time.monotonic() - started) * 1000))
            phase = 'relay'
            receipt = relay(self.connection, upstream)
            if sampled:
                event('drained', id=ident, host=label(host), **receipt,
                      elapsed_ms=round((time.monotonic() - started) * 1000))
        except (OSError, ValueError, KeyError, json.JSONDecodeError) as error:
            event('failed', id=ident, host=label(host), stage=getattr(error, 'stage', phase),
                  os_errno=getattr(error, 'errno', None), error_type=type(error).__name__,
                  elapsed_ms=round((time.monotonic() - started) * 1000))
            if not connected:
                self.send_error(502, 'HTTPS connection unavailable')
        finally:
            self.close_connection = True
            if upstream:
                upstream.close()


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 128

    def __init__(self, address, *, resolver=None, connector=connect_target, max_tunnels=1024):
        self.resolver = resolver or Resolver()
        self.connector = connector
        self.capacity = tunnel_capacity(max_tunnels)
        self.slots = threading.BoundedSemaphore(self.capacity)
        super().__init__(address, Handler)

    def process_request(self, request, client_address):
        # Admission precedes both HTTP header parsing and thread creation.
        # A tunnel needs a client socket, upstream socket and selector fd;
        # an unbounded accept/handler path can otherwise exhaust RLIMIT_NOFILE
        # before a CONNECT reaches its old semaphore.
        if not self.slots.acquire(blocking=False):
            try:
                request.settimeout(1)
                request.sendall(b'HTTP/1.1 503 Proxy capacity reached\r\n'
                                b'Retry-After: 1\r\nContent-Length: 0\r\nConnection: close\r\n\r\n')
            except OSError:
                pass
            finally:
                self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except Exception:
            self.slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.slots.release()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--port', type=int, default=17897)
    args = parser.parse_args()
    server = Server(('127.0.0.1', args.port))
    event('listening', protocol='CONNECT', capacity=server.capacity, log_fields='stage-host-counts-only')
    server.serve_forever()

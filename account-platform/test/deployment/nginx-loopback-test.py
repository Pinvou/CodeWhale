"""Public disposable nginx response gate. Only marked, generated loopback fixtures."""
import argparse
import os
import signal
import http.client
import http.server
import json
import pathlib
import socket
import ssl
import subprocess
import sys
import threading
import time

parser = argparse.ArgumentParser()
parser.add_argument('--root', required=True)
parser.add_argument('--profile', choices=['cloud_uat-loopback', 'cloud_uat-encrypted_tunnel'], required=True)
parser.add_argument('--nginx', default='nginx')
parser.add_argument('--exercise-cleanup', choices=['failure', 'cancel'])
args = parser.parse_args()
root = pathlib.Path(args.root).resolve(strict=True)
assert (root / '.pinvou-nginx-fixture').read_text() == 'PUBLIC_LOOPBACK_FIXTURE_ONLY\n'
profile = args.profile
exercise_reached = False
sigterm_received = False

def cancelled(_signal, _frame):
    global sigterm_received
    sigterm_received = _signal == signal.SIGTERM
    raise KeyboardInterrupt('fixture cancelled')

signal.signal(signal.SIGTERM, cancelled)
base = (root / (profile + '.conf')).read_text()
canary = 'isolated-proxy-canary-apt137'
seen = []
checks = []
log_before = (root / 'runtime-safe.log').stat().st_size if (root / 'runtime-safe.log').exists() else 0

class Mock(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def respond(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        seen.append((self.server.kind, self.command, self.path, {k.lower(): v for k, v in self.headers.items()}, body))
        behavior = self.server.behavior
        if behavior == 'disconnect':
            self.connection.shutdown(socket.SHUT_RDWR)
            self.connection.close()
            return
        if behavior == 'slow':
            time.sleep(8)
            return
        self.send_response(303 if behavior == 'redirect' else 401 if self.path == '/session' else 200)
        if behavior == 'redirect':
            self.send_header('Location', '/session')
        self.send_header('Content-Type', 'text/html; charset=utf-8')
        self.send_header('Cache-Control', 'no-store')
        self.send_header('Referrer-Policy', 'origin' if self.server.kind == 'portal' else 'no-referrer')
        self.send_header('Cross-Origin-Opener-Policy', 'same-origin')
        self.send_header('X-Content-Type-Options', 'nosniff')
        self.send_header('X-Frame-Options', 'DENY')
        self.send_header('Strict-Transport-Security', 'max-age=31536000; includeSubDomains')
        self.send_header('Content-Security-Policy', "default-src 'none'; script-src 'sha256-fixture-only'")
        self.end_headers()
        try:
            self.wfile.write(b'isolated upstream response')
        except (BrokenPipeError, ConnectionResetError):
            pass  # Expected after the deliberately delayed upstream times out.

    do_GET = respond
    do_POST = respond
    do_HEAD = respond

def port():
    with socket.socket() as reserve:
        reserve.bind(('127.0.0.1', 0))
        return reserve.getsockname()[1]

servers = []
threads = []
process = None
result = {'result': 'FAIL'}
try:
    for kind in ['portal', 'credential']:
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Mock)
        server.kind = kind
        server.behavior = 'normal'
        server.daemon_threads = True
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        servers.append(server)
        threads.append(thread)
    https_port, http_port = port(), port()
    assert https_port != http_port
    config = base.replace('listen 80;', f'listen 127.0.0.1:{http_port};').replace('listen [::]:80;', '')
    config = config.replace('listen 443 ssl;', f'listen 127.0.0.1:{https_port} ssl;').replace('listen [::]:443 ssl;', '')
    config = config.replace('http://127.0.0.1:18105', f'http://127.0.0.1:{servers[0].server_port}')
    config = config.replace('http://127.0.0.1:18108', f'http://127.0.0.1:{servers[1].server_port}')
    config = config.replace('fixture.pid', 'runtime.pid').replace('safe.log', 'runtime-safe.log').replace('parser.log', 'runtime-parser.log')
    assert 'listen [::]' not in config and 'listen 443' not in config and 'listen 80' not in config
    config_file = root / 'runtime.conf'
    config_file.write_text(config)
    with (root / 'runtime-driver.log').open('wb') as driver_log:
        process = subprocess.Popen([args.nginx, '-p', str(root) + '/', '-c', str(config_file), '-g', 'daemon off; master_process off;'], stdout=driver_log, stderr=driver_log)
        deadline = time.monotonic() + 5
        while True:
            assert process.poll() is None, 'isolated nginx startup'
            try:
                with socket.create_connection(('127.0.0.1', https_port), timeout=.2):
                    break
            except OSError:
                assert time.monotonic() < deadline, 'isolated nginx readiness'
                time.sleep(.05)
        context = ssl.create_default_context(cafile=str(root / 'fixture.crt'))
        if args.exercise_cleanup == 'failure':
            exercise_reached = True
            raise RuntimeError('fixture cleanup exercise')
        if args.exercise_cleanup == 'cancel':
            exercise_reached = True
            os.kill(os.getpid(), signal.SIGTERM)
            raise AssertionError('SIGTERM handler did not interrupt the fixture')

        def request(host, method, path, body=b'', declared_length=None):
            with socket.create_connection(('127.0.0.1', https_port), timeout=10) as raw:
                with context.wrap_socket(raw, server_hostname=host) as client:
                    headers = f'{method} {path} HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\nContent-Length: {len(body) if declared_length is None else declared_length}\r\nOrigin: https://account-uat.pinvou.com\r\nReferer: https://account-uat.pinvou.com/\r\nCookie: fixture={canary}\r\nAuthorization: Bearer {canary}\r\nForwarded: {canary}\r\nX-Forwarded-Proto: http\r\nX-Forwarded-Host: {canary}\r\nX-Pinvou-Client-Source: {canary}\r\nX-Pinvou-Absolute-Deadline: {canary}\r\nX-Pinvou-Deadline-Class: {canary}\r\nX-Pinvou-Tunnel-Proto: {canary}\r\n\r\n'
                    client.sendall(headers.encode() + body)
                    response = http.client.HTTPResponse(client)
                    response.begin()
                    pairs = response.getheaders()
                    counts = {}
                    for key, _ in pairs:
                        counts[key.lower()] = counts.get(key.lower(), 0) + 1
                    facts = (response.status, {**{k.lower(): v for k, v in pairs}, '_counts': counts}, response.read())
                    response.close()
                    return facts

        credential_host = 'credentials-uat.pinvou.com'
        portal_host = 'account-uat.pinvou.com'
        def safe_rejection(status, headers, expected, tls=True):
            assert status == expected, 'original error status preserved'
            for key, value in [('cache-control', 'no-store'), ('referrer-policy', 'no-referrer'), ('cross-origin-opener-policy', 'same-origin'), ('x-content-type-options', 'nosniff'), ('x-frame-options', 'DENY')]:
                assert headers.get(key) == value, 'reject header: ' + key
            if tls:
                assert 'max-age=31536000' in headers.get('strict-transport-security', ''), 'reject HSTS'
                assert headers['_counts'].get('strict-transport-security') == 1, 'exactly one rejection HSTS'
            else:
                assert 'strict-transport-security' not in headers, 'no misleading HSTS over HTTP'
            assert headers.get('content-security-policy') == "default-src 'none'; frame-ancestors 'none'; base-uri 'none'", 'safe rejection CSP'

        count = len(seen)
        plain = http.client.HTTPConnection('127.0.0.1', http_port, timeout=5)
        try:
            plain.request('GET', '/launch?fixture=' + canary, headers={'Host': credential_host})
            response = plain.getresponse()
            pairs = response.getheaders()
            plain_headers = {key.lower(): value for key, value in pairs}
            plain_headers['_counts'] = {key.lower(): sum(1 for item, _ in pairs if item.lower() == key.lower()) for key, _ in pairs}
            response.read()
            safe_rejection(response.status, plain_headers, 404, tls=False)
            assert len(seen) == count, 'HTTP request reached upstream'
        finally:
            plain.close()
        checks.append('plain-http-404-safe-headers-zero-upstream')

        def trusted_forwarding(forwarded, host):
            assert forwarded['host'] == host, 'trusted upstream Host'
            assert forwarded['origin'] == 'https://account-uat.pinvou.com' and forwarded['referer'] == 'https://account-uat.pinvou.com/', 'browser protocol headers preserved'
            for key in ['forwarded', 'x-pinvou-client-source', 'x-pinvou-absolute-deadline', 'x-pinvou-deadline-class']:
                assert key not in forwarded, 'untrusted header: ' + key
            assert forwarded['x-forwarded-proto'] == 'https' and forwarded['x-forwarded-host'] == host, 'trusted HTTPS headers'
            assert forwarded.get('x-pinvou-tunnel-proto') == ('https' if profile.endswith('encrypted_tunnel') else None), 'untrusted tunnel protocol header'

        cases = [(credential_host, 'GET', '/launch'), (credential_host, 'POST', '/session'), (credential_host, 'GET', '/session?x=' + canary), (credential_host, 'POST', '/launch?x=' + canary), (credential_host, 'POST', '/oidc/callback'), (credential_host, 'GET', '/unknown'), (portal_host, 'POST', '/credential-handoff'), (portal_host, 'GET', '/credential-handoff?x=' + canary), (portal_host, 'GET', '/credential-handoff/extra')]
        cases += [(credential_host, 'GET', path) for path in ['/s%65ssion', '/x/../session', '//session', '/session?', '/assets/credential%2Ecss', '/assets%2fcredential.css', '/oidc/%63allback?code=' + canary]]
        cases += [(credential_host, 'POST', path) for path in ['/l%61unch', '/credentials?']]
        cases += [(portal_host, 'GET', path) for path in ['/credential%2dhandoff', '/x/../credential-handoff', '//credential-handoff', '/credential-handoff?', '/%2e/credential-handoff']]
        for host, method, path in cases:
            count = len(seen)
            status, headers, _ = request(host, method, path)
            assert status == 404 and len(seen) == count, 'rejected request reached upstream'
            safe_rejection(status, headers, 404)
        checks.append('23-raw-path-and-method-rejections-safe-headers-zero-upstream')
        status, headers, _ = request(portal_host, 'GET', '/credential-handoff')
        assert status == 200 and headers.get('referrer-policy') == 'origin', 'handoff phase'
        assert headers.get('content-security-policy') == "default-src 'none'; script-src 'sha256-fixture-only'", 'handoff CSP preserved'
        assert headers['_counts'].get('strict-transport-security') == 1, 'upstream-plus-parent HSTS must not duplicate'
        forwarded = seen[-1][3]
        assert 'cookie' not in forwarded and 'authorization' not in forwarded, 'handoff identity stripped'
        trusted_forwarding(forwarded, portal_host)
        checks.append('handoff-success-headers-and-forwarding-boundary')
        status, _, _ = request(credential_host, 'POST', '/launch', ('launch_id=' + canary).encode())
        assert status == 200 and seen[-1][1] == 'POST' and seen[-1][4] == ('launch_id=' + canary).encode(), 'POST body preserved'
        trusted_forwarding(seen[-1][3], credential_host)
        checks.append('credential-upstream-forwarding-boundary')
        status, _, _ = request(credential_host, 'GET', '/oidc/callback?code=' + canary)
        assert status == 200 and seen[-1][2] == '/oidc/callback?code=' + canary, 'callback query preserved'
        checks.append('legitimate-post-and-oidc-query-preserved')
        status, headers, _ = request(credential_host, 'GET', '/session')
        assert status == 401 and headers['_counts'].get('strict-transport-security') == 1, 'application 401 and single HSTS'
        servers[1].behavior = 'redirect'
        status, headers, _ = request(credential_host, 'POST', '/launch', b'fixture-body')
        assert status == 303 and headers['location'] == '/session', 'application 303 unchanged'
        servers[1].behavior = 'normal'
        count = len(seen)
        status, headers, _ = request(credential_host, 'POST', '/launch', declared_length=65537)
        safe_rejection(status, headers, 413)
        assert len(seen) == count, 'oversized body reached upstream'
        for behavior, expected in [('disconnect', 502), ('slow', 504)]:
            servers[1].behavior = behavior
            status, headers, _ = request(credential_host, 'GET', '/session')
            safe_rejection(status, headers, expected)
        servers[1].behavior = 'normal'
        checks.append('401-303-preserved-and-413-502-504-safe-with-original-status')
        result = {'result': 'PASS', 'profile': profile, 'checks': checks, 'tunnel_header_spoofed': forwarded.get('x-pinvou-tunnel-proto') == canary}
except Exception as error:
    result = {'result': 'FAIL', 'checks': checks, 'failure_type': type(error).__name__, 'assertion': str(error) if isinstance(error, AssertionError) else 'details-omitted'}
finally:
    if process is not None and process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)
    for server in servers:
        server.shutdown()
        server.server_close()
    for thread in threads:
        thread.join(timeout=3)
    safe_log = root / 'runtime-safe.log'
    parser_log = root / 'runtime-parser.log'
    if result['result'] == 'PASS':
        assert safe_log.exists() and safe_log.stat().st_size > log_before, 'this run must add safe log evidence'
        assert canary not in safe_log.read_text() + (parser_log.read_text() if parser_log.exists() else ''), 'canary leaked to nginx logs'
        result['checks'].append('nonempty-safe-log-and-no-query-body-header-canary')
    result['owned_nginx_stopped'] = process is None or process.poll() is not None
    result['owned_mocks_stopped'] = all(not thread.is_alive() for thread in threads)
    result['exercise_mode'] = args.exercise_cleanup
    result['exercise_reached'] = exercise_reached
    result['sigterm_received'] = sigterm_received
    print(json.dumps(result))
    if result['result'] != 'PASS':
        raise SystemExit(1)

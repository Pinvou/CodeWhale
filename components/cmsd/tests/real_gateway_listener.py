"""Real occupied inference port, management accessibility and listener recovery.

Creates its own listener/daemon/state only. No model adoption/writes/inference,
no original services, no physical multi-node verification. --baseline records
the older behavior without claiming gateway runtime facts or automatic recovery.
"""
import argparse
import concurrent.futures
import hashlib
import http.client
import json
import os
import signal
import socket
import subprocess
import tempfile
import time
from pathlib import Path


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX); self.sock.settimeout(self.timeout); self.sock.connect(self.host)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--port', type=int, default=18922)
    parser.add_argument('--baseline', action='store_true')
    args = parser.parse_args(); root = args.evidence.resolve(); root.mkdir(mode=0o700, parents=True, exist_ok=False)
    binary = root / 'cmsd-tested'; binary.write_bytes(args.binary.resolve().read_bytes()); binary.chmod(0o700)
    temporary = tempfile.TemporaryDirectory(prefix='cmsd-gw-', dir='/run/user/' + str(os.geteuid()))
    sock = Path(temporary.name) / 'api.sock'
    config = root / 'cmsd.toml'
    config.write_text(''.join(k + ' = ' + json.dumps(str(v)) + '\n' for k, v in {
        'socket_path': sock, 'state_dir': root / 'state', 'gateway_listen': f'127.0.0.1:{args.port}',
        'systemd_mode': 'user', 'cards_dir': root / 'cards', 'unit_dir': root / 'units',
        'engine_root': root / 'engines', 'store_socket_path': Path(temporary.name) / 'absent.sock'}.items()))
    env = {k: v for k, v in os.environ.items() if not k.startswith('CMSD_')}; env['CMSD_CONFIG'] = str(config)
    protected = subprocess.check_output(['systemctl', '--user', 'show', 'cmsd.service', '--property=MainPID'], text=True).strip()
    report = {'scope': __doc__.strip(), 'baseline': args.baseline, 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'errors': []}
    process = None; log = (root / 'daemon.log').open('wb'); started = time.monotonic()

    def status():
        begin = time.monotonic(); connection = UnixHTTP(str(sock), timeout=2)
        try:
            connection.request('GET', '/v1/status'); response = connection.getresponse()
            assert response.status == 200
            return json.loads(response.read()), time.monotonic() - begin
        finally: connection.close()

    try:
        with socket.socket() as occupant:
            occupant.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); occupant.bind(('127.0.0.1', args.port)); occupant.listen(1)
            process = subprocess.Popen([str(binary)], env=env, stdout=log, stderr=log, start_new_session=True)
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                assert process.poll() is None
                try: initial, _ = status(); break
                except OSError: time.sleep(.02)
            else: raise TimeoutError('isolated management startup')
            time.sleep(.1); initial, _ = status()
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool: observations = list(pool.map(lambda _: status(), range(64)))
            assert all(point[0]['gateway'] == f'127.0.0.1:{args.port}' for point in observations)
            assert occupant.fileno() >= 0 and occupant.getsockname()[1] == args.port
            report['occupied'] = {'management_queries': len(observations), 'max_status_seconds': max(point[1] for point in observations),
                                  'configured_gateway': initial['gateway'], 'gateway_status': initial.get('gateway_status'), 'occupant_preserved': True}
            if not args.baseline:
                for snapshot, _ in observations:
                    facts = snapshot['gateway_status']
                    assert facts['ready'] is False and facts['bound_listen'] is None and facts['error']['code'] == 'gateway_address_in_use'
                    assert facts['upstream_health_verified'] is False and 0 <= facts['retry_after_ms'] <= 5000 and facts['retry_interval_ms'] == 5000
                inspected = subprocess.run([str(binary), '--check-config'], env=env, capture_output=True, text=True, timeout=3, check=True)
                assert json.loads(inspected.stdout)['runtime_access_verified'] is False
                # Prove repeated binding failure remains bounded and observable.
                time.sleep(5.2); again, _ = status()
                assert again['gateway_status']['ready'] is False and again['gateway_status']['bind_attempts'] >= 2
        released = time.monotonic(); deadline = released + (6 if args.baseline else 8)
        recovered = None
        while time.monotonic() < deadline:
            snapshot, _ = status()
            if snapshot.get('gateway_status', {}).get('ready') is True:
                recovered = snapshot['gateway_status']; break
            time.sleep(.05)
        connection = http.client.HTTPConnection('127.0.0.1', args.port, timeout=2)
        try:
            connection.request('GET', '/v1/models'); response = connection.getresponse(); payload = json.loads(response.read())
            reachable = True
            if not args.baseline: assert response.status == 503 and payload['error']['code'] == 'no_active_model'
        except OSError:
            reachable = False
        finally: connection.close()
        if not args.baseline:
            assert recovered is not None and recovered['bound_listen'] == f'127.0.0.1:{args.port}' and reachable
            assert process.poll() is None
        report['after_release'] = {'gateway_status': recovered, 'actual_gateway_reachable': reachable, 'seconds': round(time.monotonic() - released, 4), 'daemon_restarted': False}
        (root / 'actual-status.json').write_text(json.dumps({'occupied': initial, 'after_release': snapshot}, indent=2) + '\n')
    except Exception as error:
        report['errors'].append(repr(error)); raise
    finally:
        if process and process.poll() is None: os.killpg(process.pid, signal.SIGKILL); process.wait(timeout=5)
        log.close(); temporary.cleanup()
        report['protected_original_cmsd_unchanged'] = protected == subprocess.check_output(['systemctl', '--user', 'show', 'cmsd.service', '--property=MainPID'], text=True).strip()
        report['seconds'] = round(time.monotonic() - started, 3)
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n'); print(json.dumps(report))


if __name__ == '__main__': main()

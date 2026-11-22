#!/usr/bin/env python3
"""Actual diagnostic CLI access boundaries on an empty owned daemon.

No model inference is performed in this companion test. Real-model integration
is verified separately. Run as a non-root Linux user with a fresh
private evidence directory. No existing unit or model is stopped.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import socket
import stat
import subprocess
import tempfile
import time

from real_diagnostics import capture


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    args = parser.parse_args()
    assert os.geteuid() != 0, 'permission denial must be tested as a non-root user'
    os.umask(0o077)
    binary = args.binary.resolve(strict=True)
    root = args.evidence.resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    runtime = tempfile.TemporaryDirectory(prefix='cmsd-diagnostic-', dir='/run/user/' + str(os.geteuid()))
    sock = Path(runtime.name) / 'api.sock'
    for name in ('cards', 'units', 'engines', 'models', 'staging'):
        (root / name).mkdir(mode=0o700)
    config = { 'socket_path': str(sock), 'state_dir': str(root / 'state'),
               'gateway_listen': '127.0.0.1:0', 'systemd_mode': 'user',
               'cards_dir': str(root / 'cards'), 'unit_dir': str(root / 'units'),
               'engine_root': str(root / 'engines'), 'models_dir': str(root / 'models'),
               'staging_dir': str(root / 'staging'), 'store_socket_path': str(Path(runtime.name) / 'absent.sock') }
    (root / 'cmsd.toml').write_text(''.join(k + ' = ' + json.dumps(v) + '\n' for k, v in config.items()))
    env = {k: v for k, v in os.environ.items() if not k.startswith('CMSD_')}
    env['CMSD_CONFIG'] = str(root / 'cmsd.toml')
    log = (root / 'daemon.log').open('wb')
    original = subprocess.check_output(['systemctl', '--user', 'show', 'cmsd.service', '-p', 'MainPID'])
    started = time.monotonic()
    report = {'scope': __doc__.strip(), 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
              'errors': [], 'inference_count': 0}
    process = None
    try:
        process = subprocess.Popen([str(binary)], env=env, stdout=log, stderr=log)
        deadline = time.monotonic() + 10
        while True:
            assert process.poll() is None
            value, code, _ = capture(binary, root, 'initializing')
            if code == 0:
                break
            assert time.monotonic() < deadline, 'owned empty daemon did not become queryable'
            time.sleep(.05)
        assert next(row for row in value['checks'] if row['id'] == 'daemon')['facts']['registered_models'] == 0
        assert all(row['state'] == 'unsupported' for row in value['checks'] if row['id'] in ('remote', 'plans'))
        state = root / 'state'
        def authority():
            return {str(p.relative_to(state)): hashlib.sha256(p.read_bytes()).hexdigest()
                    for p in state.rglob('*') if p.is_file() and p.name != 'daemon.lock'}
        before = authority()
        info = sock.stat()
        try:
            sock.chmod(0)
            denied, _, _ = capture(binary, root, 'socket-permission-denied', 4)
            assert all(row['code'] == 'access_denied' and row['facts'] is None for row in denied['checks'])
            assert stat.S_IMODE(sock.stat().st_mode) == 0 and sock.stat().st_ino == info.st_ino
        finally:
            sock.chmod(stat.S_IMODE(info.st_mode))
        capture(binary, root, 'permission-restored', 0)
        assert process.poll() is None

        path = Path(runtime.name) / 'full.sock'
        holders = []
        try:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
                listener.bind(str(path)); listener.listen(1)
                for _ in range(8):
                    client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                    client.setblocking(False)
                    try:
                        client.connect(str(path))
                        holders.append(client)
                    except OSError:
                        client.close()
                        break
                assert holders
                inode = path.stat().st_ino
                blocked, _, elapsed = capture(binary, root, 'full-listen-backlog', 4, {'CMSD_SOCKET': str(path)})
                assert all(row['code'] == 'request_timeout' for row in blocked['checks'])
                assert 3.7 <= elapsed < 6.5 and path.stat().st_ino == inode
        finally:
            for holder in holders:
                holder.close()
        sentinel = Path(runtime.name) / 'sentinel'
        sentinel.write_bytes(b'preserve-ordinary-file')
        capture(binary, root, 'ordinary-file-endpoint', 4, {'CMSD_SOCKET': str(sentinel)})
        assert sentinel.read_bytes() == b'preserve-ordinary-file'
        link = Path(runtime.name) / 'broken.sock'
        link.symlink_to('absent-sentinel-target')
        inode = link.lstat().st_ino
        capture(binary, root, 'broken-link-endpoint', 4, {'CMSD_SOCKET': str(link)})
        assert link.is_symlink() and link.lstat().st_ino == inode and os.readlink(link) == 'absent-sentinel-target'
        bad = root / 'invalid.toml'
        bad.write_text('state_dir = ' + json.dumps(str(root / 'must-not-create')) + '\nunknown_option = "canary-diagnostic-source-value"\n')
        outcome = subprocess.run([str(binary), '--diagnose'], env={**env, 'CMSD_CONFIG': str(bad)}, capture_output=True, timeout=3)
        assert outcome.returncode == 2 and not outcome.stdout
        assert b'canary-diagnostic-source-value' not in outcome.stderr and not (root / 'must-not-create').exists()
        (root / 'invalid-config-result.json').write_text(json.dumps({'exit_code': outcome.returncode, 'source_value_leaked': False, 'state_created': False}) + '\n')
        capture(binary, root, 'all-boundaries-recovered', 0)
        assert authority() == before
        report.update(cases=7, optional_remote_not_required=True, authority_bytes_unchanged=True,
                      same_owned_daemon_pid=True, socket_and_sentinels_preserved=True,
                      invalid_configuration_before_connection=True)
    except BaseException as error:
        report['errors'].append(type(error).__name__)
        raise
    finally:
        if process and process.poll() is None:
            process.terminate()
            process.wait(timeout=10)
        log.close()
        runtime.cleanup()
        report['elapsed_seconds'] = time.monotonic() - started
        report['cleanup_completed'] = True
        report['original_cmsd_pid_unchanged'] = subprocess.check_output(['systemctl', '--user', 'show', 'cmsd.service', '-p', 'MainPID']) == original
        (root / 'report.json').write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
        print(json.dumps({k: report[k] for k in ('errors', 'elapsed_seconds', 'cleanup_completed', 'inference_count')}))


if __name__ == '__main__':
    main()

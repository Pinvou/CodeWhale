#!/usr/bin/env python3
"""Opt-in real Unix startup/locking failures, on new isolated state only.

No model writes/inference, service replacement, network configuration or physical
multi-node claim. Uses actual listeners, filesystem permissions, OS locks and
daemon SIGKILL; requires an unprivileged Linux desktop user (not root).
"""
import argparse
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
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.host)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    args = parser.parse_args()
    assert os.geteuid() != 0, 'Permission-denied acceptance must run unprivileged'
    root = args.evidence.resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    binary = root / 'cmsd-tested'
    binary.write_bytes(args.binary.resolve().read_bytes())
    binary.chmod(0o700)
    report = {'scope': __doc__.strip(), 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
              'cases': {}, 'errors': []}
    processes, logs = [], []
    started = time.monotonic()
    protected = subprocess.check_output(['systemctl', '--user', 'show', 'cmsd.service', '--property=MainPID'], text=True).strip()
    runtime = tempfile.TemporaryDirectory(prefix='cmsd-start-', dir='/run/user/' + str(os.geteuid()))
    short = Path(runtime.name)

    def config(name, state=None, sock=None):
        directory = root / name
        directory.mkdir(mode=0o700)
        settings = {'state_dir': state or directory / 'state', 'socket_path': sock or short / (name + '.sock'),
                    'gateway_listen': '127.0.0.1:0', 'systemd_mode': 'user', 'cards_dir': directory / 'cards',
                    'unit_dir': directory / 'units', 'engine_root': directory / 'engines',
                    'store_socket_path': short / 'absent-store.sock'}
        path = directory / 'cmsd.toml'
        path.write_text(''.join(k + ' = ' + json.dumps(str(v)) + '\n' for k, v in settings.items()))
        path.chmod(0o600)
        return path, Path(settings['state_dir']), Path(settings['socket_path'])

    def launch(conf):
        output = (conf.parent / ('daemon-' + str(len(processes)) + '.log')).open('wb')
        logs.append(output)
        env = {k: v for k, v in os.environ.items() if not k.startswith('CMSD_')}
        env['CMSD_CONFIG'] = str(conf)
        process = subprocess.Popen([str(binary)], env=env, stdout=output, stderr=output, start_new_session=True)
        processes.append(process)
        return process

    def reject(conf):
        begin = time.monotonic()
        process = launch(conf)
        assert process.wait(timeout=1.5) != 0, 'occupied/unsafe path must refuse startup'
        return round(time.monotonic() - begin, 4)

    def status(sock):
        connection = UnixHTTP(str(sock), timeout=.3)
        try:
            connection.request('GET', '/v1/status')
            response = connection.getresponse()
            assert response.status == 200
            return json.loads(response.read())
        finally:
            connection.close()

    def ready(process, sock):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            assert process.poll() is None, 'inspect private daemon log'
            try:
                return status(sock)
            except (OSError, http.client.HTTPException):
                time.sleep(.02)
        raise TimeoutError('actual daemon not ready')

    def kill(process):
        if process.poll() is None:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=5)

    try:
        for case in ('active', 'full-backlog', 'permission-denied'):
            conf, _, sock = config(case)
            with socket.socket(socket.AF_UNIX) as listener:
                listener.bind(str(sock)); listener.listen(1)
                inode = sock.stat().st_ino
                holders = []
                try:
                    if case == 'full-backlog':
                        for _ in range(4):
                            client = socket.socket(socket.AF_UNIX)
                            client.setblocking(False)
                            try:
                                client.connect(str(sock)); holders.append(client)
                            except BlockingIOError:
                                client.close(); break
                        assert len(holders) >= 2, 'real AF_UNIX accept queue not full'
                    elif case == 'permission-denied':
                        sock.chmod(0)
                    elapsed = reject(conf)
                    assert sock.stat().st_ino == inode and listener.fileno() >= 0
                    report['cases'][case] = {'refused_in_seconds': elapsed, 'occupant_inode_retained': True}
                finally:
                    for client in holders: client.close()
                    sock.chmod(0o600)

        for case in ('regular-file', 'symlink-socket', 'hardlink-socket'):
            conf, _, sock = config(case)
            target = short / (case + '.target')
            listener = None
            if case == 'regular-file':
                sock.write_text('retain sentinel')
            else:
                listener = socket.socket(socket.AF_UNIX); listener.bind(str(target)); listener.listen(1)
                if case == 'symlink-socket': sock.symlink_to(target)
                else: os.link(target, sock)
            inode = sock.lstat().st_ino
            try:
                elapsed = reject(conf)
                assert sock.lstat().st_ino == inode
                if case == 'regular-file': assert sock.read_text() == 'retain sentinel'
                report['cases'][case] = {'refused_in_seconds': elapsed, 'original_retained': True}
            finally:
                if listener: listener.close()

        conf, state, sock = config('crash-recovery')
        with socket.socket(socket.AF_UNIX) as stale:
            stale.bind(str(sock))
        state.mkdir(mode=0o700)
        legacy = state / 'daemon.lock'
        legacy.write_text('legacy sentinel'); legacy.chmod(0o644)
        legacy_inode = legacy.stat().st_ino
        process = launch(conf); ready(process, sock)
        identity = (state / 'device-identity.json').read_bytes()
        assert legacy.stat().st_ino == legacy_inode and legacy.stat().st_mode & 0o777 == 0o600
        assert legacy.read_text() == 'legacy sentinel'
        different_socket, _, unused = config('same-state', state=state)
        reject(different_socket); assert not unused.exists()
        different_state, _, _ = config('same-socket', sock=sock)
        reject(different_state); ready(process, sock)
        kill(process)
        process = launch(conf); ready(process, sock)
        assert identity == (state / 'device-identity.json').read_bytes()
        report['cases']['crash-recovery'] = {'owned_stale_socket_recovered': True, 'legacy_lock_inode_and_data_retained': True,
                                             'legacy_lock_mode': '0600', 'same_state_and_same_socket_excluded': True,
                                             'sigkill_restart_preserved_identity': True}
        kill(process)

        for case in ('symlink-state-lock', 'hardlink-state-lock', 'symlink-socket-lock', 'hardlink-socket-lock'):
            conf, state, sock = config(case)
            state.mkdir(mode=0o700)
            lock = state / 'daemon.lock' if 'state-lock' in case else sock.with_name(sock.name + '.lock')
            target = lock.parent / (case + '.sentinel'); target.write_text('retain target'); target.chmod(0o644)
            if case.startswith('symlink'): lock.symlink_to(target)
            else: os.link(target, lock)
            elapsed = reject(conf)
            assert target.read_text() == 'retain target' and target.stat().st_mode & 0o777 == 0o644
            assert not sock.exists()
            report['cases'][case] = {'refused_in_seconds': elapsed, 'sentinel_and_mode_retained': True}

        for case, shared_state in (('concurrent-state', True), ('concurrent-socket', False)):
            sock = short / (case + '.sock')
            state = root / (case + '-shared-state')
            configs = [config(case + '-' + str(i), state=state if shared_state else None, sock=sock)[0] for i in range(16)]
            group = [launch(conf) for conf in configs]
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline and sum(p.poll() is None for p in group) > 1: time.sleep(.02)
            survivors = [p for p in group if p.poll() is None]
            assert len(survivors) == 1, 'exactly one daemon may acquire shared resources'
            ready(survivors[0], sock)
            inode = sock.stat().st_ino
            assert all(p.returncode != 0 for p in group if p is not survivors[0])
            assert sock.stat().st_ino == inode
            report['cases'][case] = {'processes': 16, 'serving': 1, 'explicit_nonzero_rejections': 15}
            kill(survivors[0])
        assert protected == subprocess.check_output(['systemctl', '--user', 'show', 'cmsd.service', '--property=MainPID'], text=True).strip()
        report['protected_original_cmsd_unchanged'] = True
    except Exception as error:
        report['errors'].append(str(error))
        raise
    finally:
        for process in processes: kill(process)
        for log in logs: log.close()
        runtime.cleanup()
        report['duration_seconds'] = round(time.monotonic() - started, 3)
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
        print(json.dumps(report))


if __name__ == '__main__': main()

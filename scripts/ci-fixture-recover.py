"""Recover only a dead controller's exact Linux scope; never kill by PID alone."""
import hashlib
import os
from pathlib import Path
import re
import select
import shutil
import signal
import sys
import time


def start(pid):
    return Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()[19]


def recover(receipt, grace=5):
    if sys.platform != 'linux' or not hasattr(os, 'pidfd_open') or not hasattr(signal, 'pidfd_send_signal'):
        raise RuntimeError('exact process recovery requires Linux pidfd support')
    scope = receipt.get('scope', {})
    if (receipt.get('schema') != 1 or receipt.get('kind') != 'process'
            or type(receipt.get('pid')) is not int or receipt['pid'] <= 1
            or not isinstance(scope, dict)
            or not isinstance(scope.get('controller_start'), str)
            or not scope['controller_start'].isdecimal()
            or not isinstance(scope.get('id'), str)
            or not re.fullmatch('[0-9a-f]{32}', scope.get('id', ''))
            or scope.get('uid') != os.getuid()
            or scope.get('cgroup') != hashlib.sha256(Path('/proc/self/cgroup').read_bytes()).hexdigest()):
        raise ValueError('scope registration is outside this job boundary')
    try:
        if start(receipt['pid']) == scope['controller_start']:
            raise RuntimeError('live controller cannot be reclaimed')
    except FileNotFoundError:
        pass
    marker = ('PINVOU_CI_PROCESS_SCOPE=' + scope['id']).encode()
    deadline = time.monotonic() + grace * 2
    empty = 0
    while time.monotonic() < deadline:
        held = []
        try:
            for item in Path('/proc').iterdir():
                if not item.name.isdecimal():
                    continue
                fd = None
                try:
                    if item.stat().st_uid != scope['uid']:
                        continue
                    if hashlib.sha256((item / 'cgroup').read_bytes()).hexdigest() != scope['cgroup']:
                        continue
                    # Read credentials only in memory to compare this exact,
                    # non-secret scope marker; never output/save environment.
                    if marker not in (item / 'environ').read_bytes().split(b'\0'):
                        continue
                    before = start(int(item.name))
                    fd = os.pidfd_open(int(item.name))
                    if start(int(item.name)) != before or marker not in (item / 'environ').read_bytes().split(b'\0'):
                        raise RuntimeError('scope process identity changed')
                    held.append(fd)
                    fd = None
                except (FileNotFoundError, ProcessLookupError, PermissionError):
                    continue
                finally:
                    if fd is not None:
                        os.close(fd)
            if not held:
                empty += 1
                if empty == 2:
                    workspace = receipt['workspace']
                    root = Path(workspace['path'])
                    if root.exists():
                        current = root.lstat()
                        if (root.is_symlink() or not root.name.startswith('pinvou-ci-fixture-')
                                or (current.st_dev, current.st_ino) != (workspace['dev'], workspace['ino'])
                                or (root / '.pinvou-fixture-scope').read_text(encoding='ascii') != scope['id']):
                            raise RuntimeError('owned workspace identity changed')
                        shutil.rmtree(root)
                    return {**receipt, 'state': 'released', 'cleanup_failed': True, 'recovered_after_controller_exit': True}
            else:
                empty = 0
                signum = signal.SIGKILL if time.monotonic() > deadline - grace else signal.SIGTERM
                # All current handles have been checked before signalling any.
                for fd in held:
                    try:
                        signal.pidfd_send_signal(fd, signum)
                    except ProcessLookupError:
                        pass
        finally:
            for fd in held:
                os.close(fd)
        time.sleep(0.025)
    raise RuntimeError('owned scope retirement could not be confirmed')

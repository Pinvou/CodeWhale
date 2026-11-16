"""Execute an offline browser fixture tree in a private Linux loopback network.

The existing ci-fixture-process supervisor remains outside this namespace and
owns every descendant. Dependency installation/build preparation happens first.
No fallback to the host network is permitted when isolation is requested.
"""
import argparse
import ctypes
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import struct
import sys


def network_command(command):
    if sys.platform != 'linux' or os.getuid() == 0:
        raise RuntimeError('browser network isolation requires a non-root Linux user')
    unshare = shutil.which('unshare')
    if not unshare:
        raise RuntimeError('browser network isolation requires util-linux unshare')
    return [unshare, '--user', '--map-current-user', '--net', '--keep-caps',
            sys.executable, str(Path(__file__).resolve()), '--enter',
            '--uid', str(os.getuid()), '--gid', str(os.getgid()),
            '--parent-network', os.readlink('/proc/self/ns/net'),
            '--parent-cgroup', hashlib.sha256(Path('/proc/self/cgroup').read_bytes()).hexdigest(),
            '--', *command]


def enable_loopback():
    import fcntl
    # stdlib ioctl avoids installing iproute2 in otherwise ready CI images.
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as control:
        request = struct.pack('16sh', b'lo', 0)
        flags = struct.unpack('16sh', fcntl.ioctl(control, 0x8913, request)[:18])[1]
        fcntl.ioctl(control, 0x8914, struct.pack('16sh', b'lo', flags | 1))


def drop_setup_capabilities():
    class Header(ctypes.Structure):
        _fields_ = [('version', ctypes.c_uint32), ('pid', ctypes.c_int)]

    class Capabilities(ctypes.Structure):
        _fields_ = [('effective', ctypes.c_uint32), ('permitted', ctypes.c_uint32), ('inheritable', ctypes.c_uint32)]

    libc = ctypes.CDLL(None, use_errno=True)
    header, empty = Header(0x20080522, 0), (Capabilities * 2)()
    # unshare retains capabilities only to configure this new namespace.
    # Tests and Chrome execute with the original non-root UID and zero caps.
    if libc.prctl(47, 4, 0, 0, 0) or libc.capset(ctypes.byref(header), ctypes.byref(empty)):
        raise OSError(ctypes.get_errno(), 'cannot retire network setup capabilities')
    status = dict(line.split(':', 1) for line in Path('/proc/self/status').read_text().splitlines() if ':' in line)
    if any(int(status[name].strip(), 16) for name in ['CapEff', 'CapPrm', 'CapInh', 'CapAmb']):
        raise RuntimeError('network setup capabilities were retained')


def enter(args, command):
    network = os.readlink('/proc/self/ns/net')
    if (os.getuid(), os.getgid()) != (args.uid, args.gid) or not args.uid:
        raise RuntimeError('network isolation changed the test user identity')
    if network == args.parent_network:
        raise RuntimeError('browser fixture still shares the parent network')
    if hashlib.sha256(Path('/proc/self/cgroup').read_bytes()).hexdigest() != args.parent_cgroup:
        raise RuntimeError('network isolation changed the resource cgroup')
    if [name for _, name in socket.if_nameindex()] != ['lo']:
        raise RuntimeError('browser fixture network is not loopback-only')
    enable_loopback()
    drop_setup_capabilities()
    facts = {'schema': 1, 'mode': 'loopback', 'uid': os.getuid(), 'gid': os.getgid(),
             'network': network, 'parent_network': args.parent_network, 'capabilities': 'none'}
    ledger = os.environ.get('PINVOU_CI_RESOURCE_LEDGER')
    if ledger:
        directory = Path(ledger)
        if directory.is_symlink() or not directory.is_dir():
            raise RuntimeError('browser network evidence directory unavailable')
        # Deliberately separate from mandatory lifecycle *.json receipts.
        descriptor = os.open(directory / 'browser-network.txt', os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(descriptor, 'w') as output:
            json.dump(facts, output)
    print('[ci-browser-network] ' + json.dumps(facts), flush=True)
    os.execvpe(command[0], command, os.environ)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--enter', action='store_true', help=argparse.SUPPRESS)
    parser.add_argument('--uid', type=int, help=argparse.SUPPRESS)
    parser.add_argument('--gid', type=int, help=argparse.SUPPRESS)
    parser.add_argument('--parent-network', help=argparse.SUPPRESS)
    parser.add_argument('--parent-cgroup', help=argparse.SUPPRESS)
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args(argv)
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    if not command:
        parser.error('browser fixture command required')
    try:
        if args.enter:
            enter(args, command)
        else:
            isolated = network_command(command)
            os.execv(isolated[0], isolated)
    except (OSError, RuntimeError) as error:
        print(f'Browser network isolation failed ({type(error).__name__}): {error}', file=sys.stderr)
        return 78


if __name__ == '__main__':
    sys.exit(main())

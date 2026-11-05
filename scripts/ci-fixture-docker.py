"""Job-local Docker creation leases. Never discovers ownership by name/prefix.

Installed only on the Account job PATH. Docker itself writes a fresh cidfile
before starting a container; the exact immutable ID is the cleanup authority.
No command arguments, environment, Docker credentials or inspect payloads are
recorded. Shared images, builders and named cache volumes are never retired.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import uuid


def endpoint(environment=None):
    environment = os.environ if environment is None else environment
    return hashlib.sha256(json.dumps({key: environment.get(key) for key in
        ('DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH')},
        sort_keys=True).encode()).hexdigest()


def record(path, value):
    temporary = path.with_suffix('.' + uuid.uuid4().hex + '.tmp')
    with temporary.open('x', encoding='utf-8') as output:
        json.dump(value, output, sort_keys=True)
        output.flush()
        os.fsync(output.fileno())
    os.replace(temporary, path)


def executable():
    value = Path(os.environ['PINVOU_CI_REAL_DOCKER'])
    if not value.is_absolute() or not value.is_file():
        raise RuntimeError('registered Docker executable unavailable')
    return str(value)


def identifier(path):
    if path.is_symlink() or path.stat().st_size > 128:
        raise RuntimeError('invalid container creation receipt')
    value = path.read_text(encoding='ascii').strip()
    if not re.fullmatch('[0-9a-f]{64}', value):
        raise RuntimeError('container ID was not confirmed')
    return value


def existing(run):
    value = run(['ps', '--all', '--quiet', '--no-trunc'])
    ids = set(value.split())
    if any(not re.fullmatch('[0-9a-f]{64}', item) for item in ids):
        raise RuntimeError('container inventory was not confirmed')
    return ids


def anonymous_volumes(item):
    named_targets = set()
    config = item.get('HostConfig', {})
    for bind in config.get('Binds') or []:
        parts = bind.split(':')
        if len(parts) > 1:
            named_targets.add(parts[1])
    for mount in config.get('Mounts') or []:
        if mount.get('Source'):
            named_targets.add(mount.get('Target'))
    return [mount['Name'] for mount in item.get('Mounts', [])
            if mount.get('Type') == 'volume' and mount.get('Destination') not in named_targets]


def observe_volumes(cid, run):
    # Docker omits optional HostConfig keys when they were not specified.
    # Direct map member access fails the whole Go template on those engines;
    # index gives JSON null without exposing Config.Env or command arguments.
    raw = run(['inspect', '--format', '{{json .Id}}\n{{json (index .HostConfig "Binds")}}\n{{json (index .HostConfig "Mounts")}}\n{{json .Mounts}}', cid])
    parts = raw.splitlines()
    if len(parts) != 4 or json.loads(parts[0]) != cid:
        raise RuntimeError('container identity was not confirmed')
    item = {'HostConfig': {'Binds': json.loads(parts[1]), 'Mounts': json.loads(parts[2])}, 'Mounts': json.loads(parts[3])}
    volumes = anonymous_volumes(item)
    if any(not isinstance(name, str) or not re.fullmatch('[a-zA-Z0-9][a-zA-Z0-9_.-]*', name) for name in volumes):
        raise RuntimeError('anonymous volume identity was not confirmed')
    return volumes


def engine_run(args, environment):
    result = subprocess.run([executable(), *args], capture_output=True,
                            text=True, timeout=45, env=environment)
    if result.returncode:
        raise RuntimeError('owned container operation failed')
    return result.stdout


def recover(root, run=None):
    """Validate every lease before any destructive call, then retire exact IDs."""
    if root.is_symlink() or not root.is_dir():
        raise RuntimeError('owned Docker ledger unavailable')
    test_run = run
    def invoke(args, environment):
        if test_run is not None:
            return test_run(args)
        else:
            # Keep engine diagnostics private: Docker errors can include URLs.
            return engine_run(args, environment)
    environments = [dict(os.environ)]
    default_context = dict(os.environ)
    default_context.pop('DOCKER_HOST', None)
    environments.append(default_context)
    endpoints = {endpoint(env): env for env in environments}
    leases = []
    for path in sorted(root.glob('docker-*.json')):
        if path.is_symlink() or path.stat().st_size > 65536:
            raise RuntimeError('invalid Docker ledger boundary')
        value = json.loads(path.read_text(encoding='utf-8'))
        lease = value.get('id')
        if (value.get('schema') != 1 or value.get('kind') != 'docker'
                or not isinstance(lease, str) or not re.fullmatch('[0-9a-f]{32}', lease)
                or path.name != 'docker-' + lease + '.json'
                or value.get('endpoint') not in endpoints):
            raise RuntimeError('container lease is outside this job endpoint')
        if value.get('state') == 'released':
            continue
        cid = identifier(root / ('docker-' + lease + '.cid'))
        leases.append((path, value, cid, endpoints[value['endpoint']]))
    # Persist anonymous volume identities before removal so interrupted cleanup
    # can still verify them after the container itself has disappeared.
    prepared = []
    for path, value, cid, environment in leases:
        def run(args):
            return invoke(args, environment)
        inventory = existing(run)
        if cid in inventory:
            # --format excludes Config.Env and command arguments, including
            # possible test credentials; no unrestricted inspect is logged.
            volumes = observe_volumes(cid, run)
            value = {**value, 'anonymous_volumes': volumes}
            record(path, value)
        prepared.append((path, value, cid, environment))
    failures = []
    for path, value, cid, environment in prepared:
        def run(args):
            return invoke(args, environment)
        try:
            if cid in existing(run):
                run(['stop', '--time', '10', cid])
                # --rm containers may already have disappeared. Never remove a
                # guessed volume name: -v removes this container's anonymous
                # volumes only and leaves named caches intact.
                if cid in existing(run):
                    run(['rm', '--volumes', cid])
            if cid in existing(run):
                raise RuntimeError('container retirement was not confirmed')
            if value.get('anonymous_volumes'):
                remaining = set(run(['volume', 'ls', '--quiet']).split())
                if remaining.intersection(value['anonymous_volumes']):
                    raise RuntimeError('anonymous volume retirement was not confirmed')
            record(path, {**value, 'state': 'released'})
        except (OSError, ValueError, RuntimeError, subprocess.SubprocessError):
            failures.append(cid)
    if failures:
        raise RuntimeError('one or more owned containers could not be retired')
    return len(leases)


def main():
    args = sys.argv[1:]
    real = executable()
    # Repository creation sites use the default job endpoint. Explicit Docker
    # global options remain delegated for read/exec/build commands; creation on
    # another endpoint must be registered separately instead of escaping this
    # job's cleanup authority.
    if not args or args[0] not in {'run', 'create'}:
        if any(arg in {'run', 'create'} for arg in args) and args and args[0].startswith('-'):
            raise RuntimeError('container creation must use the job Docker endpoint')
        os.execv(real, [real, *args])
    if any(arg == '--cidfile' or arg.startswith('--cidfile=') for arg in args):
        raise RuntimeError('container creation requires a fresh job-owned cidfile')
    root = Path(os.environ['PINVOU_CI_RESOURCE_LEDGER'])
    if root.is_symlink() or not root.is_dir():
        raise RuntimeError('owned Docker ledger unavailable')
    lease = uuid.uuid4().hex
    path = root / ('docker-' + lease + '.json')
    cid = root / ('docker-' + lease + '.cid')
    value = dict(schema=1, kind='docker', id=lease, state='registered', endpoint=endpoint())
    record(path, value)  # Persist intent before the engine can create anything.
    child = subprocess.Popen([real, args[0], '--cidfile', str(cid), *args[1:]])
    cancelled = 0
    def cancel(signum, _frame):
        nonlocal cancelled
        cancelled = signum
        if child.poll() is None:
            child.send_signal(signum)
    for signum in (signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, cancel)
    code = child.wait()
    if cancelled and code == 0:
        code = 128 + cancelled
    # Creation success does not mean resource retirement. The always() job
    # step verifies/retires all registered IDs, including interrupted wrappers.
    if cid.exists():
        container = identifier(cid)
        def run(args):
            return engine_run(args, os.environ)
        try:
            # Capture while a detached container still exists, before the
            # caller can retire it. Later recovery checks these volume IDs even
            # when a normal finally has already removed the container.
            if container in existing(run):
                value['anonymous_volumes'] = observe_volumes(container, run)
        except (OSError, ValueError, RuntimeError, subprocess.SubprocessError):
            record(path, {**value, 'state': 'unconfirmed', 'exit_code': code, 'cleanup_failed': True})
            raise SystemExit(code if code > 0 else 86)
        record(path, {**value, 'state': 'created', 'exit_code': code})
    else:
        record(path, {**value, 'state': 'unconfirmed', 'exit_code': code})
    raise SystemExit(code if code >= 0 else 128 - code)


if __name__ == '__main__':
    main()

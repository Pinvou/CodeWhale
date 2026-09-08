"""Restricted local backup scheduler; no secret material or raw diagnostics in output."""
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import time

DATABASES = ['account_platform_' + name for name in ('account', 'billing', 'keycloak', 'newapi', 'token')]
CONFIRMED = b'\nPOSTGRES_BACKUP_JOB_EXIT_CONFIRMED\n'
RUNNER = '/opt/pinvou/production/postgres-backup-job.sh'
TMPFS = {'/tmp': 'rw,noexec,nosuid,nodev,size=16m,uid=999,gid=999,mode=1700',
         '/var/run/postgresql': 'rw,noexec,nosuid,nodev,size=8m,uid=999,gid=999,mode=0700'}


class Failure(Exception):
    pass


def require(condition, code):
    if not condition:
        raise Failure(code)


def private(path, directory=False):
    info = path.lstat()
    require(path.is_absolute() and path.resolve(strict=True) == path and not path.is_symlink(), 'UNSAFE_PATH')
    require(info.st_uid == 0 and info.st_gid == 0 and stat.S_IMODE(info.st_mode) == (0o700 if directory else 0o600)
            and (stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)), 'UNSAFE_PERMISSIONS')


def atomic(path, data):
    # Only the validated root-owned state directory can contain temporary files.
    with tempfile.NamedTemporaryFile(dir=path.parent, prefix='.pending-', delete=False) as stream:
        temporary = Path(stream.name)
        os.fchmod(stream.fileno(), 0o600)
        stream.write(json.dumps(data, sort_keys=True).encode('ascii'))
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    descriptor = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def command(argv, timeout=10):
    try:
        result = subprocess.run(argv, capture_output=True, timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise Failure('COMMAND_UNCONFIRMED') from None
    require(result.returncode == 0, 'COMMAND_FAILED')
    return result.stdout


def remaining(deadline, maximum):
    if deadline is None:
        return maximum
    budget = deadline - time.monotonic()
    require(budget > 0, 'MONITOR_DEADLINE_EXCEEDED')
    return min(budget, maximum)


def configuration(path):
    private(path.parent, True)
    private(path)
    require(path.stat().st_size <= 65536, 'CONFIG_TOO_LARGE')
    data = json.loads(path.read_bytes())
    for key, pattern in (('container_id', r'[a-f0-9]{64}'), ('network_id', r'[a-f0-9]{64}'), ('image_id', r'sha256:[a-f0-9]{64}'),
                         ('system_identifier', r'[0-9]{10,30}'), ('project', r'[a-z0-9][a-z0-9_-]{1,63}')):
        require(isinstance(data[key], str) and re.fullmatch(pattern, data[key]), 'INVALID_BINDING')
    for key in ('data_directory', 'repository', 'secrets_directory'):
        target = Path(data[key])
        require(target.is_dir() and target.is_absolute() and target.resolve(strict=True) == target, 'UNSAFE_PATH')
    verify_paths([Path(data[key]) for key in ('data_directory', 'repository', 'secrets_directory')])
    require(isinstance(data['mounts'], list) and data['mounts'], 'INVALID_MOUNTS')
    return data


def verify_paths(paths):
    for index, path in enumerate(paths):
        for other in paths[index + 1:]:
            require(path != other and path not in other.parents and other not in path.parents, 'OVERLAPPING_PATHS')


def verify_container(data, actual):
    host, config, state = actual['HostConfig'], actual['Config'], actual['State']
    require(actual['Id'] == data['container_id'] and actual['Image'] == data['image_id'], 'CONTAINER_MISMATCH')
    require(state['Running'] is True and not state.get('OOMKilled') and state.get('Health', {}).get('Status') == 'healthy',
            'CONTAINER_NOT_HEALTHY')
    require(config['User'] == '999:999' and host['ReadonlyRootfs'] is True and host['Privileged'] is False
            and not host.get('CapAdd') and host.get('CapDrop') == ['ALL'] and not host.get('PortBindings'), 'UNSAFE_CONTAINER')
    require(host.get('SecurityOpt') in (['no-new-privileges'], ['no-new-privileges:true'])
            and host.get('Tmpfs') == TMPFS and host.get('Memory') == 1 << 30
            and host.get('NanoCpus') == 1000000000 and host.get('PidsLimit') == 192, 'UNSAFE_CONTAINER_LIMITS')
    require(any(u['Name'] == 'core' and u['Soft'] == u['Hard'] == 0 for u in host.get('Ulimits', [])), 'CORE_DUMP_ENABLED')
    networks = actual['NetworkSettings']['Networks']
    require(set(networks) == {data['project'] + '_data'}
            and networks[data['project'] + '_data']['NetworkID'] == data['network_id'], 'NETWORK_MISMATCH')
    require(config['Labels'].get('com.docker.compose.project') == data['project']
            and config['Labels'].get('com.docker.compose.service') == 'postgres', 'PROJECT_MISMATCH')
    mounts = [{'source': m['Source'], 'target': m['Destination'], 'read_only': not m['RW']}
              for m in actual['Mounts'] if m['Type'] == 'bind']
    order = lambda items: sorted(items, key=lambda m: m['target'])
    require(order(mounts) == order(data['mounts']) and len({m['target'] for m in mounts}) == len(mounts), 'MOUNT_MISMATCH')
    require(all(m['Type'] in ('bind', 'tmpfs') for m in actual['Mounts']), 'UNEXPECTED_VOLUME')
    require(all(m['Destination'] in TMPFS for m in actual['Mounts'] if m['Type'] == 'tmpfs'), 'UNEXPECTED_TMPFS')
    for source, target, readonly in ((data['data_directory'], '/var/lib/postgresql/data', False),
                                    (data['repository'], '/var/lib/pgbackrest', False),
                                    (data['secrets_directory'], '/run/pinvou/postgres-backup', True)):
        require({'source': source, 'target': target, 'read_only': readonly} in mounts, 'REQUIRED_MOUNT_MISSING')
    require(any(m['target'] == RUNNER and m['read_only'] for m in mounts), 'RUNNER_MOUNT_MISSING')


def invoke(data, state_directory, operation, deadline=None):
    seconds = {'full': 2700, 'incr': 2700, 'create': 45, 'check': 60, 'info': 15, 'stats': 15}[operation]
    if deadline is not None:
        seconds = min(seconds, int(remaining(deadline, 3600)) - 25)
        require(seconds > 0, 'MONITOR_DEADLINE_EXCEEDED')
    pending = state_directory / 'inflight.json'
    require(not pending.exists(), 'EXECUTION_UNCONFIRMED_REQUIRES_REVIEW')
    atomic(pending, {'operation': operation, 'started_at': time.time(), 'container_id': data['container_id']})
    # Container timeout owns its descendants. Host timeout alone NEVER clears the latch.
    try:
        result = subprocess.run(['docker', 'exec', '--user', '999:999', data['container_id'], '/bin/sh', RUNNER,
                                 str(seconds), operation], capture_output=True, timeout=seconds + 25, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise Failure('EXECUTION_UNCONFIRMED_REQUIRES_REVIEW') from None
    require(result.stdout.endswith(CONFIRMED), 'EXECUTION_UNCONFIRMED_REQUIRES_REVIEW')
    pending.unlink()
    descriptor = os.open(state_directory, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    require(result.returncode == 0, 'BACKUP_TOOL_FAILED')
    return result.stdout[:-len(CONFIRMED)]


def recent(value, now, limit):
    require(type(value) in (int, float) and 0 <= now - value <= limit, 'STALE_OR_INVALID_TIME')


def backup_information(raw, now, freshness=True):
    items = json.loads(raw)
    require(len(items) == 1 and items[0]['name'] == 'platform' and items[0]['status']['code'] == 0
            and items[0]['cipher'] == 'aes-256-cbc', 'REPOSITORY_NOT_READY')
    backups = items[0]['backup']
    require(backups and all(not b.get('error', False) for b in backups), 'BACKUP_ERRORS')
    latest = max(backups, key=lambda b: b['timestamp']['stop'])
    full = max((b for b in backups if b['type'] == 'full'), key=lambda b: b['timestamp']['stop'], default=None)
    require(full is not None, 'FULL_BACKUP_MISSING')
    if freshness:
        recent(latest['timestamp']['stop'], now, 26 * 3600)
        recent(full['timestamp']['stop'], now, 8 * 86400)
    require(re.fullmatch(r'\d{8}-\d{6}F(?:_\d{8}-\d{6}[ID])?', latest['label']), 'INVALID_BACKUP_LABEL')
    return {'backup_id': latest['label'], 'last_daily_backup_at': latest['timestamp']['stop'],
            'last_full_at': full['timestamp']['stop']}


def archive_timestamp(value):
    # PostgreSQL omits trailing fractional zeros. Python 3.10 only accepts
    # three or six fractional digits; normalize valid SQL JSON timestamps.
    require(isinstance(value, str), 'INVALID_ARCHIVE_TIME')
    match = re.fullmatch(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,6}))?(Z|[+-]\d{2}:\d{2})', value)
    require(match is not None, 'INVALID_ARCHIVE_TIME')
    fraction = '.' + match[2].ljust(6, '0') if match[2] else ''
    return datetime.fromisoformat(match[1] + fraction + match[3].replace('Z', '+00:00'))


def archive_information(raw, data, now):
    info = json.loads(raw)
    require(info['system_identifier'] == data['system_identifier'], 'CLUSTER_MISMATCH')
    require(info['archive_mode'] == 'on' and info['archive_timeout'] == '30s', 'ARCHIVE_DISABLED')
    require(sorted(info['databases']) == DATABASES, 'DATABASE_SET_MISMATCH')
    require(re.fullmatch(r'[0-9A-F]{24}', info['last_archived_wal'] or ''), 'WAL_MISSING')
    stamp = archive_timestamp(info['last_archived_at'])
    require(stamp.tzinfo is not None, 'INVALID_ARCHIVE_TIME')
    recent(stamp.timestamp(), now, 60)
    if info['last_failed_at'] is not None:
        failed = archive_timestamp(info['last_failed_at'])
        require(failed.tzinfo is not None and failed <= stamp, 'ARCHIVE_FAILED')
    return {'last_wal_archived_at': stamp.timestamp()}


def capacity(path):
    fs = os.statvfs(path)
    require(fs.f_blocks > 0 and fs.f_files > 0, 'CAPACITY_UNKNOWN')
    free, ratio, inode = fs.f_bavail * fs.f_frsize, fs.f_bavail / fs.f_blocks, fs.f_favail / fs.f_files
    require(free >= 2 << 30 and ratio >= .1 and inode >= .1, 'CAPACITY_CRITICAL')
    return {'free_bytes': free, 'free_fraction': ratio, 'inode_free_fraction': inode,
            'warning': free < 5 << 30 or ratio < .2}


def scheduler(deadline=None):
    def call(argv):
        return command(argv, timeout=remaining(deadline, 10))
    for suffix in ('full', 'incr', 'monitor'):
        name = 'pinvou-postgres-backup-' + suffix
        require(call(['systemctl', 'is-enabled', name + '.timer']).strip() == b'enabled', 'TIMER_DISABLED')
        require(call(['systemctl', 'is-active', name + '.timer']).strip() == b'active', 'TIMER_INACTIVE')
        if suffix != 'monitor':
            result = call(['systemctl', 'show', name + '.service', '-p', 'Result', '-p', 'ExecMainStatus'])
            require(set(result.decode('ascii').splitlines()) == {'Result=success', 'ExecMainStatus=0'}, 'SCHEDULED_JOB_FAILED')


def execute(data, state_directory, operation):
    deadline = time.monotonic() + 120 if operation == 'monitor' else None
    def call(action):
        return invoke(data, state_directory, action, deadline)
    verify_container(data, json.loads(command(['docker', 'inspect', data['container_id']], timeout=remaining(deadline, 10)))[0])
    network = json.loads(command(['docker', 'network', 'inspect', data['network_id']], timeout=remaining(deadline, 10)))[0]
    require(network['Id'] == data['network_id'] and network['Internal'] is True, 'NETWORK_NOT_INTERNAL')
    # Verify cluster BEFORE issuing any backup/check that writes repository or WAL.
    info = json.loads(call('stats'))
    require(info['system_identifier'] == data['system_identifier'], 'CLUSTER_MISMATCH')
    for key in ('repository', 'data_directory'):
        capacity(Path(data[key]))
    if operation != 'monitor':
        before = backup_information(call('info'), time.time(), freshness=False)['backup_id']
        call(operation)
    call('check')
    result = backup_information(call('info'), time.time())
    result.update(archive_information(call('stats'), data, time.time()))
    if operation != 'monitor':
        require(result['backup_id'] != before, 'NEW_BACKUP_NOT_CREATED')
    else:
        scheduler(deadline)
    result['capacity'] = {key: capacity(Path(data[key])) for key in ('repository', 'data_directory')}
    remaining(deadline, 1)
    return result


def acquire_lock(lock, wait, fcntl):
    deadline = time.monotonic() + (3200 if wait else 0)
    while True:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return True
        except BlockingIOError:
            remaining = deadline - time.monotonic()
            if not wait or remaining <= 0:
                return False
            time.sleep(min(1, remaining))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=('full', 'incr', 'monitor'))
    parser.add_argument('--config', type=Path, default=Path('/opt/pinvou-postgres-backup/job.json'))
    parser.add_argument('--wait-lock', action='store_true', help='bounded queue for scheduled full/incr only')
    args = parser.parse_args()
    require(not args.wait_lock or args.operation in ('full', 'incr'), 'WAIT_ONLY_FOR_BACKUP')
    require(os.name == 'posix' and os.geteuid() == 0, 'LINUX_ROOT_REQUIRED')
    import fcntl
    state_directory = args.config.parent / 'state'
    private(state_directory, True)
    descriptor = os.open(state_directory / 'job.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, 'a') as lock:
        if not acquire_lock(lock, args.wait_lock, fcntl):
            print('POSTGRES_BACKUP_QUEUE_TIMEOUT' if args.wait_lock else 'POSTGRES_BACKUP_BUSY')
            return 1
        return run_locked(args.config, state_directory, args.operation)


def run_locked(config_path, state_directory, operation):
    status = {'operation': operation, 'status': 'failed', 'notification_delivery': 'local_status_only',
              'lock_acquired_at': datetime.now(timezone.utc).isoformat()}
    try:
        status.update(execute(configuration(config_path), state_directory, operation))
        status['status'] = 'warning' if any(d['warning'] for d in status['capacity'].values()) else 'ok'
    except Failure as error:
        status['error_code'] = str(error)
    except Exception:
        status['error_code'] = 'CHECK_FAILED'
    status['checked_at'] = datetime.now(timezone.utc).isoformat()
    atomic(state_directory / (operation + '.json'), status)
    print(json.dumps(status, sort_keys=True))
    return 0 if status['status'] in ('ok', 'warning') else 1


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception:
        print('POSTGRES_BACKUP_JOB_FAILED')
        raise SystemExit(1) from None

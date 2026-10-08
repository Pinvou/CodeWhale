"""Check recorded runtime credential expiry offline; never connect to a database or read secrets."""
import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile


class Invalid(Exception):
    pass


def require(value):
    if not value:
        raise Invalid()


def timestamp(value):
    require(isinstance(value, str) and re.fullmatch(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z', value))
    return datetime.strptime(value, '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=timezone.utc)


def object_pairs(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result)
        result[key] = value
    return result


def decode(raw):
    require(0 < len(raw) <= 65536)
    return json.loads(raw.decode('utf-8'), object_pairs_hook=object_pairs,
                      parse_constant=lambda _: require(False))


def private(path, directory=False):
    require(path.is_absolute() and path.resolve(strict=True) == path)
    info = path.lstat()
    require(info.st_uid == 0 and info.st_gid == 0
            and stat.S_IMODE(info.st_mode) == (0o700 if directory else 0o600)
            and (stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode))
            and (directory or info.st_nlink == 1))


def read(path):
    private(path.parent, True)
    private(path)
    with open(path, 'rb') as stream:
        info = os.fstat(stream.fileno())
        require(info.st_size <= 65536)
        data = stream.read(65537)
        after = path.stat()
        require((info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns)
                == (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns))
    return data


def evaluate(config, raw, now):
    require(type(config) is dict and set(config) == {'format', 'manifest', 'manifest_sha256', 'runtime_roles'})
    require(config['format'] == 'pinvou-release-expiry-config-v1'
            and isinstance(config['manifest'], str)
            and isinstance(config['manifest_sha256'], str)
            and re.fullmatch('[a-f0-9]{64}', config['manifest_sha256'])
            and hashlib.sha256(raw).hexdigest() == config['manifest_sha256'])
    roles = config['runtime_roles']
    require(type(roles) is list and len(roles) == 2
            and all(isinstance(r, str) and re.fullmatch('[a-z][a-z0-9_]{1,62}', r) for r in roles)
            and roles == sorted(set(roles)))
    data = decode(raw)
    require(type(data) is dict and set(data) == {'format', 'installed_at', 'entries'}
            and data['format'] == 'pinvou-release-expiry-manifest-v1')
    installed = timestamp(data['installed_at'])
    require(now.tzinfo is not None and installed <= now)
    entries = data['entries']
    require(type(entries) is list and len(entries) == 2)
    require(all(type(item) is dict and set(item) == {'role', 'generated_at', 'valid_until'} for item in entries))
    require([item['role'] for item in entries] == roles)
    remaining = []
    for item in entries:
        generated, until = timestamp(item['generated_at']), timestamp(item['valid_until'])
        require(generated <= installed < until and until - generated == timedelta(days=90))
        remaining.append((until - now).total_seconds())
    seconds = min(remaining)
    code = 3 if seconds <= 0 else 2 if seconds <= 7 * 86400 else 1 if seconds <= 14 * 86400 else 0
    return code, {'status': ['OK', 'WARNING', 'CRITICAL', 'EXPIRED'][code],
                  'evidence': 'RECORDED_EXPIRY_ONLY', 'live_database_checked': False,
                  'checked_at': now.isoformat(), 'minimum_remaining_seconds': int(seconds)}


def write_status(path, result):
    private(path.parent, True)
    if path.exists() or path.is_symlink():
        private(path)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=path.parent, prefix='.expiry-', delete=False) as stream:
            temporary = Path(stream.name)
            os.fchmod(stream.fileno(), 0o600)
            stream.write((json.dumps(result, sort_keys=True) + '\n').encode('ascii'))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        descriptor = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    finally:
        if temporary is not None and temporary.exists():
            temporary.unlink()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', type=Path, required=True)
    parser.add_argument('--state', type=Path, required=True)
    args = parser.parse_args(argv)
    os.umask(0o077)
    result = {'status': 'INVALID', 'evidence': 'RECORDED_EXPIRY_ONLY', 'live_database_checked': False}
    code = 4
    safe_state = False
    try:
        require(args.config.is_absolute() and args.state.is_absolute()
                and args.config.name == 'check.json' and args.config.parent.name == 'config'
                and args.state.name == 'latest.json' and args.state.parent.name == 'state'
                and args.state.parent.parent == args.config.parent.parent)
        private(args.state.parent, True)
        safe_state = True
        config = decode(read(args.config))
        manifest = Path(config['manifest'])
        require(manifest == args.config.parent / 'installed.json')
        code, result = evaluate(config, read(manifest), datetime.now(timezone.utc))
    except Exception:
        pass  # Never print raw file contents, exceptions, connection strings, or credentials.
    try:
        require(safe_state)
        write_status(args.state, result)
    except Exception:
        code, result = 4, {'status': 'INVALID', 'evidence': 'RECORDED_EXPIRY_ONLY', 'live_database_checked': False}
    print('RELEASE_CREDENTIAL_EXPIRY_' + result['status'])
    return code


if __name__ == '__main__':
    sys.exit(main())

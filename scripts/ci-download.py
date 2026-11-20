#!/usr/bin/env python3
"""Bounded HTTPS downloads; only independently pinned bytes enter the cache."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import time
from urllib.parse import urlsplit

RESERVE = 2 * 1024**3
MAX_OBJECT = 1024**3
DEFAULT_CAPACITY = 2 * 1024**3


class DownloadError(RuntimeError):
    pass


def digest(path):
    value = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(256 * 1024), b''):
            value.update(block)
    return value.hexdigest()


def event(phase, **fields):
    # No URL paths/query strings, stderr, authorization or proxy addresses.
    print('[ci-download] ' + json.dumps({'phase': phase, **fields}, sort_keys=True), flush=True)


def check_url(url):
    parsed = urlsplit(url)
    if (parsed.scheme != 'https' or not parsed.hostname or parsed.username or parsed.password
            or parsed.fragment or any(c in url for c in '\r\n')):
        raise DownloadError('download source must be credential-free HTTPS')
    return parsed.hostname.lower()


def curl_transfer(url, target, seconds, tls12, max_bytes):
    fields = ('http_code', 'time_namelookup', 'time_connect', 'time_appconnect',
              'time_starttransfer', 'size_download', 'num_redirects')
    template = '{' + ','.join('"' + field + '":"%{' + field + '}"' for field in fields) + '}'
    args = ['curl', '--disable', '--proto', '=https', '--proto-redir', '=https',
            '--location', '--max-redirs', '5', '--silent', '--show-error', '--fail',
            '--tlsv1.2', '--connect-timeout', str(min(15, seconds)),
            '--max-time', str(seconds), '--max-filesize', str(max_bytes),
            '--output', str(target), '--write-out', template]
    if tls12:
        args += ['--tls-max', '1.2']
    args += ['--', url]
    try:
        process = subprocess.run(args, capture_output=True, timeout=seconds + 1, check=False)
    except subprocess.TimeoutExpired:
        return {'exit': 28}
    try:
        data = json.loads(process.stdout)
        receipt = {field: float(data[field]) for field in fields if field in data}
    except (ValueError, KeyError, TypeError):
        receipt = {}
    receipt['exit'] = process.returncode
    # Timings accumulate across redirects. An earlier successful handshake
    # must not conceal a later edge's SSL timeout. Classify locally; never
    # expose curl stderr, which can include credentials or a signed URL.
    error = process.stderr.decode('utf-8', errors='replace').lower()
    if process.returncode and ('ssl connection timeout' in error or
                               (process.returncode == 35 and 'ssl' in error)):
        receipt['failure_stage'] = 'tls-handshake'
    elif process.returncode == 6:
        receipt['failure_stage'] = 'dns'
    elif process.returncode == 7:
        receipt['failure_stage'] = 'tcp-connect'
    return receipt


def regular(path):
    try:
        return stat.S_ISREG(path.lstat().st_mode)
    except FileNotFoundError:
        return False


def publication_lock(cache):
    """A nonblocking OS lock is released even if a downloader crashes."""
    path = cache / '.publication.lock'
    if path.is_symlink():
        return None
    stream = None
    try:
        fd = os.open(path, os.O_RDWR | os.O_CREAT | getattr(os, 'O_NOFOLLOW', 0), 0o600)
        stream = os.fdopen(fd, 'r+b')
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise OSError('invalid cache lock')
        if os.name == 'nt':
            import msvcrt
            if not os.fstat(fd).st_size:
                stream.write(b'\0')
                stream.flush()
            stream.seek(0)
            msvcrt.locking(fd, msvcrt.LK_NBLCK, 1)
        else:
            import fcntl
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return stream
    except OSError:
        if stream:
            stream.close()
        return None


def fetch(url, destination, expected=None, *, cache=None, seconds=180,
          attempts=3, capacity=DEFAULT_CAPACITY, max_bytes=MAX_OBJECT,
          transfer=curl_transfer, sleep=time.sleep, force_tls12=False):
    host = check_url(url)
    if expected is not None and not re.fullmatch('[0-9a-f]{64}', expected):
        raise DownloadError('invalid expected SHA-256')
    if seconds <= 0 or not 1 <= attempts <= 4 or not 0 < max_bytes <= MAX_OBJECT:
        raise DownloadError('invalid transfer budget')
    destination = Path(destination)
    destination.parent.mkdir(parents=True, exist_ok=True)
    if destination.is_symlink():
        raise DownloadError('output must not be a symbolic link')
    stored = None
    if cache and expected:
        cache = Path(cache)
        try:
            cache.mkdir(parents=True, exist_ok=True)
        except OSError:
            event('cache-disabled', reason='unavailable')
            cache = None
    if cache and expected:
        if cache.is_symlink() or not cache.is_dir():
            raise DownloadError('cache root must be a real directory')
        stored = cache / expected
        if regular(stored) and stored.stat().st_size <= max_bytes and digest(stored) == expected:
            if shutil.disk_usage(destination.parent).free - stored.stat().st_size < RESERVE:
                raise DownloadError('cache copy would consume the disk reserve')
            fd, name = tempfile.mkstemp(prefix='.ci-download-', dir=destination.parent)
            os.close(fd)
            temporary = Path(name)
            try:
                shutil.copyfile(stored, temporary)
                if digest(temporary) != expected:
                    raise DownloadError('cached object changed during read')
                os.replace(temporary, destination)
            finally:
                temporary.unlink(missing_ok=True)
            event('cache-hit', host=host, sha256=expected, bytes=destination.stat().st_size)
            return
        if stored.exists() or stored.is_symlink():
            event('cache-rejected', host=host, sha256=expected)
    deadline = time.monotonic() + seconds
    tls12 = force_tls12 or host.endswith('.blob.core.windows.net')
    fd, name = tempfile.mkstemp(prefix='.ci-download-', dir=destination.parent)
    os.close(fd)
    temporary = Path(name)
    try:
        for attempt in range(1, attempts + 1):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise DownloadError('download total budget exhausted')
            if shutil.disk_usage(destination.parent).free - max_bytes < RESERVE:
                raise DownloadError('download would consume the disk reserve')
            temporary.write_bytes(b'')
            result = transfer(url, temporary, remaining, tls12, max_bytes)
            code, status = result.get('exit', -1), int(result.get('http_code', 0))
            event('attempt', host=host, attempt=attempt, tls_policy='1.2' if tls12 else '>=1.2', **result)
            if code == 0:
                if not regular(temporary) or not 0 < temporary.stat().st_size <= max_bytes:
                    raise DownloadError('download has an invalid size')
                actual = digest(temporary)
                if expected and actual != expected:
                    raise DownloadError('download SHA-256 mismatch; refusing cache publication')
                os.replace(temporary, destination)
                event('verified' if expected else 'fresh-metadata', host=host, sha256=actual,
                      bytes=destination.stat().st_size)
                break
            # Certificate errors, authentication, missing releases and malformed
            # URLs never become a retry loop or an insecure TLS fallback.
            transient = code in (5, 6, 7, 18, 28, 35, 52, 55, 56, 92) or status in (408, 429, 500, 502, 503, 504)
            if not transient or attempt == attempts:
                raise DownloadError(f'HTTPS download failed (curl={code}, HTTP={status})')
            if result.get('failure_stage') == 'tls-handshake' or (
                    code in (28, 35) and result.get('time_connect', 0) > 0 and not result.get('time_appconnect', 0)):
                tls12 = True
            wait = min(attempt, max(0, deadline - time.monotonic()))
            sleep(wait)
        if stored:
            lock = publication_lock(cache)
            if lock is None:
                event('cache-not-published', reason='busy-or-unavailable', sha256=expected)
                return
            staged = None
            try:
                used = sum(p.stat().st_size for p in cache.iterdir() if regular(p))
                size = destination.stat().st_size
                replaced = stored.stat().st_size if regular(stored) else 0
                if used - replaced + size > capacity or shutil.disk_usage(cache).free - size < RESERVE:
                    event('cache-not-published', reason='capacity-or-disk-reserve', sha256=expected)
                    return
                fd, name = tempfile.mkstemp(prefix='.publish-', dir=cache)
                os.close(fd)
                staged = Path(name)
                shutil.copyfile(destination, staged)
                if digest(staged) != expected:
                    raise DownloadError('download changed before cache publication')
                os.replace(staged, stored)
                event('cache-published', sha256=expected, bytes=size)
            except OSError:
                event('cache-not-published', reason='busy-or-unavailable', sha256=expected)
            finally:
                if staged:
                    staged.unlink(missing_ok=True)
                lock.close()
    finally:
        temporary.unlink(missing_ok=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('url')
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--sha256')
    cache_root = os.environ.get('RUNNER_TOOL_CACHE')
    parser.add_argument('--cache', default=os.environ.get('PINVOU_CI_DOWNLOAD_CACHE') or
                        (str(Path(cache_root) / 'pinvou-verified-downloads-v1') if cache_root else None))
    parser.add_argument('--seconds', type=float, default=180)
    parser.add_argument('--max-bytes', type=int, default=MAX_OBJECT)
    args = parser.parse_args()
    try:
        fetch(args.url, args.output, args.sha256, cache=args.cache, seconds=args.seconds, max_bytes=args.max_bytes)
    except (DownloadError, OSError) as error:
        # Classifications are safe; urllib/curl raw errors can contain secrets.
        event('failed', error_type=type(error).__name__,
              reason=str(error) if isinstance(error, DownloadError) else 'local-I/O-unavailable')
        raise SystemExit(1)

"""Admit and retire only the Portal Redis service registered by this CI job."""
import argparse
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import uuid
import time

IMAGE = 'redis:8.2-alpine@sha256:fe9daae35cd4fd6ec22416aa4fe0cf6b3e36741e06ff45b9986253fe2a81c041'
SERVICE = 'portal-redis'
OWNER_KEY = 'pinvou:portal-fixture-owner'


def inspect_service(meta, item, healthy=True):
    identifier = meta.get('id', '')
    port = meta.get('ports', {}).get('6379', '')
    if not re.fullmatch('[a-f0-9]{64}', identifier) or item.get('Id') != identifier:
        raise ValueError('Redis service identity mismatch')
    if not isinstance(port, str) or not re.fullmatch('[1-9][0-9]{3,4}', port) or not 1024 <= int(port) <= 65535:
        raise ValueError('Redis service port mismatch')
    if item.get('Config', {}).get('Image') != IMAGE or item.get('State', {}).get('Running') is not True:
        raise ValueError('Redis service image/state mismatch')
    if healthy and item['State'].get('Health', {}).get('Status') != 'healthy':
        raise ValueError('Redis service is not healthy')
    if item.get('NetworkSettings', {}).get('Ports') != {'6379/tcp': [{'HostIp': '127.0.0.1', 'HostPort': port}]}:
        raise ValueError('Redis service must have exactly one loopback binding')
    mounts = item.get('Mounts', [])
    if len(mounts) != 1 or mounts[0].get('Type') != 'volume' or mounts[0].get('Destination') != '/data' or not re.fullmatch('[a-f0-9]{64}', mounts[0].get('Name', '')):
        raise ValueError('Redis requires a fresh anonymous data volume')
    host = item.get('HostConfig', {})
    if host.get('Binds') or host.get('Mounts') or host.get('VolumesFrom') or host.get('AutoRemove') is not True:
        raise ValueError('Redis reusable mount or missing auto-removal')
    return port, mounts[0]['Name']


def run(args, *, timeout=35):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError('owned Redis service operation failed')
    return result.stdout.strip()


def command(port, *args):
    wire = ('*' + str(len(args)) + '\r\n').encode()
    for arg in args:
        value = str(arg).encode()
        wire += b'$' + str(len(value)).encode() + b'\r\n' + value + b'\r\n'
    with socket.create_connection(('127.0.0.1', int(port)), timeout=5) as conn:
        conn.settimeout(5)
        conn.sendall(wire)
        with conn.makefile('rb') as stream:
            line = stream.readline(65537)
            if not line.endswith(b'\r\n') or len(line) > 65536:
                raise ValueError('invalid Redis fixture response')
            if line[:1] == b'$':
                length = int(line[1:-2])
                if not 0 <= length <= 65536:
                    raise ValueError('invalid Redis fixture bulk size')
                body = stream.read(length + 2)
                if len(body) != length + 2 or body[-2:] != b'\r\n':
                    raise ValueError('truncated Redis fixture response')
                return body[:-2].decode()
            if line[:1] in (b'+', b':'):
                return line[1:-2].decode()
            raise ValueError('Redis fixture command refused')


def fields(text):
    return dict(line.split(':', 1) for line in text.splitlines() if ':' in line)


def prove_server(direct, forwarded):
    a, b = fields(direct), fields(forwarded)
    for fact in ('run_id', 'redis_version', 'redis_mode', 'tcp_port'):
        if a.get(fact) != b.get(fact):
            raise ValueError('Redis forwarded endpoint differs from owned service')
    if not re.fullmatch('[a-f0-9]{40}', a.get('run_id', '')) or not a.get('redis_version', '').startswith('8.2.') or a.get('redis_mode') != 'standalone' or a.get('tcp_port') != '6379':
        raise ValueError('unsupported Redis service identity/version')
    return a['run_id']


def save(path, value):
    if path is None:
        return
    tmp = path.with_suffix('.tmp')
    tmp.write_text(json.dumps(value, sort_keys=True), encoding='utf-8')
    os.replace(tmp, path)




def observe_retirement(identifier, volume, *, budget=30):
    # Stop completion and AutoRemove completion are distinct. Observe only the
    # two already-proven identities, with one budget covering subprocess waits.
    deadline = time.monotonic() + budget
    while True:
        observations = []
        for command in [['docker', 'ps', '--all', '--quiet', '--no-trunc'], ['docker', 'volume', 'ls', '--quiet']]:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return False
            observations.append(run(command, timeout=min(5, remaining)).splitlines())
        if identifier not in observations[0] and volume not in observations[1]:
            return True
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return False
        time.sleep(min(0.25, remaining))


def retire_owned_service(meta, path):
    """Diagnostic I/O must never gate retirement after live identity is proven.

    A missing container still requires its valid durable volume receipt. Never
    infer an old volume from a prefix, and never reclaim anything by pruning.
    """
    identifier = meta['id']
    errors = set()
    previous = None
    output_path = path
    if path is not None:
        try:
            previous = json.loads(path.read_text(encoding='utf-8'))
            if not isinstance(previous, dict) or previous.get('schema') != 1 or previous.get('kind') != 'deployment-containers' or previous.get('id') != identifier or not re.fullmatch('[a-f0-9]{64}', previous.get('volume', '')):
                raise ValueError('untrusted Redis receipt')
            # A receipt is an ownership acknowledgement, never a log envelope
            # for arbitrary fields. Preserve only nonsecret, validated identity.
            safe = {key: previous[key] for key in ('schema', 'kind', 'id', 'volume')}
            for key, pattern in [('run_id', '[a-f0-9]{40}'), ('owner_id', 'portal-ci-[a-f0-9]{32}')]:
                value = previous.get(key)
                if isinstance(value, str) and re.fullmatch(pattern, value):
                    safe[key] = value
            previous = safe
        except FileNotFoundError:
            previous = None
        except (OSError, ValueError, TypeError):
            errors.add('RECEIPT_READ_FAILED')
            previous = None
            output_path = None  # Preserve the unreadable/corrupt original.

    def publish(receipt):
        packet = dict(receipt, errors=sorted(errors), receipt_persisted=False)
        if output_path is not None:
            try:
                save(output_path, dict(packet, receipt_persisted=True))
                packet['receipt_persisted'] = True
            except Exception:
                # Keep the error visible after cleanup; neither ENOSPC nor a
                # bookkeeping defect authorizes leaving an owned service alive.
                errors.add('RECEIPT_WRITE_FAILED')
        packet['errors'] = sorted(errors)
        try:
            print(json.dumps(packet, sort_keys=True), flush=True)
        except (OSError, ValueError):
            errors.add('RECEIPT_OUTPUT_FAILED')

    present = identifier in run(['docker', 'ps', '--all', '--quiet', '--no-trunc']).splitlines()
    if not present:
        if previous is None:
            raise ValueError('Redis service disappeared before volume registration')
        if not observe_retirement(identifier, previous['volume']):
            raise ValueError('Redis anonymous volume remains')
        previous.update(state='released', cleanup_failed=False)
        publish(previous)
        if errors:
            raise RuntimeError('Redis retired; diagnostic acknowledgement failed')
        return

    items = json.loads(run(['docker', 'inspect', identifier]))
    if len(items) != 1:
        raise ValueError('ambiguous Redis service')
    _, volume = inspect_service(meta, items[0], healthy=False)
    if previous and previous['volume'] != volume:
        # The live job metadata and exact inspect still identify this service.
        # Do not overwrite the conflicting record or touch its former volume.
        errors.add('RECEIPT_BINDING_MISMATCH')
        output_path = None
    receipt = {'schema': 1, 'kind': 'deployment-containers', 'id': identifier, 'volume': volume, 'state': 'registered'}
    if previous:
        for key in ('run_id', 'owner_id'):
            if key in previous:
                receipt[key] = previous[key]
    publish(receipt)
    try:
        run(['docker', 'stop', '--time', '15', identifier])
    except Exception:
        errors.add('DOCKER_STOP_FAILED')
    clean = False
    try:
        clean = observe_retirement(identifier, volume)
    except Exception:
        errors.add('RETIREMENT_CHECK_FAILED')
    receipt.update(state='released' if clean else 'unconfirmed', cleanup_failed=not clean)
    publish(receipt)
    if errors or not clean:
        raise RuntimeError('Redis retirement or diagnostic acknowledgement failed')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stop', action='store_true')
    args = parser.parse_args()
    if os.environ.get('GITHUB_ACTIONS') != 'true':
        raise RuntimeError('Redis service management requires this CI job context')
    metadata = json.loads(os.environ['ACCOUNT_CI_SERVICES'])
    meta = metadata.get(SERVICE, {})
    identifier = meta.get('id', '')
    if not re.fullmatch('[a-f0-9]{64}', identifier):
        raise ValueError('Redis job service registration unavailable')
    ledger = os.environ.get('PINVOU_CI_RESOURCE_LEDGER', '')
    # Services exist before job steps. Failed ledger initialization must not
    # suppress exact metadata-owned retirement. Admission still requires a
    # durable receipt before it can write any fixture state.
    path = Path(ledger) / 'portal-redis-service.json' if ledger else None
    if not args.stop and path is None:
        raise RuntimeError('Redis admission requires an initialized resource ledger')
    if args.stop:
        retire_owned_service(meta, path)
        return
    previous = json.loads(path.read_text(encoding='utf-8')) if path.exists() else None
    items = json.loads(run(['docker', 'inspect', identifier]))
    if len(items) != 1:
        raise ValueError('ambiguous Redis service')
    port, volume = inspect_service(meta, items[0])
    if previous and (previous.get('id') != identifier or previous.get('volume') != volume):
        raise ValueError('Redis lifecycle receipt differs from job service')
    receipt = {'schema': 1, 'kind': 'deployment-containers', 'id': identifier, 'volume': volume, 'state': 'registered'}
    save(path, receipt)
    direct = run(['docker', 'exec', identifier, 'redis-cli', '--raw', 'INFO', 'server'])
    run_id = prove_server(direct, command(port, 'INFO', 'server'))
    if command(port, 'DBSIZE') != '0' or run(['docker', 'exec', identifier, 'redis-cli', '--raw', 'ACL', 'USERS']) != 'default':
        raise ValueError('Redis service is not a fresh empty fixture')
    owner = 'portal-ci-' + uuid.uuid4().hex
    # Identity checks precede every fixture write. No URL supplied by a caller
    # can select a shared database for configuration, markers or test cleanup.
    for key, value in [('maxmemory', '100663296'), ('maxmemory-policy', 'noeviction'), ('maxclients', '128'), ('save', ''), ('appendonly', 'no')]:
        if command(port, 'CONFIG', 'SET', key, value) != 'OK':
            raise ValueError('Redis fixture configuration refused')
    if command(port, 'SET', OWNER_KEY, owner, 'NX') != 'OK':
        raise ValueError('Redis fixture ownership registration failed')
    values = {'url': f'redis://127.0.0.1:{port}/0', 'port': port, 'run_id': run_id, 'owner_id': owner, 'container_id': identifier}
    with Path(os.environ['GITHUB_OUTPUT']).open('a', encoding='utf-8') as output:
        output.write(''.join(f'{key}={value}\n' for key, value in values.items()))
    receipt.update(run_id=run_id, owner_id=owner)
    save(path, receipt)
    print('Admitted owned Redis 8.2 service; no credentials emitted')


if __name__ == '__main__':
    main()

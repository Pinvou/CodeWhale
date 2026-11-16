"""Admit and stop only this account CI job's disposable Docker PostgreSQL services."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import time
import uuid

SERVICES = {
    'audit': ('audit-export-postgres', 'mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea', '/var/lib/postgresql/data', '16'),
    'broker': ('existing-broker-postgres', 'postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15', '/var/lib/postgresql/18/docker', '18'),
    'financial': ('financial-postgres', 'mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea', '/var/lib/postgresql/data', '16'),
}
FINANCIAL_GROUPS = {"owners": "financial", "token": "financial_token",
                    **{f"billing-{index}": f"financial_billing_{index}" for index in range(4)}}
for key in ("financial_token", *(f"financial_billing_{index}" for index in range(4))):
    original = SERVICES["financial"]
    SERVICES[key] = (key.replace("_", "-") + "-postgres", *original[1:])
CLOSURE_SERVICES = {'financial': SERVICES['financial']}
CORE_SERVICES = {key: SERVICES[key] for key in ('audit', 'broker')}
SERVICE_PROFILES = {'full': SERVICES, 'closure': CLOSURE_SERVICES,
                    'core': CORE_SERVICES, 'financial': CLOSURE_SERVICES}

IDENTITY_SQL = "SELECT system_identifier::text,current_setting('data_directory'),current_setting('port'),session_user,current_user,current_database(),(SELECT rolsuper FROM pg_roles WHERE rolname=session_user),current_setting('server_version_num') FROM pg_control_system()"

def port_value(value):
    if not isinstance(value, str) or not re.fullmatch(r'[1-9][0-9]{3,4}', value) or not 1024 <= int(value) <= 65535:
        raise ValueError('noncanonical service port')
    return value

def inspect_service(meta, item, image, *, require_healthy=True):
    identifier = meta.get('id', '')
    port = port_value(meta.get('ports', {}).get('5432'))
    if not re.fullmatch('[0-9a-f]{64}', identifier) or item.get('Id') != identifier:
        raise ValueError('service ID mismatch')
    if item.get('State', {}).get('Running') is not True or item.get('Config', {}).get('Image') != image:
        raise ValueError('service state/image mismatch')
    if require_healthy and item['State'].get('Health', {}).get('Status') != 'healthy':
        raise ValueError('service health mismatch')
    if item.get('NetworkSettings', {}).get('Ports') != {'5432/tcp': [{'HostIp': '127.0.0.1', 'HostPort': port}]}:
        raise ValueError('service must have exactly one loopback binding')
    mounts = item.get('Mounts', [])
    if len(mounts) != 1 or mounts[0].get('Type') != 'volume' or not re.fullmatch('[0-9a-f]{64}', mounts[0].get('Name', '')) or mounts[0].get('Destination') not in ('/var/lib/postgresql/data', '/var/lib/postgresql'):
        raise ValueError('only a fresh anonymous PG data volume is allowed')
    host = item.get('HostConfig', {})
    if host.get('Binds') or host.get('Mounts') or host.get('VolumesFrom') or host.get('AutoRemove') is not True:
        raise ValueError('reusable mounts or missing anonymous-volume lifecycle')
    return port, mounts[0]['Name']

def run(argv, *, timeout=30, include_stderr=False):
    result = subprocess.run(argv, capture_output=True, text=True, check=False, timeout=timeout)
    if result.returncode:
        raise RuntimeError('account CI service operation failed')
    return (result.stdout + (result.stderr if include_stderr else '')).strip()


def save_receipt(path, value):
    if path is None:
        return
    temporary = path.with_name(path.name + '.' + uuid.uuid4().hex + '.tmp')
    temporary.write_text(json.dumps(value, sort_keys=True), encoding='utf-8')
    os.replace(temporary, path)


def stop_owned_services(metadata, receipt_path=None, *, services=None):
    services = SERVICES if services is None else services
    if os.environ.get('GITHUB_ACTIONS') != 'true':
        raise RuntimeError('service stop requires this job service context')
    # A graceful stop lets Docker AutoRemove remove anonymous data volumes.
    # The runner's later force-rm otherwise leaves those volumes behind.
    # Validate the complete set before stopping anything. Never prune volumes
    # or select containers by a name/image prefix on a shared runner host.
    if not isinstance(metadata, dict):
        save_receipt(receipt_path, {'schema': 1, 'kind': 'postgres-services', 'state': 'unconfirmed', 'reason': 'job service registration unavailable'})
        raise ValueError('job service registration unavailable; no unknown resource will be reclaimed')
    existing = set(run(['docker', 'ps', '--all', '--quiet', '--no-trunc']).splitlines())
    ids, ports, volumes, selected = set(), set(), set(), []
    missing = []
    for _, (name, image, _, _) in services.items():
        meta = metadata.get(name)
        if meta is None:
            missing.append(name)
            continue
        identifier = meta.get('id', '')
        port = port_value(meta.get('ports', {}).get('5432'))
        if not re.fullmatch('[0-9a-f]{64}', identifier) or identifier in ids or port in ports:
            raise ValueError('missing or reused job service identity')
        ids.add(identifier); ports.add(port)
        if identifier not in existing:
            continue  # Already auto-removed; do not guess its former volume.
        items = json.loads(run(['docker', 'inspect', identifier]))
        if len(items) != 1:
            raise ValueError('ambiguous Docker service')
        _, volume = inspect_service(meta, items[0], image, require_healthy=False)
        if volume in volumes:
            raise ValueError('service volume is shared')
        volumes.add(volume)
        selected.append(identifier)
    receipt = {'schema': 1, 'kind': 'postgres-services', 'state': 'registered', 'ids': sorted(ids), 'volumes': sorted(volumes), 'unregistered': missing}
    save_receipt(receipt_path, receipt)
    stop_error = None
    deadline = None
    if selected:
        # AutoRemove also removes container logs. Preserve the diagnostics that
        # the runner normally prints after the job, before retiring the service.
        for identifier in selected:
            print('PostgreSQL service logs: ' + identifier, flush=True)
            try:
                print(run(['docker', 'logs', identifier], include_stderr=True), flush=True)
            except (RuntimeError, subprocess.TimeoutExpired):
                print('::warning::PostgreSQL service log read failed; continuing owned-service cleanup', flush=True)
        # Stop and AutoRemove acknowledgement share the existing 105s budget.
        # A successful stop can precede disappearance from Docker's inventory.
        deadline = time.monotonic() + 105
        try:
            run(['docker', 'stop', '--time', '15', *selected], timeout=105)
        except (RuntimeError, subprocess.TimeoutExpired) as error:
            stop_error = error
    # Command success is not a resource-release acknowledgement. Inspect only
    # the IDs/anonymous volumes admitted above; never prune or guess old names.
    # Pending sets are retained if the budget expires before an observation.
    remaining_ids, remaining_volumes = set(ids), set(volumes)
    clean = False
    try:
        while True:
            def observe(argv):
                budget = 30 if deadline is None else min(30, deadline - time.monotonic())
                if budget <= 0:
                    raise subprocess.TimeoutExpired(argv, 105)
                return run(argv, timeout=budget)
            remaining_ids = ids.intersection(observe(['docker', 'ps', '--all', '--quiet', '--no-trunc']).splitlines())
            remaining_volumes = volumes.intersection(observe(['docker', 'volume', 'ls', '--quiet']).splitlines())
            clean = not remaining_ids and not remaining_volumes and not missing
            if clean or missing or deadline is None:
                break
            budget = deadline - time.monotonic()
            if budget <= 0:
                break
            time.sleep(min(0.25, budget))
    except (RuntimeError, subprocess.TimeoutExpired) as error:
        if stop_error is None:
            stop_error = error
    receipt.update(state='released' if clean else 'unconfirmed', remaining_ids=sorted(remaining_ids), remaining_volumes=sorted(remaining_volumes))
    save_receipt(receipt_path, receipt)
    if stop_error is not None:
        raise RuntimeError('owned PostgreSQL stop failed; postcheck receipt retained') from stop_error
    if not clean:
        raise RuntimeError('owned PostgreSQL retirement was not confirmed; receipt retained')
    print(f'Gracefully stopped {len(selected)} exact job-owned PostgreSQL services; no volume pruning')

def identity_matches(value, directory, major):
    fields = value.split('|')
    if len(fields) != 8 or not re.fullmatch('[1-9][0-9]*', fields[0]) or fields[1:7] != [directory, '5432', 'postgres', 'postgres', 'postgres', 't'] or not fields[7].isdigit() or int(fields[7]) // 10000 != int(major):
        raise ValueError('service PostgreSQL identity mismatch')
    return fields[0]

def profile_metadata(metadata, services, *, portal_redis=False):
    """Split the one declared companion from PG ownership; reject unknowns."""
    names = {item[0] for item in services.values()}
    if portal_redis and services != CORE_SERVICES:
        raise ValueError('Portal Redis companion requires the core PG profile')
    expected = names | ({'portal-redis'} if portal_redis else set())
    if not isinstance(metadata, dict) or set(metadata) != expected:
        raise ValueError('job service set does not match the selected PostgreSQL profile')
    if portal_redis:
        companion = metadata['portal-redis']
        if not isinstance(companion, dict) or not isinstance(companion.get('id'), str) or not re.fullmatch('[0-9a-f]{64}', companion['id']):
            raise ValueError('canonical Portal Redis registration required')
        ports = companion.get('ports')
        if not isinstance(ports, dict) or set(ports) != {'6379'}:
            raise ValueError('canonical Portal Redis port registration required')
        port = port_value(ports['6379'])
        for name in names:
            item = metadata[name]
            if not isinstance(item, dict) or not isinstance(item.get('ports'), dict):
                raise ValueError('canonical core PG registration required')
            if item.get('id') == companion['id'] or item['ports'].get('5432') == port:
                raise ValueError('Portal Redis and PG registrations must be distinct')
    # Redis admission and retirement remain exclusively in account-ci-redis.py.
    # Neither its ID nor its volume enters the PG supervisor's ownership set.
    return {name: metadata[name] for name in metadata if name in names}


def main(*, services=None, portal_redis=False):
    services = SERVICES if services is None else services
    metadata = profile_metadata(json.loads(os.environ['ACCOUNT_CI_SERVICES']), services, portal_redis=portal_redis)
    if not isinstance(metadata, dict) or set(metadata) != {item[0] for item in services.values()}:
        raise ValueError('job service set does not match the selected PostgreSQL profile')
    ids, ports, volumes, systems = set(), set(), set(), set()
    outputs = []
    financial = {}
    for key, (name, image, directory, major) in services.items():
        meta = metadata[name]
        identifier = meta.get('id', '')
        if not re.fullmatch('[0-9a-f]{64}', identifier):
            raise ValueError('missing canonical job service ID')
        items = json.loads(run(['docker', 'inspect', identifier]))
        if len(items) != 1:
            raise ValueError('ambiguous Docker service')
        port, volume = inspect_service(meta, items[0], image)
        url = f'postgres://postgres@127.0.0.1:{port}/postgres?sslmode=disable'
        direct = run(['docker', 'exec', identifier, 'psql', '-U', 'postgres', '-d', 'postgres', '-XAt', '-v', 'ON_ERROR_STOP=1', '-c', IDENTITY_SQL])
        endpoint = run(['psql', url, '-XAt', '-v', 'ON_ERROR_STOP=1', '-c', IDENTITY_SQL])
        system = identity_matches(direct, directory, major)
        if endpoint != direct or identifier in ids or port in ports or volume in volumes or system in systems:
            raise ValueError('service isolation or loopback namespace mismatch')
        ids.add(identifier); ports.add(port); volumes.add(volume); systems.add(system)
        outputs.extend([f'{key}_url={url}', f'{key}_port={port}', f'{key}_system={system}', f'{key}_directory={directory}'])
        if key in FINANCIAL_GROUPS.values():
            group = next(group for group, service in FINANCIAL_GROUPS.items() if service == key)
            financial[group] = dict(url=url, port=port, system=system, directory=directory)
        print(f'Account CI {key}: exact service ID, loopback port, anonymous volume and PostgreSQL identity verified')
    with Path(os.environ['GITHUB_OUTPUT']).open('a', encoding='utf-8') as output:
        output.write('\n'.join(outputs) + '\n')
        output.write('financial_services=' + json.dumps(financial, separators=(',', ':')) + '\n')

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stop-owned-services', action='store_true')
    parser.add_argument('--profile', choices=tuple(SERVICE_PROFILES), default='full')
    parser.add_argument('--with-portal-redis', action='store_true')
    args = parser.parse_args()
    if args.with_portal_redis and args.profile != 'core':
        parser.error('--with-portal-redis requires --profile core')
    services = SERVICE_PROFILES[args.profile]
    if args.stop_owned_services:
        ledger = os.environ.get('PINVOU_CI_RESOURCE_LEDGER')
        stop_owned_services(json.loads(os.environ['ACCOUNT_CI_SERVICES']), Path(ledger) / 'postgres-services.json' if ledger else None, services=services)
    else:
        main(services=services, portal_redis=args.with_portal_redis)

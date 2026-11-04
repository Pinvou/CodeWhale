"""Admit and stop only this account CI job's six disposable Docker PostgreSQL services."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import uuid

SERVICES = {
    'audit': ('audit-export-postgres', 'mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea', '/var/lib/postgresql/data', '16'),
    'broker': ('existing-broker-postgres', 'postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15', '/var/lib/postgresql/18/docker', '18'),
    'financial': ('financial-postgres', 'mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea', '/var/lib/postgresql/data', '16'),
}
FINANCIAL_GROUPS = {"owners": "financial", "token": "financial_token",
                    "billing-0": "financial_billing_0", "billing-1": "financial_billing_1"}
for key in ("financial_token", "financial_billing_0", "financial_billing_1"):
    original = SERVICES["financial"]
    SERVICES[key] = (key.replace("_", "-") + "-postgres", *original[1:])

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


def stop_owned_services(metadata, receipt_path=None):
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
    for _, (name, image, _, _) in SERVICES.items():
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
    if selected:
        # AutoRemove also removes container logs. Preserve the diagnostics that
        # the runner normally prints after the job, before retiring the service.
        for identifier in selected:
            print('PostgreSQL service logs: ' + identifier, flush=True)
            try:
                print(run(['docker', 'logs', identifier], include_stderr=True), flush=True)
            except (RuntimeError, subprocess.TimeoutExpired):
                print('::warning::PostgreSQL service log read failed; continuing owned-service cleanup', flush=True)
        try:
            run(['docker', 'stop', '--time', '15', *selected], timeout=105)
        except (RuntimeError, subprocess.TimeoutExpired) as error:
            stop_error = error
    # Command success is not a resource-release acknowledgement. Inspect only
    # the IDs/anonymous volumes admitted above; never prune or guess old names.
    remaining_ids = ids.intersection(run(['docker', 'ps', '--all', '--quiet', '--no-trunc']).splitlines())
    remaining_volumes = volumes.intersection(run(['docker', 'volume', 'ls', '--quiet']).splitlines())
    clean = not remaining_ids and not remaining_volumes and not missing
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

def main():
    metadata = json.loads(os.environ['ACCOUNT_CI_SERVICES'])
    ids, ports, volumes, systems = set(), set(), set(), set()
    outputs = []
    financial = {}
    for key, (name, image, directory, major) in SERVICES.items():
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
    if parser.parse_args().stop_owned_services:
        ledger = os.environ.get('PINVOU_CI_RESOURCE_LEDGER')
        stop_owned_services(json.loads(os.environ['ACCOUNT_CI_SERVICES']), Path(ledger) / 'postgres-services.json' if ledger else None)
    else:
        main()

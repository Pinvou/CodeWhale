"""Admit only this account CI job's three disposable Docker PostgreSQL services."""
import json
import os
from pathlib import Path
import re
import subprocess

SERVICES = {
    'audit': ('audit-export-postgres', 'postgres:16-alpine', '/var/lib/postgresql/data', '16'),
    'broker': ('existing-broker-postgres', 'postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15', '/var/lib/postgresql/18/docker', '18'),
    'financial': ('financial-postgres', 'mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea', '/var/lib/postgresql/data', '16'),
}
IDENTITY_SQL = "SELECT system_identifier::text,current_setting('data_directory'),current_setting('port'),session_user,current_user,current_database(),(SELECT rolsuper FROM pg_roles WHERE rolname=session_user),current_setting('server_version_num') FROM pg_control_system()"

def port_value(value):
    if not isinstance(value, str) or not re.fullmatch(r'[1-9][0-9]{3,4}', value) or not 1024 <= int(value) <= 65535:
        raise ValueError('noncanonical service port')
    return value

def inspect_service(meta, item, image):
    identifier = meta.get('id', '')
    port = port_value(meta.get('ports', {}).get('5432'))
    if not re.fullmatch('[0-9a-f]{64}', identifier) or item.get('Id') != identifier:
        raise ValueError('service ID mismatch')
    if item.get('State', {}).get('Running') is not True or item['State'].get('Health', {}).get('Status') != 'healthy' or item.get('Config', {}).get('Image') != image:
        raise ValueError('service state/image mismatch')
    if item.get('NetworkSettings', {}).get('Ports') != {'5432/tcp': [{'HostIp': '127.0.0.1', 'HostPort': port}]}:
        raise ValueError('service must have exactly one loopback binding')
    mounts = item.get('Mounts', [])
    if len(mounts) != 1 or mounts[0].get('Type') != 'volume' or not re.fullmatch('[0-9a-f]{64}', mounts[0].get('Name', '')) or mounts[0].get('Destination') not in ('/var/lib/postgresql/data', '/var/lib/postgresql'):
        raise ValueError('only a fresh anonymous PG data volume is allowed')
    host = item.get('HostConfig', {})
    if host.get('Binds') or host.get('Mounts') or host.get('VolumesFrom') or host.get('AutoRemove') is not True:
        raise ValueError('reusable mounts or missing anonymous-volume lifecycle')
    return port, mounts[0]['Name']

def run(argv):
    result = subprocess.run(argv, capture_output=True, text=True, check=False, timeout=30)
    if result.returncode:
        raise RuntimeError('account CI service read-only verification failed')
    return result.stdout.strip()

def identity_matches(value, directory, major):
    fields = value.split('|')
    if len(fields) != 8 or not re.fullmatch('[1-9][0-9]*', fields[0]) or fields[1:7] != [directory, '5432', 'postgres', 'postgres', 'postgres', 't'] or not fields[7].isdigit() or int(fields[7]) // 10000 != int(major):
        raise ValueError('service PostgreSQL identity mismatch')
    return fields[0]

def main():
    metadata = json.loads(os.environ['ACCOUNT_CI_SERVICES'])
    ids, ports, volumes, systems = set(), set(), set(), set()
    outputs = []
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
        print(f'Account CI {key}: exact service ID, loopback port, anonymous volume and PostgreSQL identity verified')
    with Path(os.environ['GITHUB_OUTPUT']).open('a', encoding='utf-8') as output:
        output.write('\n'.join(outputs) + '\n')

if __name__ == '__main__':
    main()

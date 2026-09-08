"""Official admin recovery, only inside the already isolated production fixture."""
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import socket
import ssl
import time
import urllib.parse


def require(value, marker):
    if not value:
        raise RuntimeError(marker)


def remove_bootstrap_settings(text):
    keys = {'bootstrap-admin-username', 'bootstrap-admin-password'}
    seen, retained = set(), []
    for line in text.splitlines(keepends=True):
        key = line.split('=', 1)[0]
        if key in keys:
            require(key not in seen, 'DUPLICATE_BOOTSTRAP_SETTING')
            seen.add(key)
        else:
            retained.append(line)
    require(seen == keys, 'EXACT_BOOTSTRAP_SETTINGS_REQUIRED')
    return ''.join(retained)


def verify(root, kc, pg, network_name, issuer, admin, pg_admin, containers, run, verify_limits, verify_mounts):
    meta = json.loads(run(['docker', 'inspect', kc]).stdout)[0]
    image = meta['Image']
    require(meta['State']['Health']['Status'] == 'healthy' and meta['Config']['User'] == '1000:0', 'RECOVERY_FIXTURE_BOUNDARY')
    mounts = {m['Destination']: m['Source'] for m in meta['Mounts'] if m['Type'] == 'bind'}
    source = Path(mounts['/run/pinvou/keycloak-config']) / 'keycloak.conf'
    require(source.resolve(strict=True) == source and source.is_relative_to(root)
            and source.stat().st_uid == 1000 and source.stat().st_mode & 0o777 == 0o600, 'RECOVERY_CONFIG_BOUNDARY')
    context = ssl.create_default_context(cafile=str(issuer / 'ca.crt'))

    class Connection(http.client.HTTPSConnection):
        def connect(self):
            current = json.loads(run(['docker', 'inspect', kc]).stdout)[0]
            ip = current['NetworkSettings']['Networks'][network_name]['IPAddress']
            self.sock = self._context.wrap_socket(socket.create_connection((ip, 8443), timeout=5), server_hostname='localhost')

    def request(method, path, data=None, token=None, form=False, expected=200):
        conn = Connection('localhost', 8443, context=context, timeout=5)
        headers = {'Content-Type': 'application/x-www-form-urlencoded' if form else 'application/json'}
        if token:
            headers['Authorization'] = 'Bearer ' + token
        try:
            conn.request(method, path, urllib.parse.urlencode(data) if form else json.dumps(data) if data is not None else None, headers)
            response = conn.getresponse()
            body = response.read(1024 * 1024 + 1)
            require(response.status == expected and len(body) <= 1024 * 1024, 'RECOVERY_HTTP_CONTRACT_' + str(response.status))
            return json.loads(body) if body else None
        finally:
            conn.close()

    def login(username, password, expected=200):
        result = request('POST', '/realms/master/protocol/openid-connect/token',
                         {'grant_type': 'password', 'client_id': 'admin-cli', 'username': username, 'password': password},
                         form=True, expected=expected)
        if expected == 400:
            require(result.get('error') == 'invalid_grant', 'REMOVED_ADMIN_LOGIN_NOT_REJECTED')
        return result

    def ready():
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            current = json.loads(run(['docker', 'inspect', kc]).stdout)[0]
            require(current['State']['Running'] and not current['State']['OOMKilled'], 'RECOVERY_ISSUER_START_FAILED')
            if current['State']['Health']['Status'] == 'healthy':
                return
            time.sleep(1)
        raise RuntimeError('RECOVERY_ISSUER_READY_TIMEOUT')

    token = login('fixture-admin', admin)['access_token']
    users = request('GET', '/admin/realms/master/users?username=fixture-admin&exact=true', token=token)
    require(len(users) == 1 and users[0]['username'] == 'fixture-admin', 'RECOVERY_ORIGINAL_ADMIN_AMBIGUOUS')
    original_realms = {r['realm'] for r in request('GET', '/admin/realms', token=token)}
    clients = request('GET', '/admin/realms/pinvou-fixture-workload/clients?clientId=identity-service', token=token)
    require(len(clients) == 1 and clients[0]['clientId'] == 'identity-service', 'RECOVERY_WORKLOAD_CLIENT_AMBIGUOUS')
    workload_secret = request('GET', '/admin/realms/pinvou-fixture-workload/clients/' + clients[0]['id'] + '/client-secret', token=token)['value']

    def workload_token():
        issued = request('POST', '/realms/pinvou-fixture-workload/protocol/openid-connect/token',
                         {'grant_type': 'client_credentials', 'client_id': 'identity-service', 'client_secret': workload_secret}, form=True)
        require('access_token' in issued and 'refresh_token' not in issued, 'RECOVERY_WORKLOAD_GRANT_CHANGED')
        return issued['access_token']

    before_workload = workload_token()
    request('DELETE', '/admin/realms/master/users/' + users[0]['id'], token=token, expected=204)
    login('fixture-admin', admin, 400)
    # Never use the removed administrator for later admin queries.
    updated = remove_bootstrap_settings(source.read_text())
    replacement = source.with_name('retired-keycloak.conf')
    with replacement.open('x') as output:
        os.fchmod(output.fileno(), 0o600)
        os.fchown(output.fileno(), 1000, 0)
        output.write(updated)
        output.flush()
        os.fsync(output.fileno())
    os.replace(replacement, source)
    descriptor = os.open(source.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    require(source.read_text() == updated, 'RECOVERY_CONFIG_REPLACEMENT_FAILED')
    run(['docker', 'stop', '--time', '20', kc])
    require(not json.loads(run(['docker', 'inspect', kc]).stdout)[0]['State']['Running'], 'RECOVERY_OFFLINE_SERVER_RUNNING')
    running = run(['docker', 'ps', '-q', '--no-trunc', '--filter', 'network=' + network_name]).stdout.split()
    require(set(running) == {pg}, 'RECOVERY_OTHER_RUNNING_NODE')
    password = secrets.token_hex(32)
    env_file = root / 'recovery-test.env'
    with env_file.open('x') as output:
        os.fchmod(output.fileno(), 0o600)
        output.write('PINVOU_FIXTURE_RECOVERY_PASSWORD=' + password + '\n')
    label = root.name
    command = ['docker', 'create', '--label', 'pinvou.recovery-fixture=' + label, '--network', network_name,
               '--user', '1000:0', '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
               '--memory', '1536m', '--cpus', '1', '--pids-limit', '256', '--ulimit', 'core=0:0',
               '--log-driver', 'local', '--log-opt', 'max-size=2m', '--log-opt', 'max-file=2',
               '--env-file', str(env_file), '--env', 'JAVA_OPTS_KC_HEAP=-Xms128m -Xmx512m',
               '--tmpfs', '/tmp:rw,nosuid,nodev,size=128m,mode=1777',
               '--tmpfs', '/opt/keycloak/data:rw,nosuid,nodev,size=128m,uid=1000,gid=0,mode=0700']
    bindings = [(mounts[target], target, True) for target in
                ('/run/pinvou/keycloak-config', '/run/pinvou/keycloak-trust', '/run/pinvou/keycloak-tls')]
    for src, dst, _ in bindings:
        command += ['--mount', 'type=bind,src=' + src + ',dst=' + dst + ',readonly']
    arguments = ['--config-file=/run/pinvou/keycloak-config/keycloak.conf', 'bootstrap-admin', 'user', '--optimized',
                 '--username', 'fixture-recovery-admin', '--password:env', 'PINVOU_FIXTURE_RECOVERY_PASSWORD', '--no-prompt']
    cid = run([*command, image, *arguments]).stdout.strip()
    require(re.fullmatch('[a-f0-9]{64}', cid), 'RECOVERY_CONTAINER_ID')
    containers.append((cid, 'pinvou.recovery-fixture', label))
    recovery = json.loads(run(['docker', 'inspect', cid]).stdout)[0]
    require(recovery['Image'] == image and recovery['Config']['User'] == '1000:0'
            and not recovery['HostConfig']['Privileged'] and not recovery['HostConfig'].get('CapAdd')
            and recovery['Config']['Cmd'] == arguments and set(recovery['NetworkSettings']['Networks']) == {network_name}, 'RECOVERY_CONTAINER_CONFIGURATION')
    verify_limits(recovery, 1536 * 1024 * 1024, 256)
    verify_mounts(recovery, bindings, {'/tmp': 'rw,nosuid,nodev,size=128m,mode=1777',
                  '/opt/keycloak/data': 'rw,nosuid,nodev,size=128m,uid=1000,gid=0,mode=0700'})
    run(['docker', 'start', cid])
    run(['docker', 'wait', cid], timeout=180)
    state = json.loads(run(['docker', 'inspect', cid]).stdout)[0]['State']
    require(not state['Running'] and state['ExitCode'] == 0 and not state['OOMKilled'], 'OFFICIAL_RECOVERY_COMMAND_FAILED')
    run(['docker', 'start', kc])
    ready()
    login('fixture-admin', admin, 400)
    recovered = login('fixture-recovery-admin', password)['access_token']
    require({r['realm'] for r in request('GET', '/admin/realms', token=recovered)} == original_realms, 'RECOVERY_REALMS_CHANGED')
    users = request('GET', '/admin/realms/master/users?username=fixture-recovery-admin&exact=true', token=recovered)
    require(len(users) == 1 and users[0]['username'] == 'fixture-recovery-admin', 'RECOVERY_NEW_ADMIN_AMBIGUOUS')
    request('DELETE', '/admin/realms/master/users/' + users[0]['id'], token=recovered, expected=204)
    login('fixture-recovery-admin', password, 400)
    run(['docker', 'stop', '--time', '20', kc])
    run(['docker', 'start', kc])
    ready()
    login('fixture-admin', admin, 400)
    login('fixture-recovery-admin', password, 400)
    after_workload = workload_token()
    query = "SELECT count(*) FROM user_entity WHERE realm_id=(SELECT id FROM realm WHERE name='master') AND username IN ('fixture-admin','fixture-recovery-admin');"
    count = run(['docker', 'exec', '-i', '--user', '999:999', pg, 'psql', '-X', '-At', '-U',
                 'account_platform_admin', '-d', 'account_platform_keycloak', '--set=ON_ERROR_STOP=1'], data=pg_admin + '\n' + query + '\n').stdout.strip()
    require(count == '0', 'RETIRED_ADMINS_RECREATED')
    for target in (kc, cid):
        logs = run(['docker', 'logs', target])
        require(not any(secret in logs.stdout + logs.stderr for secret in
                        (admin, password, workload_secret, token, recovered, before_workload, after_workload)), 'RECOVERY_SECRET_IN_LOG')
    print('KEYCLOAK_OFFICIAL_OFFLINE_RECOVERY_AND_ADMIN_RETIREMENT_PASS', flush=True)

"""Seven-agent Docker/TLS check, invoked only by the isolated Keycloak fixture."""
import base64
import http.client
import json
import os
from pathlib import Path
import re
import socket
import ssl
import subprocess
import time
import urllib.parse

SERVICES = ('account-service', 'identity-service', 'billing-service', 'token-gateway',
            'token-command-executor', 'token-credential-portal', 'keycloak-mobile-authenticator')
AGENT_SERVICES = {service: service + '-workload-agent' for service in SERVICES}


def require(value, marker):
    if not value:
        raise RuntimeError(marker)


def run(argv, env=None, check=True, timeout=30):
    result = subprocess.run(argv, capture_output=True, timeout=timeout, env=env)
    require(not check or result.returncode == 0, 'WORKLOAD_FIXTURE_COMMAND_FAILED')
    return result


def private(path, value, uid):
    with path.open('xb') as stream:
        stream.write(value)
    path.chmod(0o600)
    os.chown(path, uid, 0 if uid == 1000 else uid)


def verify(root, repository, kc, network_name, issuer_files, admin, image, containers):
    require(re.fullmatch(r'sha256:[a-f0-9]{64}', image), 'WORKLOAD_IMAGE_NOT_IMMUTABLE')
    meta = json.loads(run(['docker', 'inspect', kc]).stdout)[0]
    ip = meta['NetworkSettings']['Networks'][network_name]['IPAddress']
    network_id = meta['NetworkSettings']['Networks'][network_name]['NetworkID']
    context = ssl.create_default_context(cafile=str(issuer_files / 'ca.crt'))
    class Connection(http.client.HTTPSConnection):
        def connect(self):
            self.sock = self._context.wrap_socket(socket.create_connection((ip, 8443), timeout=5), server_hostname='localhost')
    def request(method, path, data=None, token=None, form=False):
        conn = Connection('localhost', 8443, context=context, timeout=5)
        headers = {'Content-Type': 'application/x-www-form-urlencoded' if form else 'application/json'}
        if token:
            headers['Authorization'] = 'Bearer ' + token
        try:
            conn.request(method, path, data, headers)
            response = conn.getresponse()
            body = response.read(1024 * 1024 + 1)
            require(response.status in (200, 204) and len(body) <= 1024 * 1024, 'WORKLOAD_FIXTURE_ADMIN_REQUEST_FAILED')
            return json.loads(body) if body else None
        finally:
            conn.close()
    auth = request('POST', '/realms/master/protocol/openid-connect/token',
                   urllib.parse.urlencode({'grant_type': 'password', 'client_id': 'admin-cli',
                                           'username': 'fixture-admin', 'password': admin}), form=True)['access_token']
    realm_path = '/admin/realms/pinvou-fixture-workload'
    request('PUT', realm_path, json.dumps({'attributes': {'frontendUrl': 'https://keycloak:8443'}}), token=auth)
    require(request('GET', realm_path, token=auth)['attributes']['frontendUrl'] == 'https://keycloak:8443',
            'WORKLOAD_PRIVATE_REALM_URL_NOT_SET')
    identities = root / 'identities'
    identities.mkdir(mode=0o700)
    configs, secrets = {}, []
    for service in SERVICES:
        uid = 1000 if service == 'keycloak-mobile-authenticator' else 65532
        clients = request('GET', realm_path + '/clients?clientId=' + service, token=auth)
        require(len(clients) == 1 and clients[0]['clientId'] == service, 'WORKLOAD_CLIENT_NOT_UNIQUE')
        client = clients[0]['id']
        # The preceding Go integration test deliberately disables this disposable
        # client after shortening its token lifetime. Restore only those two
        # known test mutations before exercising the production agent runtime.
        if service == 'account-service':
            state = request('GET', realm_path + '/clients/' + client, token=auth)
            require(state['enabled'] is False and state['attributes']['access.token.lifespan'] == '40',
                    'WORKLOAD_PRECEDING_REVOCATION_STATE_UNEXPECTED')
            state['enabled'] = True
            state['attributes']['access.token.lifespan'] = '300'
            request('PUT', realm_path + '/clients/' + client, json.dumps(state), token=auth)
            restored = request('GET', realm_path + '/clients/' + client, token=auth)
            require(restored['enabled'] is True and restored['attributes']['access.token.lifespan'] == '300',
                    'WORKLOAD_DISPOSABLE_CLIENT_NOT_RESTORED')
        subject = request('GET', realm_path + '/clients/' + client + '/service-account-user', token=auth)['id']
        secret = request('GET', realm_path + '/clients/' + client + '/client-secret', token=auth)['value']
        secrets.append(secret.encode())
        directory = identities / service
        directory.mkdir(mode=0o700)
        for part in ('config', 'bootstrap', 'projected'):
            target = directory / part
            target.mkdir(mode=0o700)
            os.chown(target, uid, 0 if uid == 1000 else uid)
        config = {'issuer': 'https://keycloak:8443/realms/pinvou-fixture-workload',
                  'audience': 'pinvou-account-platform-internal', 'client_id': service, 'subject': subject,
                  'client_secret_file': '/run/bootstrap/client-secret', 'token_file': '/run/projected/token'}
        configs[service] = config
        private(directory / 'config/workload.json', json.dumps(config).encode(), uid)
        private(directory / 'config/ca.crt', (issuer_files / 'ca.crt').read_bytes(), uid)
        private(directory / 'bootstrap/client-secret', secret.encode(), uid)
    require(len({config['subject'] for config in configs.values()}) == 7, 'WORKLOAD_SUBJECTS_NOT_DISTINCT')
    project = root.name + '-agents'
    env = {**os.environ, 'WORKLOAD_AGENT_IMAGE': image, 'WORKLOAD_ROOT': str(identities),
           'WORKLOAD_DATA_NETWORK': network_name}
    compose = ['docker', 'compose', '--project-name', project, '-f',
               str(repository / 'account-platform/deploy/production/compose.workload-agents.yaml')]
    expanded = json.loads(run([*compose, 'config', '--format', 'json'], env=env).stdout)
    require(set(expanded['services']) == set(AGENT_SERVICES.values())
            and not set(expanded['services']).intersection(SERVICES)
            and expanded['networks']['data']['external'] is True
            and expanded['networks']['data']['name'] == network_name, 'WORKLOAD_COMPOSE_SCOPE_MISMATCH')
    run([*compose, 'create'], env=env)
    ids = {}
    for service in SERVICES:
        agent_service = AGENT_SERVICES[service]
        cid = run([*compose, 'ps', '--all', '--quiet', agent_service], env=env).stdout.decode().strip()
        require(re.fullmatch(r'[a-f0-9]{64}', cid), 'WORKLOAD_CONTAINER_ID_INVALID')
        containers.append((cid, 'com.docker.compose.project', project))
        ids[service] = cid
        meta = json.loads(run(['docker', 'inspect', cid]).stdout)[0]
        host, config = meta['HostConfig'], meta['Config']
        expected_user = '1000:0' if service == 'keycloak-mobile-authenticator' else '65532:65532'
        require(meta['Image'] == image and config['User'] == expected_user
                and config['Entrypoint'] == ['/workload-token-agent']
                and config['Cmd'] == ['-config', '/run/config/workload.json'], 'WORKLOAD_IDENTITY_OR_COMMAND_MISMATCH')
        require(not host['Privileged'] and not host.get('CapAdd') and host['CapDrop'] == ['ALL'] and host['ReadonlyRootfs']
                and host['SecurityOpt'] in (['no-new-privileges'], ['no-new-privileges:true'])
                and not host['PortBindings'] and host['Memory'] == 96 * 1024 * 1024
                and host['NanoCpus'] == 250000000 and host['PidsLimit'] == 32
                and any(u['Name'] == 'core' and u['Soft'] == u['Hard'] == 0 for u in host['Ulimits']), 'WORKLOAD_RUNTIME_BOUNDARY')
        require(set(meta['NetworkSettings']['Networks']) == {network_name}, 'WORKLOAD_NETWORK_MISMATCH')
        expected = {(str(identities / service / part), '/run/' + part, part != 'projected')
                    for part in ('config', 'bootstrap', 'projected')}
        require(all(m['Type'] == 'bind' for m in meta['Mounts'])
                and {(m['Source'], m['Destination'], not m['RW']) for m in meta['Mounts']} == expected, 'WORKLOAD_MOUNT_ISOLATION')
        require(not any(secret in json.dumps(config).encode() for secret in secrets), 'WORKLOAD_SECRET_IN_INSPECT_CONFIG')
        run(['docker', 'update', '--restart=no', cid])
        run(['docker', 'start', cid])
        running = json.loads(run(['docker', 'inspect', cid]).stdout)[0]
        require(running['NetworkSettings']['Networks'][network_name]['NetworkID'] == network_id, 'WORKLOAD_ACTIVE_NETWORK_MISMATCH')
    def verified_claims(service):
        cid = ids[service]
        path = identities / service / 'projected/token'
        for _ in range(3):
            before = path.read_bytes()
            checked = run(['docker', 'exec', cid, '/workload-token-agent', '-config', '/run/config/workload.json', '-check'])
            require(not checked.stdout and not checked.stderr, 'WORKLOAD_VERIFIER_UNEXPECTED_OUTPUT')
            if path.read_bytes() == before:
                break
        else:
            raise RuntimeError('WORKLOAD_VERIFIED_TOKEN_CHANGED_REPEATEDLY')
        part = before.split(b'.')[1]
        claims = json.loads(base64.urlsafe_b64decode(part + b'=' * (-len(part) % 4)))
        require(claims['sub'] == configs[service]['subject'] and claims['azp'] == service
                and claims['iss'] == configs[service]['issuer'] and 0 < claims['exp'] - claims['iat'] <= 300,
                'WORKLOAD_ACTUAL_CLAIMS_MISMATCH')
        require(path.stat().st_mode & 0o777 == 0o600, 'WORKLOAD_TOKEN_PERMISSIONS')
        if before not in secrets:
            secrets.append(before)
        return claims
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        if all((identities / service / 'projected/token').is_file() for service in SERVICES):
            break
        time.sleep(1)
    initial = {service: verified_claims(service) for service in SERVICES}
    wrong = {**configs['account-service'], 'subject': configs['identity-service']['subject']}
    private(identities / 'account-service/config/wrong-subject.json', json.dumps(wrong).encode(), 65532)
    denied = run(['docker', 'exec', ids['account-service'], '/workload-token-agent', '-config',
                  '/run/config/wrong-subject.json', '-check'], check=False)
    require(denied.returncode != 0 and b'identity or lifetime invalid' in denied.stderr, 'WORKLOAD_WRONG_SUBJECT_NOT_REJECTED')
    print('WORKLOAD_SEVEN_CONTAINERS_TLS_PROJECTION_AND_WRONG_SUBJECT_PASS', flush=True)
    print('WORKLOAD_WAITING_FOR_REAL_AUTOMATIC_REFRESH', flush=True)
    deadline = time.monotonic() + 190
    while time.monotonic() < deadline:
        current = {service: verified_claims(service) for service in SERVICES}
        if all(current[service]['iat'] > initial[service]['iat'] for service in SERVICES):
            break
        time.sleep(5)
    require(all(current[service]['iat'] > initial[service]['iat'] for service in SERVICES), 'WORKLOAD_AUTOMATIC_REFRESH_NOT_OBSERVED')
    for service, cid in ids.items():
        meta = json.loads(run(['docker', 'inspect', cid]).stdout)[0]
        require(meta['State']['Health']['Status'] == 'healthy', 'WORKLOAD_AGENT_NOT_HEALTHY')
        logs = run(['docker', 'logs', cid])
        require(not any(secret in logs.stdout + logs.stderr for secret in secrets), 'WORKLOAD_SECRET_IN_LOGS')
    print('WORKLOAD_SEVEN_REAL_AUTOMATIC_REFRESH_PASS', flush=True)

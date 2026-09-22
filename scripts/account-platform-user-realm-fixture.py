"""User-realm checks inside an already isolated production Keycloak fixture."""
import base64
import hashlib
import http.client
import importlib.util
import json
import secrets
import socket
import ssl
import subprocess
import urllib.parse


def require(value, marker):
    if not value:
        raise RuntimeError(marker)


def verify(repository, kc, network_name, issuer_files, admin):
    spec = importlib.util.spec_from_file_location('user_realm', repository / 'scripts/account-platform-user-realm.py')
    generator = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(generator)
    inspected = subprocess.run(['docker', 'inspect', kc], capture_output=True, check=True, timeout=15)
    meta = json.loads(inspected.stdout)[0]
    ip = meta['NetworkSettings']['Networks'][network_name]['IPAddress']
    context = ssl.create_default_context(cafile=str(issuer_files / 'ca.crt'))
    class Connection(http.client.HTTPSConnection):
        def connect(self):
            self.sock = self._context.wrap_socket(socket.create_connection((ip, 8443), timeout=5), server_hostname='localhost')
    def request(method, path, data=None, token=None, form=False, expected=(200,), decode=True, pkce_error=None):
        conn = Connection('localhost', 8443, context=context, timeout=5)
        headers = {'Content-Type': 'application/x-www-form-urlencoded' if form else 'application/json'}
        if token:
            headers['Authorization'] = 'Bearer ' + token
        body = urllib.parse.urlencode(data) if form else json.dumps(data) if data is not None else None
        try:
            conn.request(method, path, body, headers)
            response = conn.getresponse()
            raw = response.read(1024 * 1024 + 1)
            if method == 'PUT' and path.endswith('/users/profile') and response.status == 400:
                # This payload contains only the public profile definition, no
                # user data or secrets. Report known validator categories only.
                for marker in (b"doesn't exist", b'incorrect configuration', b'unsupported role',
                               b'Invalid attribute', b'can not be removed', b'Unrecognized',
                               b'username-prohibited-characters', b'up-username-not-idn-homograph',
                               b'person-name-prohibited-characters', b'length', b'email'):
                    if marker in raw:
                        print('USER_PROFILE_PUBLIC_VALIDATION_CATEGORY=' + marker.decode(), flush=True)
            require(response.status in expected and len(raw) <= 1024 * 1024,
                    'USER_REALM_FIXTURE_HTTP_CONTRACT_' + str(response.status))
            if pkce_error is not None:
                callback, state = pkce_error
                location = urllib.parse.urlsplit(response.getheader('Location', ''))
                desired = urllib.parse.urlsplit(callback)
                query = urllib.parse.parse_qs(location.query)
                require((location.scheme, location.netloc, location.path) == (desired.scheme, desired.netloc, desired.path)
                        and not location.fragment and query.get('error') == ['invalid_request']
                        and query.get('state') == [state]
                        and query.get('error_description') in (['Missing parameter: code_challenge'],
                                                               ['Missing parameter: code_challenge_method']),
                        'USER_BROWSER_PKCE_NOT_PRECISELY_REJECTED')
            return json.loads(raw) if raw and decode else raw
        finally:
            conn.close()
    token = request('POST', '/realms/master/protocol/openid-connect/token',
                    {'grant_type': 'password', 'client_id': 'admin-cli', 'username': 'fixture-admin', 'password': admin},
                    form=True)['access_token']
    name = 'pinvou-fixture-production-users'
    bundle = generator.build_bundle('https://account.example.com', 'https://credentials.example.com', name)
    path = '/admin/realms/' + name
    request('GET', path, token=token, expected=(404,))
    request('POST', '/admin/realms', bundle['realm'], token, expected=(201,))
    request('PUT', path + '/users/profile', bundle['user_profile'], token, expected=(200,))
    actual_profile = request('GET', path + '/users/profile', token=token)
    attrs = {a['name']: a for a in actual_profile['attributes']}
    require(attrs['username']['permissions']['edit'] == ['admin']
            and set(attrs['username']['permissions']['view']) == {'admin', 'user'}, 'USER_PROFILE_USERNAME_WRITE_BOUNDARY')
    require(actual_profile.get('unmanagedAttributePolicy') is None, 'USER_PROFILE_UNMANAGED_ATTRIBUTES_ENABLED')
    for attribute in generator.MANAGED_ATTRIBUTES:
        require(attrs[attribute]['permissions'] == {'view': ['admin'], 'edit': ['admin']}, 'USER_PROFILE_BUSINESS_ATTRIBUTE_BOUNDARY')
    for attribute in ('email', 'firstName', 'lastName'):
        require(not attrs[attribute].get('required'), 'USER_PROFILE_EMAIL_OR_NAME_REQUIRED')
    for attribute in ('phone_number', 'phone_number_verified'):
        require(attrs[attribute]['permissions']['edit'] == ['admin']
                and set(attrs[attribute]['permissions']['view']) == {'admin', 'user'}, 'USER_PROFILE_PHONE_WRITE_BOUNDARY')
    discovery = request('GET', '/realms/' + name + '/.well-known/openid-configuration')
    public_issuer = 'https://account.example.com/realms/' + name
    require(discovery['issuer'] == public_issuer
            and discovery['authorization_endpoint'] == public_issuer + '/protocol/openid-connect/auth',
            'USER_REALM_PUBLIC_DISCOVERY_MISMATCH')
    require(request('GET', '/realms/pinvou-fixture-workload/.well-known/openid-configuration')['issuer']
            in ('https://localhost:8443/realms/pinvou-fixture-workload', 'https://keycloak:8443/realms/pinvou-fixture-workload'),
            'USER_REALM_CHANGED_WORKLOAD_ISSUER')
    clients = request('GET', path + '/clients', token=token)
    by_name = {c['clientId']: c for c in clients}
    for desired in bundle['realm']['clients']:
        actual = request('GET', path + '/clients/' + by_name[desired['clientId']]['id'], token=token)
        for key in ('publicClient', 'serviceAccountsEnabled', 'standardFlowEnabled', 'directAccessGrantsEnabled',
                    'implicitFlowEnabled', 'fullScopeAllowed', 'redirectUris', 'webOrigins'):
            require(actual[key] == desired[key], 'USER_REALM_CLIENT_GRANT_OR_CALLBACK_MISMATCH')
        if desired['clientId'] == generator.HANDOFF:
            require(actual.get('clientAuthenticatorType') == 'client-secret',
                    'USER_HANDOFF_CLIENT_AUTHENTICATOR_MISMATCH')
        for key in ('defaultClientScopes', 'optionalClientScopes'):
            if set(actual.get(key, [])) != set(desired[key]):
                print('USER_SCOPE_CLIENT=' + desired['clientId'] + ' FIELD=' + key
                      + ' ACTUAL=' + ','.join(sorted(actual.get(key, [])))
                      + ' EXPECTED=' + ','.join(sorted(desired[key])), flush=True)
            require(set(actual.get(key, [])) == set(desired[key]), 'USER_REALM_CLIENT_SCOPE_MISMATCH')
        require(all(actual['attributes'].get(k) == v for k, v in desired['attributes'].items()), 'USER_REALM_CLIENT_ATTRIBUTE_MISMATCH')
        audience = [m['config'] for m in actual['protocolMappers'] if m['protocolMapper'] == 'oidc-audience-mapper']
        expected_audience = [m['config'] for m in desired['protocolMappers'] if m['protocolMapper'] == 'oidc-audience-mapper']
        require(sorted(audience, key=lambda item: item['included.custom.audience'])
                == sorted(expected_audience, key=lambda item: item['included.custom.audience']),
                'USER_REALM_AUDIENCE_MAPPER_MISMATCH')
    actual_realm = request('GET', path, token=token)
    require(actual_realm['browserFlow'] == 'pinvou-browser'
            and actual_realm['directGrantFlow'] == 'pinvou-mobile-direct-grant'
            and actual_realm['loginWithEmailAllowed'] is False
            and actual_realm['editUsernameAllowed'] is True, 'USER_REALM_AUTHENTICATION_FLOW_MISMATCH')
    for alias, expected in (('pinvou-browser', {'auth-cookie', 'pinvou-browser-authenticator'}),
                            ('pinvou-mobile-direct-grant', {'pinvou-mobile-direct-grant'})):
        executions = request('GET', path + '/authentication/flows/' + alias + '/executions', token=token)
        require({e['providerId'] for e in executions} == expected, 'USER_REALM_AUTHENTICATOR_MISSING')
    print('USER_REALM_PRIVATE_TLS_PUBLIC_DISCOVERY_CALLBACKS_SCOPES_PROFILE_PASS', flush=True)
    client_id = by_name[generator.PROVISIONER]['id']
    user = request('GET', path + '/clients/' + client_id + '/service-account-user', token=token)
    management = by_name['realm-management']['id']
    roles = [request('GET', path + '/clients/' + management + '/roles/' + role, token=token)
             for role in generator.PROVISIONER_ROLES]
    request('POST', path + '/users/' + user['id'] + '/role-mappings/clients/' + management, roles, token, expected=(204,))
    direct_roles = request('GET', path + '/users/' + user['id'] + '/role-mappings/clients/' + management, token=token)
    require({r['name'] for r in direct_roles} == set(generator.PROVISIONER_ROLES), 'USER_PROVISIONER_EXTRA_DIRECT_ROLES')
    actual_roles = request('GET', path + '/users/' + user['id'] + '/role-mappings/clients/' + management + '/composite', token=token)
    if {r['name'] for r in actual_roles} != set(generator.PROVISIONER_ROLES):
        print('USER_PROVISIONER_EFFECTIVE_ROLE_NAMES=' + ','.join(sorted(r['name'] for r in actual_roles)), flush=True)
    require({r['name'] for r in actual_roles} == set(generator.PROVISIONER_ROLES), 'USER_PROVISIONER_EXTRA_MANAGEMENT_ROLES')
    secret = request('GET', path + '/clients/' + client_id + '/client-secret', token=token)['value']
    endpoint = '/realms/' + name + '/protocol/openid-connect/token'
    issued = request('POST', endpoint, {'grant_type': 'client_credentials', 'client_id': generator.PROVISIONER, 'client_secret': secret}, form=True)
    require('refresh_token' not in issued, 'USER_PROVISIONER_REFRESH_TOKEN_ISSUED')
    access = issued['access_token']
    part = access.split('.')[1]
    claims = json.loads(base64.urlsafe_b64decode(part + '=' * (-len(part) % 4)))
    require(claims['iss'] == public_issuer and claims['sub'] == user['id']
            and 0 < claims['exp'] - claims['iat'] <= 300, 'USER_PROVISIONER_TOKEN_BINDING')
    require(set(claims.get('resource_access', {})) == {'realm-management'}
            and set(claims['resource_access']['realm-management']['roles']) == set(generator.PROVISIONER_ROLES),
            'USER_PROVISIONER_TOKEN_EXTRA_PERMISSIONS')
    request('GET', path + '/users?max=1', token=access)
    # A disabled disposable user exercises managed metadata without logging in,
    # sending SMS, touching Account, or inventing production transaction results.
    marker = secrets.token_hex(16)
    attributes = {key: [marker if key != 'pinvou_bootstrap_managed' else 'true'] for key in generator.MANAGED_ATTRIBUTES}
    username = 'fixture-' + marker
    request('POST', path + '/users', {'username': username, 'enabled': False,
                                    'attributes': {**attributes, 'unregistered_fixture_attribute': [marker]}}, access, expected=(201,))
    result = request('GET', path + '/users?username=' + username + '&exact=true', token=access)
    require(len(result) == 1 and result[0]['enabled'] is False
            and all(result[0]['attributes'].get(k) == v for k, v in attributes.items()), 'USER_PROVISIONER_MANAGED_ATTRIBUTES_LOST')
    require('unregistered_fixture_attribute' not in result[0]['attributes'], 'USER_PROFILE_UNMANAGED_ATTRIBUTE_PERSISTED')
    target = path + '/users/' + result[0]['id']
    updated = {**result[0], 'username': username + '-updated'}
    request('PUT', target, updated, access, expected=(204,))
    require(request('GET', target, token=access)['username'] == updated['username'], 'USER_PROVISIONER_UPDATE_FAILED')
    queried = request('GET', path + '/users?q=pinvou_registration_operation_id:' + marker, token=access)
    require(any(u['id'] == result[0]['id'] for u in queried), 'USER_PROVISIONER_OPERATION_QUERY_FAILED')
    password = secrets.token_urlsafe(32)
    request('PUT', target + '/reset-password', {'type': 'password', 'value': password, 'temporary': False}, access, expected=(204,))
    request('POST', target + '/logout', token=access, expected=(204,))
    request('GET', path + '/clients', token=access, expected=(403,))
    request('GET', '/admin/realms/master/users', token=access, expected=(403,))
    elevated = request('GET', path + '/clients/' + management + '/roles/realm-admin', token=token)
    request('POST', path + '/users/' + user['id'] + '/role-mappings/clients/' + management,
            [elevated], access, expected=(403,))
    after_roles = request('GET', path + '/users/' + user['id'] + '/role-mappings/clients/' + management + '/composite', token=token)
    require({r['name'] for r in after_roles} == set(generator.PROVISIONER_ROLES), 'USER_PROVISIONER_ROLES_CHANGED_AFTER_DENIAL')
    for extra, error, description in (
            ({'scope': 'offline_access'}, 'invalid_scope', 'Invalid scopes: offline_access'),
            ({'grant_type': 'password', 'username': username, 'password': 'not-a-real-password'},
             'unauthorized_client', 'Client not allowed for direct access grants'),
            ({'grant_type': 'urn:ietf:params:oauth:grant-type:token-exchange', 'subject_token': access,
              'subject_token_type': 'urn:ietf:params:oauth:token-type:access_token'},
             'invalid_request', 'Standard token exchange is not enabled for the requested client')):
        form = {'grant_type': 'client_credentials', 'client_id': generator.PROVISIONER, 'client_secret': secret, **extra}
        refusal = request('POST', endpoint, form, form=True, expected=(400,))
        require(refusal.get('error') == error and refusal.get('error_description') == description,
                'USER_PROVISIONER_GRANT_NOT_PRECISELY_REJECTED')
    # The handoff client is a dedicated confidential identity. Desktop tokens
    # explicitly include its audience for claim-shape validation. Keycloak's
    # standard exchange does not use subject audience as an authorization
    # boundary, so the Portal must validate /v1/me before using this secret.
    handoff_id = by_name[generator.HANDOFF]['id']
    handoff_secret = request('GET', path + '/clients/' + handoff_id + '/client-secret', token=token)['value']
    request('PUT', target, {**updated, 'enabled': True}, access, expected=(204,))
    # The production mobile direct-grant SPI depends on the separate Identity
    # service. This isolated Keycloak fixture temporarily selects Keycloak's
    # built-in password flow only to mint a real user token for the unchanged
    # desktop client, then immediately restores the production flow.
    request('PUT', path, {'directGrantFlow': 'direct grant'}, token, expected=(204,))
    desktop_tokens = request('POST', endpoint, {'grant_type': 'password', 'client_id': generator.DESKTOP,
                                                'username': updated['username'], 'password': password,
                                                'scope': 'openid profile email offline_access'}, form=True)
    request('PUT', path, {'directGrantFlow': 'pinvou-mobile-direct-grant'}, token, expected=(204,))
    desktop_access = desktop_tokens['access_token']
    desktop_part = desktop_access.split('.')[1]
    desktop_claims = json.loads(base64.urlsafe_b64decode(desktop_part + '=' * (-len(desktop_part) % 4)))
    desktop_audience = desktop_claims.get('aud', [])
    if isinstance(desktop_audience, str):
        desktop_audience = [desktop_audience]
    require(generator.DESKTOP in desktop_audience and generator.HANDOFF in desktop_audience,
            'USER_DESKTOP_HANDOFF_AUDIENCE_MISSING')
    exchange_form = {
        'grant_type': 'urn:ietf:params:oauth:grant-type:token-exchange',
        'subject_token': desktop_access,
        'subject_token_type': 'urn:ietf:params:oauth:token-type:access_token',
        'scope': 'openid',
    }
    unauthenticated = request('POST', endpoint, {'client_id': generator.HANDOFF, **exchange_form},
                              form=True, expected=(401,))
    require(unauthenticated.get('error') in ('invalid_client', 'unauthorized_client'),
            'USER_HANDOFF_CLIENT_ACCEPTED_WITHOUT_SECRET')
    wrong_secret = request('POST', endpoint, {'client_id': generator.HANDOFF, 'client_secret': 'wrong', **exchange_form},
                           form=True, expected=(401,))
    require(wrong_secret.get('error') in ('invalid_client', 'unauthorized_client'),
            'USER_HANDOFF_CLIENT_ACCEPTED_WRONG_SECRET')
    unauthenticated_workload = request('POST', endpoint, {'client_id': generator.HANDOFF,
                                                         **{**exchange_form, 'subject_token': access}},
                                       form=True, expected=(401,))
    require(unauthenticated_workload.get('error') in ('invalid_client', 'unauthorized_client'),
            'USER_PROVISIONER_BORROWED_HANDOFF_IDENTITY')
    portal_exchange = request('POST', endpoint, {'client_id': generator.HANDOFF,
                                                 'client_secret': handoff_secret, **exchange_form},
                              form=True, expected=(200, 400))
    if portal_exchange.get('error'):
        description = portal_exchange.get('error_description', '').lower()
        categories = [marker for marker in ('audience', 'requested token type', 'scope', 'not allowed',
                                             'subject token', 'refresh token') if marker in description]
        print('USER_HANDOFF_EXCHANGE_ERROR=' + portal_exchange.get('error', 'unknown')
              + ' CATEGORY=' + ','.join(categories)
              + ' DESCRIPTION=' + description[:240].replace('\n', ' '), flush=True)
    require(portal_exchange.get('access_token') and not portal_exchange.get('refresh_token')
            and portal_exchange.get('token_type') == 'Bearer', 'USER_PORTAL_TOKEN_EXCHANGE_FAILED')
    exchanged_part = portal_exchange['access_token'].split('.')[1]
    exchanged_claims = json.loads(base64.urlsafe_b64decode(exchanged_part + '=' * (-len(exchanged_part) % 4)))
    exchanged_audience = exchanged_claims.get('aud', [])
    if isinstance(exchanged_audience, str):
        exchanged_audience = [exchanged_audience]
    require(exchanged_claims.get('azp') == generator.HANDOFF and generator.DESKTOP in exchanged_audience
            and exchanged_claims.get('sub') == desktop_claims.get('sub'),
            'USER_PORTAL_TOKEN_EXCHANGE_CLIENT_MISMATCH')
    for public_client in (generator.PORTAL, generator.CREDENTIAL):
        public_exchange = request('POST', endpoint, {'client_id': public_client, **exchange_form},
                                  form=True, expected=(400,))
        require(public_exchange.get('error') == 'invalid_request'
                and public_exchange.get('error_description') == 'Standard token exchange is not enabled for the requested client'
                and not public_exchange.get('access_token') and not public_exchange.get('refresh_token'),
                'USER_PUBLIC_PORTAL_TOKEN_EXCHANGE_NOT_REJECTED_' + public_client)
    for client in (generator.PORTAL, generator.HANDOFF, generator.CREDENTIAL):
        refusal = request('POST', endpoint, {'grant_type': 'password', 'client_id': client,
                                            'username': username, 'password': 'not-a-real-password'}, form=True, expected=(400, 401))
        require(refusal.get('error') == 'unauthorized_client', 'USER_BROWSER_PASSWORD_GRANT_NOT_DISABLED')
    verifier = secrets.token_urlsafe(32)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip('=')
    for client, callback in ((generator.PORTAL, 'https://account.example.com/auth/callback'),
                             (generator.CREDENTIAL, 'https://credentials.example.com/oidc/callback')):
        query = {'client_id': client, 'response_type': 'code', 'redirect_uri': callback,
                 'scope': 'openid', 'state': secrets.token_urlsafe(24), 'nonce': secrets.token_urlsafe(24),
                 'code_challenge': challenge, 'code_challenge_method': 'S256'}
        auth_path = '/realms/' + name + '/protocol/openid-connect/auth?'
        request('GET', auth_path + urllib.parse.urlencode(query), decode=False)
        request('GET', auth_path + urllib.parse.urlencode({**query, 'redirect_uri': callback + '/wrong'}), expected=(400,), decode=False)
        without_pkce = {k: v for k, v in query.items() if not k.startswith('code_challenge')}
        # 26.7.0 redirects a PKCE error only after validating the callback.
        # Do not follow that redirect or accept an unrelated error response.
        request('GET', auth_path + urllib.parse.urlencode(without_pkce), expected=(302,), decode=False,
                pkce_error=(callback, query['state']))
    request('DELETE', target, token=access, expected=(204,))
    request('GET', target, token=access, expected=(404,))
    logs = subprocess.run(['docker', 'logs', kc], capture_output=True, check=True, timeout=15)
    require(not any(value.encode() in logs.stdout + logs.stderr
                    for value in (admin, secret, handoff_secret, access, desktop_access, password)),
            'USER_REALM_SECRET_IN_ISSUER_LOGS')
    print('USER_REALM_PROVISIONER_REAL_PERMISSIONS_ESCALATION_DENIED_AND_PKCE_PASS', flush=True)

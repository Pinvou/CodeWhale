"""Emit a secret-free production user-realm bootstrap bundle; never contact Keycloak."""
import argparse
import ipaddress
import json
from pathlib import Path
import re
from urllib.parse import urlsplit

DESKTOP = 'pinvou-desktop'
PORTAL = 'pinvou-portal'
HANDOFF = 'pinvou-portal-handoff'
CREDENTIAL = 'pinvou-credentials'
PROVISIONER = 'pinvou-account-provisioner'
PROVISIONER_ROLES = ('manage-users', 'view-users', 'query-users')
CREDENTIAL_ORIGINS = ('https://credentials.pinvou.com', 'https://credentials-uat.pinvou.com')
PROFILE_SOURCE = Path(__file__).resolve().parents[1] / 'account-platform/deploy/keycloak/provision-mobile-auth.sh'
EVENT_TYPES = ('LOGIN', 'LOGIN_ERROR', 'LOGOUT', 'REGISTER', 'REGISTER_ERROR',
               'UPDATE_PASSWORD', 'UPDATE_PASSWORD_ERROR', 'REFRESH_TOKEN', 'REFRESH_TOKEN_ERROR')
REALM_BASELINE = {
    'bruteForceProtected': True, 'permanentLockout': False, 'failureFactor': 10,
    'maxFailureWaitSeconds': 900, 'minimumQuickLoginWaitSeconds': 60,
    'waitIncrementSeconds': 60, 'quickLoginCheckMilliSeconds': 1000,
    'maxDeltaTimeSeconds': 43200, 'accessTokenLifespan': 600,
    'revokeRefreshToken': True, 'refreshTokenMaxReuse': 0,
    'eventsEnabled': True, 'eventsExpiration': 2592000,
    'adminEventsEnabled': True, 'adminEventsDetailsEnabled': False,
}


def origin(value):
    if not isinstance(value, str) or len(value) > 253 or re.search(r'[\s\\\x00-\x1f\x7f]', value):
        raise ValueError('invalid public origin')
    parsed = urlsplit(value)
    if (parsed.scheme != 'https' or parsed.username is not None or parsed.password is not None
            or parsed.query or parsed.fragment or parsed.path not in ('', '/')
            or not parsed.hostname or parsed.hostname != parsed.netloc
            or not re.fullmatch(r'[a-z0-9]+(?:[a-z0-9.-]*[a-z0-9])?', parsed.hostname)
            or '.' not in parsed.hostname or '..' in parsed.hostname
            or any(not label or len(label) > 63 or label.startswith('-') or label.endswith('-')
                   for label in parsed.hostname.split('.'))):
        raise ValueError('invalid public origin')
    try:
        ipaddress.ip_address(parsed.hostname)
    except ValueError:
        return 'https://' + parsed.hostname
    raise ValueError('invalid public origin')


def identifier(value):
    if not isinstance(value, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,63}', value):
        raise ValueError('invalid identifier')
    return value


def _unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('invalid profile source')
        result[key] = value
    return result


def user_profile():
    # The existing online provisioner owns this contract, including identity
    # security fences. Read its one bounded literal rather than fork that policy.
    with PROFILE_SOURCE.open('rb') as handle:
        raw = handle.read(131073)
    if len(raw) > 131072:
        raise ValueError('invalid profile source')
    source = raw.decode('utf-8')
    marker = "<<'PINVOU_MOBILE_USER_PROFILE'\n"
    if source.count(marker) != 1:
        raise ValueError('invalid profile source')
    remainder = source.split(marker, 1)[1]
    end = '\nPINVOU_MOBILE_USER_PROFILE\n'
    if remainder.count(end) != 1:
        raise ValueError('invalid profile source')
    profile = json.loads(remainder.split(end, 1)[0], object_pairs_hook=_unique_object,
                         parse_constant=lambda _: (_ for _ in ()).throw(ValueError('invalid profile source')))
    if not isinstance(profile, dict) or not isinstance(profile.get('attributes'), list):
        raise ValueError('invalid profile source')
    return profile


def audience_mapper(name, audience):
    return {'name': name, 'protocol': 'openid-connect', 'protocolMapper': 'oidc-audience-mapper',
            'config': {'included.client.audience': audience, 'access.token.claim': 'true'}}


def phone_mappers():
    return [{'name': 'pinvou-' + name.replace('_', '-'), 'protocol': 'openid-connect',
             'protocolMapper': 'oidc-usermodel-attribute-mapper',
             'config': {'user.attribute': name, 'claim.name': name, 'jsonType.label': kind,
                        'id.token.claim': 'true', 'access.token.claim': 'true', 'userinfo.token.claim': 'true'}}
            for name, kind in (('phone_number', 'String'), ('phone_number_verified', 'boolean'))]


def client(name, *, public=True, standard=False, direct=False, service=False):
    return {'clientId': name, 'enabled': True, 'protocol': 'openid-connect',
            'publicClient': public, 'bearerOnly': False, 'standardFlowEnabled': standard,
            'directAccessGrantsEnabled': direct, 'implicitFlowEnabled': False,
            'serviceAccountsEnabled': service, 'fullScopeAllowed': False,
            'redirectUris': [], 'webOrigins': [], 'defaultClientScopes': [], 'optionalClientScopes': [],
            'attributes': {'standard.token.exchange.enabled': 'false'}, 'protocolMappers': []}


def authentication_flows():
    return [
        {'alias': 'pinvou-browser',
         'description': 'Pinvou browser cookie, password, SMS, verified mobile registration, and password recovery',
         'providerId': 'basic-flow', 'topLevel': True, 'builtIn': False,
         'authenticationExecutions': [
             {'authenticator': provider, 'authenticatorFlow': False, 'requirement': 'ALTERNATIVE',
              'priority': priority, 'userSetupAllowed': False}
             for provider, priority in (('auth-cookie', 10), ('pinvou-browser-authenticator', 20))]},
        {'alias': 'pinvou-mobile-direct-grant', 'description': 'Pinvou mobile password or SMS direct grant',
         'providerId': 'basic-flow', 'topLevel': True, 'builtIn': False,
         'authenticationExecutions': [{'authenticator': 'pinvou-mobile-direct-grant', 'authenticatorFlow': False,
                                       'requirement': 'REQUIRED', 'priority': 10, 'userSetupAllowed': False}]},
    ]


def build_bundle(account_origin, credential_origin=None, name='pinvou', *, handoff=False,
                 desktop_client=DESKTOP, portal_client=PORTAL, handoff_client=HANDOFF,
                 credential_client=CREDENTIAL, provisioner_client=PROVISIONER, account_audience=None):
    account = origin(account_origin)
    realm_name = identifier(name)
    if realm_name == 'master' or realm_name.endswith('-workload'):
        raise ValueError('a separate user realm is required')
    ids = [identifier(value) for value in (desktop_client, portal_client, handoff_client,
                                           credential_client, provisioner_client)]
    if len(set(ids)) != len(ids) or type(handoff) is not bool:
        raise ValueError('distinct clients and explicit handoff flag are required')
    audience = identifier(account_audience if account_audience is not None else desktop_client)
    credential = origin(credential_origin) if credential_origin is not None else None
    if credential is not None and (credential not in CREDENTIAL_ORIGINS or credential == account):
        raise ValueError('credential origin is not approved or distinct')

    desktop = client(desktop_client, direct=True)
    desktop['defaultClientScopes'] = ['basic', 'roles', 'profile', 'email']
    desktop['optionalClientScopes'] = ['offline_access']
    desktop['attributes']['pkce.code.challenge.method'] = 'S256'
    desktop['protocolMappers'] = [audience_mapper('pinvou-desktop-audience', audience), *phone_mappers()]
    portal = client(portal_client, standard=True)
    portal['redirectUris'] = [account + '/auth/callback']
    portal['defaultClientScopes'] = ['basic', 'roles', 'profile', 'email']
    portal['attributes']['pkce.code.challenge.method'] = 'S256'
    portal['protocolMappers'] = [audience_mapper('pinvou-account-audience', audience)]
    clients = [desktop, portal]
    if handoff:
        exchange = client(handoff_client, public=False)
        exchange['clientAuthenticatorType'] = 'client-secret'
        exchange['defaultClientScopes'] = ['basic', 'roles']
        exchange['attributes'] = {'standard.token.exchange.enabled': 'true',
                                  'standard.token.exchange.enableRefreshRequestedTokenType': 'false'}
        exchange['protocolMappers'] = [audience_mapper('pinvou-account-audience', audience)]
        desktop['protocolMappers'].append(audience_mapper('pinvou-portal-handoff-audience', handoff_client))
        clients.append(exchange)
    if credential:
        credentials = client(credential_client, standard=True)
        credentials['redirectUris'] = [credential + '/oidc/callback']
        credentials['defaultClientScopes'] = ['basic', 'roles', 'profile']
        credentials['attributes']['pkce.code.challenge.method'] = 'S256'
        credentials['protocolMappers'] = [audience_mapper('pinvou-credential-audience', credential_client)]
        clients.append(credentials)
    provisioner = client(provisioner_client, public=False, service=True)
    provisioner['clientAuthenticatorType'] = 'client-secret'
    provisioner['defaultClientScopes'] = ['service_account']
    provisioner['protocolMappers'] = [audience_mapper('pinvou-realm-management-audience', 'realm-management'), {
        'name': 'user-provisioning-roles', 'protocol': 'openid-connect',
        'protocolMapper': 'oidc-usermodel-client-role-mapper',
        'config': {'usermodel.clientRoleMapping.clientId': 'realm-management',
                   'claim.name': 'resource_access.realm-management.roles', 'jsonType.label': 'String',
                   'multivalued': 'true', 'access.token.claim': 'true', 'id.token.claim': 'false'}}]
    clients.append(provisioner)
    realm = {'realm': realm_name, 'enabled': True, 'displayName': 'Pinvou', 'sslRequired': 'all',
             'loginTheme': 'pinvou', 'internationalizationEnabled': True,
             'supportedLocales': ['zh-CN', 'en', 'ja'], 'defaultLocale': 'zh-CN',
             'registrationAllowed': False, 'resetPasswordAllowed': False, 'rememberMe': True,
             'registrationEmailAsUsername': False, 'loginWithEmailAllowed': False,
             'editUsernameAllowed': True, 'defaultSignatureAlgorithm': 'RS256',
             **REALM_BASELINE, 'enabledEventTypes': list(EVENT_TYPES),
             'defaultDefaultClientScopes': [], 'defaultOptionalClientScopes': [],
             'browserFlow': 'pinvou-browser', 'directGrantFlow': 'pinvou-mobile-direct-grant',
             'authenticationFlows': authentication_flows(), 'clients': clients,
             # A scope mapping is not a service-account role assignment. The
             # existing controlled provisioner resolves its real UUID and assigns
             # exactly these roles after installation; no generated users/secrets.
             'clientScopeMappings': {'realm-management': [{'client': provisioner_client,
                                                           'roles': list(PROVISIONER_ROLES)}]}}
    return {'realm': realm, 'user_profile': user_profile(), 'provisioner_roles': list(PROVISIONER_ROLES)}


class SafeParser(argparse.ArgumentParser):
    def error(self, message):
        # Arbitrary rejected arguments can contain accidentally pasted secrets.
        self.exit(2, 'USER_REALM_CONFIGURATION_INVALID\n')


def main(argv=None):
    parser = SafeParser(description=__doc__)
    parser.add_argument('--account-origin', required=True, help='canonical public HTTPS Portal origin')
    parser.add_argument('--credential-origin', help='explicit approved credential origin; omitted disables this client')
    parser.add_argument('--realm', default='pinvou')
    parser.add_argument('--handoff', action='store_true', help='include the existing access-only exchange client')
    for option, default in [('desktop-client', DESKTOP), ('portal-client', PORTAL), ('handoff-client', HANDOFF),
                            ('credential-client', CREDENTIAL), ('provisioner-client', PROVISIONER)]:
        parser.add_argument('--' + option, default=default)
    parser.add_argument('--account-audience')
    args = parser.parse_args(argv)
    try:
        bundle = build_bundle(args.account_origin, args.credential_origin, args.realm, handoff=args.handoff,
                              desktop_client=args.desktop_client, portal_client=args.portal_client,
                              handoff_client=args.handoff_client, credential_client=args.credential_client,
                              provisioner_client=args.provisioner_client, account_audience=args.account_audience)
        print(json.dumps(bundle, ensure_ascii=False, indent=2))
    except (ValueError, OSError, UnicodeError):
        parser.error('invalid configuration')


if __name__ == '__main__':
    main()

"""Emit secret-free production user realm and managed profile; no network writes."""
import argparse
import ipaddress
import json
import re
from urllib.parse import urlsplit

DESKTOP = 'pinvou-desktop'
PORTAL = 'pinvou-token-portal'
CREDENTIAL = 'pinvou-token-credential-portal'
PROVISIONER = 'pinvou-account-provisioner'
PROVISIONER_ROLES = ('manage-users',)
MANAGED_ATTRIBUTES = ('pinvou_bootstrap_managed', 'pinvou_registration_operation_id',
                      'pinvou_profile_change_operation_id')


def origin(value):
    parsed = urlsplit(value)
    if (parsed.scheme != 'https' or not parsed.hostname or parsed.username is not None
            or parsed.password is not None or parsed.port is not None or parsed.path not in ('', '/')
            or parsed.query or parsed.fragment or parsed.hostname != parsed.netloc
            or not re.fullmatch(r'[a-z0-9]+(?:[a-z0-9.-]*[a-z0-9])?', parsed.hostname)
            or '.' not in parsed.hostname or '..' in parsed.hostname):
        raise ValueError('a canonical public HTTPS origin without port or path is required')
    try:
        ipaddress.ip_address(parsed.hostname)
    except ValueError:
        return 'https://' + parsed.hostname
    raise ValueError('an IP address is not a public issuer origin')


def audience_mapper(audience):
    return {'name': 'pinvou-application-audience', 'protocol': 'openid-connect',
            'protocolMapper': 'oidc-audience-mapper', 'consentRequired': False,
            'config': {'included.custom.audience': audience, 'access.token.claim': 'true',
                       'id.token.claim': 'false', 'userinfo.token.claim': 'false', 'introspection.token.claim': 'true'}}


def phone_mappers():
    return [{'name': 'pinvou-' + name.replace('_', '-'), 'protocol': 'openid-connect',
             'protocolMapper': 'oidc-usermodel-attribute-mapper', 'consentRequired': False,
             'config': {'user.attribute': name, 'claim.name': name, 'jsonType.label': kind,
                        'id.token.claim': 'true', 'access.token.claim': 'true', 'userinfo.token.claim': 'true'}}
            for name, kind in (('phone_number', 'String'), ('phone_number_verified', 'boolean'))]


def public_client(name, *, callback=None, desktop=False, credential=False):
    return {
        'clientId': name, 'name': name, 'enabled': True, 'protocol': 'openid-connect',
        'publicClient': True, 'bearerOnly': False, 'serviceAccountsEnabled': False,
        'standardFlowEnabled': callback is not None, 'directAccessGrantsEnabled': desktop,
        'implicitFlowEnabled': False, 'fullScopeAllowed': False,
        'redirectUris': [callback] if callback else [], 'webOrigins': [],
        'defaultClientScopes': ['basic'] if credential else ['basic', 'profile', 'email'],
        'optionalClientScopes': [] if credential else ['offline_access'],
        'attributes': {'pkce.code.challenge.method': 'S256', 'access.token.signed.response.alg': 'RS256',
                       'use.refresh.tokens': 'false' if credential else 'true',
                       'client_credentials.use_refresh_token': 'false',
                       'oauth2.device.authorization.grant.enabled': 'false',
                       'oidc.ciba.grant.enabled': 'false', 'standard.token.exchange.enabled': 'false'},
        'protocolMappers': [audience_mapper(CREDENTIAL if credential else DESKTOP),
                            *([] if credential else phone_mappers())],
    }


def user_profile():
    def attribute(name, *, admin_only=False, phone=False):
        return {'name': name, 'multivalued': False,
                'permissions': {'view': ['admin'] if admin_only else ['admin', 'user'],
                                'edit': ['admin'] if admin_only or phone else ['admin', 'user']}}
    username = attribute('username')
    username['permissions']['edit'] = ['admin']
    username['validations'] = {'length': {'min': 3, 'max': 255}, 'username-prohibited-characters': {},
                               'up-username-not-idn-homograph': {}}
    email = attribute('email')
    email['validations'] = {'email': {}, 'length': {'max': 255}}
    names = [attribute(name) for name in ('firstName', 'lastName')]
    for item in names:
        item['validations'] = {'length': {'max': 255}, 'person-name-prohibited-characters': {}}
    phone = attribute('phone_number', phone=True)
    phone['validations'] = {'length': {'max': 16}}
    # Keycloak 26.7.0 represents disabled by null. Its Java enum contains
    # ENABLED/ADMIN_VIEW/ADMIN_EDIT only; the UI label is not a wire value.
    return {'unmanagedAttributePolicy': None,
            'attributes': [username, email, *names, phone, attribute('phone_number_verified', phone=True),
                           *[attribute(name, admin_only=True) for name in MANAGED_ATTRIBUTES]],
            'groups': []}


def build_bundle(account_origin, credential_origin, name='pinvou'):
    account, credential = origin(account_origin), origin(credential_origin)
    if account == credential:
        raise ValueError('account and credential origins must be distinct')
    if not re.fullmatch(r'[a-z0-9][a-z0-9-]{0,63}', name) or name == 'master' or name.endswith('-workload'):
        raise ValueError('a separate bounded user realm is required')
    provisioner = {
        'clientId': PROVISIONER, 'name': PROVISIONER, 'enabled': True, 'protocol': 'openid-connect',
        'clientAuthenticatorType': 'client-secret', 'publicClient': False, 'bearerOnly': False,
        'serviceAccountsEnabled': True, 'standardFlowEnabled': False, 'directAccessGrantsEnabled': False,
        'implicitFlowEnabled': False, 'fullScopeAllowed': False, 'redirectUris': [], 'webOrigins': [],
        # 26.7.0 attaches this built-in scope to service-account clients. Make
        # that explicit, while keeping all user/offline scopes unattached.
        'defaultClientScopes': ['service_account'], 'optionalClientScopes': [],
        'attributes': {'access.token.lifespan': '300', 'access.token.signed.response.alg': 'RS256',
                       'use.refresh.tokens': 'false', 'client_credentials.use_refresh_token': 'false',
                       'oauth2.device.authorization.grant.enabled': 'false', 'oidc.ciba.grant.enabled': 'false',
                       'standard.token.exchange.enabled': 'false'},
        'protocolMappers': [audience_mapper('realm-management'), {
            'name': 'user-provisioning-roles', 'protocol': 'openid-connect',
            'protocolMapper': 'oidc-usermodel-client-role-mapper', 'consentRequired': False,
            'config': {'usermodel.clientRoleMapping.clientId': 'realm-management',
                       'claim.name': 'resource_access.realm-management.roles', 'jsonType.label': 'String',
                       'multivalued': 'true', 'access.token.claim': 'true', 'id.token.claim': 'false'}}],
    }
    flows = [
        {'alias': 'pinvou-browser', 'description': 'Pinvou browser authentication',
         'providerId': 'basic-flow', 'topLevel': True, 'builtIn': False,
         'authenticationExecutions': [
             {'authenticator': provider, 'authenticatorFlow': False, 'requirement': 'ALTERNATIVE',
              'priority': priority, 'userSetupAllowed': False}
             for provider, priority in (('auth-cookie', 10), ('pinvou-browser-authenticator', 20))]},
        {'alias': 'pinvou-mobile-direct-grant', 'description': 'Pinvou mobile password or SMS authentication',
         'providerId': 'basic-flow', 'topLevel': True, 'builtIn': False,
         'authenticationExecutions': [{'authenticator': 'pinvou-mobile-direct-grant', 'authenticatorFlow': False,
                                       'requirement': 'REQUIRED', 'priority': 10, 'userSetupAllowed': False}]},
    ]
    realm = {
        'realm': name, 'enabled': True, 'displayName': 'Pinvou', 'sslRequired': 'all',
        'attributes': {'frontendUrl': account}, 'loginTheme': 'pinvou',
        'internationalizationEnabled': True, 'supportedLocales': ['zh-CN', 'en', 'ja'], 'defaultLocale': 'zh-CN',
        'registrationAllowed': False, 'registrationEmailAsUsername': False, 'loginWithEmailAllowed': False,
        'editUsernameAllowed': True,
        'verifyEmail': False, 'resetPasswordAllowed': False, 'rememberMe': True, 'bruteForceProtected': True,
        'accessTokenLifespan': 600, 'defaultSignatureAlgorithm': 'RS256',
        'revokeRefreshToken': True, 'refreshTokenMaxReuse': 0,
        # Retain built-in scope definitions, but never implicitly attach the
        # realm-wide defaults to the credential or provisioning clients.
        'defaultDefaultClientScopes': [], 'defaultOptionalClientScopes': [],
        'browserFlow': 'pinvou-browser', 'directGrantFlow': 'pinvou-mobile-direct-grant',
        'authenticationFlows': flows,
        'clients': [public_client(DESKTOP, desktop=True),
                    public_client(PORTAL, callback=account + '/auth/callback'),
                    public_client(CREDENTIAL, callback=credential + '/oidc/callback', credential=True), provisioner],
        # This restricts which roles may enter the provisioner token. A separate
        # audited Admin API operation assigns these roles to its real UUID.
        'clientScopeMappings': {'realm-management': [{'client': PROVISIONER, 'roles': list(PROVISIONER_ROLES)}]},
    }
    return {'realm': realm, 'user_profile': user_profile(),
            'provisioner_roles': list(PROVISIONER_ROLES)}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--account-origin', required=True)
    parser.add_argument('--credential-origin', required=True)
    parser.add_argument('--realm', default='pinvou')
    args = parser.parse_args()
    try:
        print(json.dumps(build_bundle(args.account_origin, args.credential_origin, args.realm), indent=2))
    except ValueError as error:
        parser.error(str(error))

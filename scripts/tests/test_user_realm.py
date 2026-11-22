"""Offline generator contracts against the current provisioner; no real Keycloak."""
import importlib.util
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / 'scripts/account-platform-user-realm.py'
SPEC = importlib.util.spec_from_file_location('production_user_realm', SCRIPT)
realm = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(realm)
CURRENT = (ROOT / 'account-platform/deploy/keycloak/provision-mobile-auth.sh').read_text(encoding='utf8')


def clients(bundle):
    return {client['clientId']: client for client in bundle['realm']['clients']}


def current_client(name):
    match = re.search(name + r'_json=\$\(cat <<JSON\n(.*?)\nJSON\n\)', CURRENT, re.S)
    if not match:
        raise AssertionError('current producer literal not found: ' + name)
    return json.loads(match.group(1))


class UserRealmContract(unittest.TestCase):
    def complete(self, **changes):
        return realm.build_bundle('https://account.example.com', 'https://credentials.pinvou.com',
                                  handoff=True, **changes)

    def test_default_minimal_current_layout(self):
        bundle = realm.build_bundle('https://account.example.com')
        self.assertEqual(['realm', 'user_profile', 'provisioner_roles'], list(bundle))
        self.assertEqual({'pinvou-desktop', 'pinvou-portal', 'pinvou-account-provisioner'}, set(clients(bundle)))
        self.assertNotIn('attributes', bundle['realm'])  # hostname/issuer belongs to deployment, not Portal URL.
        self.assertNotIn('users', bundle['realm'])
        self.assertNotIn('roles', bundle['realm'])

    def test_portal_exact_current_online_contract(self):
        actual = clients(self.complete())['pinvou-portal']
        expected = current_client('portal')
        expected['clientId'] = 'pinvou-portal'
        expected['redirectUris'] = ['https://account.example.com/auth/callback']
        for key, value in expected.items():
            self.assertEqual(value, actual[key], key)
        self.assertNotIn('use.refresh.tokens', actual['attributes'])  # keep ordinary online session policy.
        self.assertNotIn('offline_access', actual['defaultClientScopes'] + actual['optionalClientScopes'])

    def test_desktop_existing_direct_and_offline_contract(self):
        desktop = clients(self.complete())['pinvou-desktop']
        self.assertTrue(desktop['directAccessGrantsEnabled'])
        self.assertFalse(desktop['standardFlowEnabled'])
        self.assertEqual([], desktop['redirectUris'])
        self.assertFalse(desktop['serviceAccountsEnabled'])
        self.assertEqual(['offline_access'], desktop['optionalClientScopes'])
        self.assertEqual('false', desktop['attributes']['standard.token.exchange.enabled'])
        self.assertIn("-s 'optionalClientScopes=[\"offline_access\"]'", CURRENT)
        self.assertEqual({'phone_number', 'phone_number_verified'},
                         {m['config']['user.attribute'] for m in desktop['protocolMappers']
                          if m['protocolMapper'] == 'oidc-usermodel-attribute-mapper'})

    def test_handoff_exact_current_opt_in_access_only(self):
        actual = clients(self.complete())['pinvou-portal-handoff']
        expected = current_client('handoff')
        expected.pop('secret')
        expected['clientId'] = 'pinvou-portal-handoff'
        for key, value in expected.items():
            self.assertEqual(value, actual[key], key)
        self.assertEqual('false', actual['attributes']['standard.token.exchange.enableRefreshRequestedTokenType'])
        self.assertNotIn('secret', actual)
        desktop = clients(self.complete())['pinvou-desktop']
        self.assertIn('pinvou-portal-handoff', [m['config'].get('included.client.audience') for m in desktop['protocolMappers']])
        disabled = clients(realm.build_bundle('https://account.example.com'))['pinvou-desktop']
        self.assertNotIn('pinvou-portal-handoff', [m['config'].get('included.client.audience') for m in disabled['protocolMappers']])

    def test_credential_exact_current_limited_contract(self):
        for origin in ['https://credentials.pinvou.com', 'https://credentials-uat.pinvou.com']:
            with self.subTest(origin=origin):
                actual = clients(realm.build_bundle('https://account.example.com', origin))['pinvou-credentials']
                expected = current_client('credential')
                expected['clientId'] = 'pinvou-credentials'
                expected['redirectUris'] = [origin + '/oidc/callback']
                for key, value in expected.items():
                    self.assertEqual(value, actual[key], key)
                self.assertNotIn('offline_access', actual['optionalClientScopes'])
                self.assertEqual('pinvou-credentials', actual['protocolMappers'][0]['config']['included.client.audience'])
        for origin in ['https://credential.example.com', 'https://credentials.pinvou.com/path',
                       'https://credentials.pinvou.com:443', 'https://account.example.com']:
            with self.subTest(reject=origin):
                with self.assertRaises(ValueError):
                    realm.build_bundle('https://account.example.com', origin)

    def test_profile_exact_current_fences(self):
        raw = CURRENT.split("<<'PINVOU_MOBILE_USER_PROFILE'\n", 1)[1].split('\nPINVOU_MOBILE_USER_PROFILE\n', 1)[0]
        self.assertEqual(json.loads(raw), self.complete()['user_profile'])
        attributes = {a['name']: a for a in self.complete()['user_profile']['attributes']}
        for key in ['pinvou_bootstrap_managed', 'pinvou_registration_operation_id',
                    'pinvou_identity_security_operation_id', 'pinvou_identity_security_fence_version',
                    'pinvou_password_reset_operation_id', 'pinvou_password_reset_fence_version']:
            with self.subTest(attribute=key):
                self.assertEqual({'view': ['admin'], 'edit': ['admin']}, attributes[key]['permissions'])
        self.assertNotIn('pinvou_profile_change_operation_id', attributes)
        self.assertEqual(['admin'], attributes['phone_number']['permissions']['edit'])
        self.assertNotIn('required', attributes['email'])

    def test_provisioner_existing_roles_no_administrator_assignment(self):
        bundle = self.complete()
        self.assertEqual(['manage-users', 'view-users', 'query-users'], bundle['provisioner_roles'])
        self.assertIn('for role in manage-users view-users query-users; do', CURRENT)
        service = clients(bundle)['pinvou-account-provisioner']
        self.assertTrue(service['serviceAccountsEnabled'])
        self.assertFalse(service['standardFlowEnabled'])
        self.assertFalse(service['directAccessGrantsEnabled'])
        self.assertFalse(service['fullScopeAllowed'])
        self.assertEqual(['service_account'], service['defaultClientScopes'])
        self.assertEqual([], service['optionalClientScopes'])
        self.assertEqual({'realm-management': [{'client': 'pinvou-account-provisioner',
                         'roles': ['manage-users', 'view-users', 'query-users']}]}, bundle['realm']['clientScopeMappings'])
        text = json.dumps(bundle)
        for forbidden in ['platform-admin', 'realm-admin', 'client-secret-value', 'service-account-']:
            self.assertNotIn(forbidden, text)
        for value in clients(bundle).values():
            self.assertNotIn('secret', value)

    def test_current_realm_hardening_and_audit(self):
        current = self.complete()['realm']
        expected = {'bruteForceProtected': True, 'permanentLockout': False, 'failureFactor': 10,
                    'maxFailureWaitSeconds': 900, 'minimumQuickLoginWaitSeconds': 60,
                    'waitIncrementSeconds': 60, 'quickLoginCheckMilliSeconds': 1000,
                    'maxDeltaTimeSeconds': 43200, 'accessTokenLifespan': 600,
                    'revokeRefreshToken': True, 'refreshTokenMaxReuse': 0,
                    'eventsEnabled': True, 'eventsExpiration': 2592000,
                    'adminEventsEnabled': True, 'adminEventsDetailsEnabled': False}
        for key, value in expected.items():
            with self.subTest(field=key):
                self.assertEqual(value, current[key])
                serialized = str(value).lower() if type(value) is bool else str(value)
                self.assertIn(key + '=' + serialized, CURRENT)
        self.assertEqual(['LOGIN', 'LOGIN_ERROR', 'LOGOUT', 'REGISTER', 'REGISTER_ERROR',
                          'UPDATE_PASSWORD', 'UPDATE_PASSWORD_ERROR', 'REFRESH_TOKEN', 'REFRESH_TOKEN_ERROR'],
                         current['enabledEventTypes'])
        self.assertNotIn('ssoSessionMaxLifespan', current)
        self.assertNotIn('offlineSessionMaxLifespan', current)

    def test_original_explicit_production_identity_baseline(self):
        generated = self.complete()['realm']
        self.assertIs(False, generated['registrationEmailAsUsername'])
        self.assertIs(False, generated['loginWithEmailAllowed'])
        self.assertIs(True, generated['editUsernameAllowed'])
        self.assertEqual('RS256', generated['defaultSignatureAlgorithm'])

    def test_existing_spi_flows(self):
        generated = self.complete()['realm']
        self.assertEqual('pinvou-browser', generated['browserFlow'])
        self.assertEqual('pinvou-mobile-direct-grant', generated['directGrantFlow'])
        flows = {f['alias']: f for f in generated['authenticationFlows']}
        self.assertEqual([('auth-cookie', 'ALTERNATIVE'), ('pinvou-browser-authenticator', 'ALTERNATIVE')],
                         [(e['authenticator'], e['requirement']) for e in flows['pinvou-browser']['authenticationExecutions']])
        self.assertEqual([('pinvou-mobile-direct-grant', 'REQUIRED')],
                         [(e['authenticator'], e['requirement']) for e in flows['pinvou-mobile-direct-grant']['authenticationExecutions']])
        self.assertFalse(generated['registrationAllowed'])
        self.assertFalse(generated['resetPasswordAllowed'])
        self.assertEqual(['zh-CN', 'en', 'ja'], generated['supportedLocales'])

    def test_custom_current_identifiers_and_audiences(self):
        bundle = self.complete(desktop_client='desktop-prod', portal_client='portal-prod',
                               handoff_client='handoff-prod', credential_client='credential-prod',
                               provisioner_client='provisioner-prod', account_audience='account-prod')
        self.assertEqual({'desktop-prod', 'portal-prod', 'handoff-prod', 'credential-prod', 'provisioner-prod'}, set(clients(bundle)))
        for name in ['desktop-prod', 'portal-prod', 'handoff-prod']:
            self.assertIn('account-prod', [m['config'].get('included.client.audience') for m in clients(bundle)[name]['protocolMappers']])
        self.assertEqual('credential-prod', clients(bundle)['credential-prod']['protocolMappers'][0]['config']['included.client.audience'])
        self.assertEqual('provisioner-prod', bundle['realm']['clientScopeMappings']['realm-management'][0]['client'])

    def test_invalid_origin_fail_closed(self):
        bad = ['http://account.example.com', 'https://user:canary@account.example.com',
               'https://account.example.com:443', 'https://account.example.com/path',
               'https://account.example.com?x=canary', 'https://account.example.com#canary',
               'https://ACCOUNT.example.com', 'https://127.0.0.1', 'https://[::1]',
               'https://localhost', 'https://a..example.com', 'https://a-.example.com',
               'https://-a.example.com', 'https://account.example.com\n',
               'https://account.example.com\\anything', 'https://' + 'a' * 64 + '.example.com']
        for value in bad:
            with self.subTest(origin=value):
                with self.assertRaises(ValueError):
                    realm.build_bundle(value)
        self.assertEqual(realm.build_bundle('https://account.example.com'), realm.build_bundle('https://account.example.com/'))

    def test_invalid_realm_and_colliding_clients_fail_closed(self):
        for value in ['master', 'pinvou-workload', '', 'bad realm', '../file', 'x' * 65]:
            with self.subTest(realm=value):
                with self.assertRaises(ValueError):
                    realm.build_bundle('https://account.example.com', name=value)
        for key in ['desktop_client', 'handoff_client', 'credential_client', 'provisioner_client']:
            with self.subTest(collision=key):
                with self.assertRaises(ValueError):
                    self.complete(**{key: 'pinvou-portal'})
        with self.assertRaises(ValueError):
            realm.build_bundle('https://account.example.com', handoff='true')

    def test_profile_source_malformed_missing_or_oversize_rejected(self):
        with tempfile.TemporaryDirectory(prefix='user-realm-profile-') as directory:
            source = Path(directory) / 'provision.sh'
            bad = ['no profile', "<<'PINVOU_MOBILE_USER_PROFILE'\n{\"attributes\":[],\"attributes\":[]}\nPINVOU_MOBILE_USER_PROFILE\n",
                   "<<'PINVOU_MOBILE_USER_PROFILE'\n{\"attributes\":NaN}\nPINVOU_MOBILE_USER_PROFILE\n", 'x' * 131073]
            for content in bad:
                with self.subTest(source=len(content)):
                    source.write_text(content, encoding='utf8')
                    with patch.object(realm, 'PROFILE_SOURCE', source), self.assertRaises(ValueError):
                        self.complete()
            with patch.object(realm, 'PROFILE_SOURCE', Path(directory) / 'absent'), self.assertRaises(OSError):
                self.complete()

    def test_offline_function_never_connects(self):
        with patch.object(socket, 'socket', side_effect=AssertionError('network forbidden')):
            self.assertEqual(5, len(clients(self.complete())))

    def test_actual_cli_complete_bundle_deterministic(self):
        args = [sys.executable, str(SCRIPT), '--account-origin', 'https://account.example.com',
                '--credential-origin', 'https://credentials.pinvou.com', '--handoff']
        result = subprocess.run(args, capture_output=True, text=True, encoding='utf8', check=False)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual('', result.stderr)
        self.assertEqual(self.complete(), json.loads(result.stdout))
        again = subprocess.run(args, capture_output=True, check=False)
        self.assertEqual(result.stdout.encode('utf8').replace(b'\r\n', b'\n'), again.stdout.replace(b'\r\n', b'\n'))

    def test_actual_cli_rejects_without_echo_or_partial_bundle(self):
        for args in [['--account-origin', 'https://canary-secret@account.example.com'],
                     ['--account-origin', 'https://account.example.com', '--secret', 'canary-secret'],
                     ['--account-origin', 'https://account.example.com', '--realm', 'master']]:
            with self.subTest(args=args[0]):
                result = subprocess.run([sys.executable, str(SCRIPT), *args], capture_output=True, text=True, check=False)
                self.assertEqual(2, result.returncode)
                self.assertEqual('', result.stdout)
                self.assertEqual('USER_REALM_CONFIGURATION_INVALID\n', result.stderr)
                self.assertNotIn('canary-secret', result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()

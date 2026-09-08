import importlib.util
import json
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('user_realm', Path(__file__).resolve().parents[1] / 'account-platform-user-realm.py')
realm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(realm)


class UserRealmTests(unittest.TestCase):
    def bundle(self):
        return realm.build_bundle('https://account.example.com', 'https://credentials.example.com')

    def test_no_test_users_secrets_or_workload_clients(self):
        value = self.bundle()['realm']
        self.assertNotIn('users', value)
        self.assertNotIn('roles', value)
        self.assertEqual(len(value['clients']), 4)
        self.assertEqual([c['clientId'] for c in value['clients'] if c['serviceAccountsEnabled']], [realm.PROVISIONER])
        for client in value['clients']:
            self.assertNotIn('secret', client)
            self.assertNotIn('id', client)
        self.assertFalse(value['loginWithEmailAllowed'])
        self.assertTrue(value['editUsernameAllowed'])
        self.assertEqual(value['defaultDefaultClientScopes'], [])
        self.assertEqual(value['defaultOptionalClientScopes'], [])
        self.assertEqual(value['attributes']['frontendUrl'], 'https://account.example.com')

    def test_callbacks_audiences_and_existing_scope_compatibility(self):
        desktop, portal, credential, provisioner = self.bundle()['realm']['clients']
        self.assertTrue(desktop['directAccessGrantsEnabled'])
        self.assertFalse(desktop['standardFlowEnabled'])
        self.assertEqual(desktop['redirectUris'], [])
        self.assertEqual(portal['redirectUris'], ['https://account.example.com/auth/callback'])
        self.assertEqual(credential['redirectUris'], ['https://credentials.example.com/oidc/callback'])
        for client in (portal, credential):
            self.assertFalse(client['directAccessGrantsEnabled'])
            self.assertEqual(client['attributes']['pkce.code.challenge.method'], 'S256')
        for client in (desktop, portal):
            self.assertEqual(client['optionalClientScopes'], ['offline_access'])
            self.assertEqual(client['defaultClientScopes'], ['basic', 'profile', 'email'])
            self.assertEqual(client['protocolMappers'][0]['config']['included.custom.audience'], realm.DESKTOP)
        self.assertEqual(credential['optionalClientScopes'], [])
        self.assertEqual(credential['protocolMappers'][0]['config']['included.custom.audience'], realm.CREDENTIAL)
        for client in (credential, provisioner):
            self.assertEqual(client['attributes']['use.refresh.tokens'], 'false')
        self.assertEqual(provisioner['defaultClientScopes'], ['service_account'])
        self.assertEqual(provisioner['optionalClientScopes'], [])
        self.assertEqual(self.bundle()['provisioner_roles'], ['manage-users'])

    def test_managed_business_attributes_and_optional_email(self):
        profile = self.bundle()['user_profile']
        self.assertIsNone(profile['unmanagedAttributePolicy'])
        attrs = {a['name']: a for a in profile['attributes']}
        self.assertEqual(attrs['username']['permissions'], {'view': ['admin', 'user'], 'edit': ['admin']})
        for name in realm.MANAGED_ATTRIBUTES:
            self.assertEqual(attrs[name]['permissions'], {'view': ['admin'], 'edit': ['admin']})
        for name in ('phone_number', 'phone_number_verified'):
            self.assertEqual(attrs[name]['permissions']['edit'], ['admin'])
        for name in ('email', 'firstName', 'lastName'):
            self.assertNotIn('required', attrs[name])

    def test_only_canonical_distinct_public_origins_and_user_realm(self):
        for bad in ('http://account.example.com', 'https://user@account.example.com', 'https://127.0.0.1',
                    'https://account.example.com:443', 'https://account.example.com/path', 'https://account.example.com?q=x',
                    'https://account.example.com#x', 'https://localhost', 'https://ACCOUNT.example.com'):
            with self.assertRaises(ValueError):
                realm.build_bundle(bad, 'https://credentials.example.com')
        with self.assertRaises(ValueError):
            realm.build_bundle('https://account.example.com', 'https://account.example.com/')
        for name in ('master', 'pinvou-workload', '../realm', ''):
            with self.assertRaises(ValueError):
                realm.build_bundle('https://account.example.com', 'https://credentials.example.com', name)

    def test_deterministic_and_no_development_source_dependency(self):
        before = json.dumps(self.bundle(), sort_keys=True)
        changed = self.bundle()
        changed['realm']['clients'][0]['enabled'] = False
        self.assertEqual(before, json.dumps(self.bundle(), sort_keys=True))
        source = (Path(__file__).resolve().parents[1] / 'account-platform-user-realm.py').read_text()
        self.assertNotIn('pinvou-dev-realm.json', source)


if __name__ == '__main__':
    unittest.main()

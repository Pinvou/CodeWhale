from pathlib import Path
import unittest


class KeycloakUserAuthConfigTests(unittest.TestCase):
    def test_overlay_only_mounts_own_projection_without_legacy_secret(self):
        source = (Path(__file__).resolve().parents[2] /
                  'account-platform/deploy/production/compose.keycloak-user-auth.yaml').read_text()
        self.assertEqual(source.count('- type: bind'), 1)
        self.assertIn('KEYCLOAK_WORKLOAD_TOKEN_DIRECTORY:?', source)
        self.assertIn('target: /run/pinvou/keycloak-workload', source)
        self.assertIn('read_only: true', source)
        self.assertIn('create_host_path: false', source)
        self.assertIn('PINVOU_WORKLOAD_IDENTITY_TOKEN_FILE: /run/pinvou/keycloak-workload/token', source)
        self.assertIn('PINVOU_INTERNAL_SERVICE_TOKEN: ""', source)
        for variable in ('KEYCLOAK_ACCOUNT_AUTH_URL', 'KEYCLOAK_OIDC_ISSUER', 'KEYCLOAK_IDENTITY_AUTH_URL',
                         'KEYCLOAK_SMS_VERIFY_URL'):
            self.assertIn(variable + ':?', source)
        for forbidden in ('bootstrap', 'CLIENT_SECRET', 'ports:', 'image:', 'command:', 'networks:'):
            self.assertNotIn(forbidden, source)


if __name__ == '__main__':
    unittest.main()

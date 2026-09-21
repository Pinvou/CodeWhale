"""Public configuration contracts; no production secrets or network access."""
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[2]
DEPLOY = ROOT / 'account-platform/deploy/production'


class KeycloakProductionConfigTests(unittest.TestCase):
    def test_private_file_configuration_and_no_environment_credentials(self):
        compose = (DEPLOY / 'compose.keycloak.yaml').read_text()
        self.assertIn('"--config-file=/run/pinvou/keycloak-config/keycloak.conf", "start", "--optimized"', compose)
        for forbidden in ('start-dev', 'env_file:', 'KC_DB_PASSWORD', 'KC_BOOTSTRAP_ADMIN_PASSWORD',
                          'ports:', 'privileged:', '--import-realm'):
            self.assertNotIn(forbidden, compose)
        self.assertIn('external: true', compose)
        self.assertIn('pull_policy: never', compose)
        self.assertEqual(compose.count('create_host_path: false'), 4)
        self.assertEqual(compose.count('read_only: true'), 5)
        for required in ('user: "1000:0"', 'cap_drop: [ALL]', 'mem_limit: 1536m', 'pids_limit: 256',
                         'no-new-privileges:true', 'soft: 0, hard: 0', 'https://keycloak:9000/health/ready',
                         'KC_SPI_CACHE_EMBEDDED_DEFAULT_NODE_NAME: "pinvou-keycloak-production"'):
            self.assertIn(required, compose)

    def test_fixed_private_tls_hostname_and_distinct_rendered_secrets(self):
        template = (DEPLOY / 'keycloak.conf.template').read_text()
        self.assertEqual(template.count('@DATABASE_PASSWORD@'), 1)
        self.assertEqual(template.count('@BOOTSTRAP_ADMIN_PASSWORD@'), 1)
        for required in ('sslmode=verify-full', 'hostname=https://keycloak:8443', 'http-enabled=false',
                         'hostname-strict=true', 'hostname-backchannel-dynamic=false',
                         'bootstrap-admin-username=pinvou-bootstrap-admin'):
            self.assertIn(required, template)
        self.assertNotIn('127.0.0.1', template)
        self.assertNotIn('sslmode=require', template)


if __name__ == '__main__':
    unittest.main()

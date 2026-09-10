"""Static fixed-scope contracts; actual Docker/TLS checks remain opt-in."""
import importlib.util
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
DEPLOY = ROOT / 'account-platform/deploy/production'
spec = importlib.util.spec_from_file_location('realm', ROOT / 'scripts/account-platform-workload-realm.py')
realm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(realm)


class WorkloadProductionConfigTests(unittest.TestCase):
    def test_disposable_revocation_state_restored_before_projection(self):
        source = (ROOT / 'scripts/account-platform-workload-production-fixture.py').read_text()
        self.assertIn("realm_path = '/admin/realms/pinvou-fixture-workload'", source)
        self.assertIn("state['enabled'] is False", source)
        self.assertIn("state['attributes']['access.token.lifespan'] == '40'", source)
        self.assertIn("restored['enabled'] is True", source)
        self.assertIn("restored['attributes']['access.token.lifespan'] == '300'", source)
        self.assertLess(source.index('WORKLOAD_DISPOSABLE_CLIENT_NOT_RESTORED'), source.index("'/service-account-user'"))

    def test_exact_seven_agents_and_three_separate_directories(self):
        source = (DEPLOY / 'compose.workload-agents.yaml').read_text()
        body = source.split('services:\n', 1)[1].split('networks:\n', 1)[0]
        names = re.findall(r'^  ([a-z][a-z-]+):$', body, re.M)
        expected_agents = [service + '-workload-agent' for service in realm.SERVICES]
        self.assertEqual(names, expected_agents)
        self.assertFalse(set(names).intersection(realm.SERVICES))
        self.assertEqual(source.count('create_host_path: false'), 21)
        self.assertEqual(source.count('target: /run/config'), 7)
        self.assertEqual(source.count('target: /run/bootstrap'), 7)
        self.assertEqual(source.count('target: /run/projected'), 7)
        self.assertEqual(source.count('read_only: true'), 15)
        for service in realm.SERVICES:
            for directory in ('config', 'bootstrap', 'projected'):
                self.assertIn('/' + service + '/' + directory, source)
        self.assertIn('user: "65532:65532"', source)
        self.assertIn('keycloak-mobile-authenticator-workload-agent:\n    <<: *agent\n    user: "1000:0"', source)

    def test_private_network_healthcheck_and_bounded_runtime(self):
        source = (DEPLOY / 'compose.workload-agents.yaml').read_text()
        for required in ('external: true', 'pull_policy: never', 'cap_drop: [ALL]', 'no-new-privileges:true',
                         'mem_limit: 96m', 'cpus: 0.25', 'pids_limit: 32', 'soft: 0, hard: 0',
                         'SSL_CERT_FILE: /run/config/ca.crt', '"/run/config/workload.json", "-check"'):
            self.assertIn(required, source)
        for forbidden in ('ports:', 'privileged:', 'env_file:', 'CLIENT_SECRET:', 'TOKEN:', 'network_mode: host',
                          'aliases:'):
            self.assertNotIn(forbidden, source)
        dockerfile = (DEPLOY / 'Dockerfile.workload-agent').read_text()
        self.assertIn('FROM scratch', dockerfile)
        self.assertEqual(dockerfile.count('COPY '), 1)
        self.assertIn('COPY workload-token-agent /workload-token-agent', dockerfile)
        self.assertNotIn('RUN ', dockerfile)


if __name__ == '__main__':
    unittest.main()

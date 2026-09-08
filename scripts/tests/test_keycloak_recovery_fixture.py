import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('recovery', Path(__file__).resolve().parents[1] / 'account-platform-keycloak-recovery-fixture.py')
recovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(recovery)


class KeycloakRecoveryTests(unittest.TestCase):
    def test_remove_only_exact_bootstrap_settings(self):
        before = '# keep\ndb=postgres\nbootstrap-admin-username=temporary\nbootstrap-admin-password=fixture\nhostname=https://issuer.invalid\n'
        self.assertEqual(recovery.remove_bootstrap_settings(before), '# keep\ndb=postgres\nhostname=https://issuer.invalid\n')

    def test_missing_or_duplicate_settings_are_rejected(self):
        for source in ('db=postgres\n', 'bootstrap-admin-username=fixture\n',
                       'bootstrap-admin-username=a\nbootstrap-admin-password=fixture\nbootstrap-admin-username=b\n'):
            with self.assertRaises(RuntimeError):
                recovery.remove_bootstrap_settings(source)


if __name__ == '__main__':
    unittest.main()

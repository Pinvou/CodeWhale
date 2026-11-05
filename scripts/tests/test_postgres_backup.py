"""Public backup configuration contracts, no Docker or production credentials."""
import importlib.util
import json
from pathlib import Path
import unittest
import sys
import subprocess

ROOT = Path(__file__).resolve().parents[2]
DEPLOY = ROOT / 'account-platform/deploy/production'
spec = importlib.util.spec_from_file_location('backup_fixture', ROOT / 'scripts/account-platform-postgres-backup-fixture.py')
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class PostgresBackupTests(unittest.TestCase):
    def test_non_utf8_tool_diagnostic_does_not_hide_exit_or_ascii_error(self):
        result = fixture.run([sys.executable, '-c',
            "import os; os.write(2, bytes([255])+b'[CryptoError] unable to decrypt'); raise SystemExit(95)"], check=False)
        self.assertEqual(result.returncode, 95)
        self.assertIn('[CryptoError] unable to decrypt', result.stderr)
        self.assertTrue(result.stderr.startswith('\ufffd'))

    def test_wrong_cipher_cannot_be_satisfied_by_file_or_permission_errors(self):
        fixture.require_cipher_failure(95, 'ERROR: [095]: unable to flush')
        for message in ('[FileMissingError] cipher file missing', '[FileOpenError] permission denied',
                        'cipher aes-256-cbc error', '[CryptoError] unrelated TLS failure',
                        'ERROR: [095]: unable to flush; [FileOpenError] permission denied'):
            with self.assertRaises(RuntimeError):
                fixture.require_cipher_failure(95, message)
        with self.assertRaises(RuntimeError):
            fixture.require_cipher_failure(0, 'ERROR: [095]: unable to flush')
        for code in (29, 41, 55, 75, 103):
            with self.assertRaises(RuntimeError):
                fixture.require_cipher_failure(code, 'ERROR: [095]: unable to flush')

    def test_actual_privilege_boundary(self):
        data = {'Config': {'User': '999:999'}, 'HostConfig': {'Privileged': False, 'CapAdd': None}}
        fixture.verify_user(data)
        for value in ({'User': 'root'}, {'User': '999'}):
            with self.assertRaises(RuntimeError):
                fixture.verify_user({**data, 'Config': value})
        for value in ({'Privileged': True}, {'Privileged': False, 'CapAdd': ['SYS_ADMIN']}):
            with self.assertRaises(RuntimeError):
                fixture.verify_user({**data, 'HostConfig': value})

    def test_retention_never_discards_wal_or_expires_initial_history(self):
        config = (DEPLOY / 'pgbackrest.conf.template').read_text()
        for setting in ('expire-auto=n', 'archive-check=y', 'archive-async=n', 'repo1-cipher-type=aes-256-cbc',
                        'process-max=1', 'log-level-file=off', 'repo1-retention-full=35'):
            self.assertIn(setting, config.splitlines())
        self.assertNotIn('archive-push-queue-max', config)
        self.assertEqual(config.count('@REPOSITORY_CIPHER_PASS@'), 1)

    def test_default_postgres_remains_unchanged_and_overlay_requires_explicit_paths(self):
        base = (DEPLOY / 'postgres.conf').read_text()
        self.assertNotIn('archive_command', base)
        config = (DEPLOY / 'postgres-backup.conf').read_text()
        self.assertIn("include = '/etc/pinvou/postgres/postgres-base.conf'", config)
        self.assertIn("archive_timeout = '30s'", config)
        self.assertIn('archive_mode = on', config)
        overlay = (DEPLOY / 'compose.postgres-backup.yaml').read_text()
        self.assertIn('${POSTGRES_BACKUP_REPOSITORY:?', overlay)
        self.assertIn('${POSTGRES_BACKUP_SECRETS:?', overlay)
        self.assertEqual(overlay.count('create_host_path: false'), 6)
        self.assertNotIn('ports:', overlay)
        self.assertNotIn('environment:', overlay)

    def test_fixed_image_and_authenticated_backup_entrypoint(self):
        dockerfile = (DEPLOY / 'Dockerfile.postgres-backup').read_text()
        self.assertIn('@sha256:7bade6d532592ca8ce7ee32def7399dad2607c4ea5583839fc4352a095a11ea6', dockerfile)
        self.assertIn('pgbackrest=2.59.1-1.pgdg12+1', dockerfile)
        self.assertEqual(dockerfile.count('dpkg-query -W'), 2)
        self.assertNotIn('--allow-unauthenticated', dockerfile)
        self.assertNotIn('Verify-Peer=false', dockerfile)
        entrypoint = (DEPLOY / 'postgres-backup.sh').read_text()
        self.assertIn('create|check|full|incr|info|stats)', entrypoint)
        self.assertIn('export PGPASSFILE=/run/pinvou/postgres-backup/pgpass', entrypoint)
        self.assertIn('unset PGPASSWORD', entrypoint)
        self.assertIn('db_ready', entrypoint)
        self.assertIn('db_private_file /run/pinvou/postgres-backup/pgbackrest.conf', entrypoint)
        self.assertNotIn('eval ', entrypoint)

    def test_info_requires_real_encrypted_successful_backup(self):
        data = [{'name': 'platform', 'status': {'code': 0}, 'cipher': 'aes-256-cbc',
                 'backup': [{'type': 'full', 'label': 'fixture-full'}]}]
        self.assertEqual(fixture.info_backup(json.dumps(data), 'full')['label'], 'fixture-full')
        for change in ('cipher', 'status', 'backup'):
            candidate = json.loads(json.dumps(data))
            candidate[0][change] = {'cipher': 'none', 'status': {'code': 2}, 'backup': []}[change]
            with self.assertRaises(RuntimeError):
                fixture.info_backup(json.dumps(candidate), 'full')
        data[0]['backup'][0]['error'] = True
        with self.assertRaises(RuntimeError):
            fixture.info_backup(json.dumps(data), 'full')


    def test_fixture_boundary_functions_do_not_depend_on_deleted_full_stack_helper(self):
        source = (ROOT / 'scripts/account-platform-postgres-backup-fixture.py').read_text()
        self.assertNotIn('account-platform-keycloak-production-fixture.py', source)
        self.assertEqual(source.count('def verify_limits('), 1)
        self.assertEqual(source.count('def verify_mounts('), 1)
        meta = {'HostConfig': {'ReadonlyRootfs':True, 'PortBindings':{}, 'CapDrop':['ALL'],
                'SecurityOpt':['no-new-privileges:true'], 'Memory':1024**3, 'NanoCpus':1000000000,
                'PidsLimit':192, 'Ulimits':[{'Name':'core','Soft':0,'Hard':0}], 'Tmpfs':{}},
                'Mounts':[{'Type':'bind','Source':'/fixture/data','Destination':'/data','RW':False}]}
        fixture.verify_limits(meta, 1024**3, 192)
        fixture.verify_mounts(meta, [('/fixture/data','/data',True)], {})
        with self.assertRaisesRegex(RuntimeError, 'EXACT_BIND_MOUNTS_REQUIRED'):
            fixture.verify_mounts(meta, [('/different/data','/data',True)], {})
        with self.assertRaisesRegex(RuntimeError, 'CONTAINER_CORE_LIMIT'):
            fixture.verify_limits({**meta, 'HostConfig':{**meta['HostConfig'], 'Ulimits':[]}}, 1024**3, 192)

    def test_current_bootstrap_secret_set_and_database_owner_stay_original(self):
        source = (DEPLOY / 'postgres-init.sh').read_text()
        line = next(line for line in source.splitlines() if line.startswith('for db_name in '))
        self.assertEqual(set(line[len('for db_name in '):].removesuffix('; do').split()), set(fixture.KEYS))
        self.assertIn('-U account_platform_admin', (DEPLOY / 'postgres-health.sh').read_text())
        self.assertIn('scram-sha-256', (DEPLOY / 'compose.dependencies.yaml').read_text())
        self.assertIn('ssl = on', (DEPLOY / 'postgres.conf').read_text())
        self.assertIn('pg1-user=account_platform_admin', (DEPLOY / 'pgbackrest.conf.template').read_text())


    def test_fixture_requires_explicit_optin_and_never_echoes_unknown_arguments(self):
        script = ROOT / 'scripts/account-platform-postgres-backup-fixture.py'
        for args in ([], ['--unknown-PRIVATE_CANARY'], ['--image', 'PRIVATE_CANARY']):
            result = subprocess.run([sys.executable, str(script), *args], capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 1)
            self.assertRegex(result.stdout.strip(), rb'^BACKUP_FIXTURE_ARGUMENTS_INVALID\r?\nPG_BACKUP_FIXTURE_FAILURE_LINE_[0-9]+$')
            self.assertNotIn(b'PRIVATE_CANARY', result.stdout)
            self.assertEqual(result.stderr, b'')

    def test_current_optional_package_and_manual_runbook_do_not_auto_enable(self):
        package = json.loads((ROOT / 'account-platform/package.json').read_text(encoding='utf-8'))
        self.assertEqual(package['scripts']['test:postgres-backup-tools'],
                         'python3 -m unittest discover -s ../scripts/tests -p "test_postgres_backup*.py"')
        runbook = (ROOT / 'account-platform/deploy/runbooks/local-backup-rollout.md').read_text(encoding='utf-8')
        self.assertIn('full/incr/monitor', runbook)
        self.assertIn('inflight.json', runbook)
        self.assertNotIn('现场验收完成', runbook)
        self.assertNotIn('已启用加密仓库', runbook)


if __name__ == '__main__':
    unittest.main()

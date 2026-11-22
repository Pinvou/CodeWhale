"""No production access; failure and binding contracts for the local scheduler."""
import copy
from datetime import datetime, timezone
import importlib.util
import io
import os
import stat
import sys
import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from unittest.mock import Mock

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('backup_job', ROOT / 'scripts/account-platform-postgres-backup-job.py')
job = importlib.util.module_from_spec(spec)
spec.loader.exec_module(job)
NOW = 1800000000


def information():
    return [{'name': 'platform', 'status': {'code': 0}, 'cipher': 'aes-256-cbc', 'backup': [
        {'label': '20260908-010000F', 'type': 'full', 'timestamp': {'stop': NOW - 100}},
        {'label': '20260908-010000F_20260908-020000I', 'type': 'incr', 'timestamp': {'stop': NOW - 10}}]}]


def statistics():
    return {'system_identifier': '1234567890123', 'archive_mode': 'on', 'archive_timeout': '30s',
            'databases': job.DATABASES, 'last_archived_wal': 'A' * 24, 'last_failed_at': None,
            'last_archived_at': datetime.fromtimestamp(NOW - 5, timezone.utc).isoformat()}


class BackupJobTests(unittest.TestCase):
    def test_postgres_fractional_timestamp_supports_python310(self):
        for digits in range(1, 7):
            fraction = '123456'[:digits]
            for offset in ('Z', '+00:00', '+08:00'):
                parsed = job.archive_timestamp('2026-09-08T13:00:00.' + fraction + offset)
                self.assertEqual(parsed.microsecond, int(fraction.ljust(6, '0')))
        for value in ('2026-09-08T13:00:00', '2026-09-08T13:00:00.1234567Z', None):
            with self.assertRaises(job.Failure):
                job.archive_timestamp(value)

    def test_lock_queue_retries_only_contention_and_has_fixed_deadline(self):
        locking = SimpleNamespace(LOCK_EX=2, LOCK_NB=4, flock=Mock(side_effect=[BlockingIOError(), None]))
        with patch.object(job.time, 'monotonic', side_effect=[0, 1]), patch.object(job.time, 'sleep') as sleep:
            self.assertTrue(job.acquire_lock('lock', True, locking))
            sleep.assert_called_once_with(1)
        locking.flock = Mock(side_effect=BlockingIOError())
        with patch.object(job.time, 'monotonic', side_effect=[0, 3200]), patch.object(job.time, 'sleep') as sleep:
            self.assertFalse(job.acquire_lock('lock', True, locking))
            sleep.assert_not_called()
        with patch.object(job.time, 'monotonic', return_value=0), patch.object(job.time, 'sleep') as sleep:
            self.assertFalse(job.acquire_lock('lock', False, locking))
            sleep.assert_not_called()
        locking.flock = Mock(side_effect=PermissionError())
        with self.assertRaises(PermissionError):
            job.acquire_lock('lock', True, locking)

    def test_backup_time_encryption_and_error_boundaries(self):
        good = information()
        self.assertEqual(job.backup_information(json.dumps(good), NOW)['last_full_at'], NOW - 100)
        for key, value in [('cipher', 'none'), ('status', {'code': 1}), ('backup', [])]:
            candidate = copy.deepcopy(good)
            candidate[0][key] = value
            with self.assertRaises(job.Failure):
                job.backup_information(json.dumps(candidate), NOW)
        for stamp in (NOW + 1, NOW - 26 * 3600 - 1, float('nan'), float('inf'), True):
            candidate = copy.deepcopy(good)
            for backup in candidate[0]['backup']:
                backup['timestamp']['stop'] = stamp
            with self.assertRaises(job.Failure):
                job.backup_information(json.dumps(candidate), NOW)
        good[0]['backup'][0]['error'] = True
        with self.assertRaises(job.Failure):
            job.backup_information(json.dumps(good), NOW)

    def test_stale_backup_can_be_repaired_but_not_marked_fresh(self):
        info = information()
        for backup in info[0]['backup']:
            backup['timestamp']['stop'] = NOW - 9 * 86400
        job.backup_information(json.dumps(info), NOW, freshness=False)
        with self.assertRaises(job.Failure):
            job.backup_information(json.dumps(info), NOW)

    def test_archive_checks_exact_cluster_database_and_time(self):
        data = {'system_identifier': '1234567890123'}
        job.archive_information(json.dumps(statistics()), data, NOW)
        for key, value in [('system_identifier', '9999999999999'), ('archive_mode', 'off'),
                           ('archive_timeout', '60s'), ('databases', job.DATABASES[:-1]),
                           ('last_archived_wal', None), ('last_archived_at', '2020-01-01T00:00:00Z'),
                           ('last_archived_at', '2026-01-01T00:00:00'),
                           ('last_failed_at', datetime.fromtimestamp(NOW, timezone.utc).isoformat())]:
            with self.assertRaises(job.Failure):
                job.archive_information(json.dumps({**statistics(), key: value}), data, NOW)

    def test_capacity_thresholds(self):
        for free, inode, expected in [(50, 90, 'ok'), (19, 90, 'warning'), (9, 90, 'critical'), (50, 9, 'critical')]:
            fs = SimpleNamespace(f_blocks=100, f_bavail=free, f_frsize=1 << 30, f_files=100, f_favail=inode)
            with patch.object(job.os, 'statvfs', return_value=fs, create=True):
                if expected == 'critical':
                    with self.assertRaises(job.Failure):
                        job.capacity(Path('/fixture'))
                else:
                    self.assertEqual(job.capacity(Path('/fixture'))['warning'], expected == 'warning')

    def test_complete_container_binding(self):
        data = {'container_id': 'a' * 64, 'image_id': 'sha256:' + 'b' * 64, 'project': 'fixture', 'network_id': 'c' * 64,
                'data_directory': '/fixture/data', 'repository': '/fixture/repo', 'secrets_directory': '/fixture/secrets',
                'mounts': [{'source': '/fixture/' + source, 'target': target, 'read_only': readonly} for source, target, readonly in
                           [('data', '/var/lib/postgresql/data', False), ('repo', '/var/lib/pgbackrest', False),
                            ('secrets', '/run/pinvou/postgres-backup', True), ('runner', job.RUNNER, True)]]}
        actual = {'Id': data['container_id'], 'Image': data['image_id'],
                  'Config': {'User': '999:999', 'Labels': {'com.docker.compose.project': 'fixture', 'com.docker.compose.service': 'postgres'}},
                  'HostConfig': {'ReadonlyRootfs': True, 'Privileged': False, 'CapAdd': None, 'CapDrop': ['ALL'], 'PortBindings': {},
                                 'SecurityOpt': ['no-new-privileges:true'], 'Tmpfs': job.TMPFS, 'Memory': 1 << 30,
                                 'NanoCpus': 1000000000, 'PidsLimit': 192, 'Ulimits': [{'Name': 'core', 'Soft': 0, 'Hard': 0}]},
                  'NetworkSettings': {'Networks': {'fixture_data': {'NetworkID': data['network_id']}}},
                  'State': {'Running': True, 'Health': {'Status': 'healthy'}},
                  'Mounts': [{'Type': 'bind', 'Source': m['source'], 'Destination': m['target'], 'RW': not m['read_only']} for m in data['mounts']]}
        job.verify_container(data, actual)
        for section, key, value in [(None, 'Image', 'wrong'), ('Config', 'User', 'root'), ('HostConfig', 'Privileged', True),
                                    ('HostConfig', 'CapAdd', ['SYS_ADMIN']), ('State', 'Running', False),
                                    ('HostConfig', 'PortBindings', {'5432/tcp': []})]:
            candidate = copy.deepcopy(actual)
            (candidate[section] if section else candidate)[key] = value
            with self.assertRaises(job.Failure):
                job.verify_container(data, candidate)
        actual['Mounts'][0]['Source'] = '/other/data'
        with self.assertRaises(job.Failure):
            job.verify_container(data, actual)
        actual['Mounts'][0]['Source'] = '/fixture/data'
        for key, value in [('SecurityOpt', []), ('Tmpfs', {**job.TMPFS, '/extra': 'rw'}), ('Memory', 0),
                           ('NanoCpus', 0), ('PidsLimit', -1), ('Ulimits', [])]:
            candidate = copy.deepcopy(actual)
            candidate['HostConfig'][key] = value
            with self.assertRaises(job.Failure):
                job.verify_container(data, candidate)
        actual['NetworkSettings']['Networks']['extra'] = {'NetworkID': 'extra'}
        with self.assertRaises(job.Failure):
            job.verify_container(data, actual)

    def test_real_status_entry_keeps_warning_success_and_failure_nonzero(self):
        for warning in (False, True):
            result = {'capacity': {'repository': {'warning': warning}, 'data_directory': {'warning': False}}}
            with patch.object(job, 'configuration', return_value={}), patch.object(job, 'execute', return_value=result), \
                 patch.object(job, 'atomic') as write, patch('sys.stdout', new_callable=io.StringIO) as output:
                code = job.run_locked(Path('/fixture/job.json'), Path('/fixture/state'), 'incr')
                self.assertEqual(code, 0)
                self.assertEqual(write.call_args.args[1]['status'], 'warning' if warning else 'ok')
                self.assertIn('checked_at', json.loads(output.getvalue()))
        for error in (job.Failure('CAPACITY_CRITICAL'), RuntimeError('PRIVATE_CANARY')):
            with patch.object(job, 'configuration', return_value={}), patch.object(job, 'execute', side_effect=error), \
                 patch.object(job, 'atomic') as write, patch('sys.stdout', new_callable=io.StringIO) as output:
                self.assertEqual(job.run_locked(Path('/fixture/job.json'), Path('/fixture/state'), 'monitor'), 1)
                self.assertEqual(write.call_args.args[1]['status'], 'failed')
                self.assertNotIn('PRIVATE_CANARY', output.getvalue())

    def test_post_backup_capacity_is_resampled(self):
        data = {'container_id': 'fixture', 'network_id': 'network', 'system_identifier': 'id',
                'repository': '/fixture/repo', 'data_directory': '/fixture/data'}
        replies = [b'{"system_identifier":"id"}', b'info', b'backup', b'check', b'newinfo', b'stats']
        with patch.object(job, 'verify_container'), patch.object(job, 'command', side_effect=[b'[{}]', b'[{"Id":"network","Internal":true}]']), \
             patch.object(job, 'invoke', side_effect=replies), patch.object(job, 'archive_information', return_value={}), \
             patch.object(job, 'backup_information', side_effect=[{'backup_id': 'old'}, {'backup_id': 'new'}]), \
             patch.object(job, 'capacity', side_effect=[{}, {}, job.Failure('CAPACITY_CRITICAL')]) as capacity:
            with self.assertRaisesRegex(job.Failure, 'CAPACITY_CRITICAL'):
                job.execute(data, Path('/fixture/state'), 'incr')
            self.assertEqual(capacity.call_count, 3)

    def test_nested_paths_rejected(self):
        job.verify_paths([Path('/fixture/data'), Path('/fixture/repo'), Path('/fixture/secrets')])
        for paths in [('/fixture', '/fixture/repo'), ('/fixture/repo', '/fixture'), ('/fixture', '/fixture')]:
            with self.assertRaises(job.Failure):
                job.verify_paths([Path(p) for p in paths])

    def test_deadline_includes_cleanup_and_never_starts_unbounded_job(self):
        with patch.object(job.time, 'monotonic', return_value=100), patch.object(job.subprocess, 'run') as run:
            with self.assertRaisesRegex(job.Failure, 'DEADLINE'):
                job.invoke({}, Path('/unused'), 'check', deadline=124)
            run.assert_not_called()
            with self.assertRaisesRegex(job.Failure, 'DEADLINE'):
                job.remaining(99, 10)

    def test_uncertain_command_keeps_latch_and_blocks_next_call(self):
        with tempfile.TemporaryDirectory() as name:
            state = Path(name)
            with patch.object(job, 'atomic', side_effect=lambda p, d: p.write_text(json.dumps(d))), \
                 patch.object(job.subprocess, 'run', side_effect=subprocess.TimeoutExpired('fixture', 1)):
                with self.assertRaisesRegex(job.Failure, 'UNCONFIRMED'):
                    job.invoke({'container_id': 'fixture'}, state, 'check')
                self.assertTrue((state / 'inflight.json').exists())
                with self.assertRaisesRegex(job.Failure, 'UNCONFIRMED'):
                    job.invoke({'container_id': 'fixture'}, state, 'check')

    def test_missing_exit_confirmation_is_never_success(self):
        with tempfile.TemporaryDirectory() as name:
            with patch.object(job, 'atomic', side_effect=lambda p, d: p.write_text(json.dumps(d))), \
                 patch.object(job.subprocess, 'run', return_value=SimpleNamespace(returncode=0, stdout=b'PASS', stderr=b'secret')):
                with self.assertRaisesRegex(job.Failure, 'UNCONFIRMED'):
                    job.invoke({'container_id': 'fixture'}, Path(name), 'check')

    def test_initial_create_also_keeps_latch_without_confirmation(self):
        with tempfile.TemporaryDirectory() as name:
            with patch.object(job, 'atomic', side_effect=lambda p, d: p.write_text(json.dumps(d))), \
                 patch.object(job.subprocess, 'run', return_value=SimpleNamespace(returncode=0, stdout=b'', stderr=b'')) as command:
                with self.assertRaisesRegex(job.Failure, 'UNCONFIRMED'):
                    job.invoke({'container_id': 'fixture'}, Path(name), 'create')
                self.assertEqual(command.call_args.args[0][-2:], ['45', 'create'])
                self.assertTrue((Path(name) / 'inflight.json').exists())
        source = (ROOT / 'scripts/account-platform-postgres-backup-job.py').read_text()
        self.assertIn("choices=('full', 'incr', 'monitor')", source)

    def test_timer_schedules_and_resource_limits(self):
        deploy = ROOT / 'account-platform/deploy/production'
        for kind, calendar in [('full', 'Sun *-*-* 02:00:00 UTC'), ('incr', 'Mon..Sat *-*-* 02:00:00 UTC')]:
            self.assertIn('OnCalendar=' + calendar, (deploy / ('pinvou-postgres-backup-' + kind + '.timer')).read_text())
            self.assertIn('Persistent=true', (deploy / ('pinvou-postgres-backup-' + kind + '.timer')).read_text())
            service = (deploy / ('pinvou-postgres-backup-' + kind + '.service')).read_text()
            self.assertIn(kind + ' --wait-lock', service)
            self.assertIn('TimeoutStartSec=6400', service)
        for kind in ('full', 'incr', 'monitor'):
            service = (deploy / ('pinvou-postgres-backup-' + kind + '.service')).read_text()
            for line in ('UMask=0077', 'LimitCORE=0', 'NoNewPrivileges=true', 'MemoryMax=256M', 'ProtectSystem=strict'):
                self.assertIn(line, service)


    def test_bounded_strict_json_rejects_duplicate_and_nonfinite_evidence(self):
        self.assertEqual(job.parse_json(b'{"a":[{"b":1},{"b":2}]}')['a'][1]['b'], 2)
        for raw in (b'{"a":1,"a":2}', b'{"a":1,"\\u0061":2}', b'{"a":NaN}',
                    b'{"a":Infinity}', bytes([0xc0, 0xaf]), '{"a":1}'.encode('utf-16'), b' ' * 65537):
            with self.assertRaises(job.Failure):
                job.parse_json(raw, 65536)

    def test_unknown_dispatch_never_replays_or_removes_durable_original(self):
        for failure in (OSError('PRIVATE_CANARY'), subprocess.TimeoutExpired('fixture', 1)):
            with tempfile.TemporaryDirectory() as directory:
                state = Path(directory)
                with patch.object(job, 'atomic', side_effect=lambda p, d: p.write_bytes(json.dumps(d).encode())), \
                     patch.object(job.subprocess, 'run', side_effect=failure) as command:
                    with self.assertRaisesRegex(job.Failure, 'UNCONFIRMED'):
                        job.invoke({'container_id': 'a' * 64}, state, 'incr')
                    original = (state / 'inflight.json').read_bytes()
                    with self.assertRaisesRegex(job.Failure, 'UNCONFIRMED'):
                        job.invoke({'container_id': 'a' * 64}, state, 'full')
                    self.assertEqual(command.call_count, 1)
                    self.assertEqual((state / 'inflight.json').read_bytes(), original)
                    self.assertEqual(json.loads(original)['operation'], 'incr')

    def test_confirmed_failure_clears_only_completed_latch_and_never_claims_backup_success(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory)
            def opened(path, flags):
                self.assertEqual(path, state)
                return 123
            with patch.object(job, 'atomic', side_effect=lambda p, d: p.write_bytes(json.dumps(d).encode())), \
                 patch.object(job.subprocess, 'run', return_value=SimpleNamespace(returncode=124, stdout=job.CONFIRMED)), \
                 patch.object(job.os, 'open', side_effect=opened), patch.object(job.os, 'O_DIRECTORY', 0, create=True), \
                 patch.object(job.os, 'fsync') as synced, patch.object(job.os, 'close'):
                with self.assertRaisesRegex(job.Failure, 'BACKUP_TOOL_FAILED'):
                    job.invoke({'container_id': 'a' * 64}, state, 'incr')
                self.assertFalse((state / 'inflight.json').exists())
                synced.assert_called_once_with(123)

    def test_future_or_invalid_backups_cannot_be_used_even_as_prewrite_baseline(self):
        for stamp in (NOW + 1, float('nan'), float('inf'), True, -1):
            bad = information()
            bad[0]['backup'][0]['timestamp']['stop'] = stamp
            with self.assertRaises(job.Failure):
                job.backup_information(json.dumps(bad), NOW, freshness=False)

    def test_archive_database_duplicates_and_extra_databases_are_not_partial_success(self):
        for names in [job.DATABASES + [job.DATABASES[0]], job.DATABASES + ['account_platform_payment_sim']]:
            with self.assertRaisesRegex(job.Failure, 'DATABASE_SET_MISMATCH'):
                job.archive_information(json.dumps({**statistics(), 'databases': names}), {'system_identifier':'1234567890123'}, NOW)

    def test_configuration_exact_keys_and_mount_boolean_contract(self):
        data = {'container_id':'a'*64, 'network_id':'b'*64, 'image_id':'sha256:'+'c'*64,
                'system_identifier':'1234567890123', 'project':'fixture', 'data_directory':'/fixture/data',
                'repository':'/fixture/repository', 'secrets_directory':'/fixture/secrets',
                'mounts':[{'source':'/fixture/runner', 'target':job.RUNNER, 'read_only':True}]}
        class CanonicalDirectory:
            def __init__(self, value): self.value = value
            def is_dir(self): return True
            def is_absolute(self): return True
            def resolve(self, **_): return self
        invalid_mount = {'source':'/fixture/runner', 'target':job.RUNNER, 'read_only':1}
        for mutation, code in [({'extra':'PRIVATE_CANARY'}, 'INVALID_CONFIGURATION'),
                               ({'mounts':[invalid_mount]}, 'INVALID_MOUNTS'),
                               ({'mounts':data['mounts'] * 2}, 'INVALID_MOUNTS')]:
            with tempfile.TemporaryDirectory() as directory:
                config = Path(directory) / 'job.json'
                config.write_bytes(json.dumps({**data, **mutation}).encode())
                with patch.object(job, 'private'), patch.object(job, 'Path', CanonicalDirectory), \
                     patch.object(job, 'verify_paths'):
                    with self.assertRaisesRegex(job.Failure, code):
                        job.configuration(config)
        # An unprivileged host must stop before Docker on both supported and unsupported OSes.
        with patch.object(job.os, 'geteuid', return_value=1000, create=True), patch('sys.argv', ['backup-job', 'monitor']), \
             patch.object(job.subprocess, 'run') as command:
            with self.assertRaisesRegex(job.Failure, 'LINUX_ROOT_REQUIRED'):
                job.main()
            command.assert_not_called()


    def test_actual_cli_bad_arguments_do_not_print_input_or_execute_commands(self):
        canary = 'PRIVATE_ARGUMENT_CANARY_7f23e8'
        script = ROOT / 'scripts/account-platform-postgres-backup-job.py'
        for argv in ([canary], ['full', '--unknown-' + canary], ['monitor', '--wait-lock']):
            result = subprocess.run([sys.executable, str(script), *argv], capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(result.stdout.strip(), b'POSTGRES_BACKUP_JOB_FAILED')
            self.assertEqual(result.stderr, b'')
            self.assertNotIn(canary.encode(), result.stdout + result.stderr)

    def test_private_host_configuration_rejects_hardlinked_or_group_readable_files(self):
        for mode, links in [(0o600, 2), (0o640, 1)]:
            name = Mock()
            name.is_absolute.return_value = True
            name.resolve.return_value = name
            name.is_symlink.return_value = False
            name.lstat.return_value = SimpleNamespace(st_uid=0, st_gid=0, st_mode=stat.S_IFREG | mode, st_nlink=links)
            with self.assertRaisesRegex(job.Failure, 'UNSAFE_PERMISSIONS'):
                job.private(name)


if __name__ == '__main__':
    unittest.main()

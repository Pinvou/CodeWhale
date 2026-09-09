"""Offline expiry contracts; no production access, passwords, database, or network."""
import copy
from datetime import datetime, timedelta, timezone
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('release_expiry', ROOT / 'scripts/account-platform-release-expiry.py')
expiry = importlib.util.module_from_spec(spec)
spec.loader.exec_module(expiry)
GENERATED = datetime(2026, 1, 1, tzinfo=timezone.utc)
UNTIL = GENERATED + timedelta(days=90)
ROLES = ['credential_release_fixture', 'gateway_release_fixture']
FIXTURE_ROOT = Path(tempfile.gettempdir()) / 'pinvou-expiry-contract'
CONFIG_PATH = FIXTURE_ROOT / 'config/check.json'
STATE_PATH = FIXTURE_ROOT / 'state/latest.json'


def stamp(value):
    return value.strftime('%Y-%m-%dT%H:%M:%SZ')


def fixture():
    data = {'format': 'pinvou-release-expiry-manifest-v1', 'installed_at': stamp(GENERATED + timedelta(hours=1)),
            'entries': [{'role': role, 'generated_at': stamp(GENERATED), 'valid_until': stamp(UNTIL)} for role in ROLES]}
    raw = json.dumps(data).encode()
    config = {'format': 'pinvou-release-expiry-config-v1', 'manifest': '/fixture/config/installed.json',
              'manifest_sha256': hashlib.sha256(raw).hexdigest(), 'runtime_roles': ROLES[:]}
    return config, raw


class ExpiryTests(unittest.TestCase):
    def test_exact_thresholds_and_recorded_not_live(self):
        for days, code, state in [(15, 0, 'OK'), (14, 1, 'WARNING'), (8, 1, 'WARNING'),
                                  (7, 2, 'CRITICAL'), (1, 2, 'CRITICAL'), (0, 3, 'EXPIRED'), (-1, 3, 'EXPIRED')]:
            with self.subTest(days=days):
                actual, result = expiry.evaluate(*fixture(), UNTIL - timedelta(days=days))
                self.assertEqual((actual, result['status']), (code, state))
                self.assertFalse(result['live_database_checked'])
                self.assertEqual(result['evidence'], 'RECORDED_EXPIRY_ONLY')
        self.assertEqual(expiry.evaluate(*fixture(), UNTIL - timedelta(days=14, seconds=1))[0], 0)
        self.assertEqual(expiry.evaluate(*fixture(), UNTIL - timedelta(days=7, seconds=1))[0], 1)

    def test_digest_and_config_closed_shape(self):
        config, raw = fixture()
        for key, value in [('format', 'other'), ('manifest_sha256', 'f' * 64), ('runtime_roles', ROLES[::-1]),
                           ('runtime_roles', [ROLES[0]] * 2), ('runtime_roles', [ROLES[0], 'invalid role']), ('extra', True)]:
            changed = copy.deepcopy(config)
            changed[key] = value
            with self.subTest(key=key, value=value), self.assertRaises(Exception):
                expiry.evaluate(changed, raw, GENERATED + timedelta(days=1))

    def test_missing_or_corrupt_entries_and_timestamps_rejected(self):
        for change in [lambda d: d.update(extra=True), lambda d: d.update(entries=[]),
                       lambda d: d.update(installed_at=stamp(GENERATED - timedelta(seconds=1))),
                       lambda d: d.update(installed_at=stamp(GENERATED + timedelta(days=91))),
                       lambda d: d['entries'][0].update(role=ROLES[1]),
                       lambda d: d['entries'][0].update(valid_until='2026-02-30T00:00:00Z'),
                       lambda d: d['entries'][0].update(valid_until='2026-04-01T00:00:00'),
                       lambda d: d['entries'][0].update(valid_until=stamp(UNTIL + timedelta(seconds=1))),
                       lambda d: d['entries'][0].update(password='never-accepted'),
                       lambda d: d['entries'][0].update(generated_at=None)]:
            config, raw = fixture()
            data = json.loads(raw)
            change(data)
            raw = json.dumps(data).encode()
            config['manifest_sha256'] = hashlib.sha256(raw).hexdigest()
            with self.assertRaises(Exception):
                expiry.evaluate(config, raw, GENERATED + timedelta(days=1))
        with self.assertRaises(Exception):
            expiry.evaluate(*fixture(), GENERATED)

    def test_json_duplicate_nonfinite_encoding_size(self):
        for raw in [b'', b' ' * 65537, b'{"a":1,"a":2}', b'{"a":NaN}', b'{"a":Infinity}', b'\xff', b'not json']:
            with self.subTest(raw=raw[:30]), self.assertRaises(Exception):
                expiry.decode(raw)

    def test_missing_input_still_records_invalid_without_raw_errors(self):
        output = io.StringIO()
        with (patch.object(expiry, 'private'), patch.object(expiry, 'read', side_effect=OSError('secret-canary')),
              patch.object(expiry, 'write_status') as write, patch('sys.stdout', output)):
            code = expiry.main(['--config', str(CONFIG_PATH), '--state', str(STATE_PATH)])
        self.assertEqual(code, 4)
        self.assertEqual(write.call_args.args[1]['status'], 'INVALID')
        self.assertNotIn('secret-canary', output.getvalue())

    def test_state_cannot_overwrite_config_and_failure_is_nonzero(self):
        for target in [CONFIG_PATH, CONFIG_PATH.parent / 'installed.json', FIXTURE_ROOT / 'other/state/latest.json']:
            with patch.object(expiry, 'write_status') as write, patch('sys.stdout', io.StringIO()):
                self.assertEqual(expiry.main(['--config', str(CONFIG_PATH), '--state', str(target)]), 4)
                write.assert_not_called()
        with (patch.object(expiry, 'private'), patch.object(expiry, 'read', side_effect=OSError()),
              patch.object(expiry, 'write_status', side_effect=OSError()), patch('sys.stdout', io.StringIO())):
            self.assertEqual(expiry.main(['--config', str(CONFIG_PATH), '--state', str(STATE_PATH)]), 4)

    def test_timer_is_offline_and_does_not_hide_warnings(self):
        service = (ROOT / 'account-platform/deploy/production/pinvou-release-expiry.service').read_text()
        timer = (ROOT / 'account-platform/deploy/production/pinvou-release-expiry.timer').read_text()
        for required in ['PrivateNetwork=true', 'ProtectSystem=strict', 'NoNewPrivileges=true', 'LimitCORE=0',
                         'InaccessiblePaths=-/var/run/docker.sock -/run/docker.sock']:
            self.assertIn(required, service)
        self.assertNotIn('SuccessExitStatus=', service)
        self.assertNotIn('Requires=docker', service)
        self.assertIn('Persistent=true', timer)


if __name__ == '__main__':
    unittest.main()

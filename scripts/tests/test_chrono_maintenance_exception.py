import contextlib
import importlib.util
import io
from datetime import date
from pathlib import Path
import re
import shutil
import sys
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/guard-chrono-maintenance-exception.py"
SPEC = importlib.util.spec_from_file_location("chrono_guard", SCRIPT)
GUARD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARD)
LOCKS = ("pinvou-knowledge/Cargo.lock", "pinvou3-app/src-tauri/Cargo.lock", "pinvou-cli/Cargo.lock")
APPROVED_DATE = date(2026, 11, 17)


class ChronoExceptionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        for name in (*LOCKS, "pinvou3-app/src-tauri/deny.toml"):
            destination = self.root / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / name, destination)

    def validate(self, today=APPROVED_DATE):
        return GUARD.validate_repository(self.root, today)

    def test_reviewed_locks_pass_without_writes(self):
        self.assertEqual(GUARD.LOCKFILES, LOCKS)
        before = {str(p): p.read_bytes() for p in self.root.rglob('*') if p.is_file()}
        self.validate()
        self.assertEqual(before, {str(p): p.read_bytes() for p in self.root.rglob('*') if p.is_file()})

    def test_expiry_boundary_and_later_fail_without_grace(self):
        self.validate(date(2026, 12, 16))
        for today in (date(2026, 12, 17), date(2026, 12, 18), date(2027, 1, 1)):
            with self.subTest(today=today), self.assertRaisesRegex(GUARD.GuardError, "expired"):
                self.validate(today)

    def test_all_consumers_require_exact_version_source_checksum_and_single_package(self):
        for name in LOCKS:
            path = self.root / name
            original = path.read_text(encoding="utf-8")
            pattern = r'\[\[package\]\]\nname = "chrono"\n.*?(?=\n\[\[package\]\]|\Z)'
            block = re.search(pattern, original, re.S)[0]
            for replacement in (
                block.replace('0.4.45', '0.4.44'),
                block.replace('0.4.45', '0.4.46'),
                block.replace(GUARD.SOURCE, 'git+https://example.invalid/chrono'),
                block.replace(GUARD.CHECKSUM, '0' * 64),
                block + '\n' + block,
                '',
            ):
                with self.subTest(lock=name, mutation=replacement[:90]):
                    path.write_text(original.replace(block, replacement), encoding="utf-8")
                    with self.assertRaises(GUARD.GuardError):
                        self.validate()
            path.write_text(original, encoding="utf-8")

    def test_each_missing_or_malformed_lock_is_rejected(self):
        for name in LOCKS:
            path = self.root / name
            original = path.read_bytes()
            path.unlink()
            with self.subTest(lock=name), self.assertRaises(GUARD.GuardError):
                self.validate()
            path.write_text('invalid=[', encoding="utf-8")
            with self.assertRaises(GUARD.GuardError):
                self.validate()
            path.write_bytes(original)

    def test_exception_and_other_gates_cannot_silently_disappear(self):
        path = self.root / "pinvou3-app/src-tauri/deny.toml"
        original = path.read_text(encoding="utf-8")
        mutations = [
            original.replace('"RUSTSEC-2026-0543",', ''),
            original.replace('"RUSTSEC-2026-0543",', '"RUSTSEC-2026-0543", "RUSTSEC-2026-0543",'),
            original.replace('unmaintained = "workspace"', 'unmaintained = "none"'),
            original.replace('unsound = "workspace"', 'unsound = "none"'),
            original.replace('yanked = "deny"', 'yanked = "warn"'),
            original.replace('unused-ignored-advisory = "deny"', 'unused-ignored-advisory = "allow"'),
        ]
        for mutation in mutations:
            path.write_text(mutation, encoding="utf-8")
            with self.assertRaises(GUARD.GuardError):
                self.validate()

    def test_cli_uses_live_utc_clock_and_no_override_arguments(self):
        with mock.patch.object(GUARD, 'datetime') as clock:
            clock.now.return_value.date.return_value = date(2026, 12, 17)
            with self.assertRaisesRegex(GUARD.GuardError, 'expired'):
                GUARD.validate_repository(self.root)
            clock.now.assert_called_once_with(GUARD.timezone.utc)
        with mock.patch.object(sys, 'argv', [str(SCRIPT), '--date=2026-11-17']), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(GUARD.main(), 1)
        with mock.patch.object(sys, 'argv', [str(SCRIPT)]), mock.patch.object(
            GUARD, 'validate_repository', side_effect=GUARD.GuardError('expired')
        ) as validate, contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(GUARD.main(), 1)
            validate.assert_called_once_with(ROOT)

    def test_only_approved_advisory_is_excepted(self):
        policy = GUARD.read_toml(ROOT, 'pinvou3-app/src-tauri/deny.toml')
        self.assertEqual(policy['advisories']['ignore'], ['RUSTSEC-2026-0543'])

    def test_expiry_runs_even_without_rust_changes_and_guard_changes_route_to_audits(self):
        workflow = (ROOT / '.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        fast = re.search(r'^  fast-gate:\n(.*?)(?=^  [\w-]+:)', workflow, re.S | re.M)[1]
        step = next(s for s in fast.split('\n      - ') if s.startswith('name: Expiring chrono maintenance exception\n'))
        self.assertIn('run: python3 scripts/guard-chrono-maintenance-exception.py\n', step)
        self.assertNotIn('if:', step)
        self.assertNotIn('continue-on-error', step)
        for group in ('rust_dependencies', 'knowledge_dependencies'):
            block = re.search(rf'^            {group}:\n(.*?)(?=^            \w+:)', workflow, re.M | re.S)[1]
            for path in ('scripts/guard-chrono-maintenance-exception.py', 'scripts/tests/test_chrono_maintenance_exception.py'):
                self.assertIn(f"- '{path}'", block)


if __name__ == '__main__':
    unittest.main()

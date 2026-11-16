import errno
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('prune_rust_test_artifacts', ROOT / 'scripts/prune-rust-test-artifacts.py')
PRUNE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PRUNE)


class RustArtifactCleanupTests(unittest.TestCase):
    def setUp(self):
        self.scope = tempfile.TemporaryDirectory(prefix='pinvou-prune-contract-')
        self.addCleanup(self.scope.cleanup)
        self.root = Path(self.scope.name)
        self.target = self.root / 'pinvou3-app/src-tauri/target/debug/deps'

    def test_missing_location_is_a_noop_without_claiming_other_artifacts_absent(self):
        other = self.root / 'other-cargo-target'; other.mkdir()
        sentinel = other / 'pinvou3_lib-keep'; sentinel.write_text('other location')
        self.assertIsNone(PRUNE.prune_root_test_artifacts(self.root))
        self.assertEqual(sentinel.read_text(), 'other location')
        self.assertFalse(self.target.exists())

    def test_existing_location_only_removes_top_level_regular_root_package_files(self):
        self.target.mkdir(parents=True)
        for name in ['pinvou3_lib-abc', 'libpinvou3_lib-def.rlib', 'libthird_party-xyz.rlib', 'keep.txt']:
            (self.target / name).write_text(name)
        nested = self.target / 'nested'; nested.mkdir()
        (nested / 'pinvou3_lib-nested').write_text('keep nested')
        self.assertEqual(PRUNE.prune_root_test_artifacts(self.root), 2)
        self.assertEqual(sorted(x.name for x in self.target.iterdir()), ['keep.txt', 'libthird_party-xyz.rlib', 'nested'])
        self.assertEqual((nested / 'pinvou3_lib-nested').read_text(), 'keep nested')

    def test_non_directory_is_not_treated_as_absent(self):
        self.target.parent.mkdir(parents=True); self.target.write_text('not a directory')
        with self.assertRaises(NotADirectoryError):
            PRUNE.prune_root_test_artifacts(self.root)
        self.assertEqual(self.target.read_text(), 'not a directory')

    def test_stat_permission_failure_is_not_treated_as_absent(self):
        original = Path.lstat
        def denied(path, *args, **kwargs):
            if path == self.target: raise PermissionError(errno.EACCES, 'controlled denial')
            return original(path, *args, **kwargs)
        with patch.object(Path, 'lstat', denied), self.assertRaises(PermissionError):
            PRUNE.prune_root_test_artifacts(self.root)

    def test_unlink_failure_remains_a_failure(self):
        self.target.mkdir(parents=True); item = self.target / 'pinvou3_lib-abc'; item.write_text('retained')
        original = Path.unlink
        def denied(path, *args, **kwargs):
            if path == item: raise PermissionError(errno.EACCES, 'controlled denial')
            return original(path, *args, **kwargs)
        with patch.object(Path, 'unlink', denied), self.assertRaises(PermissionError):
            PRUNE.prune_root_test_artifacts(self.root)
        self.assertEqual(item.read_text(), 'retained')

    def symlink(self, link, target, *, directory=False):
        try:
            link.symlink_to(target, target_is_directory=directory)
        except OSError as error:
            if getattr(error, 'winerror', None) == 1314 or error.errno in {errno.EPERM, errno.ENOTSUP}:
                self.skipTest('native symlink creation is unavailable')
            raise

    def test_linked_cleanup_directory_is_rejected(self):
        self.target.parent.mkdir(parents=True)
        real = self.root / 'another-directory'; real.mkdir()
        sentinel = real / 'pinvou3_lib-keep'; sentinel.write_text('keep')
        self.symlink(self.target, real, directory=True)
        with self.assertRaises(NotADirectoryError):
            PRUNE.prune_root_test_artifacts(self.root)
        self.assertEqual(sentinel.read_text(), 'keep')

    def test_ancestor_link_cannot_reclaim_outside_workspace(self):
        with tempfile.TemporaryDirectory(prefix='pinvou-independent-artifacts-') as other:
            outside = Path(other); deps = outside / 'debug/deps'; deps.mkdir(parents=True)
            sentinel = deps / 'pinvou3_lib-keep'; sentinel.write_text('independent')
            link = self.root / 'pinvou3-app/src-tauri/target'; link.parent.mkdir(parents=True)
            self.symlink(link, outside, directory=True)
            with self.assertRaises(ValueError): PRUNE.prune_root_test_artifacts(self.root)
            self.assertEqual(sentinel.read_text(), 'independent')

    def test_workflow_keeps_real_test_cleanup_and_telemetry_order_and_error_propagation(self):
        workflow = (ROOT / '.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        job = workflow.split('\n  rust-test:', 1)[1].split('\n  cli-test:', 1)[0]
        before, cleanup = job.split('- name: 回收已完成的主测试产物', 1)
        cleanup, after = cleanup.split('- name:', 1)
        self.assertIn('- name: cargo test --lib（', before)
        self.assertIn('python3 scripts/prune-rust-test-artifacts.py', cleanup)
        self.assertIn('df -h "$GITHUB_WORKSPACE"', cleanup)
        self.assertNotIn('|| true', cleanup)
        self.assertNotIn('continue-on-error', cleanup)
        self.assertIn('cargo test --test behavior_telemetry', after)


if __name__ == '__main__':
    unittest.main()

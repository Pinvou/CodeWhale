import errno
import importlib.util
from pathlib import Path
import shutil
import subprocess
import sys
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
        self.root = Path(self.scope.name).resolve()
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

    def test_both_build_layouts_reclaim_only_root_package_top_level_files(self):
        build = self.target.parent / 'build'
        layouts = ['pinvou3-tauri-0123456789abcdef/out',
                   'pinvou3-tauri/fedcba9876543210/out']
        retained = []
        for layout in layouts:
            out = build / layout; out.mkdir(parents=True)
            (out / 'pinvou3_lib-test').write_text('root binary')
            (out / 'libpinvou3_lib-test.rlib').write_text('root library')
            for name in ['build-script-build', 'libdependency.rlib']:
                p = out / name; p.write_text('keep'); retained.append(p)
            nested = out / 'nested'; nested.mkdir()
            p = nested / 'pinvou3_lib-nested'; p.write_text('keep'); retained.append(p)
        for layout in ['other-crate-0123456789abcdef/out', 'pinvou3-tauri-not-a-hash/out',
                       'pinvou3-tauri/not-a-hash/out']:
            out = build / layout; out.mkdir(parents=True)
            p = out / 'pinvou3_lib-foreign'; p.write_text('keep'); retained.append(p)
        self.assertEqual(PRUNE.prune_root_test_artifacts(self.root), 4)
        self.assertTrue(all(p.read_text() == 'keep' for p in retained))
        self.assertEqual(PRUNE.prune_root_test_artifacts(self.root), 0)

    def test_mixed_deps_and_current_build_layout_are_both_reclaimed(self):
        self.target.mkdir(parents=True)
        item = self.target / 'pinvou3_lib-old'; item.write_text('old')
        out = self.target.parent / 'build/pinvou3-tauri/0123456789abcdef/out'; out.mkdir(parents=True)
        binary = out / 'pinvou3_lib-new'; binary.write_text('new')
        self.assertEqual(PRUNE.prune_root_test_artifacts(self.root), 2)
        self.assertFalse(item.exists()); self.assertFalse(binary.exists())

    def test_build_layout_unlink_failure_is_not_treated_as_success(self):
        out = self.target.parent / 'build/pinvou3-tauri/0123456789abcdef/out'; out.mkdir(parents=True)
        item = out / 'pinvou3_lib-new'; item.write_text('retained')
        original = Path.unlink
        def denied(path, *args, **kwargs):
            if path == item: raise PermissionError(errno.EACCES, 'controlled denial')
            return original(path, *args, **kwargs)
        with patch.object(Path, 'unlink', denied), self.assertRaises(PermissionError):
            PRUNE.prune_root_test_artifacts(self.root)
        self.assertEqual(item.read_text(), 'retained')

    def test_build_out_must_be_a_directory(self):
        generation = self.target.parent / 'build/pinvou3-tauri/0123456789abcdef'
        generation.mkdir(parents=True); (generation / 'out').write_text('not a directory')
        with self.assertRaises(NotADirectoryError): PRUNE.prune_root_test_artifacts(self.root)

    def test_linked_build_out_is_rejected_without_touching_its_files(self):
        generation = self.target.parent / 'build/pinvou3-tauri/0123456789abcdef'; generation.mkdir(parents=True)
        elsewhere = self.root / 'elsewhere'; elsewhere.mkdir()
        item = elsewhere / 'pinvou3_lib-keep'; item.write_text('keep')
        self.symlink(generation / 'out', elsewhere, directory=True)
        with self.assertRaises(NotADirectoryError): PRUNE.prune_root_test_artifacts(self.root)
        self.assertEqual(item.read_text(), 'keep')

    def test_matching_file_symlinks_are_not_followed_or_deleted(self):
        out = self.target.parent / 'build/pinvou3-tauri/0123456789abcdef/out'; out.mkdir(parents=True)
        item = self.root / 'independent-file'; item.write_text('keep')
        link = out / 'pinvou3_lib-link'; self.symlink(link, item)
        self.assertEqual(PRUNE.prune_root_test_artifacts(self.root), 0)
        self.assertTrue(link.is_symlink()); self.assertEqual(item.read_text(), 'keep')

    def cli(self):
        script = self.root / 'scripts/prune-rust-test-artifacts.py'; script.parent.mkdir(exist_ok=True)
        shutil.copyfile(ROOT / 'scripts/prune-rust-test-artifacts.py', script)
        return subprocess.run([sys.executable, str(script)], capture_output=True, text=True, timeout=10)

    def test_cli_missing_layouts_is_observable_and_non_directory_is_an_error(self):
        result = self.cli()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('build/pinvou3-tauri', result.stdout)
        self.target.parent.mkdir(parents=True); self.target.write_text('not a directory')
        result = self.cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(self.target.read_text(), 'not a directory')

    def test_cli_reports_real_reclamation_and_empty_existing_layout(self):
        layouts = ['deps', 'build/pinvou3-tauri-0123456789abcdef/out',
                   'build/pinvou3-tauri/fedcba9876543210/out']
        items = []
        for layout in layouts:
            out = self.target.parent / layout; out.mkdir(parents=True)
            item = out / 'pinvou3_lib-new'; item.write_text('root'); items.append(item)
        result = self.cli()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Reclaimed 3 ', result.stdout)
        self.assertTrue(all(not item.exists() for item in items))
        for layout in layouts:
            self.assertIn(f'Selected pinvou3-app/src-tauri/target/debug/{layout}; reclaimed 1 ', result.stdout)
        result = self.cli()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('No matching regular files', result.stdout)
        for layout in layouts:
            self.assertIn(f'Selected pinvou3-app/src-tauri/target/debug/{layout}; reclaimed 0 ', result.stdout)

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

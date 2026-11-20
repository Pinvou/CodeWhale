import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).parents[1] / 'ci-rust-cache-paths.cjs'


class RustCacheWiringTests(unittest.TestCase):
    def test_wrapper_keeps_the_reviewed_upstream_and_compatibility_checks(self):
        root = SCRIPT.parents[1]
        action = (root / '.github/actions/linux-rust-cache/action.yml').read_text()
        self.assertIn('Swatinem/rust-cache@d1527c8f67ef9af357290475d4af4b0fab8f12e0', action)
        self.assertNotIn('add-rust-environment-hash-key: false', action)
        self.assertIn('prefix-key: v0-rust', action, 'existing cache janitor must retain ownership')
        self.assertIn('env-vars: PINVOU_CI_CARGO_SOURCE PINVOU_CI_CACHE_LAYOUT PINVOU_CI_RUST_WRAPPER_SHA256', action)
        self.assertIn('save-if: ${{ inputs.save-if }}', action)
        self.assertIn('cache-on-failure: ${{ inputs.cache-on-failure }}', action)
        self.assertIn("cache-targets: 'false'", action)
        self.assertIn('cache-directories: ${{ steps.paths.outputs.targets }}', action)
        self.assertIn("cache-bin: 'false'", action, 'persistent tools must not be restored/retired as dependency cache')


    def test_lint_fallback_matches_the_writer_workspace_list(self):
        root = SCRIPT.parents[1]
        text = (root / '.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        lint = text.split('\n  rust-lint:', 1)[1].split('\n  rust-test:', 1)[0]
        test = text.split('\n  rust-test:', 1)[1].split('\n  cli-test:', 1)[0]
        for name in ('恢复 Rust lint 暖缓存', '恢复被淘汰缓存的 Rust lint 回退'):
            step = test.split('- name: '+name, 1)[1].split('\n      - name:', 1)[0]
            expected = 'workspaces: |\n            pinvou3-app/src-tauri\n            pinvou-knowledge'
            self.assertIn(expected, step)
            self.assertIn(expected, lint)
            self.assertIn('save-if: false', step)


@unittest.skipUnless(sys.platform == 'linux' and shutil.which('node'), 'Linux cache aliases require Linux and Node')
class RustCachePathsTests(unittest.TestCase):
    def prepare(self, root, **changes):
        root = Path(root)
        for name in ('cargo', 'rustup', 'workspace'):
            (root / name).mkdir(exist_ok=True)
        values = {**os.environ, 'RUNNER_ENVIRONMENT': 'github-hosted',
                  'CARGO_HOME': str(root / 'cargo'), 'RUSTUP_HOME': str(root / 'rustup'),
                  'GITHUB_WORKSPACE': str(root / 'workspace'), 'CACHE_WORKSPACES': 'app\nknowledge',
                  **changes}
        return subprocess.run(['node', '-e',
            'console.log(JSON.stringify(require(process.argv[1]).prepare(process.env, process.argv[2])))',
            str(SCRIPT), str(root / 'aliases')], env=values, capture_output=True, text=True, timeout=10)

    def test_aliases_are_idempotent_and_write_only_to_the_original_private_home(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            result = self.prepare(root)
            self.assertEqual(result.returncode, 0, result.stderr)
            values = json.loads(result.stdout)
            self.assertEqual(values['targets'], 'app/target\nknowledge/target')
            (Path(values['cargo-home']) / 'sentinel').write_bytes(b'owned')
            self.assertEqual((root / 'cargo/sentinel').read_bytes(), b'owned')
            self.assertEqual(self.prepare(root).stdout, result.stdout)

    def test_aliases_cannot_be_retargeted_to_another_home(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.assertEqual(self.prepare(root).returncode, 0)
            (root / 'other').mkdir()
            result = self.prepare(root, CARGO_HOME=str(root / 'other'))
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual((root / 'aliases/cargo').resolve(), root / 'cargo')

    def test_target_escape_and_writable_alias_root_are_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.assertNotEqual(self.prepare(root, CACHE_WORKSPACES='../outside').returncode, 0)
            os.chmod(root / 'aliases', 0o777)
            self.assertNotEqual(self.prepare(root).returncode, 0)

    def test_registry_only_cache_keeps_compiled_targets_out(self):
        with tempfile.TemporaryDirectory() as directory:
            result = self.prepare(directory, CACHE_TARGETS='false')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout)['targets'], '')

    def test_reviewed_wrapper_uses_a_stable_alias_but_content_changes_invalidate_its_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            wrapper = root / 'workspace/pinvou3-app/src-tauri/scripts/rustc-stack-wrapper'
            wrapper.parent.mkdir(parents=True)
            wrapper.write_text('#!/bin/sh\nexec "$@"\n')
            first = self.prepare(root, RUSTC_WRAPPER=str(wrapper))
            self.assertEqual(first.returncode, 0, first.stderr)
            one = json.loads(first.stdout)
            wrapper.write_text('#!/bin/sh\nexport RUST_MIN_STACK=16777216\nexec "$@"\n')
            two = json.loads(self.prepare(root, RUSTC_WRAPPER=str(wrapper)).stdout)
            self.assertEqual(one['rustc-wrapper'], two['rustc-wrapper'])
            self.assertNotEqual(one['wrapper-sha256'], two['wrapper-sha256'])
            # A later cache probe/restore sees the exported alias, not the old
            # instance pathname. Its compatibility identity must stay equal.
            three = json.loads(self.prepare(root, RUSTC_WRAPPER=two['rustc-wrapper'],
                               CARGO_HOME=two['cargo-home'], RUSTUP_HOME=two['rustup-home']).stdout)
            self.assertEqual(two, three)

    def test_private_cargo_build_configuration_is_not_lost_when_homes_are_normalized(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            one = json.loads(self.prepare(root).stdout)
            (root / 'cargo/config.toml').write_text('[build]\nrustflags=["--cfg", "cache_probe"]\n')
            two = json.loads(self.prepare(root).stdout)
            self.assertNotEqual(one['config-sha256'], two['config-sha256'])


if __name__ == '__main__':
    unittest.main()

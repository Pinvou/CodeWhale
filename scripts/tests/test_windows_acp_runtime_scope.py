"""Private source/CLI boundary contracts; mocked npm is not a real ACP smoke."""
import importlib.util
import io
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('windows_acp_scope', ROOT/'scripts/test-windows-acp-runtime.py')
SCOPE = importlib.util.module_from_spec(spec); spec.loader.exec_module(SCOPE)


def archive(entries):
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode='w') as output:
        for name, kind in entries:
            member = tarfile.TarInfo(name); member.type = kind
            if kind == tarfile.REGTYPE:
                data = b'fixed source'; member.size = len(data); output.addfile(member, io.BytesIO(data))
            else:
                if kind == tarfile.SYMTYPE: member.linkname = '../escape'
                output.addfile(member)
    return raw.getvalue()


class WindowsACPScopeTests(unittest.TestCase):
    def test_materializes_regular_sources_and_rejects_escape_and_links(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            SCOPE.materialize(archive([('pinvou3-app/file', tarfile.REGTYPE)]), base/'good')
            self.assertEqual((base/'good/pinvou3-app/file').read_bytes(), b'fixed source')
            for index, (name, kind) in enumerate([
                ('../escape', tarfile.REGTYPE), ('/pinvou3-app/file', tarfile.REGTYPE),
                ('pinvou3-app/../../escape', tarfile.REGTYPE), ('pinvou3-app/C:/file', tarfile.REGTYPE),
                ('pinvou3-app\\escape', tarfile.REGTYPE), ('pinvou3-app/link', tarfile.SYMTYPE),
            ]):
                with self.subTest(name=name), self.assertRaises(ValueError):
                    SCOPE.materialize(archive([(name, kind)]), base/str(index))
            self.assertFalse((base/'escape').exists())

    def test_supervision_is_required_before_any_git_or_npm(self):
        with patch.object(SCOPE.subprocess, 'check_output') as git:
            with self.assertRaises(RuntimeError): SCOPE.run_smoke(platform='linux', environment={})
            with self.assertRaises(RuntimeError): SCOPE.run_smoke(platform='win32', environment={})
            git.assert_not_called()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); (root/'.pinvou-fixture-scope').write_text('a'*32, encoding='ascii')
            with self.assertRaises(RuntimeError):
                SCOPE.supervised_root({'PINVOU_CI_FIXTURE_ROOT': str(root), 'PINVOU_CI_PROCESS_SCOPE': 'b'*32})

    def test_npm_uses_private_fixed_tree_without_touching_checkout_cache(self):
        for status in (0, 23):
            with self.subTest(status=status), tempfile.TemporaryDirectory() as directory:
                base = Path(directory); scope = base/'owned'; scope.mkdir()
                (scope/'.pinvou-fixture-scope').write_text('a'*32, encoding='ascii')
                checkout = base/'checkout'; cache = checkout/'pinvou3-app/src-tauri/target/windows-runtime/codex-bridge'
                cache.mkdir(parents=True); sentinel = cache/'sentinel'; sentinel.write_text('unchanged', encoding='utf-8')
                env = {'PINVOU_CI_FIXTURE_ROOT': str(scope), 'PINVOU_CI_PROCESS_SCOPE': 'a'*32, 'PATH': 'configured-path'}
                payload = archive([('pinvou3-app/package.json', tarfile.REGTYPE)])
                with patch.object(SCOPE, 'ROOT', checkout), \
                        patch.object(SCOPE.subprocess, 'check_output', side_effect=['f'*40, payload]) as git, \
                        patch.object(SCOPE.shutil, 'which', return_value='configured-npm.cmd'), \
                        patch.object(SCOPE.subprocess, 'run', return_value=subprocess.CompletedProcess([], status)) as run:
                    self.assertEqual(SCOPE.run_smoke(source_ref='e'*40, platform='win32', environment=env), status)
                    first, second = git.call_args_list
                    self.assertEqual(first.args[0], ['git', 'rev-parse', '--verify', 'e'*40+'^{tree}'])
                    self.assertEqual(second.args[0], ['git', 'archive', '--format=tar', 'f'*40, *SCOPE.SOURCE_PATHS])
                    args, options = run.call_args
                    command = args[0]; private_app = Path(command[2])
                    self.assertTrue(private_app.is_relative_to(scope))
                    self.assertFalse(private_app.is_relative_to(checkout))
                    self.assertEqual(command[3:], ['run', 'test:codex-acp-windows-runtime'])
                    self.assertEqual(options['cwd'], private_app.parent)
                    self.assertEqual(options['env'], env)
                    self.assertEqual(options['timeout'], 600)
                    self.assertFalse(options['check'])
                    self.assertTrue(private_app.exists(), 'only the supervisor reclaims owned sources after descendant exit')
                self.assertEqual(sentinel.read_text(encoding='utf-8'), 'unchanged')

    def test_workflow_routes_and_uploads_the_supervised_acp_receipt(self):
        source = (ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        block = source.split('\n  windows-codex-runtime-test:', 1)[1].split('\n  windows-runtime-test:', 1)[0]
        self.assertIn('scripts/ci-fixture-process.py --receipt "${{ runner.temp }}/windows-acp-', block)
        self.assertIn('-- python scripts/test-windows-acp-runtime.py', block)
        smoke = block.split('      - name: Windows ACP bridge runtime smoke\n', 1)[1].split('      - name:', 1)[0]
        self.assertNotIn('continue-on-error:', smoke)
        self.assertIn('name: Preserve ACP fixture exit receipt', block)
        upload = block.split('      - name: Preserve ACP fixture exit receipt\n', 1)[1]
        for setting in ('if: always()', 'timeout-minutes: 3', 'continue-on-error: true',
                        'uses: actions/upload-artifact@v7', 'retention-days: 7', 'if-no-files-found: warn'):
            self.assertIn(setting, upload)
        paths = {line.strip() for line in upload.split('          path: |\n', 1)[1].splitlines() if line.strip()}
        self.assertEqual(paths, {
            '${{ runner.temp }}/windows-acp-${{ github.run_id }}-${{ github.run_attempt }}.json',
            '${{ runner.temp }}/windows-acp-${{ github.run_id }}-${{ github.run_attempt }}-resources/*.json',
        })
        filter_block = source.split('            windows_codex:', 1)[1].split('            windows_runtime:', 1)[0]
        for name in ('scripts/test-windows-acp-runtime.py', 'scripts/tests/test_windows_acp_runtime_scope.py'):
            self.assertIn("'"+name+"'", filter_block)


if __name__ == '__main__': unittest.main()

import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import textwrap
import unittest
from ci_routing_test_support import MATRIX, matrix_rows, default_ci_routing

ROOT = Path(__file__).resolve().parents[2]

class SelfHostedRoutingTests(unittest.TestCase):
    def test_platform_matrices_are_maps_and_linux_survives_the_native_switch(self):
        count = 0
        for path in (ROOT / '.github/workflows').glob('*.yml'):
            source = path.read_text(encoding='utf-8')
            for match in MATRIX.finditer(source):
                count += 1
                enabled = matrix_rows(match.group(2))
                disabled = matrix_rows(match.group(3))
                with self.subTest(workflow=path.name):
                    self.assertTrue(any(str(row.get('os',row.get('runs-on',''))).startswith(('windows','macos')) for row in enabled))
                    self.assertTrue(all(str(row.get('os',row.get('runs-on',''))).startswith('ubuntu-22.04') for row in disabled))
                    self.assertEqual(disabled, [row for row in enabled if str(row.get('os',row.get('runs-on',''))).startswith('ubuntu-22.04')])
        self.assertEqual(count, 4)

    def test_raw_array_and_empty_matrices_are_rejected(self):
        for matrix in ([{'os':'ubuntu-22.04'}], {'include':[]}, {'include':['ubuntu-22.04']}):
            with self.subTest(matrix=matrix), self.assertRaises(ValueError):
                matrix_rows(json.dumps(matrix))

    def test_native_disabled_matrix_does_not_schedule_windows_or_mac(self):
        source=(ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        rendered=default_ci_routing(source,native_enabled=False)
        for name,next_name in [('backflow-portability','fast-gate'),('release-contract-test','knowledge-rust')]:
            job=rendered.split('\n  '+name+':',1)[1].split('\n  '+next_name+':',1)[0]
            self.assertIn('os: [ubuntu-22.04]',job)

    def test_self_hosted_disk_calls_never_request_sdk_deletion(self):
        count = 0
        for path in (ROOT/'.github/workflows').glob('*.yml'):
            for line in path.read_text(encoding='utf-8').splitlines():
                if 'exec python3 scripts/ci-rust-disk.py --check-only' in line:
                    count += 1
                    self.assertNotIn('--aggressive', line)
        self.assertGreaterEqual(count, 9)

    @unittest.skipUnless(shutil.which('bash'), 'requires bash')
    def test_native_switch_keeps_linux_relay_required(self):
        source=(ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        gate=source.split('\n  required-gate:',1)[1]
        script=textwrap.dedent(gate.split('\n        run: |\n',1)[1])
        result_names=re.findall(r'^          ([A-Z0-9_]+_RESULT):',gate,re.M)
        env=dict(os.environ, **{name:'success' for name in result_names})
        env.update(GITHUB_EVENT_NAME='pull_request',RELAY_REQUIRED_BY_POLICY='true',WINDOWS_RUST_REQUIRED_BY_POLICY='false',WINDOWS_MAC_ENABLED='false')
        for name in result_names:
            if name.startswith(('WINDOWS_', 'MACOS_')): env[name]='skipped'
        for enabled,linux,expected in [('false','success',0),('false','failure',1),('true','success',1)]:
            with self.subTest(enabled=enabled,linux=linux):
                env.update(WINDOWS_MAC_ENABLED=enabled,RELAY_RESULT=linux)
                result=subprocess.run(['bash','-c',script],env=env,capture_output=True,timeout=30)
                self.assertEqual(result.returncode,expected,result.stderr)

    @unittest.skipUnless(shutil.which('bash'), 'requires bash')
    def test_self_hosted_memory_setup_preserves_existing_configuration(self):
        env=dict(os.environ,RUNNER_ENVIRONMENT='self-hosted')
        result=subprocess.run(['bash',str(ROOT/'scripts/ci-memory-setup.sh')],env=env,capture_output=True,text=True,timeout=30)
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertIn('preserving administrator-provisioned memory and swap',result.stdout)

    @unittest.skipUnless(shutil.which('bash'), 'requires bash')
    def test_self_hosted_cache_guard_only_enables_save_after_capacity_success(self):
        source=(ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        block=source.split('      - name: Guard CLI cache post-save\n',1)[1].split('\n  windows-rust-test:',1)[0]
        script=textwrap.dedent(block.split('        run: |\n',1)[1])
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            output=Path(directory)/'github-env'
            env=dict(os.environ,RUNNER_ENVIRONMENT='self-hosted',GITHUB_ENV=output.as_posix())
            for status in (0,1):
                with self.subTest(status=status):
                    output.unlink(missing_ok=True)
                    env['FAKE_DISK_EXIT']=str(status)
                    result=subprocess.run(['bash','-eo','pipefail','-c','python3() { return "$FAKE_DISK_EXIT"; }; sudo() { return 99; };\n'+script],env=env,capture_output=True,timeout=30)
                    self.assertEqual(result.returncode,status,result.stderr)
                    self.assertEqual(output.exists(),status==0)
                    if status==0: self.assertEqual(output.read_text().strip(),'CACHE_ON_FAILURE=true')

if __name__ == '__main__':
    unittest.main()

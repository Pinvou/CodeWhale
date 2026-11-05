import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class RunnerPoolPolicyTests(unittest.TestCase):
    def test_main_jobs_keep_compilation_out_of_the_light_pool(self):
        workflow = (ROOT / '.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        expected = {
            'LIGHT': ['commit-message', 'version-consistency', 'changes', 'fast-gate', 'required-gate', 'backflow-portability'],
            'MEDIUM': ['frontend-test', 'relay-test', 'team-relay-hub-test', 'update-release-console-test', 'release-contract-test'],
            'HEAVY': ['knowledge-rust', 'rust-lint', 'rust-test', 'cli-test', 'cli-lint', 'rust-loongarch64-check'],
        }
        jobs = dict(re.findall(r'(?ms)^  ([\w-]+):\n(.*?)(?=^  [\w-]+:\n|\Z)', workflow))
        for pool, names in expected.items():
            for name in names:
                with self.subTest(job=name):
                    route = next(line for line in jobs[name].splitlines() if 'runs-on:' in line)
                    self.assertIn(f'vars.PINVOU_LINUX_{pool}_RUNNER || vars.PINVOU_LINUX_RUNNER', route)
                    self.assertIn('["ubuntu-22.04"]', route)

    def test_postgres_uses_a_job_allocated_port(self):
        source = (ROOT / '.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        job = source.split('\n  team-relay-hub-test:', 1)[1].split('\n  update-release-console-test:', 1)[0]
        self.assertIn('- 5432/tcp', job)
        self.assertNotIn('5432:5432', job)
        self.assertIn("job.services.postgres.ports['5432']", job)
        self.assertIn('--memory 2g', job)

    def test_console_override_keeps_production_ports_and_paths_out_of_ci(self):
        source = (ROOT / 'update-release-console/deploy/compose.ci.yaml').read_text(encoding='utf-8')
        self.assertIn('ports: !override', source)
        self.assertIn('127.0.0.1::8792', source)
        self.assertIn('volumes: !override', source)
        self.assertIn('PINVOU_UPDATE_CI_PUBLIC_ROOT:?', source)
        self.assertNotIn('/var/www', source)

    @unittest.skipUnless(os.name != 'nt' and shutil.which('bash'), 'Linux CI shell integration')
    def test_console_checks_and_failure_cleanup_use_only_the_job_project(self):
        stub = '''
docker() {
  printf '%s|%s\n' "$COMPOSE_PROJECT_NAME" "$*" >> "$COMMAND_LOG"
  if [[ "$*" == *'port console 8792'* ]]; then echo '127.0.0.1:43781'; fi
  if [[ "$*" == *'up --detach --build'* ]]; then return "$UP_STATUS"; fi
  return 0
}
curl() { printf '%s|curl %s\n' "$COMPOSE_PROJECT_NAME" "$*" >> "$COMMAND_LOG"; }
export -f docker curl
'''
        script = ROOT / 'scripts/ci-update-release-console-container.sh'
        with tempfile.TemporaryDirectory() as directory:
            for run_id, status in [('1001', '0'), ('1002', '17')]:
                with self.subTest(run_id=run_id, status=status):
                    log = Path(directory) / (run_id + '.log')
                    env = dict(os.environ, RUNNER_TEMP=directory, GITHUB_RUN_ID=run_id, GITHUB_RUN_ATTEMPT='1', PINVOU_UPDATE_ADMIN_TOKEN='ci-fixture', COMMAND_LOG=str(log), UP_STATUS=status, PINVOU_UPDATE_NODE_IMAGE="ci-fixture-node-image")
                    result = subprocess.run(['bash', '-c', stub + '\nbash ' + shlex.quote(str(script))], env=env, cwd=ROOT, capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, int(status), result.stderr)
                    commands = log.read_text()
                    self.assertIn(f'pinvou-ci-{run_id}-1|', commands)
                    self.assertIn('down --volumes --remove-orphans', commands)
                    self.assertNotIn('/var/www', commands)
                    if status == '0':
                        self.assertIn('127.0.0.1:43781/pinvou3/release-console/api/status', commands)
                    self.assertFalse(list(Path(directory).glob('release-console.*')))
                    self.assertFalse(list(Path(directory).glob('*.path')))


if __name__ == '__main__':
    unittest.main()

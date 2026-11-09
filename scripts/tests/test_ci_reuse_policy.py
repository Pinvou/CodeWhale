"""Queue coverage, matrix isolation and unknown-condition fallback regressions."""
import importlib.util
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('reuse_policy', ROOT/'scripts/ci-reuse-policy.py')
PLAN = importlib.util.module_from_spec(SPEC); SPEC.loader.exec_module(PLAN)


class ReusePlanTests(unittest.TestCase):
    def test_independent_workflows_require_all_platforms_and_real_test_steps(self):
        for filename, name, gate in (
            ('rustc-wrapper-smoke.yml', 'smoke', 'wrapper-gate'),
            ('evidence-validator-container.yml', 'linux-amd64', 'validator-gate'),
        ):
            with self.subTest(workflow=filename):
                source = (ROOT/'.github/workflows'/filename).read_text(encoding='utf-8')
                policy = PLAN.catalog(source, jobs={name})[name]
                rows = PLAN.matrix_rows(policy, {})
                self.assertIsNotNone(rows)
                jobs = [{'name': PLAN.display_name(policy, row), 'conclusion': 'success',
                         'labels': PLAN.labels(policy, row, {}),
                         'steps': [{'name': step['name'], 'conclusion': 'success'}
                                   for step in policy['steps'] if step['name']]}
                        for row in rows]
                self.assertTrue(PLAN.complete_group(policy, jobs, {}, 'refs/heads/queue', {}))
                jobs[-1]['steps'][-1]['conclusion'] = 'skipped'
                self.assertFalse(PLAN.complete_group(policy, jobs, {}, 'refs/heads/queue', {}))
                self.assertIn('  '+gate+':', source)
                self.assertIn('[[ "$TEST_RESULT" == success || ( "$TEST_RESULT" == skipped && "$REUSED" == true ) ]]', source)
                self.assertIn('CI_REUSE_WORKFLOW: .github/workflows/'+filename, source)

    def test_yaml_step_names_match_api_names_without_swallowing_quoted_hashes(self):
        self.assertEqual(PLAN.literal('cargo test # ignored comment'), 'cargo test')
        self.assertEqual(PLAN.literal('"cargo # literal" # comment'), 'cargo # literal')
        self.assertEqual(PLAN.literal("'cargo # literal' # comment"), 'cargo # literal')

    def test_conditions_use_three_valued_logic_and_never_execute_input(self):
        context = {'github.event_name': 'merge_group', 'matrix.phase': 'regression', 'env.RUN_HEAVY_RUST_CHECKS': 'true'}
        for expression, expected in (
            ("github.event_name == 'push'", False),
            ("github.event_name != 'push'", True),
            ("matrix.phase == 'regression' && env.RUN_HEAVY_RUST_CHECKS == 'true'", True),
            ("always() && !cancelled()", True),
            ("unknown.field && false", False),
            ("unknown.field || true", True),
        ):
            with self.subTest(expression=expression): self.assertIs(PLAN.condition(expression, context), expected)
        for expression in ("unknown.field", "unknown()", "contains(x, y)", "__import__('os')", "x['value']", "a + b", "("):
            self.assertIs(PLAN.condition(expression, context), PLAN.UNKNOWN)

    def workflow(self):
        return '''name: fixture
jobs:
  suite:
    name: suite (${{ matrix.os }})
    if: ${{ true }}
    strategy:
      matrix:
        os: [ubuntu-22.04, windows-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - name: Linux test
        if: ${{ runner.os == 'Linux' }}
        run: test-linux
      - name: Windows test
        if: ${{ runner.os == 'Windows' }}
        run: test-windows
      - name: Main cache warm
        if: ${{ github.event_name == 'push' }}
        run: compile-only
      - name: Queue full regression
        if: ${{ env.RUN_HEAVY_RUST_CHECKS == 'true' }}
        run: full-regression
'''

    def job(self, os='ubuntu-22.04'):
        return {'name': f'suite ({os})', 'labels': [os], 'conclusion': 'success', 'steps': [
            {'name': 'Linux test', 'conclusion': 'success' if os.startswith('ubuntu') else 'skipped'},
            {'name': 'Windows test', 'conclusion': 'success' if os.startswith('windows') else 'skipped'},
            {'name': 'Main cache warm', 'conclusion': 'skipped'},
            {'name': 'Queue full regression', 'conclusion': 'success'}]}

    def test_each_os_requires_its_own_successful_regression(self):
        policy = PLAN.catalog(self.workflow(), jobs={'suite'})['suite']
        for os in ('ubuntu-22.04', 'windows-latest'):
            job = self.job(os)
            self.assertTrue(PLAN.complete(policy, job, {}, 'refs/heads/queue', {}))
            for status in ('failure', 'cancelled', 'skipped'):
                job['steps'][-1]['conclusion'] = status
                self.assertFalse(PLAN.complete(policy, job, {}, 'refs/heads/queue', {}))

    def test_unknown_conditions_require_execution_and_wrong_labels_reject(self):
        policy = PLAN.catalog(self.workflow().replace("env.RUN_HEAVY_RUST_CHECKS == 'true'", 'unknown.output'), jobs={'suite'})['suite']
        job = self.job()
        self.assertTrue(PLAN.complete(policy, job, {}, 'refs/heads/queue', {}))
        job['steps'][-1]['conclusion'] = 'skipped'
        self.assertFalse(PLAN.complete(policy, job, {}, 'refs/heads/queue', {}))
        job = self.job(); job['labels'] = ['different-runner']
        self.assertFalse(PLAN.complete(policy, job, {}, 'refs/heads/queue', {}))

    def test_missing_duplicate_and_empty_step_evidence_reject(self):
        policy = PLAN.catalog(self.workflow(), jobs={'suite'})['suite']
        for steps in ([], self.job()['steps'][:-1], self.job()['steps'] + [self.job()['steps'][-1]]):
            job = self.job(); job['steps'] = steps
            self.assertFalse(PLAN.complete(policy, job, {}, 'refs/heads/queue', {}))

    def test_matrix_group_rejects_missing_or_duplicate_legs(self):
        policy = PLAN.catalog(self.workflow(), jobs={'suite'})['suite']
        jobs = [self.job(), self.job('windows-latest')]
        self.assertTrue(PLAN.complete_group(policy, jobs, {}, 'refs/heads/queue', {}))
        for incomplete in (jobs[:1], jobs + [jobs[0]]):
            self.assertFalse(PLAN.complete_group(policy, incomplete, {}, 'refs/heads/queue', {}))

    def test_full_queue_rust_coverage_cannot_reuse_a_format_only_pr(self):
        source = (ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        policy = PLAN.catalog(source)['rust-lint']
        job = {'name': 'rust-lint', 'conclusion': 'success', 'labels': ['ubuntu-22.04'],
               'steps': [{'name': step['name'], 'conclusion': 'success'} for step in policy['steps'] if step['name']]}
        self.assertTrue(PLAN.complete_group(policy, [job], {}, 'refs/heads/queue', {}))
        for step in job['steps']:
            if step['name'].startswith('cargo clippy'): step['conclusion'] = 'skipped'
        self.assertFalse(PLAN.complete_group(policy, [job], {}, 'refs/heads/queue', {}))

    def test_context_jobs_outputs_unknown_runners_and_unnamed_runs_stay_live(self):
        for name in PLAN.CONTEXT_JOBS:
            self.assertNotIn(name, PLAN.catalog(self.workflow().replace('  suite:', f'  {name}:'), jobs={name}))
        self.assertNotIn('suite', PLAN.catalog(self.workflow()))
        for addition in ('    outputs:\n      artifact: value\n', '    uses: ./reusable.yml\n'):
            self.assertNotIn('suite', PLAN.catalog(self.workflow().replace('    steps:', addition+'    steps:'), jobs={'suite'}))
        self.assertNotIn('suite', PLAN.catalog(self.workflow().replace('      - name: Linux test\n', '      - shell: bash\n'), jobs={'suite'}))
        policy = PLAN.catalog(self.workflow().replace('runs-on: ${{ matrix.os }}', 'runs-on: ${{ unknown.runner }}'), jobs={'suite'})['suite']
        self.assertFalse(PLAN.complete(policy, self.job(), {}, 'refs/heads/queue', {}))

    def test_real_catalog_covers_all_current_product_and_platform_jobs(self):
        source = (ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        policies = PLAN.catalog(source)
        for name in ('version-consistency', 'relay-test', 'account-platform-test', 'frontend-test',
                     'team-relay-hub-test', 'release-contract-test', 'update-release-console-test',
                     'rust-test', 'rust-lint', 'knowledge-rust', 'cli-test', 'cli-lint',
                     'windows-rust-test', 'windows-codex-runtime-test', 'windows-runtime-test',
                     'macos-rust-check', 'macos-cli-check', 'rust-loongarch64-check'):
            self.assertIn(name, policies)
            job = source
            self.assertIn("'"+name+"'))", job)
        self.assertFalse(set(policies) & PLAN.CONTEXT_JOBS)

    def test_real_release_matrix_requires_every_configured_platform(self):
        source = (ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        policy = PLAN.catalog(source)['release-contract-test']
        rows = PLAN.matrix_rows(policy, {})
        self.assertEqual(rows, [{'os': 'ubuntu-22.04'}, {'os': 'windows-latest'}])
        jobs = [{'name': PLAN.display_name(policy, row), 'conclusion': 'success', 'labels': [row['os']],
                 'steps': [{'name': step['name'], 'conclusion': 'success'} for step in policy['steps'] if step['name']]}
                for row in rows]
        self.assertTrue(PLAN.complete_group(policy, jobs, {}, 'refs/heads/queue', {}))
        self.assertFalse(PLAN.complete_group(policy, jobs[:1], {}, 'refs/heads/queue', {}))
        self.assertTrue(PLAN.complete_group(policy, jobs[:1], {}, 'refs/heads/queue', {'PINVOU_WINDOWS_ENABLED': 'false'}))

    def test_every_current_product_recipe_has_a_complete_verifiable_plan(self):
        source = (ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        policies = PLAN.catalog(source)
        self.assertGreaterEqual(len(policies), 23)
        for name, policy in policies.items():
            with self.subTest(job=name):
                rows = PLAN.matrix_rows(policy, {})
                self.assertIsNotNone(rows)
                jobs = [{'name': PLAN.display_name(policy, row), 'conclusion': 'success',
                         'labels': PLAN.labels(policy, row, {}),
                         'steps': [{'name': step['name'], 'conclusion': 'success'}
                                   for step in policy['steps'] if step['name']]}
                        for row in rows]
                self.assertTrue(PLAN.complete_group(policy, jobs, {}, 'refs/heads/queue', {}))


if __name__ == '__main__': unittest.main()

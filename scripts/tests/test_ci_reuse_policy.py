"""Queue coverage, matrix isolation and unknown-condition fallback regressions."""
import copy
import importlib.util
import json
from pathlib import Path
import re
import unittest
from test_windows_runner_trust import configured_pools, context, evaluate_route, labels as route_labels

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('reuse_policy', ROOT/'scripts/ci-reuse-policy.py')
PLAN = importlib.util.module_from_spec(SPEC); SPEC.loader.exec_module(PLAN)


class ReusePlanTests(unittest.TestCase):
    def trusted_runner_plans(self):
        for filename, allowed in (('pr-check.yml', None), ('rustc-wrapper-smoke.yml', {'smoke'})):
            source = (ROOT/'.github/workflows'/filename).read_text(encoding='utf-8')
            policies = PLAN.catalog(source, jobs=allowed)
            for name, policy in policies.items():
                block = re.search(r'^  '+re.escape(name)+r':\n(.*?)(?=^  [\w-]+:|\Z)', source, re.M | re.S)[1]
                # Keep the actual workflow expression independent of the
                # verifier's normalized recipe, then replay its real routing.
                runner = next(line.removeprefix('    runs-on: ')
                              for line in block.splitlines() if line.startswith('    runs-on: '))
                if 'github.' in runner:
                    yield filename, name, runner, policy

    @staticmethod
    def successful_job(policy, row, labels):
        return {'name': PLAN.display_name(policy, row), 'conclusion': 'success', 'labels': labels,
                'steps': [{'name': step['name'], 'conclusion': 'success'}
                          for step in policy['steps'] if step['name']]}

    def test_trusted_windows_recipes_match_actual_pr_and_queue_routing(self):
        plans = list(self.trusted_runner_plans())
        self.assertEqual(len(plans), 9)
        for configuration in ({}, configured_pools(),
                {'PINVOU_WINDOWS_RUNNER': '["self-hosted","Windows","legacy"]',
                 'PINVOU_LINUX_RUNNER': '["self-hosted","Linux","legacy"]'}):
            for filename, name, runner, policy in plans:
                for row in PLAN.matrix_rows(policy, configuration):
                    expected = PLAN.labels(policy, row, configuration)
                    for event in ('pull_request', 'merge_group', 'push'):
                        actual = context(event, configuration)
                        actual['matrix'] = row
                        with self.subTest(workflow=filename, job=name, row=row, event=event,
                                          configured=bool(configuration)):
                            self.assertEqual(route_labels(evaluate_route(runner, actual)), expected)
                            job = self.successful_job(policy, row, expected)
                            self.assertTrue(PLAN.complete(policy, job, {}, 'refs/heads/queue', configuration))

    def test_fork_missing_and_mismatched_identity_cannot_witness_private_queue_pool(self):
        configuration = configured_pools()
        changes = (
            ('head-name', lambda value: value['github']['event']['pull_request']['head']['repo'].update(full_name='fork/project')),
            ('base-name', lambda value: value['github']['event']['pull_request']['base']['repo'].update(full_name='other/project')),
            ('head-id', lambda value: value['github']['event']['pull_request']['head']['repo'].update(id=999)),
            ('base-id', lambda value: value['github']['event']['pull_request']['base']['repo'].update(id=999)),
            ('missing-head', lambda value: value['github']['event']['pull_request']['head'].pop('repo')),
            ('missing-base', lambda value: value['github']['event']['pull_request']['base'].pop('repo')),
            ('missing-id', lambda value: value['github'].pop('repository_id')),
            ('missing-repository', lambda value: value['github'].pop('repository')),
        )
        for filename, name, runner, policy in self.trusted_runner_plans():
            for row in PLAN.matrix_rows(policy, configuration):
                if row.get('os', 'windows-latest') != 'windows-latest': continue
                for failure, mutate in changes:
                    actual = context(variables=configuration)
                    actual['matrix'] = row
                    mutate(actual)
                    with self.subTest(workflow=filename, job=name, row=row, failure=failure):
                        labels = evaluate_route(runner, actual)
                        self.assertEqual(labels, ['windows-latest'])
                        job = self.successful_job(policy, row, labels)
                        self.assertFalse(PLAN.complete(policy, job, {}, 'refs/heads/queue', configuration))

    def test_modified_context_runner_recipes_remain_excluded(self):
        changes = (
            ("github.repository_id != '' && ", ''),
            ('github.event.pull_request.head.repo.full_name == github.repository && ', ''),
            ('github.event.pull_request.base.repo.full_name == github.repository && ', ''),
            ('github.event.pull_request.head.repo.id == github.repository_id && ', ''),
            ('github.event.pull_request.base.repo.id == github.repository_id', 'true'),
            ("github.event_name == 'merge_group'", "github.event_name == 'workflow_dispatch'"),
            ('vars.PINVOU_WINDOWS_RUNNER', 'vars.UNKNOWN_WINDOWS_RUNNER'),
        )
        for filename, name, runner, _ in self.trusted_runner_plans():
            for old, new in changes:
                changed = runner.replace(old, new)
                with self.subTest(workflow=filename, job=name, mutation=old):
                    self.assertNotEqual(changed, runner)
                    source = 'name: fixture\njobs:\n  suite:\n    runs-on: '+changed+'\n    steps:\n      - name: Required test\n        run: test\n'
                    self.assertNotIn('suite', PLAN.catalog(source, jobs={'suite'}))
            if 'matrix.os' in runner:
                for old, new in (("matrix.os == 'windows-latest'", "matrix.os != 'ubuntu-22.04'"),
                                 (' || matrix.os }}', " || 'windows-latest' }}")):
                    source = 'name: fixture\njobs:\n  suite:\n    runs-on: '+runner.replace(old, new)+'\n    steps:\n      - name: Required test\n        run: test\n'
                    with self.subTest(workflow=filename, job=name, mutation=old):
                        self.assertNotIn('suite', PLAN.catalog(source, jobs={'suite'}))

    def test_real_ready_frontend_evidence_accepts_optional_failures_and_maps_smoke(self):
        policies = PLAN.catalog((ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8'))
        policy = policies['frontend-browser-test']
        fixture = json.loads((ROOT/'scripts/tests/fixtures/ci-reuse-ready-frontend.json').read_text(encoding='utf-8'))
        job = {'name': 'frontend-browser-test (1)', 'conclusion': 'success', 'labels': ['ubuntu-22.04'], 'steps': fixture['steps']}
        self.assertTrue(PLAN.complete(policies['frontend-test'], {**job, 'name': 'frontend-test'}, {}, 'refs/heads/queue', {}))
        self.assertTrue(PLAN.complete(policy, job, {}, 'refs/heads/queue', {}))
        # Real PR shape: queue-only smoke skipped, warning skipped, PR smoke
        # passed. Diagnostic delivery is advisory even if its action fails.
        optional_failure = copy.deepcopy(job)
        for step in optional_failure['steps']:
            if step['name'] == 'Test resource lifecycle receipts': step['conclusion'] = 'failure'
        self.assertTrue(PLAN.complete(policy, optional_failure, {}, 'refs/heads/queue', {}))
        for bad in ('failure', 'skipped', 'missing'):
            broken = copy.deepcopy(optional_failure)
            for step in broken['steps']:
                if step['name'] == 'Ready PR 定向浏览器 smoke': step['conclusion'] = bad
                if step['name'] == 'Merge Queue diff-selected browser smoke': step['conclusion'] = 'success'
            if bad == 'missing': broken['steps'] = [s for s in broken['steps'] if s['name'] != 'Ready PR 定向浏览器 smoke']
            with self.subTest(pr_smoke=bad):
                self.assertFalse(PLAN.complete(policy, broken, {}, 'refs/heads/queue', {}))
        broken = copy.deepcopy(job)
        for step in broken['steps']:
            if step['name'] == 'Confirm test resource retirement': step['conclusion'] = 'skipped'
        self.assertFalse(PLAN.complete(policy, broken, {}, 'refs/heads/queue', {}))

    def test_browser_shard_reuse_requires_the_exact_complete_matrix_and_retirement(self):
        policy = PLAN.catalog((ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8'))['frontend-browser-test']
        rows = PLAN.matrix_rows(policy, {})
        self.assertEqual(rows, [{'shard': str(i)} for i in range(1, 5)])
        jobs = [self.successful_job(policy, row, ['ubuntu-22.04']) for row in rows]
        self.assertTrue(PLAN.complete_group(policy, jobs, {}, 'refs/heads/queue', {}))
        for broken in (jobs[:-1], jobs + [jobs[0]], [{**jobs[0], 'name': 'frontend-test'}] + jobs[1:]):
            self.assertFalse(PLAN.complete_group(policy, broken, {}, 'refs/heads/queue', {}))
        for status in ('failure', 'cancelled', 'skipped'):
            broken = copy.deepcopy(jobs)
            broken[2]['conclusion'] = status
            self.assertFalse(PLAN.complete_group(policy, broken, {}, 'refs/heads/queue', {}))
        broken = copy.deepcopy(jobs)
        for step in broken[2]['steps']:
            if step['name'] == 'Confirm test resource retirement': step['conclusion'] = 'skipped'
        self.assertFalse(PLAN.complete_group(policy, broken, {}, 'refs/heads/queue', {}))

    def test_named_cache_and_run_only_diagnostics_do_not_replace_required_tests(self):
        real = PLAN.catalog((ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8'))
        diagnostic = next(step for step in real['windows-rust-test']['steps']
                          if step['name'] == 'Windows 测试二进制导入诊断')
        self.assertTrue(diagnostic['optional'])
        source = '''name: fixture
jobs:
  suite:
    runs-on: ubuntu-22.04
    steps:
      - name: Dependency cache
        uses: actions/cache@v4
      - name: Advisory diagnostic
        continue-on-error: true
        run: diagnose
      - name: Required contract
        run: test
'''
        policy = PLAN.catalog(source, jobs={'suite'})['suite']
        job = {'name': 'suite', 'conclusion': 'success', 'labels': ['ubuntu-22.04'], 'steps': [
            {'name': 'Dependency cache', 'conclusion': 'failure'},
            {'name': 'Advisory diagnostic', 'conclusion': 'failure'},
            {'name': 'Required contract', 'conclusion': 'success'}]}
        self.assertTrue(PLAN.complete(policy, job, {}, 'refs/heads/queue', {}))
        job['steps'][-1]['conclusion'] = 'skipped'
        self.assertFalse(PLAN.complete(policy, job, {}, 'refs/heads/queue', {}))

    def test_handcrafted_unnamed_run_policy_cannot_reuse_arbitrary_run_evidence(self):
        policy = PLAN.catalog(self.workflow(), jobs={'suite'})['suite']
        policy['steps'].append({'name': None, 'run': True, 'if': None, 'optional': False})
        job = self.job()
        job['steps'].append({'name': 'Run unrelated-command', 'conclusion': 'success'})
        self.assertFalse(PLAN.complete(policy, job, {}, 'refs/heads/queue', {}))

    def test_unrecognized_step_name_cannot_silently_lose_queue_required_work(self):
        # Legal YAML beyond the supported literal-name dialect. If a future
        # parser silently drops this queue-only step, the other successful
        # steps would wrongly suffice. Conservatively reject this PR witness.
        source = self.workflow() + '''      - name: >-
          Queue-only additional contract
        if: ${{ github.event_name == 'merge_group' }}
        run: additional-contract
'''
        policy = PLAN.catalog(source, jobs={'suite'})['suite']
        job = self.job()
        job['steps'].append({'name': 'Queue-only additional contract', 'conclusion': 'skipped'})
        self.assertFalse(PLAN.complete(policy, job, {}, 'refs/heads/queue', {}))

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
        for name in ('version-consistency', 'relay-test', 'account-platform-test', 'frontend-test', 'frontend-browser-test',
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

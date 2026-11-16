"""Reject incomplete financial fan-out and preserve independent ownership boundaries."""
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import unittest
from unittest.mock import patch
from test_ci_gate_policy import _step_named, _step_run_lines

ROOT = Path(__file__).resolve().parents[2]


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


CHECK = module('account_financial_ci', 'scripts/check-account-financial-ci.py')
PG = module('account_financial_profiles', 'scripts/account-ci-postgres.py')


def job(source, name):
    match = re.search(r'^  ' + re.escape(name) + r':\n(.*?)(?=^  [\w-]+:|\Z)', source, re.M | re.S)
    if match is None:
        raise AssertionError('missing job ' + name)
    return match[1]


class FinancialCIMatrixTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = (ROOT / '.github/workflows/pr-check.yml').read_text(encoding='utf-8')

    def test_actual_matrix_matches_the_existing_cli_contract(self):
        self.assertCountEqual(CHECK.verify_groups(self.source), CHECK.GROUPS)
        marker = '        group: [owners, token, billing-0, billing-1, billing-2, billing-3]'
        permuted = self.source.replace(marker, '        group: [token, owners, billing-3, billing-2, billing-1, billing-0]')
        self.assertCountEqual(CHECK.verify_groups(permuted), CHECK.GROUPS)

    def test_missing_duplicate_unknown_or_unsupported_groups_are_refused(self):
        marker = '        group: [owners, token, billing-0, billing-1, billing-2, billing-3]'
        invalid = ['[owners, token, billing-0, billing-1, billing-2]',
                   '[owners, token, billing-0, billing-1, billing-2, billing-2]',
                   '[owners, token, billing-0, billing-1, billing-2, other]', '[]']
        for groups in invalid:
            with self.subTest(groups=groups), self.assertRaises(ValueError):
                CHECK.verify_groups(self.source.replace(marker, '        group: ' + groups))
        with self.assertRaises(ValueError):
            CHECK.verify_groups(self.source.replace(marker, marker + '\n        extra: [one, two]'))

    def test_fail_fast_or_serial_configuration_is_refused(self):
        block = job(self.source, 'account-financial-test')
        for wrong in (block.replace('      fail-fast: false', '      fail-fast: true'),
                      block.replace('      max-parallel: 6', '      max-parallel: 1'),
                      block.replace('      fail-fast: false', '      fail-fast: false\n      fail-fast: true')):
            with self.assertRaises(ValueError):
                CHECK.verify_groups(self.source.replace(block, wrong, 1))

    def test_core_and_financial_services_are_not_shared_or_left_unused(self):
        core = job(self.source, 'account-platform-test')
        financial = job(self.source, 'account-financial-test')
        closure = job(self.source, 'account-closure-test')
        services = lambda text: text.split('    services:\n', 1)[1].split('    steps:\n', 1)[0]
        self.assertEqual(re.findall(r'^      ([\w-]+):$', services(core), re.M),
                         ['audit-export-postgres', 'existing-broker-postgres'])
        self.assertEqual(re.findall(r'^      ([\w-]+):$', services(financial), re.M), ['financial-postgres'])
        self.assertEqual(re.findall(r'^      ([\w-]+):$', services(closure), re.M), ['financial-postgres'])
        self.assertIn('    needs: changes\n', financial)
        self.assertNotIn('needs: account-platform-test', financial)
        self.assertNotIn('scripts/test-financial-parallel.py', core)
        self.assertIn('--group "$ACCOUNT_FINANCIAL_GROUP"', financial)
        self.assertNotIn('ACCOUNT_FINANCIAL_SERVICES:', financial)
        self.assertIn('ACCOUNT_FINANCIAL_GROUP: ${{ matrix.group }}', financial)
        for key in ('url', 'port', 'system', 'directory'):
            self.assertIn('${{ steps.account-pg.outputs.financial_' + key + ' }}', financial)
        for text, profile in ((core, 'core'), (financial, 'financial'), (closure, 'closure')):
            self.assertIn('scripts/account-ci-postgres.py --profile ' + profile, text)
            self.assertIn('--profile ' + profile + ' --stop-owned-services', text)
            stop = text.index('      - name: Stop owned temporary PostgreSQL services')
            self.assertIn('if: ${{ always() }}', text[stop:])
            self.assertIn('ci-resource-ledger.py --recover', text[stop:])
        for text in (core, financial):
            self.assertIn('      GOTOOLCHAIN: local\n', text)
            self.assertIn('      CGO_ENABLED: "1"\n', text)
        for option in ('--rm', '--cpus 2', '--memory 1g', '--memory-swap 1g'):
            self.assertIn('          ' + option + '\n', financial)
        self.assertEqual(financial.count('"127.0.0.1::5432"'), 1)

    def test_actual_go_node_consumers_have_locked_dependencies_before_execution(self):
        def assert_preparation(workflow):
            financial = job(workflow, 'account-financial-test')
            node = _step_named(financial, 'Node.js for financial contract consumers')
            install = _step_named(financial, 'Install financial contract dependencies')
            consumers = _step_named(financial, 'Verify financial contract Node consumers')
            execute = _step_named(financial, 'Run isolated financial group')
            self.assertIn('uses: actions/setup-node@v7', node)
            self.assertIn('node-version: "24"', node)
            self.assertEqual(re.findall(r'^        working-directory: (.+)$', install, re.M), ['account-platform'])
            self.assertEqual(_step_run_lines(install), ['npm ci --ignore-scripts --prefer-offline --no-audit'])
            self.assertEqual(re.findall(r'^        working-directory: (.+)$', consumers, re.M), ['account-platform'])
            self.assertEqual(_step_run_lines(consumers), [
                'node --input-type=module -e "await import(\'./test/fixtures/approval-recovery-validator.mjs\'); await import(\'./test/fixtures/financial-event-validator.mjs\')"'])
            for step in (node, install, consumers):
                self.assertNotRegex(step, re.compile(r'^        (?:if|continue-on-error):', re.M), 'preparation must not be optional')
                self.assertLess(financial.index(step), financial.index(execute))
            self.assertLess(financial.index(node), financial.index(install))
            self.assertLess(financial.index(install), financial.index(consumers))
            return node, install, consumers

        steps = assert_preparation(self.source)
        for step in steps:
            with self.subTest(missing=step.splitlines()[0]), self.assertRaises(AssertionError):
                assert_preparation(self.source.replace(step, '', 1))
            conditional = step.replace('\n', "\n        if: ${{ false }}\n", 1)
            with self.subTest(conditional=step.splitlines()[0]), self.assertRaises(AssertionError):
                assert_preparation(self.source.replace(step, conditional, 1))
        # These actual Go consumers use the fixture modules above; ordinary
        # synthetic CI/gate success does not exercise their package imports.
        token = (ROOT / 'account-platform/internal/token/approval_recovery_openapi_test.go').read_text(encoding='utf-8')
        self.assertIn('"approval-recovery-validator.mjs"', token)
        billing = (ROOT / 'account-platform/internal/billing/ledger_audit_runbook_integration_test.go').read_text(encoding='utf-8')
        self.assertIn('"financial-event-validator.mjs"', billing)
        package = json.loads((ROOT / 'account-platform/package.json').read_text(encoding='utf-8'))
        for dependency in ('ajv', 'ajv-formats'):
            self.assertIn(dependency, package['devDependencies'])

    def test_policy_regressions_prepare_an_explicit_owned_python_runtime(self):
        def assert_runtime(workflow):
            core = job(workflow, 'account-platform-test')
            setup = _step_named(core, 'Python for financial runner policy regressions')
            regressions = _step_named(core, 'Financial runner policy regressions')
            self.assertIn('id: financial-policy-python', setup)
            self.assertIn('uses: actions/setup-python@v7', setup)
            self.assertIn("python-version: '3.12'", setup)
            self.assertIn('update-environment: false', setup)
            self.assertNotRegex(setup, re.compile(r'^        (?:if|continue-on-error):', re.M))
            self.assertLess(core.index(setup), core.index(regressions))
            self.assertEqual(_step_run_lines(regressions), [
                'set -euo pipefail',
                'python3 -m unittest discover -s scripts/tests -p test_token_financial_postgres.py',
                'python3 -m unittest discover -s scripts/tests -p test_financial_parallel.py',
                'python3 -m unittest discover -s scripts/tests -p test_account_financial_families_postgres.py',
                'python3 -m unittest discover -s scripts/tests -p test_go_test_contract.py',
                'policy_python="${{ steps.financial-policy-python.outputs.python-path }}"',
                'policy_library="$(dirname "$(dirname "$policy_python")")/lib"',
                'python3 scripts/ci-fixture-process.py --receipt "$PINVOU_CI_RESOURCE_LEDGER/financial-policy-process.json" -- env "LD_LIBRARY_PATH=$policy_library${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" "$policy_python" -m unittest discover -s scripts/tests -p test_account_financial_ci.py',
            ])
            return setup, regressions

        setup, regressions = assert_runtime(self.source)
        variants = [self.source.replace(setup, '', 1),
                    self.source.replace(setup, setup.replace("'3.12'", "'3.10'"), 1),
                    self.source.replace(setup, setup.replace('update-environment: false', 'update-environment: true'), 1),
                    self.source.replace(regressions, regressions.replace('"${{ steps.financial-policy-python.outputs.python-path }}"', 'python3'), 1),
                    self.source.replace(regressions, regressions.replace('env "LD_LIBRARY_PATH=$policy_library${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" ', ''), 1),
                    self.source.replace(regressions, regressions.replace(' -- env "LD_LIBRARY_PATH=', ' -- export "LD_LIBRARY_PATH='), 1)]
        for wrong in variants:
            with self.assertRaises(AssertionError):
                assert_runtime(wrong)

    def test_wrong_profile_metadata_fails_before_any_docker_command(self):
        cases = [('core', {'financial-postgres': {}}),
                 ('financial', {'audit-export-postgres': {}}),
                 ('full', {'financial-postgres': {}}),
                 ('closure', {'financial-postgres': {}, 'foreign-postgres': {}})]
        for profile, metadata in cases:
            with self.subTest(profile=profile), patch.dict(os.environ, {'ACCOUNT_CI_SERVICES': json.dumps(metadata)}), \
                    patch.object(PG, 'run', side_effect=AssertionError('unexpected Docker command')) as run:
                with self.assertRaises(ValueError):
                    PG.main(services=PG.SERVICE_PROFILES[profile])
                run.assert_not_called()

    def test_new_job_is_mandatory_and_reusable_only_as_a_complete_group(self):
        gate = job(self.source, 'required-gate')
        self.assertIn('      - account-financial-test\n', gate)
        self.assertIn('ACCOUNT_FINANCIAL_RESULT: ${{ needs.account-financial-test.result }}', gate)
        self.assertIn('"account-financial-test:ACCOUNT_FINANCIAL_RESULT"', gate)
        self.assertIn('"account-financial-test:$ACCOUNT_FINANCIAL_RESULT"', gate)
        self.assertIn('"$ACCOUNT_PLATFORM_REQUIRED" == "true" && "$ACCOUNT_FINANCIAL_RESULT" != success', gate)
        policy = CHECK.PLAN['catalog'](self.source)['account-financial-test']
        rows = CHECK.PLAN['matrix_rows'](policy, {})
        expected = [CHECK.PLAN['display_name'](policy, row) for row in rows]
        self.assertEqual(len(expected), 6)
        # Failures here are group-completeness failures, independent of step detail.
        successful = [{'name': name} for name in expected]
        for incomplete in (successful[:-1], successful[:-1] + [successful[0]],
                           successful + [{'name': 'account-financial-test (other)'}]):
            self.assertFalse(CHECK.PLAN['complete_group'](policy, incomplete, {}, 'queue', {}))

    def test_actual_gate_refuses_incomplete_financial_outcomes(self):
        gate = job(self.source, 'required-gate')
        step = _step_named(gate, '汇总所有适用检查')
        script = '\n'.join(_step_run_lines(step))
        bash = shutil.which('bash')
        if os.name == 'nt':
            candidate = Path(os.environ.get('ProgramFiles', 'C:/Program Files')) / 'Git/bin/bash.exe'
            if candidate.is_file():
                bash = str(candidate)
        self.assertIsNotNone(bash, 'the actual CI aggregation requires Bash')
        bindings = re.findall(r'^          ([A-Z][A-Z0-9_]+): ', step, re.M)
        defaults = {key: 'success' if key.endswith('_RESULT') else 'false' for key in bindings}
        defaults.update(ACCOUNT_PLATFORM_REQUIRED='true', CI_REUSE_IDS='')
        cases = [('pull_request', 'success', False, True),
                 ('merge_group', 'skipped', True, True),
                 ('merge_group', 'skipped', False, False)]
        cases += [(event, result, reuse, False) for event in ('pull_request', 'merge_group')
                  for result in ('failure', 'cancelled', '') for reuse in (False, True)]
        for event, result, reuse, accepted in cases:
            env = {**os.environ, **defaults, 'GITHUB_EVENT_NAME': event,
                   'ACCOUNT_FINANCIAL_RESULT': result,
                   'CI_REUSE_VERIFIED': 'true' if reuse else 'false',
                   'CI_REUSE_IDS': 'account-financial-test' if reuse else ''}
            with self.subTest(event=event, result=result, reuse=reuse):
                run = subprocess.run([bash, '-c', script], env=env, capture_output=True, text=True, timeout=15)
                self.assertEqual(run.returncode == 0, accepted, run.stdout + run.stderr)


if __name__ == '__main__':
    unittest.main()

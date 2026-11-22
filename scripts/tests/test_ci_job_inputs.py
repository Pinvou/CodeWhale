"""Input-boundary negative cases independent of Actions/GitHub API fixtures."""
import importlib.util
import os
from pathlib import Path
import runpy
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('job_inputs', ROOT/'scripts/ci-job-inputs.py')
INPUTS = importlib.util.module_from_spec(SPEC); SPEC.loader.exec_module(INPUTS)
WORKFLOW = (ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')


class InputTests(unittest.TestCase):
    def signature(self, entries, changed=(), configuration=None, routing=None, name='rust-test'):
        return INPUTS.fingerprint(name, WORKFLOW, entries, changed, configuration or {}, routing or {})

    def test_transitive_inputs_modes_symlinks_deletes_and_submodules_matter(self):
        for path in ('pinvou3-app/src-tauri/src/lib.rs', 'pinvou3-app/package-lock.json',
                     'pinvou-cli/Cargo.lock', 'pinvou-knowledge/Cargo.toml',
                     'remote-control-relay/package-lock.json', '.gitmodules',
                     'scripts/fixture.py', '.github/actions/setup/action.yml',
                     'unknown-domain/fixture.txt', 'CodeWhale', 'private-runtimes/windows'):
            old = ('160000' if path in ('CodeWhale', 'private-runtimes/windows') else '100644',
                   'commit' if path in ('CodeWhale', 'private-runtimes/windows') else 'blob', 'a'*40, path)
            with self.subTest(path=path):
                before = self.signature([old])
                self.assertNotEqual(before, self.signature([(*old[:2], 'b'*40, path)]))
                self.assertNotEqual(before, self.signature([]))
                self.assertNotEqual(before, self.signature([('100755', *old[1:])]))
                self.assertNotEqual(before, self.signature([('120000', *old[1:])]))
                self.assertNotEqual(before, self.signature([(*old[:-1], path+'.renamed')]))

    def test_diff_selected_tests_and_relevant_routing_cannot_reuse_other_selection(self):
        before = self.signature([], name='frontend-browser-test')
        self.assertNotEqual(before, self.signature([], changed=[('modified', 'pinvou3-app/src/foo.js')], name='frontend-browser-test'))
        self.assertNotEqual(before, self.signature([], routing={'frontend': 'true'}, name='frontend-browser-test'))
        self.assertNotEqual(self.signature([]), self.signature([], routing={'rust_full': 'true'}))

    def test_backflow_mode_only_changes_signatures_that_can_observe_it(self):
        pr_route = {'backflow_mode': 'windows_full', 'frontend': 'true', 'release_contract': 'true'}
        queue_route = {**pr_route, 'backflow_mode': 'non_rust'}
        for name in ('version-consistency', 'frontend-test', 'frontend-browser-test', 'release-contract-test'):
            with self.subTest(job=name):
                self.assertEqual(self.signature([], routing=pr_route, name=name),
                                 self.signature([], routing=queue_route, name=name))
        self.assertNotEqual(self.signature([], routing=pr_route, name='windows-rust-test'),
                            self.signature([], routing=queue_route, name='windows-rust-test'))

    def test_opaque_or_producer_needs_reads_keep_backflow_mode(self):
        for condition in ('fromJSON(toJSON(needs.changes.outputs)).backflow_mode',
                          "needs.changes.outputs['backflow_mode']", 'needs.changes.outputs.backflow_mode'):
            workflow = WORKFLOW.replace('  frontend-setup:\n',
                                        f'  frontend-setup:\n    env:\n      MODE: ${{{{ {condition} }}}}\n', 1)
            with self.subTest(condition=condition):
                self.assertNotEqual(
                    INPUTS.fingerprint('frontend-test', workflow, [], [], {}, {'backflow_mode': 'windows_full'}),
                    INPUTS.fingerprint('frontend-test', workflow, [], [], {}, {'backflow_mode': 'non_rust'}))

    def test_no_implicit_contract_or_empty_unsupported_proof(self):
        self.assertIsNone(self.signature([], name='new-unknown-job'))
        self.assertEqual(self.signature([]), self.signature([('100644', 'blob', 'a'*40, 'account-platform/new.go')]))
        # Other jobs retain full tree coverage until a narrower closure is audited.
        self.assertNotEqual(self.signature([], name='frontend-test'),
                            self.signature([('100644', 'blob', 'a'*40, 'account-platform/new.go')], name='frontend-test'))

    def test_workflow_cannot_add_a_direct_domain_dependency_without_contract_update(self):
        for name, domains in INPUTS.CONTRACTS.items():
            context = INPUTS.contexts(WORKFLOW, name)
            for root in INPUTS.DOMAINS-domains:
                with self.subTest(job=name, root=root):
                    self.assertNotIn(root+'/', context)

    def test_other_workflows_keep_the_original_proof_profile(self):
        for workflow in ('rustc-wrapper-smoke.yml', 'evidence-validator-container.yml'):
            with self.subTest(workflow=workflow), patch.dict(os.environ, {'CI_REUSE_WORKFLOW': '.github/workflows/'+workflow}):
                module = runpy.run_path(str(ROOT/'scripts/ci-test-reuse.py'))
                self.assertFalse(module['JOB_SCOPED'])
                self.assertIsNone(module['INPUTS'])
                self.assertNotIn('scripts/ci-job-inputs.py', module['POLICY'])


if __name__ == '__main__':
    unittest.main()

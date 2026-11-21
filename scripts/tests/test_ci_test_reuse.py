"""Exercise real Git trees and adversarial success evidence without network access."""
import copy
from datetime import datetime, timezone, timedelta
from functools import lru_cache
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import runpy
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest
import urllib.request
from unittest.mock import patch
import zipfile

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('ci_test_reuse', ROOT/'scripts/ci-test-reuse.py')
REUSE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(REUSE)
TARGETS = ('account-platform-test', 'frontend-test')
NOW = datetime(2026, 11, 9, 10, tzinfo=timezone.utc)


class ReuseTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.repository_temp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.repository_temp.cleanup)
        fixture = cls()
        fixture.root = Path(cls.repository_temp.name)
        fixture.git('init', '-q')
        fixture.git('config', 'user.name', 'Fixture')
        fixture.git('config', 'user.email', 'fixture@example.invalid')
        for path in REUSE.POLICY:
            file = fixture.root/path
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_text('fixed policy\n', encoding='utf-8')
        fixture.git('add', '.')
        fixture.git('commit', '-qm', 'base')
        fixture.base = fixture.git('rev-parse', 'HEAD')
        (fixture.root/'product.txt').write_text('tested code\n', encoding='utf-8')
        fixture.git('add', '.')
        fixture.git('commit', '-qm', 'PR')
        fixture.head = fixture.git('rev-parse', 'HEAD')
        fixture.tree = fixture.git('rev-parse', 'HEAD^{tree}')
        fixture.tested = fixture.git('commit-tree', fixture.tree, '-p', fixture.base, '-p', fixture.head, '-m', 'PR test merge')
        fixture.queued = fixture.git('commit-tree', fixture.tree, '-p', fixture.base, '-p', fixture.head, '-m', 'queue merge')
        cls.revisions = {name: getattr(fixture, name) for name in ('base', 'head', 'tree', 'tested', 'queued')}
        # These objects are immutable across the negative-evidence cases. Read
        # each object once instead of launching hundreds of identical Git reads.
        cls.cached_git = staticmethod(lru_cache(None)(fixture.git))

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.repository_temp.name)
        for name, value in self.revisions.items():
            setattr(self, name, value)
        self.config = {'PINVOU_CI_REUSE_EPOCH': 'runner-image-1', 'PRIVATE_CONFIGURATION': 'must never appear in a proof'}
        self.repository = 'owner/repository'
        self.pr = {'number': 7, 'draft': False, 'head': {'sha': self.head, 'repo': {'full_name': self.repository}},
                   'base': {'sha': self.base, 'ref': 'main'}}
        self.event = {'merge_group': {'head_sha': self.queued, 'base_sha': self.base, 'base_ref': 'refs/heads/main',
                                     'head_ref': f'refs/heads/gh-readonly-queue/main/pr-7-{self.base}'}}
        self.environment = patch.dict(os.environ, {'GITHUB_REPOSITORY': self.repository, 'TESTED_SHA': self.queued,
                                                    'GITHUB_RUN_ID': '12', 'GITHUB_RUN_ATTEMPT': '1',
                                                    'GITHUB_OUTPUT': str(Path(self.temp.name)/'output'), 'RUNNER_TEMP': self.temp.name})
        self.environment.start(); self.addCleanup(self.environment.stop)
        self.root_patch = patch.object(REUSE, 'ROOT', self.root)
        self.root_patch.start(); self.addCleanup(self.root_patch.stop)
        git_patch = patch.object(REUSE, 'git', side_effect=self.cached_git)
        git_patch.start(); self.addCleanup(git_patch.stop)
        self.expected = REUSE.fingerprint(self.queued, self.base, self.config)
        self.proof = {'schema': 1, 'repository': self.repository, 'pr': 7, 'run': 12, 'attempt': 1,
                      'head': self.head, 'tested': self.tested, 'base_ref': 'main', 'fingerprint': copy.deepcopy(self.expected),
                      'created': NOW.isoformat(), 'jobs': sorted(TARGETS)}
        self.run = {'id': 12, 'run_attempt': 1, 'event': 'pull_request', 'conclusion': 'success', 'head_sha': self.head,
                    'path': REUSE.WORKFLOW, 'head_repository': {'full_name': self.repository},
                    'pull_requests': [{'number': 7, 'head': {'sha': self.head}, 'base': {'sha': self.base, 'ref': 'main'}}],
                    'updated_at': NOW.isoformat()}
        self.jobs = [{'name': 'required-gate', 'conclusion': 'success', 'run_id': 12}]
        self.jobs += [{'name': name, 'conclusion': 'success', 'run_id': 12, 'labels': ['ubuntu-22.04'],
                       'steps': [{'name': step, 'conclusion': 'success'} for step in [item['name'] for item in REUSE.JOBS[name]['steps'] if item['name']]]}
                      for name in TARGETS]
        self.artifact = {'id': 99, 'name': 'ci-success-proof-1', 'expired': False, 'size_in_bytes': 1024,
                         'workflow_run': {'id': 12, 'head_sha': self.head}}
        owner = self
        class API:
            def request(self, path, *, binary=False):
                if path == 'pulls/7': return copy.deepcopy(owner.pr)
                if path.startswith('actions/workflows/pr-check.yml/runs?'):
                    return {'workflow_runs': [copy.deepcopy(owner.run)]}
                if path == 'actions/artifacts/99/zip': return owner.archive()
                if path == f'git/commits/{owner.tested}':
                    return {'tree': {'sha': owner.tree}, 'parents': [{'sha': owner.base}, {'sha': owner.head}]}
                raise AssertionError('unexpected evidence request: ' + path)
            def pages(self, path, field):
                if path == 'actions/runs/12/artifacts': return [copy.deepcopy(owner.artifact)]
                if path == 'actions/runs/12/attempts/1/jobs': return copy.deepcopy(owner.jobs)
                raise AssertionError('unexpected evidence page: ' + path)
        self.api = API()

    def git(self, *args):
        return subprocess.check_output(['git', *args], cwd=self.root, text=True, encoding='utf-8', stderr=subprocess.DEVNULL, timeout=120).strip()

    def archive(self):
        output = io.BytesIO()
        with zipfile.ZipFile(output, 'w') as archive:
            archive.writestr('proof.json', json.dumps(self.proof))
        return output.getvalue()

    def lookup(self, configuration=None):
        return REUSE.lookup(self.event, self.config if configuration is None else configuration, self.api, now=NOW)

    def test_different_commit_ids_with_identical_real_trees_reuse_success(self):
        self.assertNotEqual(self.tested, self.queued)
        self.assertEqual(self.lookup(), (sorted(TARGETS), 12))

    def test_frontend_reuses_a_real_tree_proof_across_non_consumed_backflow_mode(self):
        self.proof['schema'] = 2
        self.proof['jobs'] = ['frontend-test']
        routing = {'frontend': 'true'}
        pr_routing = {**routing, 'backflow_mode': 'windows_full'}
        self.proof['job_inputs'] = REUSE.job_inputs(self.tested, self.base, self.config, pr_routing)
        with patch.dict(os.environ, {'CI_REUSE_ROUTING_FILTERS': json.dumps(routing),
                                     'CI_REUSE_ROUTING_POLICY': '{"backflow_mode":"non_rust"}'}):
            self.assertEqual(self.lookup(), (['frontend-test'], 12))

    def test_unreadable_artifact_allows_another_verified_candidate(self):
        later_run = {**self.run, 'id': 13}
        later_artifact = {**self.artifact, 'id': 100, 'workflow_run': {'id': 13, 'head_sha': self.head}}
        api_request, api_pages = self.api.request, self.api.pages

        def pages(path, field):
            if path == 'actions/runs/13/artifacts':
                return [later_artifact]
            return api_pages(path, field)

        for error, reason in ((OSError('private download detail'), 'artifact_download_failed'),
                              (None, 'artifact_decode_failed')):
            for keep_valid in (True, False):
                with self.subTest(reason=reason, keep_valid=keep_valid):
                    def request(path, *, binary=False):
                        if path.startswith('actions/workflows/'):
                            return {'workflow_runs': [later_run, self.run] if keep_valid else [later_run]}
                        if path == 'actions/artifacts/100/zip':
                            if error:
                                raise error
                            return b'private invalid archive contents'
                        return api_request(path, binary=binary)

                    with (patch.object(self.api, 'request', side_effect=request),
                          patch.object(self.api, 'pages', side_effect=pages),
                          patch('sys.stdout', new_callable=io.StringIO) as stdout):
                        expected = (sorted(TARGETS), 12) if keep_valid else ([], None)
                        self.assertEqual(self.lookup(), expected)
                    self.assertIn(f'miss ({reason})', stdout.getvalue())
                    self.assertNotIn('private', stdout.getvalue())

    def scoped_queue(self, path):
        """Real main advance and real two-parent merge, same original PR head."""
        self.git('checkout', '--detach', self.base)
        file = self.root/path
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_text('new main input\n', encoding='utf-8')
        self.git('add', '--', path)
        self.git('commit', '-qm', 'main input changed')
        advanced = self.git('rev-parse', 'HEAD')
        self.git('merge', '--no-ff', self.head, '-m', 'queue combined tree')
        queued = self.git('rev-parse', 'HEAD')
        group = self.event['merge_group']
        group.update(head_sha=queued, base_sha=advanced,
                     head_ref=f'refs/heads/gh-readonly-queue/main/pr-7-{advanced}')
        environment = patch.dict(os.environ, {'TESTED_SHA': queued})
        environment.start(); self.addCleanup(environment.stop)
        self.proof.update(schema=2, jobs=['rust-test', 'account-platform-test'])
        self.proof['job_inputs'] = REUSE.job_inputs(self.tested, self.base, self.config, {})
        self.jobs = self.jobs[:1] + [
            {'name': name, 'conclusion': 'success', 'run_id': 12, 'labels': ['ubuntu-22.04'],
             'steps': [{'name': step['name'], 'conclusion': 'success'}
                       for step in REUSE.JOBS[name]['steps'] if step['name']]}
            for name in self.proof['jobs']]
        return queued, advanced

    def test_main_account_change_reuses_rust_but_revalidates_account(self):
        queued, advanced = self.scoped_queue('account-platform/internal/queue-input.go')
        self.assertNotEqual(REUSE.fingerprint(queued, advanced, self.config)['tree'], self.tree)
        self.assertEqual(self.lookup(), (['rust-test'], 12))

    def test_main_rust_lock_and_shared_script_and_unknown_path_revalidate(self):
        for path in ('pinvou3-app/src-tauri/Cargo.lock', 'pinvou-cli/Cargo.toml',
                     'scripts/new-fixture.py', 'new-product-domain/input.txt', 'scripts/ci-job-inputs.py'):
            with self.subTest(path=path):
                self.scoped_queue(path)
                self.assertEqual(self.lookup(), ([], None))

    def test_job_config_and_unknown_image_epoch_revalidate(self):
        self.scoped_queue('account-platform/internal/queue-input.go')
        for changes in ({'PINVOU_LINUX_HEAVY_RUNNER': '["other-pool"]'},
                        {'PINVOU_CI_REUSE_EPOCH': 'runner-image-2'},
                        {'NEW_UNKNOWN_RUNTIME_SETTING': 'changed'}):
            with self.subTest(changes=changes):
                self.assertEqual(self.lookup({**self.config, **changes}), ([], None))
        # Another job's light runner setting does not change Rust's own pool.
        self.assertEqual(self.lookup({**self.config, 'PINVOU_WINDOWS_LIGHT_RUNNER': '["other-light"]'}),
                         (['rust-test'], 12))

    def test_artifact_cannot_claim_current_digest_for_a_changed_source(self):
        queued, advanced = self.scoped_queue('pinvou3-app/src-tauri/Cargo.lock')
        self.proof['job_inputs'] = REUSE.job_inputs(queued, advanced, self.config, {})
        self.assertEqual(self.lookup(), ([], None))

    def test_scoped_reuse_still_rejects_partial_failed_or_skipped_execution(self):
        self.scoped_queue('account-platform/internal/queue-input.go')
        for verdict in ('failure', 'cancelled', 'skipped'):
            self.jobs[1]['conclusion'] = verdict
            self.assertEqual(self.lookup(), ([], None))
        self.jobs[1]['conclusion'] = 'success'
        self.jobs[1]['steps'][0]['conclusion'] = 'skipped'
        self.assertEqual(self.lookup(), ([], None))

    def test_scoped_proofs_preserve_age_attempt_fork_and_matrix_fences(self):
        self.scoped_queue('account-platform/internal/queue-input.go')
        original = copy.deepcopy(self.proof)
        for key, value in (('created', (NOW-timedelta(days=2)).isoformat()),
                           ('created', (NOW+timedelta(seconds=1)).isoformat()),
                           ('attempt', 2), ('repository', 'fork/repository')):
            with self.subTest(key=key, value=value):
                self.proof = {**original, key: value}
                self.assertEqual(self.lookup(), ([], None))
        self.proof = original
        self.jobs.append(copy.deepcopy(self.jobs[1]))
        self.assertEqual(self.lookup(), ([], None))  # duplicate execution evidence

    def test_browser_matrix_lookup_requires_all_four_successful_shards(self):
        name = 'frontend-browser-test'
        policy = REUSE.JOBS[name]
        self.proof['jobs'].append(name)
        original = copy.deepcopy(self.jobs)
        shards = [{'name': REUSE.PLAN['display_name'](policy, row), 'conclusion': 'success',
                   'run_id': 12, 'labels': ['ubuntu-22.04'],
                   'steps': [{'name': step['name'], 'conclusion': 'success'}
                             for step in policy['steps'] if step['name']]}
                  for row in REUSE.PLAN['matrix_rows'](policy, self.config)]
        self.jobs = original + shards
        self.assertEqual(self.lookup(), (sorted((*TARGETS, name)), 12))
        for problem in ('missing', 'duplicate', 'failed', 'cancelled', 'retirement'):
            with self.subTest(problem=problem):
                broken = copy.deepcopy(shards)
                if problem == 'missing': broken.pop()
                elif problem == 'duplicate': broken.append(copy.deepcopy(broken[0]))
                elif problem in ('failed', 'cancelled'):
                    broken[2]['conclusion'] = 'failure' if problem == 'failed' else 'cancelled'
                else:
                    for step in broken[2]['steps']:
                        if step['name'] == 'Confirm test resource retirement': step['conclusion'] = 'skipped'
                self.jobs = original + broken
                # Other independently proven jobs may reuse; the incomplete
                # browser group must execute instead of gaining a waiver.
                self.assertEqual(self.lookup(), (sorted(TARGETS), 12))

    def test_every_fingerprint_input_and_source_identity_invalidates(self):
        for key, bad in (('tree', 'f'*40), ('base', 'f'*40), ('configuration', 'bad'), ('policy', {}), ('selection', 'bad')):
            with self.subTest(input=key):
                original=copy.deepcopy(self.proof)
                self.proof['fingerprint'][key]=bad
                self.assertEqual(self.lookup(), ([], None))
                self.proof=original
        for key, bad in (('repository', 'fork/repository'), ('pr', 8), ('run', 13), ('attempt', 2), ('head', 'f'*40), ('schema', 2)):
            with self.subTest(identity=key):
                original=copy.deepcopy(self.proof); self.proof[key]=bad
                self.assertEqual(self.lookup(), ([], None)); self.proof=original
        self.assertEqual(self.lookup({**self.config, 'PINVOU_CI_REUSE_EPOCH': 'runner-image-2'}), ([], None))
        self.assertEqual(self.lookup({**self.config, 'PINVOU_CI_REUSE_FORCE_RUN': 'true'}), ([], None))
        with patch.dict(os.environ, {'CI_REUSE_ROUTING': '{"backflow_mode":"release"}'}):
            self.assertEqual(self.lookup(), ([], None))

    def test_changed_product_tree_cannot_reuse_even_when_pr_head_is_unchanged(self):
        blob=self.git('rev-parse', self.base + '^{tree}')
        with patch.object(REUSE, 'fingerprint', return_value={**self.expected, 'tree': blob}):
            self.assertEqual(self.lookup(), ([], None))

    def test_untrusted_runs_forks_changed_bases_and_wrong_workflows_are_rejected(self):
        for key,bad in (('event','merge_group'),('conclusion','failure'),('head_sha','f'*40),('path','.github/workflows/other.yml'),('head_repository',{'full_name':'fork/repository'}),('pull_requests',[])):
            with self.subTest(field=key):
                original=copy.deepcopy(self.run);self.run[key]=bad
                self.assertEqual(self.lookup(),([],None));self.run=original
        self.pr['head']['repo']['full_name']='fork/repository'
        self.assertEqual(self.lookup(),([],None))

    def test_expired_future_or_ambiguous_proofs_are_rejected(self):
        for delta in (timedelta(days=-2), timedelta(seconds=1)):
            self.proof['created']=(NOW+delta).isoformat()
            self.assertEqual(self.lookup(),([],None))
        self.proof['created']=NOW.isoformat()
        for key,bad in (('expired',True),('size_in_bytes',REUSE.MAX_BYTES+1),('workflow_run',{'id':13,'head_sha':self.head})):
            original=copy.deepcopy(self.artifact);self.artifact[key]=bad
            self.assertEqual(self.lookup(),([],None));self.artifact=original
        self.proof['jobs'].append(self.proof['jobs'][0])
        self.assertEqual(self.lookup(),([],None))

    def test_failed_skipped_or_missing_test_and_cleanup_evidence_never_reuses(self):
        for state in ('failure','cancelled','skipped',None):
            self.jobs[1]['conclusion']=state
            self.assertNotIn(self.jobs[1]['name'],self.lookup()[0])
        self.jobs[1]['conclusion']='success'
        self.jobs[1]['labels']=['different-runner']
        self.assertNotIn(self.jobs[1]['name'],self.lookup()[0])
        self.jobs[1]['labels']=['ubuntu-22.04']
        for step in [item for item in self.jobs[1]['steps'] if item['name'] in (
                'Token financial contracts on isolated PostgreSQL', 'Test and vet all Account Platform Go modules',
                'Confirm Account test resource retirement')]:
            step['conclusion']='skipped'
            self.assertNotIn(self.jobs[1]['name'],self.lookup()[0])
            step['conclusion']='success'
        self.jobs[0]['conclusion']='failure'
        self.assertEqual(self.lookup(),([],None))

    def test_matrix_reuse_requires_every_leg_and_every_queue_required_step(self):
        self.proof['jobs'] = ['windows-rust-test']
        policy = REUSE.JOBS['windows-rust-test']
        gate = self.jobs[0]
        self.jobs = [gate] + [
            {'name': f'windows-rust-test ({phase})', 'conclusion': 'success', 'run_id': 12,
             'labels': ['windows-latest'], 'steps': [{'name': step['name'], 'conclusion': 'success'}
                                                    for step in policy['steps'] if step['name']]}
            for phase in ('all-targets-check', 'regression')]
        self.assertEqual(self.lookup(), (['windows-rust-test'], 12))
        regression = self.jobs.pop()
        self.assertEqual(self.lookup(), ([], None))
        self.jobs.append(regression)
        for step in regression['steps']: step['conclusion'] = 'skipped'
        self.assertEqual(self.lookup(), ([], None))

    def test_financial_matrix_requires_six_exact_successful_groups_and_retirement(self):
        self.proof['jobs'] = ['account-financial-test']
        policy = REUSE.JOBS['account-financial-test']
        rows = REUSE.PLAN['matrix_rows'](policy, self.config)
        self.assertEqual({row['group'] for row in rows},
                         {'owners', 'token', 'billing-0', 'billing-1', 'billing-2', 'billing-3'})
        self.jobs = [self.jobs[0]] + [
            {'name': REUSE.PLAN['display_name'](policy, row), 'conclusion': 'success', 'run_id': 12,
             'labels': ['ubuntu-22.04'],
             'steps': [{'name': step['name'], 'conclusion': 'success'}
                       for step in policy['steps'] if step['name']]}
            for row in rows]
        original = copy.deepcopy(self.jobs)
        self.assertEqual(self.lookup(), (['account-financial-test'], 12))
        for invalid in (original[:-1], original[:-1] + [original[1]]):
            self.jobs = copy.deepcopy(invalid)
            self.assertEqual(self.lookup(), ([], None))
        for index in range(1, 7):
            for verdict in ('failure', 'cancelled', 'skipped', None):
                self.jobs = copy.deepcopy(original)
                self.jobs[index]['conclusion'] = verdict
                self.assertEqual(self.lookup(), ([], None))
            for target in ('Install financial contract dependencies', 'Verify financial contract Node consumers',
                           'Run isolated financial group', 'Confirm Account test resource retirement'):
                self.jobs = copy.deepcopy(original)
                step = next(step for step in self.jobs[index]['steps'] if step['name'] == target)
                step['conclusion'] = 'skipped'
                self.assertEqual(self.lookup(), ([], None))
    def test_windows_private_pool_reuse_binds_actual_labels_and_configuration(self):
        labels = ['self-hosted', 'Windows', 'heavy']
        self.config['PINVOU_WINDOWS_HEAVY_RUNNER'] = json.dumps(labels)
        self.proof['fingerprint'] = REUSE.fingerprint(self.queued, self.base, self.config)
        self.proof['jobs'] = ['windows-rust-test']
        policy = REUSE.JOBS['windows-rust-test']
        self.jobs = [self.jobs[0]] + [
            {'name': f'windows-rust-test ({phase})', 'conclusion': 'success', 'run_id': 12,
             'labels': labels, 'steps': [{'name': step['name'], 'conclusion': 'success'}
                                       for step in policy['steps'] if step['name']]}
            for phase in ('all-targets-check', 'regression')]
        self.assertEqual(self.lookup(), (['windows-rust-test'], 12))
        # A hosted fallback caused by absent/inconsistent PR identity cannot
        # stand in for either configured queue matrix leg.
        for index in (1, 2):
            with self.subTest(hosted_leg=index):
                self.jobs[index]['labels'] = ['windows-latest']
                self.assertEqual(self.lookup(), ([], None))
                self.jobs[index]['labels'] = labels
        changed = {**self.config, 'PINVOU_WINDOWS_HEAVY_RUNNER': '["self-hosted","Windows","other"]'}
        self.assertEqual(self.lookup(changed), ([], None))
        self.pr['head']['repo']['full_name'] = 'fork/repository'
        self.assertEqual(self.lookup(), ([], None))

    def test_queue_policy_edits_and_multi_member_shapes_force_execution(self):
        original=REUSE.git
        def changed(*args):
            if args==('rev-parse',f'{self.base}:{REUSE.POLICY[0]}'): return 'f'*40
            return original(*args)
        with patch.object(REUSE,'git',side_effect=changed):
            self.assertEqual(self.lookup(),([],None))
        self.event['merge_group']['head_ref']='refs/heads/unknown'
        self.assertEqual(self.lookup(),([],None))
        self.event['merge_group']['head_ref']=f'refs/heads/gh-readonly-queue/main/pr-7-{self.base}'
        def parents(*args):
            if args[0]=='cat-file': return f'parent {self.base}\nparent {self.head}\nparent '+ 'f'*40
            return original(*args)
        with patch.object(REUSE,'git',side_effect=parents):
            self.assertEqual(self.lookup(),([],None))

    def test_record_contains_only_successful_jobs_and_hashed_configuration(self):
        (self.root/'.git/shallow').write_text(self.tested+'\n', encoding='utf-8')
        self.cached_git.cache_clear()
        with patch.dict(os.environ,{'TESTED_SHA':self.tested}):
            REUSE.record({'pull_request':self.pr},self.config,{'account-platform-test':{'result':'success'},'frontend-test':{'result':'skipped'}})
        proof=json.loads(next(Path(self.temp.name).glob('ci-success-proof-*/proof.json')).read_text())
        self.assertEqual(proof['jobs'],['account-platform-test'])
        self.assertNotIn(self.config['PRIVATE_CONFIGURATION'],json.dumps(proof))
        self.assertEqual(proof['fingerprint'],self.expected)

    def test_real_workflow_shallow_checkout_records_a_consumable_proof(self):
        source = Path(self.temp.name)/'producer-source'
        source.mkdir()
        def git_at(repo, *args):
            return subprocess.check_output(['git', *args], cwd=repo, text=True,
                                           encoding='utf-8', stderr=subprocess.PIPE, timeout=60).strip()
        git_at(source, 'init', '-q', '-b', 'main')
        git_at(source, 'config', 'user.name', 'Fixture')
        git_at(source, 'config', 'user.email', 'fixture@example.invalid')
        for path in REUSE.POLICY:
            target = source/path
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT/path, target)
        git_at(source, 'add', '.')
        git_at(source, 'commit', '-qm', 'baseline policy')
        base = git_at(source, 'rev-parse', 'HEAD')
        git_at(source, 'switch', '-qc', 'feature')
        (source/'product.txt').write_text('PR input\n', encoding='utf-8')
        git_at(source, 'add', 'product.txt')
        git_at(source, 'commit', '-qm', 'PR input')
        head = git_at(source, 'rev-parse', 'HEAD')
        git_at(source, 'switch', '-q', 'main')
        git_at(source, 'merge', '--no-ff', '-qm', 'tested merge', 'feature')
        tested = git_at(source, 'rev-parse', 'HEAD')
        tree = git_at(source, 'rev-parse', 'HEAD^{tree}')
        queue = git_at(source, 'commit-tree', tree, '-p', base, '-p', head, '-m', 'queue merge')
        git_at(source, 'update-ref', 'refs/heads/queue', queue)
        workflow = (source/REUSE.WORKFLOW).read_text(encoding='utf-8')
        gate = re.search(r'^  required-gate:\n(.*?)(?=^  [\w-]+:|\Z)', workflow, re.M | re.S)[1]
        checkout = re.search(r'^      - name: Checkout\n(.*?)(?=^      - |\Z)', gate, re.M | re.S)[1]
        depth_match = re.search(r'^          fetch-depth: (\d+)$', checkout, re.M)
        depth = int(depth_match[1]) if depth_match else 1  # checkout@v7 default
        producer = Path(self.temp.name)/'producer-checkout'
        # A transport clone genuinely omits parent objects at depth 1. Merely
        # writing .git/shallow in a complete repository does not reproduce that.
        git_at(source, 'clone', '-q', '--no-local', '--depth', str(depth), '--branch', 'main', source.as_uri(), str(producer))
        event_file = Path(self.temp.name)/'producer-event.json'
        pr = {'number': 7, 'draft': False, 'head': {'sha': head, 'repo': {'full_name': self.repository}},
              'base': {'sha': base, 'ref': 'main'}}
        event_file.write_text(json.dumps({'pull_request': pr}), encoding='utf-8')
        record_output = Path(self.temp.name)/'producer-output'
        env = {**os.environ, 'GITHUB_EVENT_NAME': 'pull_request', 'GITHUB_EVENT_PATH': str(event_file),
               'GITHUB_REPOSITORY': self.repository, 'TESTED_SHA': tested,
               'GITHUB_RUN_ID': '12', 'GITHUB_RUN_ATTEMPT': '1', 'GITHUB_OUTPUT': str(record_output),
               'RUNNER_TEMP': self.temp.name, 'CI_REUSE_CONFIGURATION': '{}',
               'CI_REUSE_WORKFLOW': REUSE.WORKFLOW, 'CI_REUSE_ROUTING': '{}',
               'CI_REUSE_NEEDS': json.dumps({'changes': {'outputs': {}}, 'rust-test': {'result': 'success'}})}
        result = subprocess.run([sys.executable, str(producer/'scripts/ci-test-reuse.py'), 'record'],
                                env=env, capture_output=True, text=True, encoding='utf-8', timeout=60)
        self.assertEqual(result.returncode, 0, result.stderr)
        proofs = list(Path(self.temp.name).glob('ci-success-proof-*/proof.json'))
        self.assertEqual(len(proofs), 1, result.stdout)
        self.assertIn('directory='+str(proofs[0].parent), record_output.read_text(encoding='utf-8'))
        proof = json.loads(proofs[0].read_text(encoding='utf-8'))
        self.assertEqual(proof['schema'], 2)
        self.assertEqual(proof['fingerprint']['base'], base)
        # Bridge the actual producer JSON to an independently checked-out queue.
        # GitHub transport and the successful product-step list remain fixtures;
        # this does not claim that real Rust tests or Actions scheduling ran here.
        consumer = Path(self.temp.name)/'queue-checkout'
        git_at(source, 'clone', '-q', '--no-local', '--depth', '2', '--branch', 'queue', source.as_uri(), str(consumer))
        module = runpy.run_path(str(consumer/'scripts/ci-test-reuse.py'))
        policy = module['JOBS']['rust-test']
        run = {'id': 12, 'run_attempt': 1, 'event': 'pull_request', 'conclusion': 'success', 'head_sha': head,
               'path': REUSE.WORKFLOW, 'head_repository': {'full_name': self.repository},
               'pull_requests': [{'number': 7, 'head': {'sha': head}, 'base': {'ref': 'main'}}],
               'updated_at': proof['created']}
        jobs = [{'name': 'required-gate', 'run_id': 12, 'conclusion': 'success'},
                {'name': 'rust-test', 'run_id': 12, 'conclusion': 'success', 'labels': ['ubuntu-22.04'],
                 'steps': [{'name': step['name'], 'conclusion': 'success'} for step in policy['steps'] if step['name']]}]
        archive = io.BytesIO()
        with zipfile.ZipFile(archive, 'w') as z:
            z.writestr('proof.json', proofs[0].read_bytes())
        class API:
            def request(inner, path, **kwargs):
                if path == 'pulls/7': return pr
                if path.startswith('actions/workflows/'): return {'workflow_runs': [run]}
                if path == 'actions/artifacts/99/zip': return archive.getvalue()
                if path == 'git/commits/'+tested:
                    return {'tree': {'sha': tree}, 'parents': [{'sha': base}, {'sha': head}]}
                raise AssertionError(path)
            def pages(inner, path, field):
                if field == 'jobs': return jobs
                if field == 'artifacts':
                    return [{'id': 99, 'name': 'ci-success-proof-1', 'expired': False,
                             'size_in_bytes': len(archive.getvalue()), 'workflow_run': {'id': 12, 'head_sha': head}}]
                raise AssertionError(path)
        event = {'merge_group': {'head_sha': queue, 'base_sha': base, 'base_ref': 'refs/heads/main',
                                'head_ref': f'refs/heads/gh-readonly-queue/main/pr-7-{base}'}}
        with patch.dict(os.environ, {'TESTED_SHA': queue, 'CI_REUSE_ROUTING': '{}'}):
            self.assertEqual(module['lookup'](event, {}, API(), now=datetime.fromisoformat(proof['created'])+timedelta(seconds=1)),
                             (['rust-test'], 12))

    def test_record_uses_tested_merge_parent_when_event_base_is_stale(self):
        self.pr['base']['sha'] = 'f'*40
        with patch.dict(os.environ, {'TESTED_SHA': self.tested}):
            REUSE.record({'pull_request': self.pr}, self.config, {name: {'result': 'success'} for name in TARGETS})
        proof = json.loads(next(Path(self.temp.name).glob('ci-success-proof-*/proof.json')).read_text())
        self.assertEqual(proof['fingerprint'], self.expected)
        self.assertEqual(proof['tested'], self.tested)
        self.proof = proof
        self.proof['created'] = NOW.isoformat()
        # The Actions API association can also be stale; the immutable tested
        # object's parents/tree below remain the authoritative consumer check.
        self.run['pull_requests'][0]['base']['sha'] = 'e'*40
        self.assertEqual(self.lookup(), (sorted(TARGETS), 12))

    def test_ready_smoke_uses_the_tested_merge_diff_after_main_renames(self):
        workflow = (ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        ready = workflow.split('      - name: Ready PR 定向浏览器 smoke\n', 1)[1].split('      - name:', 1)[0]
        self.assertIn('TESTED_SHA: ${{ github.sha }}', ready)
        self.assertIn('PR_HEAD_SHA: ${{ github.event.pull_request.head.sha }}', ready)
        command = textwrap.dedent(ready.split('        run: |\n', 1)[1])
        self.assertEqual(command.count('${{ matrix.shard }}'), 1)
        rows = REUSE.PLAN['matrix_rows'](REUSE.JOBS['frontend-browser-test'], {})
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def git(*args):
                return subprocess.check_output(['git', *args], cwd=root, encoding='utf-8', stderr=subprocess.DEVNULL).strip()
            git('init', '-q'); git('config', 'user.name', 'Fixture'); git('config', 'user.email', 'fixture@example.invalid')
            scripts = root/'scripts'; scripts.mkdir()
            (scripts/'select-frontend-smokes.mjs').write_bytes((ROOT/'scripts/select-frontend-smokes.mjs').read_bytes())
            # Only the final browser executor is replaced. Keep real Git,
            # workflow Bash and selector; assert each worker's actual options.
            (scripts/'run-frontend-smokes.mjs').write_text(
                "import assert from 'node:assert/strict';\n"
                "assert.deepEqual(process.argv.slice(2), ['--selected', '--shard', process.env.EXPECTED_SHARD, '--isolate-network']);\n"
                "process.stdin.pipe(process.stdout);\n", encoding='utf-8')
            old = root/'pinvou3-app/src/features/settings/review.js'
            new = root/'pinvou3-app/src/shared/review.js'
            old.parent.mkdir(parents=True); old.write_text(''.join(f'line {i}\n' for i in range(100)), encoding='utf-8')
            git('add', '.'); git('commit', '-qm', 'base'); stale = git('rev-parse', 'HEAD')
            git('checkout', '-qb', 'main-advanced'); new.parent.mkdir(parents=True)
            git('mv', str(old.relative_to(root)), str(new.relative_to(root))); git('commit', '-qm', 'main rename')
            base = git('rev-parse', 'HEAD')
            git('checkout', '-qb', 'pr', stale)
            old.write_text(old.read_text(encoding='utf-8').replace('line 50', 'edited 50'), encoding='utf-8')
            git('add', '.'); git('commit', '-qm', 'PR edit'); head = git('rev-parse', 'HEAD')
            # The retained Ubuntu runner has Git 2.34. Use its ordinary merge
            # entry point, retaining real rename detection and ordered parents.
            git('checkout', '--detach', base)
            git('merge', '--no-commit', '--no-ff', head)
            git('commit', '-qm', 'tested merge')
            tested = git('rev-parse', 'HEAD')
            self.assertEqual(git('show', '-s', '--format=%P', tested).split(), [base, head])
            def select(start, end):
                return subprocess.check_output([shutil.which('node'), str(scripts/'select-frontend-smokes.mjs')],
                                               input=git('diff', '--name-only', start, end), encoding='utf-8')
            old_selection, expected = select(stale, head), select(base, tested)
            self.assertNotIn('test:bridge-smoke', old_selection)
            self.assertIn('test:bridge-smoke', expected)
            environment = {**os.environ, 'TESTED_SHA': tested, 'PR_HEAD_SHA': head,
                           'BASE_SHA': stale, 'HEAD_SHA': head}
            def execute(shard, changes=None):
                # Actions renders matrix expressions before starting Bash.
                rendered = command.replace('${{ matrix.shard }}', shard)
                return subprocess.run([shutil.which('bash'), '-c', rendered], cwd=root/'pinvou3-app',
                                      env={**environment, 'EXPECTED_SHARD': shard+'/4', **(changes or {})},
                                      capture_output=True, encoding='utf-8')
            for row in rows:
                shard = row['shard']
                with self.subTest(shard=shard):
                    result = execute(shard)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    # Every worker receives the same global, rename-aware list;
                    # actual partition/CLI coverage lives in the runner tests.
                    self.assertEqual(result.stdout, expected)
                    for invalid in ({'TESTED_SHA': head}, {'PR_HEAD_SHA': 'f'*40}):
                        rejected = execute(shard, invalid)
                        self.assertNotEqual(rejected.returncode, 0)
                        self.assertEqual(rejected.stdout, '')

    def test_record_rejections_are_observable_and_do_not_create_proofs(self):
        for problem in ('draft', 'fork', 'head-checkout', 'wrong-head', 'three-parents'):
            pr = copy.deepcopy(self.pr)
            if problem == 'draft': pr['draft'] = True
            if problem == 'fork': pr['head']['repo']['full_name'] = 'fork/repository'
            if problem == 'wrong-head': pr['head']['sha'] = 'f'*40
            tested = self.head if problem == 'head-checkout' else self.tested
            if problem == 'three-parents':
                tested = self.git('commit-tree', self.tree, '-p', self.base, '-p', self.head,
                                  '-p', self.queued, '-m', 'unsupported merge')
            with self.subTest(problem=problem), patch.dict(os.environ, {'TESTED_SHA': tested}), \
                    patch('sys.stdout', new_callable=io.StringIO) as stdout:
                REUSE.record({'pull_request': pr}, self.config, {})
                self.assertIn('::notice::CI success proof not recorded:', stdout.getvalue())
                self.assertNotIn(self.config['PRIVATE_CONFIGURATION'], stdout.getvalue())
                self.assertFalse(list(Path(self.temp.name).glob('ci-success-proof-*')))

    def test_independent_consumer_fences_reject_inconsistent_evidence(self):
        for problem in ('tested-env', 'queue-base-suffix', 'target-ref', 'run-age', 'proof-base-ref',
                        'api-tree', 'api-parents', 'unknown-job'):
            # Each fixture changes one producer/API/queue fact, preserving the
            # other valid witnesses so redundant fences have independent tests.
            with self.subTest(problem=problem):
                event, pr, run, proof = map(copy.deepcopy, (self.event, self.pr, self.run, self.proof))
                commit = {'tree': {'sha': self.tree}, 'parents': [{'sha': self.base}, {'sha': self.head}]}
                environment = {'TESTED_SHA': self.queued}
                if problem == 'tested-env': environment['TESTED_SHA'] = 'f'*40
                if problem == 'queue-base-suffix': event['merge_group']['head_ref'] = 'refs/heads/gh-readonly-queue/main/pr-7-'+'f'*40
                if problem == 'target-ref': pr['base']['ref'] = 'other'
                if problem == 'run-age': run['updated_at'] = (NOW-timedelta(days=2)).isoformat()
                if problem == 'proof-base-ref': proof['base_ref'] = 'other'
                if problem == 'api-tree': commit['tree']['sha'] = 'f'*40
                if problem == 'api-parents': commit['parents'][0]['sha'] = 'f'*40
                if problem == 'unknown-job': proof['jobs'].append('unclassified-job')
                original_request = self.api.request
                def request(path, **kwargs):
                    if path == f'git/commits/{self.tested}': return commit
                    return original_request(path, **kwargs)
                with patch.object(self, 'pr', pr), patch.object(self, 'run', run), patch.object(self, 'proof', proof), \
                        patch.dict(os.environ, environment), patch.object(self.api, 'request', side_effect=request):
                    self.assertEqual(REUSE.lookup(event, self.config, self.api, now=NOW), ([], None))

    def test_filter_cleanup_preserves_declared_routing_keys(self):
        routing = {'changes': 'true', 'product_files': 'true'}
        keys = (*REUSE.ROUTING_KEYS, *routing)
        expected = REUSE.fingerprint
        def fingerprint(tested, base, configuration, actual):
            self.assertEqual(actual['changes'], 'true')
            self.assertEqual(actual['product_files'], 'true')
            self.assertNotIn('unrelated_files', actual)
            return expected(tested, base, configuration, actual)
        with patch.object(REUSE, 'ROUTING_KEYS', keys), patch.object(REUSE, 'fingerprint', side_effect=fingerprint), \
                patch.dict(os.environ, {'CI_REUSE_ROUTING_FILTERS': json.dumps({**routing, 'unrelated_files': ['path']})}):
            self.assertEqual(self.lookup(), ([], None))  # new route inputs invalidate the old proof

    def test_proof_stream_read_is_bounded_even_if_member_metadata_is_wrong(self):
        reads = []
        class Stream(io.BytesIO):
            def read(self, size=-1):
                reads.append(size)
                return super().read(size)
        class Archive:
            def __enter__(self): return self
            def __exit__(self, *args): pass
            def infolist(self): return [type('Member', (), {'filename': 'proof.json', 'file_size': 1})()]
            def open(self, member): return Stream(b'{}'+b' '*REUSE.MAX_BYTES)
        with patch.object(REUSE.zipfile, 'ZipFile', return_value=Archive()), self.assertRaises(ValueError):
            REUSE.read_proof(b'controlled archive')
        self.assertEqual(reads, [REUSE.MAX_BYTES+1])

    def test_archives_are_bounded_and_never_extract_paths(self):
        for name,size in (('../proof.json',1),('proof.json',REUSE.MAX_BYTES+1)):
            output=io.BytesIO()
            with zipfile.ZipFile(output,'w') as archive:archive.writestr(name,' '*size)
            with self.assertRaises(ValueError):REUSE.read_proof(output.getvalue())

    def test_artifact_redirect_never_forwards_github_token(self):
        request=urllib.request.Request('https://api.github.com/repos/owner/repository/actions/artifacts/1/zip',headers={'Authorization':'Bearer private-token'})
        redirected=REUSE.SafeRedirect().redirect_request(request,None,302,'',{},'https://example.blob.core.windows.net/proof')
        self.assertIsNone(redirected.get_header('Authorization'))
        with self.assertRaises(ValueError):
            REUSE.SafeRedirect().redirect_request(request,None,302,'',{},'http://example.invalid/proof')

    def test_incomplete_paginated_job_evidence_is_rejected(self):
        api = REUSE.GitHub('owner/repository', 'private-token')
        with patch.object(api, 'request', side_effect=[{'total_count': 101, 'jobs': [{}] * 100},
                                                     {'total_count': 101, 'jobs': []}]):
            with self.assertRaises(ValueError): api.pages('actions/runs/12/jobs', 'jobs')

    def test_api_failure_falls_back_without_printing_token_or_configuration(self):
        event_path=self.root/'event.json';event_path.write_text(json.dumps(self.event),encoding='utf-8')
        with patch.dict(os.environ,{'GITHUB_EVENT_PATH':str(event_path),'GITHUB_EVENT_NAME':'merge_group','GITHUB_TOKEN':'private-token','CI_REUSE_CONFIGURATION':json.dumps(self.config)}),patch('sys.argv',['ci-test-reuse.py','lookup']),patch.object(REUSE.GitHub,'request',side_effect=OSError('private-token')),patch('sys.stdout',new_callable=io.StringIO) as stdout:
            REUSE.main()
        self.assertIn('execute normally',stdout.getvalue())
        self.assertIn('miss (pr_metadata_unavailable)', stdout.getvalue())
        self.assertNotIn('private-token',stdout.getvalue())
        self.assertFalse((Path(self.temp.name)/'output').exists())


if __name__=='__main__':unittest.main()

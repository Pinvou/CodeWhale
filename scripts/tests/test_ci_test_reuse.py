"""Exercise real Git trees and adversarial success evidence without network access."""
import copy
from datetime import datetime, timezone, timedelta
from functools import lru_cache
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
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
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def git(*args):
                return subprocess.check_output(['git', *args], cwd=root, encoding='utf-8', stderr=subprocess.DEVNULL).strip()
            git('init', '-q'); git('config', 'user.name', 'Fixture'); git('config', 'user.email', 'fixture@example.invalid')
            scripts = root/'scripts'; scripts.mkdir()
            (scripts/'select-frontend-smokes.mjs').write_bytes((ROOT/'scripts/select-frontend-smokes.mjs').read_bytes())
            # Only the final browser-command executor is replaced. Git, the
            # actual workflow Bash and selector execute unchanged.
            (scripts/'run-frontend-smokes.mjs').write_text('process.stdin.pipe(process.stdout);\n', encoding='utf-8')
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
            def execute(changes=None):
                return subprocess.run([shutil.which('bash'), '-c', command], cwd=root/'pinvou3-app',
                                      env={**environment, **(changes or {})}, capture_output=True, encoding='utf-8')
            result = execute()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, expected)
            for invalid in ({'TESTED_SHA': head}, {'PR_HEAD_SHA': 'f'*40}):
                with self.subTest(invalid=invalid):
                    rejected = execute(invalid)
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
        self.assertNotIn('private-token',stdout.getvalue())
        self.assertFalse((Path(self.temp.name)/'output').exists())


if __name__=='__main__':unittest.main()

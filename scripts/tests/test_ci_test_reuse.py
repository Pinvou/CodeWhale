"""Exercise real Git trees and adversarial success evidence without network access."""
import copy
from datetime import datetime, timezone, timedelta
from functools import lru_cache
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import urllib.request
from unittest.mock import patch
import zipfile

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('ci_test_reuse', ROOT/'scripts/ci-test-reuse.py')
REUSE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(REUSE)
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
        cls.cached_git = staticmethod(lru_cache(None)(REUSE.git))

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
                      'created': NOW.isoformat(), 'jobs': sorted(REUSE.JOBS)}
        self.run = {'id': 12, 'run_attempt': 1, 'event': 'pull_request', 'conclusion': 'success', 'head_sha': self.head,
                    'path': REUSE.WORKFLOW, 'head_repository': {'full_name': self.repository},
                    'pull_requests': [{'number': 7, 'head': {'sha': self.head}, 'base': {'sha': self.base, 'ref': 'main'}}],
                    'updated_at': NOW.isoformat()}
        self.jobs = [{'name': 'required-gate', 'conclusion': 'success', 'run_id': 12}]
        self.jobs += [{'name': name, 'conclusion': 'success', 'run_id': 12, 'labels': ['ubuntu-22.04'],
                       'steps': [{'name': step, 'conclusion': 'success'} for step in spec[1]]}
                      for name, spec in REUSE.JOBS.items()]
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
        return subprocess.check_output(['git', *args], cwd=self.root, text=True, encoding='utf-8', stderr=subprocess.DEVNULL, timeout=30).strip()

    def archive(self):
        output = io.BytesIO()
        with zipfile.ZipFile(output, 'w') as archive:
            archive.writestr('proof.json', json.dumps(self.proof))
        return output.getvalue()

    def lookup(self, configuration=None):
        return REUSE.lookup(self.event, self.config if configuration is None else configuration, self.api, now=NOW)

    def test_different_commit_ids_with_identical_real_trees_reuse_success(self):
        self.assertNotEqual(self.tested, self.queued)
        self.assertEqual(self.lookup(), (sorted(REUSE.JOBS), 12))

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
        for step in self.jobs[1]['steps']:
            step['conclusion']='skipped'
            self.assertNotIn(self.jobs[1]['name'],self.lookup()[0])
            step['conclusion']='success'
        self.jobs[0]['conclusion']='failure'
        self.assertEqual(self.lookup(),([],None))

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

    def test_api_failure_falls_back_without_printing_token_or_configuration(self):
        event_path=self.root/'event.json';event_path.write_text(json.dumps(self.event),encoding='utf-8')
        with patch.dict(os.environ,{'GITHUB_EVENT_PATH':str(event_path),'GITHUB_EVENT_NAME':'merge_group','GITHUB_TOKEN':'private-token','CI_REUSE_CONFIGURATION':json.dumps(self.config)}),patch('sys.argv',['ci-test-reuse.py','lookup']),patch.object(REUSE.GitHub,'request',side_effect=OSError('private-token')),patch('sys.stdout',new_callable=io.StringIO) as stdout:
            REUSE.main()
        self.assertIn('execute normally',stdout.getvalue())
        self.assertNotIn('private-token',stdout.getvalue())
        self.assertFalse((Path(self.temp.name)/'output').exists())


if __name__=='__main__':unittest.main()

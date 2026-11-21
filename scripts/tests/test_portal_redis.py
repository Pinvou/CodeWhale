"""Pure fixture/selector negative controls; not a Docker runtime acceptance."""
import copy
import contextlib
import io
import importlib.util
from pathlib import Path
import unittest
import json
import os
import tempfile
import subprocess
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]


def module(name, file):
    spec = importlib.util.spec_from_file_location(name, ROOT / 'scripts' / file)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


CI = module('portal_redis_ci', 'account-ci-redis.py')
SELECT = module('portal_redis_selection', 'test-portal-redis.py')


class PortalRedisGuardTests(unittest.TestCase):
    def fixture(self):
        meta = {'id': 'a' * 64, 'ports': {'6379': '49179'}}
        item = {'Id': meta['id'], 'Config': {'Image': CI.IMAGE},
                'State': {'Running': True, 'Health': {'Status': 'healthy'}},
                'NetworkSettings': {'Ports': {'6379/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '49179'}]}},
                'Mounts': [{'Type': 'volume', 'Name': 'b' * 64, 'Destination': '/data'}],
                'HostConfig': {'AutoRemove': True}}
        return meta, item

    def test_rejects_unowned_service_before_any_write(self):
        meta, original = self.fixture()
        self.assertEqual(CI.inspect_service(meta, original), ('49179', 'b' * 64))
        alterations = [lambda x: x.update(Id='c' * 64),
                       lambda x: x['Config'].update(Image='redis:latest'),
                       lambda x: x['State'].update(Running=False),
                       lambda x: x['State']['Health'].update(Status='unhealthy'),
                       lambda x: x['NetworkSettings']['Ports']['6379/tcp'][0].update(HostIp='0.0.0.0'),
                       lambda x: x['Mounts'][0].update(Type='bind'),
                       lambda x: x['Mounts'][0].update(Name='persistent-data'),
                       lambda x: x['HostConfig'].update(AutoRemove=False),
                       lambda x: x['HostConfig'].update(Binds=['/outside:/data'])]
        for alter in alterations:
            item = copy.deepcopy(original)
            alter(item)
            with self.assertRaises(ValueError):
                CI.inspect_service(meta, item)

    def test_forwarded_endpoint_must_match_direct_container_identity(self):
        info = 'run_id:' + 'a' * 40 + '\r\nredis_version:8.2.11\r\nredis_mode:standalone\r\ntcp_port:6379\r\n'
        self.assertEqual(CI.prove_server(info, info), 'a' * 40)
        for value in [info.replace('a' * 40, 'b' * 40), info.replace('8.2.11', '8.8.0'), info.replace('standalone', 'cluster')]:
            with self.assertRaises(ValueError):
                CI.prove_server(info, value)

    def test_foreign_forwarded_identity_cannot_trigger_writes(self):
        meta, item = self.fixture()
        info = 'run_id:' + 'a' * 40 + '\r\nredis_version:8.2.11\r\nredis_mode:standalone\r\ntcp_port:6379\r\n'
        calls = []
        def docker(args, **kwargs):
            if args[1:3] == ['ps', '--all']: return meta['id']
            if args[1] == 'inspect': return json.dumps([item])
            if args[1] == 'exec': return info
            self.fail('unexpected Docker mutation')
        def endpoint(port, *args):
            calls.append(args)
            return info.replace('a' * 40, 'b' * 40)
        with tempfile.TemporaryDirectory() as directory:
            env = {'GITHUB_ACTIONS':'true','ACCOUNT_CI_SERVICES':json.dumps({CI.SERVICE:meta}),'PINVOU_CI_RESOURCE_LEDGER':directory}
            with patch.dict(os.environ,env), patch('sys.argv',['account-ci-redis.py']), patch.object(CI,'run',docker), patch.object(CI,'command',endpoint):
                with self.assertRaises(ValueError): CI.main()
        self.assertEqual(calls,[('INFO','server')])

    def test_stop_without_ledger_still_retires_exact_job_service_and_volume(self):
        meta, item = self.fixture()
        calls, stopped = [], False
        def docker(args, **kwargs):
            nonlocal stopped
            calls.append(args)
            if args[1:3] == ['ps','--all']: return '' if stopped else meta['id']
            if args[1] == 'inspect': return json.dumps([item])
            if args[1] == 'stop':
                self.assertEqual(args,['docker','stop','--time','15',meta['id']])
                stopped = True
                return meta['id']
            if args[1:3] == ['volume','ls']: return ''
            self.fail('unexpected Redis cleanup command')
        env = {'GITHUB_ACTIONS':'true','ACCOUNT_CI_SERVICES':json.dumps({CI.SERVICE:meta})}
        with patch.dict(os.environ,env,clear=True), patch('sys.argv',['account-ci-redis.py','--stop']), patch.object(CI,'run',docker), patch.object(CI,'command',side_effect=AssertionError('stop must not connect to Redis')), contextlib.redirect_stdout(io.StringIO()) as output:
            CI.main()
        self.assertTrue(stopped)
        receipts = [json.loads(line) for line in output.getvalue().splitlines()]
        self.assertEqual([x['state'] for x in receipts],['registered','released'])
        self.assertTrue(all(x['id']==meta['id'] and x['volume']=='b'*64 for x in receipts))
        self.assertFalse(any('prune' in x for args in calls for x in args))

    def test_missing_ledger_cannot_start_fixture_writes_or_guess_gone_volume(self):
        meta, item = self.fixture()
        env = {'GITHUB_ACTIONS':'true','ACCOUNT_CI_SERVICES':json.dumps({CI.SERVICE:meta})}
        with patch.dict(os.environ,env,clear=True), patch('sys.argv',['account-ci-redis.py']), patch.object(CI,'run',side_effect=AssertionError('no admission without receipt')), patch.object(CI,'command',side_effect=AssertionError('no fixture writes')):
            with self.assertRaises(RuntimeError): CI.main()
        with patch.dict(os.environ,env,clear=True), patch('sys.argv',['account-ci-redis.py','--stop']), patch.object(CI,'run',return_value=''), patch.object(CI,'command',side_effect=AssertionError('no fixture writes')):
            with self.assertRaisesRegex(ValueError,'before volume registration'): CI.main()


    def test_diagnostic_io_faults_cannot_suppress_verified_live_retirement(self):
        for fault in ['malformed', 'read', 'write', 'missing_directory', 'output']:
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as directory:
                meta, item = self.fixture()
                ledger = Path(directory) / 'missing' if fault == 'missing_directory' else Path(directory)
                receipt = ledger / 'portal-redis-service.json'
                original = json.dumps({'schema':1,'kind':'deployment-containers','id':meta['id'],'volume':'b'*64,'state':'registered'})
                if fault != 'missing_directory':
                    receipt.write_text('{invalid' if fault == 'malformed' else original, encoding='utf-8')
                stopped, calls = False, []
                def docker(args, **kwargs):
                    nonlocal stopped
                    calls.append(args)
                    if args[1:3] == ['ps','--all']: return '' if stopped else meta['id']
                    if args[1] == 'inspect': return json.dumps([item])
                    if args[1] == 'stop': stopped = True; return meta['id']
                    if args[1:3] == ['volume','ls']: return ''
                    self.fail('unexpected cleanup command')
                class UnavailableOutput(io.StringIO):
                    def write(self, text): raise OSError('controlled output failure')
                output = UnavailableOutput() if fault == 'output' else io.StringIO()
                env = {'GITHUB_ACTIONS':'true','ACCOUNT_CI_SERVICES':json.dumps({CI.SERVICE:meta}),'PINVOU_CI_RESOURCE_LEDGER':str(ledger)}
                with contextlib.ExitStack() as stack:
                    stack.enter_context(patch.dict(os.environ,env,clear=True))
                    stack.enter_context(patch('sys.argv',['account-ci-redis.py','--stop']))
                    stack.enter_context(patch.object(CI,'run',docker))
                    stack.enter_context(patch.object(CI,'command',side_effect=AssertionError('cleanup cannot mutate Redis')))
                    stack.enter_context(contextlib.redirect_stdout(output))
                    if fault == 'read': stack.enter_context(patch.object(Path,'read_text',side_effect=PermissionError('controlled read failure')))
                    if fault == 'write': stack.enter_context(patch.object(CI,'save',side_effect=OSError('controlled ENOSPC')))
                    with self.assertRaises(RuntimeError): CI.main()
                self.assertTrue(stopped, 'diagnostic error prevented exact owned stop')
                self.assertEqual(sum(x[1]=='stop' for x in calls),1)
                self.assertFalse(any('prune' in x for args in calls for x in args))
                if fault != 'output':
                    last = json.loads(output.getvalue().splitlines()[-1])
                    self.assertEqual(last['state'],'released')
                    self.assertFalse(last['cleanup_failed'])
                    self.assertFalse(last['receipt_persisted'])
                    self.assertTrue(last['errors'])
                if fault in ('malformed','read','write'):
                    self.assertEqual(receipt.read_text(encoding='utf-8'),'{invalid' if fault=='malformed' else original)

    def test_receipt_write_failure_recovers_next_attempt_without_repeating_stop(self):
        meta, item = self.fixture()
        stopped, stop_count = False, 0
        def docker(args, **kwargs):
            nonlocal stopped, stop_count
            if args[1:3] == ['ps','--all']: return '' if stopped else meta['id']
            if args[1] == 'inspect': return json.dumps([item])
            if args[1] == 'stop': stopped=True; stop_count+=1; return meta['id']
            if args[1:3] == ['volume','ls']: return ''
            self.fail('unexpected cleanup command')
        with tempfile.TemporaryDirectory() as directory:
            receipt=Path(directory)/'portal-redis-service.json'
            receipt.write_text(json.dumps({'schema':1,'kind':'deployment-containers','id':meta['id'],'volume':'b'*64,'state':'registered'}),encoding='utf-8')
            env={'GITHUB_ACTIONS':'true','ACCOUNT_CI_SERVICES':json.dumps({CI.SERVICE:meta}),'PINVOU_CI_RESOURCE_LEDGER':directory}
            with patch.dict(os.environ,env,clear=True),patch('sys.argv',['account-ci-redis.py','--stop']),patch.object(CI,'run',docker),contextlib.redirect_stdout(io.StringIO()):
                with patch.object(CI,'save',side_effect=OSError('controlled ENOSPC')):
                    with self.assertRaises(RuntimeError): CI.main()
                self.assertTrue(stopped)
                CI.main()
            self.assertEqual(stop_count,1)
            self.assertEqual(json.loads(receipt.read_text(encoding='utf-8'))['state'],'released')

    def test_untrusted_receipt_fields_are_not_logged_or_used_for_foreign_cleanup(self):
        meta, item = self.fixture()
        for live in [False,True]:
            stopped=False
            def docker(args, **kwargs):
                nonlocal stopped
                if args[1:3] == ['ps','--all']: return meta['id'] if live and not stopped else ''
                if args[1]=='inspect': return json.dumps([item])
                if args[1]=='stop': stopped=True;return meta['id']
                if args[1:3]==['volume','ls']:return ''
                self.fail('unexpected cleanup command')
            with tempfile.TemporaryDirectory() as directory:
                receipt=Path(directory)/'portal-redis-service.json'
                receipt.write_text(json.dumps({'schema':1,'kind':'deployment-containers','id':meta['id'],'volume':'b'*64,'state':'registered','outside':'controlled-private-value','run_id':'a'*40,'owner_id':'portal-ci-'+'c'*32}),encoding='utf-8')
                env={'GITHUB_ACTIONS':'true','ACCOUNT_CI_SERVICES':json.dumps({CI.SERVICE:meta}),'PINVOU_CI_RESOURCE_LEDGER':directory}
                with patch.dict(os.environ,env,clear=True),patch('sys.argv',['account-ci-redis.py','--stop']),patch.object(CI,'run',docker),contextlib.redirect_stdout(io.StringIO()) as output:
                    CI.main()
                self.assertNotIn('controlled-private',output.getvalue())
                state=json.loads(receipt.read_text(encoding='utf-8'))
                self.assertEqual(state['run_id'],'a'*40)
                self.assertEqual(state['owner_id'],'portal-ci-'+'c'*32)
                self.assertNotIn('outside',state)


    def test_retirement_waits_for_delayed_autoremove_with_bounded_query_timeouts(self):
        now, volume_reads, waits = [0.0], [0], []
        def docker(args, **kwargs):
            self.assertGreater(kwargs['timeout'],0)
            self.assertLessEqual(kwargs['timeout'],5)
            if args[1:3]==['ps','--all']: return ''
            if args[1:3]==['volume','ls']:
                volume_reads[0]+=1
                return 'b'*64 if volume_reads[0]==1 else ''
            self.fail('observer must not mutate Docker')
        def pause(seconds): waits.append(seconds);now[0]+=seconds
        with patch.object(CI,'run',docker),patch.object(CI.time,'monotonic',lambda:now[0]),patch.object(CI.time,'sleep',pause):
            self.assertTrue(CI.observe_retirement('a'*64,'b'*64,budget=1))
        self.assertEqual(volume_reads[0],2)
        self.assertEqual(waits,[0.25])

    def test_retirement_deadline_never_turns_persistent_resources_into_success(self):
        now, calls = [0.0], []
        def docker(args, **kwargs):
            calls.append((args,kwargs['timeout']))
            elapsed=min(0.2,kwargs['timeout'])
            now[0]+=elapsed  # The subprocess honors its actual remaining timeout.
            if elapsed<0.2: raise subprocess.TimeoutExpired(args,kwargs['timeout'])
            return 'a'*64 if args[1]=='ps' else 'b'*64
        def pause(seconds): now[0]+=seconds
        with patch.object(CI,'run',docker),patch.object(CI.time,'monotonic',lambda:now[0]),patch.object(CI.time,'sleep',pause):
            with self.assertRaises(subprocess.TimeoutExpired):
                CI.observe_retirement('a'*64,'b'*64,budget=1)
        self.assertLessEqual(now[0],1.0)
        self.assertTrue(all(0<timeout<=1 for _,timeout in calls))
        self.assertTrue(all(args[1] in ('ps','volume') for args,_ in calls))

    def events(self):
        names = SELECT.ROOTS | {'TestRealRedisPortalSharedAdmissionAndCapacity/' + x for x in SELECT.CAPACITY_CASES}
        return [item for name in sorted(names) for item in [{'Action': 'run', 'Test': name}, {'Action': 'pass', 'Test': name}]] + [{'Action': 'pass', 'Package': 'portal'}]

    def test_selector_requires_every_actual_root_and_child(self):
        self.assertEqual(SELECT.verify(self.events())['roots'], 15)
        for name in ['TestRealRedisFlowConsumptionAndSessionTTL', 'TestRealRedisPortalSharedAdmissionAndCapacity/ConcurrentCreatorsCannotOversubscribe']:
            with self.assertRaises(ValueError):
                SELECT.verify([x for x in self.events() if x.get('Test') != name])
        for event in [{'Action': 'skip', 'Test': 'hidden'}, {'Action': 'fail', 'Package': 'portal'}]:
            with self.assertRaises(ValueError):
                SELECT.verify(self.events() + [event])
        with self.assertRaises(ValueError):
            SELECT.verify([{'Action': 'pass', 'Package': 'portal'}])


if __name__ == '__main__':
    unittest.main()

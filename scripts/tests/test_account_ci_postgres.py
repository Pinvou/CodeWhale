"""Pure admission regressions; synthetic metadata is not a Docker acceptance."""
import copy
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import runpy
import subprocess
import sys
import tempfile
import textwrap
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
def module(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    value = importlib.util.module_from_spec(spec); spec.loader.exec_module(value)
    return value
PG = module('account_ci_postgres', 'scripts/account-ci-postgres.py')
FIN = module('financial_identity', 'scripts/test-token-financial-postgres.py')

class AccountCIAdmissionTests(unittest.TestCase):
    def test_closure_cli_admits_and_retires_only_its_single_service(self):
        self.assertEqual(PG.CLOSURE_SERVICES, {'financial': PG.SERVICES['financial']})
        meta, item = self.fixture()
        identity = '12345|/var/lib/postgresql/data|5432|postgres|postgres|postgres|t|160015'
        for stop in (False, True):
            calls, stopped = [], []
            def run(argv, **kwargs):
                calls.append(argv)
                if argv[:2] == ['docker', 'inspect']:
                    self.assertEqual(argv[2], meta['id']); value = json.dumps([item])
                elif argv[:2] == ['docker', 'exec']:
                    self.assertEqual(argv[2], meta['id']); value = identity
                elif argv[0] == 'psql':
                    self.assertIn('127.0.0.1:49173/', argv[1]); value = identity
                elif argv[:2] == ['docker', 'ps']: value = '' if stopped else meta['id']
                elif argv[:3] == ['docker', 'volume', 'ls']: value = '' if stopped else 'b'*64
                elif argv[:2] == ['docker', 'logs']:
                    self.assertEqual(argv[2], meta['id']); value = 'owned service log'
                elif argv[:2] == ['docker', 'stop']:
                    self.assertEqual(argv[2:], ['--time', '15', meta['id']]); stopped.append(meta['id']); value = ''
                else: raise AssertionError('unexpected closure subprocess')
                return subprocess.CompletedProcess(argv, 0, stdout=value, stderr='')
            with self.subTest(stop=stop), tempfile.TemporaryDirectory() as directory:
                output = Path(directory)/'output'
                env = {'GITHUB_ACTIONS': 'true', 'ACCOUNT_CI_SERVICES': json.dumps({'financial-postgres': meta}),
                       'GITHUB_OUTPUT': str(output), 'PINVOU_CI_RESOURCE_LEDGER': directory}
                argv = ['account-ci-postgres.py', '--profile', 'closure'] + (['--stop-owned-services'] if stop else [])
                with patch.dict(os.environ, env), patch.object(sys, 'argv', argv), \
                        patch.object(subprocess, 'run', side_effect=run), contextlib.redirect_stdout(io.StringIO()):
                    runpy.run_path(str(ROOT/'scripts/account-ci-postgres.py'), run_name='__main__')
                if stop:
                    self.assertEqual(stopped, [meta['id']])
                    receipt = json.loads((Path(directory)/'postgres-services.json').read_text())
                    self.assertEqual(receipt['state'], 'released')
                    self.assertEqual(receipt['ids'], [meta['id']])
                else:
                    self.assertEqual(stopped, [])
                    values = output.read_text()
                    self.assertIn('financial_system=12345', values)
                    self.assertNotIn('audit_url=', values)
                    self.assertNotIn('financial_token_url=', values)
                    self.assertEqual(sum(argv[:2] == ['docker', 'inspect'] for argv in calls), 1)

    def test_closure_job_has_independent_admission_and_required_result(self):
        source=(ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        closure=source.split('\n  account-closure-test:',1)[1].split('\n  windows-relay-dacl:',1)[0]
        account=source.split('\n  account-platform-test:',1)[1].split('\n  account-closure-test:',1)[0]
        self.assertIn('\n    needs: changes\n', closure)
        self.assertIn("needs.changes.outputs.account_platform == 'true'", closure)
        self.assertEqual(closure.count('"127.0.0.1::5432"'), 1)
        self.assertIn('scripts/account-ci-postgres.py --profile closure\n', closure)
        self.assertIn('scripts/account-ci-postgres.py --profile closure --stop-owned-services', closure)
        self.assertIn('PINVOU_ACCOUNT_CLOSURE_TEST_DATABASE_URL: ${{ steps.account-pg.outputs.financial_url }}', closure)
        self.assertIn('Confirm Account test resource retirement', closure)
        self.assertNotIn('python3 scripts/test-account-closure-postgres.py', account)
        gate=source.split('\n  required-gate:',1)[1]
        self.assertIn('      - account-closure-test\n', gate)
        self.assertIn('ACCOUNT_CLOSURE_RESULT: ${{ needs.account-closure-test.result }}', gate)
        self.assertIn('"$ACCOUNT_CLOSURE_RESULT" != success', gate)
        self.assertIn("fromJSON(toJSON(needs.changes.outputs)).reuse_verified == 'true'", closure)
        self.assertIn("'account-closure-test'))", closure)
        self.assertIn('"$ACCOUNT_CLOSURE_RESULT" == skipped && "${CLOSURE_REUSE_VERIFIED:-false}" == true', gate)

    def fixture(self):
        meta = {'id': 'a'*64, 'ports': {'5432':'49173'}}
        item = {'Id': meta['id'], 'State': {'Running':True, 'Health': {'Status':'healthy'}},
                'Config': {'Image':'mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea'},
                'NetworkSettings': {'Ports': {'5432/tcp':[{'HostIp':'127.0.0.1','HostPort':'49173'}]}},
                'Mounts':[{'Type':'volume','Name':'b'*64,'Destination':'/var/lib/postgresql/data'}],
                'HostConfig':{'AutoRemove':True}}
        return meta,item

    def test_canonical_ports(self):
        for invalid in ('','0','1023','65536','049173','49173\n','$(true)',49173):
            with self.subTest(value=invalid), self.assertRaises(ValueError): PG.port_value(invalid)
        self.assertEqual(PG.port_value('49173'),'49173')

    def test_exact_docker_admission(self):
        meta,item=self.fixture()
        self.assertEqual(PG.inspect_service(meta,item,'mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea'),('49173','b'*64))
        mutations = [lambda x:x.update(Id='c'*64),lambda x:x['State'].update(Running=False),
                     lambda x:x['State']['Health'].update(Status='starting'),lambda x:x['Config'].update(Image='other'),
                     lambda x:x['NetworkSettings']['Ports']['5432/tcp'][0].update(HostIp='0.0.0.0'),
                     lambda x:x['NetworkSettings']['Ports']['5432/tcp'][0].update(HostPort='49174'),
                     lambda x:x['NetworkSettings']['Ports']['5432/tcp'].append({'HostIp':'::','HostPort':'49173'}),
                     lambda x:x['Mounts'][0].update(Type='bind'),lambda x:x['Mounts'][0].update(Name='reuse'),
                     lambda x:x['HostConfig'].update(Binds=['x:y']),lambda x:x['HostConfig'].update(AutoRemove=False)]
        for mutation in mutations:
            changed=copy.deepcopy(item);mutation(changed)
            with self.assertRaises(ValueError):PG.inspect_service(meta,changed,'mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea')

    def test_sql_identity(self):
        good='12345|/var/lib/postgresql/data|5432|postgres|postgres|postgres|t|160015'
        self.assertEqual(PG.identity_matches(good,'/var/lib/postgresql/data','16'),'12345')
        for old,new in [('5432','5433'),('data','other'),('|t|','|f|'),('160015','180004')]:
            with self.assertRaises(ValueError):PG.identity_matches(good.replace(old,new),'/var/lib/postgresql/data','16')

    def test_stop_only_exact_owned_services_after_complete_validation(self):
        import json
        metadata, items = {}, {}
        for index, (_, (name, image, _, major)) in enumerate(PG.SERVICES.items()):
            meta, item = self.fixture()
            meta['id'] = str(index+1)*64
            meta['ports']['5432'] = str(49173+index)
            item['Id'] = meta['id']; item['Config']['Image'] = image
            item['NetworkSettings']['Ports']['5432/tcp'][0]['HostPort'] = meta['ports']['5432']
            item['Mounts'][0].update(Name=format(index+4,'x')*64, Destination='/var/lib/postgresql' if major=='18' else '/var/lib/postgresql/data')
            metadata[name] = meta; items[meta['id']] = item
        for problem in ('none', 'unhealthy', 'gone', 'all-gone', 'image', 'bind', 'reused-volume', 'reused-id', 'logs-failed', 'stop-failed', 'remaining-container', 'remaining-volume', 'partial-registration'):
            current, actual = copy.deepcopy(metadata), copy.deepcopy(items)
            stops = []
            if problem == 'unhealthy': actual['2'*64]['State']['Health']['Status'] = 'unhealthy'
            if problem == 'gone': actual.pop('2'*64)
            if problem == 'all-gone': actual.clear()
            if problem == 'image': actual['2'*64]['Config']['Image'] = 'unrelated'
            if problem == 'bind': actual['2'*64]['HostConfig']['Binds'] = ['/business:/data']
            if problem == 'reused-volume': actual['2'*64]['Mounts'][0]['Name'] = '4'*64
            if problem == 'reused-id': current['existing-broker-postgres']['id'] = '1'*64
            if problem == 'partial-registration': current.pop('existing-broker-postgres')
            def run(argv, **kwargs):
                if argv[:2] == ['docker', 'ps']:
                    remaining = set(actual) - set(stops)
                    if problem == 'remaining-container': remaining.add('1'*64)
                    return '\n'.join([*remaining, 'c'*64])  # unrelated container must never be stopped
                if argv[:3] == ['docker', 'volume', 'ls']:
                    return '4'*64 if problem == 'remaining-volume' else ''
                if argv[:2] == ['docker', 'inspect']:
                    return json.dumps([actual[argv[2]]])
                if argv[:2] == ['docker', 'logs']:
                    self.assertIn(argv[2], actual)
                    self.assertTrue(kwargs['include_stderr'])
                    if problem == 'logs-failed': raise RuntimeError('controlled log failure')
                    return 'controlled PostgreSQL stdout and stderr'
                if argv[:2] == ['docker', 'stop']:
                    stops.extend(argv[4:]); self.assertEqual(argv[2:4], ['--time', '15'])
                    self.assertEqual(kwargs['timeout'], 105)
                    if problem == 'stop-failed': raise RuntimeError('controlled stop failure')
                    return ''
                raise AssertionError('unexpected Docker operation')
            elapsed = [0.0]
            def advance(delay): elapsed[0] += delay
            with patch.dict(os.environ, {'GITHUB_ACTIONS':'true'}), patch.object(PG, 'run', side_effect=run), patch('time.monotonic', side_effect=lambda: elapsed[0]), patch('time.sleep', side_effect=advance), contextlib.redirect_stdout(io.StringIO()) as output:
                if problem in ('image', 'bind', 'reused-volume', 'reused-id'):
                    with self.assertRaises(ValueError): PG.stop_owned_services(current)
                    self.assertEqual(stops, [])
                elif problem in ('stop-failed', 'remaining-container', 'remaining-volume', 'partial-registration'):
                    with self.assertRaises(RuntimeError): PG.stop_owned_services(current)
                    if problem == 'partial-registration': self.assertNotIn('2'*64, stops)
                else:
                    PG.stop_owned_services(current)
                    self.assertEqual(set(stops), set(actual))
                self.assertNotIn('c'*64, stops)
                if problem == 'logs-failed': self.assertIn('::warning::', output.getvalue())
        with patch.dict(os.environ, {'GITHUB_ACTIONS':''}), patch.object(PG, 'run') as run:
            with self.assertRaises(RuntimeError): PG.stop_owned_services(metadata)
            run.assert_not_called()
        with patch.dict(os.environ, {'GITHUB_ACTIONS':'true'}), patch.object(PG, 'run') as run:
            with self.assertRaisesRegex(ValueError, 'registration unavailable'): PG.stop_owned_services(None)
            run.assert_not_called()

    def test_three_services_and_namespace_match(self):
        import json
        import tempfile
        metadata={};items={};identities={}
        for index, (_, (name,image,directory,major)) in enumerate(PG.SERVICES.items()):
            meta,item=self.fixture();meta['id']=str(index+1)*64;meta['ports']['5432']=str(49173+index)
            item['Id']=meta['id'];item['Config']['Image']=image
            item['NetworkSettings']['Ports']['5432/tcp'][0]['HostPort']=meta['ports']['5432']
            item['Mounts'][0].update(Name=format(index+4,'x')*64,Destination='/var/lib/postgresql' if major=='18' else '/var/lib/postgresql/data')
            metadata[name]=meta;items[meta['id']]=item
            identities[meta['id']]=f"{100+index}|{directory}|5432|postgres|postgres|postgres|t|{int(major)*10000}"
        for failure in ('none','namespace','volume','port','id'):
            current=copy.deepcopy(metadata);actual=copy.deepcopy(items)
            if failure=='volume':actual['2'*64]['Mounts'][0]['Name']='4'*64
            if failure=='port':current['existing-broker-postgres']['ports']['5432']='49173'
            if failure=='id':current['existing-broker-postgres']['id']='1'*64
            def run(argv):
                if argv[:2]==['docker','inspect']:return json.dumps([actual[argv[2]]])
                if argv[:2]==['docker','exec']:return identities[argv[2]]
                for name,meta in current.items():
                    if f":{meta['ports']['5432']}/" in argv[1]:
                        value=identities[meta['id']]
                        return value.replace(value.split('|')[0],'999') if failure=='namespace' else value
                raise AssertionError('unexpected endpoint')
            with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ,{'ACCOUNT_CI_SERVICES':json.dumps(current),'GITHUB_OUTPUT':str(Path(directory)/'output')}), patch.object(PG,'run',side_effect=run):
                if failure=='none':
                    PG.main();self.assertIn('financial_system=102',(Path(directory)/'output').read_text())
                    output=(Path(directory)/'output').read_text()
                    from_line=next(line for line in output.splitlines() if line.startswith('financial_services='))
                    self.assertEqual(set(json.loads(from_line.split('=',1)[1])), set(PG.FINANCIAL_GROUPS))
                else:
                    with self.assertRaises(ValueError):PG.main()
                    self.assertFalse((Path(directory)/'output').exists())

    def test_each_psql_operation_fences_same_backend(self):
        env={'GITHUB_ACTIONS':'true','PINVOU_ACCOUNT_CI_PG_PORT':'49173','PINVOU_ACCOUNT_CI_PG_SYSTEM_ID':'12345','PINVOU_ACCOUNT_CI_PG_DIRECTORY':'/var/lib/postgresql/data'}
        raw='postgres://postgres@127.0.0.1:49173/postgres?sslmode=disable'
        with patch.dict(os.environ,env,clear=True):
            guard=FIN.ci_identity_guard(raw)
            self.assertEqual(guard[0],'-c');self.assertIn('pg_control_system()',guard[1])
            self.assertIn("system_identifier::text FROM pg_control_system())='12345'",guard[1])
            for invalid in (raw.replace('49173','49174'), raw+'&host=other',raw.replace('postgres@','other@')):
                with self.assertRaises(RuntimeError):FIN.ci_identity_guard(invalid)
            for name in env:
                with patch.dict(os.environ,{name:''}):
                    with self.assertRaises(RuntimeError):FIN.ci_identity_guard(raw)

    def test_workflow_consumers_and_anonymous_lifecycle(self):
        source=(ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        job=source.split('\n  account-platform-test:',1)[1].split('\n  account-closure-test:',1)[0]
        routes=[line.strip() for line in job.splitlines() if line.lstrip().startswith('runs-on:')]
        self.assertEqual(routes, ["""runs-on: ${{ fromJSON(vars.PINVOU_LINUX_HEAVY_RUNNER || vars.PINVOU_LINUX_RUNNER || '["ubuntu-22.04"]') }}"""])
        self.assertEqual(job.count('"127.0.0.1::5432"'),2);self.assertEqual(job.count('          --rm\n'),2)
        for variable,key in [('PINVOU_TEST_SCHEMA_DATABASE_URL','broker'),('F17_TEST_DATABASE_URL','audit')]:
            self.assertIn(variable+': ${{ steps.account-pg.outputs.'+key+'_url }}',job)
        self.assertNotIn('ACCOUNT_FINANCIAL_SERVICES:',job)
        self.assertIn('scripts/account-ci-postgres.py --profile core',job)
        self.assertEqual(job.count('          --memory 1g\n'),0)
        self.assertEqual(job.count('          --memory-swap 1g\n'),0)
        self.assertEqual(job.count('          --cpus 2\n'),0)
        self.assertIn('ACCOUNT_CI_SERVICES: ${{ toJSON(job.services) }}',job)
        self.assertIn('F17_TEST_SERVICE_PORT: ${{ steps.account-pg.outputs.audit_port }}',job)
        self.assertIn('lua5.1 postgresql-client',job)

    def test_audit_workflow_requires_invitation_refusal_and_forward_recovery(self):
        source=(ROOT/'.github/workflows/pr-check.yml').read_text(encoding='utf-8')
        marker='          python3 - "$RUNNER_TEMP/f17-audit-schema.jsonl" "$RUNNER_TEMP/f17-audit-account.jsonl" <<\'PY\'\n'
        before, code=source.split(marker,1)
        code=textwrap.dedent(code.split('\n          PY',1)[0])
        pattern=re.findall(r"./internal/accountschema -run '([^']+)'",before)[-1]
        roots={"TestAuditExportRollbackRoleStorageGuard", "TestAuditExportPublicDownRetainsOriginalHistory",
               "TestAuditExportGuardedRollbackTransactions", "TestAccount039RollbackInventoryPostgres",
               "TestMembershipInvitationLegacyRollbackPublicPostgres"}
        for root in roots:self.assertIsNotNone(re.fullmatch(pattern,root))
        self.assertIsNone(re.fullmatch(pattern,"TestUnrelatedOptionalFixture"))
        invitation="TestMembershipInvitationLegacyRollbackPublicPostgres"
        children={
            "TestAccount039RollbackInventoryPostgres": {"empty-ledger", "before-approval-schema", "installed-039", "higher-version", "retained-v2-reference", "wrong-database", "missing-approval-column-grant"},
            invitation: {"retained-link-refuses-down-without-fact-or-schema-loss", "forward-upgrade-replays-original-and-accepts-next-command"},
        }
        events=[]
        for root in roots:
            events.append({"Test":root,"Action":"pass"})
            events.extend({"Test":root+"/"+child,"Action":"pass"} for child in children.get(root,{"actual"}))
        account=[{"Test":"TestAuditExportPersistedBoundaries","Action":"pass"},
                 {"Test":"TestAuditExportPersistedBoundaries/actual","Action":"pass"}]
        lease_cases={"TestAuditExportPersistedBoundaries/generation_renews_and_recovers/"+case
                     for case in ("renew_live_attempt", "cancel_expired_attempt_and_recover", "ordinary_error_stops_inflight_renewal")}
        account.extend({"Test":case,"Action":"pass"} for case in lease_cases)
        retry_cases={"TestAuditExportPersistedBoundaries/bounded_retry_policy/"+case for case in (
            "transient_database_errors_stop_at_three", "expired_third_attempt_is_terminal",
            "permanent_generation_error_is_not_retried", "pending_without_worker_retains_quota")}
        account.extend({"Test":case,"Action":"pass"} for case in retry_cases)
        commerce = "TestCommerceSyncLifecycleFence"
        commerce_cases = {commerce + "/" + case for case in (
            "current_generation_completes", "stale_pending_not_claimed",
            "late_completion_rejects_old_lifecycle_without_effects",
            "late_retry_cannot_write_new_lifecycle_mapping",
            "mapping_version_drift_records_stale_in_same_lifecycle",
            "temporary_disable_preserves_same_generation_retry",
        )}
        account.append({"Test": commerce, "Action": "pass"})
        account.extend({"Test":case,"Action":"pass"} for case in commerce_cases)
        with tempfile.TemporaryDirectory(prefix='audit-result-contract-') as directory:
            schema_path, account_path=Path(directory)/'schema.jsonl',Path(directory)/'account.jsonl'
            account_path.write_text('\n'.join(map(json.dumps,account)),encoding='utf-8')
            def verify(items):
                schema_path.write_text('\n'.join(map(json.dumps,items)),encoding='utf-8')
                with patch.object(sys,'argv',['audit-validator',str(schema_path),str(account_path)]):
                    exec(compile(code,'<actual-audit-workflow-validator>','exec'),{})
            verify(events)
            for child in children[invitation]:
                for invalid in ([e for e in events if e['Test']!=invitation+'/'+child],
                                [dict(e,Action='skip') if e['Test']==invitation+'/'+child else e for e in events]):
                    with self.subTest(child=child),self.assertRaises(AssertionError):verify(invalid)
            with self.assertRaises(AssertionError):verify([e for e in events if not e['Test'].startswith(invitation)])
            for case in lease_cases | retry_cases | commerce_cases:
                for invalid in ([e for e in account if e['Test']!=case],
                                [dict(e,Action='skip') if e['Test']==case else e for e in account]):
                    account_path.write_text('\n'.join(map(json.dumps,invalid)),encoding='utf-8')
                    with self.subTest(lease_case=case),self.assertRaises(AssertionError):verify(events)

# Controlled Docker acknowledgements do not replace real CI services.
class PostgresRetirementTests(unittest.TestCase):
    def exercise(self, scenario):
        metadata, items = {}, {}
        for index, (name, image, _, _) in enumerate(PG.SERVICES.values()):
            identifier, volume, port = f'{index + 1:064x}', f'{index + 101:064x}', str(49000 + index)
            metadata[name] = {'id': identifier, 'ports': {'5432': port}}
            items[identifier] = {'Id': identifier, 'State': {'Running': True},
                'Config': {'Image': image}, 'HostConfig': {'AutoRemove': True},
                'NetworkSettings': {'Ports': {'5432/tcp': [{'HostIp': '127.0.0.1', 'HostPort': port}]}},
                'Mounts': [{'Type': 'volume', 'Name': volume, 'Destination': '/var/lib/postgresql/data'}]}
        owned_ids = set(items)
        owned_volumes = {item['Mounts'][0]['Name'] for item in items.values()}
        if scenario == 'partial-registration':
            metadata.pop(next(iter(metadata)))
        elapsed, stopped, observations = [0.0], [], [0]
        calls = []

        def run(argv, **kwargs):
            calls.append((copy.deepcopy(argv), kwargs))
            self.assertGreater(kwargs.get('timeout', 30), 0)
            self.assertLessEqual(kwargs.get('timeout', 30), 105)
            if argv[:2] == ['docker', 'ps']:
                if not stopped:
                    return '\n'.join([*owned_ids, 'f' * 64])
                observations[0] += 1
                if scenario == 'query-error':
                    raise RuntimeError('controlled query failure')
                delayed = scenario == 'delayed-container' and observations[0] < 3
                persistent = scenario == 'persistent-container'
                return '\n'.join([next(iter(owned_ids)), 'f' * 64] if delayed or persistent else ['f' * 64])
            if argv[:3] == ['docker', 'volume', 'ls']:
                delayed = scenario == 'delayed-volume' and observations[0] < 3
                return '\n'.join([next(iter(owned_volumes)), 'e' * 64] if delayed or scenario == 'persistent-volume' else ['e' * 64])
            if argv[:2] == ['docker', 'inspect']:
                return json.dumps([items[argv[2]]])
            if argv[:2] == ['docker', 'logs']:
                return 'controlled logs'
            if argv[:2] == ['docker', 'stop']:
                self.assertEqual(argv[2:4], ['--time', '15'])
                self.assertFalse(stopped, 'the stop operation must never be retried')
                stopped.extend(argv[4:])
                if scenario == 'stop-failed':
                    raise RuntimeError('controlled stop failure')
                if scenario == 'budget-exhausted':
                    elapsed[0] = 105.0
                return ''
            self.fail(f'unexpected operation {argv[:2]}')

        def sleep(delay):
            self.assertGreater(delay, 0)
            self.assertLessEqual(delay, .25)
            elapsed[0] += delay

        with tempfile.TemporaryDirectory(prefix='pg-retirement-contract-') as directory:
            receipt = Path(directory) / 'receipt.json'
            with patch.dict(os.environ, {'GITHUB_ACTIONS': 'true'}), patch.object(PG, 'run', side_effect=run), \
                    patch('time.monotonic', side_effect=lambda: elapsed[0]), patch('time.sleep', side_effect=sleep), \
                    contextlib.redirect_stdout(io.StringIO()):
                if scenario in ('delayed-container', 'delayed-volume', 'immediate'):
                    PG.stop_owned_services(metadata, receipt)
                    self.assertEqual(json.loads(receipt.read_text())['state'], 'released')
                else:
                    with self.assertRaises(RuntimeError):
                        PG.stop_owned_services(metadata, receipt)
                    state = json.loads(receipt.read_text())['state']
                    self.assertEqual(state, 'released' if scenario == 'stop-failed' else 'unconfirmed')
            self.assertEqual(set(stopped), {v['id'] for v in metadata.values()})
            self.assertNotIn('f' * 64, stopped)
            self.assertLessEqual(elapsed[0], 105)
            if scenario.startswith('delayed-'):
                self.assertEqual(observations[0], 3)
            return json.loads(receipt.read_text()), calls

    def test_waits_for_exact_container_and_volume_acknowledgements(self):
        for scenario in ('immediate', 'delayed-container', 'delayed-volume'):
            with self.subTest(scenario=scenario):
                self.exercise(scenario)

    def test_unknown_or_failed_retirement_never_becomes_success(self):
        for scenario in ('persistent-container', 'persistent-volume', 'partial-registration',
                         'stop-failed', 'query-error', 'budget-exhausted'):
            with self.subTest(scenario=scenario):
                self.exercise(scenario)


if __name__=='__main__':unittest.main()

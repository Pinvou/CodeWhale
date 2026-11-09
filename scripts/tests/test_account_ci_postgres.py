"""Pure admission regressions; synthetic metadata is not a Docker acceptance."""
import copy
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
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
            with patch.dict(os.environ, {'GITHUB_ACTIONS':'true'}), patch.object(PG, 'run', side_effect=run), contextlib.redirect_stdout(io.StringIO()) as output:
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
        self.assertEqual(job.count('"127.0.0.1::5432"'),8);self.assertEqual(job.count('          --rm\n'),8)
        for variable,key in [('PINVOU_TEST_SCHEMA_DATABASE_URL','broker'),('F17_TEST_DATABASE_URL','audit')]:
            self.assertIn(variable+': ${{ steps.account-pg.outputs.'+key+'_url }}',job)
        self.assertIn('ACCOUNT_FINANCIAL_SERVICES: ${{ steps.account-pg.outputs.financial_services }}',job)
        self.assertEqual(job.count('          --memory 1g\n'),6)
        self.assertEqual(job.count('          --memory-swap 1g\n'),6)
        self.assertEqual(job.count('          --cpus 2\n'),6)
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

if __name__=='__main__':unittest.main()

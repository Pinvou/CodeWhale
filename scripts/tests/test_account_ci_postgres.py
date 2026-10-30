"""Pure admission regressions; synthetic metadata is not a Docker acceptance."""
import copy
import importlib.util
import os
from pathlib import Path
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
    def fixture(self):
        meta = {'id': 'a'*64, 'ports': {'5432':'49173'}}
        item = {'Id': meta['id'], 'State': {'Running':True, 'Health': {'Status':'healthy'}},
                'Config': {'Image':'postgres:16-alpine'},
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
        self.assertEqual(PG.inspect_service(meta,item,'postgres:16-alpine'),('49173','b'*64))
        mutations = [lambda x:x.update(Id='c'*64),lambda x:x['State'].update(Running=False),
                     lambda x:x['State']['Health'].update(Status='starting'),lambda x:x['Config'].update(Image='other'),
                     lambda x:x['NetworkSettings']['Ports']['5432/tcp'][0].update(HostIp='0.0.0.0'),
                     lambda x:x['NetworkSettings']['Ports']['5432/tcp'][0].update(HostPort='49174'),
                     lambda x:x['NetworkSettings']['Ports']['5432/tcp'].append({'HostIp':'::','HostPort':'49173'}),
                     lambda x:x['Mounts'][0].update(Type='bind'),lambda x:x['Mounts'][0].update(Name='reuse'),
                     lambda x:x['HostConfig'].update(Binds=['x:y']),lambda x:x['HostConfig'].update(AutoRemove=False)]
        for mutation in mutations:
            changed=copy.deepcopy(item);mutation(changed)
            with self.assertRaises(ValueError):PG.inspect_service(meta,changed,'postgres:16-alpine')

    def test_sql_identity(self):
        good='12345|/var/lib/postgresql/data|5432|postgres|postgres|postgres|t|160015'
        self.assertEqual(PG.identity_matches(good,'/var/lib/postgresql/data','16'),'12345')
        for old,new in [('5432','5433'),('data','other'),('|t|','|f|'),('160015','180004')]:
            with self.assertRaises(ValueError):PG.identity_matches(good.replace(old,new),'/var/lib/postgresql/data','16')

    def test_three_services_and_namespace_match(self):
        import json
        import tempfile
        metadata={};items={};identities={}
        for index, (_, (name,image,directory,major)) in enumerate(PG.SERVICES.items()):
            meta,item=self.fixture();meta['id']=str(index+1)*64;meta['ports']['5432']=str(49173+index)
            item['Id']=meta['id'];item['Config']['Image']=image
            item['NetworkSettings']['Ports']['5432/tcp'][0]['HostPort']=meta['ports']['5432']
            item['Mounts'][0].update(Name=str(index+4)*64,Destination='/var/lib/postgresql' if major=='18' else '/var/lib/postgresql/data')
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
        job=source.split('\n  account-platform-test:',1)[1].split('\n  windows-relay-dacl:',1)[0]
        self.assertIn('PINVOU_LINUX_MEDIUM_RUNNER',job)
        self.assertEqual(job.count('"127.0.0.1::5432"'),3);self.assertEqual(job.count('          --rm\n'),3)
        for variable,key in [('PINVOU_TEST_SCHEMA_DATABASE_URL','broker'),('F17_TEST_DATABASE_URL','audit'),('PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL','financial'),('PINVOU_ACCOUNT_CLOSURE_TEST_DATABASE_URL','financial')]:
            self.assertIn(variable+': ${{ steps.account-pg.outputs.'+key+'_url }}',job)
        self.assertIn('ACCOUNT_CI_SERVICES: ${{ toJSON(job.services) }}',job)
        self.assertIn('F17_TEST_SERVICE_PORT: ${{ steps.account-pg.outputs.audit_port }}',job)
        self.assertIn('lua5.1 postgresql-client',job)

if __name__=='__main__':unittest.main()

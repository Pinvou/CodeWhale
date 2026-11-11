"""Pure fixture/selector negative controls; not a Docker runtime acceptance."""
import copy
import importlib.util
from pathlib import Path
import unittest
import json
import os
import tempfile
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
        def docker(args):
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

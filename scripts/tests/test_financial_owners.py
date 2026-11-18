"""Owners fan-in keeps every original phase and independent ownership boundary."""
import contextlib
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('financial_owners', ROOT / 'scripts/test-financial-owners.py')
OWNER = importlib.util.module_from_spec(spec)
spec.loader.exec_module(OWNER)
pg_spec = importlib.util.spec_from_file_location('owners_pg', ROOT / 'scripts/account-ci-postgres.py')
PG = importlib.util.module_from_spec(pg_spec)
pg_spec.loader.exec_module(PG)


class OwnersTests(unittest.TestCase):
    def services(self):
        return {lane: dict(url=f'postgres://postgres@127.0.0.1:{49170+i}/postgres?sslmode=disable',
                           port=str(49170+i), system=str(12340+i), id=hex(i+10)[2:]*64,
                           directory='/var/lib/postgresql/data')
                for i, lane in enumerate(OWNER.LANES)}

    def test_all_original_phases_and_roots_are_selected_once(self):
        phases = [label for labels in OWNER.LANES.values() for label in labels]
        self.assertCountEqual(phases, OWNER.FIN['OWNER_ORDER'])
        self.assertEqual(len(phases), len(set(phases)))
        self.assertEqual(sum(len(roots) for roots in OWNER.REQUIRED.values()), 72)

    def test_shared_or_missing_service_identity_is_rejected_before_workers(self):
        good = self.services()
        self.assertEqual(set(OWNER.service_bindings(json.dumps(good))), set(OWNER.LANES))
        for key in ('id', 'port', 'system', 'url', 'directory'):
            bad = copy.deepcopy(good)
            bad['wallet'][key] = '/data' if key == 'directory' else good['schema'][key]
            with self.subTest(key=key), self.assertRaises(ValueError):
                OWNER.service_bindings(json.dumps(bad))
        for bad in ({}, [], {**good, 'wallet': None}, {**good, 'foreign': good['wallet']}):
            with self.assertRaises(ValueError):
                OWNER.service_bindings(json.dumps(bad))
        with patch.dict(os.environ, {'GITHUB_ACTIONS': 'true', 'ACCOUNT_FINANCIAL_OWNER_SERVICES': '{}'}), \
                patch.object(OWNER, 'run_lane') as run:
            with self.assertRaises(ValueError):
                OWNER.main()
            run.assert_not_called()

    def test_cpu_memory_and_disjoint_affinity_bound_parallelism(self):
        env = {'PINVOU_CI_CPU_CORES': '12', 'PINVOU_CI_MEMORY_GIB': '24'}
        plan = OWNER.resource_plan(env, range(16), cpu_max='1200000 100000', memory_max=str(24*1024**3))
        self.assertEqual(plan['workers'], 3)
        sets = list(map(set, plan['cpu_sets'].values()))
        self.assertEqual(len(set.union(*sets)), 6)
        self.assertTrue(all(len(group) == 2 for group in sets))
        for kwargs in ({'cpu_max': '900000 100000'}, {'memory_max': str(8*1024**3)}):
            self.assertEqual(OWNER.resource_plan(env, range(16), **kwargs)['mode'], 'serial-resource-bound')
        self.assertEqual(OWNER.resource_plan({}, range(16))['workers'], 1, 'unknown memory is not capacity')
        self.assertEqual(OWNER.resource_plan(env, range(4))['workers'], 1)
        for key in env:
            for value in ('0', '-1', 'nan', 'inf', 'invalid'):
                with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                    OWNER.resource_plan({**env, key: value}, range(16))

    def test_inherited_go_limit_is_never_increased(self):
        for value in (None, 'off', '4GiB'):
            self.assertEqual(OWNER.bounded_go_memory(value), '2GiB')
        for value in ('512MiB', '1GiB', '128B'):
            self.assertEqual(OWNER.bounded_go_memory(value), value)
        for value in ('-1GiB', '0B', '3GB', 'nan', ''):
            with self.assertRaises(ValueError):
                OWNER.bounded_go_memory(value)

    def test_workers_overlap_and_failure_waits_for_all_cleanup(self):
        for failure in (None, 'exit', 'exception'):
            barrier = threading.Barrier(3, timeout=5)
            finished = []
            def worker(lane, binding, ledger, plan):
                barrier.wait()
                self.assertEqual(binding['PINVOU_ACCOUNT_CI_PG_SYSTEM_ID'], self.services()[lane]['system'])
                finished.append(lane)
                if failure == 'exception' and lane == 'wallet':
                    raise OSError('controlled failure')
                return dict(lane=lane, exit_code=7 if failure == 'exit' and lane == 'wallet' else 0)
            with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
                'GITHUB_ACTIONS': 'true', 'ACCOUNT_FINANCIAL_OWNER_SERVICES': json.dumps(self.services()),
                'PINVOU_CI_RESOURCE_LEDGER': directory,
                **OWNER.service_bindings(json.dumps(self.services()))['schema'],
            }), patch.object(OWNER.os, 'sched_getaffinity', return_value=set(range(12)), create=True), \
                    patch.object(OWNER, 'cgroup_limit', return_value=None), \
                    patch.object(OWNER, 'resource_plan', return_value={'workers': 3}), \
                    patch.object(OWNER, 'run_lane', side_effect=worker), contextlib.redirect_stdout(io.StringIO()):
                if failure:
                    with self.assertRaises(RuntimeError):
                        OWNER.main()
                else:
                    OWNER.main()
                self.assertCountEqual(finished, OWNER.LANES)
                summary = json.loads((Path(directory) / 'owners-summary.txt').read_text())
                self.assertEqual(any(row['exit_code'] for row in summary['results']), bool(failure))
                self.assertEqual(list(Path(directory).glob('*.json')), [], 'diagnostics cannot masquerade as leases')

    def lane_evidence(self, directory, lane):
        receipt = directory / f'owner-{lane}-process.json'
        receipt.write_text(json.dumps(dict(schema=1, kind='process', state='released', cleanup_failed=False, exit_code=0)))
        target = directory / f'owner-{lane}-process-resources'
        target.mkdir()
        labels = list(OWNER.LANES[lane]) + ['fixture-template-isolation']
        if lane == 'wallet':
            labels.append('owner-wallet-scope-isolation')
        for label in labels:
            roots = OWNER.REQUIRED.get(label, {'TestDatabaseTemplateIsolationPostgres'} if label == 'fixture-template-isolation'
                                       else {'TestOwnerWalletTemplateScopePostgres'})
            (target / f'financial-phase-{label}.txt').write_text(json.dumps(dict(
                phase=label, root_passes=sorted(roots), exit_code=0, templates={'remaining': 0})))
        return receipt, target

    def test_fan_in_refuses_omissions_wrong_roots_failed_phase_and_leaked_snapshots(self):
        for lane in OWNER.LANES:
            for mutation in ('none', 'missing', 'roots', 'exit', 'templates', 'receipt'):
                with self.subTest(lane=lane, mutation=mutation), tempfile.TemporaryDirectory() as name:
                    directory = Path(name)
                    receipt, target = self.lane_evidence(directory, lane)
                    path = target / f'financial-phase-{OWNER.LANES[lane][0]}.txt'
                    value = json.loads(path.read_text())
                    if mutation == 'missing':
                        path.unlink()
                    elif mutation in ('roots', 'exit', 'templates'):
                        value.update({'roots': {'root_passes': []}, 'exit': {'exit_code': 1},
                                      'templates': {'templates': {'remaining': 1}}}[mutation])
                        path.write_text(json.dumps(value))
                    elif mutation == 'receipt':
                        receipt.write_text(json.dumps(dict(schema=1, kind='process', state='released', cleanup_failed=True)))
                    if mutation == 'none':
                        OWNER.verify_lane(lane, directory)
                    else:
                        with self.assertRaises(RuntimeError):
                            OWNER.verify_lane(lane, directory)

    def test_new_scope_probe_requires_every_original_root_boundary_and_child(self):
        root = 'TestOwnerWalletTemplateScopePostgres'
        self.assertEqual(OWNER.FIN['EXACT_REQUIRED_CHILDREN'][root], {
            'first', 'first/case-a', 'first/case-b', 'second', 'second/case-a', 'second/case-b'})
        expected = {root, *(root + '/' + name for name in OWNER.FIN['EXACT_REQUIRED_CHILDREN'][root])}
        events = [dict(Action=action, Package=OWNER.FIN['BILLING_PACKAGE'], Test=name)
                  for name in expected for action in ('run', 'pass')]
        events.append(dict(Action='pass', Package=OWNER.FIN['BILLING_PACKAGE']))
        for missing in (None, *sorted(expected)):
            selected = [event for event in events if missing is None or event.get('Test') != missing]
            if missing is None:
                OWNER.FIN['verify_results'](selected, {root}, package=OWNER.FIN['BILLING_PACKAGE'], require_children=True, exact_roots=True)
            else:
                with self.assertRaises(RuntimeError):
                    OWNER.FIN['verify_results'](selected, {root}, package=OWNER.FIN['BILLING_PACKAGE'], require_children=True, exact_roots=True)

    def test_owners_profile_enforces_per_service_hard_bounds(self):
        for bad in (None, 'NanoCpus', 'Memory', 'MemorySwap'):
            metadata, items, identities = {}, {}, {}
            for index, (key, (name, image, directory, major)) in enumerate(PG.OWNER_SERVICES.items()):
                identifier, port = hex(10+index)[2:]*64, str(49170+index)
                metadata[name] = {'id': identifier, 'ports': {'5432': port}}
                items[identifier] = dict(Id=identifier, State={'Running': True, 'Health': {'Status': 'healthy'}},
                    Config={'Image': image}, NetworkSettings={'Ports': {'5432/tcp': [{'HostIp': '127.0.0.1', 'HostPort': port}]}},
                    Mounts=[{'Type': 'volume', 'Name': hex(13+index)[2:]*64, 'Destination': directory}],
                    HostConfig={'AutoRemove': True, 'NanoCpus': (2 if key == 'financial' else 1)*10**9,
                                'Memory': 1024**3, 'MemorySwap': 1024**3})
                identities[identifier] = f'{12340+index}|{directory}|5432|postgres|postgres|postgres|t|160015'
            if bad:
                next(iter(items.values()))['HostConfig'][bad] = 0
            def run(argv, **kwargs):
                if argv[:2] == ['docker', 'inspect']:
                    return json.dumps([items[argv[2]]])
                if argv[:2] == ['docker', 'exec']:
                    return identities[argv[2]]
                if argv[0] == 'psql':
                    identifier = next(meta['id'] for meta in metadata.values() if ':' + meta['ports']['5432'] + '/' in argv[1])
                    return identities[identifier]
                raise AssertionError('unexpected admission command')
            with tempfile.TemporaryDirectory() as name, patch.dict(os.environ, {
                'ACCOUNT_CI_SERVICES': json.dumps(metadata), 'GITHUB_OUTPUT': str(Path(name) / 'output'),
            }), patch.object(PG, 'run', side_effect=run), contextlib.redirect_stdout(io.StringIO()):
                if bad:
                    with self.assertRaises(ValueError):
                        PG.main(services=PG.OWNER_SERVICES)
                else:
                    PG.main(services=PG.OWNER_SERVICES)
                    outputs = (Path(name) / 'output').read_text().splitlines()
                    bindings = OWNER.service_bindings(next(row.split('=', 1)[1] for row in outputs if row.startswith('owners_services=')))
                    self.assertEqual(set(bindings), set(OWNER.LANES))


if __name__ == '__main__':
    unittest.main()

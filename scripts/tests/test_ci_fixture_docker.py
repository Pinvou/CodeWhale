import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('fixture_docker', ROOT / 'scripts/ci-fixture-docker.py')
docker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(docker)
CID = 'a' * 64


class DockerLeaseTests(unittest.TestCase):
    def lease(self, root, lease='1' * 32, endpoint=None):
        path = root / ('docker-' + lease + '.json')
        value = dict(schema=1, kind='docker', id=lease, state='registered', endpoint=endpoint or docker.endpoint())
        docker.record(path, value)
        (root / ('docker-' + lease + '.cid')).write_text(CID, encoding='ascii')
        return path

    def engine(self, *, dirty=False, fail_stop=False, volume=False):
        alive, volumes, calls = {CID, 'b' * 64}, {'owned-anonymous', 'shared-cache'}, []
        def run(args):
            calls.append(args)
            if args[0] == 'ps':
                return '\n'.join(sorted(alive))
            if args[0] == 'inspect':
                mounts = [dict(Type='volume', Name='owned-anonymous', Destination='/data'),
                          dict(Type='volume', Name='shared-cache', Destination='/cache')]
                return '\n'.join(map(json.dumps, [CID, ['shared-cache:/cache'], [], mounts]))
            if args[0] == 'stop':
                if fail_stop:
                    raise RuntimeError('stop failed')
                return ''
            if args[0] == 'rm':
                self.assertEqual(args, ['rm', '--volumes', CID])
                if not dirty:
                    alive.discard(CID)
                if not volume:
                    volumes.discard('owned-anonymous')
                return ''
            if args[:2] == ['volume', 'ls']:
                return '\n'.join(sorted(volumes))
            self.fail('unexpected cleanup: ' + str(args))
        return run, calls, alive, volumes

    def test_interrupted_create_is_recovered_by_engine_written_id_only(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = self.lease(root)
            run, calls, alive, volumes = self.engine()
            self.assertEqual(docker.recover(root, run), 1)
            self.assertEqual(json.loads(path.read_text())['state'], 'released')
            self.assertEqual(alive, {'b' * 64})
            self.assertEqual(volumes, {'shared-cache'})
            self.assertEqual(docker.recover(root, run), 0)
            self.assertFalse(any('prune' in call for call in calls))

    def test_validate_complete_set_before_mutating_any_container(self):
        for invalid in ['missing', 'symlink', 'endpoint', 'short-id']:
            with self.subTest(invalid=invalid), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                self.lease(root)
                self.lease(root, lease='2' * 32, endpoint='wrong' if invalid == 'endpoint' else None)
                cid = root / ('docker-' + '2' * 32 + '.cid')
                if invalid in {'missing', 'symlink'}:
                    cid.unlink()
                if invalid == 'symlink':
                    try:
                        cid.symlink_to(root / ('docker-' + '1' * 32 + '.cid'))
                    except OSError:
                        continue  # Windows without symlink permission; Linux proof covers it.
                if invalid == 'short-id':
                    cid.write_text('a' * 12)
                run, calls, _, _ = self.engine()
                with self.assertRaises((OSError, RuntimeError)):
                    docker.recover(root, run)
                self.assertFalse(calls)

    def test_successful_cli_does_not_substitute_for_zero_postcondition(self):
        for options in [dict(dirty=True), dict(volume=True), dict(fail_stop=True)]:
            with self.subTest(options=options), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                path = self.lease(root)
                run, _, _, _ = self.engine(**options)
                with self.assertRaises(RuntimeError):
                    docker.recover(root, run)
                self.assertNotEqual(json.loads(path.read_text())['state'], 'released')

    def test_normal_finally_removal_still_checks_previously_registered_volume(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = self.lease(root)
            value = json.loads(path.read_text())
            docker.record(path, {**value, 'anonymous_volumes': ['still-owned']})
            def run(args):
                if args[0] == 'ps':
                    return ''
                if args[:2] == ['volume', 'ls']:
                    return 'still-owned\nshared-cache'
                self.fail('an absent container must not authorize another deletion')
            with self.assertRaises(RuntimeError):
                docker.recover(root, run)
            self.assertNotEqual(json.loads(path.read_text())['state'], 'released')

    def test_cli_delegates_streams_and_keeps_arguments_out_of_receipts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            class Child:
                def __init__(self, args):
                    self.args = args
                    self.path = Path(args[3])
                    self.path.write_text(CID)
                def wait(self):
                    return 7
            with patch.object(docker, 'executable', return_value='/real/docker'), patch.dict(docker.os.environ, PINVOU_CI_RESOURCE_LEDGER=directory), patch.object(docker.sys, 'argv', ['docker', 'run', '--rm', '-e', 'SYNTHETIC_SECRET=private', 'image']), patch.object(docker.subprocess, 'Popen', side_effect=Child) as popen, patch.object(docker.signal, 'signal'), patch.object(docker, 'engine_run', return_value=''):
                with self.assertRaises(SystemExit) as caught:
                    docker.main()
                self.assertEqual(caught.exception.code, 7)
                self.assertEqual(popen.call_args.args[0][:3], ['/real/docker', 'run', '--cidfile'])
                text = next(root.glob('*.json')).read_text()
                self.assertNotIn('private', text)
                self.assertEqual(json.loads(text)['exit_code'], 7)


if __name__ == '__main__':
    unittest.main()

"""Local safety/contract tests; do not access a real host or Docker daemon."""
import contextlib
import hashlib
import io
import json
from pathlib import Path
import shutil
import uuid
import unittest
from unittest.mock import patch

import controller as m


class ControllerTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(__file__).resolve().parent / ('.test-' + uuid.uuid4().hex)
        self.root.mkdir()
        self.model = self.root / 'model'
        self.model.mkdir()
        (self.model / 'encoding').mkdir()
        (self.model / 'encoding/encoding_dsv4.py').write_text('# encoder')
        (self.model / 'config.json').write_text('{}')
        (self.model / 'tokenizer.json').write_text('{}')
        (self.model / 'a.safetensors').write_bytes(b'weights')
        (self.model / 'model.safetensors.index.json').write_text(json.dumps({'weight_map': {'a': 'a.safetensors'}}))
        self.c = {'role': 'head', 'worker_user': 'customer', 'worker_dir': '/home/customer/megacube-deepseek',
                  'rails': [{'iface': 'eth1', 'ip': '192.168.240.1', 'hca': 'mlx5_0', 'port': '1'}]}

    def tearDown(self):
        assert self.root.resolve().parent == Path(__file__).resolve().parent
        shutil.rmtree(self.root)

    def test_model_complete(self):
        m.check_model(self.model)
        self.assertEqual(m.model_hashes(self.model)['a.safetensors'], hashlib.sha256(b'weights').hexdigest())

    def test_missing_shard_rejected(self):
        (self.model / 'a.safetensors').unlink()
        with self.assertRaises(RuntimeError): m.check_model(self.model)

    def test_path_traversal_rejected(self):
        (self.root / 'outside').write_bytes(b'bad')
        (self.model / 'model.safetensors.index.json').write_text(json.dumps({'weight_map': {'a': '../outside'}}))
        with self.assertRaises(RuntimeError): m.check_model(self.model)

    def test_corruption_changes_hash(self):
        before = m.model_hashes(self.model)
        (self.model / 'a.safetensors').write_bytes(b'changed')
        self.assertNotEqual(before, m.model_hashes(self.model))

    def test_download_metadata_excluded(self):
        before = m.model_hashes(self.model)
        (self.model / '.cache').mkdir()
        (self.model / '.cache/download').write_text('metadata')
        self.assertEqual(before, m.model_hashes(self.model))

    def test_worker_failure_does_not_skip_local_stop(self):
        with patch.object(m, 'remote', side_effect=RuntimeError('unreachable')), patch.object(m, 'node') as node:
            with self.assertRaises(RuntimeError): m.both(self.c, 'stop')
            node.assert_called_once_with('stop', {})

    def test_remote_shell_quoting(self):
        c = dict(self.c, worker_dir='/home/test/a b;echo nope')
        with patch.object(m, 'run', return_value='') as run:
            m.remote(c, 'check')
            command = run.call_args.args[0][-1]
            import shlex
            self.assertEqual(shlex.split(command)[1], '/home/test/a b;echo nope/controller.py')

    def test_ssh_host_key_checking(self):
        args = m.ssh_base(self.c)
        self.assertIn('StrictHostKeyChecking=yes', args)
        self.assertIn('BatchMode=yes', args)
        self.assertEqual(args[-1], 'customer@192.168.240.2')

    def create_gid(self, hca, index, ip, iface='eth1'):
        root = self.root / hca / 'ports/1'
        for directory, content in [('gids', '::ffff:' + ip), ('gid_attrs/types', 'RoCE v2'), ('gid_attrs/ndevs', iface)]:
            (root / directory).mkdir(parents=True, exist_ok=True)
            (root / directory / str(index)).write_text(content)

    def test_gid_uses_ip_and_interface(self):
        self.create_gid('mlx5_0', 1, '192.168.240.2')
        self.create_gid('mlx5_0', 3, '192.168.240.1')
        self.assertEqual(m.gid_index(self.c['rails'], self.root), 3)

    def test_gid_no_common_index_fails(self):
        self.create_gid('mlx5_0', 3, '192.168.240.1')
        self.create_gid('mlx5_1', 4, '192.168.241.1', 'eth2')
        rails = self.c['rails'] + [dict(hca='mlx5_1', port='1', iface='eth2', ip='192.168.241.1')]
        with self.assertRaises(RuntimeError): m.gid_index(rails, self.root)

    def test_gid_common_index(self):
        self.create_gid('mlx5_0', 3, '192.168.240.1')
        self.create_gid('mlx5_1', 3, '192.168.241.1', 'eth2')
        rails = self.c['rails'] + [dict(hca='mlx5_1', port='1', iface='eth2', ip='192.168.241.1')]
        self.assertEqual(m.gid_index(rails, self.root), 3)

    def test_bundle_tamper_rejected(self):
        (self.root / 'file').write_text('x')
        (self.root / 'SHA256SUMS').write_text(hashlib.sha256(b'x').hexdigest() + '  file\n')
        with patch.object(m, 'ROOT', self.root):
            m.verify_bundle()
            (self.root / 'file').write_text('bad')
            with self.assertRaises(RuntimeError): m.verify_bundle()

    def test_stop_vs_uninstall(self):
        (self.root / 'runtime').mkdir()
        (self.root / 'runtime/node.env').write_text('')
        with patch.object(m, 'ROOT', self.root), patch.object(m, 'config', return_value=self.c), patch.object(m, 'compose') as compose:
            m.node('stop', {})
            compose.assert_called_with(['stop', '--timeout', '30'])
            m.node('uninstall', {})
            compose.assert_called_with(['down', '--timeout', '30'])

    def test_invalid_timeout_has_no_side_effect(self):
        with patch.object(m, 'remote') as remote:
            with self.assertRaises(RuntimeError): m.start(self.c, 0)
            remote.assert_not_called()

    def test_start_failure_stops_both_nodes(self):
        manifest = self.root / 'model-manifest.json'
        manifest.write_text('{}')
        digest = hashlib.sha256(manifest.read_bytes()).hexdigest()
        def remote(c, action, **kwargs):
            if action == 'image-id': return 'same'
            if action == 'manifest-id': return digest
            return None
        with patch.object(m, 'DATA', self.root), patch.object(m, 'pair_check'), patch.object(m, 'both') as both, \
             patch.object(m, 'run', return_value='same'), patch.object(m, 'remote', side_effect=remote), \
             patch.object(m, 'node', side_effect=RuntimeError('head failed')):
            with self.assertRaises(RuntimeError): m.start(self.c, 10)
            self.assertIn(unittest.mock.call(self.c, 'stop'), both.call_args_list)

    def test_image_mismatch_blocks_start(self):
        with patch.object(m, 'pair_check'), patch.object(m, 'both'), patch.object(m, 'run', return_value='one'), \
             patch.object(m, 'remote', return_value='two'), patch.object(m, 'node') as node:
            with self.assertRaises(RuntimeError): m.start(self.c, 10)
            node.assert_not_called()

    def test_generated_profile_matches_tested_parameters(self):
        (self.root / 'runtime').mkdir()
        c = dict(self.c, bind='127.0.0.1', port=8888)
        with patch.object(m, 'ROOT', self.root), patch.object(m, 'DATA', self.root / 'data'), \
             patch.object(m, 'gid_index', return_value=3), patch.object(m, 'compose'), patch.object(Path, 'chmod'):
            m.write_env(c)
            env = (self.root / 'runtime/node.env').read_text()
            for text in ["MAX_MODEL_LEN='262144'", "MAX_NUM_SEQS='8'", "MAX_NUM_BATCHED_TOKENS='16384'",
                         "GPU_MEMORY_UTILIZATION='0.83'", "MTP_NUM_TOKENS='6'", "NODE_RANK='0'", "HEADLESS=''",
                         "NCCL_IB_HCA='=mlx5_0:1'", "NCCL_IB_GID_INDEX='3'", "DSPARK_RESTART_POLICY='no'"]:
                self.assertIn(text, env)

    def test_smoke_rejects_empty_response(self):
        with patch.object(m, 'api', return_value=b'{"choices": []}'):
            with self.assertRaises(RuntimeError): m.smoke(self.c)

    def test_vision_sends_inline_png(self):
        with patch.object(m, 'api', return_value=json.dumps({'choices': [{'message': {'content': '红色'}}]}).encode()) as api:
            m.vision_smoke(self.c)
            body = api.call_args.args[2]
            url = body['messages'][0]['content'][1]['image_url']['url']
            self.assertTrue(url.startswith('data:image/png;base64,iVBOR'))


if __name__ == '__main__':
    unittest.main()

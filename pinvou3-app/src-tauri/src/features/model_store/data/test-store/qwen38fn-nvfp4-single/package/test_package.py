import hashlib
import json
from pathlib import Path
import shutil
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import uuid

import controller as c
import model_assets as a


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(__file__).resolve().parent / ('.test-' + uuid.uuid4().hex)
        self.root.mkdir()
        self.model = self.root / 'model'; self.model.mkdir()
        self.state = self.root / 'state'; self.state.mkdir()
        data = {'config.json': json.dumps({'architectures': ['Qwen4ExpForConditionalGeneration']}).encode(),
                'model.safetensors.index.json': b'{"weight_map":{"weight":"model-1.safetensors"}}',
                'tokenizer.json': b'{}', 'model-1.safetensors': b'original-weights'}
        self.rows = []
        for name, content in data.items():
            (self.model / name).write_bytes(content)
            algorithm = 'sha256' if name.endswith('safetensors') else 'git-sha1'
            h = hashlib.sha256(content) if algorithm == 'sha256' else hashlib.sha1(f'blob {len(content)}\0'.encode() + content)
            self.rows.append(dict(name=name, size=len(content), algorithm=algorithm, digest=h.hexdigest()))
        self.source = dict(repo=a.REPO, revision='a' * 40, files=self.rows)
        a.write_json(self.state / 'model-source.json', self.source)
        self.cfg = dict(bind='127.0.0.1', port=8000, model_dir=str(self.model), cache_dir=str(self.root / 'cache'),
                        revision='', startup_timeout=3600)
        self.info = {'State': {'Running': True, 'Status': 'running'},
                     'Config': {'Labels': {'megacube.owner': c.OWNER, 'megacube.install': str(c.ROOT)}}}

    def tearDown(self):
        assert self.root.resolve().parent == Path(__file__).resolve().parent
        shutil.rmtree(self.root)

    def test_official_hash_algorithms(self):
        for row in self.rows: self.assertTrue(a.file_matches(self.model, row))

    def test_full_then_quick_verify(self):
        a.verify(self.model, self.state)
        a.quick_verify(self.model, self.state)

    def test_corruption_fails_full_verify(self):
        (self.model / 'model-1.safetensors').write_bytes(b'bad')
        with self.assertRaises(RuntimeError): a.verify(self.model, self.state)
        self.assertFalse((self.state / 'model-verified.json').exists())

    def test_modified_file_fails_quick_verify(self):
        a.verify(self.model, self.state)
        (self.model / 'model-1.safetensors').write_bytes(b'bad')
        with self.assertRaises(RuntimeError): a.quick_verify(self.model, self.state)

    def test_manifest_change_fails_quick_verify(self):
        a.verify(self.model, self.state)
        self.source['revision'] = 'b' * 40
        a.write_json(self.state / 'model-source.json', self.source)
        with self.assertRaises(RuntimeError): a.quick_verify(self.model, self.state)

    def test_missing_file(self):
        (self.model / 'tokenizer.json').unlink()
        self.assertFalse(a.file_matches(self.model, self.rows[2]))

    def test_extra_shard_rejected(self):
        (self.model / 'extra.safetensors').write_bytes(b'extra')
        with self.assertRaises(RuntimeError): a.structure(self.model, self.source)

    def test_path_traversal_rejected(self):
        for name in ['../secret', '/outside', 'a/../../secret', 'a\\secret']:
            with self.assertRaises(RuntimeError): a.safe_file(self.model, name)

    def test_duplicate_source_rejected(self):
        self.source['files'].append(self.rows[0])
        a.write_json(self.state / 'model-source.json', self.source)
        with self.assertRaises(RuntimeError): a.load_source(self.state / 'model-source.json')

    def test_download_reuses_revision_and_repairs(self):
        (self.model / 'model-1.safetensors').write_bytes(b'bad')
        seen = []
        def snapshot(*args, **kwargs): seen.append(kwargs['revision'])
        def repair(repo, name, **kwargs):
            self.assertTrue(kwargs['force_download'])
            (self.model / name).write_bytes(b'original-weights')
        fake = SimpleNamespace(HfApi=lambda: None, snapshot_download=snapshot, hf_hub_download=repair)
        with patch.dict('sys.modules', {'huggingface_hub': fake}):
            a.download(self.model, self.state, '')
        self.assertEqual(seen, ['a' * 40])
        a.quick_verify(self.model, self.state)

    def test_requested_revision_conflict(self):
        fake = SimpleNamespace(HfApi=lambda: None, snapshot_download=lambda **kw: None, hf_hub_download=lambda **kw: None)
        with patch.dict('sys.modules', {'huggingface_hub': fake}):
            with self.assertRaises(RuntimeError): a.download(self.model, self.state, 'b' * 40)

    def test_recipe_preserves_tuning(self):
        args = c.launch_args(self.cfg, 'sha256:image', 'signature')
        for flag, value in {'--tensor-parallel-size': '1', '--max-model-len': '262144', '--max-num-seqs': '1',
                            '--max-num-batched-tokens': '2048', '--gpu-memory-utilization': '0.80'}.items():
            self.assertEqual(args[args.index(flag) + 1], value)
        self.assertIn('--enable-prefix-caching', args)
        self.assertNotIn('--no-enable-prefix-caching', args)
        self.assertIn('QWEN4EXP_PLE_STAGED=1', args)
        self.assertNotIn('--kv-cache-dtype', args)
        self.assertNotIn('--compilation-config', args)
        self.assertEqual(json.loads(args[args.index('--speculative-config') + 1])['num_speculative_tokens'], 3)
        self.assertEqual(args[args.index('sha256:image') + 1], '/models/qwen38fn')
        self.assertEqual(sum('readonly' in x for x in args), 10)

    def test_spaces_in_path_are_one_argument(self):
        cfg = dict(self.cfg, model_dir='/home/customer/model data')
        args = c.launch_args(cfg, 'image', 'signature')
        self.assertIn('type=bind,src=/home/customer/model data,dst=/models/qwen38fn,readonly', args)

    def test_foreign_container_not_owned(self):
        self.info['Config']['Labels']['megacube.owner'] = 'foreign'
        with patch.object(c, 'run', side_effect=['id', json.dumps([self.info])]):
            with self.assertRaises(RuntimeError): c.inspect_container()

    def test_different_install_not_owned(self):
        self.info['Config']['Labels']['megacube.install'] = '/another/install'
        with patch.object(c, 'run', side_effect=['id', json.dumps([self.info])]):
            with self.assertRaises(RuntimeError): c.inspect_container()

    def test_uninstall_preserves_weights(self):
        before = {p.name: p.read_bytes() for p in self.model.iterdir()}
        with patch.object(c, 'inspect_container', return_value=self.info), patch.object(c, 'save_logs'), patch.object(c, 'run') as run:
            c.uninstall()
            self.assertEqual(run.call_args_list[-1].args[0], ['docker', 'rm', c.NAME])
            self.assertEqual(len(run.call_args_list), 2)
        self.assertEqual(before, {p.name: p.read_bytes() for p in self.model.iterdir()})

    def test_uninstall_absent_idempotent(self):
        with patch.object(c, 'inspect_container', return_value=None), patch.object(c, 'run') as run:
            c.uninstall(); run.assert_not_called()

    def test_foreign_service_never_removed(self):
        with patch.object(c, 'inspect_container', side_effect=RuntimeError('foreign')), patch.object(c, 'run') as run:
            with self.assertRaises(RuntimeError): c.uninstall()
            run.assert_not_called()

    def test_wrong_image_identity_rejected(self):
        with patch.object(c, 'STATE', self.root / 'empty'), patch.object(c, 'run'), \
             patch.object(c, 'image_info', return_value={'Id': 'sha256:wrong', 'RepoDigests': []}):
            with self.assertRaises(RuntimeError): c.image_prepare()

    def image(self, identity='sha256:config', ref=None):
        return {'Id': identity, 'RepoDigests': [ref or c.IMAGE], 'Architecture': 'arm64', 'Os': 'linux'}

    def test_both_storage_backends_and_offline_reuse(self):
        for identity in ['sha256:config', c.RECORDED_IDENTITY]:
            with self.subTest(identity=identity), patch.object(c, 'STATE', self.state), \
                 patch.object(c, 'run') as run, patch.object(c, 'image_info', return_value=self.image(identity)):
                self.assertEqual(c.image_prepare(), c.IMAGE)
                self.assertEqual(c.locked_image(), c.IMAGE)
                run.assert_not_called()
            record = json.loads((self.state / 'image.json').read_text())
            self.assertEqual(record['id'], identity)
            self.assertEqual(record['schema'], 2)

    def test_pull_only_pinned_platform_manifest(self):
        missing = subprocess.CalledProcessError(1, ['docker', 'image', 'inspect'])
        with patch.object(c, 'STATE', self.state), patch.object(c, 'run') as run, \
             patch.object(c, 'image_info', side_effect=[missing, self.image()]) as inspect:
            self.assertEqual(c.image_prepare(), c.IMAGE)
            run.assert_called_once_with(['docker', 'pull', '--platform', 'linux/arm64', c.IMAGE])
            self.assertTrue(all(x.args == (c.IMAGE,) for x in inspect.call_args_list))

    def test_config_id_alone_is_not_manifest_evidence(self):
        info = self.image(c.RECORDED_IDENTITY)
        info['RepoDigests'] = []
        with self.assertRaisesRegex(RuntimeError, '清单身份不一致'): c.verify_image(info)

    def test_index_or_other_repository_rejected(self):
        for ref in [c.IMAGE_REPOSITORY + '@sha256:f5df5cc3302b5f404848c4eca88d7bf7ed5226e151c056da22816d7734644d67',
                    'other/repository@' + c.RECORDED_IDENTITY]:
            with self.subTest(ref=ref), self.assertRaises(RuntimeError): c.verify_image(self.image(ref=ref))

    def test_docker_hub_canonical_names_accepted(self):
        for prefix in ['', 'docker.io/', 'index.docker.io/']:
            c.verify_image(self.image(ref=prefix + c.IMAGE))

    def test_wrong_platform_rejected_even_with_correct_digest(self):
        for field, value in [('Architecture', 'amd64'), ('Os', 'windows')]:
            info = self.image(); info[field] = value
            with self.subTest(field=field), self.assertRaises(RuntimeError): c.verify_image(info)

    def test_legacy_lock_migrated_only_after_verification(self):
        old = {'id': 'sha256:old', 'source': 'old:tag', 'repo_digests': ['untrusted/repo@sha256:old']}
        a.write_json(self.state / 'image.json', old)
        with patch.object(c, 'STATE', self.state), patch.object(c, 'run') as run, \
             patch.object(c, 'image_info', return_value=self.image()) as inspect:
            self.assertEqual(c.image_prepare(), c.IMAGE)
            inspect.assert_called_once_with(c.IMAGE)
            run.assert_not_called()
        backups = list(self.state.glob('image.previous-*.json'))
        self.assertEqual(len(backups), 1)
        self.assertEqual(json.loads(backups[0].read_text()), old)

    def test_bad_image_preserves_legacy_lock(self):
        old = {'source': 'old:tag'}
        a.write_json(self.state / 'image.json', old)
        before = (self.state / 'image.json').read_bytes()
        with patch.object(c, 'STATE', self.state), patch.object(c, 'run'), \
             patch.object(c, 'image_info', return_value=self.image(ref='wrong@sha256:wrong')):
            with self.assertRaises(RuntimeError): c.image_prepare()
        self.assertEqual((self.state / 'image.json').read_bytes(), before)
        self.assertEqual(list(self.state.glob('image.previous-*.json')), [])

    def test_failed_pull_does_not_create_lock_or_fallback(self):
        missing = subprocess.CalledProcessError(1, ['docker'])
        with patch.object(c, 'STATE', self.state), patch.object(c, 'run', side_effect=missing) as run, \
             patch.object(c, 'image_info', side_effect=missing):
            with self.assertRaises(subprocess.CalledProcessError): c.image_prepare()
            run.assert_called_once_with(['docker', 'pull', '--platform', c.IMAGE_PLATFORM, c.IMAGE])
        self.assertFalse((self.state / 'image.json').exists())

    def test_start_rejects_legacy_lock_without_pull(self):
        a.write_json(self.state / 'image.json', {'source': 'old:tag'})
        with patch.object(c, 'STATE', self.state), patch.object(c, 'run') as run:
            with self.assertRaisesRegex(RuntimeError, '旧版'): c.locked_image()
            run.assert_not_called()

    def test_start_survives_backend_id_change_but_rejects_manifest_change(self):
        with patch.object(c, 'STATE', self.state), patch.object(c, 'image_info', return_value=self.image()), \
             patch.object(c, 'run'):
            c.image_prepare()
            with patch.object(c, 'image_info', return_value=self.image(c.RECORDED_IDENTITY)):
                self.assertEqual(c.locked_image(), c.IMAGE)
            with patch.object(c, 'image_info', return_value=self.image(ref='wrong@sha256:wrong')):
                with self.assertRaises(RuntimeError): c.locked_image()

    def test_engine_does_not_download_weights_or_start_inference(self):
        with patch.object(c, 'preflight'), patch.object(c, 'ensure_stopped'), \
             patch.object(c, 'image_prepare', return_value=c.IMAGE), patch.object(c, 'gpu_probe') as probe, \
             patch.object(c, 'download') as download, patch.object(c, 'start') as start:
            c.engine(self.cfg)
            probe.assert_called_once_with(c.IMAGE)
            download.assert_not_called(); start.assert_not_called()

    def test_engine_refuses_running_service(self):
        with patch.object(c, 'preflight'), patch.object(c, 'ensure_stopped', side_effect=RuntimeError('running')), \
             patch.object(c, 'image_prepare') as prepare:
            with self.assertRaises(RuntimeError): c.engine(self.cfg)
            prepare.assert_not_called()

    def test_pin_matches_registry_evidence(self):
        evidence = json.loads((c.ROOT / 'IMAGE-PROVENANCE.json').read_text(encoding='utf-8'))
        manifest = evidence['responses'][1]
        self.assertEqual(manifest['digest'], c.RECORDED_IDENTITY)
        self.assertEqual(evidence['config_summary']['architecture'], 'arm64')
        self.assertEqual(evidence['config_summary']['os'], 'linux')
        self.assertEqual(manifest['manifest']['config']['digest'], evidence['config_summary']['digest'])
        matches = [x for x in evidence['responses'][0]['manifest']['manifests']
                   if x.get('platform', {}).get('architecture') == 'arm64'
                   and x.get('platform', {}).get('os') == 'linux']
        self.assertEqual([x['digest'] for x in matches], [c.RECORDED_IDENTITY])

    def test_smoke_rejects_wrong_model(self):
        with patch.object(c, 'api', return_value=b'{"data":[{"id":"other"}]}'):
            with self.assertRaises(RuntimeError): c.smoke(self.cfg)

    def test_smoke_rejects_empty_generation(self):
        with patch.object(c, 'api', side_effect=[b'{"data":[{"id":"qwen3.8-flash-next"}]}', b'{"choices":[]}']):
            with self.assertRaises(RuntimeError): c.smoke(self.cfg)

    def test_start_failure_stops_created_service(self):
        with patch.object(c, 'preflight'), patch.object(c, 'quick_verify'), patch.object(c, 'locked_image', return_value='image'), \
             patch.object(c, 'fingerprint', return_value='signature'), patch.object(c, 'inspect_container', return_value=None), \
             patch.object(Path, 'read_text', return_value='MemAvailable: 120000000 kB'), patch.object(c.socket, 'socket'), \
             patch.object(c, 'gpu_probe'), patch.object(c, 'run'), patch.object(c, 'wait_ready', side_effect=RuntimeError('load failed')), \
             patch.object(c, 'save_logs') as logs, patch.object(c, 'stop') as stop:
            with self.assertRaises(RuntimeError): c.start(self.cfg)
            logs.assert_called_once(); stop.assert_called_once()

    def test_patch_contains_local_mtp_fix(self):
        source = (c.ROOT / 'runtime/patch/upstream-overlays/modelopt.py').read_text(encoding='utf-8')
        self.assertIn('isinstance(layer, RoutedExperts) and "mtp" in prefix', source)
        self.assertIn('weight_block_size=[128, 128]', source)


if __name__ == '__main__':
    unittest.main()

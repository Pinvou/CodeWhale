import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest

SPEC = importlib.util.spec_from_file_location('network_env', Path(__file__).parents[1] / 'ci-network-environment.py')
ENV = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ENV)


class NetworkEnvironmentTests(unittest.TestCase):
    @unittest.skipUnless(shutil.which('node'), 'Node reports the actual safe download receipt')
    def test_report_retains_stage_evidence_without_raw_errors_urls_or_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'statistics.json').write_text(json.dumps({'events':[
                {'phase':'attempt', 'host':'static.rust-lang.org', 'exit':60,
                 'stderr':'secret-raw-error', 'url':'https://example.invalid/?secret=query',
                 'failure_stage':'tls-handshake'},
                {'phase':'request-failed', 'error_type':'OSError', 'errno':2}]}))
            result = subprocess.run([shutil.which('node'),
                str(Path(__file__).parents[1] / 'ci-rust-network.cjs'), '--report'],
                env={**os.environ, 'PINVOU_CI_RUST_DIST_STATE':str(root / 'ready.json')},
                capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('tls-handshake', result.stdout)
            self.assertIn('"errno":2', result.stdout)
            self.assertNotIn('secret', result.stdout)
            self.assertNotIn('example.invalid', result.stdout)

    def test_ci_entry_starts_owned_cache_preserves_bypasses_and_retires_on_state_removal(self):
        with tempfile.TemporaryDirectory(prefix='rust CI ') as directory:
            root = Path(directory)
            cargo = root / 'cargo'; cargo.mkdir()
            environment = root / 'github-env'
            result = subprocess.run([__import__('sys').executable,
                str(Path(__file__).parents[1] / 'ci-network-environment.py'),
                '--cargo-home', str(cargo), '--cache', str(root / 'cache'), '--dist-cache'],
                env={**os.environ, 'GITHUB_ACTIONS': 'true', 'RUNNER_TEMP': str(root),
                     'GITHUB_ENV': str(environment), 'NO_PROXY': 'example.invalid'},
                capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            values = dict(line.split('=', 1) for line in environment.read_text().splitlines())
            state = Path(values['PINVOU_CI_RUST_DIST_STATE'])
            ready = json.loads(state.read_text())
            try:
                self.assertEqual(values['RUSTUP_DIST_SERVER'], f"http://127.0.0.1:{ready['port']}")
                self.assertEqual(values['NO_PROXY'], 'example.invalid,localhost,127.0.0.1,::1')
                self.assertEqual(values['PINVOU_CI_RUST_DIST_ORIGIN'], 'https://static.rust-lang.org')
            finally:
                state.unlink(missing_ok=True)
            # This integration path makes no upstream request and leaves no
            # listener behind even without a real Runner's orphan cleanup.
            import socket
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                try:
                    with socket.create_connection(('127.0.0.1', ready['port']), timeout=.2):
                        pass
                except OSError:
                    break
                time.sleep(.05)
            else:
                self.fail('owned Rust listener did not retire')

    def test_actual_node_entry_preserves_the_toolchain_and_exports_job_sources(self):
        node = shutil.which('node')
        if not node:
            self.skipTest('Node is exercised by the real transport workflow')
        with tempfile.TemporaryDirectory(prefix='rust network ') as directory:
            root = Path(directory)
            cargo = root / 'cargo'; cargo.mkdir()
            rustup = root / 'rustup'; rustup.mkdir()
            (rustup / 'sentinel').write_bytes(b'installed-toolchain')
            environment = root / 'github-env'
            result = subprocess.run([node, str(Path(__file__).parents[1] / 'ci-rust-network.cjs')],
                env={**os.environ, 'GITHUB_ACTIONS': 'false', 'CARGO_HOME':str(cargo), 'RUSTUP_HOME':str(rustup),
                     'GITHUB_ENV':str(environment), 'PINVOU_CI_DOWNLOAD_CACHE':str(root / 'cache')},
                capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('RUSTUP_DIST_SERVER=https://static.rust-lang.org\n', environment.read_text())
            self.assertEqual((rustup / 'sentinel').read_bytes(), b'installed-toolchain')

    def test_only_known_mirror_is_removed_and_every_other_setting_survives(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = root / 'config.toml'
            other = '[http]\nmultiplexing = false\n[profile.dev.package.codewhale-tui]\nopt-level = 1\n'
            original = '[source.crates-io]\nreplace-with = "rsproxy-ci"\n\n[source.rsproxy-ci]\nregistry = "sparse+https://rsproxy.cn/index/"\n\n' + other
            config.write_text(original)
            self.assertTrue(ENV.retire_mirror(config))
            self.assertIn(other, config.read_text())
            self.assertNotIn('rsproxy', config.read_text())
            self.assertEqual(next(root.glob('*.before-network*')).read_text(), original)
            self.assertFalse(ENV.retire_mirror(config))

    def test_custom_registries_and_toolchain_bytes_are_not_rewritten(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / 'config.toml'
            text = '[source.crates-io]\nreplace-with = "company"\n[source.company]\nregistry = "https://example.invalid/index"\n'
            config.write_text(text)
            self.assertFalse(ENV.retire_mirror(config))
            self.assertEqual(config.read_text(), text)

    def test_extra_source_policy_fails_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / 'config.toml'
            text = '[source.crates-io]\nreplace-with = "rsproxy-ci"\nlocal-registry = "custom"\n[source.rsproxy-ci]\nregistry = "sparse+https://rsproxy.cn/index/"\n'
            config.write_text(text)
            with self.assertRaises(ValueError):
                ENV.retire_mirror(config)
            self.assertEqual(config.read_text(), text)

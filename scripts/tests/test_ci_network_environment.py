import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location('network_env', Path(__file__).parents[1] / 'ci-network-environment.py')
ENV = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ENV)


class NetworkEnvironmentTests(unittest.TestCase):
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
                env={**os.environ, 'CARGO_HOME':str(cargo), 'RUSTUP_HOME':str(rustup),
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

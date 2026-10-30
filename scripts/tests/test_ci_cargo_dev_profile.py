import importlib.util
from pathlib import Path
import tempfile
import tomllib
import unittest

SPEC = importlib.util.spec_from_file_location(
    "ci_cargo_dev_profile", Path(__file__).resolve().parents[1] / "ci-cargo-dev-profile.py")
PROFILE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROFILE)


class CargoDevProfileTests(unittest.TestCase):
    def test_fresh_and_repeated_jobs_produce_one_valid_override(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "cargo/config.toml"
            PROFILE.configure(config, PROFILE.PAYLOAD)
            first = config.read_bytes()
            PROFILE.configure(config, PROFILE.PAYLOAD)
            self.assertEqual(first, config.read_bytes())
            self.assertEqual(0, tomllib.loads(config.read_text())["profile"]["dev"]["package"]["codewhale-tui"]["opt-level"])

    def test_repairs_duplicate_managed_tables_without_losing_other_settings(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config.toml"
            other = '[net]\nretry = 4\n\n[profile.release]\ndebug = 1\n'
            config.write_text(PROFILE.PAYLOAD + "\n" + other + "\n" + PROFILE.PAYLOAD)
            PROFILE.configure(config, PROFILE.PAYLOAD)
            parsed = tomllib.loads(config.read_text())
            self.assertEqual(4, parsed["net"]["retry"])
            self.assertEqual(1, parsed["profile"]["release"]["debug"])
            self.assertEqual(1, config.read_text().count(PROFILE.HEADER))

    def test_custom_profile_is_preserved_and_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config.toml"
            original = PROFILE.HEADER + "\nopt-level = 2\n"
            config.write_text(original)
            with self.assertRaises(ValueError):
                PROFILE.configure(config, PROFILE.PAYLOAD)
            self.assertEqual(original, config.read_text())

    def test_unexpected_payload_does_not_create_config(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config.toml"
            with self.assertRaises(ValueError):
                PROFILE.configure(config, "[profile.release]\nopt-level = 0\n")
            self.assertFalse(config.exists())

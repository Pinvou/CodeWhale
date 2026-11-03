import importlib.util
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('fixture_guard', ROOT / 'scripts/ci-fixture-guard.py')
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


class FixtureGuardTests(unittest.TestCase):
    def check(self, go='', node=''):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'account-platform').mkdir()
            (root / 'pinvou3-app/tests').mkdir(parents=True)
            (root / 'account-platform/fixture_test.go').write_text(go, encoding='utf-8')
            (root / 'pinvou3-app/tests/probe.test.mjs').write_text(node, encoding='utf-8')
            return guard.violations(root)

    def test_raw_database_cleanup_cannot_reappear(self):
        for sql in ['CREATE DATABASE ', 'DROP DATABASE ']:
            self.assertTrue(self.check(go='admin.Exec(ctx, "'+sql+'"+pgx.Identifier{name}.Sanitize())'))
            self.assertTrue(self.check(go='exec(admin, `'+sql+'`+quoted)'))

    def test_ownership_lease_and_business_queries_are_allowed(self):
        self.assertFalse(self.check(go='cifixture.CreateDatabase(ctx, admin, name, "CREATE DATABASE "+quoted)\ncifixture.DropDatabase(ctx, admin, lease)\nadmin.Exec(ctx, "SELECT 1")'))

    def test_comments_and_error_descriptions_are_not_destructive_operations(self):
        self.assertFalse(self.check(go='// admin.Exec(ctx, "DROP DATABASE x")\nt.Error("DROP DATABASE failed")\n/* exec(admin, `CREATE DATABASE x`) */'))

    def test_native_resource_factories_are_blocked_but_shared_factories_pass(self):
        for package, operation in [('vite', 'createServer'), ('puppeteer-core', 'launch'), ('node:http', 'createServer')]:
            self.assertTrue(self.check(node=f"import {{ {operation} }} from '{package}'; {operation}();"))
        self.assertFalse(self.check(node="import { createServer } from './helpers/vite-fixture.mjs'; createServer();"))
        self.assertFalse(self.check(node="import { build } from 'vite'; build();"))

    def test_commonjs_and_dynamic_imports_cannot_bypass_factory(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'account-platform').mkdir()
            tests = root / 'pinvou3-app/tests'
            tests.mkdir(parents=True)
            for suffix in ['js', 'cjs', 'mjs']:
                path = tests / ('smoke.' + suffix)
                path.write_text("const http = require('http'); http.createServer();")
                self.assertTrue(guard.violations(root))
                path.unlink()
            (tests / 'dynamic.mjs').write_text("const {createServer} = await import('vite'); createServer();")
            self.assertTrue(guard.violations(root))


if __name__ == '__main__':
    unittest.main()

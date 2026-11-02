import importlib.util
import contextlib
import io
import json
import os
from pathlib import Path
import runpy
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("deployment_contracts", ROOT / "scripts/account-platform-deployment-contracts.py")
deployment = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deployment)
verify_results = runpy.run_path(str(ROOT / "scripts/test-account-closure-postgres.py"))["verify_results"]


class DeploymentContracts(unittest.TestCase):
    def image_inspect_result(self, *, cached=True, digests=None):
        if digests is None:
            digests = [deployment.POSTGRES.split(":17-alpine", 1)[0] + "@" + deployment.POSTGRES.split("@", 1)[1]]
        return subprocess.CompletedProcess([], 0 if cached else 1, json.dumps(digests), "")

    def test_pinned_postgres_cache_hit_never_contacts_registry(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "postgres-image.log"
            for reference in deployment.POSTGRES_PROFILES.values():
                digest = "docker.io/library/postgres@" + reference.split("@", 1)[1]
                with patch.object(deployment.subprocess, "run", return_value=self.image_inspect_result(digests=[digest])) as inspect, \
                        patch.object(deployment, "prepare_dependencies") as pull, contextlib.redirect_stdout(io.StringIO()):
                    deployment.ensure_postgres_image(reference, logfile)
                pull.assert_not_called()
                self.assertEqual(inspect.call_count, 1)
                self.assertEqual(inspect.call_args.args[0], ["docker", "image", "inspect", reference, "--format", "{{json .RepoDigests}}"])
                self.assertIn("POSTGRES_IMAGE_CACHE_HIT " + reference, logfile.read_text())

    def test_missing_postgres_downloads_only_pinned_manifest_and_revalidates(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "postgres-image.log"
            with patch.object(deployment.subprocess, "run", side_effect=[self.image_inspect_result(cached=False), self.image_inspect_result()]) as inspect, \
                    patch.object(deployment, "prepare_dependencies") as pull:
                deployment.ensure_postgres_image(deployment.POSTGRES, logfile)
            pull.assert_called_once_with(["docker", "pull", deployment.POSTGRES], logfile)
            self.assertEqual(inspect.call_count, 2)

    def test_wrong_or_malformed_cached_digest_fails_without_mutating_cache(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "postgres-image.log"
            for payload in (["postgres@sha256:" + "0" * 64], [], None, {}, [42], "not json"):
                with self.subTest(payload=payload), \
                        patch.object(deployment.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(payload), "")), \
                        patch.object(deployment, "prepare_dependencies") as pull:
                    with self.assertRaisesRegex(RuntimeError, "cache"):
                        deployment.ensure_postgres_image(deployment.POSTGRES, logfile)
                    pull.assert_not_called()

    def test_postgres_pull_failure_cannot_become_a_cache_or_contract_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(deployment.subprocess, "run", return_value=self.image_inspect_result(cached=False)), \
                    patch.object(deployment, "prepare_dependencies", side_effect=RuntimeError("mandatory contracts did not run")):
                with self.assertRaisesRegex(RuntimeError, "mandatory contracts did not run"):
                    deployment.ensure_postgres_image(deployment.POSTGRES, Path(directory) / "postgres-image.log")

    def test_successful_pull_without_a_usable_image_still_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(deployment.subprocess, "run", return_value=self.image_inspect_result(cached=False)), \
                    patch.object(deployment, "prepare_dependencies") as pull:
                with self.assertRaisesRegex(RuntimeError, "unavailable after preparation"):
                    deployment.ensure_postgres_image(deployment.POSTGRES, Path(directory) / "postgres-image.log")
                pull.assert_called_once()

    def test_postgres_tag_only_reference_is_rejected_before_docker_access(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(deployment.subprocess, "run") as inspect:
            with self.assertRaisesRegex(RuntimeError, "pinned digest"):
                deployment.ensure_postgres_image("postgres:17-alpine", Path(directory) / "postgres-image.log")
            inspect.assert_not_called()

    def run_workflow_cache_fixture(self, fail_allocation=False):
        workflow = (ROOT / ".github/workflows/pr-check.yml").read_text(encoding="utf-8")
        start = workflow.index("      - name: Production history and managed PostgreSQL release contracts\n")
        end = workflow.index("\n      - name:", start + 1)
        script = textwrap.dedent(workflow[start:end].split("        run: |\n", 1)[1])
        # Git for Windows prepends its own bin before the supplied PATH.
        script = 'export PATH="$(cd "$CACHE_TEST_BIN" && pwd -P):$PATH"\n' + script
        bash = os.environ.get("PINVOU_TEST_BASH") or shutil.which("bash")
        self.assertTrue(bash, "Bash is required for the real workflow cache producer")
        with tempfile.TemporaryDirectory(prefix="deployment-cache-contract-") as directory:
            root = Path(directory)
            runner = root / "runner-temp"
            binary = root / "bin"
            protected = root / "existing-cache"
            for path in (runner, binary, protected):
                path.mkdir()
            sentinel = protected / "preserve"
            sentinel.write_text("existing cache must stay unchanged\n")
            events = root / "events"
            consumer = binary / "python3"
            consumer.write_text('''#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == -m ]]; then exit 0; fi
test "$1" = scripts/account-platform-deployment-contracts.py
cache="${PINVOU_DEPLOYMENT_TEST_MODULE_CACHE:?}"
test "$cache" != "$PROTECTED_CACHE"
test -d "$cache"
printf '%s\\t%s\\n' "$3" "$cache" >> "$CACHE_EVENTS"
if [[ "$3" == 18.4 ]]; then test -f "$cache/profile-17"; fi
printf 'owned consumer marker\\n' > "$cache/profile-$3"
''')
            consumer.chmod(0o700)
            if fail_allocation:
                allocator = binary / "mktemp"
                allocator.write_text("#!/usr/bin/env bash\nexit 73\n")
                allocator.chmod(0o700)
            environment = {**os.environ, "PATH": binary.as_posix() + os.pathsep + os.environ["PATH"],
                           "RUNNER_TEMP": runner.as_posix(), "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1",
                           "PINVOU_DEPLOYMENT_TEST_MODULE_CACHE": protected.as_posix(),
                           "PROTECTED_CACHE": protected.as_posix(), "CACHE_EVENTS": events.as_posix(),
                           "CACHE_TEST_BIN": binary.as_posix()}
            captures = []
            for _ in range(1 if fail_allocation else 2):
                result = subprocess.run([bash, "-c", script], cwd=ROOT, env=environment,
                                        text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)
                self.assertEqual(result.returncode, 73 if fail_allocation else 0, result.stdout)
            if events.exists():
                for line in events.read_text().splitlines():
                    version, name = line.split("\t")
                    if os.name == "nt" and name.startswith("/") and name[2:3] == "/":
                        name = name[1].upper() + ":/" + name[3:]
                    path = Path(name)
                    self.assertEqual(path.resolve().parent, runner.resolve())
                    self.assertFalse(path.is_symlink())
                    if os.name != "nt":
                        self.assertEqual(path.stat().st_mode & 0o777, 0o700)
                        self.assertEqual(path.stat().st_uid, os.getuid())
                    captures.append((version, str(path.resolve())))
            self.assertEqual(sentinel.read_text(), "existing cache must stay unchanged\n")
            self.assertEqual(list(protected.iterdir()), [sentinel])
            return captures

    def test_workflow_uses_fresh_cache_per_step_and_reuses_it_only_for_two_postgres_profiles(self):
        captures = self.run_workflow_cache_fixture()
        self.assertEqual([version for version, _ in captures], ["17", "18.4", "17", "18.4"])
        self.assertEqual(captures[0][1], captures[1][1])
        self.assertEqual(captures[2][1], captures[3][1])
        self.assertNotEqual(captures[0][1], captures[2][1])

    def test_workflow_cache_allocation_failure_stops_before_contracts(self):
        self.assertEqual(self.run_workflow_cache_fixture(fail_allocation=True), [])

    def test_dependency_failure_retains_exit_and_private_log_with_bounded_redacted_diagnostics(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "dependencies.log"
            errors = io.StringIO()
            secret = "fixture-private-token-not-public"
            program = "import sys; print('x'*70000); print('registry timeout https://user:password@example.invalid/package?token=private'); print('Authorization: Bearer fixture-header-secret'); print('npm _authToken=fixture-config-secret'); print('env '+sys.argv[1]); sys.exit(7)"
            with patch.dict(os.environ, {"NPM_TOKEN": secret}), contextlib.redirect_stderr(errors):
                with self.assertRaisesRegex(RuntimeError, "mandatory contracts did not run") as raised:
                    deployment.prepare_dependencies([sys.executable, "-c", program, secret], logfile)
            emitted = errors.getvalue()
            self.assertIn("DEPENDENCY_PREPARATION_FAILED exit=7 log=dependencies.log", emitted)
            self.assertIn("registry timeout", emitted)
            self.assertLessEqual(len(emitted), 24200)
            for value in (secret, "user:password", "token=private", "fixture-header-secret", "fixture-config-secret"):
                self.assertNotIn(value, emitted)
                self.assertNotIn(value, str(raised.exception))
            self.assertIn(secret, logfile.read_text())
            self.assertGreater(logfile.stat().st_size, 65536)

    def test_successful_dependency_preparation_is_not_a_contract_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "dependencies.log"
            errors = io.StringIO()
            with contextlib.redirect_stderr(errors):
                deployment.prepare_dependencies([sys.executable, "-c", "print('dependencies ready')"], logfile)
            self.assertEqual(errors.getvalue(), "")
            self.assertEqual(logfile.read_text().strip(), "dependencies ready")

    def test_truncated_url_cannot_expose_query_credentials_after_its_prefix_is_lost(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = "controlled-fixture-query-credential"
            payload = root / "payload.txt"
            payload.write_text("https://example.invalid/" + "x" * 70000 + "?auth=" + value + "\n")
            script = root / "emit.py"
            script.write_text("from pathlib import Path\nimport sys\nsys.stdout.write(Path(sys.argv[1]).read_text())\nsys.exit(7)\n")
            logfile = root / "dependencies.log"
            errors = io.StringIO()
            with contextlib.redirect_stderr(errors):
                with self.assertRaisesRegex(RuntimeError, "mandatory contracts did not run"):
                    deployment.prepare_dependencies([sys.executable, str(script), str(payload)], logfile)
            self.assertNotIn(value, errors.getvalue())
            self.assertIn("DEPENDENCY_PREPARATION_FAILED exit=7", errors.getvalue())
            self.assertIn(value, logfile.read_text())

    def test_published_fixture_is_complete_and_uses_frozen_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory)
            deployment.published_history(ROOT, target)
            self.assertEqual(len(list((target / "account").glob("*.up.sql"))), 87)
            self.assertEqual(len(list((target / "token").glob("*.up.sql"))), 34)
            self.assertNotIn(b"publication_generation", (target / "account/000003_event_delivery.up.sql").read_bytes())
            self.assertIn(b"event_replay_requests_immutable", (target / "token/000018_event_replay_audit.up.sql").read_bytes())

    def test_required_root_cannot_hide_skipped_child_or_incomplete_stream(self):
        def events(*values):
            return "\n".join(json.dumps(value) for value in values)
        start = {"Action": "start", "Package": "fixture/package"}
        parent = {"Action": "pass", "Package": "fixture/package", "Test": "Required"}
        end = {"Action": "pass", "Package": "fixture/package"}
        for raw in (
            events(start, parent, {"Action": "skip", "Package": "fixture/package", "Test": "Required/actual-database"}, end),
            events(start, parent),
            events(start, {"Action": "pass", "Package": "fixture/package", "Test": "Unrelated"}, end),
        ):
            with self.assertRaises(RuntimeError):
                verify_results(raw, {"Required"})

    def test_published_history_rejects_missing_or_escaping_entries(self):
        original = json.loads((ROOT / "account-platform/test/fixtures/production-history-manifest.json").read_text())
        for mutate in (lambda value: value["domains"]["account"].pop(),
                       lambda value: value["domains"]["account"][0].update(file="../outside.up.sql")):
            with tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                path = root / "account-platform/test/fixtures/production-history-manifest.json"
                path.parent.mkdir(parents=True)
                value = json.loads(json.dumps(original))
                mutate(value)
                path.write_text(json.dumps(value))
                with self.assertRaises(RuntimeError):
                    deployment.published_history(root, root / "legacy")


if __name__ == "__main__":
    unittest.main()

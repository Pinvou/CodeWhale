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
    def test_backend_exit_regression_is_a_required_managed_contract_root(self):
        required = deployment.MANAGED_CONTRACT_ROOTS
        regression = "TestManagedFixtureDatabaseCleanupPostgres"
        self.assertIn(regression, required)
        def stream(names):
            rows = [{"Action": "start", "Package": "fixture/package"}]
            for name in sorted(names):
                rows += [{"Action": "run", "Package": "fixture/package", "Test": name},
                         {"Action": "pass", "Package": "fixture/package", "Test": name}]
            rows.append({"Action": "pass", "Package": "fixture/package"})
            return "\n".join(json.dumps(row) for row in rows)
        with self.assertRaises(RuntimeError):
            verify_results(stream(required - {regression}), required)
        verify_results(stream(required), required)

    def test_actions_warms_runner_cache_in_isolated_source_before_offline_consumption(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "source"
            source.mkdir()
            logfile = root / "download.log"
            with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "GOMODCACHE": str(root / "mod")}), \
                    patch.object(deployment.shutil, "which", return_value="/tools/go"), \
                    patch.object(deployment, "prepare_dependencies") as prepare:
                self.assertTrue(deployment.warm_runner_go_cache(source, logfile))
            prepare.assert_called_once_with(["/tools/go", "mod", "download"], logfile, cwd=source)

    def test_standalone_docker_contracts_do_not_require_host_go(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for environment, go in [({"GITHUB_ACTIONS": "false", "GOMODCACHE": str(root / "mod")}, "/tools/go"),
                                    ({"GITHUB_ACTIONS": "true", "GOMODCACHE": ""}, "/tools/go"),
                                    ({"GITHUB_ACTIONS": "true", "GOMODCACHE": str(root / "mod")}, None)]:
                with self.subTest(environment=environment, go=go), patch.dict(os.environ, environment), \
                        patch.object(deployment.shutil, "which", return_value=go), \
                        patch.object(deployment, "prepare_dependencies") as prepare:
                    self.assertFalse(deployment.warm_runner_go_cache(root, root / "download.log"))
                    prepare.assert_not_called()

    def test_host_cache_download_failure_stops_before_contracts(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "GOMODCACHE": str(Path(directory) / "mod")}), \
                    patch.object(deployment.shutil, "which", return_value="/tools/go"), \
                    patch.object(deployment, "prepare_dependencies", side_effect=RuntimeError("mandatory contracts did not run")):
                with self.assertRaisesRegex(RuntimeError, "mandatory contracts did not run"):
                    deployment.warm_runner_go_cache(Path(directory), Path(directory) / "download.log")

    def test_runner_go_archive_cache_is_readonly_and_preserves_checksum_policy(self):
        with tempfile.TemporaryDirectory() as directory:
            module = Path(directory) / "mod"
            archive = module / "cache/download"
            archive.mkdir(parents=True)
            sentinel = archive / "preserve"
            sentinel.write_text("archive bytes\n")
            policy = {"GOMODCACHE": str(module), "GOPROXY": "https://example.invalid/proxy|direct",
                      "GOSUMDB": "sum.golang.org", "GOPRIVATE": "private.example.invalid/*",
                      "GONOSUMDB": "private.example.invalid/*", "GONOPROXY": "private.example.invalid/*"}
            mounts, values = deployment.dependency_cache_options(policy)
            self.assertEqual(mounts, ["--mount", f"type=bind,src={archive.resolve()},dst=/runner-download-cache-0,readonly"])
            self.assertIn("GOPROXY=file:///runner-download-cache-0,https://example.invalid/proxy|direct", values)
            for key in ["GOSUMDB", "GOPRIVATE", "GONOSUMDB", "GONOPROXY"]:
                self.assertIn(key + "=" + policy[key], values)
            self.assertFalse(any(x.startswith("GOMODCACHE=") for x in values))
            self.assertEqual(sentinel.read_text(), "archive bytes\n")

    def test_file_proxy_is_rebased_into_container_and_separators_are_preserved(self):
        with tempfile.TemporaryDirectory(prefix="deployment file proxy ") as directory:
            path = Path(directory)
            mounts, values = deployment.dependency_cache_options({"GOPROXY": path.as_uri() + "|https://example.invalid/proxy,direct"})
            self.assertEqual(mounts, ["--mount", f"type=bind,src={path.resolve()},dst=/runner-download-cache-0,readonly"])
            self.assertEqual(values, ["GOPROXY=file:///runner-download-cache-0|https://example.invalid/proxy,direct"])

    def test_duplicate_go_archive_source_is_mounted_once(self):
        with tempfile.TemporaryDirectory() as directory:
            module = Path(directory)
            path = module / "cache/download"
            path.mkdir(parents=True)
            mounts, _ = deployment.dependency_cache_options({"GOMODCACHE": str(module), "GOPROXY": path.as_uri() + ",direct"})
            self.assertEqual(mounts.count("--mount"), 1)

    def test_missing_optional_cache_keeps_the_configured_fallback(self):
        with tempfile.TemporaryDirectory() as directory:
            missing = Path(directory) / "missing"
            proxy = missing.as_uri() + ",https://example.invalid/proxy,direct"
            mounts, values = deployment.dependency_cache_options({"GOMODCACHE": str(missing), "GOPROXY": proxy, "NPM_CONFIG_CACHE": str(missing)})
            self.assertEqual(mounts, [])
            self.assertEqual(values, ["GOPROXY=" + proxy])

    def test_proxy_off_is_not_overridden_by_available_archive_cache(self):
        with tempfile.TemporaryDirectory() as directory:
            (Path(directory) / "cache/download").mkdir(parents=True)
            mounts, values = deployment.dependency_cache_options({"GOMODCACHE": directory, "GOPROXY": "off"})
            self.assertEqual((mounts, values), ([], ["GOPROXY=off"]))

    def test_npm_reuses_configured_content_addressed_download_cache(self):
        with tempfile.TemporaryDirectory() as directory:
            mounts, values = deployment.dependency_cache_options({"NPM_CONFIG_CACHE": directory})
            self.assertEqual(mounts, ["--mount", f"type=bind,src={Path(directory).resolve()},dst=/runner-download-cache-0"])
            self.assertIn("NPM_CONFIG_CACHE=/runner-download-cache-0", values)
            self.assertIn("NPM_CONFIG_PREFER_OFFLINE=true", values)

    def test_unsafe_or_nonlocal_cache_paths_are_rejected(self):
        for environment in ({"GOPROXY": "file://foreign.example.invalid/cache,direct"},
                            {"GOPROXY": "file:///cache?token=private,direct"},
                            {"NPM_CONFIG_CACHE": "relative/cache"},
                            {"NPM_CONFIG_CACHE": str(ROOT.anchor)},
                            {"NPM_CONFIG_CACHE": str(ROOT / "unsafe,path")}):
            with self.subTest(environment=environment), self.assertRaises(RuntimeError):
                deployment.dependency_cache_options(environment)

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

    def test_contract_failure_exposes_bounded_redacted_evidence_without_argv(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "managed.jsonl"
            secret = "fixture-contract-private-password"
            program = "import sys; print('x'*70000); print('managed assertion failed'); print('postgres://fixture:private@localhost/db?password=private'); print('env '+sys.argv[1]); sys.exit(9)"
            errors = io.StringIO()
            with patch.dict(os.environ, {"DATABASE_PASSWORD": secret}), contextlib.redirect_stderr(errors):
                with self.assertRaisesRegex(RuntimeError, "deployment contract command failed") as raised:
                    deployment.run_logged_command([sys.executable, "-c", program, secret], logfile)
            emitted = errors.getvalue()
            self.assertIn("CONTRACT_COMMAND_FAILED exit=9 log=managed.jsonl", emitted)
            self.assertIn("managed assertion failed", emitted)
            self.assertLessEqual(len(emitted), 24200)
            for value in (secret, "fixture:private", "password=private"):
                self.assertNotIn(value, emitted)
                self.assertNotIn(value, str(raised.exception))
            self.assertIn(secret, logfile.read_text())

    def test_contract_success_preserves_raw_go_json_for_mandatory_verification(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "managed.jsonl"
            errors = io.StringIO()
            payload = json.dumps({"Action": "fail", "Test": "Required", "Package": "fixture"})
            with contextlib.redirect_stderr(errors):
                deployment.run_logged_command([sys.executable, "-c", "import sys; print(sys.argv[1])", payload], logfile)
            self.assertEqual(errors.getvalue(), "")
            self.assertEqual(logfile.read_text().strip(), payload)
            with self.assertRaises(RuntimeError):
                verify_results(logfile.read_text(), {"Required"})

    def test_early_go_failure_survives_later_passing_output_without_exposing_secrets(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            logfile, payload = root / "managed.jsonl", root / "input.jsonl"
            secret = "fixture-first-assertion-credential"
            rows = [
                {"Action": "output", "Package": "fixture/managed", "Test": "TestRequired/early",
                 "Output": "fixture.go:23: controlled first assertion " + secret + " postgres://fixture:private@localhost/db\n"},
                {"Action": "fail", "Package": "fixture/managed", "Test": "TestRequired/early"},
            ]
            rows += [{"Action": "output", "Package": "fixture/managed", "Test": "TestLater",
                      "Output": "later passing output " + "x" * 1000} for _ in range(160)]
            rows += [{"Action": "pass", "Package": "fixture/managed", "Test": "TestLater"},
                     {"Action": "fail", "Package": "fixture/managed"}]
            raw = "\n".join(json.dumps(row) for row in rows) + "\n"
            payload.write_bytes(raw.encode("utf-8"))
            program = "from pathlib import Path; import sys; sys.stdout.buffer.write(Path(sys.argv[1]).read_bytes()); sys.exit(9)"
            errors = io.StringIO()
            with patch.dict(os.environ, {"DATABASE_PASSWORD": secret}), contextlib.redirect_stderr(errors):
                with self.assertRaisesRegex(RuntimeError, "deployment contract command failed"):
                    deployment.run_logged_command([sys.executable, "-c", program, str(payload)], logfile)
            emitted = errors.getvalue()
            summaries = [json.loads(line.removeprefix("GO_TEST_FAILURE ")) for line in emitted.splitlines()
                         if line.startswith("GO_TEST_FAILURE ")]
            self.assertEqual(len(summaries), 1)
            self.assertEqual(summaries[0]["Test"], "TestRequired/early")
            self.assertIn("fixture.go:23: controlled first assertion", summaries[0]["Output"])
            for value in (secret, "fixture:private", "postgres://"):
                self.assertNotIn(value, emitted)
            self.assertIn("CONTRACT_COMMAND_FAILED exit=9", emitted)
            self.assertLessEqual(len(emitted), 41000)
            self.assertEqual(logfile.read_bytes(), raw.encode())

    def test_go_failure_summary_is_bounded_and_does_not_parse_partial_oversized_lines(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "managed.jsonl"
            malformed = '{"Action":"fail","Package":"fixture","Test":"NotACompleteEvent","x":"' + "x" * 100000
            rows = [json.dumps({"Action": "output", "Package": "fixture", "Test": "Test" + str(i),
                                "Output": "controlled failure " + "界" * 10000}) + "\n" +
                    json.dumps({"Action": "fail", "Package": "fixture", "Test": "Test" + str(i)})
                    for i in range(20)]
            logfile.write_text(malformed + "\n" + "\n".join(rows), encoding="utf-8")
            summary = deployment.first_go_failures(logfile)
            self.assertNotIn("NotACompleteEvent", summary)
            self.assertIn('"Test": "Test0"', summary)
            self.assertLessEqual(len(summary.encode("ascii")), 16384)
            self.assertLessEqual(len(summary.splitlines()), 5)

    def test_go_json_escaped_environment_secret_is_redacted_in_summary_and_tail(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            secret = 'fixture-quoted-"credential\\<&>-界\u2028\u2029'
            def go_escape(value):
                return (value.replace("<", r"\u003c").replace(">", r"\u003e").replace("&", r"\u0026")
                        .replace("\u2028", r"\u2028").replace("\u2029", r"\u2029"))
            for spelling in ("python-unicode", "python-ascii", "go-html", "nested-go-html"):
                with self.subTest(spelling=spelling):
                    logfile, payload = root / "managed.jsonl", root / "input.jsonl"
                    observation = go_escape(json.dumps({"value": secret}, ensure_ascii=False)) if spelling == "nested-go-html" else secret
                    rows = [{"Action": "output", "Package": "fixture", "Test": "TestRequired",
                             "Output": "controlled credential observation " + observation},
                            {"Action": "fail", "Package": "fixture", "Test": "TestRequired"}]
                    raw = "\n".join(json.dumps(row, ensure_ascii=spelling == "python-ascii") for row in rows) + "\n"
                    if spelling in ("go-html", "nested-go-html"):
                        raw = go_escape(raw)
                    payload.write_bytes(raw.encode("utf-8"))
                    program = "from pathlib import Path; import sys; sys.stdout.buffer.write(Path(sys.argv[1]).read_bytes()); sys.exit(3)"
                    errors = io.StringIO()
                    with patch.dict(os.environ, {"FIXTURE_TOKEN": secret}), contextlib.redirect_stderr(errors):
                        with self.assertRaises(RuntimeError):
                            deployment.run_logged_command([sys.executable, "-c", program, str(payload)], logfile)
                    emitted = errors.getvalue()
                    for form in (secret, json.dumps(secret, ensure_ascii=spelling == "python-ascii")[1:-1], "fixture-quoted-"):
                        self.assertNotIn(form, emitted)
                    self.assertIn("controlled credential observation", emitted)
                    self.assertIn("[redacted]", emitted)
                    self.assertEqual(logfile.read_bytes(), raw.encode("utf-8"))

    def test_go_failure_context_keeps_assertion_after_verbose_test_output(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "managed.jsonl"
            rows = [{"Action": "output", "Package": "fixture", "Test": "TestRequired", "Output": output}
                    for output in ("ordinary verbose output " + "x" * 1600,
                                   "fixture.go:42: controlled first assertion\n", "--- FAIL: TestRequired\n")]
            rows.append({"Action": "fail", "Package": "fixture", "Test": "TestRequired"})
            logfile.write_text("\n".join(json.dumps(row) for row in rows), encoding="utf-8")
            summary = json.loads(deployment.first_go_failures(logfile).removeprefix("GO_TEST_FAILURE "))
            self.assertIn("fixture.go:42: controlled first assertion", summary["Output"])

    def test_failure_context_does_not_reassemble_a_secret_from_go_output_chunks(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            logfile, payload = root / "managed.jsonl", root / "input.jsonl"
            secret = "fixture-split-credential-value"
            rows = [{"Action": "output", "Package": "fixture", "Test": "TestRequired", "Output": part}
                    for part in (secret[:13], secret[13:])]
            rows.insert(1, {"Action": "output", "Package": "fixture", "Test": "TestParallel", "Output": "unrelated output"})
            rows.append({"Action": "fail", "Package": "fixture", "Test": "TestRequired"})
            raw = "\n".join(json.dumps(row) for row in rows) + "\n"
            payload.write_bytes(raw.encode("utf-8"))
            program = "from pathlib import Path; import sys; sys.stdout.buffer.write(Path(sys.argv[1]).read_bytes()); sys.exit(3)"
            errors = io.StringIO()
            with patch.dict(os.environ, {"FIXTURE_TOKEN": secret}), contextlib.redirect_stderr(errors):
                with self.assertRaises(RuntimeError):
                    deployment.run_logged_command([sys.executable, "-c", program, str(payload)], logfile)
                summary = json.loads(deployment.first_go_failures(logfile).removeprefix("GO_TEST_FAILURE "))
            self.assertEqual(summary["Output"], "[redacted]")
            outputs = [json.loads(line)["Output"] for line in errors.getvalue().splitlines()
                       if line.startswith("{") and json.loads(line).get("Action") == "output"]
            self.assertEqual(outputs, ["[redacted]", "unrelated output"])
            self.assertEqual(logfile.read_bytes(), raw.encode("utf-8"))

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


    def failure_diagnostic(self, payload, *, environment=None, name="managed.jsonl"):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / name
            errors = io.StringIO()
            payload_file = Path(directory) / "payload.txt"
            payload_file.write_bytes(payload.encode("utf-8"))
            program = "from pathlib import Path; import sys; sys.stdout.write(Path(sys.argv[1]).read_text(encoding='utf-8')); sys.exit(9)"
            with patch.dict(os.environ, environment or {}), contextlib.redirect_stderr(errors):
                with self.assertRaisesRegex(RuntimeError, "deployment contract command failed") as raised:
                    deployment.run_logged_command([sys.executable, "-X", "utf8", "-c", program, str(payload_file)], logfile)
            self.assertEqual(logfile.read_text(), payload)
            emitted = errors.getvalue()
            self.assertIn("CONTRACT_COMMAND_FAILED exit=9 log=" + name, emitted)
            # Approved independent summary + tail channels, not the old
            # whole-stderr 24k contract. Limits are characters, not UTF-8 bytes.
            self.assertLessEqual(len(emitted), 41000)
            self.assertIn("CONTRACT_LOG_TAIL\n", emitted)
            tail = emitted.split("CONTRACT_LOG_TAIL\n", 1)[1]
            self.assertLessEqual(len(tail), 24001)  # print adds one newline
            self.assertLessEqual(len(tail.splitlines()), 100)
            return emitted, str(raised.exception)

    def managed_event(self, name, action, output=None, **fields):
        event = {"Package": "github.com/Pinvou/pinvou3/account-platform/cmd/token-release-prepare",
                 "Test": name, "Action": action, **fields}
        if output is not None:
            event["Output"] = output
        return json.dumps(event) + "\n"

    def test_managed_failure_context_preserves_early_fail_over_later_pass(self):
        target = "TestManagedRDSReleaseActualPostgres/early-failure"
        payload = self.managed_event(target, "output", "controlled-early-assertion\n") + self.managed_event(target, "fail")
        payload += "".join(self.managed_event("TestManagedRDSReleaseProductionHistoryPostgres/later", "output", "later PASS output\n") for _ in range(140))
        emitted, _ = self.failure_diagnostic(payload)
        self.assertIn("MANAGED_FAILURE_CONTEXT", emitted)
        self.assertIn("controlled-early-assertion", emitted)

    def test_managed_context_is_same_test_before_its_failure_only(self):
        target = "TestManagedRDSReleaseActualPostgres/exact-child"
        other = "TestManagedRDSReleaseActualPostgres/later-child"
        payload = self.managed_event(target, "output", "exact child failure\n")
        payload += self.managed_event(other, "output", "unrelated child output\n")
        payload += self.managed_event(target, "fail")
        payload += self.managed_event(target, "output", "after failure output\n")
        emitted, _ = self.failure_diagnostic(payload)
        context = emitted.split("MANAGED_FAILURE_CONTEXT", 1)[1]
        self.assertIn("exact child failure", context)
        self.assertNotIn("unrelated child output", context)
        self.assertNotIn("after failure output", context)

    def test_selected_context_redacts_urls_auth_keys_and_go_escaped_env(self):
        target = "TestManagedRDSReleaseActualPostgres/private-context"
        # Literal Go encoding/json form: UTF-8 text, mandatory line-separator
        # escapes, HTML escapes, quote/backslash and a control character.
        secret = 'fixture-中\u2028\u2029"<>&\\\nend'
        go_line = ('{"Package":"github.com/Pinvou/pinvou3/account-platform/cmd/token-release-prepare",'
                   '"Test":"' + target + '","Action":"output","Output":"env fixture-中'
                   r'\u2028\u2029\"\u003c\u003e\u0026\\\nend\n"}' + "\n")
        self.assertEqual(json.loads(go_line)["Output"], "env " + secret + "\n")
        payload = self.managed_event(target, "output", "https://user:fixture-url-password@example.invalid/?token=fixture-query-secret\n")
        payload += self.managed_event(target, "output", "Authorization: Bearer fixture-header-secret\n")
        payload += self.managed_event(target, "output", "npm _authToken=fixture-config-secret\n") + go_line
        payload += self.managed_event(target, "fail")
        payload += "".join(self.managed_event("TestManagedRDSReleaseProductionHistoryPostgres/later", "output", "later PASS\n") for _ in range(130))
        emitted, error = self.failure_diagnostic(payload, environment={"DATABASE_PASSWORD": secret})
        context = emitted.split("MANAGED_FAILURE_CONTEXT", 1)[1]
        self.assertIn("[redacted]", context)
        for private in [secret, r'fixture-中\u2028\u2029\"\u003c\u003e\u0026\\\nend',
                        "fixture-url-password", "fixture-query-secret", "fixture-header-secret", "fixture-config-secret"]:
            self.assertNotIn(private, emitted)
            self.assertNotIn(private, error)

    def test_raw_tail_redacts_json_env_strings_with_control_and_non_ascii_values(self):
        secret = 'fixture-中"\\\n\u2028\u2029<>&-private'
        for ensure_ascii in [True, False]:
            with self.subTest(ensure_ascii=ensure_ascii):
                payload = json.dumps({"Output": "env " + secret}, ensure_ascii=ensure_ascii) + "\n"
                emitted, _ = self.failure_diagnostic(payload, environment={"DATABASE_PASSWORD": secret})
                self.assertNotIn(secret, emitted)
                self.assertNotIn(json.dumps(secret, ensure_ascii=ensure_ascii)[1:-1], emitted)
                self.assertIn("[redacted]", emitted)
                self.assertNotIn("MANAGED_FAILURE_CONTEXT", emitted)

    def test_malformed_non_map_wrong_package_and_lookalike_roots_keep_tail(self):
        root = "TestManagedRDSReleaseActualPostgres"
        payloads = ["not-json\n", "[]\n", "null\n", '"string"\n',
                    self.managed_event(root + "-lookalike", "fail"),
                    self.managed_event(root, "fail", Package="unrelated/package"),
                    self.managed_event(root, "fail", Test={"invalid": True}),
                    self.managed_event(root, "fail", Output=17)]
        for payload in payloads:
            with self.subTest(payload=payload):
                emitted, _ = self.failure_diagnostic(payload)
                self.assertNotIn("MANAGED_FAILURE_CONTEXT", emitted)
                self.assertIn(payload.strip(), emitted)

    def test_failure_outside_the_original_window_is_not_claimed_recovered(self):
        target = "TestManagedRDSReleaseActualPostgres/outside-window"
        payload = self.managed_event(target, "output", "outside-window-assertion\n") + self.managed_event(target, "fail")
        payload += "x" * 70000 + "\nordinary final tail\n"
        emitted, _ = self.failure_diagnostic(payload)
        self.assertNotIn("MANAGED_FAILURE_CONTEXT", emitted)
        # The bounded-window selector does not recover this event. The
        # independent main full-log summary intentionally can report it.
        tail = emitted.split("CONTRACT_LOG_TAIL\n", 1)[1]
        self.assertNotIn("outside-window-assertion", tail)
        self.assertIn("outside-window-assertion", emitted.split("CONTRACT_LOG_TAIL\n", 1)[0])
        self.assertIn("ordinary final tail", tail)

    def test_partial_first_event_is_discarded_before_context_and_redaction(self):
        target = "TestManagedRDSReleaseActualPostgres/partial"
        payload = self.managed_event(target, "output", "https://example.invalid/" + "x" * 70000 + "?auth=fixture-private-query\n")
        payload += self.managed_event(target, "fail")
        emitted, _ = self.failure_diagnostic(payload)
        self.assertIn("MANAGED_FAILURE_CONTEXT", emitted)
        self.assertNotIn("fixture-private-query", emitted)

    def test_many_managed_failures_and_nonmanaged_logs_stay_bounded(self):
        payload = "".join(self.managed_event("TestManagedRDSReleaseActualPostgres/case-" + str(i), "output", "controlled error\n") +
                          self.managed_event("TestManagedRDSReleaseActualPostgres/case-" + str(i), "fail") for i in range(100))
        emitted, _ = self.failure_diagnostic(payload)
        context = emitted.split("MANAGED_FAILURE_CONTEXT", 1)[1]
        self.assertLessEqual(sum(json.loads(line).get("Action") == "fail" for line in context.splitlines() if line.startswith("{")), 8)
        emitted, _ = self.failure_diagnostic(payload, name="history.jsonl")
        self.assertNotIn("MANAGED_FAILURE_CONTEXT", emitted)


    def test_overlapping_environment_credentials_are_redacted_longest_first(self):
        short, long = "overlap-private", "overlap-private-confidential-suffix"
        target = "TestManagedRDSReleaseActualPostgres/overlapping-env"
        payload = self.managed_event(target, "output", "env " + long + "\n") + self.managed_event(target, "fail")
        payload += "".join(self.managed_event("TestManagedRDSReleaseProductionHistoryPostgres/later", "output", "later PASS\n") for _ in range(130))
        emitted, _ = self.failure_diagnostic(payload, environment={"FIRST_TOKEN": short, "SECOND_PASSWORD": long})
        context = emitted.split("MANAGED_FAILURE_CONTEXT", 1)[1]
        self.assertIn("[redacted]", context)
        self.assertNotIn("confidential-suffix", emitted)
        self.assertNotIn(short, emitted)


    def test_split_overlapping_environment_credentials_are_masked_before_summary_truncation(self):
        target = "TestManagedRDSReleaseActualPostgres/split-overlap"
        payload = self.managed_event(target, "output", "abcd")
        payload += self.managed_event(target, "output", "TAIL")
        payload += self.managed_event(target, "fail")
        emitted, _ = self.failure_diagnostic(payload, environment={"FIXTURE_TOKEN": "abcd", "FIXTURE_SECRET": "abcdTAIL"})
        summaries = [json.loads(line.removeprefix("GO_TEST_FAILURE ")) for line in emitted.splitlines()
                     if line.startswith("GO_TEST_FAILURE ")]
        self.assertEqual(summaries[0]["Output"], "[redacted]")
        self.assertNotIn("TAIL", emitted.replace("CONTRACT_LOG_TAIL", ""))
        context = emitted.split("MANAGED_FAILURE_CONTEXT", 1)[1]
        self.assertIn("[redacted]", context)

    def test_summary_redacts_complete_url_before_limiting_output(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "managed.jsonl"
            target = "TestManagedRDSReleaseActualPostgres/long-url"
            payload = self.managed_event(target, "output", "https://example.invalid/" + "x" * 5000 + "?auth=private-query-suffix")
            payload += self.managed_event(target, "output", "\nfixture.go:42: controlled assertion\n")
            payload += self.managed_event(target, "fail")
            logfile.write_bytes(payload.encode("utf-8"))
            summary = deployment.first_go_failures(logfile)
            self.assertNotIn("private-query-suffix", summary)
            self.assertNotIn("https://", summary)
            self.assertIn("controlled assertion", summary)
            self.assertIn("[URL redacted]", summary)
            self.assertEqual(logfile.read_bytes(), payload.encode("utf-8"))

    def test_summary_and_selected_context_keep_last_four_outputs(self):
        target = "TestManagedRDSReleaseActualPostgres/last-four"
        payload = "".join(self.managed_event(target, "output", "controlled-part-" + str(i) + "\n") for i in range(5))
        payload += self.managed_event(target, "fail")
        emitted, _ = self.failure_diagnostic(payload)
        summary = next(json.loads(line.removeprefix("GO_TEST_FAILURE ")) for line in emitted.splitlines()
                       if line.startswith("GO_TEST_FAILURE "))
        self.assertNotIn("controlled-part-0", summary["Output"])
        context = emitted.split("MANAGED_FAILURE_CONTEXT", 1)[1]
        self.assertNotIn("controlled-part-0", context)
        for i in range(1, 5):
            self.assertIn("controlled-part-" + str(i), summary["Output"])
            self.assertIn("controlled-part-" + str(i), context)

    def test_deep_json_does_not_replace_primary_command_failure(self):
        payload = "[" * 2000 + "0" + "]" * 2000 + "\n"
        emitted, error = self.failure_diagnostic(payload)
        self.assertIn("CONTRACT_COMMAND_FAILED exit=9", emitted)
        self.assertEqual(error, "deployment contract command failed; see redacted diagnostic")
        self.assertNotIn("GO_TEST_FAILURE", emitted)



if __name__ == "__main__":
    unittest.main()

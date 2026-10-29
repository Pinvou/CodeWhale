import importlib.util
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("ci_rust_disk", ROOT / "scripts/ci-rust-disk.py")
DISK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DISK)


class AllowlistTierDefaultsTests(unittest.TestCase):
    """Pin the unpatched module defaults so the two tiers stay explicit."""

    def test_default_allowlist_tiers_are_pinned(self):
        self.assertEqual(
            DISK.BASE_SDK_PATHS,
            (Path("/usr/local/lib/android"), Path("/usr/share/dotnet"), Path("/usr/local/.ghcup")),
        )
        self.assertEqual(DISK.GLOB_SDK_DIRS, ((Path("/usr/local"), "julia*"),))
        self.assertEqual(
            DISK.AGGRESSIVE_SDK_PATHS,
            (Path("/opt/hostedtoolcache"), Path("/opt/google/chrome"), Path("/opt/microsoft/msedge")),
        )
        self.assertEqual(DISK.DEFAULT_MIN_FREE_GIB, 12)


class RustDiskTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.workspace = self.root / "checkout"
        workflow = self.workspace / ".github/workflows/pr-check.yml"
        workflow.parent.mkdir(parents=True)
        workflow.touch()
        self.sdk = self.root / "sdk"
        self.sdk.mkdir()
        (self.sdk / "unused.bin").write_bytes(b"sdk")
        self.keep = self.workspace / "target/dependency.rlib"
        self.keep.parent.mkdir()
        self.keep.write_bytes(b"keep")
        self.enterContext(patch.object(DISK.sys, "platform", "linux"))
        self.enterContext(patch.dict(os.environ, {"RUNNER_ENVIRONMENT": "github-hosted"}))
        self.enterContext(patch.object(DISK, "BASE_SDK_PATHS", (self.sdk, self.root / "missing-sdk")))
        # Keep the glob scan and the aggressive tier pointed at the temp root so
        # tests never touch the real /usr/local or /opt allowlist entries.
        self.enterContext(patch.object(DISK, "GLOB_SDK_DIRS", ()))
        self.enterContext(patch.object(DISK, "AGGRESSIVE_SDK_PATHS", (self.root / "aggressive-sdk",)))
        self.enterContext(patch.object(DISK.shutil, "disk_usage", return_value=SimpleNamespace(free=DISK.MIN_FREE_BYTES)))

    def test_only_allowlisted_sdks_are_removed_and_missing_paths_are_ok(self):
        DISK.prepare_disk(self.workspace)
        self.assertFalse(self.sdk.exists())
        self.assertEqual(self.keep.read_bytes(), b"keep")
        # Idempotent: absent SDKs must not fail a repeated preparation.
        DISK.prepare_disk(self.workspace)

    def test_base_tier_resolves_ghcup_and_julia_glob(self):
        ghcup = self.root / ".ghcup"
        ghcup.mkdir()
        julia = self.root / "julia1.12.7"
        julia.mkdir()
        unrelated = self.root / "not-julia"
        unrelated.mkdir()
        with patch.object(DISK, "BASE_SDK_PATHS", (self.sdk, ghcup)), patch.object(
            DISK, "GLOB_SDK_DIRS", ((self.root, "julia*"),)
        ):
            DISK.prepare_disk(self.workspace)
        self.assertFalse(self.sdk.exists())
        self.assertFalse(ghcup.exists())
        self.assertFalse(julia.exists())
        self.assertTrue(unrelated.exists())
        self.assertEqual(self.keep.read_bytes(), b"keep")

    def test_without_aggressive_flag_opt_paths_are_not_removed(self):
        hosted = self.root / "hostedtoolcache"
        chrome = self.root / "chrome"
        msedge = self.root / "msedge"
        for path in (hosted, chrome, msedge):
            path.mkdir()
        with patch.object(DISK, "AGGRESSIVE_SDK_PATHS", (hosted, chrome, msedge)):
            DISK.prepare_disk(self.workspace)
        self.assertFalse(self.sdk.exists())
        for path in (hosted, chrome, msedge):
            self.assertTrue(path.exists())

    def test_aggressive_flag_adds_opt_paths(self):
        hosted = self.root / "hostedtoolcache"
        chrome = self.root / "chrome"
        msedge = self.root / "msedge"
        for path in (hosted, chrome, msedge):
            path.mkdir()
        with patch.object(DISK, "AGGRESSIVE_SDK_PATHS", (hosted, chrome, msedge)):
            DISK.prepare_disk(self.workspace, aggressive=True)
        for path in (hosted, chrome, msedge):
            self.assertFalse(path.exists())
        self.assertEqual(self.keep.read_bytes(), b"keep")

    def test_rejects_self_hosted_or_non_linux_before_deleting(self):
        for platform, environment in (("linux", "self-hosted"), ("win32", "github-hosted")):
            with self.subTest(platform=platform, environment=environment):
                with patch.object(DISK.sys, "platform", platform), patch.dict(os.environ, {"RUNNER_ENVIRONMENT": environment}):
                    with self.assertRaisesRegex(RuntimeError, "GitHub-hosted Linux"):
                        DISK.prepare_disk(self.workspace)
                self.assertTrue(self.sdk.exists())

    def test_self_hosted_check_only_preserves_sdks_and_build_outputs(self):
        with patch.dict(os.environ, {"RUNNER_ENVIRONMENT": "self-hosted"}), patch.object(DISK, "resolve_sdk_paths") as scan, patch.object(DISK.shutil, "rmtree") as remove:
            DISK.prepare_disk(self.workspace, check_only=True)
            scan.assert_not_called()
            remove.assert_not_called()
        self.assertTrue(self.sdk.exists())
        self.assertEqual(self.keep.read_bytes(), b"keep")

    def test_self_hosted_check_only_still_rejects_insufficient_capacity(self):
        with patch.dict(os.environ, {"RUNNER_ENVIRONMENT": "self-hosted"}), patch.object(DISK.shutil, "rmtree") as remove:
            with self.assertRaisesRegex(RuntimeError, "at least 24 GiB"):
                DISK.prepare_disk(self.workspace, check_only=True, min_free_gib=24)
            remove.assert_not_called()
        self.assertTrue(self.sdk.exists())

    def test_check_only_refuses_aggressive_cleanup(self):
        with patch.dict(os.environ, {"RUNNER_ENVIRONMENT": "self-hosted"}):
            with self.assertRaisesRegex(RuntimeError, "cannot be combined"):
                DISK.prepare_disk(self.workspace, check_only=True, aggressive=True)
        self.assertTrue(self.sdk.exists())

    def test_check_only_cli_enforces_capacity_without_deletion(self):
        with patch.dict(os.environ, {"RUNNER_ENVIRONMENT": "self-hosted", "GITHUB_WORKSPACE": str(self.workspace)}):
            self.assertEqual(DISK.main(["--check-only"]), 0)
            self.assertEqual(DISK.main(["--check-only", "--min-free-gib", "24"]), 1)
        self.assertTrue(self.sdk.exists())

    def test_rejects_workspace_overlap_before_any_deletion(self):
        for unsafe in (self.workspace, self.workspace / "target", self.root):
            with self.subTest(unsafe=unsafe), patch.object(DISK, "BASE_SDK_PATHS", (self.sdk, unsafe)):
                with self.assertRaisesRegex(RuntimeError, "overlapping"):
                    DISK.prepare_disk(self.workspace)
                self.assertTrue(self.sdk.exists())

    def test_rejects_redirected_path_before_any_deletion(self):
        original_resolve = Path.resolve

        def redirected_resolve(path, *args, **kwargs):
            return self.workspace if path == self.sdk else original_resolve(path)

        with patch.object(Path, "resolve", redirected_resolve):
            with self.assertRaisesRegex(RuntimeError, "redirected"):
                DISK.prepare_disk(self.workspace)
        self.assertTrue(self.sdk.exists())

    def test_rejects_non_directory_target_before_any_deletion(self):
        unexpected = self.root / "file"
        unexpected.touch()
        with patch.object(DISK, "BASE_SDK_PATHS", (self.sdk, unexpected)):
            with self.assertRaisesRegex(RuntimeError, "unexpected"):
                DISK.prepare_disk(self.workspace)
        self.assertTrue(self.sdk.exists())

    def test_glob_resolved_symlink_is_rejected_before_deletion(self):
        julia = self.root / "julia1.12.7"
        julia.mkdir()
        link = self.root / "julia-link"
        link.symlink_to(julia)
        with patch.object(DISK, "GLOB_SDK_DIRS", ((self.root, "julia*"),)):
            with self.assertRaisesRegex(RuntimeError, "redirected"):
                DISK.prepare_disk(self.workspace)
        self.assertTrue(self.sdk.exists())
        self.assertTrue(julia.exists())
        self.assertTrue(link.exists())

    def test_glob_resolved_mount_point_is_rejected(self):
        julia = self.root / "julia2.0.0"
        julia.mkdir()

        # Only the glob-resolved entry reports itself as a mount point, so the
        # fixed allowlist entries validate normally first and the assertion
        # below really exercises the glob item (an unconditional True stub
        # would trip on the first fixed entry and never reach the glob scan).
        def mounted(path):
            return path == julia

        with patch.object(DISK, "GLOB_SDK_DIRS", ((self.root, "julia*"),)), patch.object(
            Path, "is_mount", mounted
        ):
            with self.assertRaisesRegex(RuntimeError, "unexpected"):
                DISK.prepare_disk(self.workspace)
        self.assertTrue(julia.exists())
        self.assertTrue(self.sdk.exists())

    def test_rejects_low_disk_after_cleanup(self):
        with patch.object(DISK.shutil, "disk_usage", return_value=SimpleNamespace(free=DISK.MIN_FREE_BYTES - 1)):
            with self.assertRaisesRegex(RuntimeError, "at least 12 GiB"):
                DISK.prepare_disk(self.workspace)
        self.assertEqual(self.keep.read_bytes(), b"keep")

    def test_custom_min_free_gib_is_enforced(self):
        required = 8 * 1024**3
        with patch.object(DISK.shutil, "disk_usage", return_value=SimpleNamespace(free=required - 1)):
            with self.assertRaisesRegex(RuntimeError, "at least 8 GiB"):
                DISK.prepare_disk(self.workspace, min_free_gib=8)
        # The same free space passes once it meets the custom requirement.
        with patch.object(DISK.shutil, "disk_usage", return_value=SimpleNamespace(free=required)):
            DISK.prepare_disk(self.workspace, min_free_gib=8)

    def test_du_failure_does_not_block_cleanup(self):
        sdk = self.root / "du-sdk"
        sdk.mkdir()
        with patch.object(DISK, "BASE_SDK_PATHS", (sdk,)):
            with patch.object(DISK.subprocess, "run", side_effect=OSError("du missing")):
                DISK.prepare_disk(self.workspace)
            self.assertFalse(sdk.exists())
            sdk.mkdir()
            with patch.object(DISK.subprocess, "run", side_effect=DISK.subprocess.TimeoutExpired(cmd="du", timeout=DISK.DU_TIMEOUT_SECS)):
                DISK.prepare_disk(self.workspace)
            self.assertFalse(sdk.exists())
            sdk.mkdir()
            with patch.object(DISK.subprocess, "run", return_value=SimpleNamespace(returncode=1, stdout="")):
                DISK.prepare_disk(self.workspace)
            self.assertFalse(sdk.exists())
            sdk.mkdir()
            # The 120s cap must be part of the du call itself: an uncapped du
            # could hang the whole cleanup step.
            def assert_capped_du(*args, **kwargs):
                self.assertEqual(kwargs.get("timeout"), DISK.DU_TIMEOUT_SECS)
                self.assertEqual(args[0][:2], ["du", "-sh"])
                return SimpleNamespace(returncode=0, stdout="4.0K\t/x\n")

            with patch.object(DISK.subprocess, "run", side_effect=assert_capped_du):
                DISK.prepare_disk(self.workspace)
        self.assertFalse(sdk.exists())

    def test_non_positive_min_free_gib_is_rejected(self):
        # A zero/negative threshold would silently disable the ENOSPC gate.
        with self.assertRaisesRegex(RuntimeError, "min_free_gib must be >= 1"):
            DISK.prepare_disk(self.workspace, min_free_gib=0)
        self.assertTrue(self.sdk.exists())
        with patch.dict(os.environ, {"GITHUB_WORKSPACE": str(self.workspace)}):
            for bad in ("0", "-5", "abc"):
                with self.subTest(value=bad):
                    with self.assertRaises(SystemExit) as ctx:
                        DISK.main(["--min-free-gib", bad])
                    self.assertEqual(ctx.exception.code, 2)
        self.assertTrue(self.sdk.exists())

    def test_main_fails_without_workspace(self):
        with patch.dict(os.environ, {"GITHUB_WORKSPACE": ""}):
            self.assertEqual(DISK.main([]), 1)
        self.assertTrue(self.sdk.exists())

    def test_cleanup_errors_are_not_ignored(self):
        with patch.dict(os.environ, {"GITHUB_WORKSPACE": str(self.workspace)}):
            with patch.object(DISK.shutil, "rmtree", side_effect=PermissionError("denied")):
                self.assertEqual(DISK.main([]), 1)

    def test_main_passes_cli_flags_through(self):
        extra = self.root / "extra-sdk"
        extra.mkdir()
        with patch.dict(os.environ, {"GITHUB_WORKSPACE": str(self.workspace)}):
            with patch.object(DISK, "AGGRESSIVE_SDK_PATHS", (extra,)):
                with patch.object(DISK.shutil, "disk_usage", return_value=SimpleNamespace(free=DISK.MIN_FREE_BYTES)):
                    self.assertEqual(DISK.main(["--aggressive", "--min-free-gib", "8"]), 0)
        self.assertFalse(self.sdk.exists())
        self.assertFalse(extra.exists())

    def test_main_enforces_custom_min_free_gib(self):
        with patch.dict(os.environ, {"GITHUB_WORKSPACE": str(self.workspace)}):
            with patch.object(DISK.shutil, "disk_usage", return_value=SimpleNamespace(free=5 * 1024**3)):
                self.assertEqual(DISK.main(["--min-free-gib", "10"]), 1)
        self.assertTrue(self.keep.exists())

    def _rust_test_job(self):
        workflow = (ROOT / ".github/workflows/pr-check.yml").read_text(encoding="utf-8")
        return workflow.split("\n  rust-test:", 1)[1].split("\n  cli-test:", 1)[0]

    def _assert_disk_preparation_recipe(self, job):
        pre_swap = "- name: 在 swap 与缓存前准备 Rust 磁盘空间"
        pre = "- name: 缓存恢复前准备 Rust 磁盘空间"
        post = "- name: 在缓存后确认两轮 Rust 测试磁盘空间"
        self.assertEqual(job.count("python3 scripts/ci-rust-disk.py"), 6)
        self.assertLess(job.index(pre_swap), job.index("Set up zram and swap"))
        self.assertLess(job.index("Set up zram and swap"), job.index(pre))
        self.assertLess(job.index(pre), job.index("- name: 恢复 Rust lint 暖缓存"))
        self.assertLess(job.index(pre), job.index("- name: Cargo test cache"))
        self.assertLess(job.index("- name: 恢复 Rust lint 暖缓存"), job.index(post))
        self.assertLess(job.index("- name: Cargo test cache"), job.index(post))
        self.assertLess(job.index(post), job.index("- name: cargo test --lib --no-run"))
        self.assertLess(job.index(post), job.index("- name: cargo test --test behavior_telemetry"))
        expected = (
            'run: |\n'
            '          sudo env GITHUB_WORKSPACE="$GITHUB_WORKSPACE" \\\n'
            '            RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" \\\n'
            '            python3 scripts/ci-rust-disk.py'
        )
        for marker in (pre_swap, pre, post):
            step = job.split(marker, 1)[1].split("\n      - name:", 1)[0]
            body = "\n".join(line for line in step.splitlines() if not line.lstrip().startswith("#"))
            # Both explicit runner modes enforce the same threshold: no aggressive SDK removal, reduced
            # threshold, skipped gate or swallowed failure after tools install.
            suffix = " --min-free-gib 24" if marker == pre_swap else ""
            self_hosted = (
                'run: |\n'
                '          if [[ "$RUNNER_ENVIRONMENT" == self-hosted ]]; then\n'
                '            exec python3 scripts/ci-rust-disk.py --check-only' + suffix + '\n'
                '          fi\n'
            )
            self.assertEqual(body.strip(), self_hosted + expected.split('run: |\n', 1)[1] + suffix)
        self.assertNotIn("continue-on-error", job)

    def test_workflow_prepares_disk_before_swap_and_rechecks_after_caches(self):
        workflow = (ROOT / ".github/workflows/pr-check.yml").read_text(encoding="utf-8")
        job = workflow.split("\n  rust-test:", 1)[1].split("\n  cli-test:", 1)[0]
        preflight = job.index("- name: 在 swap 与缓存前准备 Rust 磁盘空间")
        swap = job.index("- name: Set up zram and swap")
        cache = job.index("- name: Cargo test cache")
        recheck = job.index("- name: 在缓存后确认两轮 Rust 测试磁盘空间")

        self.assertEqual(job.count("python3 scripts/ci-rust-disk.py"), 6)
        self.assertLess(preflight, swap)
        self.assertLess(swap, cache)
        self.assertLess(cache, recheck)
        self.assertIn("--min-free-gib 24", job[preflight:swap])
        self.assertLess(recheck, job.index("- name: cargo test --lib --no-run"))
        self.assertLess(recheck, job.index("- name: cargo test --test behavior_telemetry"))
        self.assertIn('RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT"', job)
        self.assertNotIn("continue-on-error", job)

    def test_workflow_prepares_disk_before_cache_restore_and_rechecks_after_it(self):
        self._assert_disk_preparation_recipe(self._rust_test_job())

    def test_workflow_rejects_cache_restore_after_post_restore_check(self):
        job = self._rust_test_job()
        post = "      - name: 在缓存后确认两轮 Rust 测试磁盘空间"
        for cache in ("恢复 Rust lint 暖缓存", "Cargo test cache"):
            marker = "      - name: " + cache
            start = job.index(marker)
            end = job.index("\n      - name:", start + len(marker))
            block = job[start:end]
            without_cache = job[:start] + job[end:]
            post_start = without_cache.index(post)
            after_post = without_cache.index("\n      - name:", post_start + len(post))
            changed = without_cache[:after_post] + "\n" + block + without_cache[after_post:]
            with self.subTest(cache=cache), self.assertRaises(AssertionError):
                self._assert_disk_preparation_recipe(changed)

    def test_workflow_rejects_missing_or_reordered_disk_preparation(self):
        job = self._rust_test_job()
        pre = "      - name: 缓存恢复前准备 Rust 磁盘空间"
        post = "      - name: 在缓存后确认两轮 Rust 测试磁盘空间"
        pre_start = job.index(pre)
        pre_end = job.index("\n      - name:", pre_start + len(pre))
        post_start = job.index(post)
        post_end = job.index("\n      - name:", post_start + len(post))
        block = job[pre_start:pre_end]
        without_pre = job[:pre_start] + job[pre_end:]
        late = without_pre.index(post)
        mutations = {
            "missing pre-restore": without_pre,
            "missing post-restore": job[:post_start] + job[post_end:],
            "both preparations after cache": without_pre[:late] + block + without_pre[late:],
        }
        for name, changed in mutations.items():
            with self.subTest(mutation=name), self.assertRaises((AssertionError, ValueError)):
                self._assert_disk_preparation_recipe(changed)

    def test_workflow_rejects_weakened_or_unsafe_disk_preparation(self):
        job = self._rust_test_job()
        command = "python3 scripts/ci-rust-disk.py"
        for replacement in (
            command + " --aggressive",
            command + " --min-free-gib 1",
            command + " || true",
            "echo " + command,
        ):
            with self.subTest(replacement=replacement), self.assertRaises(AssertionError):
                self._assert_disk_preparation_recipe(job.replace(command, replacement, 1))
        marker = "- name: 缓存恢复前准备 Rust 磁盘空间"
        for bypass in ("\n        if: false", "\n        continue-on-error: true"):
            with self.subTest(bypass=bypass), self.assertRaises(AssertionError):
                self._assert_disk_preparation_recipe(job.replace(marker, marker + bypass, 1))


class CliLintDiskPolicyTests(unittest.TestCase):
    def _cli_lint_job(self):
        workflow = (ROOT / ".github/workflows/pr-check.yml").read_text(encoding="utf-8")
        return workflow.split("\n  cli-lint:", 1)[1].split("\n  windows-rust-test:", 1)[0]

    def _step(self, job, marker):
        return job.split(marker, 1)[1].split("\n      - name:", 1)[0]

    def _assert_disk_preparation_recipe(self, job):
        pre = "- name: Prepare CLI disk capacity before swap"
        precache = "- name: Check CLI disk capacity before cache"
        post = "- name: Check CLI disk capacity after cache"
        postsave = "- name: Guard CLI cache post-save"
        swap = "- name: Set up zram and swap"
        cache = "- name: Cargo cache"
        clippy = "- name: cargo clippy (workspace, all targets)"
        featureless = "- name: cargo check (workspace, no default features, all targets)"

        # Restoring the cache itself can ENOSPC; a check only after it is too
        # late. Swap and dependency installation must also follow preparation.
        self.assertLess(job.index(pre), job.index(swap))
        self.assertLess(job.index(pre), job.index("- name: Install Tauri Linux system dependencies"))
        self.assertLess(job.index(swap), job.index(precache))
        self.assertLess(job.index(precache), job.index(cache))
        self.assertLess(job.index(cache), job.index(post))
        self.assertLess(job.index(post), job.index(clippy))
        self.assertLess(job.index(post), job.index(featureless))
        self.assertLess(job.index(featureless), job.index(postsave))
        self.assertEqual(job.count("python3 scripts/ci-rust-disk.py"), 8)

        # Keep the existing safe base allowlist and hard capacity floor. An
        # exact recipe also rejects if:false, swallowed failures or aggressive
        # deletion of setup-action tools; none of these checks may become optional.
        expected = (
            '          sudo env GITHUB_WORKSPACE="$GITHUB_WORKSPACE" \\\n'
            '            RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" \\\n'
            '            python3 scripts/ci-rust-disk.py'
        )
        for marker, suffix in ((pre, " --min-free-gib 24"), (precache, ""), (post, "")):
            step = self._step(job, marker)
            body = "\n".join(line for line in step.splitlines() if not line.lstrip().startswith("#"))
            self_hosted = (
                'run: |\n'
                '          if [[ "$RUNNER_ENVIRONMENT" == self-hosted ]]; then\n'
                '            exec python3 scripts/ci-rust-disk.py --check-only' + suffix + '\n'
                '          fi\n'
            )
            self.assertEqual(body.strip(), self_hosted + expected + suffix)
        post_save = (
            "if: ${{ always() && !cancelled() && steps.cli_lint_cache.outcome == 'success' }}\n"
            '        run: |\n'
            '          if [[ "$RUNNER_ENVIRONMENT" == self-hosted ]]; then\n'
            '            python3 scripts/ci-rust-disk.py --check-only\n'
            '          else\n'
            + "\n".join("  " + line for line in expected.splitlines()) + '\n'
            '          fi\n'
            "          printf '%s\\n' 'CACHE_ON_FAILURE=true' >> \"$GITHUB_ENV\""
        )
        self.assertEqual(self._step(job, postsave).strip(), post_save)
        self.assertNotIn("continue-on-error", job)

    def test_cli_lint_reserves_disk_before_swap_and_after_cache(self):
        self._assert_disk_preparation_recipe(self._cli_lint_job())

    def test_cli_lint_rejects_weakened_capacity_checks_for_either_runner(self):
        job = self._cli_lint_job()
        command = "python3 scripts/ci-rust-disk.py"
        markers = (
            ("- name: Prepare CLI disk capacity before swap", " --min-free-gib 24"),
            ("- name: Check CLI disk capacity before cache", ""),
            ("- name: Check CLI disk capacity after cache", ""),
            ("- name: Guard CLI cache post-save", ""),
        )
        for marker, suffix in markers:
            step = self._step(job, marker)
            for check_only in (False, True):
                prefix = "exec " if check_only and marker != markers[-1][0] else ""
                actual = prefix + command + (" --check-only" if check_only else "") + suffix
                line = next(line for line in step.splitlines() if line.strip() == actual)
                weakened = actual.replace("24", "1") if suffix else actual + " --min-free-gib 1"
                for replacement in ("", weakened, actual + " || true", actual + " --aggressive", "echo " + actual):
                    changed_step = step.replace(line, line.replace(actual, replacement), 1)
                    changed = job.replace(marker + step, marker + changed_step, 1)
                    with self.subTest(step=marker, check_only=check_only, replacement=replacement), self.assertRaises(AssertionError):
                        self._assert_disk_preparation_recipe(changed)
            for bypass in ("\n        if: false", "\n        continue-on-error: true"):
                with self.subTest(step=marker, bypass=bypass), self.assertRaises(AssertionError):
                    self._assert_disk_preparation_recipe(job.replace(marker, marker + bypass, 1))

    def test_cli_lint_rejects_missing_or_reordered_checks_and_early_cache_save(self):
        job = self._cli_lint_job()
        markers = (
            "- name: Prepare CLI disk capacity before swap",
            "- name: Check CLI disk capacity before cache",
            "- name: Check CLI disk capacity after cache",
            "- name: Guard CLI cache post-save",
        )
        for marker in markers:
            block = "      " + marker + self._step(job, marker)
            without = job.replace(block, "", 1)
            with self.subTest(missing=marker), self.assertRaises((AssertionError, ValueError)):
                self._assert_disk_preparation_recipe(without)
        pre = "      " + markers[1] + self._step(job, markers[1])
        post = "      " + markers[2]
        without_pre = job.replace(pre, "", 1)
        changed = without_pre.replace(post, pre + post, 1)
        with self.assertRaises(AssertionError):
            self._assert_disk_preparation_recipe(changed)
        post_save = self._step(job, markers[-1])
        flag = next(line for line in post_save.splitlines() if "CACHE_ON_FAILURE=true" in line)
        changed_step = post_save.replace(flag, "", 1).replace('        run: |', '        run: |\n' + flag, 1)
        with self.assertRaises(AssertionError):
            self._assert_disk_preparation_recipe(job.replace(markers[-1] + post_save, markers[-1] + changed_step, 1))


if __name__ == "__main__":
    unittest.main()

import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
RELEASE_WORKFLOW = REPO_ROOT / ".github/workflows/release-packages.yml"
PR_CHECK_WORKFLOW = REPO_ROOT / ".github/workflows/pr-check.yml"
RUST_CACHE_ACTION = "uses: Swatinem/rust-cache@v2"
MAIN_ONLY_SAVE = "save-if: ${{ github.ref == 'refs/heads/main' }}"


class ReleaseCachePolicyTests(unittest.TestCase):
    def test_release_rust_caches_are_read_only_outside_main(self):
        workflow = RELEASE_WORKFLOW.read_text(encoding="utf-8")
        cache_steps = [
            step
            for step in workflow.split("\n      - name:")
            if RUST_CACHE_ACTION in step
        ]

        self.assertEqual(
            len(cache_steps),
            4,
            "新增或删除 release Rust cache 时需要同步检查缓存配额策略",
        )
        for step in cache_steps:
            with self.subTest(step=step.splitlines()[0].strip()):
                self.assertIn(
                    MAIN_ONLY_SAVE,
                    step,
                    "非 main release 只能读取缓存，禁止写入分支缓存",
                )

    def test_rust_test_allows_main_to_rebuild_cold_cache(self):
        workflow = PR_CHECK_WORKFLOW.read_text(encoding="utf-8")
        rust_test_job = workflow.split("\n  rust-test:", maxsplit=1)[1].split(
            "\n  windows-rust-test:", maxsplit=1
        )[0]

        self.assertIn("timeout-minutes: 120", rust_test_job)
        self.assertIn("shared-key: rust-test-v2", rust_test_job)
        self.assertIn(MAIN_ONLY_SAVE, rust_test_job)

    def test_rust_test_restores_lint_cache_before_its_own_cache(self):
        workflow = PR_CHECK_WORKFLOW.read_text(encoding="utf-8")
        rust_test_job = workflow.split("\n  rust-test:", maxsplit=1)[1].split(
            "\n  windows-rust-test:", maxsplit=1
        )[0]
        lint_cache = rust_test_job.index("shared-key: rust-lint")
        test_cache = rust_test_job.index("shared-key: rust-test")
        warm_cache_step = rust_test_job.split(
            "- name: 恢复 Rust lint 暖缓存", maxsplit=1
        )[1].split("- name: Cargo test cache", maxsplit=1)[0]

        self.assertLess(lint_cache, test_cache)
        self.assertIn("save-if: false", warm_cache_step)

    def test_linux_release_jobs_use_hosted_runner_profile(self):
        workflow = RELEASE_WORKFLOW.read_text(encoding="utf-8")
        linux_jobs = {
            "x64": workflow.split("\n  build-linux-x64:", maxsplit=1)[1].split(
                "\n  build-linux-arm64:", maxsplit=1
            )[0],
            "arm64": workflow.split(
                "\n  build-linux-arm64:", maxsplit=1
            )[1].split("\n  build-windows-x64:", maxsplit=1)[0],
        }

        for architecture, job in linux_jobs.items():
            with self.subTest(architecture=architecture):
                job_env = job.split("\n    steps:", maxsplit=1)[0]
                self.assertIn(
                    'CARGO_PROFILE_RELEASE_LTO: "false"', job_env
                )
                self.assertIn(
                    'CARGO_PROFILE_RELEASE_CODEGEN_UNITS: "16"', job_env
                )
                self.assertIn(
                    'RUSTFLAGS: "-C link-arg=-fuse-ld=lld"', job_env
                )
                self.assertIn("build-essential pkg-config cmake lld", job)

    def test_windows_artifact_release_uses_unsigned_build_entry(self):
        workflow = RELEASE_WORKFLOW.read_text(encoding="utf-8")
        windows_job = workflow.split(
            "\n  build-windows-x64:", maxsplit=1
        )[1].split("\n  build-macos-universal:", maxsplit=1)[0]

        self.assertIn("run: npm run build:nsis:local", windows_job)
        self.assertNotIn("run: npm run build:nsis\n", windows_job)
        self.assertNotIn("signing.wosign.conf.json", windows_job)
        self.assertNotIn(".builtin-secrets.env", windows_job)


if __name__ == "__main__":
    unittest.main()

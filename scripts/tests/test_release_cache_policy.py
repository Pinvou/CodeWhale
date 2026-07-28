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
                    "release PR 只能读取缓存，禁止写入 pull-ref 缓存",
                )

    def test_rust_test_allows_main_to_rebuild_cold_cache(self):
        workflow = PR_CHECK_WORKFLOW.read_text(encoding="utf-8")
        rust_test_job = workflow.split("\n  rust-test:", maxsplit=1)[1].split(
            "\n  windows-rust-test:", maxsplit=1
        )[0]

        self.assertIn("timeout-minutes: 60", rust_test_job)
        self.assertIn("shared-key: rust-test", rust_test_job)
        self.assertIn(MAIN_ONLY_SAVE, rust_test_job)


if __name__ == "__main__":
    unittest.main()

import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
RELEASE_WORKFLOW = ROOT / ".github/workflows/release-packages.yml"


class ReleaseArm64PolicyTests(unittest.TestCase):
    def setUp(self):
        self.workflow = RELEASE_WORKFLOW.read_text(encoding="utf-8")
        self.arm64_job = self.workflow.split(
            "\n  build-linux-arm64:", maxsplit=1
        )[1].split("\n  build-windows-x64:", maxsplit=1)[0]

    def release_job(self, start, end):
        return self.workflow.split(f"\n  {start}:", maxsplit=1)[1].split(
            f"\n  {end}:", maxsplit=1
        )[0]

    def test_public_package_jobs_skip_pull_requests(self):
        for start, end in (
            ("build-linux-x64", "build-linux-arm64"),
            ("build-linux-arm64", "validate-windows-runtime-pr"),
            ("build-macos-universal", None),
        ):
            job = (
                self.release_job(start, end)
                if end
                else self.workflow.split(f"\n  {start}:", maxsplit=1)[1]
            )
            with self.subTest(job=start):
                job_header = job.split("\n    steps:", maxsplit=1)[0]
                self.assertIn(
                    "needs.check-version-bump.outputs.build_required == 'true'",
                    job_header,
                )
                self.assertIn(
                    "github.event_name == 'workflow_dispatch'", job_header
                )
                self.assertIn("github.event_name == 'push'", job_header)
                self.assertIn("github.ref == 'refs/heads/main'", job_header)

    def test_arm64_all_release_events_use_hosted_runner_profile(self):
        job_env = self.arm64_job.split("\n    steps:", maxsplit=1)[0]
        # release profile 已设 thin LTO(thin 替代 fat),ARM 不再需要 env 覆盖;
        # 保留 lld(thin LTO 的 link 阶段需要支持 LLVM bitcode 的链接器)。
        self.assertIn('RUSTFLAGS: "-C link-arg=-fuse-ld=lld"', job_env)
        self.assertNotIn('CARGO_PROFILE_RELEASE_LTO', job_env)
        self.assertNotIn('CARGO_PROFILE_RELEASE_CODEGEN_UNITS', job_env)

        self.assertIn("build-essential pkg-config cmake lld", self.arm64_job)
        self.assertIn(
            "- name: 构建 deb（托管 ARM64 发布配置）", self.arm64_job
        )
        self.assertEqual(
            self.arm64_job.count(
                "run: node scripts/tauri/build.js build --bundles deb"
            ),
            1,
        )


if __name__ == "__main__":
    unittest.main()

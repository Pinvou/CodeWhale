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
        for setting in (
            'CARGO_PROFILE_RELEASE_LTO: "false"',
            'CARGO_PROFILE_RELEASE_CODEGEN_UNITS: "16"',
            # Size-policy flags(与 x64 job 同款): --icf=safe 为 lld 同码折叠
            # (safe 层只折叠地址确定不被取用的函数,保守处理 rust#160267 的
            # 函数地址同一性边界); remap-path-prefix 将制品内嵌的构建机绝对
            # 路径规一化为 /(顶层 rustc flag,不是 -C codegen 选项)。
            'RUSTFLAGS: "-C link-arg=-fuse-ld=lld '
            '-C link-arg=-Wl,--icf=safe '
            '--remap-path-prefix=${{ github.workspace }}=/"',
        ):
            self.assertIn(setting, job_env)

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

    def test_arm64_release_budget_keeps_measured_headroom(self):
        # 2026-10 measured baseline: 19 fully-successful release builds
        # ran 37m12s-46m22s; the highest observed arm64 job (48m14s) ran
        # inside a run cancelled via its windows job. The old 90 cap held ~2x
        # headroom. 120 keeps ~2.5x for toolchain rollovers and cold
        # caches; the cap is pinned end-anchored (a bare substring would
        # also match 1200) with the last-key-wins count check, so it can
        # neither regress to 90 nor drift upward unnoticed. The loong64
        # 1.5x budget reads this cap live in test_release_loongarch64_policy.
        arm64_job = self.workflow.split(
            "\n  build-linux-arm64:", maxsplit=1
        )[1].split("\n  build-linux-loongarch64:", maxsplit=1)[0]
        # Comment-stripped like the loong64 twin, so a future explanatory
        # comment containing "timeout-minutes:" cannot false-red the count.
        live = "\n".join(
            line
            for line in arm64_job.splitlines()
            if not line.lstrip().startswith("#")
        )
        self.assertIn("\n    timeout-minutes: 120\n", live)
        self.assertEqual(
            live.count("timeout-minutes:"),
            1,
            "exactly one live timeout budget may exist in the job",
        )


if __name__ == "__main__":
    unittest.main()

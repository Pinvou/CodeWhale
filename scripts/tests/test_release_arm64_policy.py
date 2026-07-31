import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
RELEASE_WORKFLOW = ROOT / ".github/workflows/release-packages.yml"


class ReleaseArm64PolicyTests(unittest.TestCase):
    def setUp(self):
        workflow = RELEASE_WORKFLOW.read_text(encoding="utf-8")
        self.arm64_job = workflow.split("\n  build-linux-arm64:", maxsplit=1)[1].split(
            "\n  build-windows-x64:", maxsplit=1
        )[0]

    def test_arm64_all_release_events_use_hosted_runner_profile(self):
        job_env = self.arm64_job.split("\n    steps:", maxsplit=1)[0]
        for setting in (
            'CARGO_PROFILE_RELEASE_LTO: "false"',
            'CARGO_PROFILE_RELEASE_CODEGEN_UNITS: "16"',
            'RUSTFLAGS: "-C link-arg=-fuse-ld=lld"',
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


if __name__ == "__main__":
    unittest.main()

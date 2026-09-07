import platform
import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
RELEASE_WORKFLOW = ROOT / ".github/workflows/release-packages.yml"
GUARD_SCRIPT = ROOT / "scripts" / "check-linux-glibc-floor.sh"
ELF_POLICY_SCRIPT = ROOT / "scripts" / "check-linux-elf-policy.sh"
ASR_BUILD_SCRIPT = ROOT / "scripts" / "asr" / "build-sensevoice-runtime.sh"
LINUX_OVERLAY = ROOT / "pinvou3-app/src-tauri/config/platforms/linux/tauri.conf.json"

# Linux 发布基线是 Ubuntu 22.04 (glibc 2.35) x86_64/arm64。发布二进制链接
# 构建机 glibc,runner 升级会静默抬高基线,因此 runner 标签与 glibc 守护
# 都是有意决策,本测试防止无意的基线漂移。


class ReleaseLinuxFloorPolicyTests(unittest.TestCase):
    def setUp(self):
        self.workflow = RELEASE_WORKFLOW.read_text(encoding="utf-8")
        self.x64_job = self.workflow.split("\n  build-linux-x64:", maxsplit=1)[1].split(
            "\n  build-linux-arm64:", maxsplit=1
        )[0]
        self.arm64_job = self.workflow.split("\n  build-linux-arm64:", maxsplit=1)[1].split(
            "\n  validate-windows-runtime-pr:", maxsplit=1
        )[0]

    def test_linux_builds_pin_ubuntu_22_04_runners(self):
        # 带换行断言,避免 x64 段误匹配 ubuntu-22.04-arm 这类更长标签。
        self.assertIn("runs-on: ubuntu-22.04\n", self.x64_job)
        self.assertIn("runs-on: ubuntu-22.04-arm\n", self.arm64_job)
        self.assertNotIn("ubuntu-latest", self.x64_job.split("\n    steps:")[0])
        self.assertNotIn("ubuntu-24.04", self.arm64_job.split("\n    steps:")[0])

    def test_linux_builds_run_arch_and_glibc_guard_before_upload(self):
        # 守护必须覆盖两个 Linux job、传入对应 deb 架构，且位于上传之前。
        for job, arch in ((self.x64_job, "amd64"), (self.arm64_job, "arm64")):
            guard = job.find("check-linux-glibc-floor.sh")
            upload = job.find("uses: actions/upload-artifact@")
            self.assertGreater(guard, 0, "missing glibc floor guard step")
            self.assertGreater(upload, guard, "glibc floor guard must run before upload")
            guard_step = self._step_block(job, "check-linux-glibc-floor.sh")
            self.assertRegex(guard_step, rf'\.deb" {arch}\s*$')
        self.assertEqual(
            self.workflow.count("run: ./scripts/check-linux-glibc-floor.sh"), 2
        )

    def test_linux_builds_rotate_rust_cache_key_per_runner_baseline(self):
        # 换 runner 基座必须换 rust-cache key:24.04 glibc 环境编译的缓存产物
        # 复用进 22.04 构建会把高版本符号引用混进发布二进制,-jammy 后缀把
        # 两个基座的缓存隔离。runner 标签与 cache key 必须绑定变更。
        self.assertIn("shared-key: release-linux-x64-jammy", self.x64_job)
        self.assertIn("shared-key: release-linux-arm64-jammy", self.arm64_job)

    def _step_block(self, job, needle):
        # 提取含 needle 的步骤块(步骤以 6 空格 + '- ' 开始),供单步骤断言。
        at = job.find(needle)
        self.assertGreater(at, 0, f"missing step containing {needle!r}")
        start = job.rfind("\n      - ", 0, at)
        self.assertGreater(start, 0, f"{needle!r} must live inside a step")
        end = job.find("\n      - ", at)
        return job[start:] if end == -1 else job[start:end]

    def test_glibc_floor_guard_step_cannot_be_neutralized(self):
        # continue-on-error 或步骤级 if: 会让超基线产物绿着通过守护,把启动
        # 崩溃留给 22.04 用户;守护步骤必须无条件执行且直接调用脚本。
        for job in (self.x64_job, self.arm64_job):
            step = self._step_block(job, "check-linux-glibc-floor.sh")
            self.assertIn("run: ./scripts/check-linux-glibc-floor.sh", step)
            self.assertNotIn("continue-on-error", step)
            self.assertNotIn("\n        if:", step)

    def test_linux_deb_unified_name_consistent_across_rename_guard_upload(self):
        # deb 统一名横跨 重命名/校验→守护→上传 手工同步;失配只会在长时间
        # 发布构建末尾暴露,因此在轻量契约层直接钉住。
        for job, arch in ((self.x64_job, "x64"), (self.arm64_job, "arm64")):
            local_name = "pinvou3_${VERSION}_" + arch + ".deb"
            upload_name = (
                "pinvou3_${{ needs.check-version-bump.outputs.version }}_" + arch + ".deb"
            )
            self.assertEqual(
                job.count(local_name), 2, f"{arch}: local deb name must appear in packaging and guard"
            )
            self.assertEqual(
                job.count(upload_name), 2, f"{arch}: upload deb name must appear in both upload paths"
            )

    def test_guard_script_defaults_to_2204_glibc_floor(self):
        # 通用 ELF 守卫统一持有基线，deb wrapper 只负责解包和传递目标架构。
        source = ELF_POLICY_SCRIPT.read_text(encoding="utf-8")
        self.assertIn('glibc_floor="${3:-2.35}"', source)
        self.assertIn('"3.4.30"', source)  # GLIBCXX_ (jammy gcc 12)
        self.assertIn('"1.3.13"', source)  # CXXABI_ (jammy gcc 12)
        wrapper = GUARD_SCRIPT.read_text(encoding="utf-8")
        self.assertIn('expected_arch="${2:-}"', wrapper)
        self.assertIn("check-linux-elf-policy.sh", wrapper)

    def test_guard_script_is_valid_bash(self):
        for script in (GUARD_SCRIPT, ELF_POLICY_SCRIPT, ASR_BUILD_SCRIPT):
            with self.subTest(script=script.name):
                subprocess.run(["bash", "-n", str(script)], check=True)

    def test_sensevoice_runtime_is_rebuilt_from_pinned_source(self):
        source = ASR_BUILD_SCRIPT.read_text(encoding="utf-8")
        self.assertIn("sensevoice-source.env", source)
        self.assertIn("-DBUILD_SHARED_LIBS=OFF", source)
        self.assertIn("-DGGML_NATIVE=OFF", source)
        self.assertIn("check-linux-elf-policy.sh", source)
        self.assertNotIn(
            "pinvou3-app/src-tauri/resources/platforms/linux/asr/sense-voice-main",
            subprocess.run(
                ["git", "ls-files"], cwd=ROOT, check=True, text=True, capture_output=True
            ).stdout.splitlines(),
        )

    def test_elf_policy_accepts_host_arch_and_rejects_mismatch(self):
        machine = platform.machine().lower()
        if machine in ("x86_64", "amd64"):
            expected, wrong = "amd64", "arm64"
        elif machine in ("aarch64", "arm64"):
            expected, wrong = "arm64", "amd64"
        else:
            self.skipTest(f"unsupported test host architecture: {machine}")

        host_elf = str(Path("/bin/true").resolve())
        subprocess.run([str(ELF_POLICY_SCRIPT), host_elf, expected], check=True)
        mismatch = subprocess.run(
            [str(ELF_POLICY_SCRIPT), host_elf, wrong],
            text=True,
            capture_output=True,
        )
        self.assertNotEqual(mismatch.returncode, 0)
        self.assertIn("architecture", mismatch.stderr)
        self.assertIn("!= expected", mismatch.stderr)

    def test_deb_declares_webkit_floor(self):
        # tauri-runtime-wry 启用 webkit2gtk v2_40,运行时需要 WebKitGTK ≥ 2.40;
        # jammy 原始 pocket 只有 2.36.0,显式 depends 让 apt 给出明确错误而非启动崩溃。
        overlay = LINUX_OVERLAY.read_text(encoding="utf-8")
        self.assertIn("libwebkit2gtk-4.1-0 (>= 2.40)", overlay)


if __name__ == "__main__":
    unittest.main()

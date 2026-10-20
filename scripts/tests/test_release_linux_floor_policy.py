import platform
import subprocess
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
RELEASE_WORKFLOW = ROOT / ".github/workflows/release-packages.yml"
GUARD_SCRIPT = ROOT / "scripts" / "check-linux-glibc-floor.sh"
ELF_POLICY_SCRIPT = ROOT / "scripts" / "check-linux-elf-policy.sh"
ASR_BUILD_SCRIPT = ROOT / "scripts" / "asr" / "build-sensevoice-runtime.sh"
ASR_SETUP_SCRIPT = ROOT / "scripts" / "asr" / "setup-sensevoice.sh"
LINUX_OVERLAY = ROOT / "pinvou3-app/src-tauri/config/platforms/linux/tauri.conf.json"

# Linux 发布基线:Ubuntu 22.04 (glibc 2.35) x86_64/arm64 + loongarch64 的
# deepin 23/25 / UOS V25 (glibc 2.38)。x86_64/arm64 发布二进制链接构建机
# glibc,runner 升级会静默抬高基线,因此 runner 标签与 glibc 守护都是有
# 意决策;loongarch64 走交叉工具链 + deepin sysroot,基线由 sysroot 决定。
# 本测试防止无意的基线漂移。


def _without_yaml_comments(block):
    # Same helper as test_release_cache_policy: assertions must judge live
    # configuration, so commented-out lines can neither satisfy a pin nor
    # false-fail one (a policy comment merely naming the action must not
    # turn the absence pin red).
    return "\n".join(
        line for line in block.splitlines() if not line.lstrip().startswith("#")
    )


class ReleaseLinuxFloorPolicyTests(unittest.TestCase):
    def setUp(self):
        self.workflow = RELEASE_WORKFLOW.read_text(encoding="utf-8")
        self.x64_job = self.workflow.split("\n  build-linux-x64:", maxsplit=1)[1].split(
            "\n  build-linux-arm64:", maxsplit=1
        )[0]
        self.arm64_job = self.workflow.split("\n  build-linux-arm64:", maxsplit=1)[1].split(
            "\n  build-linux-loongarch64:", maxsplit=1
        )[0]
        self.loong64_job = self.workflow.split(
            "\n  build-linux-loongarch64:", maxsplit=1
        )[1].split("\n  validate-windows-runtime-pr:", maxsplit=1)[0]

    def test_linux_builds_pin_ubuntu_22_04_runners(self):
        # 带换行断言,避免 x64 段误匹配 ubuntu-22.04-arm 这类更长标签。
        self.assertIn("runs-on: ubuntu-22.04\n", self.x64_job)
        self.assertIn("runs-on: ubuntu-22.04-arm\n", self.arm64_job)
        self.assertNotIn("ubuntu-latest", self.x64_job.split("\n    steps:")[0])
        self.assertNotIn("ubuntu-24.04", self.arm64_job.split("\n    steps:")[0])
        # loongarch64 没有 GitHub 托管 loong64 runner:交叉构建必须钉在
        # x86_64 ubuntu-22.04(工具链资产按宿主架构 sha256 钉死),不允许
        # 漂到 arm 基座或 latest。
        self.assertIn("runs-on: ubuntu-22.04\n", self.loong64_job)
        self.assertNotIn("runs-on: ubuntu-22.04-arm", self.loong64_job)
        self.assertNotIn("ubuntu-latest", self.loong64_job.split("\n    steps:")[0])
        self.assertNotIn("ubuntu-24.04", self.loong64_job.split("\n    steps:")[0])

    def test_linux_builds_run_arch_and_glibc_guard_before_upload(self):
        # 守护必须覆盖三个 Linux job、传入对应 deb 架构，且位于上传之前。
        for job in (self.x64_job, self.arm64_job, self.loong64_job):
            guard = job.find("check-linux-glibc-floor.sh")
            upload = job.find("uses: actions/upload-artifact@")
            self.assertGreater(guard, 0, "missing glibc floor guard step")
            self.assertGreater(upload, guard, "glibc floor guard must run before upload")
        self.assertEqual(
            self.workflow.count("run: ./scripts/check-linux-glibc-floor.sh"), 3
        )
        # 每个守护必须拿到自己的 deb 架构参数,其他架构的 ELF 不可能混进包里。
        for job, arch in (
            (self.x64_job, "amd64"),
            (self.arm64_job, "arm64"),
            (self.loong64_job, "loongarch64"),
        ):
            step = self._step_block(job, "check-linux-glibc-floor.sh")
            self.assertRegex(step, rf'\.deb" {arch}\s*$')

    def test_linux_builds_carry_no_rust_cache(self):
        # The runner base and the rust-cache key had to rotate together:
        # artifacts compiled under 24.04's glibc must not be reused by a
        # 22.04 build (newer symbol references would leak into the release
        # binaries) — the old -jammy suffix encoded exactly that binding.
        # Since 2026-10-15 the release legs hold no rust-cache at all
        # (the restore-only steps were removed, see
        # test_release_cache_policy): release builds run fully cold, so no
        # cross-base reuse surface is left. If a release-side cache is ever
        # reintroduced, the runner-label/key-binding contract must come
        # back with it. Assertions run over comment-stripped text: a
        # policy comment merely naming the action must not false-fail the
        # absence pin. Belt-and-braces with test_release_cache_policy's
        # file-wide absence pin (the same mutation trips both); the
        # per-job scoping here adds locality and keeps the
        # runner-base/key-binding contract visible next to the floor
        # policy it protects.
        self.assertNotIn("Swatinem/rust-cache", _without_yaml_comments(self.x64_job))
        self.assertNotIn(
            "Swatinem/rust-cache", _without_yaml_comments(self.arm64_job)
        )
        # The loongarch64 job is cross-compiled cold by design and restores
        # the release-linux-loong64-jammy-* plain actions/cache keys
        # (toolchain/sysroot) read-only — pinned by
        # test_release_loongarch64_policy; it deliberately holds no
        # rust-cache either.
        self.assertNotIn(
            "Swatinem/rust-cache", _without_yaml_comments(self.loong64_job)
        )

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
        # 崩溃留给用户;守护步骤必须无条件执行且直接调用脚本。
        for job in (self.x64_job, self.arm64_job, self.loong64_job):
            step = self._step_block(job, "check-linux-glibc-floor.sh")
            self.assertIn("run: ./scripts/check-linux-glibc-floor.sh", step)
            self.assertNotIn("continue-on-error", step)
            self.assertNotIn("\n        if:", step)

    def test_linux_deb_unified_name_consistent_across_rename_guard_upload(self):
        # deb 统一名横跨 重命名/校验→守护→上传 手工同步;失配只会在长时间
        # 发布构建末尾暴露,因此在轻量契约层直接钉住。loong64 deb 由编排脚本
        # 直接按该命名产出(打包脚本按 dpkg 架构命名),无重命名步骤。
        for job, arch in (
            (self.x64_job, "x64"),
            (self.arm64_job, "arm64"),
            (self.loong64_job, "loong64"),
        ):
            local_name = "pinvou3_${VERSION}_" + arch + ".deb"
            upload_name = (
                "pinvou3_${{ needs.check-version-bump.outputs.version }}_" + arch + ".deb"
            )
            # x64: 重命名/校验 + glibc 守卫;arm64 多一处 Repack 步骤
            # (xz -9 重打包直接引用本地名,见 docs/binary-size-optimization.md)。
            expected_local = 3 if arch == "arm64" else 2
            self.assertEqual(
                job.count(local_name), expected_local,
                f"{arch}: local deb name must appear in packaging/repack and guard",
            )
            self.assertEqual(
                job.count(upload_name), 2, f"{arch}: upload deb name must appear in both upload paths"
            )

    def test_guard_script_defaults_to_per_arch_glibc_floors(self):
        # deb wrapper 的 glibc 基线按架构取默认值,通用 ELF 守卫持有按架构的
        # 默认映射与 jammy (gcc 12) 的 GLIBCXX_/CXXABI_ 基线。amd64/arm64 默认
        # Ubuntu 22.04 基线(workflow 不传基线);loongarch64 默认 2.38,即其
        # 交叉 sysroot 构建自的 deepin 23/25 + UOS V25 基线。
        source = GUARD_SCRIPT.read_text(encoding="utf-8")
        self.assertIn('amd64|arm64) glibc_floor="${glibc_floor:-2.35}" ;;', source)
        self.assertIn('loongarch64) glibc_floor="${glibc_floor:-2.38}" ;;', source)
        self.assertIn("check-linux-elf-policy.sh", source)
        policy = ELF_POLICY_SCRIPT.read_text(encoding="utf-8")
        self.assertIn('default_glibc_floor="2.35"', policy)
        self.assertIn('loongarch64) expected_machine="LoongArch";', policy)
        self.assertIn('default_glibc_floor="2.38"; baseline_label="deepin 23/25 / UOS V25 (glibc 2.38)"', policy)
        self.assertIn('"3.4.30"', policy)  # GLIBCXX_ (jammy gcc 12)
        self.assertIn('"1.3.13"', policy)  # CXXABI_ (jammy gcc 12)

    def test_guard_script_is_valid_bash(self):
        for script in (GUARD_SCRIPT, ELF_POLICY_SCRIPT, ASR_BUILD_SCRIPT, ASR_SETUP_SCRIPT):
            with self.subTest(script=script.name):
                subprocess.run(["bash", "-n", str(script)], check=True)

    def test_sensevoice_runtime_is_rebuilt_from_pinned_source(self):
        source = ASR_BUILD_SCRIPT.read_text(encoding="utf-8")
        self.assertIn("sensevoice-source.env", source)
        self.assertIn("-DBUILD_SHARED_LIBS=OFF", source)
        self.assertIn("-DGGML_NATIVE=OFF", source)
        self.assertIn("check-linux-elf-policy.sh", source)
        # 浅克隆检出后必须复核正是 pinned commit(与 setup-sensevoice.sh
        # 同一道 tripwire),防 refspec/环境值被改坏时发布产物悄悄换源;
        # 失配诊断必须带上实际检出的 commit。
        self.assertIn('rev-parse HEAD)" = "$SENSEVOICE_SOURCE_COMMIT"', source)
        self.assertIn(
            "expected $SENSEVOICE_SOURCE_COMMIT, got $(git -C",
            source,
        )
        self.assertNotIn(
            "pinvou3-app/src-tauri/resources/platforms/linux/asr/sense-voice-main",
            subprocess.run(
                ["git", "ls-files"], cwd=ROOT, check=True, text=True, capture_output=True
            ).stdout.splitlines(),
        )

    def test_sensevoice_setup_script_pins_downloads_and_checksums(self):
        # #1149 复审跟进:setup 脚本此前在 scripts/tests 零覆盖(兄弟构建
        # 脚本有钉定先例)。钉住供应链加固锚点,防止后续改动悄悄拆掉:
        # 两处下载的重试与 sha256 校验、量化档/架构分档校验和、pinned
        # commit tripwire、校验失败显式报错(不得 --status 静默退出,且
        # 必须带期望/实际值并走 stderr)、同目录 tmp+rename 原子落位
        # (引擎/模型/shim 三处)与失败时的临时文件清理。
        source = ASR_SETUP_SCRIPT.read_text(encoding="utf-8")
        self.assertIn("q4_k) MODEL_SHA256=", source)
        self.assertIn("q8_0) MODEL_SHA256=", source)
        self.assertIn("aarch64) CMAKE_SHA256=", source)
        self.assertIn("x86_64) CMAKE_SHA256=", source)
        self.assertEqual(source.count("--retry-all-errors"), 2)
        self.assertEqual(source.count("sha256sum --check --status"), 2)
        # 校验/tripwire 失败必须有用户可见报错(--status 会吞掉诊断),
        # 且携带期望/实际值供用户自助比对。
        self.assertEqual(source.count("校验失败"), 2)
        self.assertEqual(source.count("，实际 $("), 2)
        self.assertIn("pinned commit 复核失败", source)
        # 所有 ❌ 报错走 stderr,管道/CI 捕获场景不丢诊断。
        self.assertEqual(source.count(">&2"), 7)
        # 引擎/模型/shim 都必须经同目录临时文件 + rename 落位。
        self.assertEqual(source.count(".tmp.$$"), 3)
        self.assertEqual(source.count("mv -f"), 3)
        # 失败/中断时 EXIT trap 清理已创建的落位临时文件。
        self.assertIn(
            '[ -z "${engine_tmp:-}" ] || rm -f "$engine_tmp"', source
        )

    @unittest.skipUnless(sys.platform.startswith("linux"), "needs a Linux ELF host")
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

"""Local package and CI routing regression; never install a deb or contact OTA."""
import fnmatch
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import struct
import subprocess
import sys
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]
GUARD = ROOT / "scripts/check-linux-glibc-floor.sh"
HELPER = ROOT / "scripts/check-cmsd-bundled-package.py"
RESOURCE = Path("usr/lib/pinvou3/runtime/cmsd")


def paths_filter(workflow, name):
    body = workflow.split(f"            {name}:\n", 1)[1]
    body = re.split(r"^            \w+:", body, maxsplit=1, flags=re.M)[0]
    return re.findall(r"^              - ['\"]([^'\"]+)['\"]", body, re.M)


def matches(path, patterns):
    return (any(fnmatch.fnmatchcase(path, item) for item in patterns if not item.startswith("!"))
            and not any(fnmatch.fnmatchcase(path, item[1:]) for item in patterns if item.startswith("!")))


@unittest.skipUnless(sys.platform.startswith("linux") and shutil.which("dpkg-deb")
                     and platform.machine() in ("x86_64", "aarch64"), "native Linux deb tools required")
class BundledCmsdDebTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="pinvou-cmsd-deb-policy-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.root = self.base / "root"
        self.component = self.root / RESOURCE
        self.component.mkdir(parents=True)
        self.architecture = "arm64" if platform.machine() == "aarch64" else "amd64"
        self.manifest_arch = "aarch64" if self.architecture == "arm64" else "x86_64"
        self.payload = Path("/bin/true").resolve().read_bytes()
        (self.root / "usr/bin").mkdir(parents=True)
        # Whole-package ELF scanning must not mistake the desktop executable
        # for proof that CMS is present.
        (self.root / "usr/bin/pinvou3-tauri").write_bytes(self.payload)
        (self.root / "usr/bin/pinvou3-tauri").chmod(0o755)
        self.write_component()

    def write_component(self, payload=None):
        binary = self.component / "cmsd"
        binary.write_bytes(self.payload if payload is None else payload)
        binary.chmod(0o755)
        self.manifest = {"format": "megacube-cmsd-component-v1", "component": "cmsd", "version": "0.1.0",
                         "architecture": self.manifest_arch, "os": "linux", "api": "v1",
                         "state_format": "cmsd-json-v1", "sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                         "size": binary.stat().st_size}
        self.save_manifest()

    def save_manifest(self):
        (self.component / "manifest.json").write_text(json.dumps(self.manifest))

    def guard(self, expected=0, architecture=None, declared=None):
        architecture = architecture or self.architecture
        control = self.root / "DEBIAN/control"
        control.parent.mkdir(exist_ok=True)
        control.write_text("Package: pinvou3\nVersion: 0.1.0\nArchitecture: "
                           + (declared or architecture) + "\nMaintainer: Fixture <fixture@example.invalid>\n"
                           "Description: isolated package guard fixture\n")
        deb = self.base / "fixture.deb"
        subprocess.run(["dpkg-deb", "--build", "--root-owner-group", str(self.root), str(deb)],
                       check=True, capture_output=True)
        result = subprocess.run(["bash", str(GUARD), str(deb), architecture], text=True, capture_output=True)
        if expected == 0:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def test_complete_native_package_passes(self):
        self.guard()

    def test_loong_deb_control_field_uses_debian_arch_name(self):
        # r9 P1-1：真实 loong64 deb 的 control 字段是 loong64（DEB_ARCH），调用
        # 方传 Rust target 口径 loongarch64。归一化后架构门必须放行——后续组件
        # 阶段因 loong 不随包 cmsd 而失败是另一码事，只断言不因架构口径卡死。
        result = self.guard(expected=1, architecture="loongarch64", declared="loong64")
        self.assertNotIn("deb architecture", result.stdout + result.stderr)

    def test_missing_and_partial_component_packages_fail_with_desktop_elf_present(self):
        binary = self.component / "cmsd"
        original = binary.read_bytes()
        binary.unlink()
        self.assertIn("cmsd", self.guard(1).stderr)
        binary.write_bytes(original)
        binary.chmod(0o755)
        (self.component / "manifest.json").unlink()
        self.assertIn("manifest.json", self.guard(1).stderr)
        shutil.rmtree(self.component)
        self.guard(1)

    def test_wrong_manifest_hash_size_and_contract_fail(self):
        for key, value in (("sha256", "0" * 64), ("size", len(self.payload) + 1),
                           ("architecture", "aarch64" if self.manifest_arch == "x86_64" else "x86_64"),
                           ("api", "v99"), ("state_format", "unknown"), ("size", True)):
            with self.subTest(field=key, value=value):
                self.write_component()
                self.manifest[key] = value
                self.save_manifest()
                self.guard(1)

    def test_wrong_real_elf_architecture_fails_even_with_valid_checksum(self):
        payload = bytearray(self.payload)
        struct.pack_into("<H", payload, 18, 62 if self.architecture == "arm64" else 183)
        self.write_component(payload)
        self.assertIn("ELF architecture", self.guard(1).stderr)

    def test_component_abi_is_checked_before_publication(self):
        # Alter only a symbol-version label in a real ELF. readelf sees the
        # unsupported requirement; the fixture is inspected, never executed.
        versions = re.findall(rb"GLIBC_([0-9]+\.[0-9]+)", self.payload)
        version = next((value for value in versions if len(value) == len(b"9.99")), None)
        self.assertIsNotNone(version, "host fixture must carry a two-digit GLIBC minor")
        payload = self.payload.replace(b"GLIBC_" + version, b"GLIBC_9.99")
        self.write_component(payload)
        result = self.guard(1)
        self.assertIn("GLIBC_9.99", result.stderr)

    def test_component_symlinks_and_parent_links_fail(self):
        for relative in (RESOURCE / "cmsd", RESOURCE / "manifest.json", RESOURCE):
            with self.subTest(path=relative):
                target = self.root / relative
                retained = target.with_name(target.name + "-retained")
                target.rename(retained)
                target.symlink_to(retained.name, target_is_directory=retained.is_dir())
                self.assertIn("symlink", self.guard(1).stderr)
                target.unlink()
                retained.rename(target)
        runtime = self.component.parent
        retained = runtime.with_name("runtime-retained")
        runtime.rename(retained)
        runtime.symlink_to(retained.name, target_is_directory=True)
        self.assertIn("symlink", self.guard(1).stderr)

    def test_component_must_have_execution_bit(self):
        (self.component / "cmsd").chmod(0o644)
        self.assertIn("executable", self.guard(1).stderr)

    def test_wrong_debian_header_fails(self):
        self.assertIn("deb architecture", self.guard(1, declared="arm64" if self.architecture == "amd64" else "amd64").stderr)

    def test_duplicate_or_oversized_manifest_fails(self):
        file = self.component / "manifest.json"
        file.write_text(json.dumps(self.manifest)[:-1] + ',"api":"v1"}')
        self.assertIn("duplicate", self.guard(1).stderr)
        file.write_bytes(b" " * 16385)
        self.guard(1)

    def test_loongarch_deb_without_cmsd_preserves_existing_elf_gate(self):
        shutil.rmtree(self.component)
        payload = bytearray(self.payload)
        struct.pack_into("<H", payload, 18, 258)
        (self.root / "usr/bin/pinvou3-tauri").write_bytes(payload)
        self.guard(architecture="loongarch64")

    def test_component_in_wrong_resource_location_is_rejected(self):
        self.component.rename(self.component.with_name("cmsd-wrong"))
        self.guard(1)

    def test_loongarch_explicitly_has_no_cmsd(self):
        result = subprocess.run([sys.executable, str(HELPER), str(self.root), "loongarch64"],
                                text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsupported", result.stderr)
        shutil.rmtree(self.component)
        subprocess.run([sys.executable, str(HELPER), str(self.root), "loongarch64"], check=True,
                       capture_output=True)


class CmsdCiRoutingTests(unittest.TestCase):
    def setUp(self):
        self.workflow = (ROOT / ".github/workflows/pr-check.yml").read_text()
        self.release = (ROOT / ".github/workflows/release-packages.yml").read_text()

    def test_component_only_routes_to_own_gate_and_unclassified_rust_remains_protected(self):
        cmsd = paths_filter(self.workflow, "cmsd")
        for path in ("components/cmsd/src/main.rs", "components/cmsd/Cargo.toml", "components/cmsd/tests/test_build_linux_component.py"):
            self.assertTrue(matches(path, cmsd), path)
            for group in ("rust_code", "rust_full", "rust_dependencies", "cli_rust"):
                self.assertFalse(matches(path, paths_filter(self.workflow, group)), (group, path))
        for path in ("pinvou3-app/src-tauri/src/new_module.rs", "unexpected/new_rust_crate/src/main.rs"):
            self.assertTrue(matches(path, paths_filter(self.workflow, "rust_code")), path)
        self.assertTrue(matches("unexpected/new_rust_crate/Cargo.toml", paths_filter(self.workflow, "rust_full")))
        for path in ("scripts/check-cmsd-bundled-package.py", "scripts/check-linux-glibc-floor.sh",
                     "scripts/release-deb.sh", "pinvou3-app/scripts/tauri/cmsd-component.js"):
            self.assertTrue(matches(path, cmsd), path)
            self.assertTrue(matches(path, paths_filter(self.workflow, "release_contract")), path)

    def test_main_release_path_and_real_diff_regex_select_component_changes(self):
        patterns = re.findall(r"^      - '([^']+)'", self.release.split("  pull_request:")[0], re.M)
        pull_patterns = re.findall(r"^      - '([^']+)'", self.release.split("  pull_request:")[1].split("  workflow_dispatch:")[0], re.M)
        line = next(line.strip() for line in self.release.splitlines() if line.strip().startswith("'^(components/cmsd/"))
        pattern = line.split("'", 2)[1]
        for path in ("components/cmsd/src/main.rs", "components/cmsd/Cargo.lock", "scripts/check-cmsd-bundled-package.py",
                     "scripts/release-deb.sh", "scripts/tests/test_cmsd_release_policy.py",
                     "pinvou3-app/scripts/tauri/cmsd-component.js", "pinvou3-app/tests/cmsd_bundled_packaging.test.js"):
            self.assertTrue(matches(path, patterns), path)
            self.assertTrue(matches(path, pull_patterns), path)
            if shutil.which("grep"):
                result = subprocess.run(["grep", "-Eq", pattern], input=path + "\n", text=True)
                self.assertEqual(result.returncode, 0, path)

    def test_required_gate_rejects_applicable_skipped_cmsd_matrix(self):
        required = self.workflow.split("\n  required-gate:", 1)[1]
        self.assertIn("      - cmsd-check\n", required)
        self.assertIn("CMSD_REQUIRED_BY_POLICY: ${{ needs.changes.outputs.cmsd == 'true' }}", required)
        # required-gate 的运行块之后可能还有后续 step（如 CI 复用的成功输入记录），
        # 截断到首个非脚本缩进行，避免把 YAML 当 bash 执行。
        run_body = required.split("        run: |\n", 1)[1]
        script_lines = []
        for line in run_body.splitlines():
            if line and not line.startswith("          "):
                break
            script_lines.append(line)
        command = textwrap.dedent("\n".join(script_lines))
        environment = {**os.environ, "GITHUB_EVENT_NAME": "pull_request", "BACKFLOW_MODE": "ordinary"}
        for key in re.findall(r"^          ([A-Z0-9_]+):", required, re.M):
            # r8 合并后 required-gate 段含 runner 专属 PATH 等进程关键键
            # （来自 main 的 rust-network 包装步骤），覆盖本地进程 PATH 会让
            # subprocess 找不到 bash。仅模拟门禁语义变量。
            if key in ("PATH", "HOME", "TMPDIR"):
                continue
            environment[key] = "success" if key.endswith("_RESULT") else "false"
        environment["CMSD_REQUIRED_BY_POLICY"] = "true"
        for status in ("success", "skipped", "failure", "cancelled", ""):
            with self.subTest(status=status):
                result = subprocess.run(["bash", "-eu", "-c", command], env={**environment, "CMSD_RESULT": status},
                                        text=True, capture_output=True)
                self.assertEqual(result.returncode == 0, status == "success", result.stdout + result.stderr)
        result = subprocess.run(["bash", "-eu", "-c", command], env={**environment,
                                "CMSD_RESULT": "skipped", "CMSD_REQUIRED_BY_POLICY": "false"}, capture_output=True)
        self.assertEqual(result.returncode, 0)

    def test_native_matrix_checks_real_crate_and_package(self):
        job = self.workflow.split("\n  cmsd-check:\n", 1)[1].split("\n  knowledge-rust:", 1)[0]
        self.assertIn("os: [ubuntu-22.04, ubuntu-22.04-arm]", job)
        self.assertIn("needs.changes.outputs.cmsd == 'true'", job)
        for command in ("cargo fmt", "cargo clippy", "cargo test", "components/cmsd/scripts/build-linux-component.py",
                        "scripts/check-cmsd-bundled-package.py"):
            self.assertIn(command, job)
        self.assertNotIn("continue-on-error", job)

    def test_ota_guard_precedes_every_network_access(self):
        script = (ROOT / "scripts/release-deb.sh").read_text()
        guard = script.index('"$REPO_ROOT/scripts/check-linux-glibc-floor.sh"')
        self.assertGreater(guard, script.index('"$REPO_ROOT/scripts/repack-deb-xz.sh"'))
        for command in ("$(ssh ", "rsync -", "curl -"):
            self.assertLess(guard, script.index(command))
        subprocess.run(["bash", "-n", str(ROOT / "scripts/release-deb.sh")], check=True)


if __name__ == "__main__":
    unittest.main()

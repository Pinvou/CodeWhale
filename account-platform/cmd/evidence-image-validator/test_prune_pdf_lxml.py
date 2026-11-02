import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zipfile

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("prune_pdf_lxml", HERE / "prune_pdf_lxml.py")
prune = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prune)


class PruneTests(unittest.TestCase):
    def test_missing_audited_wheel_is_not_accepted(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {"EVIDENCE_PDF_AUDIT_DIR": tmp}):
            with self.assertRaises(FileNotFoundError):
                with zipfile.ZipFile(self.source("aarch64")):
                    pass

    def test_only_pdf_image_applies_pruning_after_hashed_install(self):
        platform = HERE.parent.parent
        pdf = (platform / "Dockerfile.evidence-validator-pdf").read_text()
        image_only = (platform / "Dockerfile.evidence-image-validator").read_text()
        run = next(line for line in pdf.splitlines() if line.startswith("RUN ") and "-m pip install" in line)
        self.assertLess(run.index("--require-hashes"), run.index("/prune_pdf_lxml.py"))
        self.assertLess(run.index("/prune_pdf_lxml.py"), run.index("/pdf_pruned_smoke.py"))
        self.assertIn("--no-compile", run)
        self.assertIn("--network=none", run)
        self.assertNotIn("prune_pdf_lxml", image_only)

    def source(self, arch):
        root = os.environ.get("EVIDENCE_PDF_AUDIT_DIR")
        if not root:
            if os.environ.get("EVIDENCE_PDF_REQUIRE_SYMLINK") == "1":
                self.fail("audited Linux wheels are required in strict mode")
            self.skipTest("audited Linux wheels not configured")
        return Path(root) / arch / prune.WHEELS[arch][0]

    def extract(self, wheel, root):
        with zipfile.ZipFile(wheel) as z:
            z.extractall(root)

    def test_both_architectures_preserve_every_other_recorded_file(self):
        self.assertEqual(set(prune.WHEELS), {"x86_64", "aarch64"})
        for arch in ("x86_64", "aarch64"):
            with self.subTest(arch=arch), tempfile.TemporaryDirectory() as tmp:
                wheel = self.source(arch)
                site = Path(tmp) / "site"; site.mkdir()
                self.extract(wheel, site)
                before = prune.rows((site / prune.RECORD).read_bytes())
                before_data = {p: hashlib.sha256((site / p).read_bytes()).hexdigest() for p in before if p != prune.RECORD}
                audit = Path(tmp) / "audit"
                prune.prune(site, wheel, arch, audit)
                after = prune.rows((site / prune.RECORD).read_bytes())
                self.assertEqual(set(after), set(before) - set(prune.REMOVED))
                for name in set(before_data) - set(prune.REMOVED):
                    self.assertEqual(hashlib.sha256((site / name).read_bytes()).hexdigest(), before_data[name])
                for name in prune.REMOVED:
                    self.assertFalse((site / name).exists())
                self.assertEqual(prune.rows((audit / "lxml-original-wheel.RECORD").read_bytes()), before)
                manifest = json.loads((audit / "lxml-pruning.json").read_text())
                self.assertEqual(manifest["removed_files"], prune.REMOVED)
                self.assertEqual(manifest["original_wheel_sha256"], prune.WHEELS[arch][1])
                self.assertEqual(manifest["installed_record_after_sha256"], prune.digest((site / prune.RECORD).read_bytes()))
                self.assertIn("modified", (audit / "NOTICE-pruning.txt").read_text())
                fragment = json.loads((audit / "lxml-pruning-sbom-fragment.json").read_text())
                self.assertEqual(fragment["bomFormat"], "CycloneDX")
                self.assertIn("not a complete SBOM", fragment["components"][0]["properties"][0]["value"])

    def test_tamper_wrong_arch_and_unrecorded_fail_before_deletion(self):
        for mode in ("resource", "other", "unrecorded", "wrong_arch", "duplicate_record"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as tmp:
                wheel = self.source("x86_64")
                site = Path(tmp) / "site"; site.mkdir(); self.extract(wheel, site)
                a, b = [site / p for p in prune.REMOVED]
                if mode == "resource": a.write_bytes(b"changed")
                if mode == "other": (site / "lxml/__init__.py").write_bytes(b"changed")
                if mode == "unrecorded": (site / "lxml/unrecorded").write_bytes(b"x")
                if mode == "duplicate_record":
                    record = site / prune.RECORD; record.write_bytes(record.read_bytes() + record.read_bytes().splitlines(keepends=True)[0])
                selected_arch = "aarch64" if mode == "wrong_arch" else "x86_64"
                with self.assertRaises(ValueError):
                    prune.prune(site, wheel, selected_arch, Path(tmp) / "audit")
                self.assertTrue(a.exists() and b.exists())
                self.assertFalse((Path(tmp) / "audit").exists())

    def test_audit_path_failures_do_not_modify_installation(self):
        wheel = self.source("x86_64")
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            site = root / "site"; site.mkdir(); self.extract(wheel, site)
            record = site / prune.RECORD
            original = record.read_bytes()
            original_resources = {name: (site / name).read_bytes() for name in prune.REMOVED}
            parent_file = root / "not-a-directory"; parent_file.write_bytes(b"unchanged")
            existing = root / "existing"; existing.mkdir()
            for target in (parent_file / "audit", parent_file, existing, site / "audit"):
                with self.subTest(target=target), self.assertRaises(ValueError):
                    prune.prune(site, wheel, "x86_64", target)
                self.assertEqual(record.read_bytes(), original)
                self.assertEqual({name: (site / name).read_bytes() for name in prune.REMOVED}, original_resources)
            self.assertEqual(parent_file.read_bytes(), b"unchanged")

            real_open = Path.open
            def deny_probe(path, *args, **kwargs):
                if path.name == ".write-probe":
                    raise PermissionError("synthetic unwritable audit directory")
                return real_open(path, *args, **kwargs)
            with patch.object(Path, "open", deny_probe), self.assertRaises(PermissionError):
                prune.prune(site, wheel, "x86_64", root / "unwritable")
            self.assertEqual(record.read_bytes(), original)
            self.assertEqual({name: (site / name).read_bytes() for name in prune.REMOVED}, original_resources)

    def test_symlinked_audit_parent_is_rejected(self):
        wheel = self.source("x86_64")
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            site = root / "site"; site.mkdir(); self.extract(wheel, site)
            record = site / prune.RECORD
            original = record.read_bytes()
            original_resources = {name: (site / name).read_bytes() for name in prune.REMOVED}
            target = root / "target"; target.mkdir()
            link = root / "link"
            try:
                link.symlink_to(target, target_is_directory=True)
            except (OSError, NotImplementedError):
                if os.environ.get("EVIDENCE_PDF_REQUIRE_SYMLINK") == "1":
                    self.fail("required symlink creation is unavailable")
                self.skipTest("local platform cannot create directory symlinks")
            with self.assertRaises(ValueError):
                prune.prune(site, wheel, "x86_64", link / "audit")
            self.assertEqual(record.read_bytes(), original)
            self.assertEqual({name: (site / name).read_bytes() for name in prune.REMOVED}, original_resources)


if __name__ == "__main__":
    unittest.main()

"""Static checks only; these do not build or exercise the Linux image."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
PLATFORM = HERE.parent.parent
spec = importlib.util.spec_from_file_location("prepare_pdf_image", HERE / "prepare_pdf_image.py")
prepare_pdf_image = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare_pdf_image)


class ImageCandidateTests(unittest.TestCase):
    def test_offline_hashed_install_and_disabled_entrypoint(self):
        lines = (PLATFORM / "Dockerfile.evidence-validator-pdf").read_text().splitlines()
        instructions = [line.strip() for line in lines if line.strip() and not line.lstrip().startswith("#")]
        runs = [line for line in instructions if line.startswith("RUN ")]
        installs = [line for line in runs if " -m pip install " in line]
        self.assertEqual(len(installs), 1)
        for required in ("--network=none", "--no-index", "--only-binary=:all:",
                         "--require-hashes", "--no-deps", "pip check", "__libqpdf_version__"):
            self.assertIn(required, installs[0])
        self.assertIn("USER 65533:65533", instructions)
        self.assertIn('ENTRYPOINT ["/validator"]', instructions)
        self.assertFalse(any("EVIDENCE_PDF_CANDIDATE_ENABLED=true" in line or
                             "EVIDENCE_IMAGE_VALIDATOR_ENABLED=true" in line for line in instructions))

    def test_explicit_context_is_flat_and_verified(self):
        location = os.environ.get("EVIDENCE_PDF_AUDIT_DIR")
        if not location:
            self.skipTest("local audit directory not supplied")
        with tempfile.TemporaryDirectory() as directory:
            output = str(Path(directory) / "out.oci")
            args = ["prepare_pdf_image", "--audit-dir", location, "--platform", "linux/arm64",
                    "--build", "--output", output]
            def inspect_build(command, check):
                self.assertTrue(check)
                index = command.index("--build-context")
                self.assertTrue(command[index + 1].startswith("pdf-wheels="))
                context = Path(command[index + 1].split("=", 1)[1])
                self.assertEqual(len(list(context.iterdir())), 4)
                self.assertTrue(all(p.is_file() and p.suffix == ".whl" for p in context.iterdir()))
                self.assertIn("linux/arm64", command)
                artifact = Path(command[command.index("--output") + 1].split("dest=", 1)[1])
                self.assertNotEqual(artifact, Path(output))
                artifact.write_bytes(b"test archive")
            with patch.object(sys, "argv", args), patch.object(prepare_pdf_image.subprocess, "run", side_effect=inspect_build) as run:
                prepare_pdf_image.main()
                self.assertEqual(run.call_count, 1)
            self.assertEqual(Path(output).read_bytes(), b"test archive")
            self.assertEqual([p.name for p in Path(directory).iterdir()], ["out.oci"])

    def test_build_race_and_failure_never_replace_destination(self):
        location = os.environ.get("EVIDENCE_PDF_AUDIT_DIR")
        if not location:
            self.skipTest("local audit directory not supplied")
        for mode in ("race", "preexisting", "failure", "cancel", "hardlink_unavailable"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                output = Path(directory) / "out.oci"
                if mode == "preexisting":
                    output.write_bytes(b"other process")
                args = ["prepare_pdf_image", "--audit-dir", location, "--platform", "linux/amd64",
                        "--build", "--output", str(output)]
                def build(command, check):
                    artifact = Path(command[command.index("--output") + 1].split("dest=", 1)[1])
                    artifact.write_bytes(b"candidate")
                    if mode == "race":
                        output.write_bytes(b"other process")
                    if mode == "failure":
                        raise subprocess.CalledProcessError(1, command)
                    if mode == "cancel":
                        raise KeyboardInterrupt()
                link = patch.object(prepare_pdf_image.os, "link", side_effect=OSError("unsupported")) if mode == "hardlink_unavailable" else patch.object(prepare_pdf_image.os, "link", wraps=prepare_pdf_image.os.link)
                with patch.object(sys, "argv", args), patch.object(prepare_pdf_image.subprocess, "run", side_effect=build), link:
                    with self.assertRaises((OSError, subprocess.CalledProcessError, KeyboardInterrupt)):
                        prepare_pdf_image.main()
                self.assertEqual(output.read_bytes() if output.exists() else None,
                                 b"other process" if mode in ("race", "preexisting") else None)
                self.assertFalse(any(p.name.startswith(".pinvou-pdf-build-") for p in Path(directory).iterdir()))

    def test_existing_and_dangling_output_symlinks_are_not_followed(self):
        location = os.environ.get("EVIDENCE_PDF_AUDIT_DIR")
        if not location:
            self.skipTest("local audit directory not supplied")
        for dangling in (False, True):
            with self.subTest(dangling=dangling), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                output, target = root / "out.oci", root / "target.oci"
                if not dangling:
                    target.write_bytes(b"existing target")
                try:
                    output.symlink_to(target)
                except (OSError, NotImplementedError):
                    self.skipTest("local platform does not permit symlink creation")
                args = ["prepare_pdf_image", "--audit-dir", location, "--platform", "linux/amd64",
                        "--build", "--output", str(output)]
                def build(command, check):
                    Path(command[command.index("--output") + 1].split("dest=", 1)[1]).write_bytes(b"candidate")
                with patch.object(sys, "argv", args), patch.object(prepare_pdf_image.subprocess, "run", side_effect=build):
                    with self.assertRaises(FileExistsError):
                        prepare_pdf_image.main()
                self.assertTrue(output.is_symlink())
                self.assertEqual(target.read_bytes() if target.exists() else None,
                                 None if dangling else b"existing target")

    def test_lock_matches_audited_wheels_when_supplied(self):
        location = os.environ.get("EVIDENCE_PDF_AUDIT_DIR")
        if not location:
            self.skipTest("local audit directory not supplied")
        directory = Path(location)
        manifest = json.loads((directory / "manifest.json").read_text())
        bindings = json.loads((HERE / "pdf-wheel-bindings.json").read_text())
        self.assertEqual(bindings, [{k: wheel[k] for k in ("platform", "name", "version", "filename", "sha256")}
                                    for wheel in manifest["wheels"]])
        lock = (HERE / "pdf-requirements.lock").read_text()
        entries = {}
        for line in lock.splitlines():
            if not line or line.startswith("#"):
                continue
            name_version = line.split()[0]
            entries[name_version] = set(re.findall(r"--hash=sha256:([0-9a-f]{64})", line))
        expected = {}
        combinations = set()
        for wheel in manifest["wheels"]:
            name_version = wheel["name"] + "==" + wheel["version"]
            self.assertTrue(wheel["filename"].endswith(".whl"))
            digest = hashlib.sha256((directory / wheel["platform"] / wheel["filename"]).read_bytes()).hexdigest()
            self.assertEqual(digest, wheel["sha256"])
            expected.setdefault(name_version, set()).add(digest)
            combinations.add((wheel["platform"], wheel["name"]))
        self.assertEqual(entries, expected)
        self.assertEqual(combinations, {(arch, name) for arch in ("x86_64", "aarch64")
                                        for name in ("lxml", "packaging", "pikepdf", "pillow")})

    def test_renamed_other_architecture_is_rejected(self):
        location = os.environ.get("EVIDENCE_PDF_AUDIT_DIR")
        if not location:
            self.skipTest("local audit directory not supplied")
        audit = Path(location)
        manifest = json.loads((audit / "manifest.json").read_text())
        arm = next(w for w in manifest["wheels"] if w["platform"] == "aarch64" and w["name"] == "pikepdf")
        amd = next(w for w in manifest["wheels"] if w["platform"] == "x86_64" and w["name"] == "pikepdf")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "aarch64").mkdir()
            shutil.copyfile(audit / "x86_64" / amd["filename"], root / "aarch64" / arm["filename"])
            forged = dict(arm, sha256=amd["sha256"])
            (root / "manifest.json").write_text(json.dumps({"wheels": [forged]}))
            with self.assertRaisesRegex(ValueError, "filename and hash binding"):
                prepare_pdf_image.checked_wheels(root, "linux/arm64")


if __name__ == "__main__":
    unittest.main()

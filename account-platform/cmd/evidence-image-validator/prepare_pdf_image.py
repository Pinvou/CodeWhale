"""Validate a local wheel audit and explicitly provide BuildKit's named context.

Default is a read-only preflight. --build invokes Docker and writes the requested
OCI archive; it is not an approval of the candidate for production use.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile

HERE = Path(__file__).resolve().parent
PLATFORM_ROOT = HERE.parent.parent
PACKAGES = {"lxml", "packaging", "pikepdf", "pillow"}
ARCHES = {"linux/amd64": "x86_64", "linux/arm64": "aarch64"}


def checked_wheels(audit, platform):
    audit = audit.resolve(strict=True)
    manifest = json.loads((audit / "manifest.json").read_text(encoding="utf-8"))
    entries = {}
    for line in (HERE / "pdf-requirements.lock").read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            entries[line.split()[0]] = set(re.findall(r"--hash=sha256:([0-9a-f]{64})", line))
    if {name.split("==")[0] for name in entries} != PACKAGES:
        raise ValueError("unexpected lock packages")
    wheels = {}
    architecture = ARCHES[platform]
    bindings = json.loads((HERE / "pdf-wheel-bindings.json").read_text(encoding="utf-8"))
    expected = {item["name"]: item for item in bindings if item["platform"] == architecture}
    if len(expected) != len(PACKAGES) or set(expected) != PACKAGES:
        raise ValueError("incomplete wheel bindings")
    for item in manifest["wheels"]:
        if item["platform"] != architecture:
            continue
        name = item["name"]
        filename = item["filename"]
        if name not in PACKAGES or name in wheels or Path(filename).name != filename or not filename.endswith(".whl"):
            raise ValueError("unexpected or duplicate wheel")
        if any(item.get(key) != expected[name][key] for key in ("platform", "name", "version", "filename", "sha256")):
            raise ValueError("wheel does not match filename and hash binding")
        if name == "packaging":
            if not filename.endswith("-py3-none-any.whl"):
                raise ValueError("unexpected universal wheel")
        elif "manylinux" not in filename or "_" + architecture not in filename or "-cp312-cp312-" not in filename:
            raise ValueError("wheel does not match target platform")
        source = audit / architecture / filename
        if source.is_symlink() or source.resolve(strict=True).parent != (audit / architecture).resolve(strict=True) or not source.is_file():
            raise ValueError("wheel outside audit directory")
        digest = hashlib.sha256(source.read_bytes()).hexdigest()
        if digest != item["sha256"] or digest not in entries.get(name + "==" + item["version"], set()):
            raise ValueError("wheel hash or version mismatch")
        wheels[name] = (source, digest)
    if set(wheels) != PACKAGES:
        raise ValueError("incomplete wheel set")
    return wheels


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--audit-dir", required=True, type=Path)
    parser.add_argument("--platform", required=True, choices=ARCHES)
    parser.add_argument("--build", action="store_true", help="explicitly invoke Docker buildx")
    parser.add_argument("--output", type=Path, help="new OCI archive path (required with --build)")
    args = parser.parse_args()
    if args.build != (args.output is not None):
        parser.error("--build and --output must be supplied together")
    wheels = checked_wheels(args.audit_dir, args.platform)
    if not args.build:
        print("preflight OK:", args.platform, "four locked wheels; no build invoked")
        return
    # Resolve the parent only: resolving the final component could follow a
    # pre-existing symlink. os.link atomically fails if that name already exists.
    output = args.output.absolute()
    parent = output.parent.resolve(strict=True)
    output = parent / output.name
    if not parent.is_dir() or "," in str(parent) or output.name in ("", ".", ".."):
        raise ValueError("output requires an existing directory without commas")
    with tempfile.TemporaryDirectory(prefix=".pinvou-pdf-build-", dir=parent) as temporary:
        work = Path(temporary)
        context = work / "wheels"
        context.mkdir()
        artifact = work / "image.oci"
        for source, digest in wheels.values():
            destination = context / source.name
            shutil.copyfile(source, destination)
            if hashlib.sha256(destination.read_bytes()).hexdigest() != digest:
                raise ValueError("copied wheel hash mismatch")
        subprocess.run(["docker", "buildx", "build", "--platform", args.platform,
                        "--build-context", "pdf-wheels=" + str(context),
                        "--file", str(PLATFORM_ROOT / "Dockerfile.evidence-validator-pdf"),
                        "--output", "type=oci,dest=" + str(artifact), str(PLATFORM_ROOT)], check=True)
        if not stat.S_ISREG(artifact.lstat().st_mode):
            raise ValueError("build did not produce a regular OCI archive")
        # Never remove or replace output: an existing file, symlink, race winner,
        # or filesystem without hard-link support makes publication fail closed.
        os.link(artifact, output, follow_symlinks=False)


if __name__ == "__main__":
    main()

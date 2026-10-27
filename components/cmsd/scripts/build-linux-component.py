#!/usr/bin/env python3
"""Build the in-tree cmsd component for native Linux with GLIBC <= 2.35.

Release builds require committed Pinvou component sources. --allow-dirty is
explicitly for local development and embeds a distinct, content-bound identity.
A compatible sysroot is required when the host libc is newer than the baseline.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shlex
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
TARGETS = {"aarch64": "aarch64-unknown-linux-gnu", "x86_64": "x86_64-unknown-linux-gnu"}
GENERATED = {"target", "target-jammy", ".dev", "__pycache__", ".git"}


def run(*args, cwd=ROOT, **kwargs):
    return subprocess.check_output(args, cwd=cwd, text=True, **kwargs).strip()


def source_identity(root, allow_dirty=False):
    """Bind the actual component files to their owning Pinvou commit."""
    repository = Path(run("git", "rev-parse", "--show-toplevel", cwd=root)).resolve()
    if root.resolve() != repository / "components/cmsd":
        raise ValueError("cmsd must be built from Pinvou components/cmsd")
    revision = run("git", "rev-parse", "HEAD", cwd=repository)
    committed = {}
    tree = subprocess.check_output(
        ["git", "ls-tree", "-r", "-z", "HEAD", "--", "components/cmsd"], cwd=repository)
    for entry in tree.decode().split("\0"):
        if entry:
            metadata, name = entry.split("\t", 1)
            mode, kind, object_id = metadata.split()
            if kind != "blob":
                raise ValueError("component sources must be ordinary tracked files")
            committed[name] = (mode, object_id)
    object_format = run("git", "rev-parse", "--show-object-format", cwd=repository)
    digest = hashlib.sha256()
    actual = set()
    modified = False
    for folder, directories, names in os.walk(root, followlinks=False):
        current = Path(folder)
        directories[:] = sorted(name for name in directories
                                if name != "__pycache__"
                                and not (current == root and name in GENERATED))
        for directory in directories:
            if (current / directory).is_symlink():
                raise ValueError("component source directories must not be symlinks")
        for name in sorted(names):
            if name.endswith((".pyc", ".pyo")):
                continue
            file = current / name
            if file.is_symlink():
                raise ValueError("component source files must not be symlinks")
            relative = file.relative_to(repository).as_posix()
            actual.add(relative)
            content = file.read_bytes()
            executable = bool(file.stat().st_mode & 0o111)
            blob = hashlib.new(object_format, b"blob " + str(len(content)).encode() + b"\0" + content)
            expected = ("100755" if executable else "100644", blob.hexdigest())
            modified |= committed.get(relative) != expected
            digest.update(relative.encode() + b"\0")
            digest.update(hashlib.sha256(content).digest())
            digest.update(b"x" if executable else b"-")
    changed = bool(run("git", "status", "--porcelain", "--untracked-files=all",
                       "--", "components/cmsd", cwd=repository))
    dirty = changed or modified or actual != set(committed)
    if dirty and not allow_dirty:
        raise ValueError("component sources must all be committed and unchanged; "
                         "--allow-dirty is only for local development")
    source_hash = digest.hexdigest()
    identity = f"development:{revision}:{source_hash}" if dirty else revision
    return {"repository_revision": revision, "source_directory": "components/cmsd",
            "source_sha256": source_hash, "dirty": dirty, "build_revision": identity}


def validate_binary_info(binary, identity, architecture):
    info = json.loads(subprocess.check_output([str(binary), "--component-info"], timeout=5))
    if (info.get("component") != "cmsd" or info.get("build_revision") != identity
            or info.get("os") != "linux" or info.get("architecture") != architecture):
        raise ValueError("built binary identity/platform does not match the requested source")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--sysroot", type=Path)
    parser.add_argument("--target-dir", type=Path, default=ROOT / "target")
    parser.add_argument("--allow-dirty", action="store_true",
                        help="local development only; embed a content-bound development identity")
    parser.add_argument("--offline", action="store_true", help="require locally cached Cargo dependencies")
    args = parser.parse_args()
    architecture = platform.machine()
    target = TARGETS.get(architecture)
    if sys.platform != "linux" or target is None:
        parser.error("native Linux aarch64/x86_64 build required")
    if args.output.exists():
        parser.error("output directory must be new")
    try:
        source = source_identity(ROOT, args.allow_dirty)
    except ValueError as error:
        parser.error(str(error))
    build = args.target_dir.resolve()
    env = {**os.environ, "CMSD_BUILD_REVISION": source["build_revision"],
           "CARGO_BUILD_JOBS": os.environ.get("CARGO_BUILD_JOBS", "1"),
           "CARGO_TARGET_DIR": str(build)}
    if args.sysroot:
        sysroot = args.sysroot.resolve(strict=True)
        triplet = "aarch64-linux-gnu" if architecture == "aarch64" else "x86_64-linux-gnu"
        library = sysroot / "usr/lib" / triplet
        if not (library / "crt1.o").is_file():
            parser.error("sysroot must contain the matching libc development files")
        wrapper = build / "component-linker.sh"
        wrapper.parent.mkdir(parents=True, exist_ok=True)
        wrapper.write_text("#!/bin/sh\nexec gcc --sysroot=" + shlex.quote(str(sysroot))
                           + " -B" + shlex.quote(str(library) + "/") + ' "$@"\n')
        wrapper.chmod(0o700)
        env["CARGO_TARGET_" + target.upper().replace("-", "_") + "_LINKER"] = str(wrapper)
        env["CC_" + target.replace("-", "_")] = str(wrapper)
    command = ["cargo", "build", "--locked", "--release", "--target", target]
    if args.offline:
        command.append("--offline")
    subprocess.run(command, cwd=ROOT, env=env, check=True)
    binary = build / target / "release/cmsd"
    validate_binary_info(binary, source["build_revision"], architecture)
    versions = run("readelf", "--version-info", str(binary))
    required = {tuple(map(int, match.split("."))) for match in re.findall(r"\bGLIBC_(\d+\.\d+(?:\.\d+)?)\b", versions)}
    if not required or max(required) > (2, 35):
        raise RuntimeError("component exceeds the GLIBC 2.35 baseline; rebuild with a compatible sysroot")
    # A source edit while Cargo ran must never be attributed to the captured identity.
    if source_identity(ROOT, args.allow_dirty) != source:
        raise RuntimeError("component sources changed during build; discard and rebuild")
    subprocess.run([sys.executable, str(ROOT / "scripts/build-component-package.py"),
                    "--binary", str(binary), "--output", str(args.output.resolve())], check=True)
    (args.output / "source.json").write_text(json.dumps(source, indent=2) + "\n")


if __name__ == "__main__":
    main()

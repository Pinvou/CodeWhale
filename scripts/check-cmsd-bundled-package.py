#!/usr/bin/env python3
"""Validate cmsd at its real Tauri resource location in an extracted Linux deb.

Pure local inspection: never execute the packaged daemon or control a service.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import stat
import struct
import subprocess
import sys

RESOURCE_PATH = Path("usr/lib/pinvou3/runtime/cmsd")
ARCHITECTURES = {"amd64": ("x86_64", 62), "arm64": ("aarch64", 183)}
MANIFEST_KEYS = {"format", "component", "version", "architecture", "os", "api",
                 "state_format", "sha256", "size"}


def ordinary_path(root, relative, directory=False):
    current = root
    for part in relative.parts:
        current = current / part
        mode = current.lstat().st_mode
        if stat.S_ISLNK(mode):
            raise ValueError(f"bundled cmsd path must not contain a symlink: {relative}")
        if current != root / relative or directory:
            if not stat.S_ISDIR(mode):
                raise ValueError(f"bundled cmsd directory is invalid: {relative}")
        elif not stat.S_ISREG(mode):
            raise ValueError(f"bundled cmsd file is not ordinary: {relative}")
    return current


def unique_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate cmsd manifest field: {key}")
        result[key] = value
    return result


def validate(root, architecture):
    if architecture == "loongarch64":
        # CMS has no LoongArch runtime support; never package a host binary there.
        directory = root / RESOURCE_PATH
        if directory.exists() or directory.is_symlink():
            raise ValueError("cmsd must not be bundled on unsupported loongarch64")
        return
    expected_architecture, machine = ARCHITECTURES[architecture]
    ordinary_path(root, RESOURCE_PATH, directory=True)
    binary = ordinary_path(root, RESOURCE_PATH / "cmsd")
    manifest_file = ordinary_path(root, RESOURCE_PATH / "manifest.json")
    if manifest_file.stat().st_size > 16384:
        raise ValueError("cmsd manifest exceeds 16 KiB")
    manifest = json.loads(manifest_file.read_text(encoding="utf-8"), object_pairs_hook=unique_keys)
    if not isinstance(manifest, dict) or set(manifest) != MANIFEST_KEYS:
        raise ValueError("invalid cmsd manifest fields")
    if (manifest["format"] != "megacube-cmsd-component-v1" or manifest["component"] != "cmsd"
            or manifest["os"] != "linux" or manifest["architecture"] != expected_architecture
            or manifest["api"] != "v1" or manifest["state_format"] != "cmsd-json-v1"
            or not isinstance(manifest["version"], str)
            or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?", manifest["version"])
            or not isinstance(manifest["sha256"], str)
            or not re.fullmatch(r"[a-f0-9]{64}", manifest["sha256"])
            or type(manifest["size"]) is not int or not 64 <= manifest["size"] <= 256 * 1024 * 1024):
        raise ValueError("incompatible cmsd manifest architecture or contract")
    if binary.stat().st_size != manifest["size"]:
        raise ValueError("cmsd binary size mismatch")
    if not binary.stat().st_mode & 0o111:
        raise ValueError("cmsd binary has no executable bit")
    with binary.open("rb") as stream:
        header = stream.read(64)
        stream.seek(0)
        digest = hashlib.sha256()
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    if (header[:7] != b"\x7fELF\x02\x01\x01"
            or struct.unpack_from("<H", header, 18)[0] != machine):
        raise ValueError("cmsd ELF architecture mismatch")
    if digest.hexdigest() != manifest["sha256"]:
        raise ValueError("cmsd binary SHA-256 mismatch")
    # Check the actual payload even when this helper is used independently of
    # the whole-deb scan. readelf inspects bytes and never runs the daemon.
    subprocess.run([str(Path(__file__).resolve().with_name("check-linux-elf-policy.sh")),
                    str(binary), architecture], check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path, help="directory extracted with dpkg-deb -x")
    parser.add_argument("architecture", choices=[*ARCHITECTURES, "loongarch64"])
    args = parser.parse_args()
    try:
        validate(args.root, args.architecture)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"FAIL: bundled cmsd: {error}", file=sys.stderr)
        return 1
    print(f"PASS: bundled cmsd policy for {args.architecture}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

#!/usr/bin/env python3
"""Prune completed root-package files from bounded Cargo artifact layouts."""
import errno
from pathlib import Path
import re
import stat
import sys


PACKAGE_HASH = re.compile(r'[0-9a-f]{16}')
LEGACY_PACKAGE = re.compile(r'pinvou3-tauri-[0-9a-f]{16}')
LOCATIONS = 'deps and build/pinvou3-tauri[-<hash>]/[<hash>/]out'


def plain_directory(root, target):
    if not target.resolve().is_relative_to(root):
        raise ValueError('artifact directory escapes the job workspace')
    try:
        mode = target.lstat().st_mode
    except FileNotFoundError:
        return False
    if not stat.S_ISDIR(mode):
        raise NotADirectoryError(errno.ENOTDIR, 'artifact location is not a plain directory')
    return True


def artifact_directories(root):
    debug = root / 'pinvou3-app/src-tauri/target/debug'
    deps = debug / 'deps'
    if plain_directory(root, deps):
        yield deps
    build = debug / 'build'
    if not plain_directory(root, build):
        return
    for package in build.iterdir():
        if LEGACY_PACKAGE.fullmatch(package.name):
            if plain_directory(root, package):
                out = package / 'out'
                if plain_directory(root, out):
                    yield out
        elif package.name == 'pinvou3-tauri' and plain_directory(root, package):
            for generation in package.iterdir():
                if PACKAGE_HASH.fullmatch(generation.name) and plain_directory(root, generation):
                    out = generation / 'out'
                    if plain_directory(root, out):
                        yield out


def prune_directory(target):
    removed = 0
    for entry in target.iterdir():
        if entry.name.startswith(('pinvou3_lib-', 'libpinvou3_lib-')) and stat.S_ISREG(entry.lstat().st_mode):
            entry.unlink()
            removed += 1
    return removed


def prune_root_test_artifacts(workspace, *, report=None):
    root = Path(workspace).resolve(strict=True)
    removed, found = 0, False
    for target in artifact_directories(root):
        found = True
        count = prune_directory(target)
        removed += count
        if report is not None:
            report(target.relative_to(root).as_posix(), count)
    return removed if found else None


def main():
    try:
        removed = prune_root_test_artifacts(
            Path(__file__).resolve().parents[1],
            report=lambda location, count: print(f'Selected {location}; reclaimed {count} artifact files.'),
        )
    except (OSError, ValueError, RuntimeError):
        print('Root test artifact cleanup failed; no later harness is authorized by this step.', file=sys.stderr)
        return 1
    if removed is None:
        print(f'No configured artifact directory found ({LOCATIONS}); no files reclaimed.')
    else:
        print(f'Reclaimed {removed} completed root-package artifact files.')
        if removed == 0:
            print(f'No matching regular files in {LOCATIONS}; custom targets are not searched.')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())

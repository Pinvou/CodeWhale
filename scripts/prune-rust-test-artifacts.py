#!/usr/bin/env python3
"""Prune only completed root-package files at the existing CI deps location."""
import errno
from pathlib import Path
import stat
import sys


def prune_root_test_artifacts(workspace):
    root = Path(workspace).resolve(strict=True)
    target = root / 'pinvou3-app/src-tauri/target/debug/deps'
    if not target.resolve().is_relative_to(root):
        raise ValueError('artifact directory escapes the job workspace')
    try:
        mode = target.lstat().st_mode
    except FileNotFoundError:
        # Cargo may have produced artifacts elsewhere. This says only that
        # the existing cleanup location is absent; it does not adopt caches.
        return None
    if not stat.S_ISDIR(mode):
        raise NotADirectoryError(errno.ENOTDIR, 'artifact location is not a plain directory')
    removed = 0
    for entry in target.iterdir():
        if entry.name.startswith(('pinvou3_lib-', 'libpinvou3_lib-')) and stat.S_ISREG(entry.lstat().st_mode):
            entry.unlink()
            removed += 1
    return removed


def main():
    try:
        removed = prune_root_test_artifacts(Path(__file__).resolve().parents[1])
    except (OSError, ValueError, RuntimeError):
        print('Root test artifact cleanup failed; no later harness is authorized by this step.', file=sys.stderr)
        return 1
    if removed is None:
        print('Configured root test artifact directory is absent; no files reclaimed.')
    else:
        print(f'Reclaimed {removed} completed root-package artifact files.')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())

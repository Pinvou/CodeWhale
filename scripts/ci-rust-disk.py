#!/usr/bin/env python3
"""Reserve disk for both Rust test harnesses on disposable GitHub Linux runners.

Only the fixed, unused Android and .NET SDKs may be removed. Never clean the
checkout, Cargo cache, toolchain, or build outputs: the later telemetry harness
still needs its own root library plus the dependency artifacts.
"""

import os
from pathlib import Path
import shutil
import sys


SDK_PATHS = (Path("/usr/local/lib/android"), Path("/usr/share/dotnet"))
MIN_FREE_BYTES = 12 * 1024**3


def prepare_disk(workspace: Path) -> None:
    if sys.platform != "linux" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted":
        raise RuntimeError("Disk preparation is restricted to GitHub-hosted Linux runners")
    workspace = workspace.resolve(strict=True)
    if not workspace.is_dir() or not (workspace / ".github/workflows/pr-check.yml").is_file():
        raise RuntimeError("GITHUB_WORKSPACE must be the checked-out repository")

    # Validate the entire allowlist before deleting anything, including parent
    # symlinks and overlapping workspace paths. Missing SDKs are harmless.
    targets = []
    for sdk in SDK_PATHS:
        if sdk.resolve() != sdk or sdk.is_symlink():
            raise RuntimeError(f"Refusing redirected SDK path: {sdk}")
        if workspace == sdk or workspace in sdk.parents or sdk in workspace.parents:
            raise RuntimeError(f"Refusing SDK path overlapping the workspace: {sdk}")
        if sdk.exists():
            if not sdk.is_dir() or sdk.is_mount():
                raise RuntimeError(f"Refusing unexpected SDK path: {sdk}")
            targets.append(sdk)

    before = shutil.disk_usage(workspace).free
    print(f"Rust CI disk available before preparation: {before / 1024**3:.1f} GiB", flush=True)
    for sdk in targets:
        print(f"Removing unused hosted-runner SDK: {sdk}", flush=True)
        shutil.rmtree(sdk)
    after = shutil.disk_usage(workspace).free
    print(f"Rust CI disk available after preparation: {after / 1024**3:.1f} GiB", flush=True)
    if after < MIN_FREE_BYTES:
        raise RuntimeError(
            f"Rust tests require at least {MIN_FREE_BYTES / 1024**3:.0f} GiB free "
            "after dependencies, caches and swap are prepared; refusing an undersized runner"
        )


def main() -> int:
    try:
        workspace = os.environ.get("GITHUB_WORKSPACE", "").strip()
        if not workspace:
            raise RuntimeError("GITHUB_WORKSPACE is required")
        prepare_disk(Path(workspace))
    except (OSError, RuntimeError) as error:
        print(f"Rust CI disk preparation failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

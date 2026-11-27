#!/usr/bin/env python3
"""Publish retained Portal build assets to a shared origin's document root."""

import argparse
import os
from pathlib import Path
import re
import shutil
import tempfile


ASSET_NAME = re.compile(
    r"[A-Za-z0-9_-]+-[A-Za-z0-9_-]{8,}\.(?:js|css|svg|png|jpe?g|webp|gif|avif|ico|woff2?|ttf|otf|wasm)"
)


def asset_files(directory):
    if directory.is_symlink() or not directory.is_dir():
        raise ValueError("release must have a regular assets directory")
    files = sorted(directory.iterdir())
    if not files:
        raise ValueError("release contains no Portal assets")
    for asset in files:
        if asset.is_symlink() or not asset.is_file() or not ASSET_NAME.fullmatch(asset.name):
            raise ValueError(f"not a regular, hashed Portal asset: {asset.name}")
    return files


def matches(source, target):
    if target.is_symlink() or not target.is_file():
        return False
    with source.open("rb") as expected, target.open("rb") as published:
        for chunk in iter(lambda: expected.read(64 * 1024), b""):
            if published.read(len(chunk)) != chunk:
                return False
        return published.read(1) == b""


def publish_assets(release, document_root, *, check):
    files = asset_files(release / "assets")
    destination = document_root / "assets"
    if (document_root.is_symlink() or not document_root.is_dir()
            or destination.is_symlink() or not destination.is_dir()):
        raise ValueError("provision a dedicated origin root with an ordinary assets directory first")

    for source in files:
        target = destination / source.name
        if target.exists() or target.is_symlink():
            if not matches(source, target):
                raise ValueError(f"asset name already contains different bytes: {source.name}")
        elif check:
            raise ValueError(f"asset not published: {source.name}")
        else:
            # The shared filesystem must support atomic, no-replace links.
            # A concurrent publisher can win only with identical content.
            with tempfile.NamedTemporaryFile(dir=destination, prefix=".portal-", delete=False) as stage:
                stage_path = Path(stage.name)
                try:
                    with source.open("rb") as payload:
                        shutil.copyfileobj(payload, stage)
                    stage.flush()
                    os.fsync(stage.fileno())
                    stage.close()
                    stage_path.chmod(0o644)
                    try:
                        os.link(stage_path, target)
                    except FileExistsError:
                        if not matches(source, target):
                            raise ValueError(
                                f"concurrent publish contains different bytes: {source.name}"
                            ) from None
                finally:
                    stage.close()
                    stage_path.unlink()
    return len(files)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("release", type=Path, help="static directory from a trusted Portal build")
    parser.add_argument("document_root", type=Path, help="writable mount of the origin's document root")
    parser.add_argument("--check", action="store_true", help="verify bytes without publishing")
    arguments = parser.parse_args()
    try:
        count = publish_assets(arguments.release, arguments.document_root, check=arguments.check)
    except (OSError, ValueError) as error:
        parser.exit(1, f"Portal assets: {error}\n")
    print(f"{'Verified' if arguments.check else 'Published'} {count} Portal assets")


if __name__ == "__main__":
    main()

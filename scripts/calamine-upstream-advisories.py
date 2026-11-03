#!/usr/bin/env python3
"""Scan the reviewed Git calamine under its upstream registry advisory identity.

Run after the ordinary four-category cargo-deny check. Only the temporary audit
metadata changes identity; production manifests, locks and sources remain inputs.
"""

import argparse
import copy
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[1]
MANIFESTS = ("pinvou-knowledge/Cargo.toml", "pinvou3-app/src-tauri/Cargo.toml")
REVISION = "54d55a113b309c5d7ab9a4a6d6b2dd8d7320ebea"
GIT_SOURCE = f"git+https://github.com/zhuowp/calamine.git?rev={REVISION}"
REGISTRY = "registry+https://github.com/rust-lang/crates.io-index"


def project_upstream(metadata):
    """Preserve the resolved graph while changing exactly one package identity."""
    projected = copy.deepcopy(metadata)
    packages = projected["packages"]
    matches = [package for package in packages if package["name"] == "calamine"]
    if len(matches) != 1:
        raise ValueError("expected exactly one resolved calamine package")
    package = matches[0]
    if package["source"] != f"{GIT_SOURCE}#{REVISION}":
        raise ValueError("calamine is not from the reviewed Git source and full revision")
    version = package["version"]
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?", version):
        raise ValueError("invalid calamine version")
    original_id = package["id"]
    if original_id != f"{GIT_SOURCE}#calamine@{version}":
        raise ValueError("calamine package ID disagrees with its source or version")
    upstream_id = f"{REGISTRY}#calamine@{version}"
    if any(row["id"] == upstream_id for row in packages):
        raise ValueError("upstream calamine identity already exists")
    if original_id in projected["workspace_members"]:
        raise ValueError("calamine must be a dependency, not a workspace member")
    nodes = projected["resolve"]["nodes"]
    if sum(node["id"] == original_id for node in nodes) != 1:
        raise ValueError("expected exactly one resolved calamine node")

    declarations = 0
    for parent in packages:
        for dependency in parent["dependencies"]:
            if dependency["name"] == "calamine":
                if dependency["source"] != GIT_SOURCE or dependency.get("path"):
                    raise ValueError("calamine dependency declaration has an unexpected source")
                dependency["source"] = REGISTRY
                declarations += 1
    if declarations == 0:
        raise ValueError("calamine has no parent dependency declaration")
    package["id"] = upstream_id
    package["source"] = REGISTRY
    for node in nodes:
        if node["id"] == original_id:
            node["id"] = upstream_id
        node["dependencies"] = [
            upstream_id if identity == original_id else identity
            for identity in node["dependencies"]
        ]
        for dependency in node["deps"]:
            if dependency["pkg"] == original_id:
                dependency["pkg"] = upstream_id
    return projected


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest-path", required=True, choices=MANIFESTS)
    args = parser.parse_args(argv)
    try:
        result = subprocess.run(
            ["cargo", "metadata", "--locked", "--all-features", "--format-version", "1",
             "--manifest-path", args.manifest_path],
            cwd=ROOT, check=True, stdout=subprocess.PIPE, text=True, encoding="utf-8",
        )
        projected = project_upstream(json.loads(result.stdout))
        with tempfile.TemporaryDirectory(prefix="calamine-upstream-advisories-") as temporary:
            metadata_path = Path(temporary) / "metadata.json"
            metadata_path.write_text(json.dumps(projected), encoding="utf-8")
            result = subprocess.run(
                ["cargo", "deny", "--locked", "--all-features",
                 "--manifest-path", args.manifest_path,
                 "--metadata-path", str(metadata_path),
                 "--config", "pinvou3-app/src-tauri/deny.toml", "check", "advisories"],
                cwd=ROOT, check=True,
            )
    except subprocess.CalledProcessError as error:
        return error.returncode if error.returncode > 0 else 1
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(f"calamine upstream advisory check failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

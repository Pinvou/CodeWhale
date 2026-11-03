"""Fetch both complete locked wheel sets for the Linux PDF CI test gate."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import subprocess
import sys

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("prune_pdf_lxml", HERE / "prune_pdf_lxml.py")
prune = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prune)
image_spec = importlib.util.spec_from_file_location("prepare_pdf_image", HERE / "prepare_pdf_image.py")
image = importlib.util.module_from_spec(image_spec)
image_spec.loader.exec_module(image)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if set(prune.WHEELS) != {"x86_64", "aarch64"}:
        raise ValueError("both lxml architecture bindings are required")
    root = args.output.absolute()
    root.mkdir(parents=False, exist_ok=False)
    lines = [line for line in (HERE / "pdf-requirements.lock").read_text().splitlines() if line.startswith("lxml==")]
    if len(lines) != 1 or not lines[0].startswith("lxml==6.1.3 "):
        raise ValueError("unexpected lxml lock")
    hashes = re.findall(r"--hash=sha256:([0-9a-f]{64})", lines[0])
    if len(hashes) != 2 or set(hashes) != {value[1] for value in prune.WHEELS.values()}:
        raise ValueError("lxml wheel bindings do not match lock")
    bindings = json.loads((HERE / "pdf-wheel-bindings.json").read_text(encoding="utf-8"))
    pairs = {(entry["platform"], entry["name"]) for entry in bindings}
    if len(bindings) != 8 or pairs != {(arch, name) for arch in prune.WHEELS for name in image.PACKAGES}:
        raise ValueError("both complete image wheel sets are required")
    lock = root / "lxml.lock"
    lock.write_text(lines[0] + "\n", encoding="utf-8")
    for arch, (filename, expected) in prune.WHEELS.items():
        selected = [entry for entry in bindings if entry["platform"] == arch]
        lxml = next(entry for entry in selected if entry["name"] == "lxml")
        if (lxml["filename"], lxml["sha256"]) != (filename, expected):
            raise ValueError("image and pruning lxml bindings differ")
        dest = root / arch
        dest.mkdir()
        subprocess.run([sys.executable, "-I", "-B", "-m", "pip", "--isolated", "download",
                        "--index-url", "https://pypi.org/simple", "--only-binary=:all:",
                        "--require-hashes", "--no-deps", "--platform", "manylinux_2_28_" + arch,
                        "--python-version", "3.12", "--implementation", "cp", "--abi", "cp312",
                        "--dest", str(dest), "-r", str(HERE / "pdf-requirements.lock")], check=True, timeout=120)
        if {p.name for p in dest.iterdir()} != {entry["filename"] for entry in selected}:
            raise ValueError("downloaded wheel set differs from its architecture binding")
        for entry in selected:
            wheel = dest / entry["filename"]
            if not wheel.is_file() or hashlib.sha256(wheel.read_bytes()).hexdigest() != entry["sha256"]:
                raise ValueError("downloaded wheel does not match its architecture binding")
    (root / "manifest.json").write_text(json.dumps({"wheels": bindings}), encoding="utf-8")
    for platform in image.ARCHES:
        image.checked_wheels(root, platform)
    print("both complete locked PDF wheel audits and the pruning lock prepared")


if __name__ == "__main__":
    main()

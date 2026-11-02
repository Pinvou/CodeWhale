"""Fetch the two locked official lxml wheels for the Linux CI test gate."""
import argparse
import hashlib
import importlib.util
from pathlib import Path
import re
import subprocess
import sys

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("prune_pdf_lxml", HERE / "prune_pdf_lxml.py")
prune = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prune)


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
    lock = root / "lxml.lock"
    lock.write_text(lines[0] + "\n", encoding="utf-8")
    for arch, (filename, expected) in prune.WHEELS.items():
        dest = root / arch
        dest.mkdir()
        subprocess.run([sys.executable, "-I", "-B", "-m", "pip", "--isolated", "download",
                        "--index-url", "https://pypi.org/simple", "--only-binary=:all:",
                        "--require-hashes", "--no-deps", "--platform", "manylinux_2_28_" + arch,
                        "--python-version", "3.12", "--implementation", "cp", "--abi", "cp312",
                        "--dest", str(dest), "-r", str(lock)], check=True, timeout=120)
        wheel = dest / filename
        if not wheel.is_file() or set(dest.iterdir()) != {wheel} or hashlib.sha256(wheel.read_bytes()).hexdigest() != expected:
            raise ValueError("downloaded lxml wheel does not match its architecture binding")
    print("both locked lxml wheels prepared for the CI test gate")


if __name__ == "__main__":
    main()

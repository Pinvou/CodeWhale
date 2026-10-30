#!/usr/bin/env python3
"""Install the CI dev-profile override without accumulating duplicate tables."""
import os
from pathlib import Path
import re
import sys
import tempfile

HEADER = "[profile.dev.package.codewhale-tui]"
PAYLOAD = HEADER + "\nopt-level = 0\n"


def configure(path: Path, payload: str) -> None:
    meaningful = lambda text: [line.strip() for line in text.splitlines()
                               if line.strip() and not line.lstrip().startswith("#")]
    if meaningful(payload) != meaningful(PAYLOAD):
        raise ValueError("unexpected CI dev-profile payload")
    original = path.read_text(encoding="utf-8") if path.exists() else ""
    lines = original.splitlines(keepends=True)
    retained = []
    index = 0
    while index < len(lines):
        if re.fullmatch(r"\s*\[profile\.dev\.package\.codewhale-tui\]\s*(?:#.*)?", lines[index]):
            end = index + 1
            while end < len(lines) and not lines[end].lstrip().startswith("["):
                end += 1
            settings = meaningful("".join(lines[index + 1:end]))
            if len(settings) != 1 or not re.fullmatch(r"opt-level\s*=\s*0\s*(?:#.*)?", settings[0]):
                raise ValueError("refusing to replace custom settings in the CI dev-profile section")
            index = end
        else:
            retained.append(lines[index])
            index += 1
    prefix = "".join(retained).rstrip()
    updated = (prefix + "\n\n" if prefix else "") + PAYLOAD
    path.parent.mkdir(parents=True, exist_ok=True)
    mode = path.stat().st_mode & 0o777 if path.exists() else 0o600
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", newline="\n",
                                         dir=path.parent, delete=False) as output:
            temporary = Path(output.name)
            output.write(updated)
        os.chmod(temporary, mode)
        os.replace(temporary, path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


if __name__ == "__main__":
    configure(Path(sys.argv[1]), sys.stdin.read())

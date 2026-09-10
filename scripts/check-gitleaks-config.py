#!/usr/bin/env python3
"""Prove fixture exceptions do not exempt whole files or matching values elsewhere."""

import json
from pathlib import Path
import subprocess
import sys
import tempfile


def main():
    config = Path(__file__).resolve().parents[1] / ".gitleaks.toml"
    with tempfile.TemporaryDirectory(prefix="gitleaks-config-") as directory:
        root = Path(directory)
        fixture = root / "crates/tui/src/lib.rs"
        fixture.parent.mkdir(parents=True)
        # Artificial markers only; neither value is a usable credential.
        allowed = "sk-live-abcdef0123456789abcdef"
        changed = "sk-live-abcdef0123456789abcdea"
        fixture.write_text(
            f'let api_key = "{allowed}";\nlet api_key = "{changed}";\n'
        )
        (root / "unrelated.rs").write_text(f'let api_key = "{allowed}";\n')
        report = root / "report.json"
        result = subprocess.run(
            [sys.argv[1], "dir", ".", "--config", str(config), "--redact",
             "--no-banner", "--report-format", "json", "--report-path", str(report)],
            cwd=root,
            check=False,
        )
        if result.returncode != 1:
            raise SystemExit("Negative control must report leaks (exit 1)")
        findings = json.loads(report.read_text())
        locations = {(item["File"], item["StartLine"]) for item in findings}
        if len(findings) != 2 or locations != {
            ("crates/tui/src/lib.rs", 2), ("unrelated.rs", 1)
        }:
            raise SystemExit("Fixture exceptions must match both path and exact value")
        print("PASS: exact fixture allowed; changed value and unrelated path blocked")


if __name__ == "__main__":
    main()

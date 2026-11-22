"""Run the real PDF child regression; strict mode fails on missing coverage."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

HERE = Path(__file__).resolve().parent
PLATFORM = HERE.parent.parent
FIXTURES = HERE / "testdata" / "pdf"
CASES = {
    "plain-compressed.pdf": "937d06f2cd68336862c14890a07debd3d05837b65c98382c5b047cfe1cc1f85b",
    "empty-user-owner.pdf": "741f4fe624d726a701ad357888a489157efebcbc052f794dea7bd3402a38b0ad",
    "user-password.pdf": "66f99261725c00b1a6afe6d6fe834509e2d5dd227cce9c8d60b24fd156a2df91",
    "truncated.pdf": "f9fb9990ddea1eaac1202764de41c50228c0b0fc9aa1ac7b0b02c53f6776703e",
    "incremental.pdf": "556a298a0d7b43b8bff3d84e452ca293083b8b105a8ef45c90a21be54a4550ce",
}
ROOT_TEST = "TestPDFCandidateRealSamples"


def check_python_result(output):
    result = json.loads(output)
    fields = {"tests", "skipped", "failures", "errors", "expected_failures", "unexpected_successes"}
    if not isinstance(result, dict) or set(result) != fields or any(type(result[k]) is not int for k in fields):
        raise ValueError("invalid Python test report")
    if result["tests"] <= 0 or any(result[k] != 0 for k in fields - {"tests"}):
        raise ValueError("Python test suite missing, skipped, or failed")
    return result["tests"]


def check_results(output):
    expected = {ROOT_TEST} | {ROOT_TEST + "/" + name for name in CASES}
    counts = {name: {"run": 0, "pass": 0} for name in expected}
    package_pass = 0
    for line in output.splitlines():
        event = json.loads(line)
        action, name = event.get("Action"), event.get("Test")
        if action in ("skip", "fail"):
            raise ValueError("PDF Go test reported skip or failure")
        if name in counts and action in ("run", "pass"):
            counts[name][action] += 1
        if not name and action == "pass":
            package_pass += 1
    if package_pass != 1 or any(c != {"run": 1, "pass": 1} for c in counts.values()):
        raise ValueError("PDF Go test coverage missing or duplicated")


def go_failure_summary(output):
    # Test output can contain file paths or other sensitive data. Report only
    # fixed, known fixture names and event types, never arbitrary Go output.
    allowed = {ROOT_TEST} | {ROOT_TEST + "/" + name for name in CASES}
    found = set()
    for line in output.splitlines():
        try:
            event = json.loads(line)
        except (ValueError, TypeError):
            continue
        if not isinstance(event, dict):
            continue
        name, action = event.get("Test"), event.get("Action")
        if isinstance(name, str) and name in allowed and action in ("fail", "skip"):
            found.add((name, action))
    if not found:
        return "no classified failing test event; raw output withheld"
    return ", ".join(f"{action}: {name}" for name, action in sorted(found))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--strict", action="store_true", help="fail if runtime or tests are unavailable")
    args = parser.parse_args()
    python = os.environ.get("EVIDENCE_PDF_TEST_PYTHON", sys.executable)
    go = os.environ.get("EVIDENCE_PDF_GO", "go")
    python = shutil.which(python) or python
    try:
        probe = subprocess.run([python, "-I", "-B", "-c", 'import pikepdf; assert pikepdf.__version__ == "10.17.0" and pikepdf.__libqpdf_version__ == "12.4.2"'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
        ready = probe.returncode == 0 and shutil.which(go) is not None
    except (OSError, subprocess.SubprocessError):
        ready = False
    if not ready:
        if args.strict:
            raise RuntimeError("required PDF runtime or Go tool is unavailable")
        print("PDF gate skipped: local runtime or Go tool unavailable")
        return
    for name, digest in CASES.items():
        if hashlib.sha256((FIXTURES / name).read_bytes()).hexdigest() != digest:
            raise ValueError("PDF fixture hash mismatch: " + name)
    tests = ["test_pdf_gate.py", "test_pdf_validator.py"]
    test_env = os.environ.copy()
    if args.strict:
        if not test_env.get("EVIDENCE_PDF_AUDIT_DIR"):
            raise RuntimeError("strict PDF gate requires both audited lxml wheels")
        test_env["EVIDENCE_PDF_REQUIRE_SYMLINK"] = "1"
        tests.append("test_prune_pdf_lxml.py")
        tests.append("test_pdf_image_candidate.py")
    for test in tests:
        report = subprocess.run([python, "-I", "-B", str(HERE / "run_pdf_unittest.py"), str(HERE / test)], cwd=HERE, env=test_env, check=True, stdout=subprocess.PIPE, text=True, timeout=120)
        print(test, "passed:", check_python_result(report.stdout), "tests")
    env = os.environ.copy()
    # Preserve the venv executable path itself: resolving a POSIX symlink to
    # the system interpreter bypasses pyvenv.cfg and the installed packages.
    env.update(EVIDENCE_PDF_TEST_PYTHON=os.path.abspath(python), EVIDENCE_PDF_TEST_FIXTURES=str(FIXTURES))
    run = subprocess.run([go, "test", "-json", "-count=1", "./cmd/evidence-image-validator", "-run", "^" + ROOT_TEST + "$"], cwd=PLATFORM, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=180)
    if run.returncode:
        raise RuntimeError(f"PDF Go tests failed (exit {run.returncode}; {go_failure_summary(run.stdout)})")
    check_results(run.stdout)
    if args.strict:
        print("strict PDF gate passed: pruning tests and five real child cases, no skipped tests")
    else:
        print("local PDF tests passed: five real child cases; strict pruning tests not selected")


if __name__ == "__main__":
    main()

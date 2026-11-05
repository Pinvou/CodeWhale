"""Run a single test module and emit a bounded machine-readable summary."""
import importlib.util
import io
import json
from pathlib import Path
import sys
import unittest


def run_suite(module):
    result = unittest.TextTestRunner(stream=io.StringIO()).run(unittest.defaultTestLoader.loadTestsFromModule(module))
    return {"tests": result.testsRun, "skipped": len(result.skipped),
            "failures": len(result.failures), "errors": len(result.errors),
            "expected_failures": len(result.expectedFailures),
            "unexpected_successes": len(result.unexpectedSuccesses)}


def main():
    if len(sys.argv) != 2:
        raise ValueError("one test module is required")
    path = Path(sys.argv[1]).resolve(strict=True)
    if path.parent != Path(__file__).resolve().parent or path.name not in {"test_pdf_gate.py", "test_pdf_validator.py", "test_prune_pdf_lxml.py", "test_pdf_image_candidate.py"}:
        raise ValueError("unexpected test module")
    spec = importlib.util.spec_from_file_location("pdf_gate_test_module", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    print(json.dumps(run_suite(module)))


if __name__ == "__main__":
    main()

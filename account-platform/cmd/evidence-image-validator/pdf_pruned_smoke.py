"""Build-time PDF regression for the dedicated, pruned image candidate."""
import hashlib
import importlib
import importlib.util
from pathlib import Path
import sys

FIXTURES = {
    "plain-compressed.pdf": ("937d06f2cd68336862c14890a07debd3d05837b65c98382c5b047cfe1cc1f85b", 1),
    "empty-user-owner.pdf": ("741f4fe624d726a701ad357888a489157efebcbc052f794dea7bd3402a38b0ad", 1),
    "user-password.pdf": ("66f99261725c00b1a6afe6d6fe834509e2d5dd227cce9c8d60b24fd156a2df91", 4),
    "truncated.pdf": ("f9fb9990ddea1eaac1202764de41c50228c0b0fc9aa1ac7b0b02c53f6776703e", 2),
    "incremental.pdf": ("556a298a0d7b43b8bff3d84e452ca293083b8b105a8ef45c90a21be54a4550ce", 2),
}


def main():
    if len(sys.argv) != 3:
        raise ValueError("expected validator and fixture paths")
    import lxml.etree  # noqa: F401
    try:
        importlib.import_module("lxml.isoschematron")
    except OSError:
        pass
    else:
        raise RuntimeError("pruned Schematron unexpectedly available")
    spec = importlib.util.spec_from_file_location("pdf_validator", sys.argv[1])
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    for name, (sha, expected) in FIXTURES.items():
        data = (Path(sys.argv[2]) / name).read_bytes()
        if hashlib.sha256(data).hexdigest() != sha or module.validate(data) != expected:
            raise RuntimeError("PDF smoke case failed: " + name)
    print("five pruned-image PDF smoke cases passed")


if __name__ == "__main__":
    main()

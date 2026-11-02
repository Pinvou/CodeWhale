"""Verify and prune two lxml resources for the dedicated PDF image only."""
import base64
import csv
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import platform
import stat
import sys
import zipfile

VERSION = "6.1.3"
WHEELS = {
    "x86_64": ("lxml-6.1.3-cp312-cp312-manylinux_2_26_x86_64.manylinux_2_28_x86_64.whl", "909f4e927bb051f7740d6367285fc60cdcfdaf0258c2dba4ff5ba7eadadc250c"),
    "aarch64": ("lxml-6.1.3-cp312-cp312-manylinux_2_26_aarch64.manylinux_2_28_aarch64.whl", "b37772102d44bb6628186accca3a121b1fa3a6b3d97518a8c29a5229ca4c0d0a"),
}
REMOVED = {
    "lxml/isoschematron/resources/xsl/RNG2Schtrn.xsl": "39b79bb01f16b7e777b8803f539354f394e99d0dcfc4f5f7f1374e02aa2c31a7",
    "lxml/isoschematron/resources/xsl/XSD2Schtrn.xsl": "430791ac820cfb315c820f7c19703609a59f21b8151345ca11e6127efbfaec0d",
}
DIST = "lxml-6.1.3.dist-info/"
RECORD = DIST + "RECORD"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def safe_file(root, name):
    p = PurePosixPath(name)
    if not name or p.is_absolute() or ".." in p.parts or "\\" in name or str(p) != name:
        raise ValueError("unsafe installed path")
    current = root
    for part in p.parts:
        current = current / part
        if current.is_symlink():
            raise ValueError("symlink in installed path")
    if not stat.S_ISREG(current.stat().st_mode):
        raise ValueError("installed path is not a regular file")
    return current


def rows(data):
    entries = list(csv.reader(io.StringIO(data.decode("utf-8"), newline="")))
    if any(len(r) != 3 for r in entries) or len({r[0] for r in entries}) != len(entries):
        raise ValueError("invalid RECORD")
    return {r[0]: r for r in entries}


def verify_installed(site, entries):
    if RECORD not in entries or entries[RECORD][1:] != ["", ""]:
        raise ValueError("invalid RECORD self entry")
    for name, (_, h, size) in entries.items():
        if not (name.startswith("lxml/") or name.startswith(DIST)):
            raise ValueError("unexpected installed path")
        path = safe_file(site, name)
        if name == RECORD:
            continue
        if not h.startswith("sha256=") or not size.isdecimal():
            raise ValueError("unverifiable installed record")
        data = path.read_bytes()
        expected = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode("ascii")
        if h != "sha256=" + expected or int(size) != len(data):
            raise ValueError("installed file mismatch")
    for dirname in ("lxml", DIST.rstrip("/")):
        root = site / dirname
        if root.is_symlink() or not root.is_dir():
            raise ValueError("unsafe package directory")
        for base, dirs, files in os.walk(root, followlinks=False):
            for leaf in dirs + files:
                path = Path(base) / leaf
                if path.is_symlink():
                    raise ValueError("unexpected package symlink")
                if path.is_file() and path.relative_to(site).as_posix() not in entries:
                    raise ValueError("unrecorded installed file")


def prepare_audit_dir(site, audit_dir):
    audit_dir = Path(audit_dir)
    if not audit_dir.is_absolute() or ".." in audit_dir.parts:
        raise ValueError("unsafe audit path")
    if audit_dir == site or site in audit_dir.parents:
        raise ValueError("audit directory must not be inside the installation")
    if audit_dir.exists() or audit_dir.is_symlink():
        raise ValueError("audit directory already exists")
    # Validate all existing ancestors without following symlinks. Create and
    # probe the destination before deleting either installed resource.
    for parent in reversed(audit_dir.parents):
        if parent.is_symlink():
            raise ValueError("symlink in audit path")
        if parent.exists() and not parent.is_dir():
            raise ValueError("audit parent is not a directory")
    audit_dir.mkdir(parents=True, exist_ok=False)
    probe = audit_dir / ".write-probe"
    with probe.open("xb") as stream:
        stream.write(b"audit preflight\n")
        stream.flush()
    probe.unlink()
    return audit_dir


def prune(site, wheel, architecture, audit_dir):
    site = Path(site).resolve(strict=True)
    wheel = Path(wheel)
    if architecture not in WHEELS or wheel.name != WHEELS[architecture][0] or wheel.is_symlink() or digest(wheel.read_bytes()) != WHEELS[architecture][1]:
        raise ValueError("untrusted source wheel")
    record_path = safe_file(site, RECORD)
    original_install_record = record_path.read_bytes()
    installed = rows(original_install_record)
    verify_installed(site, installed)
    with zipfile.ZipFile(wheel) as archive:
        wheel_record_bytes = archive.read(RECORD)
        wheel_records = rows(wheel_record_bytes)
        if not set(wheel_records).issubset(installed):
            raise ValueError("source RECORD entries missing")
        generated = set(installed) - set(wheel_records)
        if not generated.issubset({DIST + "INSTALLER", DIST + "REQUESTED"}):
            raise ValueError("unexpected installer entries")
        if DIST + "INSTALLER" in generated and safe_file(site, DIST + "INSTALLER").read_bytes() != b"pip\n":
            raise ValueError("unexpected installer")
        if DIST + "REQUESTED" in generated and safe_file(site, DIST + "REQUESTED").read_bytes() != b"":
            raise ValueError("unexpected request marker")
        for name, row in wheel_records.items():
            if name != RECORD and installed[name] != row:
                raise ValueError("source and installed RECORD differ")
        for name, expected in REMOVED.items():
            if name not in wheel_records or digest(archive.read(name)) != expected or digest(safe_file(site, name).read_bytes()) != expected:
                raise ValueError("unexpected resource content")
    audit_dir = prepare_audit_dir(site, audit_dir)
    for name in REMOVED:
        safe_file(site, name).unlink()
        del installed[name]
    out = io.StringIO(newline="")
    writer = csv.writer(out, lineterminator="\n")
    writer.writerows(installed.values())
    record_path.write_text(out.getvalue(), encoding="utf-8", newline="")
    verify_installed(site, rows(record_path.read_bytes()))
    (audit_dir / "lxml-original-wheel.RECORD").write_bytes(wheel_record_bytes)
    (audit_dir / "lxml-original-installed.RECORD").write_bytes(original_install_record)
    (audit_dir / "lxml-pruning.json").write_text(json.dumps({
        "package": "lxml", "version": VERSION, "architecture": architecture,
        "original_wheel": wheel.name, "original_wheel_sha256": WHEELS[architecture][1],
        "removed_files": REMOVED, "installed_record_before_sha256": digest(original_install_record),
        "installed_record_after_sha256": digest(record_path.read_bytes()),
        "scope": "dedicated PDF image; isoschematron import is not supported",
        "original_wheel_retained": "external audited build input; not included in runtime image",
    }, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    (audit_dir / "NOTICE-pruning.txt").write_text(
        "Dedicated PDF validator image: lxml 6.1.3 was modified after installing "
        "the verified official wheel by removing RNG2Schtrn.xsl and XSD2Schtrn.xsl. "
        "The isoschematron module is unsupported. Original wheel and installed "
        "RECORDs and exact removed-file hashes are retained here for audit. "
        "The unmodified source wheel is retained outside the runtime image.\n",
        encoding="utf-8",
    )
    (audit_dir / "lxml-pruning-sbom-fragment.json").write_text(json.dumps({
        "bomFormat": "CycloneDX", "specVersion": "1.5", "version": 1,
        "components": [{"type": "library", "name": "lxml", "version": VERSION,
                        "properties": [
                            {"name": "pinvou:scope", "value": "modified PDF-image fragment; not a complete SBOM"},
                            {"name": "pinvou:source-wheel-sha256", "value": WHEELS[architecture][1]},
                            {"name": "pinvou:removed-resources", "value": json.dumps(REMOVED, sort_keys=True)},
                        ]}],
    }, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def main():
    if len(sys.argv) != 3:
        raise ValueError("expected wheelhouse and audit directory")
    arch = platform.machine().lower()
    if arch not in WHEELS:
        raise ValueError("unsupported architecture")
    site = Path(sys.prefix) / "lib" / "python3.12" / "site-packages"
    prune(site, Path(sys.argv[1]) / WHEELS[arch][0], arch, sys.argv[2])


if __name__ == "__main__":
    main()

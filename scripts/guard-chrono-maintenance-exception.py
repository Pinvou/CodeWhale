#!/usr/bin/env python3
"""Bound the temporary chrono maintenance advisory exception; no network/writes."""

from datetime import date, datetime, timezone
from pathlib import Path
import sys
try:
    import tomllib
except ModuleNotFoundError:
    sys.exit("chrono exception guard requires Python 3.11+ (tomllib)")


ADVISORY = "RUSTSEC-2026-0543"
EXPIRES = date(2026, 12, 17)  # Exclusive: fail from 00:00 UTC on this date.
LOCKFILES = (
    "pinvou-knowledge/Cargo.lock",
    "pinvou3-app/src-tauri/Cargo.lock",
    "pinvou-cli/Cargo.lock",
    "pinvou3-app/src-tauri/tests/controller-crypto-harness/Cargo.lock",
)
POLICY = "pinvou3-app/src-tauri/deny.toml"
VERSION = "0.4.45"
SOURCE = "registry+https://github.com/rust-lang/crates.io-index"
CHECKSUM = "1aa79e62e7697b8e29b513a68abacf485adcd1fe8284a4316c5ae868e6633327"


class GuardError(ValueError):
    pass


def read_toml(root, relative):
    try:
        with (root / relative).open("rb") as stream:
            return tomllib.load(stream)
    except (OSError, ValueError, RecursionError) as error:
        raise GuardError(f"{relative}: cannot read valid TOML") from error


def validate_repository(root, today=None):
    # Only unit tests inject a date. The CLI accepts no clock/lock selectors,
    # environment override, renewal switch or grace period.
    today = datetime.now(timezone.utc).date() if today is None else today
    if today >= EXPIRES:
        raise GuardError(f"{ADVISORY} exception expired at {EXPIRES} 00:00 UTC; migrate chrono and remove it")
    policy = read_toml(root, POLICY).get("advisories", {})
    if not isinstance(policy, dict):
        raise GuardError("invalid advisory policy table")
    if type(policy.get("version")) is not int or policy["version"] != 2:
        raise GuardError("expected reviewed advisory policy format 2")
    ignores = policy.get("ignore", [])
    if not isinstance(ignores, list):
        raise GuardError("invalid advisory ignore list")
    for item in ignores:
        if isinstance(item, str):
            continue
        if (not isinstance(item, dict) or set(item) - {"id", "reason"}
                or not isinstance(item.get("id"), str)
                or ("reason" in item and not isinstance(item["reason"], str))):
            raise GuardError("invalid advisory ignore entry; expected id and optional string reason")
    ids = [item if isinstance(item, str) else item["id"] for item in ignores]
    if ids != [ADVISORY]:
        raise GuardError("expected only the reviewed chrono advisory exception; remove guard together with exception after migration")
    if (policy.get("unmaintained") != "workspace"
            or policy.get("unsound") != "workspace"
            or policy.get("yanked") != "deny"
            or policy.get("unused-ignored-advisory") != "deny"):
        raise GuardError("existing advisory gates must remain enforced")
    for relative in LOCKFILES:
        lock = read_toml(root, relative)
        packages = lock.get("package")
        if type(lock.get("version")) is not int or lock["version"] != 4:
            raise GuardError(f"{relative}: unsupported lockfile format")
        if not isinstance(packages, list) or not all(isinstance(p, dict) for p in packages):
            raise GuardError(f"{relative}: invalid package inventory")
        chrono = [p for p in packages if p.get("name") == "chrono"]
        if (len(chrono) != 1 or chrono[0].get("version") != VERSION
                or chrono[0].get("source") != SOURCE
                or chrono[0].get("checksum") != CHECKSUM):
            raise GuardError(f"{relative}: expected exactly reviewed chrono {VERSION}, source and checksum; reassessment required")


def main():
    if len(sys.argv) != 1:
        print("chrono exception guard takes no arguments", file=sys.stderr)
        return 1
    try:
        validate_repository(Path(__file__).resolve().parents[1])
    except GuardError as error:
        print(f"chrono exception rejected: {error}; see docs/chrono-maintenance-exception.md", file=sys.stderr)
        return 1
    print(f"chrono {VERSION}: only {ADVISORY} excepted until {EXPIRES} 00:00 UTC; other advisories remain blocking")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

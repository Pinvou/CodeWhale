#!/usr/bin/env python3
"""Run the public Account/Billing/Token closure fixtures on disposable PG.

The Go fixtures own UUID databases and exclusively absent actors. This wrapper
never resets a cluster or drops actors; it rejects shared/deployed inputs and
requires both the parent and its actual child contracts to execute.
"""
import json
import os
from pathlib import Path
import runpy
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
local_database_url = runpy.run_path(str(ROOT / "scripts/test-token-financial-postgres.py"))["local_database_url"]
BILLING_ROOTS = {
    "TestClosedGrantPublishedOwnerEffects",
    "TestAccountClosureIsolationPreservesPriorAuditFacts",
    "TestAccountClosureIsolationScrubsContactWithoutFinancialMutation",
    "TestAccountClosurePreservesUnknownForcedRefundEvidence",
    "TestAccountClosureRequiresAuthoritativeChargebackTerminal",
}
CONTRACTS = [
    ("closure-combination", "./internal/billingschema", {"TestClosurePrivatePublicCombination"},
     {"": {"TestClosureClaimACKOrder"}}),
    ("closed-grants", "./internal/billingschema", {"TestClosedGrantPublicOwnerHarness"},
     {"": BILLING_ROOTS}),
    ("gift-closure", "./internal/billingschema", {"TestGiftClosurePublicHarness"},
     {"": {"TestGiftClosureProducerBoundaries"}, ".billing": {"TestClosedGrantPublishedOwnerEffects"}}),
    ("account-anonymization", "./internal/account",
     {"TestAccountClosureAnonymizesOwnedHistoricalContacts", "TestAccountClosureRollbackPreservesEvidence"}, {}),
]


def emit_child_logs(child_log, children):
    logs = {}
    for suffix in children:
        path = Path(str(child_log) + suffix)
        if path.is_file():
            logs[suffix] = path.read_text(encoding="utf-8")
            sys.stdout.write(logs[suffix])
    return logs


def verify_results(raw, required):
    passed = set()
    count = 0
    running = set()
    started_packages, passed_packages, test_packages = set(), set(), set()
    for line in raw.splitlines():
        if not line.startswith("{"):
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError as error:
            raise RuntimeError("incomplete Go JSON result stream") from error
        name, action = event.get("Test", ""), event.get("Action")
        package = event.get("Package", "")
        if action == "start" and package:
            started_packages.add(package)
        if not name and action == "pass" and package:
            passed_packages.add(package)
        if name:
            if not package:
                raise RuntimeError("Go test result is missing its package")
            test_packages.add(package)
            identity = (package, name)
            if action == "run":
                running.add(identity)
            elif action in {"pass", "skip", "fail"}:
                running.discard(identity)
        if action == "fail" or (name and action == "skip"):
            raise RuntimeError("closure contract failed or skipped")
        if name and action == "pass":
            count += 1
            if "/" not in name:
                passed.add(name)
    if (running or not passed_packages or started_packages - passed_packages
            or test_packages - passed_packages):
        raise RuntimeError("incomplete Go package or test lifecycle")
    if not required or not count or required - passed:
        raise RuntimeError("required closure contracts did not execute: " + ", ".join(sorted(required - passed)))
    return count


def main():
    raw = local_database_url(os.environ.get("PINVOU_ACCOUNT_CLOSURE_TEST_DATABASE_URL", ""))
    psql = os.environ.get("PINVOU_ACCOUNT_TEST_PSQL", "psql")
    go = shutil.which(os.environ.get("PINVOU_ACCOUNT_TEST_GO", "go"))
    if not go:
        raise RuntimeError("Go executable is required by the child fixtures")

    def query(sql):
        result = subprocess.run(
            [psql, raw, "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", sql],
            capture_output=True, text=True, encoding="utf-8", check=False, timeout=30,
        )
        if result.returncode:
            raise RuntimeError("disposable PostgreSQL operation failed")
        return result.stdout.strip()

    databases = "postgres\ntemplate0\ntemplate1"
    user = query("SELECT current_user")

    def require_empty():
        if (query("SELECT datname FROM pg_database ORDER BY datname") != databases
                or query("SELECT rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY rolname") != user):
            raise RuntimeError("database or actors exist; retain them for diagnosis, no setup writes permitted")
        objects = query("""SELECT
            (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
             WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema') +
            (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
             WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema')""")
        if objects != "0":
            raise RuntimeError("bootstrap database contains user objects; no setup writes permitted")

    require_empty()
    if int(query("SHOW server_version_num")) < 160000:
        raise RuntimeError("PostgreSQL 16 or newer is required by existing actor grants")
    artifact_root = os.environ.get("PINVOU_ACCOUNT_CLOSURE_TEST_ARTIFACT_DIR")
    if artifact_root:
        Path(artifact_root).mkdir(parents=True, exist_ok=True)
    artifacts = Path(tempfile.mkdtemp(prefix="account-closure-postgres-", dir=artifact_root))
    # Do not inherit a deployment test connection or a previous fixture's URLs.
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(("PINVOU_TEST_", "F08_")) and key != "F03_WIRE_FIXTURE"}
    env.update(PINVOU_TEST_SCHEMA_DATABASE_URL=raw, PINVOU_TEST_ACCOUNT_DATABASE_URL=raw,
               PINVOU_TEST_BILLING_DATABASE_URL=raw, F08_TEST_GO=go)
    for name, package, roots, children in CONTRACTS:
        require_empty()
        child_log = artifacts / (name + ".child.jsonl")
        argv = [go, "test", "-p=1", "-race", "-tags=nomsgpack", "-mod=readonly", "-count=1",
                "-timeout=8m", "-json", package, "-run", "^(" + "|".join(sorted(roots)) + ")$"]
        try:
            parent_log = artifacts / (name + ".parent.jsonl")
            stderr_log = artifacts / (name + ".stderr.log")
            try:
                result = subprocess.run(argv, cwd=ROOT / "account-platform",
                                        env={**env, "F08_TEST_CHILD_LOG": str(child_log)},
                                        capture_output=True, text=True, encoding="utf-8", check=False, timeout=540)
            except subprocess.TimeoutExpired as error:
                # TimeoutExpired can retain bytes even when text=True. Preserve
                # the partial stream without treating it as a successful run.
                parent_log.write_bytes(error.stdout if isinstance(error.stdout, bytes)
                                       else (error.stdout or "").encode("utf-8"))
                stderr_log.write_bytes(error.stderr if isinstance(error.stderr, bytes)
                                       else (error.stderr or "").encode("utf-8"))
                emit_child_logs(child_log, children)
                raise RuntimeError("closure fixture timed out; partial logs retained at " + str(artifacts)) from error
            parent_log.write_text(result.stdout, encoding="utf-8")
            stderr_log.write_text(result.stderr, encoding="utf-8")
            sys.stdout.write(result.stdout)
            sys.stderr.write(result.stderr)
            child_logs = emit_child_logs(child_log, children)
            if result.returncode:
                raise RuntimeError("closure PostgreSQL parent fixture failed; logs retained at " + str(artifacts))
            parent_count = verify_results(result.stdout, roots)
            child_counts = {}
            for suffix, required in children.items():
                if suffix not in child_logs:
                    raise RuntimeError("required child result log is missing: " + name + suffix)
                child_raw = child_logs[suffix]
                child_counts[suffix or "account_or_billing"] = verify_results(child_raw, required)
            print(json.dumps({"contract": name, "parent_named_passes": parent_count,
                              "child_named_passes": child_counts, "artifacts": str(artifacts)}))
        finally:
            # Go fixtures own cleanup. Unknown remnants are never force-removed.
            require_empty()


if __name__ == "__main__":
    main()

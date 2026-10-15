#!/usr/bin/env python3
"""Run existing Token financial contracts on an empty, disposable local PG.

The production fixtures create their own UUID databases and Token actors. Only
the three Billing deployment actors are bootstrapped here, using their existing
privilege contract. This runner must never accept a deployed database URL.
"""
import json
import os
from pathlib import Path
import subprocess
import sys
from urllib.parse import parse_qs, urlsplit

ROOT = Path(__file__).resolve().parents[1]
ACTORS = {"billing_migrator", "billing_schema_owner", "billing_service"}
REQUIRED = {
    "TestContextEligibilityUsesCurrentScopedProductionRoutes",
    "TestReconciliationEvidenceMatchesPublicSchema",
    "TestF05DispatchLeaseLifecycle",
    "TestF05IncompleteUpstreamErrorReplayPublicPostgres",
    "TestF05MissingReferenceDeadlineFundsPublicPostgres",
    "TestF05CancellationKeyScopePublicPostgres",
    "TestF05CancellationRecoveryReconciliationOwnership",
    "TestF05CancellationRecoveryClosurePublicPostgres",
    "TestF05RotationRetainedContentBoundaries",
    "TestF05UnsettledContentRetention",
    "TestF05ExpiredUnacquiredDispatchRecovery",
    "TestF05PersonalHistoryAndCancellationPersistedOwnerScope",
    "TestF05AdminUsageGateOffUsesSafeCollectionProjection",
    "TestPersonalExecutionSameKeyReplaysAtInflightLimit",
    "TestPersonalExecutionPublicPostgresFundsAndReplay",
    "TestManagementCancellationFundsRemainTruthful",
    "TestManagementEmergencyCancellationRecoversLostReserveACK",
    "TestManagementCancellationHoldRecoveryRequiresRecordedProof",
}


def local_database_url(raw):
    url = urlsplit(raw)
    query = parse_qs(url.query, keep_blank_values=True)
    if (
        url.scheme not in {"postgres", "postgresql"}
        or url.hostname != "127.0.0.1"
        or not url.port
        or not url.username
        or url.password is not None
        or url.path != "/postgres"
        or url.fragment
        or query != {"sslmode": ["disable"]}
    ):
        raise ValueError("requires password-free disposable 127.0.0.1/postgres URL with explicit port")
    return raw


def verify_results(events):
    passed = set()
    count = 0
    for event in events:
        name = event.get("Test", "")
        if name and event.get("Action") == "pass":
            count += 1
            if "/" not in name:
                passed.add(name)
        if event.get("Action") == "fail":
            raise RuntimeError("Go financial contract failed")
        if event.get("Action") == "skip" and name.split("/", 1)[0] in REQUIRED:
            raise RuntimeError("required financial contract or subcase skipped")
    if REQUIRED - passed:
        raise RuntimeError("required financial contracts did not execute: " + ", ".join(sorted(REQUIRED - passed)))
    return count


def main():
    raw = local_database_url(os.environ.get("PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL", ""))
    psql = os.environ.get("PINVOU_ACCOUNT_TEST_PSQL", "psql")
    go = os.environ.get("PINVOU_ACCOUNT_TEST_GO", "go")

    def query(sql):
        result = subprocess.run(
            [psql, raw, "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", sql],
            capture_output=True, text=True, encoding="utf-8", check=False,
        )
        if result.returncode:
            raise RuntimeError("disposable PostgreSQL operation failed")
        return result.stdout.strip()

    databases = query("SELECT datname FROM pg_database ORDER BY datname")
    user = query("SELECT current_user")
    roles = query("SELECT rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY rolname")
    if databases != "postgres\ntemplate0\ntemplate1" or roles != user:
        raise RuntimeError("database or deployment actors already exist; no writes permitted")
    if query("""SELECT
        (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
         WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema') +
        (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
         WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema')""") != "0":
        raise RuntimeError("bootstrap database contains user objects; no writes permitted")
    if int(query("SHOW server_version_num")) < 160000:
        raise RuntimeError("PostgreSQL 16 or newer is required by existing actor grants")
    query("""
BEGIN;
CREATE ROLE billing_schema_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE billing_migrator LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE billing_service LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
GRANT billing_service,billing_schema_owner TO billing_migrator;
GRANT billing_service TO billing_schema_owner WITH INHERIT TRUE,SET TRUE;
COMMIT;
""")
    try:
        env = {key: value for key, value in os.environ.items() if not key.startswith("PINVOU_TEST_")}
        env.update(PINVOU_TEST_TOKEN_DATABASE_URL=raw, PINVOU_TEST_BILLING_DATABASE_URL=raw)
        result = subprocess.run(
            [go, "test", "-p=1", "-race", "-tags=nomsgpack", "-mod=readonly", "-count=1", "-timeout=20m", "-json", "./internal/token"],
            cwd=ROOT / "account-platform", env=env, capture_output=True, text=True, encoding="utf-8", check=False,
        )
        # Preserve genuine Go output, including optional external-service skips.
        sys.stdout.write(result.stdout)
        sys.stderr.write(result.stderr)
        if result.returncode:
            raise RuntimeError("Go financial contracts failed")
        count = verify_results([json.loads(line) for line in result.stdout.splitlines() if line.startswith("{")])
        print(f"Token contracts: {count} named passes; all {len(REQUIRED)} required top-level suites executed without skipped subcases")
    finally:
        # Do not force-delete remnants from failed fixtures or unknown actors.
        current = set(query("SELECT rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY rolname").splitlines())
        if query("SELECT datname FROM pg_database ORDER BY datname") != databases or current != ACTORS | {user}:
            raise RuntimeError("fixture cleanup incomplete; actors retained for diagnosis")
        query("BEGIN; DROP ROLE billing_migrator; DROP ROLE billing_schema_owner; DROP ROLE billing_service; COMMIT;")
        if query("SELECT rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY rolname") != roles:
            raise RuntimeError("original role set was not restored")


if __name__ == "__main__":
    main()

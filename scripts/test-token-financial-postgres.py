#!/usr/bin/env python3
"""Run existing Token and scoped organization financial contracts on disposable PG.

The Account fixture first owns its eight absent actors, then the wallet
evidence fixture owns its absent Billing actors exclusively.
After strict restoration, retained fixtures create their own UUID databases and
Token actors while this runner owns the three Billing deployment actors. Only
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
    "TestF05AdditionalReservationProjectionPublicPostgres",
    "TestF06OrganizationAdmissionBoundariesPublicPostgres",
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

ACCOUNT_PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/account"
ACCOUNT_REQUIRED = {"TestF06OrganizationStatusProjectionVersionPublicPostgres", "TestF06OrganizationDeletionVersionAndTerminalScope"}
TOKEN_PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/token"
BILLING_PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/billing"
OWNER_REQUIRED = {"TestF06OrganizationWalletRequiresLifecycleEvidence"}
RETAINED_REQUIRED = {
    "TestLedgerRepairClaimActivationReadErrorsPostgres",
    "TestMemberHoldIdentityMutationBoundaries",
    "TestPersonalUsageHoldBoundaries",
    "TestHoldExtensionIdempotencePostgres",
    "TestPersonalUsageHoldClockBoundaries",
    "TestF05ExpiredHoldRenewalIsConflictAndUnprovenFundsStayReserved",
}
# This original regression is one named leaf test. Other retained roots must
# still produce their original child-case evidence, not just a parent PASS.
BILLING_CHILD_REQUIRED = RETAINED_REQUIRED - {"TestF05ExpiredHoldRenewalIsConflictAndUnprovenFundsStayReserved"}


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


def verify_results(events, required=None, *, package=None, require_children=False, exact_roots=False, children_required=None):
    required = REQUIRED if required is None else required
    started, passed = set(), set()
    children = set()
    package_passed = False
    count = 0
    for event in events:
        name = event.get("Test", "")
        root = name.split("/", 1)[0]
        action = event.get("Action")
        if action == "fail":
            raise RuntimeError("Go financial contract failed")
        if package is not None and name and event.get("Package") != package:
            raise RuntimeError("financial contract came from an unexpected package")
        if exact_roots and name and root not in required:
            raise RuntimeError("unexpected financial contract in scoped phase")
        if action == "run" and name and "/" not in name:
            started.add(name)
        if action == "pass":
            if name:
                count += 1
                if "/" not in name:
                    passed.add(name)
                else:
                    children.add(root)
            elif event.get("Package") == package:
                package_passed = True
        if action == "skip" and root in required:
            raise RuntimeError("required financial contract or subcase skipped")
    if required - passed or required - started:
        raise RuntimeError("required financial contracts did not execute: " + ", ".join(sorted(required - passed | required - started)))
    if require_children and (required if children_required is None else children_required) - children:
        raise RuntimeError("required financial contract has no completed child cases")
    if package is not None and not package_passed:
        raise RuntimeError("financial package did not complete successfully")
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
    def assert_instance(expected_roles):
        if query("SELECT datname FROM pg_database ORDER BY datname") != databases or set(query("SELECT rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY rolname").splitlines()) != expected_roles:
            raise RuntimeError("database or deployment actors differ; no fixture writes or cleanup permitted")
        if query("""SELECT
            (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
             WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema') +
            (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
             WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema')""") != "0":
            raise RuntimeError("bootstrap database contains user objects; no writes permitted")
        if query("SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid()") != "0":
            raise RuntimeError("another database client is active; exclusive fixture required")

    if databases != "postgres\ntemplate0\ntemplate1" or roles != user:
        raise RuntimeError("database or deployment actors already exist; no writes permitted")
    assert_instance({user})
    if int(query("SHOW server_version_num")) < 160000:
        raise RuntimeError("PostgreSQL 16 or newer is required by existing actor grants")

    def run_phase(label, target, package, required, *, scoped=False):
        env = {key: value for key, value in os.environ.items() if not key.startswith("PINVOU_TEST_")}
        if target == "./internal/account":
            env["PINVOU_TEST_ACCOUNT_DATABASE_URL"] = raw
        else:
            env["PINVOU_TEST_BILLING_DATABASE_URL"] = raw
        if target == "./internal/token":
            env["PINVOU_TEST_TOKEN_DATABASE_URL"] = raw
        command = [go, "test", "-p=1", "-race", "-tags=nomsgpack", "-mod=readonly", "-count=1", "-timeout=20m", "-json"]
        if scoped:
            command += ["-run", "^(" + "|".join(sorted(required)) + ")$"] if len(required) > 1 else ["-run", "^" + next(iter(required)) + "$"]
        command.append(target)
        print(f"Financial phase: {label}", flush=True)
        result = subprocess.run(command, cwd=ROOT / "account-platform", env=env,
                                capture_output=True, text=True, encoding="utf-8", check=False)
        # Preserve real phase output even when Go failed, skipped or was truncated.
        sys.stdout.write(result.stdout)
        sys.stderr.write(result.stderr)
        if result.returncode:
            raise RuntimeError("Go financial phase failed: " + label)
        count = verify_results([json.loads(line) for line in result.stdout.splitlines() if line.startswith("{")],
                               required, package=package, require_children=scoped, exact_roots=scoped,
                               children_required=BILLING_CHILD_REQUIRED if label == "retained-billing" else None)
        print(f"Financial phase {label}: {count} named passes; all {len(required)} required roots completed", flush=True)

    # Each existing owner fixture requires its own absent actors. Restoration
    # is checked between every phase, before any next fixture can write.
    try:
        run_phase("owner-account", "./internal/account", ACCOUNT_PACKAGE, ACCOUNT_REQUIRED, scoped=True)
    finally:
        assert_instance({user})
    try:
        run_phase("owner-wallet", "./internal/billing", BILLING_PACKAGE, OWNER_REQUIRED, scoped=True)
    finally:
        assert_instance({user})

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
        run_phase("retained-token", "./internal/token", TOKEN_PACKAGE, REQUIRED)
        assert_instance(ACTORS | {user})
        run_phase("retained-billing", "./internal/billing", BILLING_PACKAGE, RETAINED_REQUIRED, scoped=True)
    finally:
        # Do not force-delete remnants from failed fixtures or unknown actors.
        assert_instance(ACTORS | {user})
        if query("""SELECT
            EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass
                   AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service')) AND dbid<>0)
            OR EXISTS(SELECT 1 FROM pg_auth_members
                      WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service'))
                             OR member IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service')))
                        AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service'))
                                AND member IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service'))))""") != "f":
            raise RuntimeError("external actor dependencies; owned roles retained for diagnosis")
        query("BEGIN; DROP ROLE billing_migrator; DROP ROLE billing_schema_owner; DROP ROLE billing_service; COMMIT;")
        assert_instance({user})



if __name__ == "__main__":
    main()

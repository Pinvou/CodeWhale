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
_financial_helpers = runpy.run_path(str(ROOT / "scripts/test-token-financial-postgres.py"))
local_database_url = _financial_helpers["local_database_url"]
ci_identity_guard = _financial_helpers["ci_identity_guard"]
LEDGER_REPAIR_APPROVAL_ROOT = "TestLedgerRepairCrossDomainActivationPostgresIntegration"
LEDGER_REPAIR_PAYMENT_E2E_ROOT = "TestLedgerRepairPaymentAuthorityE2EPostgresIntegration"
LEDGER_REPAIR_APPROVAL_ROOTS = {LEDGER_REPAIR_APPROVAL_ROOT, LEDGER_REPAIR_PAYMENT_E2E_ROOT}
BILLING_ACTORS = {"billing_migrator", "billing_schema_owner", "billing_service"}
MIGRATION_CLI_ROOTS = {"TestAccountClosureLegacyRollbackCLI", "TestPhaseMigrationCLIPinsBeforeDown", "TestAccountClosureSagaIdentityDispatchPostgres"}
MIGRATION_IDENTITY_ROOT = "TestAccountClosureSagaIdentityDispatchPostgres"
MIGRATION_IDENTITY_CASES = {
    "source-only-name-and-checksum", "ledger-only-renamed-directory",
    "ledger-checksum-only-from-original-writer", "actual-cli-rejects-mutated-down-without-effects",
    "mixed-domains-reject-before-writes", "unknown-custom027-keeps-generic-cli",
}
BILLING_ROOTS = {
    "TestClosedGrantPublishedOwnerEffects",
    "TestAccountClosureIsolationPreservesPriorAuditFacts",
    "TestAccountClosureIsolationScrubsContactWithoutFinancialMutation",
    "TestAccountClosurePreservesUnknownForcedRefundEvidence",
    "TestAccountClosureRequiresAuthoritativeChargebackTerminal",
    "TestAccountClosureAssessmentIncludesMemberReservations",
    "TestF08MembershipClosureReasonQuotaReplayPostgres",
    "TestF08ClosedGrantConcurrentReceiptReplayPostgres",
    "TestF08ClosureBlocksPendingFinancialAnomalyPostgres",
    "TestF08ReclaimBlocksPendingFinancialAnomalyPostgres",
}
# Each fixed public harness owns its complete actor/schema lifecycle. The
# original CI child spent ~394s on nine roots before its 403s hard timeout;
# these groups retain the existing per-invocation limits instead of enlarging
# that shared timeout. Their observed root totals were ~107s, ~190s and >96s.
CLOSED_GRANT_GROUPS = [
    ("closed-grants-owner", "TestClosedGrantPublicOwnerHarness", {
        "TestClosedGrantPublishedOwnerEffects",
        "TestF08ClosedGrantConcurrentReceiptReplayPostgres",
        "TestAccountClosureIsolationScrubsContactWithoutFinancialMutation",
        "TestAccountClosureIsolationPreservesPriorAuditFacts",
    }),
    ("closed-grants-financial", "TestClosedGrantPublicFinancialHarness", {
        "TestAccountClosurePreservesUnknownForcedRefundEvidence",
        "TestAccountClosureRequiresAuthoritativeChargebackTerminal",
        "TestF08ClosureBlocksPendingFinancialAnomalyPostgres",
        "TestF08ReclaimBlocksPendingFinancialAnomalyPostgres",
    }),
    ("closed-grants-membership", "TestClosedGrantPublicMembershipHarness", {
        "TestAccountClosureAssessmentIncludesMemberReservations",
        "TestF08MembershipClosureReasonQuotaReplayPostgres",
    }),
]
CONTRACTS = [
    ("account-pagination", "./internal/account", {"TestAccountClosurePaginationPostgres"}, {}),
    ("migration-cli", "./cmd/migrate",
     MIGRATION_CLI_ROOTS, {}),
    ("closure-combination", "./internal/billingschema", {"TestClosurePrivatePublicCombination"},
     {"": {"TestClosureClaimACKOrder"}}),
    ("membership-closure-event", "./internal/billingschema",
     {"TestF08AccountClosureMembershipEventConsumerPublicPostgres"}, {}),
    *((name, "./internal/billingschema", {parent}, {"": roots})
      for name, parent, roots in CLOSED_GRANT_GROUPS),
    ("gift-closure", "./internal/billingschema", {"TestGiftClosurePublicHarness"},
     {"": {"TestGiftClosureProducerBoundaries"}, ".billing": {"TestClosedGrantPublishedOwnerEffects"}}),
    ("ledger-repair-approval-proof", "./internal/account", LEDGER_REPAIR_APPROVAL_ROOTS, {}),
    ("account-anonymization", "./internal/account",
     {"TestAccountClosureAnonymizesOwnedHistoricalContacts", "TestAccountClosureRollbackPreservesEvidence",
      "TestAccountClosureSelfServiceRecoveryPostgres", "TestF08ClosureAdvanceFailureIsScheduledPostgres",
      "TestAccountClosureBlockedSLAAlertsPostgres"}, {}),
]


def validate_closed_grant_contracts(contracts):
    migration = ("migration-cli", "./cmd/migrate", MIGRATION_CLI_ROOTS, {})
    if [row for row in contracts if row[0] == migration[0]] != [migration]:
        raise RuntimeError("all original migration CLI and Account027 identity roots are required exactly once")
    ledger = ("ledger-repair-approval-proof", "./internal/account", LEDGER_REPAIR_APPROVAL_ROOTS, {})
    if [row for row in contracts if row[0] == ledger[0]] != [ledger]:
        raise RuntimeError("ledger repair approval proof phase is required exactly once")
    seen, names, parents = set(), set(), set()
    for name, parent, roots in CLOSED_GRANT_GROUPS:
        if not roots or seen & roots or name in names or parent in parents:
            raise RuntimeError("closed-grant groups must be nonempty and disjoint")
        seen.update(roots)
        names.add(name)
        parents.add(parent)
    if seen != BILLING_ROOTS:
        raise RuntimeError("closed-grant groups must execute exactly the ten required owner roots")
    expected = [(name, "./internal/billingschema", {parent}, {"": roots})
                for name, parent, roots in CLOSED_GRANT_GROUPS]
    selected = [row for row in contracts if row[0].startswith("closed-grants")]
    if selected != expected:
        raise RuntimeError("every closed-grant public harness invocation is required exactly once")


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
                if name in passed:
                    raise RuntimeError("duplicate closure root result: " + name)
                passed.add(name)
    if (running or not passed_packages or started_packages - passed_packages
            or test_packages - passed_packages):
        raise RuntimeError("incomplete Go package or test lifecycle")
    if not required or not count or passed != required:
        raise RuntimeError("required closure contracts did not execute exactly: missing="
                           + ", ".join(sorted(required - passed)) + "; unexpected="
                           + ", ".join(sorted(passed - required)))
    return count


def verify_migration_identity_results(raw):
    """Bind the027 contract to its actual package and complete named cases."""
    wanted = {MIGRATION_IDENTITY_ROOT, *(MIGRATION_IDENTITY_ROOT + "/" + case for case in MIGRATION_IDENTITY_CASES)}
    observed = {name: [] for name in wanted}
    for index, line in enumerate(raw.splitlines()):
        if not line.startswith("{"):
            continue
        event = json.loads(line)
        name = event.get("Test", "")
        if name != MIGRATION_IDENTITY_ROOT and not name.startswith(MIGRATION_IDENTITY_ROOT + "/"):
            continue
        if name not in wanted or event.get("Package") != "github.com/Pinvou/pinvou3/account-platform/cmd/migrate":
            raise RuntimeError("unreviewed027 case or wrong migration CLI package")
        if event.get("Action") != "output":
            observed[name].append((event.get("Action"), index))
    for events in observed.values():
        if [action for action, _ in events] != ["run", "pass"]:
            raise RuntimeError("original027 identity root/case must run and pass exactly once")
    start, end = [index for _, index in observed[MIGRATION_IDENTITY_ROOT]]
    if any(not start < events[0][1] < events[1][1] < end for name, events in observed.items() if name != MIGRATION_IDENTITY_ROOT):
        raise RuntimeError("027 child cases escaped their root lifecycle")


def main():
    validate_closed_grant_contracts(CONTRACTS)
    raw = local_database_url(os.environ.get("PINVOU_ACCOUNT_CLOSURE_TEST_DATABASE_URL", ""))
    psql = os.environ.get("PINVOU_ACCOUNT_TEST_PSQL", "psql")
    go = shutil.which(os.environ.get("PINVOU_ACCOUNT_TEST_GO", "go"))
    if not go:
        raise RuntimeError("Go executable is required by the child fixtures")

    def query(sql):
        result = subprocess.run(
            [psql, raw, "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", *ci_identity_guard(raw), "-c", sql],
            capture_output=True, text=True, encoding="utf-8", check=False, timeout=30,
        )
        if result.returncode:
            raise RuntimeError("disposable PostgreSQL operation failed")
        return result.stdout.strip()

    databases = "postgres\ntemplate0\ntemplate1"
    user = query("SELECT current_user")

    def require_instance(billing=False):
        expected = {user} | (BILLING_ACTORS if billing else set())
        roles = set(query("SELECT rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY rolname").splitlines())
        if query("SELECT datname FROM pg_database ORDER BY datname") != databases or roles != expected:
            raise RuntimeError("database or actors differ; retain them for diagnosis, no setup writes permitted")
        if query("SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid()") != "0":
            raise RuntimeError("another client is active; exclusive fixture required")
        objects = query("""SELECT
            (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
             WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema') +
            (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
             WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema')""")
        if objects != "0":
            raise RuntimeError("bootstrap database contains user objects; no setup writes permitted")

    require_instance()
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
        require_instance()
        owns_billing = False
        child_log = artifacts / (name + ".child.jsonl")
        race_enabled = sys.platform != "win32"
        argv = [go, "test", "-p=1", *(["-race"] if race_enabled else []), "-tags=nomsgpack", "-mod=readonly", "-count=1",
                "-timeout=8m", "-json", package, "-run", "^(" + "|".join(sorted(roots)) + ")$"]
        try:
            if name == "ledger-repair-approval-proof":
                # This root's public Account fixture creates its own Account
                # roles; the separate Billing fixture needs these three roles.
                # The absence check above occurs before any actor is created.
                query("""BEGIN;
CREATE ROLE billing_schema_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE billing_migrator LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE billing_service LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
GRANT billing_service,billing_schema_owner TO billing_migrator;
GRANT billing_service TO billing_schema_owner WITH INHERIT TRUE,SET TRUE;
COMMIT;""")
                owns_billing = True
                require_instance(billing=True)
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
            if name == "migration-cli":
                verify_migration_identity_results(result.stdout)
            child_counts = {}
            for suffix, required in children.items():
                if suffix not in child_logs:
                    raise RuntimeError("required child result log is missing: " + name + suffix)
                child_raw = child_logs[suffix]
                child_counts[suffix or "account_or_billing"] = verify_results(child_raw, required)
            print(json.dumps({"contract": name, "parent_named_passes": parent_count,
                              "child_named_passes": child_counts, "race_enabled": race_enabled,
                              "artifacts": str(artifacts)}))
        finally:
            # Go fixtures own their UUID database and Account-role cleanup.
            # Never remove an unknown actor or a role with external dependencies.
            if owns_billing:
                require_instance(billing=True)
                if query("""SELECT
                    EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass
                      AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service')) AND dbid<>0)
                    OR EXISTS(SELECT 1 FROM pg_auth_members
                      WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service'))
                             OR member IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service')))
                        AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service'))
                                AND member IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service'))))""") != "f":
                    raise RuntimeError("external Billing dependencies; owned roles retained for diagnosis")
                query("BEGIN; DROP ROLE billing_migrator; DROP ROLE billing_schema_owner; DROP ROLE billing_service; COMMIT;")
            require_instance()


if __name__ == "__main__":
    main()

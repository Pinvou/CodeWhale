"""Explicit owned PostgreSQL/TLS NATS acceptance; conditional skips are failures."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
REQUIRED = {
    "TestActualHistoricalRefundUpgrade",
    "TestRefundAttemptIsDurableWhenResultUnknown",
    "TestRefundCLIUnknownACKReadback",
    "TestRefundPreparationRejectsHistoricalDrift",
    "TestRefundPreparationRejectsPendingOrAckPending",
    "TestRefundUpgradeNeedsTemporaryOperatorPermission",
    "TestRefundTemporaryPermissionReload",
    "TestConcurrentRefundAttemptsSendAtMostOnce",
    "TestRefundLostActualUpdateReplyCannotResend",
    "TestExistingBrokerOperatorActualPGTLSNATS",
    "TestExistingBrokerCLIActualPGTLSNATS",
    "TestExistingBrokerCLIUnknownACKReadbackAndRecovery",
    "TestExistingBrokerDriftRejectsBeforeSiblingWrites",
    "TestExistingBrokerOperatorCannotUpdateRefund",
    "TestExistingBrokerRetainedAccountIdentityReload",
    "TestExistingBrokerArgumentsRejectBeforeCredentials",
    "TestFreshApprovalOperatorActualPGTLSNATS",
    "TestPasswordPreparationExactWorkloadRoute",
}
REQUIRED_CASES = {
    "TestRefundPreparationRejectsPendingOrAckPending/pending",
    "TestRefundPreparationRejectsPendingOrAckPending/ack-pending",
    "TestRefundPreparationRejectsHistoricalDrift/shared-maxdeliver",
    "TestRefundPreparationRejectsHistoricalDrift/refund-maxdeliver",
    "TestRefundPreparationRejectsHistoricalDrift/token-pending",
    "TestExistingBrokerCLIUnknownACKReadbackAndRecovery/token-update-interrupted",
    "TestExistingBrokerCLIUnknownACKReadbackAndRecovery/billing-create-interrupted",
    "TestExistingBrokerCLIUnknownACKReadbackAndRecovery/token-update-timeout",
    "TestExistingBrokerDriftRejectsBeforeSiblingWrites/message-removed-after-plan",
    "TestExistingBrokerDriftRejectsBeforeSiblingWrites/message-added-after-plan",
}

def main():
    database = os.environ.get("PINVOU_TEST_SCHEMA_DATABASE_URL", "")
    nats = os.environ.get("PINVOU_TEST_NATS_SERVER_BINARY", "")
    if not database or not nats or not Path(nats).is_absolute() or not Path(nats).is_file() or not os.access(nats, os.X_OK):
        raise SystemExit("EXPLICIT_OWNED_POSTGRES_AND_EXECUTABLE_NATS_REQUIRED")
    # Database fixtures independently refuse existing deployment actors before
    # setup, create UUID databases using real public migrations, and guard cleanup.
    with tempfile.TemporaryDirectory(prefix="pinvou-existing-broker-") as directory:
        binary = str(Path(directory) / ("approval-resources.exe" if os.name == "nt" else "approval-resources"))
        build = subprocess.run(["go", "build", "-mod=readonly", "-trimpath", "-o", binary, "./cmd/approval-resources"], cwd=ROOT, capture_output=True)
        if build.returncode:
            sys.stderr.buffer.write(build.stderr)
            raise SystemExit("OPERATOR_BUILD_FAILED")
        identity_binary = str(Path(directory) / ("nats-identities.exe" if os.name == "nt" else "nats-identities"))
        identity_build = subprocess.run(["go", "build", "-mod=readonly", "-trimpath", "-o", identity_binary, "./cmd/nats-identities"], cwd=ROOT / "tools/kms-recovery", capture_output=True)
        if identity_build.returncode:
            sys.stderr.buffer.write(identity_build.stderr)
            raise SystemExit("IDENTITY_OPERATOR_BUILD_FAILED")
        environment = dict(os.environ, PINVOU_TEST_APPROVAL_OPERATOR_BINARY=binary, PINVOU_TEST_NATS_IDENTITY_BINARY=identity_binary, GIN_MODE="release")
        command = ["go", "test", "-mod=readonly", "-json", "-count=1", "-p", "1", "-parallel=1", "-run", "Test(ExistingBroker|FreshApprovalOperator|PasswordPreparationExactWorkloadRoute|ActualHistoricalRefundUpgrade|Refund|ConcurrentRefund)", "./internal/platform/events", "./internal/billingschema", "./cmd/approval-resources", "./internal/account"]
        result = subprocess.run(command, cwd=ROOT, env=environment, capture_output=True)
        passed, skipped = set(), set()
        for line in result.stdout.splitlines():
            event = json.loads(line)
            if event.get("Action") == "pass": passed.add(event.get("Test", ""))
            if event.get("Action") == "skip": skipped.add(event.get("Test", ""))
        missing = (REQUIRED | REQUIRED_CASES) - passed
        skipped_required = {name for name in skipped if name.split("/", 1)[0] in REQUIRED}
        if result.returncode or missing or skipped_required:
            sys.stderr.buffer.write(result.stdout + result.stderr)
            raise SystemExit("EXISTING_BROKER_ACCEPTANCE_FAILED_OR_SKIPPED")
        print(json.dumps({"status": "OWNED_PG_TLS_NATS_ACCEPTANCE_PASSED", "required_tests": sorted(REQUIRED), "required_cases": sorted(REQUIRED_CASES), "production_verified": False}))

if __name__ == "__main__":
    main()

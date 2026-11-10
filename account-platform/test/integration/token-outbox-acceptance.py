"""Run the ordinary Token publisher against explicitly owned PostgreSQL/TLS NATS."""
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
TEST = "TestOrdinaryTokenRelayActualTLSNATSPG"
PACKAGE = "github.com/Pinvou/pinvou3/account-platform/cmd/event-relay"
REQUIRED = {TEST} | {TEST + "/" + name for name in (
    "publisher-scope", "wrong-initial-identity", "wrong-database-owner", "publish-confirmed"
)}


def validate(output, exit_code):
    events = [json.loads(line) for line in output.splitlines() if line.strip()]
    terminal = [e for e in events if e.get("Action") in ("pass", "fail", "skip")]
    passed = {e.get("Test") for e in terminal if e.get("Test") and e.get("Action") == "pass"}
    if (exit_code or not REQUIRED <= passed
            or any(e.get("Package") != PACKAGE or e.get("Action") != "pass" for e in terminal)
            or not any(e.get("Action") == "pass" and not e.get("Test") for e in terminal)):
        raise ValueError("TOKEN_OUTBOX_ACCEPTANCE_FAILED_OR_SKIPPED")


def main():
    binary = os.environ.get("PINVOU_TEST_NATS_SERVER_BINARY", "")
    if (not os.environ.get("PINVOU_TEST_SCHEMA_DATABASE_URL") or not binary
            or not Path(binary).is_absolute() or not Path(binary).is_file()
            or not os.access(binary, os.X_OK)):
        raise SystemExit("EXPLICIT_OWNED_POSTGRES_AND_EXECUTABLE_NATS_REQUIRED")
    command = [os.environ.get("PINVOU_ACCOUNT_TEST_GO", "go"), "test", "-race", "-tags=nomsgpack",
               "-mod=readonly", "-count=1", "-json", "-timeout=3m", "-run", "^" + TEST + "$", "./cmd/event-relay"]
    result = subprocess.run(command, cwd=ROOT, capture_output=True, timeout=210)
    sys.stdout.buffer.write(result.stdout)
    sys.stderr.buffer.write(result.stderr)
    try:
        validate(result.stdout.decode("utf-8"), result.returncode)
    except (ValueError, UnicodeError):
        raise SystemExit("TOKEN_OUTBOX_ACCEPTANCE_FAILED_OR_SKIPPED") from None
    print(json.dumps({"status": "OWNED_TOKEN_OUTBOX_ACCEPTANCE_PASSED", "production_verified": False}))


if __name__ == "__main__":
    main()

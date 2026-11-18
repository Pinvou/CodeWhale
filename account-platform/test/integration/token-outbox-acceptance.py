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
    def reject():
        raise ValueError("TOKEN_OUTBOX_ACCEPTANCE_FAILED_OR_SKIPPED")
    if exit_code:
        reject()
    started, passed = set(), set()
    package_started = package_passed = False
    for line in output.splitlines():
        if not line.strip():
            continue
        event = json.loads(line)
        if not isinstance(event, dict) or event.get("Package") != PACKAGE:
            reject()
        action, name = event.get("Action"), event.get("Test")
        if "Test" in event and (not isinstance(name, str) or not name):
            reject()
        if package_passed or action in ("fail", "skip"):
            reject()
        if name:
            if name not in REQUIRED or not package_started:
                reject()
            if action == "run":
                if name in started or (name != TEST and (TEST not in started or TEST in passed)):
                    reject()
                started.add(name)
            elif action == "pass":
                if (name not in started or name in passed
                        or (name == TEST and passed != REQUIRED - {TEST})):
                    reject()
                passed.add(name)
            elif action not in ("output", "pause", "cont") or name not in started:
                reject()
        elif action == "start":
            if package_started:
                reject()
            package_started = True
        elif action == "pass":
            if not package_started or started != REQUIRED or passed != REQUIRED:
                reject()
            package_passed = True
        elif action != "output" or not package_started:
            reject()
    if not package_passed:
        reject()


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

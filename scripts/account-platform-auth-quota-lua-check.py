#!/usr/bin/env python3
"""Require actual production quota Lua execution through the Go consumers.

Lua5.1 is a test-only CLI, not a product dependency or a Redis server. The Go
fixture controls Redis primitives and faults, while Lua executes the production
algorithm. No listener, external service, database or credentials are used.

CI uses Linux owned-session cleanup, including descendants after parent exit.
Windows local validation cleans the live parent's tree on timeout; it does not
claim Job-object containment of detached children after a parent already exits.
"""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile

PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
REQUIRED = {
    "TestTokenQuotaDeviceBoundaryAndHTTPConsumer",
    "TestTokenQuotaSourceDay200And201KeepOriginalDeadline",
    "TestTokenQuotaSourceMinute20And21",
    "TestTokenQuotaRefreshSessionAndActionBoundaries",
    "TestTokenQuotaLegacyCountersRepairOnceAndExpireThroughHTTP",
    "TestTokenQuotaZeroTTLStillRejectsAndHealthyOtherDimensionSurvives",
    "TestTokenQuotaPartialCommandFailuresAndUnknownResponseFailClosed",
    "TestTokenQuotaDoesNotRequireSeparateExpirePermission",
    "TestTokenQuotaExpireFaultRecoveryRetainsSourceBoundaryThroughHTTP",
    "TestTokenQuotaRejectsInvalidScriptResults",
    "TestTokenQuotaConcurrentAdmissionsAndFailurePolicyIsolation",
    "TestTokenQuotaScriptUsesExistingIdentityACL",
}
LUA_REQUIRED = REQUIRED - {"TestTokenQuotaScriptUsesExistingIdentityACL"}


def command(value):
    args = json.loads(value)
    if not isinstance(args, list) or not args or any(not isinstance(item, str) or not item for item in args):
        raise ValueError("command must be a nonempty JSON string array")
    if not shutil.which(args[0]):
        raise ValueError("required test executable is unavailable")
    return args


def validate(events, returncode):
    if returncode != 0:
        raise ValueError("quota Lua Go tests failed")
    if any(e.get("Action") in {"skip", "fail"} for e in events):
        raise ValueError("quota Lua test skipped or failed")
    roots = [e.get("Test") for e in events if e.get("Action") == "pass" and e.get("Test") and "/" not in e["Test"]]
    if set(roots) != REQUIRED or len(roots) != len(REQUIRED):
        raise ValueError("required quota Lua test selection did not execute exactly once")
    packages = [e for e in events if e.get("Action") == "pass" and not e.get("Test")]
    if len(packages) != 1 or packages[0].get("Package") != PACKAGE:
        raise ValueError("quota Lua package did not pass")
    actual = set()
    for event in events:
        match = re.search(r"AUTH_QUOTA_ACTUAL_LUA executions=([1-9][0-9]*)\b", event.get("Output", ""))
        if match:
            actual.add(event.get("Test", "").split("/", 1)[0])
    if actual != LUA_REQUIRED:
        raise ValueError("required consumers did not execute the actual Lua program")


def stop_owned_process(process):
    if os.name == "nt":
        if process.poll() is not None:
            # Windows has no retained process-tree handle here. This local
            # adapter does not claim cleanup of a previously detached child.
            return
        # PID comes only from our Popen handle; no unrelated process is selected.
        subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10, check=False)
    else:
        # Also reap descendants after the Go parent has already exited. The
        # session/group was created solely for this invocation.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    process.wait(timeout=10)


def run(args):
    lua, go = command(args.lua_command_json), command(args.go_command_json)
    environment = dict(os.environ)
    environment["PINVOU_TEST_AUTH_QUOTA_LUA_COMMAND"] = json.dumps(lua)
    test = [*go, "test", "-p=2", "-tags=nomsgpack", "-mod=readonly", "-count=1", "-timeout=90s", "-json", "-run=^TestTokenQuota", "./internal/identityadapter"]
    if args.race:
        test.insert(len(go) + 1, "-race")
    if args.overlay:
        test.insert(len(go) + 1, "-overlay=" + str(Path(args.overlay).resolve()))
    root = Path(__file__).resolve().parent.parent
    with tempfile.TemporaryDirectory(prefix="pinvou-auth-quota-") as temporary:
        output, errors = Path(temporary) / "go.jsonl", Path(temporary) / "go.stderr"
        options = {"creationflags": subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == "nt" else {"start_new_session": True}
        with output.open("wb") as stdout, errors.open("wb") as stderr:
            process = subprocess.Popen(test, cwd=root / "account-platform", env=environment, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr, **options)
            timed_out = False
            try:
                process.wait(timeout=args.timeout)
            except subprocess.TimeoutExpired:
                timed_out = True
            finally:
                stop_owned_process(process)
        if output.stat().st_size > 16 << 20 or errors.stat().st_size > 1 << 20:
            raise ValueError("quota Lua test output exceeded its bound")
        # A killed process may leave the final UTF-8 character incomplete. Only
        # failure diagnostics may replace it; successful JSON remains strict.
        decoding = "replace" if timed_out else "strict"
        raw = output.read_text(encoding="utf-8", errors=decoding)
        sys.stdout.write(raw)
        sys.stderr.write(errors.read_text(encoding="utf-8", errors=decoding))
        # Preserve bounded diagnostics before the private directory is removed.
        # Even a complete-looking pass stream cannot turn a timeout into success.
        if timed_out:
            raise ValueError("quota Lua runner exceeded its process deadline")
        events = [json.loads(line) for line in raw.splitlines() if line.strip()]
        if any(not isinstance(event, dict) for event in events):
            raise ValueError("quota Lua test emitted invalid JSON events")
        validate(events, process.returncode)
    print("AUTH_QUOTA_PRODUCTION_LUA_PASS roots=12 actual_lua_roots=11", file=sys.stderr)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lua-command-json", default='["lua5.1"]')
    parser.add_argument("--go-command-json", default='["go"]')
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--overlay", help="isolated Go source overlay for regression negative controls")
    parser.add_argument("--timeout", type=float, default=180, help="outer process deadline in seconds")
    args = parser.parse_args(argv)
    try:
        if not 0 < args.timeout <= 180:
            raise ValueError("quota Lua process deadline must be within 0..180 seconds")
        run(args)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print("AUTH_QUOTA_PRODUCTION_LUA_FAIL: " + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

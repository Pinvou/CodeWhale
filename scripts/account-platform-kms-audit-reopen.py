"""Reopen only the validated KMS audit file after a preserving log rotation."""

import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import time


ROOT = Path("/opt/pinvou-security")
IMAGE = "quay.io/openbao/openbao@sha256:11fd73a2102cda9c55d5d881a8c3210303146a7ec1e8ac76f526e175c6d24641"


def require(condition):
    if not condition:
        raise ValueError("KMS_AUDIT_REOPEN_FAILED")


def validate_target(data, root):
    config = data["Config"]
    require(data["State"]["Running"] and config["Image"] == IMAGE and config["User"] == "65532:65532")
    require(config["Labels"]["com.docker.compose.project"] == "pinvou-security"
            and config["Labels"]["com.docker.compose.service"] == "openbao")
    require(config["Entrypoint"] == ["/usr/bin/bao"] and config["Cmd"] == ["server", "-config=/bao/config/server.hcl"])
    audit = [m for m in data["Mounts"] if m["Destination"] == "/bao/audit"]
    require(len(audit) == 1 and audit[0]["Type"] == "bind" and audit[0]["RW"] is True
            and audit[0]["Source"] == str(root / "openbao/audit"))
    require(len(data["Id"]) == 64 and all(c in "0123456789abcdef" for c in data["Id"]))
    return data["Id"]


def command(args, timeout=15):
    result = subprocess.run(args, capture_output=True, check=False, timeout=timeout)
    require(result.returncode == 0)
    return result.stdout


def main():
    require(os.name == "posix" and os.geteuid() == 0)
    log = ROOT / "openbao/audit/audit.jsonl"
    info = log.lstat()
    require(log.resolve(strict=True) == log and stat.S_ISREG(info.st_mode)
            and stat.S_IMODE(info.st_mode) == 0o600 and info.st_uid == 65532 and info.st_gid == 65532)
    env = ROOT / "monitor/monitor.env"
    require(env.resolve(strict=True) == env and env.stat().st_uid == 0 and env.stat().st_mode & 0o077 == 0)
    require(env.stat().st_size <= 4096)
    values = dict(line.split("=", 1) for line in env.read_text().splitlines() if line and not line.startswith("#"))
    require(set(values) == {"KMS_MONITOR_ENDPOINT", "KMS_MONITOR_NETWORK"})
    data = json.loads(command(["docker", "container", "inspect", "pinvou-security-openbao"]))[0]
    container_id = validate_target(data, ROOT)
    command(["docker", "kill", "--signal=HUP", container_id])
    for _ in range(5):
        time.sleep(1)
        try:
            output = command([sys.executable, str(ROOT / "agents/account-platform-kms-agent-check.py"),
                              "--endpoint", values["KMS_MONITOR_ENDPOINT"], "--network", values["KMS_MONITOR_NETWORK"],
                              "--agent-directory", str(ROOT / "agents"), "--trust-directory", str(ROOT / "openbao/trust"),
                              "--lease-only"], timeout=55)
            current = log.stat()
            require(output.strip() == b"KMS_AGENTS_LIVE_LEASES_VERIFIED" and current.st_ino == info.st_ino
                    and current.st_size > info.st_size and current.st_mtime >= info.st_mtime)
            print("KMS_AUDIT_REOPEN_VERIFIED")
            return
        except Exception:
            continue
    raise ValueError("KMS_AUDIT_REOPEN_FAILED")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("KMS_AUDIT_REOPEN_FAILED_PRESERVE_ALL_LOGS", file=sys.stderr)
        sys.exit(1)

"""Read-only KMS operational checks. Output contains fixed checks, never secrets."""

import argparse
from datetime import datetime, timezone
import hashlib
import http.client
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import ssl
import stat
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit


def require(condition):
    if not condition:
        raise ValueError("KMS_MONITOR_CHECK_FAILED")


def private(path, directory=False, uid=65532):
    info = path.lstat()
    require(path.resolve(strict=True) == path and not path.is_symlink())
    require((stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode))
            and stat.S_IMODE(info.st_mode) == (0o700 if directory else 0o600)
            and info.st_uid == uid and info.st_gid == uid)
    return info


def certificate_status(expires, now):
    return "ok" if expires - now >= 14 * 86400 else "critical"


def capacity_status(total, free, inodes, available):
    if total <= 0 or free < max(2 << 30, total * 0.1):
        return "critical"
    if free < max(5 << 30, total * 0.2) or (inodes > 0 and available / inodes < 0.1):
        return "warning"
    return "ok"


def age_status(captured_at, now):
    stamp = datetime.fromisoformat(captured_at.replace("Z", "+00:00"))
    require(stamp.tzinfo is not None)
    age = now - stamp.timestamp()
    return "ok" if -300 <= age <= 90 * 60 else "critical"


def check_snapshot(root, now):
    private(root, directory=True)
    candidates = []
    stale_partial = False
    # Every scan is bounded; growth beyond the limit requires retention review.
    for count, item in enumerate(root.iterdir()):
        require(count < 10000)
        if not re.fullmatch(r"snapshot-\d{8}T\d{6}Z-[a-f0-9]{16}", item.name):
            raise ValueError("KMS_MONITOR_CHECK_FAILED")
        private(item, directory=True)
        marker = item / "SNAPSHOT_READY"
        if not marker.exists():
            stale_partial |= now - item.stat().st_mtime > 15 * 60
            continue
        private(marker)
        require(marker.stat().st_size <= 4096)
        data = json.loads(marker.read_bytes())
        require(data["version"] == 1 and isinstance(data["captured_at"], str))
        candidates.append((data["captured_at"], item, data))
    require(candidates)
    _, latest, data = max(candidates, key=lambda value: value[0])
    cipher = latest / "snapshot.age"
    info = private(cipher)
    require(0 < info.st_size <= 65 << 20 and info.st_size == data["ciphertext_bytes"])
    digest = hashlib.sha256()
    with cipher.open("rb") as stream:
        for chunk in iter(lambda: stream.read(65536), b""):
            digest.update(chunk)
    require(digest.hexdigest() == data["ciphertext_sha256"])
    result = age_status(data["captured_at"], now)
    return "warning" if stale_partial and result == "ok" else result


def check_tls(endpoint, ca, now):
    url = urlsplit(endpoint)
    require(url.scheme == "https" and url.port == 8200 and not url.username and not url.password
            and not url.path and not url.query and not url.fragment and ipaddress.ip_address(url.hostname).is_private)
    private(ca)
    context = ssl.create_default_context(cafile=str(ca))
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    conn = http.client.HTTPSConnection(url.hostname, 8200, context=context, timeout=10)
    try:
        conn.connect()
        expires = ssl.cert_time_to_seconds(conn.sock.getpeercert()["notAfter"])
        conn.request("GET", "/v1/sys/health")
        response = conn.getresponse()
        body = response.read(4097)
        require(response.status == 200 and len(body) <= 4096)
        health = json.loads(body)
        require(health["initialized"] is True and health["sealed"] is False)
        return certificate_status(expires, now)
    finally:
        conn.close()


def run_command(arguments, expected=None, timeout=10):
    result = subprocess.run(arguments, capture_output=True, timeout=timeout, check=False)
    require(result.returncode == 0 and (expected is None or result.stdout.strip() == expected))
    return result.stdout


def check_backup_unit():
    run_command(["systemctl", "is-active", "pinvou-kms-snapshot.timer"], b"active")
    run_command(["systemctl", "is-enabled", "pinvou-kms-snapshot.timer"], b"enabled")
    state = run_command(["systemctl", "show", "pinvou-kms-snapshot.service", "-p", "Result", "-p", "ExecMainStatus"])
    require(set(state.decode("ascii").strip().splitlines()) == {"Result=success", "ExecMainStatus=0"})
    return "ok"


def check_agents(root, endpoint, network):
    run_command([sys.executable, str(root / "agents/account-platform-kms-agent-check.py"),
                 "--endpoint", endpoint, "--agent-directory", str(root / "agents"),
                 "--trust-directory", str(root / "openbao/trust"), "--network", network, "--lease-only"],
                b"KMS_AGENTS_LIVE_LEASES_VERIFIED", timeout=55)
    return "ok"


def check_disk(root):
    disk = shutil.disk_usage(root)
    fs = os.statvfs(root)
    return capacity_status(disk.total, disk.free, fs.f_files, fs.f_favail)


def check_audit(path, now):
    info = private(path)
    require(info.st_size > 0 and -300 <= now - info.st_mtime <= 10 * 60)
    return "ok"


def collect(root, endpoint, network, now):
    checks = {}
    operations = {
        "kms_tls_health_and_certificate": lambda: check_tls(endpoint, root / "openbao/trust/ca.crt", now),
        "agent_live_leases": lambda: check_agents(root, endpoint, network),
        "backup_job": check_backup_unit,
        "snapshot_freshness_and_integrity": lambda: check_snapshot(root / "backup/snapshots", now),
        "disk_and_inodes": lambda: check_disk(root),
        # Successful authenticated Agent queries above must continue to produce audit writes.
        "audit_permissions_and_activity": lambda: check_audit(root / "openbao/audit/audit.jsonl", now),
    }
    for name, check in operations.items():
        try:
            checks[name] = check()
        except Exception:
            checks[name] = "critical"
    return {"version": 1, "checked_at": datetime.fromtimestamp(now, timezone.utc).isoformat(),
            "status": "ok" if all(value == "ok" for value in checks.values()) else "attention_required",
            "checks": checks, "notification_delivery": "local_status_only"}


def publish(directory, evidence):
    private(directory, directory=True, uid=0)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=directory, prefix=".status-", delete=False) as stream:
            temporary = stream.name
            os.chmod(temporary, 0o600)
            json.dump(evidence, stream, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, directory / "status.json")
        temporary = None
        fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if temporary is not None:
            os.unlink(temporary)  # Only the just-created owned temporary file.


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--security-directory", required=True)
    parser.add_argument("--endpoint", required=True)
    parser.add_argument("--network", required=True)
    args = parser.parse_args()
    require(os.name == "posix" and os.geteuid() == 0)
    root = Path(args.security_directory)
    require(root.is_absolute() and root.resolve(strict=True) == root)
    evidence = collect(root, args.endpoint, args.network, time.time())
    publish(root / "monitor", evidence)
    print(json.dumps(evidence, sort_keys=True))
    return 0 if evidence["status"] == "ok" else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        print("KMS_MONITOR_FAILED_NO_VALID_STATUS", file=sys.stderr)
        sys.exit(1)

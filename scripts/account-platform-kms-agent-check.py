"""Verify existing Linux KMS Agents without reading bootstrap or printing secrets."""

import argparse
import base64
from datetime import datetime, timezone
import http.client
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import ssl
import stat
import subprocess
import sys
from urllib.parse import urlsplit


ROLES = {
    "pinvou-token-gateway": "pinvou-kms-agent-gateway",
    "pinvou-credential-portal": "pinvou-kms-agent-credentials",
    "pinvou-kms-backup": "pinvou-kms-agent-backup",
}
IMAGE = "quay.io/openbao/openbao@sha256:11fd73a2102cda9c55d5d881a8c3210303146a7ec1e8ac76f526e175c6d24641"
SINK_ROOT = Path("/run/pinvou-kms-agents")


def require(condition):
    if not condition:
        raise ValueError("KMS_AGENT_CHECK_FAILED")


def private_path(path, mode, uid=65532, directory=False):
    info = path.lstat()
    require(not path.is_symlink() and stat.S_IMODE(info.st_mode) == mode
            and info.st_uid == uid and info.st_gid == uid)
    require(stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode))
    require(path.resolve(strict=True) == path)


def read_token(path):
    private_path(path.parent, 0o700, directory=True)
    private_path(path, 0o600)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600
                and info.st_uid == 65532 and info.st_gid == 65532)
        token = stream.read(4097).decode("ascii")
    require(20 <= len(token) <= 4096 and all(33 <= ord(c) <= 126 for c in token))
    return token


def docker_inspect(kind, name):
    result = subprocess.run(["docker", kind, "inspect", name], capture_output=True, timeout=15, check=False)
    require(result.returncode == 0)
    return json.loads(result.stdout)[0]


def validate_container(data, role, network, agent_root, trust_root):
    config, host = data["Config"], data["HostConfig"]
    require(data["State"]["Running"] and config["Image"] == IMAGE and config["User"] == "65532:65532")
    require(config["Entrypoint"] == ["/usr/bin/bao"] and config["Cmd"] == ["agent", "-config=/bao/config/agent.hcl"])
    require(host["ReadonlyRootfs"] and not host["Privileged"] and host["CapDrop"] == ["ALL"] and not host["CapAdd"])
    require("no-new-privileges:true" in host["SecurityOpt"] and not host["PortBindings"])
    require(host["Memory"] == 128 * 1024 * 1024 and host["NanoCpus"] == 500000000 and host["PidsLimit"] == 64)
    require(any(item == {"Name": "core", "Soft": 0, "Hard": 0} for item in host["Ulimits"]))
    require(set(data["NetworkSettings"]["Networks"]) == {network})
    require(host["RestartPolicy"]["Name"] == "unless-stopped" and host["LogConfig"]["Type"] == "local")
    require(all(not item.split("=", 1)[0].endswith(("TOKEN", "SECRET_ID", "ROLE_ID")) for item in config["Env"]))
    expected = {
        "/bao/config": (str(agent_root / "config"), False),
        "/bao/trust": (str(trust_root), False),
        "/bao/bootstrap": (str(agent_root / "bootstrap" / role), False),
        "/bao/token": (str(SINK_ROOT / role), True),
    }
    actual = {item["Destination"]: (item["Source"], item["RW"]) for item in data["Mounts"] if item["Type"] == "bind"}
    require(actual == expected and all(item["Type"] in ("bind", "tmpfs") for item in data["Mounts"]))
    require(set(host["Tmpfs"]) == {"/tmp", "/openbao/file", "/openbao/logs"})
    require(all(all(flag in options.split(",") for flag in ("nosuid", "nodev", "noexec")) for options in host["Tmpfs"].values()))


def validate_lease(data, role):
    require(data["policies"] == [role] and data["type"] == "service" and data["renewable"] is True)
    require(data["orphan"] is True and 0 < data["ttl"] <= 300 and data["explicit_max_ttl"] == 900)
    require(data["path"] == "auth/approle/login" and data["meta"]["role_name"] == role)


def validate_renewal(data):
    # OpenBao returns last_renewal as RFC3339, not a Unix timestamp.
    value = data.get("last_renewal")
    require(isinstance(value, str))
    parts = re.fullmatch(r"(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})", value)
    require(parts is not None)
    # Python 3.10 accepts microseconds, whereas Go emits RFC3339Nano.
    fraction = (parts[2] or "0").ljust(6, "0")[:6]
    renewed = datetime.fromisoformat(parts[1] + "." + fraction + parts[3].replace("Z", "+00:00"))
    require(renewed.tzinfo is not None)
    require(data["creation_time"] < renewed.timestamp() <= datetime.now(timezone.utc).timestamp() + 5)


class API:
    def __init__(self, endpoint, ca):
        url = urlsplit(endpoint)
        require(url.scheme == "https" and url.port == 8200 and not url.username and not url.password
                and not url.path and not url.query and not url.fragment)
        require(ipaddress.ip_address(url.hostname).is_private)
        self.host = url.hostname
        self.context = ssl.create_default_context(cafile=str(ca))
        self.context.minimum_version = ssl.TLSVersion.TLSv1_2

    def call(self, path, token, body=None, expected=200, snapshot=False):
        connection = http.client.HTTPSConnection(self.host, 8200, context=self.context, timeout=10)
        try:
            # Direct TLS connection: no environment proxy, redirects, or automatic retry.
            headers = {"X-Vault-Token": token, "Content-Type": "application/json"}
            connection.request("GET" if body is None else "POST", "/v1/" + path,
                               body=None if body is None else json.dumps(body), headers=headers)
            response = connection.getresponse()
            limit = 16 * 1024 * 1024 if snapshot else 65536
            raw = response.read(limit + 1)
            require(response.status == expected and len(raw) <= limit)
            if snapshot:
                require(len(raw) >= 64 and raw[:2] == b"\x1f\x8b")
            elif expected == 200:
                return json.loads(raw)["data"]
        finally:
            connection.close()


def verify_permissions(api, role, token):
    for path in ("sys/auth", "sys/policies/acl/" + role, "transit/keys/pinvou-token-data",
                 "transit/export/encryption-key/pinvou-token-data"):
        api.call(path, token, expected=403)
    if role == "pinvou-kms-backup":
        api.call("transit/encrypt/pinvou-token-data", token, {}, expected=403)
        api.call("sys/storage/raft/snapshot", token, snapshot=True)
        return
    api.call("sys/storage/raft/snapshot", token, expected=403)
    plaintext = base64.b64encode(secrets.token_bytes(32)).decode("ascii")
    aad = base64.b64encode(b"pinvou-agent-deployment-verification-v1").decode("ascii")
    encrypted = api.call("transit/encrypt/pinvou-token-data", token, {"plaintext": plaintext, "associated_data": aad})
    body = {"ciphertext": encrypted["ciphertext"], "associated_data": aad}
    require(api.call("transit/decrypt/pinvou-token-data", token, body)["plaintext"] == plaintext)
    api.call("transit/encrypt/pinvou-token-data", token, {"plaintext": plaintext}, expected=403)
    body["associated_data"] = base64.b64encode(b"wrong-aad").decode("ascii")
    api.call("transit/decrypt/pinvou-token-data", token, body, expected=400)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--endpoint", required=True, help="private HTTPS IP endpoint on port 8200")
    parser.add_argument("--agent-directory", required=True)
    parser.add_argument("--trust-directory", required=True)
    parser.add_argument("--network", required=True)
    parser.add_argument("--require-renewal", action="store_true")
    parser.add_argument("--lease-only", action="store_true", help="read-only routine monitoring, no crypto or snapshot probes")
    args = parser.parse_args()
    require(os.name == "posix" and os.geteuid() == 0)
    root, trust = Path(args.agent_directory), Path(args.trust_directory)
    require(root.is_absolute() and trust.is_absolute())
    private_path(root, 0o700, uid=0, directory=True)
    private_path(SINK_ROOT, 0o700, uid=0, directory=True)
    result = subprocess.run(["findmnt", "-T", str(SINK_ROOT), "-n", "-o", "FSTYPE"], capture_output=True, timeout=5)
    require(result.returncode == 0 and result.stdout.strip() == b"tmpfs")
    require(docker_inspect("network", args.network)["Internal"] is True)
    private_path(trust, 0o700, directory=True)
    private_path(trust / "ca.crt", 0o600)
    api = API(args.endpoint, trust / "ca.crt")
    server = docker_inspect("container", "pinvou-security-openbao")
    require(server["NetworkSettings"]["Networks"][args.network]["IPAddress"] == api.host)
    tokens = set()
    for role, name in ROLES.items():
        private_path(root / "bootstrap" / role, 0o700, directory=True)
        for filename in ("role-id", "secret-id"):
            private_path(root / "bootstrap" / role / filename, 0o600)
        validate_container(docker_inspect("container", name), role, args.network, root, trust)
        token = read_token(SINK_ROOT / role / "token")
        require(token not in tokens)
        tokens.add(token)
        lease = api.call("auth/token/lookup-self", token)
        validate_lease(lease, role)
        if args.require_renewal:
            validate_renewal(lease)
        if not args.lease_only:
            verify_permissions(api, role, token)
    print("KMS_AGENTS_LIVE_LEASES_VERIFIED" if args.lease_only else
          "KMS_AGENTS_RENEWAL_VERIFIED" if args.require_renewal else "KMS_AGENTS_VERIFIED")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        # Do not print exception objects, tracebacks, paths, response bodies or secrets.
        print("KMS_AGENT_CHECK_FAILED", file=sys.stderr)
        sys.exit(1)

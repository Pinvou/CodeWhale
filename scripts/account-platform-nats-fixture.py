"""Opt-in exact production Compose test with fresh ephemeral NKey/TLS fixtures."""
import argparse
import errno
import hashlib
import json
import os
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import tempfile
import time

IMAGE = "sha256:e4bf19f15fd3218814a4e3c9e0064e1334bd8aa20d5984b9f1a0afd084f8cc00"


def require(ok, code):
    if not ok:
        raise RuntimeError(code)


def run(args, env=None, timeout=60):
    result = subprocess.run(args, capture_output=True, text=True, env=env, timeout=timeout)
    require(result.returncode == 0, "NATS_FIXTURE_COMMAND_FAILED_" + Path(args[0]).name)
    return result.stdout.strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", required=True, action="store_true")
    parser.add_argument("--bin-directory", required=True, type=Path)
    args = parser.parse_args()
    require(os.name == "posix" and os.geteuid() == 0, "LINUX_ROOT_FIXTURE_ONLY")
    bins = args.bin_directory.resolve(strict=True)
    for binary in ("age-keygen", "nats-identities", "event-resources"):
        require((bins / binary).is_file(), "NATS_FIXTURE_BINARY_MISSING")
    source = Path(__file__).resolve().parents[1] / "account-platform/deploy/production"
    run(["docker", "image", "inspect", IMAGE])
    project = "pinvou-nats-fixture-" + secrets.token_hex(8)
    container_id = network_id = None
    with tempfile.TemporaryDirectory(prefix="pinvou-nats-fixture-") as raw:
        root = Path(raw)
        root.chmod(0o700)
        for name in ("data", "tls", "trust"):
            (root / name).mkdir(mode=0o700)
            os.chown(root / name, 65532, 65532)
        run([str(bins / "age-keygen"), "-o", str(root / "fixture.agekey")])
        recipient = run([str(bins / "age-keygen"), "-y", str(root / "fixture.agekey")])
        (root / "recipient").write_text(recipient)
        (root / "recipient").chmod(0o600)
        (root / "identities-config.json").write_text(json.dumps({
            "recipient_file": str(root / "recipient"),
            "recipient_sha256": hashlib.sha256(recipient.encode()).hexdigest(),
            "output_directory": str(root / "identities")}))
        (root / "identities-config.json").chmod(0o600)
        run([str(bins / "nats-identities"), "-config", str(root / "identities-config.json")])
        public = root / "identities/public-keys.json"
        run([str(bins / "event-resources"), "-render-authorization", "-public-keys", str(public),
             "-public-keys-sha256", hashlib.sha256(public.read_bytes()).hexdigest(),
             "-output", str(root / "authorization.conf")])
        # Independent trusted digest above is appropriate only for newly generated test output.
        os.chown(root / "authorization.conf", 65532, 65532)
        operator = root / "identities/event-resource-operator"
        os.chown(operator, 65532, 65532)
        os.chown(operator / "nkey.seed", 65532, 65532)
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
             "-subj", "/CN=Temporary NATS fixture CA", "-addext", "basicConstraints=critical,CA:TRUE",
             "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-keyout", str(root / "ca.key"),
             "-out", str(root / "trust/ca.crt")])
        run(["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=nats",
             "-keyout", str(root / "tls/server.key"), "-out", str(root / "server.csr")])
        (root / "ext").write_text("subjectAltName=DNS:nats\nextendedKeyUsage=serverAuth\n")
        run(["openssl", "x509", "-req", "-in", str(root / "server.csr"), "-CA", str(root / "trust/ca.crt"),
             "-CAkey", str(root / "ca.key"), "-CAcreateserial", "-days", "2", "-extfile", str(root / "ext"),
             "-out", str(root / "tls/server.crt")])
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
             "-subj", "/CN=Wrong NATS CA", "-keyout", str(root / "wrong.key"), "-out", str(root / "wrong.crt")])
        for name in ("tls/server.crt", "tls/server.key", "trust/ca.crt"):
            (root / name).chmod(0o600)
            os.chown(root / name, 65532, 65532)
        env = {**os.environ, "NATS_IMAGE": IMAGE, "NATS_DATA_DIRECTORY": str(root / "data"),
               "NATS_TLS_DIRECTORY": str(root / "tls"), "NATS_AUTHORIZATION_FILE": str(root / "authorization.conf")}
        compose = ["docker", "compose", "--project-name", project, "-f", str(source / "compose.nats.yaml")]

        def dc(*args):
            return run([*compose, *args], env)

        def inspect():
            return json.loads(run(["docker", "inspect", container_id]))[0]

        def ready():
            for _ in range(80):
                state = inspect()["State"]
                require(state["Running"], "NATS_SERVER_EXITED")
                if state.get("Health", {}).get("Status") == "healthy":
                    return
                time.sleep(0.25)
            raise RuntimeError("NATS_SERVER_NOT_HEALTHY")

        def resources(*args):
            result = run(["docker", "run", "--rm", "--read-only", "--user", "65532:65532", "--cap-drop", "ALL",
                          "--network", network_id, "--entrypoint", "/fixture/event-resources",
                          "--mount", f"type=bind,src={bins / 'event-resources'},dst=/fixture/event-resources,readonly",
                          "--mount", f"type=bind,src={operator},dst=/fixture/identity,readonly",
                          "--mount", f"type=bind,src={root / 'trust'},dst=/fixture/trust,readonly",
                          "--env", "NATS_URL=tls://nats:4222", "--env", "NATS_CA_FILE=/fixture/trust/ca.crt",
                          "--env", "NATS_NKEY_SEED_FILE=/fixture/identity/nkey.seed", IMAGE, *args])
            require(result == "NATS_FIXED_RESOURCES_VERIFIED", "NATS_RESOURCE_VERIFICATION_FAILED")

        def connection():
            sock = socket.create_connection((ip, 4222), timeout=4)
            info = bytearray()
            while not info.endswith(b"\r\n") and len(info) < 16384:
                part = sock.recv(1)
                require(bool(part), "NATS_INFO_TRUNCATED")
                info.extend(part)
            require(info.startswith(b"INFO "), "NATS_INFO_MISSING")
            data = json.loads(info[5:])
            require(data.get("tls_required") is True and data.get("auth_required") is True, "NATS_TLS_AUTH_NOT_REQUIRED")
            return sock

        try:
            config = json.loads(dc("config", "--format", "json"))
            require(set(config["services"]) == {"nats"}, "NATS_UNEXPECTED_SERVICE")
            dc("create", "nats")
            container_id = dc("ps", "--all", "--quiet", "nats")
            network = json.loads(run(["docker", "network", "inspect", config["networks"]["data"]["name"]]))[0]
            require(network["Internal"] and network["Labels"]["com.docker.compose.project"] == project, "NATS_WRONG_NETWORK")
            network_id = network["Id"]
            actual = inspect()
            require(actual["Config"]["User"] == "65532:65532" and actual["HostConfig"]["ReadonlyRootfs"], "NATS_CONTAINER_ISOLATION")
            require(actual["HostConfig"]["PortBindings"] == {}, "NATS_PUBLISHED_PORT")
            binds = [m for m in actual["Mounts"] if m["Type"] == "bind"]
            require(len(binds) == 4 and all(m["RW"] == (m["Destination"] == "/data") for m in binds), "NATS_MOUNT_ISOLATION")
            require(all(m["Type"] in ("bind", "tmpfs") for m in actual["Mounts"]), "NATS_ANONYMOUS_VOLUME")
            require(actual["HostConfig"]["Memory"] == 512 * 1024 * 1024 and actual["HostConfig"]["PidsLimit"] == 64,
                    "NATS_RESOURCE_LIMITS")
            run(["docker", "update", "--restart=no", container_id])
            run(["docker", "start", container_id])
            ready()
            ip = next(iter(inspect()["NetworkSettings"]["Networks"].values()))["IPAddress"]
            context = ssl.create_default_context(cafile=str(root / "trust/ca.crt"))
            with context.wrap_socket(connection(), server_hostname="nats") as conn:
                require(conn.version() in ("TLSv1.2", "TLSv1.3"), "NATS_TLS_VERSION")
                conn.sendall(b'CONNECT {}\r\nPING\r\n')
                require(b"Authorization Violation" in conn.recv(4096), "NATS_ANONYMOUS_NOT_REJECTED")
            with connection() as conn:
                conn.sendall(b'CONNECT {}\r\nPING\r\n')
                try:
                    response = conn.recv(4096)
                    require(response == b"", "NATS_PLAINTEXT_NOT_CLOSED")
                except ConnectionResetError:
                    pass
            wrong = ssl.create_default_context(cafile=str(root / "wrong.crt"))
            try:
                with wrong.wrap_socket(connection(), server_hostname="nats"):
                    raise RuntimeError("NATS_WRONG_CA_ACCEPTED")
            except ssl.SSLCertVerificationError:
                pass
            with socket.socket() as monitor:
                monitor.settimeout(3)
                require(monitor.connect_ex((ip, 8222)) == errno.ECONNREFUSED, "NATS_MONITOR_PUBLIC")
            resources("-create-only")
            resources()
            run(["docker", "restart", "--time", "10", container_id])
            ready()
            resources()  # Strict read-only: cannot recreate lost stream/consumers.
            logs = subprocess.run(["docker", "logs", container_id], capture_output=True, text=True, timeout=10)
            require(logs.returncode == 0, "NATS_LOG_READ_FAILED")
            for seed in (root / "identities").glob("*/nkey.seed"):
                require(seed.read_text().strip() not in logs.stdout + logs.stderr, "NATS_SEED_LOGGED")
            print("PRODUCTION_NATS_TLS_NKEY_PERSISTENCE_PRIVATE_MONITOR_PASS")
        finally:
            if container_id:
                require(inspect()["Config"]["Labels"]["com.docker.compose.project"] == project, "NATS_CLEANUP_PROJECT")
                run(["docker", "rm", "-f", container_id])
            if network_id:
                network = json.loads(run(["docker", "network", "inspect", network_id]))[0]
                require(network["Labels"]["com.docker.compose.project"] == project and not network["Containers"], "NATS_CLEANUP_NETWORK")
                run(["docker", "network", "rm", network_id])


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, subprocess.TimeoutExpired) as exc:
        print(str(exc) if isinstance(exc, RuntimeError) else "NATS_FIXTURE_FAILED")
        raise SystemExit(1)

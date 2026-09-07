"""Run an opt-in, disposable Linux Keycloak fixture with no business mounts."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile

IMAGE = "quay.io/keycloak/keycloak@sha256:ff4257d0d64efbe99ed1ddfaf07765cc3c36dc7518bf8324d41961327f441c54"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--generator", required=True)
    parser.add_argument("--isolated-fixture", action="store_true", required=True)
    args = parser.parse_args()
    if os.name != "posix" or os.geteuid() != 0:
        raise RuntimeError("fixture requires Linux Docker operator with UID setup authority")
    binary, generator = Path(args.binary).resolve(strict=True), Path(args.generator).resolve(strict=True)
    if not binary.is_file() or not generator.is_file():
        raise RuntimeError("fixture inputs must be files")
    spec = importlib.util.spec_from_file_location("workload_realm", generator)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    directory = Path(tempfile.mkdtemp(prefix="pinvou-workload-keycloak-", dir="/tmp")).resolve()
    name = "pinvou-workload-keycloak-" + secrets.token_hex(6)
    started = False
    try:
        os.chmod(directory, 0o755)
        secret = secrets.token_urlsafe(32)
        for filename, data in {
            "admin-secret": secret,
            "fixture.env": "KC_BOOTSTRAP_ADMIN_USERNAME=fixture-admin\nKC_BOOTSTRAP_ADMIN_PASSWORD=" + secret + "\n",
            "realm.json": json.dumps(module.build_realm("pinvou-fixture-workload")),
        }.items():
            path = directory / filename
            with path.open("x", encoding="utf-8") as stream:
                stream.write(data)
            os.chmod(path, 0o600)
            os.chown(path, 1000, 0)
        subprocess.run([
            "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1",
            "-keyout", str(directory / "tls.key"), "-out", str(directory / "tls.crt"),
        ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        os.chmod(directory / "tls.key", 0o600)
        os.chown(directory / "tls.key", 1000, 0)
        target_binary = directory / "workload.test"
        shutil.copyfile(binary, target_binary)
        os.chmod(target_binary, 0o555)
        command = [
            "docker", "create", "--name", name, "--network", "none",
            "--log-driver", "local", "--log-opt", "max-size=1m", "--log-opt", "max-file=2",
            "--memory", "1536m", "--cpus", "1", "--pids-limit", "256",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--env-file", str(directory / "fixture.env"),
            "--env", "JAVA_OPTS_KC_HEAP=-Xms128m -Xmx512m",
            "--mount", "type=bind,src=" + str(directory) + ",dst=/fixture,readonly",
            "--mount", "type=bind,src=" + str(directory / "realm.json") + ",dst=/opt/keycloak/data/import/pinvou-fixture-workload-realm.json,readonly",
            "--tmpfs", "/tmp:rw,nosuid,nodev,size=128m,mode=1777",
            IMAGE, "start-dev", "--log-level=WARN", "--http-enabled=false", "--hostname=https://localhost:8443",
            "--https-certificate-file=/fixture/tls.crt", "--https-certificate-key-file=/fixture/tls.key", "--import-realm",
        ]
        # Fresh container writable layer holds only disposable H2/augmentation
        # data. There are no named volumes, shared networks, or published ports.
        launched = subprocess.run(command, capture_output=True, text=True, check=False)
        if launched.returncode:
            diagnostic = launched.stderr.strip()
            if any(word in diagnostic.lower() for word in ("password", "secret", "token", "authorization")):
                diagnostic = "sensitive launch diagnostic suppressed"
            print("fixture container launch failed: " + diagnostic[:400])
            raise RuntimeError("fixture container launch failed")
        started = True
        subprocess.run(["docker", "start", name], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        result = subprocess.run([
            "docker", "exec", "--env", "SSL_CERT_FILE=/fixture/tls.crt", "--env", "GOMEMLIMIT=128MiB",
            "--env", "PINVOU_TEST_KEYCLOAK_ISOLATED=1", name,
            "/fixture/workload.test", "-test.run=^TestKeycloakWorkloadIntegration$", "-test.v", "-test.timeout=240s",
        ], check=False, capture_output=True, text=True, timeout=250)
        print(result.stdout, end="")
        pressure = subprocess.run(["docker", "exec", name, "cat", "/sys/fs/cgroup/memory.events"],
                                  capture_output=True, text=True, check=False)
        if pressure.returncode == 0:
            counts = {}
            for line in pressure.stdout.splitlines():
                parts = line.split()
                if len(parts) == 2 and parts[0] in ("oom", "oom_kill", "max") and parts[1].isdigit():
                    counts[parts[0]] = int(parts[1])
            print(json.dumps({"fixture_memory_events": counts}))
        if result.returncode or "--- PASS: TestKeycloakWorkloadIntegration" not in result.stdout or "SKIP" in result.stdout:
            logs = subprocess.run(["docker", "logs", "--tail", "30", name], capture_output=True, text=True, check=False)
            for line in (logs.stdout + logs.stderr).splitlines():
                # This is a fresh issuer with only random fixture credentials.
                # Print startup error stages, never auth/debug/request logs.
                if "error" in line.lower() or "caused by" in line.lower():
                    if any(word in line.lower() for word in ("password", "secret", "token", "authorization")):
                        print("fixture startup error: sensitive diagnostic suppressed")
                    else:
                        print(line[:400])
            inspected = subprocess.run(["docker", "inspect", name], capture_output=True, text=True, check=False)
            if inspected.returncode == 0:
                state = json.loads(inspected.stdout)[0]["State"]
                print(json.dumps({"fixture_test_exit": result.returncode, "container_exit": state["ExitCode"],
                                  "oom_killed": state["OOMKilled"], "running": state["Running"]}))
            raise RuntimeError("real Keycloak workload fixture failed; no production changes made")
    finally:
        # Exact owned container only; never prune other containers or volumes.
        if started:
            cleanup = subprocess.run(["docker", "rm", "--force", name], capture_output=True, check=False)
            if cleanup.returncode != 0:
                raise RuntimeError("owned fixture container cleanup failed")
        if directory.parent != Path("/tmp") or not directory.name.startswith("pinvou-workload-keycloak-"):
            raise RuntimeError("unsafe fixture cleanup target")
        shutil.rmtree(directory)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, subprocess.SubprocessError, OSError, ValueError):
        raise SystemExit("Isolated Keycloak fixture failed; inspect fixed test-stage diagnostics only.") from None

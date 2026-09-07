"""Exercise TLS/Raft/Agent using fresh disposable OpenBao infrastructure only."""

import argparse
import base64
import ipaddress
import json
import os
import re
from pathlib import Path
import secrets
import shutil
import socket
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

IMAGE = "quay.io/openbao/openbao@sha256:11fd73a2102cda9c55d5d881a8c3210303146a7ec1e8ac76f526e175c6d24641"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def require(condition, stage):
    if not condition:
        raise RuntimeError(stage)


def docker(*args):
    result = subprocess.run(["docker", *args], capture_output=True, text=True, check=False, timeout=45)
    if result.returncode:
        diagnostic = result.stderr.strip()
        if any(word in diagnostic.lower() for word in ("password", "secret", "authorization", "plaintext")):
            diagnostic = "sensitive diagnostic suppressed"
        print("fixture Docker diagnostic: " + diagnostic[:400], flush=True)
    require(result.returncode == 0, "docker operation failed: " + args[0])
    return result.stdout.strip()


def private_write(path, data):
    with path.open("xb") as stream:
        stream.write(data.encode() if isinstance(data, str) else data)
    os.chmod(path, 0o600)
    os.chown(path, 65532, 65532)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config-directory", required=True)
    parser.add_argument("--isolated-fixture", action="store_true", required=True)
    recovery = parser.add_mutually_exclusive_group()
    recovery.add_argument("--initializer-test-binary", help="optional Linux Go initializer test binary; runs initialization suite")
    recovery.add_argument("--operator-test-binary", help="optional Linux Go operator test binary; runs operator suite")
    parser.add_argument("--restore-fixture", action="store_true", help="create a second isolated empty Raft instance for the operator restore proof")
    parser.add_argument("--bootstrap-fixture", action="store_true", help="test bounded role bootstrap and initial root retirement")
    args = parser.parse_args()
    require(os.name == "posix" and os.geteuid() == 0, "Linux Docker operator required")
    require(not args.restore_fixture or args.operator_test_binary, "restore proof requires the operator test binary")
    require(not args.bootstrap_fixture or (args.operator_test_binary and not args.restore_fixture), "bootstrap proof requires operator and excludes restore")
    source = Path(args.config_directory).resolve(strict=True)
    initializer_binary = None
    if args.initializer_test_binary or args.operator_test_binary:
        initializer_binary = Path(args.initializer_test_binary or args.operator_test_binary).resolve(strict=True)
        require(initializer_binary.is_file() and initializer_binary.is_absolute(), "initializer test binary missing")
    require(all((source / name).is_file() for name in ("server.hcl", "agent.hcl", "runtime-policy.hcl", "approle.json")), "public fixture config missing")
    root = Path(tempfile.mkdtemp(prefix="pinvou-openbao-fixture-", dir="/tmp")).resolve()
    prefix = "pinvou-openbao-fixture-" + secrets.token_hex(5)
    network, server = prefix + "-net", prefix + "-server"
    restore_network, restore_server = prefix + "-restore-net", prefix + "-restore-server"
    restore_network_created = False
    restore_ip = ""
    containers = []
    network_created = False
    try:
        os.chmod(root, 0o755)
        for name in ("config", "tls", "trust", "data", "audit", "restore-data", "restore-audit", "gateway-bootstrap", "gateway-token", "credential-bootstrap", "credential-token"):
            path = root / name
            path.mkdir(mode=0o700)
            os.chown(path, 65532, 65532)
        private_write(root / "config" / "server.hcl", (source / "server.hcl").read_bytes())
        private_write(root / "config" / "agent.hcl", (source / "agent.hcl").read_bytes())
        docker("network", "create", "--internal", network)
        network_created = True
        network_info = json.loads(docker("network", "inspect", network))[0]
        require(network_info["Internal"] is True, "fixture network is not isolated")
        subnet = ipaddress.ip_network(network_info["IPAM"]["Config"][0]["Subnet"])
        require(subnet.version == 4 and subnet.is_private, "fixture subnet is not private IPv4")
        server_ip = str(subnet.network_address + 2)
        if args.restore_fixture:
            docker("network", "create", "--internal", restore_network)
            restore_network_created = True
            restore_info = json.loads(docker("network", "inspect", restore_network))[0]
            restore_subnet = ipaddress.ip_network(restore_info["IPAM"]["Config"][0]["Subnet"])
            require(restore_info["Internal"] is True and restore_subnet.is_private and restore_subnet.version == 4 and not restore_subnet.overlaps(subnet), "restore network is not isolated")
            restore_ip = str(restore_subnet.network_address + 2)
        # Test-only CA stays on the operator side, never mounted into the server.
        for command in (
            ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=isolated-fixture-ca", "-keyout", str(root / "ca.key"), "-out", str(root / "trust" / "ca.crt")],
            ["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=openbao", "-keyout", str(root / "tls" / "server.key"), "-out", str(root / "server.csr")],
        ):
            subprocess.run(command, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        private_write(root / "extensions", "subjectAltName=DNS:openbao,IP:" + server_ip + (",IP:" + restore_ip if restore_ip else "") + "\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n")
        subprocess.run(["openssl", "x509", "-req", "-in", str(root / "server.csr"), "-CA", str(root / "trust" / "ca.crt"), "-CAkey", str(root / "ca.key"), "-CAcreateserial", "-days", "1", "-extfile", str(root / "extensions"), "-out", str(root / "tls" / "server.crt")], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        os.chmod(root / "ca.key", 0o600)
        for path in (root / "tls" / "server.key", root / "tls" / "server.crt", root / "trust" / "ca.crt"):
            os.chmod(path, 0o600)
            os.chown(path, 65532, 65532)
        common = ["--network", network, "--user", "65532:65532", "--read-only", "--cap-drop", "ALL", "--cap-add", "IPC_LOCK", "--security-opt", "no-new-privileges", "--ulimit", "memlock=-1:-1", "--ulimit", "core=0:0", "--pids-limit", "128", "--cpus", "1", "--log-driver", "local", "--log-opt", "max-size=1m", "--log-opt", "max-file=2", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=16m,mode=1777", "--tmpfs", "/openbao/file:rw,nosuid,nodev,noexec,size=1m,mode=0700,uid=65532,gid=65532", "--tmpfs", "/openbao/logs:rw,nosuid,nodev,noexec,size=1m,mode=0700,uid=65532,gid=65532", "--entrypoint", "/usr/bin/bao"]
        mounts = lambda pairs: [arg for src, dst, readonly in pairs for arg in ("--mount", "type=bind,src=" + str(root / src) + ",dst=" + dst + (",readonly" if readonly else ""))]
        server_args = ["create", "--name", server, *common, "--memory", "512m", "--network-alias", "openbao", "--ip", server_ip, *mounts((("config", "/bao/config", True), ("tls", "/bao/tls", True), ("data", "/bao/data", False), ("audit", "/bao/audit", False))), IMAGE, "server", "-config=/bao/config/server.hcl"]
        docker(*server_args)
        containers.append(server)
        docker("start", server)
        inspected = json.loads(docker("inspect", server))[0]
        require(not inspected["HostConfig"]["PortBindings"], "fixture published a host port")
        require(inspected["NetworkSettings"]["Networks"][network]["IPAddress"] == server_ip, "fixture IP does not match its dedicated network")
        endpoint = "https://" + server_ip + ":8200"
        context = ssl.create_default_context(cafile=str(root / "trust" / "ca.crt"))
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=context), NoRedirect())

        def api(path, body=None, token="", method=None):
            encoded = None if body is None else json.dumps(body).encode()
            request = urllib.request.Request(endpoint + "/v1/" + path, data=encoded, method=method or ("POST" if body is not None else "GET"), headers={"Content-Type": "application/json", "X-Vault-Token": token})
            try:
                response = opener.open(request, timeout=5)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                raw = response.read(2 * 1024 * 1024 + 1)
                require(len(raw) <= 2 * 1024 * 1024, "fixture response exceeded bound")
                return response.status, json.loads(raw) if raw else {}

        def expect(path, body=None, token="", statuses=(200, 204), method=None):
            status, result = api(path, body, token, method)
            require(status in statuses, "unexpected API status at " + path + ": " + str(status))
            return result

        def wait_ready(sealed=False):
            for _ in range(45):
                try:
                    status, result = api("sys/seal-status" if sealed else "sys/health")
                    if status == 200 and (not sealed or result.get("sealed") is True):
                        return
                except (OSError, urllib.error.URLError):
                    pass
                time.sleep(0.5)
            raise RuntimeError("fixture readiness failed")

        wait_ready(sealed=True)
        if args.restore_fixture:
            restore_common = list(common)
            restore_common[restore_common.index("--network") + 1] = restore_network
            docker("create", "--name", restore_server, *restore_common, "--memory", "512m", "--network-alias", "openbao", "--ip", restore_ip, *mounts((("config", "/bao/config", True), ("tls", "/bao/tls", True), ("restore-data", "/bao/data", False), ("restore-audit", "/bao/audit", False))), IMAGE, "server", "-config=/bao/config/server.hcl")
            containers.append(restore_server)
            docker("start", restore_server)
            restored_info = json.loads(docker("inspect", restore_server))[0]
            require(not restored_info["HostConfig"]["PortBindings"] and list(restored_info["NetworkSettings"]["Networks"]) == [restore_network], "restore instance has unexpected exposure or network")
            source_endpoint = endpoint
            endpoint = "https://" + restore_ip + ":8200"
            wait_ready(sealed=True)
            endpoint = source_endpoint
        # TLS negatives send no credentials.
        try:
            urllib.request.urlopen(endpoint + "/v1/sys/health", context=ssl.create_default_context(), timeout=3)
        except urllib.error.URLError as error:
            require(isinstance(error.reason, ssl.SSLCertVerificationError), "unexpected TLS negative failure")
        else:
            raise RuntimeError("invalid TLS trust accepted")
        try:
            with socket.create_connection((server_ip, 8200), timeout=3) as stream:
                with context.wrap_socket(stream, server_hostname="wrong.fixture.invalid"):
                    raise RuntimeError("invalid TLS hostname accepted")
        except ssl.SSLCertVerificationError:
            pass
        if initializer_binary:
            environment = dict(os.environ)
            environment.update(PINVOU_TEST_OPENBAO_INITIALIZER_ISOLATED="1", PINVOU_TEST_OPENBAO_ENDPOINT=endpoint, PINVOU_TEST_OPENBAO_FIXTURE_ROOT=str(root))
            if args.operator_test_binary:
                environment["PINVOU_TEST_OPENBAO_OPERATOR_ISOLATED"] = "1"
            if args.restore_fixture:
                environment["PINVOU_TEST_OPENBAO_RESTORE_ENDPOINT"] = "https://" + restore_ip + ":8200"
            if args.bootstrap_fixture:
                environment["PINVOU_TEST_OPENBAO_BOOTSTRAP_ISOLATED"] = "1"
            result = subprocess.run([str(initializer_binary), "-test.v", "-test.timeout=120s"], env=environment, capture_output=True, text=True, check=False, timeout=130)
            # The test emits fixed stages only; do not expose arbitrary subprocess output.
            marker = "ACTUAL_OPENBAO_OPERATOR_UNSEAL_PASS" if args.operator_test_binary else "ACTUAL_OPENBAO_ENCRYPTED_INITIALIZATION_PASS"
            if result.returncode != 0:
                for name in re.findall(r"--- FAIL: (Test[A-Za-z0-9_/]+)", result.stdout):
                    print("fixture failed test: " + name, flush=True)
                for stage in sorted(set(re.findall(r"\b(?:BOOTSTRAP|OPERATOR)_[A-Z0-9_]+\b", result.stdout))):
                    print(stage, flush=True)
            require(result.returncode == 0 and marker in result.stdout and "SKIP" not in result.stdout, "actual recovery integration failed")
            if args.restore_fixture:
                require("ACTUAL_OPENBAO_SNAPSHOT_RESTORE_PASS" in result.stdout, "actual snapshot restore did not execute")
                print("OPENBAO_SNAPSHOT_RESTORE_FIXTURE_PASS", flush=True)
            if args.bootstrap_fixture:
                require("ACTUAL_OPENBAO_BOOTSTRAP_PASS" in result.stdout, "actual bootstrap did not execute")
                print("OPENBAO_BOOTSTRAP_FIXTURE_PASS", flush=True)
            print("OPENBAO_OPERATOR_FIXTURE_PASS" if args.operator_test_binary else "OPENBAO_INITIALIZER_FIXTURE_PASS", flush=True)
            return
        initialized = expect("sys/init", {"secret_shares": 1, "secret_threshold": 1})
        root_token = initialized["root_token"]
        unseal = (initialized.get("keys_base64") or initialized["keys"])[0]
        expect("sys/unseal", {"key": unseal})
        wait_ready()
        expect("sys/mounts/transit", {"type": "transit"}, root_token)
        expect("transit/keys/pinvou-token-data", {"type": "aes256-gcm96", "exportable": False, "allow_plaintext_backup": False, "deletion_allowed": False}, root_token)
        expect("sys/auth/approle", {"type": "approle"}, root_token)
        policies = (source / "runtime-policy.hcl").read_text()
        for service in ("gateway", "credential"):
            policy_name = service + "-runtime"
            expect("sys/policies/acl/" + policy_name, {"policy": policies}, root_token)
            role = json.loads((source / "approle.json").read_text())
            role.update(token_policies=[policy_name], token_ttl=12, token_max_ttl=24, token_explicit_max_ttl=24)
            expect("auth/approle/role/" + service, role, root_token)
            role_id = expect("auth/approle/role/" + service + "/role-id", token=root_token)["data"]["role_id"]
            secret_id = expect("auth/approle/role/" + service + "/secret-id", {}, root_token)["data"]["secret_id"]
            private_write(root / (service + "-bootstrap") / "role-id", role_id)
            private_write(root / (service + "-bootstrap") / "secret-id", secret_id)
            agent_name = prefix + "-" + service
            docker("create", "--name", agent_name, *common, "--memory", "128m", *mounts((("config", "/bao/config", True), ("trust", "/bao/trust", True), (service + "-bootstrap", "/bao/bootstrap", True), (service + "-token", "/bao/token", False))), IMAGE, "agent", "-config=/bao/config/agent.hcl")
            containers.append(agent_name)
            docker("start", agent_name)

        def projected(service):
            path = root / (service + "-token") / "token"
            for _ in range(60):
                if path.exists() and path.stat().st_size > 16:
                    require(path.stat().st_mode & 0o777 == 0o600, "Agent sink permissions differ from 0600")
                    return path.read_text().strip()
                time.sleep(0.25)
            raise RuntimeError("Agent did not project a token")

        runtime = projected("gateway")
        credential = projected("credential")
        require(runtime != credential, "services shared a runtime token")
        lease = expect("auth/token/lookup-self", token=runtime)["data"]
        require(lease["creation_ttl"] <= 12 and lease["explicit_max_ttl"] == 24, "fixture role did not enforce its hard lifetime")
        require(lease["policies"] == ["gateway-runtime"], "gateway inherited an unrelated or default policy")
        require(expect("auth/token/lookup-self", token=credential)["data"]["policies"] == ["credential-runtime"], "credential service inherited an unrelated or default policy")
        aad = base64.b64encode(b"isolated-fixture-associated-data").decode()
        plaintext = base64.b64encode(b"isolated-fixture-plaintext-canary").decode()
        ciphertext = expect("transit/encrypt/pinvou-token-data", {"plaintext": plaintext, "associated_data": aad}, runtime)["data"]["ciphertext"]
        restored = expect("transit/decrypt/pinvou-token-data", {"ciphertext": ciphertext, "associated_data": aad}, credential)["data"]["plaintext"]
        require(restored == plaintext, "separate services cannot decrypt the intended key")
        for path, body in (("transit/keys/pinvou-token-data/rotate", {}), ("transit/keys/other", {}), ("auth/token/create", {}), ("sys/policies/acl/escalation", {"policy": policies}), ("transit/encrypt/pinvou-token-data", {"plaintext": plaintext}), ("transit/encrypt/pinvou-token-data", {"plaintext": plaintext, "associated_data": ""}), ("transit/encrypt/pinvou-token-data", {"plaintext": plaintext, "associated_data": aad, "key_version": 1})):
            expect(path, body, runtime, statuses=(403,))
        for path in ("transit/export/encryption-key/pinvou-token-data", "transit/backup/pinvou-token-data"):
            expect(path, token=runtime, statuses=(403,))
        expect("auth/token/revoke", {"token": credential}, runtime, statuses=(403,))
        expect("transit/decrypt/other-key", {"ciphertext": ciphertext, "associated_data": aad}, runtime, statuses=(403,))
        metadata = expect("transit/keys/pinvou-token-data", token=root_token)["data"]
        require(metadata["type"] == "aes256-gcm96" and metadata["exportable"] is False and metadata["allow_plaintext_backup"] is False and metadata["deletion_allowed"] is False, "Transit key protection differs from policy")
        expect("transit/decrypt/pinvou-token-data", {"ciphertext": ciphertext, "associated_data": base64.b64encode(b"wrong").decode()}, runtime, statuses=(400,))
        expect("transit/keys/pinvou-token-data/rotate", {}, root_token)
        require(expect("transit/encrypt/pinvou-token-data", {"plaintext": plaintext, "associated_data": aad}, runtime)["data"]["ciphertext"].startswith("vault:v2:"), "key rotation did not select latest version")
        print("PASS: TLS, separate AppRoles, bounded ACL, AAD and key rotation", flush=True)
        deadline = time.monotonic() + 35
        while time.monotonic() < deadline and projected("gateway") == runtime:
            time.sleep(0.5)
        require(projected("gateway") != runtime, "Agent did not reauthenticate at explicit maximum TTL")
        # Agent proactively replaces tokens before the predecessor's expiry;
        # replacement is not itself evidence of old-token revocation.
        expired = False
        for _ in range(20):
            if api("auth/token/lookup-self", token=runtime)[0] == 403:
                expired = True
                break
            time.sleep(0.5)
        require(expired, "old Agent token outlived the bounded renewal window")
        audit = (root / "audit" / "audit.jsonl").read_bytes()
        require(b"auth/token/renew-self" in audit, "actual Agent renewal audit missing")
        for value in (plaintext, "isolated-fixture-plaintext-canary", aad, ciphertext, root_token, runtime, credential, unseal):
            require(value.encode() not in audit, "audit leaked a secret canary")
        print("PASS: real Agent renewal/reauthentication and HMAC audit", flush=True)
        # Audit file rotation must continue writing after SIGHUP.
        (root / "audit" / "audit.jsonl").rename(root / "audit" / "previous.jsonl")
        private_write(root / "audit" / "audit.jsonl", "")
        docker("kill", "--signal=HUP", server)
        time.sleep(1)
        expect("transit/decrypt/pinvou-token-data", {"ciphertext": ciphertext, "associated_data": aad}, projected("credential"))
        require((root / "audit" / "audit.jsonl").stat().st_size > 0, "audit did not resume after log rotation")
        docker("restart", "--time", "5", server)
        wait_ready(sealed=True)
        expect("transit/decrypt/pinvou-token-data", {"ciphertext": ciphertext, "associated_data": aad}, projected("gateway"), statuses=(503,))
        expect("sys/unseal", {"key": unseal})
        wait_ready()
        # Agent authentication may race the short test TTL after restart.
        recovered = False
        for _ in range(40):
            status, result = api("transit/decrypt/pinvou-token-data", {"ciphertext": ciphertext, "associated_data": aad}, projected("gateway"))
            if status == 200:
                recovered = result["data"]["plaintext"] == plaintext
                break
            time.sleep(0.5)
        require(recovered, "Raft restart lost old ciphertext or Agent did not recover")
        expect("auth/token/revoke-self", {}, root_token)
        expect("sys/policies/acl/default", token=root_token, statuses=(403,))
        print("PASS: audit reopen, sealed restart, persisted historical decrypt, root revocation", flush=True)
        print("OPENBAO_INFRASTRUCTURE_FIXTURE_PASS", flush=True)
    except Exception:
        # Only startup/operational error stages; no request bodies or secrets.
        for name in containers:
            result = subprocess.run(["docker", "logs", "--tail", "12", name], capture_output=True, text=True, check=False)
            for line in (result.stdout + result.stderr).splitlines():
                if "error" in line.lower():
                    if any(word in line.lower() for word in ("secret", "token", "password", "authorization", "plaintext")):
                        print("fixture diagnostic suppressed", flush=True)
                    else:
                        print(line[:300], flush=True)
        raise
    finally:
        for name in reversed(containers):
            docker("rm", "--force", name)
        if restore_network_created:
            docker("network", "rm", restore_network)
        if network_created:
            docker("network", "rm", network)
        require(root.parent == Path("/tmp") and root.name.startswith("pinvou-openbao-fixture-"), "unsafe fixture cleanup target")
        shutil.rmtree(root)


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as error:
        raise SystemExit("Isolated OpenBao fixture failed: " + str(error)) from None
    except (OSError, ValueError, KeyError, subprocess.SubprocessError):
        raise SystemExit("Isolated OpenBao fixture failed in a local, transport or parsing stage") from None

"""Opt-in production Redis configuration and real Portal/Identity client fixture."""
import argparse
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

IMAGE = "sha256:a7859ed111db3c1f5404a973a4747505d559fb5ca32d37e447afc0ef845a2103"


def require(ok, code):
    if not ok:
        raise RuntimeError(code)


def run(args, env=None, timeout=60):
    result = subprocess.run(args, capture_output=True, text=True, env=env, timeout=timeout)
    if result.returncode != 0 and "/fixture/client.test" in args:
        # Selected fixture tests use fixed failure messages, never connection data.
        for line in result.stdout.splitlines():
            if "_test.go:" in line:
                print(line[:400], flush=True)
    require(result.returncode == 0, "REDIS_FIXTURE_COMMAND_FAILED_" + Path(args[0]).name)
    return result.stdout.strip()


class RedisError(Exception):
    pass


class Client:
    def __init__(self, ip, hostname, ca, username=None, password=None):
        context = ssl.create_default_context(cafile=str(ca))
        self.sock = context.wrap_socket(socket.create_connection((ip, 6379), timeout=4), server_hostname=hostname)
        self.reader = self.sock.makefile("rb")
        if username:
            try:
                require(self.command("AUTH", username, password) == "OK", "REDIS_AUTH_NOT_OK")
            except BaseException:
                self.close()
                raise

    def read(self):
        line = self.reader.readline(65536)
        require(line.endswith(b"\r\n"), "REDIS_INVALID_RESPONSE")
        kind, value = line[:1], line[1:-2]
        if kind == b"-":
            raise RedisError(value.decode())
        if kind == b"+":
            return value.decode()
        if kind == b":":
            return int(value)
        if kind == b"$":
            size = int(value)
            if size == -1:
                return None
            require(0 <= size <= 1 << 20, "REDIS_RESPONSE_TOO_LARGE")
            body = self.reader.read(size + 2)
            require(len(body) == size + 2 and body[-2:] == b"\r\n", "REDIS_BULK_TRUNCATED")
            return body[:-2].decode()
        if kind == b"*":
            require(0 <= int(value) <= 2048, "REDIS_ARRAY_TOO_LARGE")
            return [self.read() for _ in range(int(value))]
        raise RuntimeError("REDIS_UNEXPECTED_RESPONSE")

    def command(self, *args):
        parts = [str(arg).encode() for arg in args]
        self.sock.sendall(b"*%d\r\n" % len(parts) + b"".join(b"$%d\r\n" % len(p) + p + b"\r\n" for p in parts))
        return self.read()

    def close(self):
        self.reader.close()
        self.sock.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", required=True, action="store_true")
    parser.add_argument("--bin-directory", required=True, type=Path)
    args = parser.parse_args()
    require(os.name == "posix" and os.geteuid() == 0, "LINUX_ROOT_FIXTURE_ONLY")
    bins = args.bin_directory.resolve(strict=True)
    for binary in ("redis-portal.test", "redis-identity.test", "redis-options.test"):
        require((bins / binary).is_file(), "REDIS_CLIENT_BINARY_MISSING")
    source = Path(__file__).resolve().parents[1] / "account-platform/deploy/production"
    run(["docker", "image", "inspect", IMAGE])
    project = "pinvou-redis-fixture-" + secrets.token_hex(8)
    ids, clients, network_id = {}, [], None
    with tempfile.TemporaryDirectory(prefix="pinvou-redis-fixture-") as raw:
        root = Path(raw)
        root.chmod(0o700)
        passwords = {name: secrets.token_hex(32) for name in ("portal", "identity", "newapi")}
        (root / "trust").mkdir(mode=0o700)
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
             "-subj", "/CN=Temporary Redis fixture CA", "-addext", "basicConstraints=critical,CA:TRUE",
             "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-keyout", str(root / "ca.key"), "-out", str(root / "trust/ca.crt")])
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2", "-subj", "/CN=Wrong Redis CA",
             "-keyout", str(root / "wrong.key"), "-out", str(root / "wrong.crt")])
        env = {**os.environ, "REDIS_IMAGE": IMAGE}
        for name, prefix, template in (("redis", "REDIS", "redis-accounts.acl.template"), ("newapi-redis", "NEWAPI_REDIS", "redis-newapi.acl.template")):
            for suffix in ("data", "tls"):
                (root / f"{name}-{suffix}").mkdir(mode=0o700)
                os.chown(root / f"{name}-{suffix}", 65532, 65532)
            leaf = root / f"{name}-tls"
            run(["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", f"/CN={name}", "-keyout", str(leaf / "server.key"), "-out", str(root / "server.csr")])
            (root / "ext").write_text(f"subjectAltName=DNS:{name}\nextendedKeyUsage=serverAuth\n")
            run(["openssl", "x509", "-req", "-in", str(root / "server.csr"), "-CA", str(root / "trust/ca.crt"), "-CAkey", str(root / "ca.key"), "-CAcreateserial", "-days", "2", "-extfile", str(root / "ext"), "-out", str(leaf / "server.crt")])
            acl = (source / template).read_text()
            for user, password in passwords.items():
                acl = acl.replace(user.upper() + "_PASSWORD_SHA256", hashlib.sha256(password.encode()).hexdigest())
            (root / f"{name}.acl").write_text(acl)
            for file in (leaf / "server.key", leaf / "server.crt", root / f"{name}.acl"):
                file.chmod(0o600)
                os.chown(file, 65532, 65532)
            env.update({prefix + "_DATA_DIRECTORY": str(root / f"{name}-data"), prefix + "_TLS_DIRECTORY": str(leaf), prefix + "_ACL_FILE": str(root / f"{name}.acl")})
        (root / "trust/ca.crt").chmod(0o600)
        os.chown(root / "trust", 65532, 65532)
        os.chown(root / "trust/ca.crt", 65532, 65532)
        config = root / "client.json"
        config.write_text(json.dumps({"portal_url": f"rediss://portal:{passwords['portal']}@redis:6379/0",
                                      "identity_url": f"rediss://identity:{passwords['identity']}@redis:6379/0", "ca_file": "/fixture/trust/ca.crt"}))
        config.chmod(0o600)
        os.chown(config, 65532, 65532)
        compose = ["docker", "compose", "--project-name", project, "-f", str(source / "compose.redis.yaml")]

        def dc(*args):
            return run([*compose, *args], env)

        def inspect(name):
            return json.loads(run(["docker", "inspect", ids[name]]))[0]

        def ready(name):
            for _ in range(80):
                state = inspect(name)["State"]
                require(state["Running"], "REDIS_SERVER_EXITED_" + name)
                if state.get("Health", {}).get("Status") == "healthy":
                    return
                time.sleep(0.25)
            raise RuntimeError("REDIS_SERVER_NOT_HEALTHY_" + name)

        def client(name, user):
            c = Client(ips[name], name, root / "trust/ca.crt", user, passwords[user])
            clients.append(c)
            return c

        def denied(c, *cmd):
            try:
                c.command(*cmd)
            except RedisError as err:
                require(str(err).startswith("NOPERM"), "REDIS_WRONG_DENIAL_CLASS")
                return
            raise RuntimeError("REDIS_FORBIDDEN_COMMAND_ACCEPTED")

        try:
            dc("config", "--quiet")
            dc("create")
            for name in ("redis", "newapi-redis"):
                ids[name] = dc("ps", "--all", "--quiet", name)
            network = json.loads(run(["docker", "network", "inspect", project + "_data"]))[0]
            require(network["Internal"] and network["Labels"]["com.docker.compose.project"] == project, "REDIS_WRONG_NETWORK")
            network_id = network["Id"]
            ips = {}
            for name in ids:
                actual = inspect(name)
                require(actual["Config"]["User"] == "65532:65532" and actual["HostConfig"]["ReadonlyRootfs"], "REDIS_CONTAINER_ISOLATION")
                require(actual["HostConfig"]["PortBindings"] == {}, "REDIS_PUBLISHED_PORT")
                binds = [m for m in actual["Mounts"] if m["Type"] == "bind"]
                require(len(binds) == 5 and all(m["RW"] == (m["Destination"] == "/data") for m in binds), "REDIS_MOUNT_ISOLATION")
                require(all(m["Type"] in ("bind", "tmpfs") for m in actual["Mounts"]), "REDIS_ANONYMOUS_VOLUME")
                require(actual["HostConfig"]["Memory"] == 384 * 1024 * 1024 and actual["HostConfig"]["PidsLimit"] == 64, "REDIS_RESOURCE_LIMITS")
                run(["docker", "update", "--restart=no", ids[name]])
                run(["docker", "start", ids[name]])
                ready(name)
                ips[name] = next(iter(inspect(name)["NetworkSettings"]["Networks"].values()))["IPAddress"]
            for binary, testname, marker in (("redis-portal.test", "TestRedisStoreProductionTLS", "PRODUCTION_REDIS_PORTAL_CLIENT_PASS"),
                                              ("redis-identity.test", "TestRedisAdmissionProductionTLS", "PRODUCTION_REDIS_IDENTITY_CLIENT_PASS"),
                                              ("redis-options.test", "TestOptions", "PASS")):
                result = run(["docker", "run", "--rm", "--name", project + "-client", "--label", "pinvou.redis-fixture=" + project,
                              "--read-only", "--user", "65532:65532", "--cap-drop", "ALL", "--network", network_id,
                              "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=8m,uid=65532,gid=65532,mode=0700",
                              "--entrypoint", "/fixture/client.test", "--mount", f"type=bind,src={bins / binary},dst=/fixture/client.test,readonly",
                              "--mount", f"type=bind,src={config},dst=/fixture/client.json,readonly", "--mount", f"type=bind,src={root / 'trust'},dst=/fixture/trust,readonly",
                              "--env", "PINVOU_TEST_REDIS_FIXTURE_CONFIG=/fixture/client.json", IMAGE, "-test.v", "-test.run=^" + testname])
                require(marker in result and "SKIP" not in result, "REDIS_ACTUAL_CLIENT_NOT_VERIFIED")
                print(marker, flush=True)
            portal, identity, model = client("redis", "portal"), client("redis", "identity"), client("newapi-redis", "newapi")
            for c in (portal, identity, model):
                require(c.command("PING") == "PONG", "REDIS_AUTH_TLS_PING_FAILED")
                for cmd in (("ACL", "LIST"), ("CONFIG", "GET", "*"), ("FLUSHALL",), ("FLUSHDB",), ("MODULE", "LIST"), ("SCRIPT", "FLUSH"), ("CLIENT", "LIST"), ("KEYS", "*")):
                    denied(c, *cmd)
            for c in (portal, identity):
                denied(c, "SCAN", "0")
            # Exact independent-instance operations exercised by pinned rc.26 paths.
            require(model.command("SET", "fixture:string", "1") == "OK", "REDIS_MODEL_SET")
            require(model.command("GETSET", "fixture:string", "2") == "1", "REDIS_MODEL_GETSET")
            require(model.command("INCRBY", "fixture:string", 2) == 4, "REDIS_MODEL_INCREMENT")
            require(model.command("MGET", "fixture:string") == ["4"], "REDIS_MODEL_MGET")
            require(model.command("HSET", "fixture:hash", "a", "1") == 1, "REDIS_MODEL_HSET")
            require(model.command("HINCRBY", "fixture:hash", "a", "2") == 3, "REDIS_MODEL_HINCR")
            require(model.command("HGETALL", "fixture:hash") == ["a", "3"], "REDIS_MODEL_HGETALL")
            lua = "redis.call('TIME'); redis.call('HMSET',KEYS[1],'b',4); redis.call('PEXPIRE',KEYS[1],60000); return redis.call('HMGET',KEYS[1],'b')"
            digest = model.command("SCRIPT", "LOAD", lua)
            require(model.command("EVALSHA", digest, 1, "fixture:hash") == ["4"], "REDIS_MODEL_SCRIPT")
            require(model.command("EVAL", "return redis.call('HEXISTS',KEYS[1],'a')", 1, "fixture:hash") == 1, "REDIS_MODEL_EVAL")
            require(model.command("LPUSH", "fixture:list", "a") == 1 and model.command("LLEN", "fixture:list") == 1, "REDIS_MODEL_LIST")
            require(model.command("LINDEX", "fixture:list", 0) == "a" and model.command("LTRIM", "fixture:list", 0, 0) == "OK", "REDIS_MODEL_LIST_RANGE")
            require(model.command("MULTI") == "OK" and model.command("EXPIRE", "fixture:list", 60) == "QUEUED" and model.command("EXEC") == [1], "REDIS_MODEL_TRANSACTION")
            require(model.command("TTL", "fixture:list") > 0 and model.command("PTTL", "fixture:hash") > 0, "REDIS_MODEL_TTL")
            require(model.command("MULTI") == "OK" and model.command("DISCARD") == "OK", "REDIS_MODEL_DISCARD")
            require(model.command("UNLINK", "fixture:list") == 1, "REDIS_MODEL_UNLINK")
            require(model.command("SCAN", "0", "MATCH", "portal:*", "COUNT", 1000)[1] == [], "REDIS_MODEL_ACCOUNT_METADATA")
            for name in ids:
                for ca, host in ((root / "wrong.crt", name), (root / "trust/ca.crt", "wrong-host")):
                    try:
                        c = Client(ips[name], host, ca)
                        c.close()
                    except ssl.SSLCertVerificationError:
                        pass
                    else:
                        raise RuntimeError("REDIS_BAD_TLS_ACCEPTED")
                for username, password in (("default", ""), ("portal" if name == "redis" else "newapi", "wrong-password"), ("newapi" if name == "redis" else "portal", passwords["newapi" if name == "redis" else "portal"])):
                    try:
                        c = Client(ips[name], name, root / "trust/ca.crt", username, password)
                        c.close()
                    except RedisError as err:
                        require(str(err).startswith("WRONGPASS"), "REDIS_BAD_AUTH_WRONG_ERROR")
                    else:
                        raise RuntimeError("REDIS_BAD_AUTH_ACCEPTED")
                with socket.create_connection((ips[name], 6379), timeout=4) as sock:
                    sock.sendall(b"*1\r\n$4\r\nPING\r\n")
                    try:
                        require(sock.recv(128) == b"", "REDIS_PLAINTEXT_ACCEPTED")
                    except ConnectionResetError:
                        pass
            for c, key in ((portal, "portal:session:persistent"), (identity, "pinvou:auth-alias:persistent"), (model, "fixture:persistent")):
                require(c.command("SET", key, "47") == "OK", "REDIS_PERSIST_WRITE")
            for c in clients:
                c.close()
            clients.clear()
            for name in ids:
                run(["docker", "restart", ids[name]])
                ready(name)
            for name, user, key in (("redis", "portal", "portal:session:persistent"), ("redis", "identity", "pinvou:auth-alias:persistent"), ("newapi-redis", "newapi", "fixture:persistent")):
                require(client(name, user).command("GET", key) == "47", "REDIS_RESTART_PERSISTENCE")
            for name in ids:
                logs = subprocess.run(["docker", "logs", ids[name]], capture_output=True, text=True, timeout=10)
                require(logs.returncode == 0, "REDIS_LOG_READ_FAILED")
                require(not any(password in logs.stdout or password in logs.stderr for password in passwords.values()), "REDIS_SECRET_IN_LOGS")
            print("PRODUCTION_REDIS_TLS_ACL_CLIENTS_ISOLATION_PERSISTENCE_PASS", flush=True)
        finally:
            for c in clients:
                c.close()
            leftovers = run(["docker", "ps", "--all", "--quiet", "--filter", "label=pinvou.redis-fixture=" + project]).splitlines()
            for container_id in list(ids.values()) + leftovers:
                if container_id:
                    actual = json.loads(run(["docker", "inspect", container_id]))[0]
                    labels = actual["Config"]["Labels"]
                    service_owned = container_id in ids.values() and labels.get("com.docker.compose.project") == project and labels.get("com.docker.compose.service") in ids
                    client_owned = container_id in leftovers and labels.get("pinvou.redis-fixture") == project
                    require(actual["Id"].startswith(container_id) and (service_owned or client_owned), "REDIS_CLEANUP_CONTAINER_NOT_OWNED")
                    run(["docker", "rm", "--force", container_id])
            if network_id:
                network = json.loads(run(["docker", "network", "inspect", network_id]))[0]
                require(not network["Containers"] and network["Labels"]["com.docker.compose.project"] == project, "REDIS_CLEANUP_NETWORK_NOT_OWNED")
                run(["docker", "network", "rm", network_id])


if __name__ == "__main__":
    try:
        main()
    except RedisError:
        raise SystemExit("REDIS_FIXTURE_UNEXPECTED_SERVER_ERROR") from None

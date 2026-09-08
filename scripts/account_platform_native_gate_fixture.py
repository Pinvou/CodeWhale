"""Optional isolated gate integration; called only by the OpenBao fixture.

All DB rows, passwords, AppRoles and root authority belong to that disposable
instance. This module cannot discover or connect to formal custody material.
"""

import json
import os
import secrets
import subprocess
import time
from urllib.parse import urlencode

POSTGRES = "sha256:bd39a1ca524d0e8c65a36e68f422111a622f7110a8e0a5a8ef1df0ab2de0b24e"
TABLES = {
    "provider_credentials": "ciphertext",
    "usage_dispatch_preparations": "encrypted_envelope",
    "usage_dispatch_intents": "encrypted_envelope",
    "usage_stream_events": "encrypted_payload",
    "usage_replay_results": "encrypted_payload",
    "credential_isolation_sessions": "pkce_verifier_ciphertext",
}


def run_native_gate_fixture(root, source, network, prefix, subnet, common, mounts,
                            containers, runner, endpoint, expect, root_token):
    def require(ok, code):
        if not ok:
            raise RuntimeError("native gate fixture: " + code)

    def command(args, data=None, timeout=45):
        result = subprocess.run(args, input=data, capture_output=True, text=True, timeout=timeout)
        stage = args[0] + (" " + args[1] if args[0] == "docker" else "")
        require(result.returncode == 0, "subprocess failed: " + stage)
        return result.stdout.strip()

    def write(file, value, uid=65532):
        with file.open("xb") as stream:
            stream.write(value.encode())
        file.chmod(0o600)
        os.chown(file, uid, uid)

    for service, role in (("gateway", "pinvou-token-gateway"), ("credential", "pinvou-credential-portal")):
        expect("sys/policies/acl/" + role, {"policy": (source / "runtime-policy.hcl").read_text()}, root_token)
        settings = json.loads((source / "approle.json").read_text())
        settings.update(token_policies=[role], token_ttl=30, token_max_ttl=900, token_explicit_max_ttl=900)
        expect("auth/approle/role/" + role, settings, root_token)
        write(root / (service + "-bootstrap") / "role-id",
              expect("auth/approle/role/" + role + "/role-id", token=root_token)["data"]["role_id"])
        write(root / (service + "-bootstrap") / "secret-id",
              expect("auth/approle/role/" + role + "/secret-id", {}, root_token)["data"]["secret_id"])
        name = prefix + "-" + service
        command(["docker", "create", "--name", name, *common, "--memory", "128m",
                 *mounts((("config", "/bao/config", True), ("trust", "/bao/trust", True),
                          (service + "-bootstrap", "/bao/bootstrap", True), (service + "-token", "/bao/token", False))),
                 "quay.io/openbao/openbao@sha256:11fd73a2102cda9c55d5d881a8c3210303146a7ec1e8ac76f526e175c6d24641",
                 "agent", "-config=/bao/config/agent.hcl"])
        containers.append(name)
        command(["docker", "start", name])

    # The database has no host volume/port and no connection to business networks.
    # .2 is OpenBao; the two Agents allocate .3/.4 on this fresh network.
    pg_ip = str(subnet.network_address + 5)
    pg_config = root / "gate-pg"
    pg_config.mkdir(mode=0o700)
    os.chown(pg_config, 999, 999)
    gate_dir = root / "gate-state"
    gate_dir.mkdir(mode=0o700)
    admin_password, reader_password = secrets.token_hex(32), secrets.token_hex(32)
    write(pg_config / "password", admin_password, 999)
    command(["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=isolated-postgres",
             "-keyout", str(pg_config / "server.key"), "-out", str(root / "pg.csr")])
    write(root / "pg.extensions", "subjectAltName=IP:" + pg_ip + "\nbasicConstraints=critical,CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n")
    command(["openssl", "x509", "-req", "-in", str(root / "pg.csr"), "-CA", str(root / "trust" / "ca.crt"),
             "-CAkey", str(root / "ca.key"), "-CAserial", str(root / "trust" / "ca.srl"), "-days", "30",
             "-extfile", str(root / "pg.extensions"), "-out", str(pg_config / "server.crt")])
    for file in pg_config.iterdir():
        file.chmod(0o600)
        os.chown(file, 999, 999)
    name = prefix + "-postgres"
    command(["docker", "create", "--name", name, "--network", network, "--ip", pg_ip,
             "--user", "999:999", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
             "--memory", "384m", "--cpus", "1", "--pids-limit", "128", "--ulimit", "core=0:0",
             "--log-driver", "none", "--tmpfs", "/var/lib/postgresql/data:rw,uid=999,gid=999,mode=0700,size=256m",
             "--tmpfs", "/var/run/postgresql:rw,uid=999,gid=999,mode=0700,size=8m",
             "--tmpfs", "/tmp:rw,mode=1777,size=16m", "--mount", "type=bind,src=" + str(pg_config) + ",dst=/fixture,readonly",
             "--env", "POSTGRES_PASSWORD_FILE=/fixture/password", "--env", "POSTGRES_DB=gate_fixture",
             POSTGRES, "postgres", "-c", "ssl=on", "-c", "ssl_cert_file=/fixture/server.crt",
             "-c", "ssl_key_file=/fixture/server.key"])
    containers.append(name)
    command(["docker", "start", name])
    ready = False
    for _ in range(60):
        result = subprocess.run(["docker", "exec", name, "pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-d", "gate_fixture"],
                                capture_output=True, timeout=5)
        if result.returncode == 0:
            ready = True
            break
        time.sleep(0.5)
    require(ready, "postgres readiness")

    def sql(statement):
        return command(["docker", "exec", "-i", name, "psql", "-XqAt", "-v", "ON_ERROR_STOP=1",
                        "-U", "postgres", "-d", "gate_fixture"], statement)

    # Real PostgreSQL SQL/TLS/ACL test, deliberately not a business migration test.
    sql("CREATE ROLE gate_reader LOGIN PASSWORD '" + reader_password + "';\n" +
        "\n".join("CREATE TABLE public." + table + " (key_id text, " + column + " bytea);" for table, column in TABLES.items()) +
        "\nGRANT SELECT ON ALL TABLES IN SCHEMA public TO gate_reader;")
    dsn = "postgresql://gate_reader:" + reader_password + "@" + pg_ip + ":5432/gate_fixture?" + urlencode(
        {"sslmode": "verify-full", "sslrootcert": str(root / "trust" / "ca.crt")})
    write(gate_dir / "database", dsn)
    environment = {"PATH": os.environ["PATH"], "PINVOU_SECURITY_INSTALL_MODE": "fresh-install",
                   "TOKEN_SECRET_PROVIDER_URL": endpoint, "TOKEN_SECRET_KEY_ID": "pinvou-token-data",
                   "TOKEN_SECRET_PROVIDER_TOKEN_FILE": str(root / "gateway-token" / "token"),
                   "PINVOU_SECURITY_CREDENTIAL_TOKEN_FILE": str(root / "credential-token" / "token"),
                   "TOKEN_SECRET_PROVIDER_CA_FILE": str(root / "trust" / "ca.crt"),
                   "PINVOU_SECURITY_NATIVE_ROTATION_PROOF_FILE": str(gate_dir / "proof"),
                   "PINVOU_SECURITY_FRESH_DATABASE_URL_FILE": str(gate_dir / "database")}

    def gate(action):
        result = subprocess.run(["node", str(runner), action], env=environment, capture_output=True, text=True, timeout=50)
        require(result.returncode == 0 and result.stdout.strip() == "NATIVE_FIXTURE_" + action.upper() + "_PASS",
                "runner " + action)

    deadline = time.monotonic() + 60
    while True:
        renewed = True
        for service in ("gateway", "credential"):
            file = root / (service + "-token") / "token"
            if not file.is_file():
                renewed = False
                continue
            data = expect("auth/token/lookup-self", token=file.read_text().strip())["data"]
            renewed = renewed and bool(data.get("last_renewal"))
        if renewed:
            break
        require(time.monotonic() < deadline, "actual Agent renewal absent")
        time.sleep(0.5)
    command(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "30",
             "-subj", "/CN=wrong-fixture-ca", "-addext", "basicConstraints=critical,CA:TRUE",
             "-keyout", str(gate_dir / "wrong-ca.key"), "-out", str(gate_dir / "wrong-ca.crt")])
    (gate_dir / "wrong-ca.key").chmod(0o600)
    gate("wrong-ca")
    gate("prepare")
    gate("same-version")
    expect("transit/keys/pinvou-token-data/rotate", {}, root_token)
    gate("verify")
    for table, column in TABLES.items():
        sql("INSERT INTO public." + table + " VALUES ('legacy-fixture-key','old'::bytea);")
        gate("foreign")
        gate("nonfresh")
        sql("DELETE FROM public." + table + ";")
    sql("INSERT INTO public.credential_isolation_sessions VALUES ('',''::bytea);")
    gate("verify")
    sql("DELETE FROM public.credential_isolation_sessions;")
    # RLS cannot turn a hidden historical row into an apparently clean database.
    sql("INSERT INTO provider_credentials VALUES ('legacy-fixture-key','old'::bytea); ALTER TABLE provider_credentials ENABLE ROW LEVEL SECURITY;")
    gate("rls")
    sql("ALTER TABLE provider_credentials DISABLE ROW LEVEL SECURITY; DELETE FROM provider_credentials;")
    gate("verify")
    # Identity policies are additive and separate from the AppRole token policy.
    # Add only an innocuous read permission; never exercise an escalation.
    policy = "fixture-extra-policy"
    expect("sys/policies/acl/" + policy, {"policy": 'path "sys/health" { capabilities = ["read"] }'}, root_token)
    for service in ("gateway", "credential"):
        token = (root / (service + "-token") / "token").read_text().strip()
        entity = expect("auth/token/lookup-self", token=token)["data"]["entity_id"]
        require(bool(entity), "AppRole entity missing")
        expect("identity/entity/id/" + entity, {"policies": [policy]}, root_token)
        gate("extra-policy")
        expect("identity/entity/id/" + entity, {"policies": []}, root_token)
        gate("verify")
    print("NATIVE_GATE_ACTUAL_TLS_POSTGRES_ROTATION_PASS", flush=True)

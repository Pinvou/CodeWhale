"""Opt-in Linux Docker acceptance of the exact production PostgreSQL configuration.

Only randomly named, newly created fixture resources are used. No formal paths,
credentials, existing database or user-supplied connection URL are accepted.
"""

import argparse
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time

IMAGE = "m.daocloud.io/docker.io/library/postgres@sha256:7bade6d532592ca8ce7ee32def7399dad2607c4ea5583839fc4352a095a11ea6"
KEYS = ("ACCOUNT_DB_PASSWORD", "BILLING_DB_PASSWORD", "TOKEN_DB_PASSWORD", "ACCOUNT_MIGRATION_DB_PASSWORD",
        "BILLING_MIGRATION_DB_PASSWORD", "TOKEN_MIGRATION_DB_PASSWORD",
        "TOKEN_POLICY_DB_PASSWORD", "NEWAPI_DB_PASSWORD", "KEYCLOAK_DB_PASSWORD",
        "ACCOUNT_APPROVAL_WRITER_DB_PASSWORD", "ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD", "ACCOUNT_APPROVAL_RELAY_DB_PASSWORD")
LOGINS = (("account_service", "account", KEYS[0]),
          ("billing_service", "billing", KEYS[1]),
          ("token_gateway", "token", KEYS[2]),
          ("account_migrator", "account", KEYS[3]),
          ("billing_migrator", "billing", KEYS[4]),
          ("token_migrator", "token", KEYS[5]),
          ("token_policy_operator", "token", KEYS[6]),
          ("new_api", "newapi", KEYS[7]), ("keycloak", "keycloak", KEYS[8]),
          ("account_approval_writer", "account", KEYS[9]),
          ("account_approval_backfill", "account", KEYS[10]),
          ("account_approval_relay", "account", KEYS[11]))


def require(ok, code):
    if not ok:
        raise RuntimeError(code)


def run(args, data=None, env=None, check=True, timeout=60):
    result = subprocess.run(args, input=data, text=True, capture_output=True,
                            env=env, timeout=timeout)
    if check:
        require(result.returncode == 0, "FIXTURE_SUBPROCESS_FAILED_" + args[0])
    return result


def private(path, value):
    with path.open("x", encoding="utf-8") as stream:
        stream.write(value)
    path.chmod(0o600)
    os.chown(path, 999, 999)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", action="store_true", required=True)
    parser.parse_args()
    require(os.name == "posix" and os.geteuid() == 0, "LINUX_ROOT_FIXTURE_ONLY")
    source = Path(__file__).resolve().parents[1] / "account-platform/deploy"
    require(source.is_dir(), "FIXTURE_SOURCE_MISSING")
    run(["docker", "image", "inspect", IMAGE])
    version = run(["docker", "run", "--rm", "--network", "none", "--user", "999:999", "--read-only",
                   "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--entrypoint", "postgres", IMAGE, "--version"]).stdout.strip()
    require(version == "postgres (PostgreSQL) 17.11 (Debian 17.11-1.pgdg12+2)", "POSTGRES_IMAGE_VERSION_MISMATCH")
    project = "pinvou-pg-fixture-" + secrets.token_hex(8)
    container_id = None
    network_id = None
    all_secrets = []
    with tempfile.TemporaryDirectory(prefix="pinvou-pg-fixture-") as raw:
        root = Path(raw)
        root.chmod(0o755)  # No secrets directly in the traversable fixture root.
        for name in ("data", "tls", "admin", "bootstrap", "trust"):
            directory = root / name
            directory.mkdir(mode=0o700)
            os.chown(directory, 999, 999)
        passwords = {key: secrets.token_hex(32) for key in KEYS}
        admin = secrets.token_hex(32)
        all_secrets = [admin, *passwords.values()]
        private(root / "admin/password", admin)
        for key, value in passwords.items():
            private(root / "bootstrap" / key, value)
        # Deliberate invalid duplicate causes initdb to succeed but domain init fail.
        (root / "bootstrap" / KEYS[1]).write_text(passwords[KEYS[0]])
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
             "-subj", "/CN=Pinvou temporary fixture CA", "-addext", "basicConstraints=critical,CA:TRUE",
             "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-keyout", str(root / "ca.key"),
             "-out", str(root / "trust/ca.crt")])
        run(["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=postgres",
             "-keyout", str(root / "tls/server.key"), "-out", str(root / "server.csr")])
        (root / "ext").write_text("subjectAltName=DNS:postgres\nextendedKeyUsage=serverAuth\n")
        run(["openssl", "x509", "-req", "-in", str(root / "server.csr"), "-CA", str(root / "trust/ca.crt"),
             "-CAkey", str(root / "ca.key"), "-CAcreateserial", "-days", "2", "-extfile", str(root / "ext"),
             "-out", str(root / "tls/server.crt")])
        for path in (root / "tls").iterdir():
            path.chmod(0o600)
            os.chown(path, 999, 999)
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
             "-subj", "/CN=Wrong temporary CA", "-addext", "basicConstraints=critical,CA:TRUE",
             "-keyout", str(root / "wrong.key"), "-out", str(root / "trust/wrong.crt")])
        (root / "trust/wrong.crt").chmod(0o600)
        os.chown(root / "trust/wrong.crt", 999, 999)
        (root / "trust/ca.crt").chmod(0o600)
        os.chown(root / "trust/ca.crt", 999, 999)
        env = {**os.environ, "POSTGRES_IMAGE": IMAGE,
               **{"POSTGRES_" + key + "_DIRECTORY": str(root / value) for key, value in
                  (("DATA", "data"), ("TLS", "tls"), ("ADMIN", "admin"), ("BOOTSTRAP", "bootstrap"))}}
        compose = ["docker", "compose", "--project-name", project, "-f",
                   str(source / "production/compose.dependencies.yaml")]

        def dc(*args):
            return run([*compose, *args], env=env).stdout.strip()

        def inspect():
            return json.loads(run(["docker", "inspect", container_id]).stdout)[0]

        def wait_exit():
            for _ in range(80):
                state = inspect()["State"]
                if not state["Running"]:
                    require(state["ExitCode"] != 0, "FAILURE_MUST_EXIT_NONZERO")
                    return
                time.sleep(0.25)
            raise RuntimeError("EXPECTED_STARTUP_REFUSAL")

        def wait_ready():
            for _ in range(100):
                state = inspect()["State"]
                require(state["Running"], "POSTGRES_EARLY_EXIT")
                if state.get("Health", {}).get("Status") == "healthy":
                    return
                time.sleep(0.25)
            raise RuntimeError("POSTGRES_NOT_HEALTHY")

        def local_sql(sql):
            # Password is stdin only; never argv, Docker environment or diagnostics.
            return run(["docker", "exec", "-i", "-u", "999:999", container_id, "psql", "-X", "-At",
                        "-U", "account_platform_admin", "-d", "postgres", "--set=ON_ERROR_STOP=1"],
                       data=admin + "\n" + sql + "\n").stdout.strip()

        def client(role, db, password, sql="SELECT 1;", sslmode="verify-full", host="postgres", trust=True):
            args = ["docker", "run", "--rm", "-i", "--user", "999:999", "--read-only", "--cap-drop", "ALL",
                    "--network", network_id, "--mount", f"type=bind,src={root / 'trust'},dst=/trust,readonly",
                    "--entrypoint", "psql", IMAGE, "-X", "-At", "--set=ON_ERROR_STOP=1",
                    f"host={host} port=5432 dbname=account_platform_{db} user={role} sslmode={sslmode} "
                    f"sslrootcert={'/trust/ca.crt' if trust else '/trust/wrong.crt'} connect_timeout=3"]
            return run(args, data=password + "\n" + sql + "\n", check=False, timeout=15)

        def rejected(result, expected, code, status=2):
            # Prove libpq/SQL rejected the intended condition, not Docker or tooling.
            require(result.returncode == status and expected in result.stderr,
                    "EXPECTED_PRECISE_REFUSAL_" + code)

        try:
            config = json.loads(dc("config", "--format", "json"))
            require(set(config["services"]) == {"postgres"}, "UNEXPECTED_SERVICE")
            require(config["networks"]["data"]["internal"], "NON_PRIVATE_NETWORK")
            dc("create", "postgres")
            container_id = dc("ps", "--all", "--quiet", "postgres")
            require(len(container_id) == 64, "CREATED_CONTAINER_MISSING")
            actual = inspect()
            require(actual["Config"]["Labels"]["com.docker.compose.project"] == project, "WRONG_PROJECT")
            require(actual["HostConfig"]["PortBindings"] == {}, "PUBLISHED_PORT")
            require(actual["Config"]["User"] == "999:999", "WRONG_UID")
            require(actual["HostConfig"]["ReadonlyRootfs"], "WRITABLE_ROOT")
            require(all(m["Type"] == "bind" for m in actual["Mounts"] if m["Type"] != "tmpfs"), "ANONYMOUS_VOLUME")
            network = json.loads(run(["docker", "network", "inspect", config["networks"]["data"]["name"]]).stdout)[0]
            require(network["Labels"]["com.docker.compose.project"] == project, "WRONG_NETWORK_PROJECT")
            network_id = network["Id"]
            require(len(network_id) == 64, "NETWORK_MISSING")
            run(["docker", "update", "--restart=no", container_id])
            run(["docker", "start", container_id])
            wait_exit()
            require((root / "data/PG_VERSION").is_file(), "INITDB_FAILURE_NOT_EXERCISED")
            require(not (root / "data/.pinvou-bootstrap-complete").exists(), "PARTIAL_MARKER")
            (root / "bootstrap" / KEYS[1]).write_text(passwords[KEYS[1]])
            run(["docker", "start", container_id])
            wait_exit()
            require("PRODUCTION_DATABASE_CONFIGURATION_REJECTED" in run(["docker", "logs", container_id]).stderr,
                    "PARTIAL_RESTART_NOT_REFUSED")
            # Fixture-only reset of its independently created empty-business failed cluster.
            require((root / "data").resolve().parent == root.resolve(), "UNSAFE_FIXTURE_DATA")
            shutil.rmtree(root / "data")
            (root / "data").mkdir(mode=0o700)
            os.chown(root / "data", 999, 999)
            run(["docker", "start", container_id])
            wait_ready()
            require(local_sql("SHOW server_encoding;") == "UTF8", "SERVER_ENCODING_NOT_UTF8")
            require(local_sql("SELECT count(*) FROM pg_database WHERE pg_encoding_to_char(encoding)<>'UTF8' OR datcollate<>'C' OR datctype<>'C';") == "0", "DATABASE_ENCODING_OR_LOCALE_MISMATCH")
            require(local_sql("DROP DATABASE account_platform_newapi; CREATE DATABASE account_platform_newapi WITH TEMPLATE template0 OWNER new_api ENCODING 'SQL_ASCII' LC_COLLATE 'C' LC_CTYPE 'C';") is not None, "ENCODING_DRIFT_SETUP_FAILED")
            require(local_sql("SELECT count(*) FROM pg_database;") == "8", "ENCODING_DRIFT_DATABASE_COUNT")
            require(local_sql("SELECT string_agg(datname,',' ORDER BY datname) FROM pg_database;") == "account_platform_account,account_platform_billing,account_platform_keycloak,account_platform_newapi,account_platform_token,postgres,template0,template1", "ENCODING_DRIFT_DATABASE_NAMES")
            require(local_sql("SELECT count(*) FROM pg_database WHERE pg_encoding_to_char(encoding)='UTF8';") == "7", "ENCODING_DRIFT_UTF8_CONTROL")
            require(local_sql("SELECT pg_encoding_to_char(encoding) FROM pg_database WHERE datname='account_platform_newapi';") == "SQL_ASCII", "ENCODING_DRIFT_NOT_CREATED")
            drift = run(["docker", "exec", "-u", "999:999", container_id, "/bin/sh", "/opt/pinvou/production/postgres-health.sh"], check=False)
            require(drift.returncode != 0 and "PRODUCTION_DATABASE_CONFIGURATION_REJECTED" in drift.stderr,
                    "SQL_ASCII_BUSINESS_DATABASE_NOT_REJECTED")
            require(local_sql("DROP DATABASE account_platform_newapi; CREATE DATABASE account_platform_newapi WITH TEMPLATE template0 OWNER new_api ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C';") is not None, "ENCODING_DRIFT_CLEANUP_FAILED")
            require(run(["docker", "exec", "-u", "999:999", container_id, "/bin/sh", "/opt/pinvou/production/postgres-health.sh"], check=False).returncode == 0,
                    "UTF8_HEALTH_DID_NOT_RECOVER")
            for role, db, key in LOGINS:
                result = client(role, db, passwords[key])
                require(result.returncode == 0 and result.stdout.strip() == "1", "TLS_LOGIN_FAILED_" + role)
            require(local_sql("SELECT count(*) FROM pg_authid WHERE rolcanlogin AND rolpassword LIKE 'SCRAM-SHA-256$%';") == "10", "SCRAM_COUNT")
            require(local_sql("SELECT count(*) FROM pg_roles WHERE rolname='payment_simulator';") == "0", "SIMULATOR_PRESENT")
            require(local_sql("SELECT count(*) FROM pg_hba_file_rules WHERE error IS NOT NULL;") == "0", "HBA_PARSE_ERROR")
            require(client("account_service", "account", passwords[KEYS[0]], "SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid();").stdout.strip() == "t", "TLS_NOT_USED")
            rejected(client("account_service", "account", passwords[KEYS[0]], sslmode="disable"),
                     "no encryption", "PLAINTEXT")
            rejected(client("account_service", "account", secrets.token_hex(32)),
                     "password authentication failed", "PASSWORD")
            rejected(client("account_service", "billing", passwords[KEYS[0]]),
                     "pg_hba.conf rejects connection", "CROSS_DATABASE")
            rejected(client("account_platform_admin", "account", admin),
                     "pg_hba.conf rejects connection", "REMOTE_ADMIN")
            rejected(client("billing_service", "billing", passwords[KEYS[1]], "SET ROLE billing_schema_owner;"),
                     "permission denied to set role", "BILLING_ESCALATION", status=3)
            rejected(client("token_gateway", "token", passwords[KEYS[2]], "SET ROLE token_schema_owner;"),
                     "permission denied to set role", "TOKEN_ESCALATION", status=3)
            rejected(client("account_service", "account", passwords[KEYS[0]], "SET ROLE account_approval_control_owner;"),
                     "permission denied to set role", "ACCOUNT_CONTROL_OWNER_ESCALATION", status=3)
            rejected(client("account_service", "account", passwords[KEYS[0]], "SET ROLE account_migrator;"),
                     "permission denied to set role", "ACCOUNT_MIGRATOR_ESCALATION", status=3)
            rejected(client("account_service", "account", passwords[KEYS[0]], trust=False),
                     "certificate verify failed", "WRONG_CA")
            ip = next(iter(inspect()["NetworkSettings"]["Networks"].values()))["IPAddress"]
            rejected(client("account_service", "account", passwords[KEYS[0]], host=ip),
                     "does not match host name", "WRONG_HOSTNAME")
            positive = client("account_service", "account", passwords[KEYS[0]])
            require(positive.returncode == 0 and positive.stdout.strip() == "1", "POST_NEGATIVE_LOGIN_FAILED")
            require(local_sql("CREATE TABLE public.fixture_persistence(value int); INSERT INTO public.fixture_persistence VALUES(47);") is not None, "WRITE_FAILED")
            run(["docker", "stop", "--time", "10", container_id])
            marker = root / "data/.pinvou-bootstrap-complete"
            original_marker = marker.read_text()
            marker.write_text("pinvou-production-postgres-v1 " + original_marker.split()[1] + "\n")
            run(["docker", "start", container_id])
            wait_exit()
            marker.write_text("pinvou-production-postgres-v2-utf8 0\n")
            run(["docker", "start", container_id])
            wait_exit()
            marker.write_text(original_marker)
            for key in KEYS:
                (root / "bootstrap" / key).unlink()
            run(["docker", "start", container_id])
            wait_ready()
            require(local_sql("SELECT value FROM public.fixture_persistence;") == "47", "RESTART_PERSISTENCE_FAILED")
            logs = run(["docker", "logs", container_id])
            require(not any(value in logs.stdout + logs.stderr for value in all_secrets), "SECRET_IN_LOG")
            print("PRODUCTION_POSTGRES_TLS_SCRAM_PARTIAL_INIT_RESTART_PASS")
        finally:
            if container_id:
                actual = inspect()
                require(actual["Config"]["Labels"]["com.docker.compose.project"] == project, "CLEANUP_PROJECT_MISMATCH")
                run(["docker", "rm", "-f", container_id])
            if network_id:
                network = json.loads(run(["docker", "network", "inspect", network_id]).stdout)[0]
                require(network["Labels"]["com.docker.compose.project"] == project and not network["Containers"], "CLEANUP_NETWORK_MISMATCH")
                run(["docker", "network", "rm", network_id])


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, subprocess.TimeoutExpired) as exc:
        # Subprocess output/arguments may contain test secrets: print fixed codes only.
        print(str(exc) if isinstance(exc, RuntimeError) else "POSTGRES_FIXTURE_FAILED")
        raise SystemExit(1)

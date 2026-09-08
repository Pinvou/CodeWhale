"""Opt-in candidate Keycloak production-mode test; only new disposable resources."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import tempfile
import time
import urllib.parse
import ssl

PG_IMAGE = "m.daocloud.io/docker.io/library/postgres@sha256:7bade6d532592ca8ce7ee32def7399dad2607c4ea5583839fc4352a095a11ea6"


def require(condition, marker):
    if not condition:
        raise RuntimeError(marker)


def run(argv, *, data=None, timeout=60, check=True, env=None):
    result = subprocess.run(argv, input=data, capture_output=True, text=True, timeout=timeout, env=env)
    require(not check or result.returncode == 0, "FIXTURE_COMMAND_FAILED")
    return result


def private(path, value, uid):
    with path.open("x", encoding="utf-8") as output:
        output.write(value)
    path.chmod(0o600)
    os.chown(path, uid, 0 if uid == 1000 else uid)


def verify_mounts(meta, bindings, temporary):
    actual = set()
    for mount in meta["Mounts"]:
        if mount["Type"] == "tmpfs":
            require(mount["Destination"] in temporary, "EXTRA_TEMPORARY_MOUNT")
        else:
            require(mount["Type"] == "bind", "NON_BIND_MOUNT")
            actual.add((mount["Source"], mount["Destination"], not mount["RW"]))
    require(actual == set(bindings), "EXACT_BIND_MOUNTS_REQUIRED")
    require(meta["HostConfig"].get("Tmpfs", {}) == temporary, "EXACT_TEMPORARY_MOUNTS_REQUIRED")


def verify_limits(meta, memory, pids):
    limits = meta["HostConfig"]
    require(limits["ReadonlyRootfs"] and not limits["PortBindings"], "CONTAINER_BOUNDARY")
    require(limits["CapDrop"] == ["ALL"] and limits["SecurityOpt"] in (["no-new-privileges"], ["no-new-privileges:true"]), "CONTAINER_PRIVILEGES")
    require(limits["Memory"] == memory and limits["NanoCpus"] == 1000000000 and limits["PidsLimit"] == pids, "CONTAINER_LIMITS")
    require(any(item["Name"] == "core" and item["Soft"] == item["Hard"] == 0 for item in limits["Ulimits"]), "CONTAINER_CORE_LIMIT")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, help="verified local candidate sha256 image ID only")
    parser.add_argument("--binary", required=True)
    parser.add_argument("--production-healthcheck", help="opt into production Compose/file configuration with this same-source binary")
    parser.add_argument("--workload-agent-image", help="also verify seven production agents against this isolated issuer")
    parser.add_argument("--production-user-realm", action="store_true", help="also verify the secret-free production user realm in isolation")
    parser.add_argument("--administrator-recovery", action="store_true", help="also exercise official offline admin recovery in this isolated fixture")
    parser.add_argument("--user-realm-only", action="store_true", help="focused user-realm iteration; does not rerun or claim workload integration")
    parser.add_argument("--run", action="store_true", required=True)
    args = parser.parse_args()
    require(os.name == "posix" and os.geteuid() == 0, "LINUX_OPERATOR_REQUIRED")
    require(re.fullmatch(r"sha256:[a-f0-9]{64}", args.image), "IMMUTABLE_CANDIDATE_REQUIRED")
    binary = Path(args.binary).resolve(strict=True)
    require(binary.is_file(), "TEST_BINARY_REQUIRED")
    healthcheck = Path(args.production_healthcheck).resolve(strict=True) if args.production_healthcheck else None
    require(healthcheck is None or healthcheck.is_file(), "HEALTHCHECK_BINARY_REQUIRED")
    require(not args.workload_agent_image or (healthcheck and re.fullmatch(r"sha256:[a-f0-9]{64}", args.workload_agent_image)),
            "WORKLOAD_AGENT_IMAGE_REQUIRES_PRODUCTION_CONFIG")
    require(not args.user_realm_only or (args.production_user_realm and healthcheck and not args.workload_agent_image),
            "FOCUSED_USER_REALM_REQUIRES_ITS_PRODUCTION_CONFIG")
    require(not args.administrator_recovery or (args.production_user_realm and healthcheck and not args.workload_agent_image),
            "ADMIN_RECOVERY_REQUIRES_ISOLATED_USER_REALM_WITHOUT_CONCURRENT_AGENTS")
    repository = Path(__file__).resolve().parents[1]
    spec = importlib.util.spec_from_file_location("realm", repository / "scripts/account-platform-workload-realm.py")
    realm = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(realm)
    candidate = json.loads(run(["docker", "image", "inspect", args.image]).stdout)[0]
    require(candidate["Id"] == args.image and candidate["Config"]["User"] == "1000", "CANDIDATE_ID_OR_UID")
    run(["docker", "image", "inspect", PG_IMAGE])
    name = "pinvou-kc-production-fixture-" + secrets.token_hex(6)
    root = Path(tempfile.mkdtemp(prefix=name + "-", dir="/tmp")).resolve()
    root.chmod(0o755)
    containers = []
    network = None
    uncertain_creation = False
    test_secrets = []
    try:
        # Only public configuration is reused. No formal path is accepted.
        for directory, uid in (("pg-data", 999), ("pg-tls", 999), ("pg-admin", 999),
                               ("pg-bootstrap", 999), ("issuer", 1000)):
            path = root / directory
            path.mkdir(mode=0o700)
            os.chown(path, uid, 0 if uid == 1000 else uid)
        issuer = root / "issuer"
        keys = ("ACCOUNT_DB_PASSWORD", "BILLING_DB_PASSWORD", "TOKEN_DB_PASSWORD",
                "BILLING_MIGRATION_DB_PASSWORD", "TOKEN_MIGRATION_DB_PASSWORD", "TOKEN_POLICY_DB_PASSWORD",
                "NEWAPI_DB_PASSWORD", "KEYCLOAK_DB_PASSWORD")
        passwords = {key: secrets.token_hex(32) for key in keys}
        pg_admin, admin = secrets.token_hex(32), secrets.token_hex(32)
        test_secrets = [pg_admin, admin, *passwords.values()]
        private(root / "pg-admin/password", pg_admin, 999)
        for key, value in passwords.items():
            private(root / "pg-bootstrap" / key, value, 999)
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
             "-subj", "/CN=Temporary Keycloak fixture CA", "-addext", "basicConstraints=critical,CA:TRUE",
             "-keyout", str(root / "ca.key"), "-out", str(issuer / "ca.crt")])
        (root / "ca.key").chmod(0o600)
        for target, hostname, uid in ((root / "pg-tls", "postgres", 999), (issuer, "localhost", 1000)):
            run(["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=" + hostname,
                 "-keyout", str(target / "server.key"), "-out", str(target / "server.csr")])
            names = hostname + (",DNS:keycloak" if target == issuer and healthcheck else "")
            private(target / "ext", "subjectAltName=DNS:" + names + "\nextendedKeyUsage=serverAuth\n", uid)
            run(["openssl", "x509", "-req", "-in", str(target / "server.csr"), "-CA", str(issuer / "ca.crt"),
                 "-CAkey", str(root / "ca.key"), "-CAcreateserial", "-days", "2", "-extfile", str(target / "ext"),
                 "-out", str(target / "server.crt")])
            for path in target.iterdir():
                path.chmod(0o600)
                os.chown(path, uid, 0 if uid == 1000 else uid)
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
             "-subj", "/CN=Unrelated fixture CA", "-keyout", str(root / "wrong.key"), "-out", str(issuer / "wrong.crt")])
        (root / "wrong.key").chmod(0o600)
        (issuer / "wrong.crt").chmod(0o644)
        private(issuer / "admin-secret", admin, 1000)
        private(issuer / "workload.json", json.dumps(realm.build_realm("pinvou-fixture-workload")), 1000)
        user_realm = {"realm": "pinvou-fixture-users", "enabled": True, "sslRequired": "all", "loginTheme": "pinvou",
                      "internationalizationEnabled": True, "supportedLocales": ["zh-CN", "en", "ja"], "defaultLocale": "zh-CN",
                      "browserFlow": "fixture-browser", "authenticationFlows": [{"alias": "fixture-browser",
                      "providerId": "basic-flow", "topLevel": True, "builtIn": False,
                      "authenticationExecutions": [{"authenticator": "pinvou-browser-authenticator", "requirement": "REQUIRED", "priority": 10}]}],
                      "clients": [{"clientId": "fixture-browser", "enabled": True, "publicClient": True,
                      "standardFlowEnabled": True, "directAccessGrantsEnabled": False,
                      "redirectUris": ["https://localhost:8443/fixture/callback"]}]}
        private(issuer / "users.json", json.dumps(user_realm), 1000)
        shutil.copyfile(binary, issuer / "workload.test")
        (issuer / "workload.test").chmod(0o555)
        # Env file is temporary and not mounted; Docker never receives formal secrets.
        private(root / "issuer.env", "KC_BOOTSTRAP_ADMIN_USERNAME=fixture-admin\nKC_BOOTSTRAP_ADMIN_PASSWORD=" + admin
                + "\nKC_DB_USERNAME=keycloak\nKC_DB_PASSWORD=" + passwords["KEYCLOAK_DB_PASSWORD"] + "\n", 0)
        compose_env = {**os.environ, "POSTGRES_IMAGE": PG_IMAGE,
                       **{"POSTGRES_" + key + "_DIRECTORY": str(root / directory) for key, directory in
                          (("DATA", "pg-data"), ("TLS", "pg-tls"), ("ADMIN", "pg-admin"), ("BOOTSTRAP", "pg-bootstrap"))}}
        compose = ["docker", "compose", "--project-name", name, "-f", str(repository / "account-platform/deploy/production/compose.dependencies.yaml")]
        rendered = json.loads(run([*compose, "config", "--format", "json"], env=compose_env).stdout)
        require(set(rendered["services"]) == {"postgres"}, "FIXED_POSTGRES_SERVICE_ONLY")
        uncertain_creation = True
        run([*compose, "create", "postgres"], env=compose_env)
        pg = run([*compose, "ps", "--all", "--quiet", "postgres"], env=compose_env).stdout.strip()
        require(re.fullmatch(r"[a-f0-9]{64}", pg), "FIXTURE_POSTGRES_ID")
        containers.append((pg, "com.docker.compose.project", name))
        pg_meta = json.loads(run(["docker", "inspect", pg]).stdout)[0]
        network_name = rendered["networks"]["data"]["name"]
        require(set(pg_meta["NetworkSettings"]["Networks"]) == {network_name}, "POSTGRES_NETWORK")
        network_meta = json.loads(run(["docker", "network", "inspect", network_name]).stdout)[0]
        network = network_meta["Id"]
        uncertain_creation = False
        require(network_meta["Internal"] and network_meta["Labels"].get("com.docker.compose.project") == name, "PRIVATE_FIXTURE_NETWORK")
        pg_image_id = json.loads(run(["docker", "image", "inspect", PG_IMAGE]).stdout)[0]["Id"]
        require(pg_meta["Image"] == pg_image_id and pg_meta["Config"]["User"] == "999:999", "POSTGRES_IMAGE_OR_UID")
        verify_limits(pg_meta, 1024 * 1024 * 1024, 192)
        pg_config = rendered["services"]["postgres"]
        verify_mounts(pg_meta, [(m["source"], m["target"], m.get("read_only", False)) for m in pg_config["volumes"]],
                      dict(value.split(":", 1) for value in pg_config["tmpfs"]))
        run(["docker", "update", "--restart=no", pg])
        run(["docker", "start", pg])
        for _ in range(120):
            state = json.loads(run(["docker", "inspect", pg]).stdout)[0]["State"]
            require(state["Running"], "POSTGRES_START_FAILED")
            if state.get("Health", {}).get("Status") == "healthy":
                break
            time.sleep(0.5)
        else:
            raise RuntimeError("POSTGRES_READY_TIMEOUT")
        pg_meta = json.loads(run(["docker", "inspect", pg]).stdout)[0]

        def start_issuer(suffix, host="postgres", ca="ca.crt"):
            nonlocal uncertain_creation
            if healthcheck:
                return start_file_issuer(suffix, host, ca)
            jdbc = "jdbc:postgresql://" + host + ":5432/account_platform_keycloak?sslmode=verify-full&sslrootcert=/fixture/" + ca
            command = ["docker", "create", "--name", name + "-" + suffix, "--label", "pinvou.fixture=" + name,
                       "--network", network, "--user", "1000:0", "--read-only", "--cap-drop", "ALL",
                       "--security-opt", "no-new-privileges", "--ulimit", "core=0:0", "--memory", "1536m", "--cpus", "1", "--pids-limit", "256",
                       "--log-driver", "local", "--log-opt", "max-size=2m", "--log-opt", "max-file=2",
                       "--env-file", str(root / "issuer.env"), "--env", "JAVA_OPTS_KC_HEAP=-Xms128m -Xmx512m",
                       "--env", "KC_DB_URL=" + jdbc, "--mount", f"type=bind,src={issuer},dst=/fixture,readonly",
                       "--mount", f"type=bind,src={issuer / 'workload.json'},dst=/opt/keycloak/data/import/pinvou-fixture-workload-realm.json,readonly",
                       "--mount", f"type=bind,src={issuer / 'users.json'},dst=/opt/keycloak/data/import/pinvou-fixture-users-realm.json,readonly",
                       "--tmpfs", "/tmp:rw,nosuid,nodev,size=128m,mode=1777",
                       "--tmpfs", "/opt/keycloak/data:rw,nosuid,nodev,size=128m,uid=1000,gid=0,mode=0700",
                       args.image, "start", "--optimized", "--http-enabled=false", "--hostname=https://localhost:8443",
                       "--https-certificate-file=/fixture/server.crt", "--https-certificate-key-file=/fixture/server.key", "--import-realm"]
            uncertain_creation = True
            cid = run(command).stdout.strip()
            require(re.fullmatch(r"[a-f0-9]{64}", cid), "FIXTURE_ISSUER_ID")
            containers.append((cid, "pinvou.fixture", name))
            uncertain_creation = False
            meta = json.loads(run(["docker", "inspect", cid]).stdout)[0]
            require(meta["Image"] == args.image and meta["Config"]["User"] == "1000:0", "ISSUER_IMAGE_OR_UID")
            verify_limits(meta, 1536 * 1024 * 1024, 256)
            require(set(meta["NetworkSettings"]["Networks"]) == {network_name}, "ISSUER_NETWORK")
            verify_mounts(meta, [(str(issuer), "/fixture", True),
                          (str(issuer / "workload.json"), "/opt/keycloak/data/import/pinvou-fixture-workload-realm.json", True),
                          (str(issuer / "users.json"), "/opt/keycloak/data/import/pinvou-fixture-users-realm.json", True)],
                          {"/tmp": "rw,nosuid,nodev,size=128m,mode=1777",
                           "/opt/keycloak/data": "rw,nosuid,nodev,size=128m,uid=1000,gid=0,mode=0700"})
            run(["docker", "start", cid])
            return cid

        def start_file_issuer(suffix, host, ca):
            nonlocal uncertain_creation
            production = repository / "account-platform/deploy/production"
            config = root / ("keycloak-config-" + suffix)
            config.mkdir(mode=0o700)
            os.chown(config, 1000, 0)
            text = (production / "keycloak.conf.template").read_text()
            text = text.replace("@DATABASE_PASSWORD@", passwords["KEYCLOAK_DB_PASSWORD"])
            text = text.replace("@BOOTSTRAP_ADMIN_PASSWORD@", admin)
            text = text.replace("bootstrap-admin-username=pinvou-bootstrap-admin", "bootstrap-admin-username=fixture-admin")
            text = text.replace("hostname=https://keycloak:8443", "hostname=https://localhost:8443")
            text = text.replace("jdbc:postgresql://postgres:", "jdbc:postgresql://" + host + ":")
            text = text.replace("sslrootcert=/run/pinvou/keycloak-trust/ca.crt", "sslrootcert=/run/pinvou/keycloak-trust/" + ca)
            require("@" not in text, "UNRENDERED_KEYCLOAK_CONFIG")
            private(config / "keycloak.conf", text, 1000)
            env = {**os.environ, "KEYCLOAK_IMAGE": args.image, "KEYCLOAK_CONFIG_DIRECTORY": str(config),
                   "KEYCLOAK_TLS_DIRECTORY": str(issuer), "KEYCLOAK_TRUST_DIRECTORY": str(issuer),
                   "KEYCLOAK_HEALTHCHECK_BINARY": str(healthcheck), "KEYCLOAK_DATA_NETWORK": network_name}
            overlay = root / ("keycloak-" + suffix + ".json")
            fixture_binds = [(str(issuer), "/fixture", True),
                             (str(issuer / "workload.json"), "/opt/keycloak/data/import/pinvou-fixture-workload-realm.json", True),
                             (str(issuer / "users.json"), "/opt/keycloak/data/import/pinvou-fixture-users-realm.json", True)]
            override = {"services": {"keycloak": {
                "command": ["--config-file=/run/pinvou/keycloak-config/keycloak.conf", "start", "--optimized", "--import-realm"],
                "volumes": [{"type": "bind", "source": src, "target": dst, "read_only": ro,
                             "bind": {"create_host_path": False}} for src, dst, ro in fixture_binds]}}}
            private(overlay, json.dumps(override), 0)
            project = name + "-" + suffix
            command = ["docker", "compose", "--project-name", project, "-f", str(production / "compose.keycloak.yaml"), "-f", str(overlay)]
            rendered = json.loads(run([*command, "config", "--format", "json"], env=env).stdout)
            require(set(rendered["services"]) == {"keycloak"}, "KEYCLOAK_ONLY_PROJECT")
            require(rendered["networks"]["data"]["external"] is True
                    and rendered["networks"]["data"]["name"] == network_name, "KEYCLOAK_FIXED_EXTERNAL_NETWORK")
            uncertain_creation = True
            run([*command, "create", "keycloak"], env=env)
            cid = run([*command, "ps", "--all", "--quiet", "keycloak"], env=env).stdout.strip()
            require(re.fullmatch(r"[a-f0-9]{64}", cid), "FIXTURE_ISSUER_ID")
            containers.append((cid, "com.docker.compose.project", project))
            uncertain_creation = False
            meta = json.loads(run(["docker", "inspect", cid]).stdout)[0]
            require(meta["Image"] == args.image and meta["Config"]["User"] == "1000:0"
                    and not meta["HostConfig"]["Privileged"] and not meta["HostConfig"].get("CapAdd"), "ISSUER_BOUNDARY")
            require(not any(value.startswith(("KC_DB_", "KC_BOOTSTRAP_", "KC_HOSTNAME", "KC_HTTP", "KC_HTTPS", "KC_PROXY"))
                            for value in meta["Config"]["Env"]), "ENVIRONMENT_CONFIGURATION_OVERRIDE")
            require(meta["Config"]["Cmd"] == override["services"]["keycloak"]["command"], "COMMAND_CONFIGURATION_OVERRIDE")
            require(set(meta["NetworkSettings"]["Networks"]) == {network_name}, "ISSUER_NETWORK")
            verify_limits(meta, 1536 * 1024 * 1024, 256)
            verify_mounts(meta, [(str(config), "/run/pinvou/keycloak-config", True),
                                (str(issuer), "/run/pinvou/keycloak-tls", True),
                                (str(issuer), "/run/pinvou/keycloak-trust", True),
                                (str(healthcheck), "/opt/pinvou/healthcheck", True), *fixture_binds],
                          {"/tmp": "rw,nosuid,nodev,size=128m,mode=1777",
                           "/opt/keycloak/data": "rw,nosuid,nodev,size=128m,uid=1000,gid=0,mode=0700"})
            run(["docker", "update", "--restart=no", cid])
            run(["docker", "start", cid])
            return cid

        kc = start_issuer("positive")
        # Test binary stays in the issuer namespace: fixed loopback URL, no arbitrary target.
        if args.user_realm_only:
            for _ in range(120):
                state = json.loads(run(["docker", "inspect", kc]).stdout)[0]["State"]
                if state.get("Health", {}).get("Status") == "healthy":
                    break
                time.sleep(1)
            require(state.get("Health", {}).get("Status") == "healthy", "FOCUSED_USER_REALM_ISSUER_NOT_READY")
            print("USER_REALM_FOCUSED_RUN_WORKLOAD_INTEGRATION_NOT_RUN")
        else:
            result = run(["docker", "exec", "--env", "SSL_CERT_FILE=/fixture/ca.crt", "--env", "GOMEMLIMIT=128MiB",
                          "--env", "PINVOU_TEST_KEYCLOAK_ISOLATED=1", kc, "/fixture/workload.test",
                          "-test.run=^TestKeycloakWorkloadIntegration$", "-test.v", "-test.timeout=240s"], timeout=250, check=False)
            require(not any(value in result.stdout + result.stderr for value in test_secrets), "TEST_SECRET_OUTPUT")
            require(result.returncode == 0 and "--- PASS: TestKeycloakWorkloadIntegration" in result.stdout and "SKIP" not in result.stdout, "WORKLOAD_ACTUAL_TEST_FAILED")
            for service in realm.SERVICES:
                require("--- PASS: TestKeycloakWorkloadIntegration/actual_identity_" + service + " " in result.stdout, "NAMED_SERVICE_ACCEPTANCE_MISSING")
                print("KEYCLOAK_ACTUAL_IDENTITY_PASS: " + service)
            print("KEYCLOAK_WORKLOAD_REFRESH_EXPIRY_ROTATION_PERMISSIONS_PASS")
        tls_sql = "SELECT count(*) FROM pg_stat_activity a JOIN pg_stat_ssl s USING(pid) WHERE a.usename='keycloak' AND s.ssl;"
        count = run(["docker", "exec", "-i", "-u", "999:999", pg, "psql", "-X", "-At", "-U", "account_platform_admin", "-d", "postgres", "--set=ON_ERROR_STOP=1"], data=pg_admin + "\n" + tls_sql + "\n").stdout.strip()
        require(count.isdigit() and int(count) > 0, "ACTUAL_JDBC_TLS_NOT_OBSERVED")
        print("KEYCLOAK_PRODUCTION_POSTGRES_TLS_PASS" if args.user_realm_only else "KEYCLOAK_PRODUCTION_POSTGRES_TLS_AND_SEVEN_IDENTITIES_PASS")
        # Host-side TLS probe only to this inspected temporary container.
        ip = json.loads(run(["docker", "inspect", kc]).stdout)[0]["NetworkSettings"]["Networks"][network_name]["IPAddress"]
        probe_issuer(ip, issuer, admin)
        if healthcheck:
            for _ in range(30):
                state = json.loads(run(["docker", "inspect", kc]).stdout)[0]["State"]
                if state.get("Health", {}).get("Status") == "healthy":
                    break
                time.sleep(1)
            require(state["Health"]["Status"] == "healthy", "PRODUCTION_TLS_READINESS_NOT_HEALTHY")
            print("KEYCLOAK_PRODUCTION_FILE_CONFIG_AND_COMPOSE_READINESS_PASS")
        if args.production_user_realm:
            user_spec = importlib.util.spec_from_file_location("user_fixture", repository / "scripts/account-platform-user-realm-fixture.py")
            user_fixture = importlib.util.module_from_spec(user_spec)
            user_spec.loader.exec_module(user_fixture)
            try:
                user_fixture.verify(repository, kc, network_name, issuer, admin)
            except Exception as error:
                trace = error.__traceback__
                lines = []
                while trace:
                    lines.append(str(trace.tb_lineno))
                    trace = trace.tb_next
                print("USER_REALM_FIXTURE_FAILURE_TYPE=" + type(error).__name__ + " LINES=" + ",".join(lines[-3:]), flush=True)
                raise
        if args.workload_agent_image:
            workload_spec = importlib.util.spec_from_file_location("workload_fixture", repository / "scripts/account-platform-workload-production-fixture.py")
            workload_fixture = importlib.util.module_from_spec(workload_spec)
            workload_spec.loader.exec_module(workload_fixture)
            # Any unconfirmed multi-container creation preserves the entire
            # isolated environment; never guess cleanup after a partial result.
            uncertain_creation = True
            try:
                workload_fixture.verify(root, repository, kc, network_name, issuer, admin, args.workload_agent_image, containers)
            except Exception as error:
                # Fixed type and source line only: never print HTTP bodies,
                # subprocess output, tokens or arbitrary exception messages.
                trace = error.__traceback__
                while trace.tb_next:
                    trace = trace.tb_next
                print("WORKLOAD_FIXTURE_FAILURE_TYPE=" + type(error).__name__ + " LINE=" + str(trace.tb_lineno), flush=True)
                raise
            uncertain_creation = False
        if args.administrator_recovery:
            recovery_spec = importlib.util.spec_from_file_location("recovery_fixture", repository / "scripts/account-platform-keycloak-recovery-fixture.py")
            recovery_fixture = importlib.util.module_from_spec(recovery_spec)
            recovery_spec.loader.exec_module(recovery_fixture)
            uncertain_creation = True
            try:
                recovery_fixture.verify(root, kc, pg, network_name, issuer, admin, pg_admin, containers, run, verify_limits, verify_mounts)
            except Exception as error:
                trace = error.__traceback__
                while trace.tb_next:
                    trace = trace.tb_next
                print("RECOVERY_FIXTURE_FAILURE_TYPE=" + type(error).__name__ + " LINE=" + str(trace.tb_lineno), flush=True)
                raise
            uncertain_creation = False
        run(["docker", "stop", "--time", "20", kc])
        for suffix, host, ca, evidence in (("wrong-ca", "postgres", "wrong.crt", "PKIX path building failed"),
                                            ("wrong-host", pg_meta["NetworkSettings"]["Networks"][network_name]["IPAddress"], "ca.crt", "could not be verified by hostnameverifier")):
            bad = start_issuer(suffix, host, ca)
            for _ in range(120):
                state = json.loads(run(["docker", "inspect", bad]).stdout)[0]["State"]
                if not state["Running"]:
                    break
                time.sleep(0.5)
            require(not state["Running"] and state["ExitCode"] != 0 and not state["OOMKilled"], "TLS_NEGATIVE_MUST_EXIT")
            logs = run(["docker", "logs", bad])
            require(evidence.lower() in (logs.stdout + logs.stderr).lower(), "PRECISE_JDBC_CERTIFICATE_REFUSAL_MISSING")
            print("KEYCLOAK_JDBC_" + suffix.upper().replace("-", "_") + "_REJECTED")
    finally:
        require(not uncertain_creation, "CREATION_UNCERTAIN_KEEP_FIXTURE_RESOURCES_AND_DIRECTORY")
        for cid, key, value in reversed(containers):
            meta = json.loads(run(["docker", "inspect", cid]).stdout)[0]
            require(meta["Config"]["Labels"].get(key) == value, "CLEANUP_CONTAINER_LABEL")
            run(["docker", "rm", "--force", cid])
        if network:
            meta = json.loads(run(["docker", "network", "inspect", network]).stdout)[0]
            require(meta["Labels"].get("com.docker.compose.project") == name, "CLEANUP_NETWORK_LABEL")
            run(["docker", "network", "rm", network])
        require(root.parent == Path("/tmp") and root.name.startswith(name + "-"), "CLEANUP_DIRECTORY_BOUNDARY")
        shutil.rmtree(root)


def probe_issuer(ip, directory, admin):
    # Connect to the inspected IP with localhost SNI; never disable name checks.
    import http.client

    class Connection(http.client.HTTPSConnection):
        def connect(self):
            import socket
            sock = socket.create_connection((ip, self.port), timeout=5)
            self.sock = self._context.wrap_socket(sock, server_hostname="localhost")

    context = ssl.create_default_context(cafile=str(directory / "ca.crt"))

    def request(method, path, data=None, token=None, port=8443):
        client = Connection("localhost", port, context=context, timeout=5)
        headers = {"Content-Type": "application/x-www-form-urlencoded"}
        if token:
            headers["Authorization"] = "Bearer " + token
        try:
            client.request(method, path, data, headers)
            response = client.getresponse()
            payload = response.read(1024 * 1024 + 1)
            require(len(payload) <= 1024 * 1024, "PROBE_RESPONSE_LIMIT")
            return response.status, payload
        finally:
            client.close()

    status, body = request("GET", "/health/ready", port=9000)
    require(status == 200 and json.loads(body).get("status") == "UP", "PRIVATE_HTTPS_READINESS_FAILED")
    status, body = request("POST", "/realms/master/protocol/openid-connect/token", urllib.parse.urlencode(
        {"grant_type": "password", "client_id": "admin-cli", "username": "fixture-admin", "password": admin}))
    require(status == 200, "PROBE_ADMIN_AUTH")
    token = json.loads(body)["access_token"]
    status, body = request("GET", "/admin/realms/pinvou-fixture-users/authentication/authenticator-providers", token=token)
    require(status == 200, "PROBE_PROVIDER_LIST")
    ids = {item["id"] for item in json.loads(body)}
    require({"pinvou-browser-authenticator", "pinvou-mobile-direct-grant"} <= ids, "BOTH_RUNTIME_PROVIDERS_REQUIRED")
    status, body = request("GET", "/admin/realms/pinvou-fixture-users/clients?clientId=account-service", token=token)
    require(status == 200 and json.loads(body) == [], "WORKLOAD_CLIENT_LEAKED_TO_USER_REALM")
    status, _ = request("POST", "/realms/pinvou-fixture-users/protocol/openid-connect/token",
                        "grant_type=client_credentials&client_id=account-service&client_secret=fixture-invalid")
    require(status == 401, "CROSS_REALM_CLIENT_NOT_REJECTED")
    params = urllib.parse.urlencode({"client_id": "fixture-browser", "response_type": "code", "scope": "openid",
                                    "redirect_uri": "https://localhost:8443/fixture/callback", "state": "fixture-state"})
    status, page = request("GET", "/realms/pinvou-fixture-users/protocol/openid-connect/auth?" + params)
    require(status == 200, "CUSTOM_LOGIN_FORM_HTTP_STATUS_" + str(status))
    require(b'pv-auth-form' in page and b'pinvou-login.css' in page, "CUSTOM_LOGIN_FORM_NOT_RENDERED")
    assets = re.findall(rb'(?:href|src)="([^"]+pinvou-login\.(?:css|js))"', page)
    require(len(assets) == 2, "BOTH_THEME_ASSETS_REQUIRED")
    for asset in assets:
        url = urllib.parse.urlsplit(asset.decode())
        require(not url.netloc or url.netloc == "localhost:8443", "THEME_EXTERNAL_ORIGIN")
        status, payload = request("GET", url.path)
        require(status == 200 and len(payload) > 20, "CUSTOM_THEME_RESOURCE_UNAVAILABLE")
    print("KEYCLOAK_RUNTIME_PROVIDERS_REALM_SEPARATION_AND_THEME_PASS")


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, subprocess.SubprocessError, OSError, ValueError, KeyError) as error:
        marker = str(error) if isinstance(error, RuntimeError) else type(error).__name__
        raise SystemExit("KEYCLOAK_ISOLATED_PRODUCTION_FIXTURE_FAILED: " + marker) from None

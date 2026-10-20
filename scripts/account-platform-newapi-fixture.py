"""Opt-in rc.26 fresh-database acceptance; never opens existing business data."""

import argparse
import ipaddress
import json
import os
import re
import secrets
import subprocess
import time
import traceback
import urllib.error
import urllib.request

INDEX = "sha256:7b82c866e9c0fb02faca7d5068e9d1261e47edf52462d63d9db7b614e7f1027d"
REVISION = "8f6961c675932f406260ff0c218bc2aa0603e9b2"
LABEL = "io.pinvou.newapi-fixture"


def require(condition, stage):
    if not condition:
        raise RuntimeError(stage)


def docker(*args, payload=None):
    # No raw Docker error or command echo: stdin can contain ephemeral secrets.
    result = subprocess.run(["docker", *args], input=payload, capture_output=True,
                            text=True, timeout=60, check=False)
    require(result.returncode == 0, "docker_" + args[0] + "_failed")
    return result.stdout.strip()


def inspect(kind, target):
    return json.loads(docker(kind, "inspect", target))[0]


def validate_image(reference, metadata):
    require(reference.endswith("@" + INDEX), "newapi_digest_mismatch")
    labels = metadata.get("Config", {}).get("Labels", {}) or {}
    require(metadata.get("Os") == "linux" and metadata.get("Architecture") == "amd64",
            "newapi_platform_mismatch")
    require(labels.get("org.opencontainers.image.revision") == REVISION
            and labels.get("org.opencontainers.image.version") == "v1.0.0-rc.26"
            and labels.get("org.opencontainers.image.source") == "https://github.com/QuantumNous/new-api",
            "newapi_provenance_mismatch")


def owned(metadata, kind, run_id):
    labels = metadata.get("Config", {}).get("Labels", {}) if kind == "container" else metadata.get("Labels", {})
    return isinstance(labels, dict) and labels.get(LABEL) == run_id


def cleanup(resources, run_id):
    failures = []
    for kind, target in reversed(resources):
        try:
            require(owned(inspect(kind, target), kind, run_id), "cleanup_ownership_mismatch")
            docker(kind, "rm", *(["--force"] if kind == "container" else []), target)
        except Exception:
            failures.append(kind)
    require(not failures, "fixture_cleanup_failed")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise RuntimeError("fixture_http_redirect_rejected")


class API:
    def __init__(self, address):
        require(ipaddress.ip_address(address).is_private, "fixture_address_not_private")
        self.origin = "http://" + address + ":3000"
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def raw_request(self, path, body=None, headers=None, method=None):
        request = urllib.request.Request(self.origin + path,
                                         data=None if body is None else json.dumps(body).encode(),
                                         headers={"Content-Type": "application/json", **(headers or {})}, method=method)
        try:
            response = self.opener.open(request, timeout=3)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read(1024 * 1024 + 1)
            require(len(raw) <= 1024 * 1024, "fixture_http_body_too_large")
            return response.status, dict(response.headers), raw

    def request(self, path, body=None, headers=None, method=None):
        status, _, raw = self.raw_request(path, body, headers, method)
        return status, json.loads(raw)


def eventually(probe, stage):
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        try:
            if probe():
                return
        except (RuntimeError, ValueError, OSError, subprocess.TimeoutExpired):
            pass
        time.sleep(1)
    raise RuntimeError(stage)


def run(newapi_image, postgres_image, scenario=None):
    require(os.name == "posix", "fixture_requires_linux")
    validate_image(newapi_image, inspect("image", newapi_image))
    require(re.fullmatch(r"sha256:[a-f0-9]{64}", postgres_image) is not None,
            "postgres_requires_local_image_id")
    pg_metadata = inspect("image", postgres_image)
    require(pg_metadata.get("Os") == "linux" and pg_metadata.get("Architecture") == "amd64",
            "postgres_platform_mismatch")
    run_id = secrets.token_hex(12)
    prefix = "pinvou-newapi-fixture-" + run_id
    resources = []
    checks = []
    try:
        net = docker("network", "create", "--internal", "--label", LABEL + "=" + run_id, prefix)
        resources.append(("network", net))
        scenario_env = scenario.prepare(net, resources, run_id) if scenario else {}
        require(set(scenario_env).issubset({"REDIS_CONN_STRING"}), "scenario_environment_invalid")
        pg_env = "POSTGRES_USER=fixture\nPOSTGRES_DB=fixture\nPOSTGRES_PASSWORD=" + secrets.token_hex(32) + "\n"
        pg_password = pg_env.split("POSTGRES_PASSWORD=", 1)[1].strip()
        pg = docker("create", "--pull=never", "--name", prefix + "-pg", "--network", net,
                    "--network-alias", "fixture-pg", "--label", LABEL + "=" + run_id,
                    "--user", "999:999", "--read-only", "--cap-drop", "ALL",
                    "--security-opt", "no-new-privileges", "--log-driver", "none",
                    "--memory", "768m", "--cpus", "1", "--pids-limit", "128",
                    "--tmpfs", "/var/lib/postgresql/data:rw,nosuid,nodev,size=384m,uid=999,gid=999,mode=0700",
                    "--tmpfs", "/var/run/postgresql:rw,nosuid,nodev,size=16m,uid=999,gid=999,mode=0700",
                    "--tmpfs", "/tmp:rw,nosuid,nodev,size=16m,mode=1777",
                    "--env-file", "/dev/stdin", postgres_image, payload=pg_env)
        resources.append(("container", pg))
        docker("start", pg)
        eventually(lambda: docker("exec", pg, "pg_isready", "-U", "fixture", "-d", "fixture").endswith("accepting connections"),
                   "postgres_not_ready")
        def sql(query):
            return docker("exec", "-i", pg, "psql", "-U", "fixture", "-d", "fixture", "-At", "-v", "ON_ERROR_STOP=1", payload=query)
        require(sql("SHOW server_version_num;").startswith("17"), "postgres_version_mismatch")
        require(sql("SELECT count(*) FROM information_schema.tables WHERE table_schema='public';") == "0", "database_not_empty")
        checks.append("fresh_postgres")
        env = {
            "SQL_DSN": "postgresql://fixture:" + pg_password + "@fixture-pg:5432/fixture?sslmode=disable",
            "PORT": "3000", "SESSION_SECRET": secrets.token_hex(32), "CRYPTO_SECRET": secrets.token_hex(32),
            "GIN_MODE": "release", "TRUSTED_PROXIES": "none", "NODE_TYPE": "master",
            "BATCH_UPDATE_ENABLED": "true", "ERROR_LOG_ENABLED": "true",
        }
        env.update(scenario_env)
        app = docker("create", "--pull=never", "--name", prefix + "-app", "--network", net,
                     "--label", LABEL + "=" + run_id, "--user", "65532:65532", "--read-only",
                     "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--log-driver", "none",
                     "--memory", "512m", "--cpus", "1", "--pids-limit", "128",
                     "--tmpfs", "/data:rw,nosuid,nodev,size=64m,uid=65532,gid=65532,mode=0700",
                     "--tmpfs", "/tmp:rw,nosuid,nodev,size=16m,uid=65532,gid=65532,mode=1777",
                     "--env-file", "/dev/stdin", newapi_image,
                     payload="".join(key + "=" + value + "\n" for key, value in env.items()))
        resources.append(("container", app))
        docker("start", app)
        metadata = inspect("container", app)
        require(not metadata["HostConfig"].get("PortBindings"), "fixture_published_port")
        networks = list(metadata["NetworkSettings"]["Networks"].values())
        require(len(networks) == 1 and networks[0]["NetworkID"] == net, "fixture_network_invalid")
        network = inspect("network", net)
        require(network["Internal"] is True and owned(network, "network", run_id), "fixture_network_not_owned_internal")
        # Docker may deliberately suppress port publishing on internal networks.
        # The host runner probes only its own container's isolated bridge IP.
        api = API(networks[0]["IPAddress"])
        def ready():
            status, body = api.request("/api/status")
            return status == 200 and body.get("success") is True and body.get("data", {}).get("version") == "v1.0.0-rc.26"
        eventually(ready, "newapi_not_ready")
        require(docker("exec", app, "id", "-u") == "65532", "newapi_not_nonroot")
        checks.append("nonroot_readonly_startup")
        status, setup = api.request("/api/setup")
        require(status == 200 and setup.get("success") is True and setup.get("data", {}).get("status") is False
                and setup["data"].get("root_init") is False, "fresh_setup_state_invalid")
        password = secrets.token_urlsafe(24)
        setup_body = {"username": "fixtureadmin", "password": password, "confirmPassword": password,
                      "SelfUseModeEnabled": False, "DemoSiteEnabled": False}
        require(api.request("/api/setup", setup_body)[1].get("success") is True, "setup_failed")
        require(api.request("/api/setup", setup_body)[1].get("success") is False, "duplicate_setup_not_rejected")
        checks.append("setup_once")
        login_status, login = api.request("/api/user/login", {"username": "fixtureadmin", "password": password})
        require(login_status == 200 and login.get("success") is True, "login_failed")
        checks.append("admin_login")
        for path in ["/api/user/", "/api/token/", "/api/log/", "/v1/models"]:
            status, body = api.request(path)
            require(status in (401, 403), "anonymous_access_not_denied")
        checks.append("anonymous_api_denied")
        require(sql("SELECT data_type FROM information_schema.columns WHERE table_schema='public' AND table_name='users' AND column_name='quota';") == "bigint",
                "quota_column_not_bigint")
        require(sql("UPDATE users SET quota=5000000000 WHERE username='fixtureadmin'; SELECT quota FROM users WHERE username='fixtureadmin';").splitlines()[-1] == "5000000000",
                "large_quota_roundtrip_failed")
        checks.append("quota_bigint_roundtrip")
        docker("restart", app)
        eventually(ready, "newapi_restart_not_ready")
        require(api.request("/api/setup")[1].get("data", {}).get("status") is True, "setup_not_persisted")
        require(sql("SELECT quota FROM users WHERE username='fixtureadmin';") == "5000000000", "quota_not_persisted")
        checks.append("application_restart")
        if scenario:
            checks.extend(scenario.verify(api, sql, login))
    finally:
        cleanup(resources, run_id)
    print(json.dumps({"result": "NEWAPI_BUSINESS_FIXTURE_PASS" if scenario else "NEWAPI_FRESH_FIXTURE_PASS", "checks": checks,
                      "version": "v1.0.0-rc.26", "index": INDEX, "production_changed": False}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--isolated-fixture", action="store_true", required=True)
    parser.add_argument("--newapi-image", required=True)
    parser.add_argument("--postgres-image", required=True)
    args = parser.parse_args()
    try:
        run(args.newapi_image, args.postgres_image)
    except Exception as error:
        # Only our fixed codes are safe; never print a traceback/HTTP/Docker body.
        code = str(error) if isinstance(error, RuntimeError) and re.fullmatch(r"[a-z_]+", str(error)) else "unexpected_failure"
        print("NEWAPI_FRESH_FIXTURE_FAIL:" + code)
        if code == "unexpected_failure":
            # Source line numbers only, never exception text, locals or source lines.
            print(json.dumps({"fixture_lines": [frame.lineno for frame in traceback.extract_tb(error.__traceback__)
                                                 if frame.filename == __file__]}))
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

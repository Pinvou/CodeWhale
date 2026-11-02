#!/usr/bin/env python3
"""Run production-history and managed-control contracts on owned Docker PG.

No supplied database URL is accepted. Containers have no published ports or
external network during verification. Source/CLI hashes and raw no-skip results
are retained; dependency preparation is the only network-enabled tools step.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import runpy
import re
import shutil
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit
from urllib.request import url2pathname
import uuid

ROOT = Path(__file__).resolve().parents[1]
POSTGRES = "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"
POSTGRES_PROFILES = {
    "17": POSTGRES,
    "18.4": "postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15",
}
LABEL = "pinvou.deployment-contracts"


def run_logged_command(arguments, logfile, *, cwd=None, dependency_preparation=False):
    """Keep the complete private log; emit bounded, redacted failure evidence."""
    try:
        with logfile.open("w") as output:
            subprocess.run(arguments, check=True, stdout=output, stderr=subprocess.STDOUT, cwd=cwd)
    except subprocess.CalledProcessError as error:
        with logfile.open("rb") as output:
            output.seek(0, os.SEEK_END)
            offset = max(0, output.tell() - 65536)
            output.seek(offset)
            tail_bytes = output.read()
        if offset:
            # A partial first line may have lost a URL/auth prefix needed for
            # redaction. Only complete lines can enter the public diagnostic.
            boundary = tail_bytes.find(b"\n")
            tail_bytes = tail_bytes[boundary + 1:] if boundary >= 0 else b""
        tail = tail_bytes.decode("utf-8", errors="replace")
        # A caller may configure an authenticated registry or proxy.
        for name, value in os.environ.items():
            if len(value) >= 4 and re.search(r"TOKEN|PASSWORD|SECRET|CREDENTIAL|API_KEY", name, re.I):
                tail = tail.replace(value, "[redacted]")
        tail = re.sub(r"(?i)\b(?:https?|git\+https?|postgres(?:ql)?)://[^\s<>\"']+", "[URL redacted]", tail)
        tail = re.sub(r"(?i)(authorization\s*[:=]\s*(?:bearer|basic)\s+)\S+", r"\1[redacted]", tail)
        tail = re.sub(r"(?i)((?:_authToken|password|token|secret)\s*[:=]\s*)\S+", r"\1[redacted]", tail)
        tail = "\n".join(tail.splitlines()[-100:])[-24000:]
        label = "DEPENDENCY_PREPARATION_FAILED" if dependency_preparation else "CONTRACT_COMMAND_FAILED"
        print(label + " exit=" + str(error.returncode) + " log=" + logfile.name, file=sys.stderr)
        print(tail, file=sys.stderr)
        # CalledProcessError includes argv, which can contain GOPROXY secrets.
        message = "dependency preparation failed; mandatory contracts did not run" if dependency_preparation else "deployment contract command failed; see redacted diagnostic"
        raise RuntimeError(message) from None


def prepare_dependencies(arguments, logfile, *, cwd=None):
    run_logged_command(arguments, logfile, cwd=cwd, dependency_preparation=True)


def ensure_postgres_image(reference, logfile):
    """Reuse only the exact supported manifest; a tag alone is never a hit."""
    if reference not in POSTGRES_PROFILES.values():
        raise RuntimeError("PostgreSQL image must use a supported pinned digest")
    digest = reference.split("@", 1)[1]
    expected = {repository + "@" + digest for repository in (
        "postgres", "library/postgres", "docker.io/library/postgres",
    )}

    def inspect():
        result = subprocess.run(
            ["docker", "image", "inspect", reference, "--format", "{{json .RepoDigests}}"],
            text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30,
        )
        if result.returncode:
            return False
        try:
            digests = json.loads(result.stdout)
        except (ValueError, TypeError):
            raise RuntimeError("PostgreSQL image cache returned invalid digest metadata") from None
        if not isinstance(digests, list) or not all(isinstance(item, str) for item in digests) or not expected.intersection(digests):
            raise RuntimeError("PostgreSQL image cache does not match the pinned manifest")
        return True

    if inspect():
        logfile.write_text("POSTGRES_IMAGE_CACHE_HIT " + reference + "\n")
        print("POSTGRES_IMAGE_CACHE_HIT " + reference)
        return
    prepare_dependencies(["docker", "pull", reference], logfile)
    if not inspect():
        raise RuntimeError("PostgreSQL image is unavailable after preparation")


def dependency_cache_options(environment):
    """Expose download caches only to preparation, never replace its clean /gomod."""
    mounts, values, destinations = [], [], {}

    def mount(path, readonly):
        path = Path(path).expanduser()
        if not path.is_absolute() or any(char in str(path) for char in ",\r\n"):
            raise RuntimeError("dependency cache must have an absolute Docker-bind-safe path")
        if not path.is_dir():
            return None
        path = path.resolve()
        if path == Path(path.anchor) or any(char in str(path) for char in ",\r\n"):
            raise RuntimeError("dependency cache cannot expose a filesystem root or unsafe path")
        key = (str(path), readonly)
        if key not in destinations:
            destination = "/runner-download-cache-" + str(len(destinations))
            destinations[key] = destination
            mounts.extend(["--mount", f"type=bind,src={path},dst={destination}" + (",readonly" if readonly else "")])
        return destinations[key]

    proxy = environment.get("GOPROXY") or "https://proxy.golang.org,direct"
    if proxy != "off":
        local = None
        if environment.get("GOMODCACHE"):
            local = mount(Path(environment["GOMODCACHE"]) / "cache/download", True)
        parts = re.split(r"([,|])", proxy)
        for index in range(0, len(parts), 2):
            if not parts[index].startswith("file:"):
                continue
            uri = urlsplit(parts[index])
            if uri.netloc not in ("", "localhost") or uri.query or uri.fragment:
                raise RuntimeError("file Go proxy must identify a local directory")
            destination = mount(url2pathname(uri.path), True)
            if destination:
                parts[index] = "file://" + destination
        proxy = ("file://" + local + "," if local else "") + "".join(parts)
    values.append("GOPROXY=" + proxy)
    for name in ("GOSUMDB", "GOPRIVATE", "GONOSUMDB", "GONOPROXY", "GOVCS"):
        if name in environment:
            values.append(name + "=" + environment[name])
    if environment.get("NPM_CONFIG_CACHE"):
        destination = mount(environment["NPM_CONFIG_CACHE"], False)
        if destination:
            values += ["NPM_CONFIG_CACHE=" + destination, "NPM_CONFIG_PREFER_OFFLINE=true"]
    return mounts, values


def warm_runner_go_cache(source, logfile):
    """In Actions, fill the existing Runner cache through its configured Go tool."""
    if os.environ.get("GITHUB_ACTIONS") != "true" or not os.environ.get("GOMODCACHE"):
        return False
    go = shutil.which("go")
    if not go:
        return False  # Standalone Docker-only operators need no host Go toolchain.
    cache = Path(os.environ["GOMODCACHE"]).expanduser()
    if not cache.is_absolute() or cache.resolve() == Path(cache.anchor):
        raise RuntimeError("Runner Go cache must be an explicit non-root absolute path")
    prepare_dependencies([go, "mod", "download"], logfile, cwd=source)
    return True


def published_history(root, output):
    manifest = json.loads((root / "account-platform/test/fixtures/production-history-manifest.json").read_text())
    if manifest.get("version") != 1 or set(manifest["domains"]) != {"account", "token"}:
        raise RuntimeError("published history manifest is invalid")
    for domain, expected in (("account", 87), ("token", 34)):
        entries = manifest["domains"][domain]
        if len(entries) != expected or len({item["file"] for item in entries}) != expected:
            raise RuntimeError("published history is incomplete")
        directory = output / domain
        directory.mkdir(parents=True)
        for item in entries:
            name = item["file"]
            if Path(name).name != name or not name.endswith(".up.sql"):
                raise RuntimeError("published migration filename is invalid")
            source = root / "account-platform/migrations" / domain / name
            if (domain == "account" and name.startswith("000003_")) or (domain == "token" and name.startswith("000018_")):
                source = root / "account-platform/internal" / (domain + "schema") / "retained" / name
            body = source.read_bytes().replace(b"\r\n", b"\n")
            if hashlib.sha256(body).hexdigest() != item["sha256"]:
                raise RuntimeError("published migration bytes do not match the frozen source")
            (directory / name).write_bytes(body)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--postgres-version", choices=sorted(POSTGRES_PROFILES), default="17")
    args = parser.parse_args()
    postgres_image = POSTGRES_PROFILES[args.postgres_version]
    postgres_versions = {}
    if sys.platform != "linux" or os.environ.get("DOCKER_HOST"):
        raise RuntimeError("local Linux Docker engine is required")
    context = subprocess.check_output(["docker", "context", "show"], text=True).strip()
    endpoint = json.loads(subprocess.check_output(["docker", "context", "inspect", context]))[0]["Endpoints"]["docker"]["Host"]
    if not endpoint.startswith("unix://"):
        raise RuntimeError("remote Docker context is not supported")
    artifacts = args.artifacts.resolve()
    artifacts.mkdir(parents=True, exist_ok=False)
    artifacts.chmod(0o700)
    identity = uuid.uuid4().hex
    image = "pinvou-deployment-contracts:" + identity
    owned = []
    verify_results = runpy.run_path(str(ROOT / "scripts/test-account-closure-postgres.py"))["verify_results"]

    def command(arguments, logfile=None):
        if logfile:
            if logfile == "dependencies.log":
                prepare_dependencies(arguments, artifacts / logfile)
            else:
                run_logged_command(arguments, artifacts / logfile)
        else:
            return subprocess.check_output(arguments, text=True).strip()

    with tempfile.TemporaryDirectory(prefix="pinvou-deployment-contracts-") as temporary:
        work = Path(temporary)
        source = work / "source"
        source.mkdir()
        # Verification runs on an isolated copy; npm/build/test output cannot
        # mutate the developer's checkout. Reviewed source identity is sealed.
        for name in ("account-platform", "scripts"):
            shutil.copytree(ROOT / name, source / name, ignore=shutil.ignore_patterns("node_modules", ".git", "__pycache__", ".env", "site.env"))
        hashes = {}
        for path in sorted(source.rglob("*")):
            if path.is_symlink():
                raise RuntimeError("source symlinks are not accepted")
            if path.is_file():
                hashes[str(path.relative_to(source))] = hashlib.sha256(path.read_bytes()).hexdigest()
        (artifacts / "source-hashes.json").write_text(json.dumps(hashes, sort_keys=True, indent=2) + "\n")
        legacy = work / "legacy"
        published_history(source, legacy)
        fixture = work / "tls"
        fixture.mkdir(mode=0o755)
        fixture.chmod(0o755)
        output = work / "output"
        output.mkdir()
        (output / "bin").mkdir()
        caches = []
        for suffix, variable in (("gomod", "PINVOU_DEPLOYMENT_TEST_MODULE_CACHE"), ("gobuild", "PINVOU_DEPLOYMENT_TEST_BUILD_CACHE")):
            path = Path(os.environ.get(variable, str(work / suffix))).resolve()
            path.mkdir(parents=True, exist_ok=True)
            caches += ["--mount", f"type=bind,src={path},dst=/{suffix}"]
        common = ["docker", "run", "--rm", "--cpus", "2", "--memory", "2g",
                  "--mount", f"type=bind,src={source},dst=/src",
                  "--mount", f"type=bind,src={output},dst=/out",
                  "--mount", f"type=bind,src={fixture},dst=/fixture",
                  "--mount", f"type=bind,src={legacy},dst=/legacy,readonly",
                  *caches, "-e", "GOMODCACHE=/gomod", "-e", "GOCACHE=/gobuild",
                  "-e", "GOTOOLCHAIN=local", "-e", "GOMAXPROCS=2", "-w", "/src/account-platform"]

        def tools(arguments, logfile, network="none", environment=(), download_cache=False):
            mounts, download_environment = dependency_cache_options(os.environ) if download_cache else ([], [])
            variables = [*download_environment, *environment] if download_cache else environment
            user = ["--user", f"{os.getuid()}:{os.getgid()}", "-e", "HOME=/tmp/pinvou-deployment-deps"] if download_cache else []
            command(common + mounts + user + ["--network", network] + [part for item in variables for part in ("-e", item)] + [image] + arguments, logfile)

        def postgres(kind, user, database, data, tls=False):
            name = "pinvou-contract-" + kind + "-" + identity
            pg = ["docker", "run", "-d", "--name", name, "--label", f"{LABEL}={identity}",
                  "--network", "none", "--tmpfs", "/var/lib/postgresql/data:rw,nosuid,size=512m",
                  "--tmpfs", "/tmp:rw,nosuid,size=512m", "-e", "POSTGRES_USER=" + user,
                  "-e", "POSTGRES_DB=" + database, "-e", "PGDATA=" + data,
                  "-e", "POSTGRES_HOST_AUTH_METHOD=trust"]
            if tls:
                pg += ["--mount", f"type=bind,src={fixture},dst=/fixture,readonly"]
            pg += [postgres_image, "-c", "listen_addresses=127.0.0.1"]
            if tls:
                pg += ["-c", "ssl=on", "-c", "ssl_cert_file=/fixture/ca.crt", "-c", "ssl_key_file=/fixture/server.key", "-c", "hba_file=/fixture/pg_hba.conf", "-c", "allow_system_table_mods=on"]
            command(pg)
            owned.append(name)
            deadline = time.monotonic() + 40
            while subprocess.run(["docker", "exec", name, "pg_isready", "-h", "127.0.0.1", "-U", user, "-d", database], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
                if time.monotonic() > deadline:
                    raise RuntimeError("owned PostgreSQL fixture did not start")
                time.sleep(0.1)
            version = int(command(["docker", "exec", name, "psql", "-XAt", "-h", "127.0.0.1", "-U", user, "-d", database, "-c", "SHOW server_version_num"]))
            if (args.postgres_version == "18.4" and version != 180004) or (args.postgres_version == "17" and version // 10000 != 17):
                raise RuntimeError("owned PostgreSQL version does not match the selected pinned profile")
            postgres_versions[kind] = version
            return name

        try:
            empty = work / "image-context"
            empty.mkdir()
            command(["docker", "build", "-t", image, "-f", str(ROOT / "account-platform/test/Dockerfile.deployment-control"), str(empty)], "tools-build.log")
            ensure_postgres_image(postgres_image, artifacts / "postgres-image.log")
            warm_runner_go_cache(source / "account-platform", artifacts / "runner-go-download.log")
            tools(["sh", "-c", "go version && node --version && go mod download && go mod verify && npm ci --ignore-scripts"], "dependencies.log", "bridge", download_cache=True)
            tools(["go", "build", "-trimpath", "-tags=nomsgpack", "-mod=readonly", "-o", "/out/bin/token-governance", "./cmd/token-governance"], "cli-build.log")
            tools(["sh", "-c", "openssl req -x509 -newkey rsa:2048 -nodes -keyout /fixture/server.key -out /fixture/ca.crt -days 2 -subj /CN=localhost -addext subjectAltName=IP:127.0.0.1,DNS:localhost && chown 70:70 /fixture/server.key && chmod 600 /fixture/server.key && chmod 644 /fixture/ca.crt"], "tls-fixture.log")
            (fixture / "pg_hba.conf").write_text("local all all trust\nhostssl all pinvou_control_fixture,token_migrator,account_policy_fixture,account_migrator,managed_dba_fixture 127.0.0.1/32 trust\nhostssl all all 127.0.0.1/32 scram-sha-256\nhost all all 0.0.0.0/0 reject\n")
            (fixture / "pg_hba.conf").chmod(0o644)
            managed = postgres("managed", "pinvou_control_fixture", "managed_control_fixture", "/var/lib/postgresql/data/managed_control_fixture", tls=True)
            roots = {"TestManagedRDSReleaseActualPostgres", "TestManagedRDSReleaseProductionHistoryPostgres", "TestManagedBindingRejectsBeforeAnyNetworkAuthentication", "TestManagedReleaseInputsBindProviderAndPrivateCredentials"}
            selection = "^(" + "|".join(sorted(roots)) + ")$"
            tools(["go", "test", "-p", "1", "-race", "-json", "-tags=nomsgpack", "-mod=readonly", "-count=1", "-run", selection, "./cmd/token-release-prepare"], "managed.jsonl", "container:" + managed,
                  ["PINVOU_TEST_MANAGED_POSTGRES_URL=postgres://pinvou_control_fixture@127.0.0.1:5432/managed_control_fixture?sslmode=verify-full&sslrootcert=/fixture/ca.crt", "PINVOU_TEST_LEGACY_MIGRATIONS_DIRECTORY=/legacy", "PINVOU_TEST_RELEASE_CLI_DIRECTORY=/out/bin"])
            verify_results((artifacts / "managed.jsonl").read_text(), roots)
            history_data = "/tmp/pinvou-f17-audit-" + str(uuid.uuid4()) + "/data"
            history = postgres("history", "pinvou_audit_fixture", "postgres", history_data)
            roots = {"TestProductionAccountEventDeliveryUpgrade", "TestProductionTokenReplayAuditHistoryUpgrade"}
            tools(["go", "test", "-p", "1", "-race", "-json", "-tags=nomsgpack", "-mod=readonly", "-count=1", "-run", "^(" + "|".join(sorted(roots)) + ")$", "./internal/accountschema", "./internal/tokenschema"], "history.jsonl", "container:" + history,
                  ["F17_TEST_DATABASE_URL=postgres://pinvou_audit_fixture@127.0.0.1:5432/postgres?sslmode=disable", "F17_TEST_DATABASE_PROFILE=local-owned", "F17_TEST_OWNED_PORT=5432", "F17_TEST_OWNED_DATA_DIRECTORY=" + history_data, "PINVOU_TEST_TOKEN_DATABASE_URL=postgres://pinvou_audit_fixture@127.0.0.1:5432/postgres?sslmode=disable", "PINVOU_TEST_LEGACY_MIGRATIONS_DIRECTORY=/legacy"])
            verify_results((artifacts / "history.jsonl").read_text(), roots)
            receipt = {"status": "ISOLATED_CONTRACTS_PASSED_NOT_DEPLOYED", "tools_image": command(["docker", "image", "inspect", image, "--format", "{{.Id}}"]), "postgres_image": command(["docker", "image", "inspect", postgres_image, "--format", "{{.Id}}"]), "postgres_profile": args.postgres_version, "postgres_server_versions": postgres_versions, "governance_cli_sha256": hashlib.sha256((output / "bin/token-governance").read_bytes()).hexdigest(), "source_manifest_sha256": hashlib.sha256((artifacts / "source-hashes.json").read_bytes()).hexdigest(), "managed_log_sha256": hashlib.sha256((artifacts / "managed.jsonl").read_bytes()).hexdigest(), "history_log_sha256": hashlib.sha256((artifacts / "history.jsonl").read_bytes()).hexdigest()}
            (artifacts / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
            print("ISOLATED_CONTRACTS_PASSED_NOT_DEPLOYED")
        finally:
            for name in reversed(owned):
                actual = command(["docker", "inspect", name, "--format", '{{index .Config.Labels "' + LABEL + '"}}'])
                if actual != identity:
                    raise RuntimeError("refusing cleanup of a container outside this invocation")
                command(["docker", "rm", "-f", "-v", name])
            subprocess.run(["docker", "image", "rm", image], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    main()

"""One-time fixed four-server projection; preserves sources and never starts services."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import subprocess


def module(name, file):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).resolve().parent / file)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


CHECK = module("dependency_bundle_check", "account-platform-dependency-bundle-check.py")
PASSWORD = module("dependency_passwords", "account-platform-dependency-passwords.py")
POSTGRES_BOOTSTRAP_PASSWORD_NAMES = (
    "ACCOUNT_DB_PASSWORD", "BILLING_DB_PASSWORD", "TOKEN_DB_PASSWORD",
    "ACCOUNT_MIGRATION_DB_PASSWORD", "BILLING_MIGRATION_DB_PASSWORD",
    "ACCOUNT_APPROVAL_WRITER_DB_PASSWORD", "ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD", "ACCOUNT_APPROVAL_RELAY_DB_PASSWORD",
    "TOKEN_MIGRATION_DB_PASSWORD", "TOKEN_POLICY_DB_PASSWORD",
    "NEWAPI_DB_PASSWORD", "KEYCLOAK_DB_PASSWORD",
)
TEMPLATES = {"redis-accounts.acl.template": "0075ae0ab0aeede003fc0ceb7df0646b8f0fe5d1873801dbffc620734df2ee23",
             "redis-newapi.acl.template": "289a73ac98038bc0c3ecaefe6a0fb629f700db86da60c258c8ca3f739b98cc2f"}


def require(ok):
    if not ok:
        raise ValueError("DEPENDENCY_PROJECTION_FAILED_PRESERVE_OUTPUT")


def snapshot(root, trusted):
    raw = CHECK.read(root / "manifest.json")
    require(CHECK.sha(raw) == trusted)
    manifest = json.loads(raw, object_pairs_hook=CHECK.closed_object)
    files = {name: CHECK.read(root / name) for name in manifest["files"]}
    require(all(CHECK.sha(body) == manifest["files"][name] for name, body in files.items()))
    return files


def file_matches(path, body, uid):
    info = path.lstat()
    require(path.resolve(strict=True) == path and stat.S_ISREG(info.st_mode) and info.st_nlink == 1
            and (info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)) == (uid, uid, 0o600))
    with path.open("rb") as file:
        opened = os.fstat(file.fileno())
        require((opened.st_dev, opened.st_ino, opened.st_uid, opened.st_gid, opened.st_mode, opened.st_nlink)
                == (info.st_dev, info.st_ino, uid, uid, info.st_mode, 1))
        require(file.read(len(body) + 1) == body)


def write_file(path, body, uid):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as file:
        file.write(body)
        os.fchown(file.fileno(), uid, uid)
        file.flush()
        os.fsync(file.fileno())
    file_matches(path, body, uid)
    PASSWORD.sync_directory(path.parent)


def postgres_bootstrap_files(passwords):
    require(set(POSTGRES_BOOTSTRAP_PASSWORD_NAMES) < set(passwords))
    return {"postgres/bootstrap/" + name: (passwords[name], 999)
            for name in POSTGRES_BOOTSTRAP_PASSWORD_NAMES}


def project(config_file):
    require(os.name == "posix" and os.geteuid() == 0)
    config = json.loads(CHECK.read(config_file), object_pairs_hook=CHECK.closed_object)
    fields = {"material_directory", "output_directory", "template_directory", "recipient_sha256",
              "pki_manifest_sha256", "nats_manifest_sha256", "password_manifest_sha256", "nats_acl_sha256"}
    require(type(config) is dict and set(config) == fields and all(type(v) is str and v for v in config.values()))
    require(all(re.fullmatch("[0-9a-f]{64}", v) for k, v in config.items() if k.endswith("_sha256")))
    source, output, templates = (Path(config[k]) for k in ("material_directory", "output_directory", "template_directory"))
    CHECK.private(source, True)
    require(output.is_absolute() and output.parent == source.parent and output != source)
    parent = output.parent.lstat()
    require(output.parent.resolve(strict=True) == output.parent and stat.S_ISDIR(parent.st_mode)
            and parent.st_uid == 0 and not parent.st_mode & 0o022)
    actual = CHECK.verify(source / "pki", source / "nats", config["recipient_sha256"])
    require(actual["pki_manifest_sha256"] == config["pki_manifest_sha256"]
            and actual["nats_manifest_sha256"] == config["nats_manifest_sha256"])
    digest = CHECK.manifest(source / "passwords", config["recipient_sha256"], 1, PASSWORD.PURPOSE,
                            set(PASSWORD.NAMES) | {"passwords.age"}, set(), "DEPENDENCY_PASSWORDS_READY_V1")
    require(digest == config["password_manifest_sha256"])
    pki = snapshot(source / "pki", config["pki_manifest_sha256"])
    # Validate all NKey files but never project any seed into dependency servers.
    snapshot(source / "nats", config["nats_manifest_sha256"])
    passwords = snapshot(source / "passwords", config["password_manifest_sha256"])
    require(all(re.fullmatch(rb"[0-9a-f]{64}", passwords[name]) for name in PASSWORD.NAMES)
            and len({passwords[name] for name in PASSWORD.NAMES}) == len(PASSWORD.NAMES))
    nats_acl = CHECK.read(source / "nats-authorization.conf")
    require(CHECK.sha(nats_acl) == config["nats_acl_sha256"])
    template = {}
    for name, expected in TEMPLATES.items():
        body = PASSWORD.read_file(templates / name, 8192).replace(b"\r\n", b"\n")
        require(CHECK.sha(body) == expected)
        template[name] = body
    accounts_acl = template["redis-accounts.acl.template"]
    for user in ("PORTAL", "IDENTITY"):
        accounts_acl = accounts_acl.replace((user + "_PASSWORD_SHA256").encode(), CHECK.sha(passwords[user + "_REDIS_PASSWORD"]).encode())
    newapi_acl = template["redis-newapi.acl.template"].replace(b"NEWAPI_PASSWORD_SHA256", CHECK.sha(passwords["NEWAPI_REDIS_PASSWORD"]).encode())
    files, directories = {}, {}
    for service in ("postgres", "redis", "newapi-redis", "nats"):
        uid = 999 if service == "postgres" else 65532
        directories[service] = 0
        directories[service + "/data"] = uid
        directories[service + "/tls"] = uid
        for suffix in ("crt", "key"):
            files[f"{service}/tls/server.{suffix}"] = (pki[f"{service}/server.{suffix}"], uid)
    directories.update({"postgres/admin": 999, "postgres/bootstrap": 999, "postgres/trust": 999, "trust": 65532})
    files["postgres/admin/password"] = (passwords["POSTGRES_ADMIN_PASSWORD"], 999)
    files.update(postgres_bootstrap_files(passwords))
    for service, acl in (("redis", accounts_acl), ("newapi-redis", newapi_acl), ("nats", nats_acl)):
        directories[service + "/authorization"] = 65532
        files[service + "/authorization/" + ("authorization.conf" if service == "nats" else "users.acl")] = (acl, 65532)
    files["trust/ca.crt"] = (pki["ca.crt"], 65532)
    files["postgres/trust/ca.crt"] = (pki["ca.crt"], 999)
    # All source bytes and the exact mapping are now frozen. No source reread below.
    output.mkdir(mode=0o700)
    PASSWORD.sync_directory(output.parent)
    CHECK.private(output, True)
    for name, uid in directories.items():
        path = output / name
        path.mkdir(mode=0o700)
        os.chown(path, uid, uid)
        PASSWORD.sync_directory(path.parent)
    for name, (body, uid) in files.items():
        write_file(output / name, body, uid)
    for name, uid in directories.items():
        info = (output / name).lstat()
        require(stat.S_ISDIR(info.st_mode) and (info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)) == (uid, uid, 0o700))
    require({p.relative_to(output).as_posix() for p in output.rglob("*")} == set(files) | set(directories))
    manifest = {"version": 1, "purpose": "pinvou-four-dependency-projection", "directories": directories,
                "files": {name: {"sha256": CHECK.sha(body), "uid": uid, "gid": uid} for name, (body, uid) in files.items()}}
    raw = json.dumps(manifest, sort_keys=True).encode()
    write_file(output / "manifest.json", raw, 0)
    digest = CHECK.sha(raw)
    write_file(output / "READY", ("DEPENDENCY_PROJECTION_READY_V1\n" + digest + "\n").encode(), 0)
    return digest


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True, type=Path)
    args = parser.parse_args()
    try:
        print("DEPENDENCY_PROJECTION_READY_MANIFEST_SHA256=" + project(args.config))
    except (OSError, ValueError, KeyError, TypeError, IndexError, subprocess.SubprocessError):
        raise SystemExit("DEPENDENCY_PROJECTION_FAILED_PRESERVE_OUTPUT") from None

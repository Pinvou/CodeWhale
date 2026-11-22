"""Prepare the fixed sixteen first-install secrets; never modifies running services."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import stat
import subprocess

NAMES = ("POSTGRES_ADMIN_PASSWORD", "ACCOUNT_DB_PASSWORD", "BILLING_DB_PASSWORD", "TOKEN_DB_PASSWORD", "ACCOUNT_MIGRATION_DB_PASSWORD",
         "ACCOUNT_APPROVAL_WRITER_DB_PASSWORD", "ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD", "ACCOUNT_APPROVAL_RELAY_DB_PASSWORD",
         "BILLING_MIGRATION_DB_PASSWORD", "TOKEN_MIGRATION_DB_PASSWORD", "TOKEN_POLICY_DB_PASSWORD",
         "NEWAPI_DB_PASSWORD", "KEYCLOAK_DB_PASSWORD", "PORTAL_REDIS_PASSWORD", "IDENTITY_REDIS_PASSWORD", "NEWAPI_REDIS_PASSWORD")
PURPOSE = "pinvou-production-dependency-passwords"


def require(ok):
    if not ok:
        raise ValueError("DEPENDENCY_PASSWORD_PREPARATION_FAILED_PRESERVE_OUTPUT")


def sha(data):
    return hashlib.sha256(data).hexdigest()


def closed_object(pairs):
    obj = {}
    for key, value in pairs:
        require(key not in obj)
        obj[key] = value
    return obj


def private_directory(path):
    info = path.lstat()
    require(path.is_absolute() and path.resolve(strict=True) == path and stat.S_ISDIR(info.st_mode)
            and info.st_uid == os.geteuid() and stat.S_IMODE(info.st_mode) == 0o700)


def read_file(path, limit, private=False):
    info = path.lstat()
    require(path.is_absolute() and path.resolve(strict=True) == path and stat.S_ISREG(info.st_mode)
            and not info.st_mode & 0o022 and info.st_nlink == 1 and 0 < info.st_size <= limit)
    if private:
        require(info.st_uid == os.geteuid() and stat.S_IMODE(info.st_mode) == 0o600)
    with path.open("rb") as file:
        opened = os.fstat(file.fileno())
        require((info.st_dev, info.st_ino) == (opened.st_dev, opened.st_ino)
                and stat.S_ISREG(opened.st_mode) and not opened.st_mode & 0o022 and opened.st_nlink == 1)
        if private:
            require(opened.st_uid == os.geteuid() and stat.S_IMODE(opened.st_mode) == 0o600)
        result = file.read(limit + 1)
    require(0 < len(result) <= limit)
    return result


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_new(path, data):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as file:
        file.write(data)
        file.flush()
        os.fsync(file.fileno())
    require(read_file(path, 1 << 20, private=True) == data)
    sync_directory(path.parent)


def generate_password_values():
    values = {name: secrets.token_hex(32) for name in NAMES}
    require(len(values) == len(NAMES) and len(set(values.values())) == len(NAMES))
    return values


def prepare(config_file):
    require(os.name == "posix")
    config = json.loads(read_file(config_file, 8192, private=True), object_pairs_hook=closed_object)
    require(type(config) is dict and set(config) == {"recipient_file", "recipient_sha256", "age_binary", "age_sha256", "output_directory"})
    require(all(type(value) is str and value for value in config.values()))
    require(re.fullmatch("[0-9a-f]{64}", config["recipient_sha256"]) and re.fullmatch("[0-9a-f]{64}", config["age_sha256"]))
    recipient = read_file(Path(config["recipient_file"]), 4096).strip()
    require(sha(recipient) == config["recipient_sha256"] and re.fullmatch(rb"age1[0-9a-z]{58}", recipient))
    binary = Path(config["age_binary"])
    require(sha(read_file(binary, 32 << 20)) == config["age_sha256"] and binary.stat().st_uid == os.geteuid() and os.access(binary, os.X_OK))
    directory = Path(config["output_directory"])
    require(directory.is_absolute())
    private_directory(directory.parent)
    # Exclusive: failures leave the original attempt intact, never regenerate over it.
    directory.mkdir(mode=0o700)
    sync_directory(directory.parent)
    private_directory(directory)
    values = generate_password_values()
    envelope = {"version": 1, "purpose": PURPOSE, "recipient_sha256": config["recipient_sha256"], "passwords": values}
    plain = json.dumps(envelope, sort_keys=True).encode()
    # The recipient is public and fingerprint checked. Passing this verified value
    # avoids a second read of a mutable recipient path by the external command.
    result = subprocess.run([str(binary), "-r", recipient.decode()], input=plain, capture_output=True, timeout=30)
    require(result.returncode == 0 and result.stdout.startswith(b"age-encryption.org/v1\n") and 0 < len(result.stdout) < 1 << 20)
    write_new(directory / "passwords.age", result.stdout)
    files = {"passwords.age": sha(result.stdout)}
    for name, value in values.items():
        data = value.encode()
        write_new(directory / name, data)
        files[name] = sha(data)
    manifest = {"version": 1, "purpose": PURPOSE, "recipient_sha256": config["recipient_sha256"], "files": files}
    raw = json.dumps(manifest, sort_keys=True).encode()
    write_new(directory / "manifest.json", raw)
    digest = sha(raw)
    write_new(directory / "READY", ("DEPENDENCY_PASSWORDS_READY_V1\n" + digest + "\n").encode())
    return digest


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True, type=Path)
    args = parser.parse_args()
    try:
        print("DEPENDENCY_PASSWORDS_READY_MANIFEST_SHA256=" + prepare(args.config))
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        raise SystemExit("DEPENDENCY_PASSWORD_PREPARATION_FAILED_PRESERVE_OUTPUT") from None

"""Read-only check of the fixed infrastructure PKI v2 and NATS identity bundles.

Never decrypts custody packages, writes files, starts services, or reads KMS keys.
Prints only independent manifest hashes after all checks pass.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess

SERVICES = ("postgres", "redis", "nats", "keycloak", "newapi", "newapi-redis")
ROLES = ("account-outbox-relay", "billing-outbox-relay", "token-outbox-relay",
         "billing-command-executor", "token-command-executor", "event-resource-operator")


def require(ok):
    if not ok:
        raise ValueError("DEPENDENCY_BUNDLE_CHECK_FAILED")


def closed_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result)
        result[key] = value
    return result


def private(path, directory=False):
    info = path.lstat()
    require(path.is_absolute() and path.resolve(strict=True) == path)
    require(info.st_uid == os.geteuid() and stat.S_IMODE(info.st_mode) == (0o700 if directory else 0o600))
    require(stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode))
    if not directory:
        require(info.st_nlink == 1 and 0 < info.st_size <= 1 << 20)


def read(path):
    private(path)
    before = path.lstat()
    with path.open("rb") as file:
        opened = os.fstat(file.fileno())
        require((before.st_dev, before.st_ino) == (opened.st_dev, opened.st_ino))
        require(stat.S_ISREG(opened.st_mode) and stat.S_IMODE(opened.st_mode) == 0o600 and opened.st_uid == os.geteuid() and opened.st_nlink == 1)
        data = file.read((1 << 20) + 1)
    require(0 < len(data) <= 1 << 20)
    return data


def sha(data):
    return hashlib.sha256(data).hexdigest()


def manifest(root, recipient, version, purpose, expected, directories, marker):
    private(root, True)
    for name in directories:
        private(root / name, True)
    actual = set()
    for path in root.rglob("*"):
        relative = path.relative_to(root).as_posix()
        if relative in directories:
            private(path, True)
        else:
            private(path)
            actual.add(relative)
    require(actual == expected | {"manifest.json", "READY"})
    raw = read(root / "manifest.json")
    value = json.loads(raw, object_pairs_hook=closed_object)
    require(set(value) == {"version", "purpose", "recipient_sha256", "files"})
    require(type(value["version"]) is int and value["version"] == version and value["purpose"] == purpose and value["recipient_sha256"] == recipient)
    require(type(value["files"]) is dict and set(value["files"]) == expected)
    for name, digest in value["files"].items():
        require(type(digest) is str and re.fullmatch("[0-9a-f]{64}", digest) and sha(read(root / name)) == digest)
    digest = sha(raw)
    require(read(root / "READY") == (marker + "\n" + digest + "\n").encode())
    return digest


def openssl(*args):
    process = subprocess.run(["openssl", *map(str, args)], capture_output=True, timeout=10)
    require(process.returncode == 0)
    return process.stdout


def single_certificate(path):
    require(re.fullmatch(rb"\s*-----BEGIN CERTIFICATE-----\n[A-Za-z0-9+/=\r\n]+-----END CERTIFICATE-----\s*", read(path)))


def verify(pki, nats, recipient):
    require(os.name == "posix" and re.fullmatch("[0-9a-f]{64}", recipient))
    require(pki != nats and not pki.is_relative_to(nats) and not nats.is_relative_to(pki))
    files = {"ca-private.age", "ca.crt"} | {f"{s}/server.{suffix}" for s in SERVICES for suffix in ("crt", "key")}
    pki_hash = manifest(pki, recipient, 2, "pinvou-platform-internal-pki", files, set(SERVICES), "INFRASTRUCTURE_PKI_READY_V2")
    nats_hash = manifest(nats, recipient, 1, "pinvou-nats-service-identities", {"identities.age", "public-keys.json"} | {f"{role}/nkey.seed" for role in ROLES}, set(ROLES), "NATS_IDENTITIES_READY_V1")
    require(read(pki / "ca-private.age").startswith(b"age-encryption.org/v1\n") and read(nats / "identities.age").startswith(b"age-encryption.org/v1\n"))
    ca = pki / "ca.crt"
    single_certificate(ca)
    openssl("verify", "-no-CApath", "-no-CAstore", "-check_ss_sig", "-CAfile", ca, ca)
    require(b"CA:TRUE, pathlen:0" in openssl("x509", "-in", ca, "-noout", "-ext", "basicConstraints"))
    require(b"Certificate Sign" in openssl("x509", "-in", ca, "-noout", "-ext", "keyUsage"))
    openssl("x509", "-in", ca, "-noout", "-checkend", "1209600")
    keys = {openssl("x509", "-in", ca, "-pubkey", "-noout")}
    for service in SERVICES:
        cert, key = pki / service / "server.crt", pki / service / "server.key"
        single_certificate(cert)
        openssl("verify", "-no-CApath", "-no-CAstore", "-CAfile", ca, "-purpose", "sslserver", "-verify_hostname", service, cert)
        require(openssl("x509", "-in", cert, "-noout", "-ext", "basicConstraints").decode().splitlines()[1].strip() == "CA:FALSE")
        openssl("x509", "-in", cert, "-noout", "-checkend", "1209600")
        require(openssl("x509", "-in", cert, "-noout", "-ext", "subjectAltName").decode().splitlines()[1].strip() == "DNS:" + service)
        require(openssl("x509", "-in", cert, "-noout", "-ext", "extendedKeyUsage").decode().splitlines()[1].strip() == "TLS Web Server Authentication")
        public = openssl("x509", "-in", cert, "-pubkey", "-noout")
        require(public == openssl("pkey", "-in", key, "-pubout") and public not in keys)
        keys.add(public)
    public = json.loads(read(nats / "public-keys.json"), object_pairs_hook=closed_object)
    require(set(public) == {"version", "purpose", "public_keys"} and type(public["version"]) is int and public["version"] == 1 and public["purpose"] == "pinvou-nats-service-identities")
    require(type(public["public_keys"]) is dict and set(public["public_keys"]) == set(ROLES))
    require(all(type(v) is str and re.fullmatch("U[A-Z2-7]{55}", v) for v in public["public_keys"].values()) and len(set(public["public_keys"].values())) == len(ROLES))
    # Official key decoding/CRC checks and authorization still belong to the
    # existing fixed event-resources renderer; the generator proved seed mapping.
    return {"pki_manifest_sha256": pki_hash, "nats_manifest_sha256": nats_hash,
            "nats_public_keys_sha256": sha(read(nats / "public-keys.json"))}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pki", required=True, type=Path)
    parser.add_argument("--nats", required=True, type=Path)
    parser.add_argument("--recipient-sha256", required=True)
    args = parser.parse_args()
    try:
        print(json.dumps(verify(args.pki, args.nats, args.recipient_sha256), sort_keys=True))
    except (OSError, ValueError, KeyError, TypeError, IndexError, subprocess.SubprocessError):
        raise SystemExit("DEPENDENCY_BUNDLE_CHECK_FAILED") from None

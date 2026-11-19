#!/usr/bin/env python3
"""Publish/fetch frontend bundles with tested-commit and SHA-256 verification."""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parent / 'lib'))
from ci_transfer_deadline import DeadlineExceeded, command_deadline  # noqa: E402

MAX_BLOB = 128 * 1024**2


def checksum(path):
    value = hashlib.sha256()
    with open(path, "rb") as stream:
        for block in iter(lambda: stream.read(256 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def validate_identity(commit, digest):
    if not re.fullmatch(r"[0-9a-f]{40}", commit or "") or not re.fullmatch(r"[0-9a-f]{64}", digest or ""):
        raise ValueError("invalid bundle identity")


def tested_commit():
    return subprocess.check_output(["git", "rev-parse", "HEAD"], stderr=subprocess.PIPE).decode().strip()


def cache_url():
    value = os.environ.get("PINVOU_FRONTEND_CACHE_URL", "").rstrip("/")
    if value:
        parsed = urllib.parse.urlsplit(value)
        if (parsed.scheme not in ("http", "https") or parsed.hostname not in ("127.0.0.1", "localhost", "::1")
                or parsed.username or parsed.path or parsed.query or parsed.fragment):
            raise ValueError("host cache must use a loopback endpoint")
    return value


def repository():
    value = os.environ.get("GITHUB_REPOSITORY", "")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", value):
        raise ValueError("invalid repository identity")
    return value


def event(phase, **values):
    print(json.dumps({"phase": phase, **values}, sort_keys=True), flush=True)


def publish(path):
    start = time.monotonic()
    commit = tested_commit()
    digest = checksum(path)
    validate_identity(commit, digest)
    size = path.stat().st_size
    if not 0 < size <= MAX_BLOB:
        raise ValueError("invalid bundle size")
    transport = "github"
    replicas = 0
    endpoint = cache_url()
    if endpoint:
        request = urllib.request.Request(endpoint + "/v1/" + commit + "/" + digest,
                                         headers={"X-Repository": repository(), "Content-Length": str(size)},
                                         method="PUT")
        client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with command_deadline(time.monotonic() + 65), path.open("rb") as stream:
                request.data = stream
                # Request.data clears a previously supplied content length.
                # Restore it so file-backed uploads never switch to chunked.
                request.add_header("Content-Length", str(size))
                with client.open(request, timeout=65) as response:
                    receipt = json.load(response)
            if receipt.get("sha256") != digest or receipt.get("commit") != commit:
                raise ValueError("cache publication receipt identity mismatch")
            replicas = int(receipt.get("replicas", 0))
            if replicas < 2:
                raise OSError("cache redundancy unavailable")
            transport = "lan"
        except (OSError, ValueError, urllib.error.URLError, DeadlineExceeded) as error:
            # Fallback changes only delivery, never the tested bytes or gates.
            event("cache-publication-fallback", error=type(error).__name__, http_status=getattr(error, "code", None))
            if isinstance(error, urllib.error.HTTPError):
                error.close()
    output = os.environ.get("GITHUB_OUTPUT")
    if output:
        with open(output, "a", encoding="utf-8") as stream:
            for key, value in {"commit": commit, "sha256": digest, "size": size, "transport": transport}.items():
                stream.write(f"{key}={value}\n")
    event("bundle-published", transport=transport, commit=commit, sha256=digest,
          bytes=size, replicas=replicas, seconds=round(time.monotonic() - start, 3))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def github_zip(destination, deadline):
    with command_deadline(deadline):
        return _github_zip(destination, deadline)


def _github_zip(destination, deadline):
    token = os.environ.get("GITHUB_TOKEN", "")
    run = os.environ.get("GITHUB_RUN_ID", "")
    api = os.environ.get("GITHUB_API_URL", "https://api.github.com").rstrip("/")
    parsed = urllib.parse.urlsplit(api)
    if not token or not run.isdigit() or parsed.scheme != "https" or parsed.username or parsed.query or parsed.fragment:
        raise ValueError("invalid GitHub artifact context")
    client = urllib.request.build_opener(NoRedirect)
    headers = {"Authorization": "Bearer " + token, "Accept": "application/vnd.github+json"}
    root = api + "/repos/" + repository() + "/actions/"
    candidates = []
    for poll in range(3):
        if time.monotonic() >= deadline:
            raise DeadlineExceeded('artifact total transfer budget exhausted')
        try:
            with client.open(urllib.request.Request(root + "runs/" + run + "/artifacts?per_page=100", headers=headers), timeout=15) as response:
                rows = json.load(response)["artifacts"]
        except (OSError, urllib.error.URLError) as error:
            if isinstance(error, urllib.error.HTTPError):
                error.close()
            if getattr(error, "code", None) in (401, 403) or poll == 2:
                raise RuntimeError("GitHub artifact metadata unavailable") from None
            event("cloud-metadata-retry", attempt=poll + 1, error=type(error).__name__)
            time.sleep(2)
            continue
        candidates = [row for row in rows if row["name"] == "frontend-setup-bundle" and not row["expired"]]
        if candidates:
            break
        if poll < 2:
            time.sleep(2)
    if len(candidates) != 1:
        raise ValueError("expected exactly one current-run frontend bundle")
    artifact = candidates[0]
    if not 0 < int(artifact["size_in_bytes"]) <= MAX_BLOB:
        raise ValueError("invalid artifact size")
    for attempt in range(3):
        if time.monotonic() >= deadline:
            raise DeadlineExceeded("artifact total transfer budget exhausted")
        offset = destination.stat().st_size if destination.exists() else 0
        try:
            request = urllib.request.Request(root + "artifacts/" + str(int(artifact["id"])) + "/zip", headers=headers)
            try:
                client.open(request, timeout=15)
                raise ValueError("expected artifact redirect")
            except urllib.error.HTTPError as error:
                if error.code != 302:
                    error.close()
                    raise
                location = error.headers["Location"]
                error.close()
            if urllib.parse.urlsplit(location).scheme != "https":
                raise ValueError("artifact redirect must use HTTPS")
            # Authorization is deliberately absent on the signed storage URL.
            request = urllib.request.Request(location, headers={"Range": "bytes=" + str(offset) + "-"} if offset else {})
            with urllib.request.urlopen(request, timeout=15) as response:
                if offset and response.status == 206:
                    expected = "bytes " + str(offset) + "-"
                    if not response.headers.get("Content-Range", "").startswith(expected):
                        raise ValueError("invalid resumed artifact range")
                else:
                    if response.status != 200:
                        raise ValueError("invalid artifact transfer status")
                    offset = 0
                with destination.open("ab" if offset else "wb") as stream:
                    while True:
                        if time.monotonic() >= deadline:
                            raise DeadlineExceeded("artifact total transfer budget exhausted")
                        block = response.read(128 * 1024)
                        if time.monotonic() >= deadline:
                            raise DeadlineExceeded("artifact total transfer budget exhausted")
                        if not block:
                            break
                        stream.write(block)
                        offset += len(block)
                        if offset > MAX_BLOB:
                            raise ValueError("artifact exceeds size cap")
            if offset != artifact["size_in_bytes"]:
                raise OSError("incomplete artifact transfer")
            return
        except (OSError, urllib.error.URLError, http.client.HTTPException) as error:
            event("cloud-transfer-retry", attempt=attempt + 1, received_bytes=offset,
                  error=type(error).__name__)
            if attempt == 2:
                raise RuntimeError("GitHub bundle transfer failed") from None


def fetch(args):
    with command_deadline(time.monotonic() + (90 if args.transport == 'lan' else 180)) as budget:
        return _fetch(args, budget)


def _fetch(args, budget):
    start = time.monotonic()
    validate_identity(args.commit, args.sha256)
    if tested_commit() != args.commit or not 0 < args.size <= MAX_BLOB:
        raise ValueError("bundle does not match this checked-out commit")
    args.output.parent.mkdir(parents=True, exist_ok=True)
    descriptor, name = tempfile.mkstemp(prefix="frontend-download-", dir=args.output.parent)
    os.close(descriptor)
    temporary = Path(name)
    source = "github"
    try:
        if args.transport == "lan":
            endpoint = cache_url()
            if not endpoint:
                raise ValueError("LAN bundle requires its configured host cache")
            client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            request = urllib.request.Request(endpoint + "/v1/" + args.commit + "/" + args.sha256,
                                             headers={"X-Repository": repository()})
            with client.open(request, timeout=65) as response:
                if (response.headers.get("X-Tested-Commit") != args.commit or
                        response.headers.get("X-Content-SHA256") != args.sha256 or
                        int(response.headers["Content-Length"]) != args.size):
                    raise ValueError("cache response identity mismatch")
                source = response.headers.get("X-Cache-Source", "unknown")
                if source not in ("local-hit", "peer-fill"):
                    raise ValueError("unknown cache response source")
                with temporary.open("wb") as output:
                    remaining = args.size
                    while remaining:
                        if time.monotonic() - start >= 90:
                            raise TimeoutError("host cache transfer budget exhausted")
                        block = response.read(min(256 * 1024, remaining))
                        budget.check()
                        if not block:
                            raise ValueError("incomplete cache response")
                        output.write(block)
                        remaining -= len(block)
        else:
            archive = temporary.with_suffix(".zip")
            try:
                github_zip(archive, budget.end)
                with zipfile.ZipFile(archive) as value:
                    members = value.infolist()
                    if len(members) != 1 or members[0].filename != "frontend-setup-bundle.tar.gz" or members[0].file_size != args.size:
                        raise ValueError("unexpected artifact archive contents")
                    with value.open(members[0]) as source_stream, temporary.open("wb") as output:
                        shutil.copyfileobj(source_stream, output, 256 * 1024)
            finally:
                archive.unlink(missing_ok=True)
        if temporary.stat().st_size != args.size or checksum(temporary) != args.sha256:
            raise ValueError("downloaded bundle checksum mismatch")
        budget.check()
        os.replace(temporary, args.output)
        event("bundle-fetched", source=source, commit=args.commit, sha256=args.sha256,
              bytes=args.size, seconds=round(time.monotonic() - start, 3))
    finally:
        temporary.unlink(missing_ok=True)


def unpack(args):
    validate_identity(args.commit, args.sha256)
    if tested_commit() != args.commit or checksum(args.bundle) != args.sha256:
        raise ValueError("unpack identity mismatch")
    workspace = args.workspace.resolve()
    if workspace != Path.cwd().parent.resolve():
        raise ValueError("unpack requires the current frontend workspace")
    roots = ("pinvou3-app/dist", "remote-control-relay/web/dist")
    with tempfile.TemporaryDirectory(prefix="frontend-unpack-", dir=os.environ.get("RUNNER_TEMP")) as temporary:
        stage = Path(temporary)
        with tarfile.open(args.bundle, "r:gz") as archive:
            members = archive.getmembers()
            if len(members) > 20000 or sum(m.size for m in members) > 512 * 1024**2:
                raise ValueError("unpacked bundle exceeds limits")
            for member in members:
                path = PurePosixPath(member.name)
                if (path.is_absolute() or ".." in path.parts or "\\" in member.name or
                        not (member.isfile() or member.isdir()) or
                        not any(member.name.rstrip("/") == root or member.name.startswith(root + "/") for root in roots)):
                    raise ValueError("unsafe frontend bundle member")
            for member in members:
                target = stage.joinpath(*PurePosixPath(member.name).parts)
                if member.isdir():
                    target.mkdir(parents=True, exist_ok=True)
                else:
                    target.parent.mkdir(parents=True, exist_ok=True)
                    with archive.extractfile(member) as source, target.open("xb") as output:
                        shutil.copyfileobj(source, output)
            for root in roots:
                if not (stage / root / "index.html").is_file():
                    raise ValueError("frontend bundle is missing a required entry")
        for root in roots:
            destination = workspace / root
            if any(parent.is_symlink() for parent in (destination, *destination.parents) if parent != workspace.parent):
                raise ValueError("unpack target must not contain links")
            if destination.exists():
                shutil.rmtree(destination)
            shutil.move(str(stage / root), destination)
            for path in destination.rglob("*"):
                os.utime(path, None)
        event("bundle-unpacked", commit=args.commit, sha256=args.sha256)


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    command = commands.add_parser("publish")
    command.add_argument("--bundle", type=Path, required=True)
    command = commands.add_parser("fetch")
    for name in ("commit", "sha256", "transport"):
        command.add_argument("--" + name, required=True, choices=("lan", "github") if name == "transport" else None)
    command.add_argument("--size", required=True, type=int)
    command.add_argument("--output", required=True, type=Path)
    command = commands.add_parser("unpack")
    for name in ("commit", "sha256"):
        command.add_argument("--" + name, required=True)
    command.add_argument("--bundle", required=True, type=Path)
    command.add_argument("--workspace", required=True, type=Path)
    args = parser.parse_args()
    try:
        if args.command == "publish":
            publish(args.bundle)
        elif args.command == "fetch":
            fetch(args)
        else:
            unpack(args)
    except Exception as error:
        event("bundle-error", error=type(error).__name__)
        raise SystemExit(1) from None


if __name__ == "__main__":
    main()

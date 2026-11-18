"""Immutable, bounded storage for one repository's verified frontend bundles."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import threading
import time
import uuid


def identity(commit, digest):
    if not re.fullmatch(r"[0-9a-f]{40}", commit or "") or not re.fullmatch(r"[0-9a-f]{64}", digest or ""):
        raise ValueError("invalid bundle identity")


def sha256_file(path):
    result = hashlib.sha256()
    with open(path, "rb") as stream:
        for block in iter(lambda: stream.read(256 * 1024), b""):
            result.update(block)
    return result.hexdigest()


class Store:
    def __init__(self, root, *, max_bytes=20 * 1024**3, max_entries=1000,
                 max_blob=128 * 1024**2, reserve=2 * 1024**3, ttl=7 * 86400):
        self.root = Path(root)
        if self.root.is_symlink():
            raise ValueError("cache root must not be a link")
        self.root.mkdir(parents=True, exist_ok=True)
        self.objects = self.root / "objects"
        self.refs = self.root / "refs"
        self.temporary = self.root / "temporary"
        for path in (self.objects, self.refs, self.temporary):
            if path.is_symlink():
                raise ValueError("cache directory must not be a link")
            path.mkdir(exist_ok=True)
        self.max_bytes, self.max_entries = max_bytes, max_entries
        self.max_blob, self.reserve, self.ttl = max_blob, reserve, ttl
        self.locks = [threading.Lock() for _ in range(64)]
        self.prune_lock = threading.RLock()

    def paths(self, commit, digest):
        identity(commit, digest)
        return self.objects / digest, self.refs / (commit + "-" + digest + ".json")

    def open(self, commit, digest):
        blob, ref = self.paths(commit, digest)
        with self.locks[int(digest[:4], 16) % len(self.locks)], self.prune_lock:
            if not blob.is_file() or blob.is_symlink() or not ref.is_file() or ref.is_symlink():
                return None
            try:
                metadata = json.loads(ref.read_text())
            except (OSError, ValueError):
                return None
            if metadata != {"commit": commit, "sha256": digest, "size": blob.stat().st_size}:
                return None
            if sha256_file(blob) != digest:
                blob.unlink()
                return None
            stream = blob.open("rb")
            os.utime(ref, None)
            os.utime(blob, None)
            return stream, metadata["size"]

    def put(self, commit, digest, stream, size):
        blob, ref = self.paths(commit, digest)
        if not isinstance(size, int) or not 0 < size <= self.max_blob:
            raise ValueError("invalid bundle size")
        if shutil.disk_usage(self.root).free < self.reserve + size:
            raise OSError("cache disk reserve reached")
        temporary = self.temporary / uuid.uuid4().hex
        checksum = hashlib.sha256()
        deadline = time.monotonic() + 90
        try:
            with temporary.open("xb") as output:
                remaining = size
                while remaining:
                    if time.monotonic() >= deadline:
                        raise TimeoutError("cache upload budget exhausted")
                    block = stream.read(min(256 * 1024, remaining))
                    if not block:
                        raise ValueError("incomplete upload")
                    output.write(block)
                    checksum.update(block)
                    remaining -= len(block)
                output.flush()
                os.fsync(output.fileno())
            if checksum.hexdigest() != digest:
                raise ValueError("bundle checksum mismatch")
            with self.locks[int(digest[:4], 16) % len(self.locks)], self.prune_lock:
                if blob.is_symlink() or ref.is_symlink():
                    raise ValueError("cache entry must not be a link")
                if not blob.exists() or sha256_file(blob) != digest:
                    os.replace(temporary, blob)
                data = {"commit": commit, "sha256": digest, "size": size}
                temporary_ref = self.temporary / uuid.uuid4().hex
                try:
                    temporary_ref.write_text(json.dumps(data, sort_keys=True))
                    os.replace(temporary_ref, ref)
                finally:
                    temporary_ref.unlink(missing_ok=True)
                os.utime(blob, None)
            self.prune(keep=(commit, digest))
            return data
        finally:
            temporary.unlink(missing_ok=True)

    def prune(self, keep=None):
        with self.prune_lock:
            refs = sorted((p for p in self.refs.glob("*.json") if not p.is_symlink()),
                          key=lambda p: p.stat().st_mtime, reverse=True)
            used = 0
            retained = set()
            now = time.time()
            count = 0
            for ref in refs:
                match = re.fullmatch(r"([0-9a-f]{40})-([0-9a-f]{64})\.json", ref.name)
                if not match:
                    continue
                commit, digest = match.groups()
                blob = self.objects / digest
                size = blob.stat().st_size if blob.is_file() else 0
                additional = 0 if digest in retained else size
                wanted = ((commit, digest) == keep or
                          (count < self.max_entries and used + additional <= self.max_bytes and
                           now - ref.stat().st_mtime <= self.ttl))
                if wanted and size:
                    count += 1
                    used += additional
                    retained.add(digest)
                else:
                    ref.unlink(missing_ok=True)
            for blob in self.objects.iterdir():
                if re.fullmatch(r"[0-9a-f]{64}", blob.name) and blob.name not in retained and not blob.is_symlink():
                    blob.unlink(missing_ok=True)

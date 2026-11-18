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
        self.pending_bytes = 0
        # A root belongs to one service process. On startup no upload in this
        # process can own a staging file left by the previous process.
        for path in self.temporary.iterdir():
            if re.fullmatch(r'[0-9a-f]{32}', path.name) and path.is_file() and not path.is_symlink():
                path.unlink()
        self.prune()

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

    def put(self, commit, digest, stream, size, *, deadline=None):
        blob, ref = self.paths(commit, digest)
        if type(size) is not int or not 0 < size <= min(self.max_blob, self.max_bytes):
            raise ValueError("invalid bundle size")
        with self.prune_lock:
            required_free = self.reserve + self.pending_bytes + size
            # Expired/evictable objects must be reclaimed before rejecting the
            # next publication or peer-fill; no successful write is required.
            self.prune(required_free=required_free)
            if shutil.disk_usage(self.root).free < required_free:
                raise OSError("cache disk reserve reached")
            # Charge every admitted upload, including other listener workers.
            # Already-written staging bytes are conservatively charged twice.
            self.pending_bytes += size
        temporary = self.temporary / uuid.uuid4().hex
        checksum = hashlib.sha256()
        deadline = time.monotonic() + 90 if deadline is None else deadline
        try:
            with temporary.open("xb") as output:
                remaining = size
                while remaining:
                    if time.monotonic() >= deadline:
                        raise TimeoutError("cache upload budget exhausted")
                    block = stream.read(min(256 * 1024, remaining))
                    if time.monotonic() >= deadline:
                        raise TimeoutError("cache upload budget exhausted")
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
                if time.monotonic() >= deadline:
                    raise TimeoutError("cache upload budget exhausted")
                if blob.is_symlink() or ref.is_symlink():
                    raise ValueError("cache entry must not be a link")
                valid_existing = blob.exists() and sha256_file(blob) == digest
                if time.monotonic() >= deadline:
                    raise TimeoutError("cache upload budget exhausted")
                if not valid_existing:
                    os.replace(temporary, blob)
                data = {"commit": commit, "sha256": digest, "size": size}
                temporary_ref = self.temporary / uuid.uuid4().hex
                try:
                    temporary_ref.write_text(json.dumps(data, sort_keys=True))
                    if time.monotonic() >= deadline:
                        raise TimeoutError("cache upload budget exhausted")
                    os.replace(temporary_ref, ref)
                finally:
                    temporary_ref.unlink(missing_ok=True)
                os.utime(blob, None)
            self.prune(keep=(commit, digest))
            return data
        finally:
            try:
                temporary.unlink(missing_ok=True)
            finally:
                with self.prune_lock:
                    self.pending_bytes -= size

    def prune(self, keep=None, required_free=0):
        with self.prune_lock:
            refs = sorted((p for p in self.refs.glob("*.json") if not p.is_symlink()),
                          key=lambda p: (bool(keep and p.name == keep[0] + '-' + keep[1] + '.json'),
                                         p.stat().st_mtime), reverse=True)
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
            if required_free:
                # Oldest references go first, including all references to a
                # shared object before deleting its bytes. Only owned cache
                # data is eligible; active staging is never an eviction target.
                remaining = {ref: re.fullmatch(r'([0-9a-f]{40})-([0-9a-f]{64})\.json', ref.name).groups()[1]
                             for ref in refs if ref.exists() and
                             re.fullmatch(r'([0-9a-f]{40})-([0-9a-f]{64})\.json', ref.name)}
                for ref, digest in reversed(list(remaining.items())):
                    if shutil.disk_usage(self.root).free >= required_free:
                        break
                    if keep and ref.name == keep[0] + '-' + keep[1] + '.json':
                        continue
                    ref.unlink(missing_ok=True)
                    del remaining[ref]
                    blob = self.objects / digest
                    if digest not in remaining.values() and not blob.is_symlink():
                        blob.unlink(missing_ok=True)

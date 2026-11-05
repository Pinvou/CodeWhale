#!/usr/bin/env python3
"""Validate a declared Windows runner peer without reading its configuration.

Inputs are runner installation directories, not paths inferred from a cache or
tool directory. Their provisioning/identity mapping belongs to the caller.
"""

import argparse
import ctypes
from ctypes import wintypes
import errno
import json
import ntpath
import os
import stat
import sys


class IsolationError(RuntimeError):
    """A fixed diagnostic code, with no host configuration or file contents."""


INVALID_HANDLE_VALUE = ctypes.c_void_p(-1).value
FILE_READ_DATA = 0x0001


def _kernel32():
    library = ctypes.WinDLL("kernel32", use_last_error=True)
    library.CreateFileW.argtypes = [wintypes.LPCWSTR, wintypes.DWORD, wintypes.DWORD,
                                   wintypes.LPVOID, wintypes.DWORD, wintypes.DWORD, wintypes.HANDLE]
    library.CreateFileW.restype = wintypes.HANDLE
    library.CloseHandle.argtypes = [wintypes.HANDLE]
    library.CloseHandle.restype = wintypes.BOOL
    return library


def _last_error():
    return ctypes.get_last_error()


def _require_read_denied(configuration):
    # The CRT's os.open can lose the native Windows error code. Use the native
    # handle API so only ERROR_ACCESS_DENIED is positive ACL evidence.
    try:
        library = _kernel32()
        # Request only FILE_READ_DATA; GENERIC_READ also includes metadata and
        # security-descriptor rights whose denial does not prove data is unreadable.
        # Share READ|WRITE|DELETE; OPEN_EXISTING; FILE_ATTRIBUTE_NORMAL.
        # No content read, creation or write access.
        handle = library.CreateFileW(configuration, FILE_READ_DATA, 7, None, 3, 0x80, None)
    except OSError:
        raise IsolationError("windows_read_api_unavailable") from None
    if handle == INVALID_HANDLE_VALUE:
        if _last_error() == 5:
            return
        raise IsolationError("peer_read_did_not_prove_acl_denial")
    if handle is None:
        raise IsolationError("peer_read_did_not_prove_acl_denial")
    if not library.CloseHandle(handle):
        raise IsolationError("peer_handle_close_failed")
    raise IsolationError("peer_configuration_readable")


def _root(value, label):
    if not isinstance(value, str) or not value.strip():
        raise IsolationError(f"{label}_undefined")
    value = value.replace("/", "\\")
    drive, tail = ntpath.splitdrive(value)
    if drive.lower() != "d:" or not tail.startswith("\\"):
        raise IsolationError(f"{label}_must_be_absolute_d_drive")
    for part in tail.split("\\"):
        if part in ("", ".", ".."):
            continue
        # Win32 trims trailing dots/spaces and interprets device names and
        # streams specially. Refuse those aliases rather than comparing them
        # as ordinary directory names.
        device = part.split(".", 1)[0].upper()
        if (part.endswith((" ", ".")) or any(ord(c) < 32 or c in '<>:"|?*' for c in part)
                or device in {"CON", "PRN", "AUX", "NUL"}
                or device in {f"{prefix}{n}" for prefix in ("COM", "LPT") for n in range(1, 10)}):
            raise IsolationError(f"{label}_ambiguous_windows_path")
    normalized = ntpath.normpath(value)
    if normalized.lower() == "d:\\":
        raise IsolationError(f"{label}_must_be_instance_directory")
    return normalized


def _disjoint(current, peer):
    current, peer = ntpath.normcase(current), ntpath.normcase(peer)
    if ntpath.commonpath((current, peer)) in (current, peer):
        raise IsolationError("runner_directories_overlap")


def _existing_directory(value, label):
    try:
        canonical = os.path.realpath(value, strict=True)
        # Windows realpath can return the extended namespace for long paths;
        # normalize only an OS-produced local-drive prefix, never user input.
        if canonical.startswith("\\\\?\\"):
            canonical = canonical[4:]
        canonical = _root(canonical, label)
        if not stat.S_ISDIR(os.stat(canonical).st_mode):
            raise IsolationError(f"{label}_not_directory")
        return canonical
    except OSError:
        raise IsolationError(f"{label}_unverifiable") from None


def _runner_identity(root):
    try:
        with open(ntpath.join(root, ".runner"), "rb") as source:
            contents = source.read(64 * 1024 + 1)
        if len(contents) > 64 * 1024:
            raise IsolationError("current_runner_metadata_invalid")
        metadata = json.loads(contents)
        name = metadata.get("agentName") if isinstance(metadata, dict) else None
        if not isinstance(name, str) or not name.strip():
            raise IsolationError("current_runner_metadata_invalid")
        return name
    except (OSError, UnicodeError, ValueError):
        raise IsolationError("current_runner_metadata_unverifiable") from None


def _configuration_metadata(configuration, *, optional=False):
    try:
        info = os.lstat(configuration)
    except FileNotFoundError as error:
        # Only a positively identified missing file may be optional. A missing
        # parent (Windows error 3), hidden metadata or another error is not proof
        # that an optional credential store is absent.
        if (optional and error.errno == errno.ENOENT
                and getattr(error, "winerror", None) in (None, 2)):
            return False
        raise IsolationError("peer_configuration_unverifiable") from None
    except OSError:
        raise IsolationError("peer_configuration_unverifiable") from None
    if not stat.S_ISREG(info.st_mode) or getattr(info, "st_file_attributes", 0) & 0x400:
        raise IsolationError("peer_configuration_not_regular")
    return True


def validate_peer_isolation(current_root, peer_root, runner_name, peer_runner_name):
    """Require a verifiable, distinct peer and an actual Windows ACL denial.

    Names/roots must come from the maintenance administrator's explicit mapping.
    The current readable .runner metadata additionally anchors that mapping to
    RUNNER_NAME. Unavailable directory/file metadata is uncertainty, not proof
    that an arbitrary or missing peer's configuration is protected.
    """
    if sys.platform != "win32":
        raise IsolationError("native_windows_required")
    if any(not isinstance(name, str) or not name.strip() for name in (runner_name, peer_runner_name)):
        raise IsolationError("runner_identity_undefined")
    if runner_name.casefold() == peer_runner_name.casefold():
        raise IsolationError("peer_runner_identity_is_current")
    current = _root(current_root, "current_root")
    peer = _root(peer_root, "peer_root")
    _disjoint(current, peer)
    current = _existing_directory(current, "current_root")
    peer = _existing_directory(peer, "peer_root")
    _disjoint(current, peer)
    if _runner_identity(current).casefold() != runner_name.casefold():
        raise IsolationError("current_runner_identity_mismatch")
    for filename in (".runner", ".env"):
        _configuration_metadata(ntpath.join(peer, filename))
    protected = [ntpath.join(peer, ".env")]
    # Match runner ConfigurationStore.HasCredentials: either store can hold a
    # registered identity, but every existing store must deny peer reads.
    # https://github.com/actions/runner/blob/cf65f3ec8100ee0046d735ac5d647e00f4b8e817/src/Runner.Common/ConfigurationStore.cs
    for filename in (".credentials", ".credentials_migrated"):
        configuration = ntpath.join(peer, filename)
        if _configuration_metadata(configuration, optional=True):
            protected.append(configuration)
    if len(protected) == 1:
        raise IsolationError("peer_credentials_missing")
    rsa = ntpath.join(peer, ".credentials_rsaparams")
    if _configuration_metadata(rsa, optional=True):
        protected.append(rsa)
    for configuration in protected:
        _require_read_denied(configuration)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--current-root", required=True)
    parser.add_argument("--peer-root", required=True)
    parser.add_argument("--runner-name", required=True)
    parser.add_argument("--peer-runner-name", required=True)
    args = parser.parse_args(argv)
    try:
        validate_peer_isolation(args.current_root, args.peer_root, args.runner_name, args.peer_runner_name)
    except IsolationError as error:
        print(f"Windows peer isolation failed: {error}", file=sys.stderr)
        return 1
    print("Windows peer configuration open was denied for the declared distinct runner.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

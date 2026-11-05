"""Exercise the production peer validator; Windows I/O outcomes are controlled.

These cases run on Linux too. They do not establish native Windows ACL or
GetFinalPathNameByHandle behavior; the manual Windows workflow must do that.
"""

import contextlib
import errno
import importlib.util
import io
import json
from pathlib import Path
import stat
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, mock_open, patch


SCRIPT = Path(__file__).resolve().parents[1] / "ci-windows-peer-isolation.py"
SPEC = importlib.util.spec_from_file_location("windows_peer_isolation", SCRIPT)
PEER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PEER)

CURRENT = r"D:\workers\one\runner"
OTHER = r"D:\workers\two\runner"


def windows_error(code):
    error = OSError("private host path or environment contents must not reach logs")
    error.winerror = code
    return error


def missing_file(code=2):
    error = FileNotFoundError(errno.ENOENT, "private peer configuration path")
    error.winerror = code
    return error


class WindowsPeerIsolationTests(unittest.TestCase):
    def setUp(self):
        self.enterContext(patch.object(PEER.sys, "platform", "win32"))
        self.resolve = self.enterContext(patch.object(PEER.os.path, "realpath", side_effect=lambda path, **_: path))
        self.directory = self.enterContext(patch.object(PEER.os, "stat", return_value=SimpleNamespace(st_mode=stat.S_IFDIR)))
        self.metadata = self.enterContext(patch("builtins.open", mock_open(read_data=json.dumps({"agentName": "one"}).encode())))
        self.info = self.enterContext(patch.object(PEER.os, "lstat", return_value=SimpleNamespace(st_mode=stat.S_IFREG, st_file_attributes=0)))
        self.open_handle = Mock(return_value=PEER.INVALID_HANDLE_VALUE)
        self.close = Mock(return_value=1)
        self.kernel = self.enterContext(patch.object(PEER, "_kernel32", return_value=SimpleNamespace(
            CreateFileW=self.open_handle, CloseHandle=self.close)))
        self.last_error = self.enterContext(patch.object(PEER, "_last_error", return_value=5))
        self.read = self.enterContext(patch.object(PEER.os, "read"))

    def verify(self, current=CURRENT, peer=OTHER, runner="one", peer_runner="two"):
        return PEER.validate_peer_isolation(current, peer, runner, peer_runner)

    def test_explicit_arbitrary_peer_is_used_without_layout_guess_or_content_read(self):
        self.assertIsNone(self.verify(peer=r"D:\separate-root\custom-agent"))
        protected = [".env", ".credentials", ".credentials_migrated", ".credentials_rsaparams"]
        self.assertEqual(self.open_handle.call_count, len(protected))
        self.assertEqual([call.args for call in self.open_handle.call_args_list], [
            (r"D:\separate-root\custom-agent" + "\\" + name, 0x0001, 7, None, 3, 0x80, None)
            for name in protected
        ])
        self.assertEqual([call.args[0] for call in self.info.call_args_list], [
            r"D:\separate-root\custom-agent\.runner", r"D:\separate-root\custom-agent\.env",
            r"D:\separate-root\custom-agent\.credentials",
            r"D:\separate-root\custom-agent\.credentials_migrated",
            r"D:\separate-root\custom-agent\.credentials_rsaparams",
        ])
        self.metadata.assert_called_once_with(CURRENT + r"\.runner", "rb")
        self.metadata().read.assert_called_once_with(64 * 1024 + 1)
        self.read.assert_not_called()
        self.close.assert_not_called()

    def test_equivalent_lexical_roots_fail_before_filesystem_access(self):
        for peer in (CURRENT, CURRENT.lower(), CURRENT + "\\", r"D:\workers\one\runner\..\runner", "D:/workers/one/runner"):
            with self.subTest(peer=peer), self.assertRaisesRegex(PEER.IsolationError, "overlap"):
                self.verify(peer=peer)
        self.resolve.assert_not_called()
        self.open_handle.assert_not_called()

    def test_either_credential_store_can_establish_the_required_presence(self):
        regular = SimpleNamespace(st_mode=stat.S_IFREG, st_file_attributes=0)
        for present, absent in ((".credentials", ".credentials_migrated"),
                                (".credentials_migrated", ".credentials")):
            def metadata(path):
                if path.endswith("\\" + absent) or path.endswith("\\.credentials_rsaparams"):
                    raise missing_file()
                return regular

            with self.subTest(present=present):
                self.info.side_effect = metadata
                self.open_handle.reset_mock()
                self.assertIsNone(self.verify())
                self.assertEqual([call.args[0] for call in self.open_handle.call_args_list], [
                    OTHER + r"\.env", OTHER + "\\" + present,
                ])
        self.metadata.assert_called_with(CURRENT + r"\.runner", "rb")
        self.read.assert_not_called()

    def test_both_missing_credential_stores_fail_before_any_read_open(self):
        regular = SimpleNamespace(st_mode=stat.S_IFREG, st_file_attributes=0)

        def metadata(path):
            if path.endswith(("\\.credentials", "\\.credentials_migrated")):
                raise missing_file()
            return regular

        self.info.side_effect = metadata
        with self.assertRaisesRegex(PEER.IsolationError, "peer_credentials_missing"):
            self.verify()
        self.open_handle.assert_not_called()

    def test_optional_rsa_absence_is_accepted_without_opening_a_missing_file(self):
        regular = SimpleNamespace(st_mode=stat.S_IFREG, st_file_attributes=0)

        def metadata(path):
            if path.endswith("\\.credentials_rsaparams"):
                raise missing_file()
            return regular

        self.info.side_effect = metadata
        self.assertIsNone(self.verify())
        self.assertEqual([call.args[0] for call in self.open_handle.call_args_list], [
            OTHER + r"\.env", OTHER + r"\.credentials", OTHER + r"\.credentials_migrated",
        ])
        self.metadata.assert_called_once_with(CURRENT + r"\.runner", "rb")
        self.read.assert_not_called()

    def test_readable_credential_or_rsa_file_fails_even_when_env_is_denied(self):
        for filename in (".credentials", ".credentials_migrated", ".credentials_rsaparams"):
            with self.subTest(filename=filename):
                self.open_handle.reset_mock()
                self.close.reset_mock()
                self.open_handle.side_effect = lambda path, *_, name=filename: (
                    123 if path.endswith("\\" + name) else PEER.INVALID_HANDLE_VALUE)
                with self.assertRaisesRegex(PEER.IsolationError, "configuration_readable"):
                    self.verify()
                self.assertEqual(self.open_handle.call_args_list[0].args[0], OTHER + r"\.env")
                self.assertEqual(self.open_handle.call_args_list[-1].args[0], OTHER + "\\" + filename)
                self.close.assert_called_once_with(123)
        self.metadata.assert_called_with(CURRENT + r"\.runner", "rb")
        self.read.assert_not_called()

    def test_generic_read_denial_cannot_mask_permitted_file_data_access(self):
        # Model an ACL that denies a right bundled into GENERIC_READ while
        # allowing the minimal FILE_READ_DATA request on one protected file.
        for filename in (".env", ".credentials", ".credentials_migrated", ".credentials_rsaparams"):
            def acl(path, access, *_):
                if path.endswith("\\" + filename) and access == 0x0001:
                    return 123
                return PEER.INVALID_HANDLE_VALUE

            with self.subTest(filename=filename):
                self.open_handle.reset_mock()
                self.close.reset_mock()
                self.open_handle.side_effect = acl
                with self.assertRaisesRegex(PEER.IsolationError, "configuration_readable"):
                    self.verify()
                self.assertTrue(all(call.args[1] == 0x0001 for call in self.open_handle.call_args_list))
                self.assertEqual(self.open_handle.call_args.args[0], OTHER + "\\" + filename)
                self.close.assert_called_once_with(123)
        self.read.assert_not_called()

    def test_only_definite_optional_file_absence_is_accepted(self):
        regular = SimpleNamespace(st_mode=stat.S_IFREG, st_file_attributes=0)
        for filename in (".credentials", ".credentials_migrated", ".credentials_rsaparams"):
            for failure in (windows_error(5), windows_error(32), missing_file(3),
                            FileNotFoundError("unknown absence"), OSError("unknown metadata error")):
                def metadata(path):
                    if path.endswith("\\" + filename):
                        raise failure
                    return regular

                with self.subTest(filename=filename, error=type(failure).__name__,
                                  winerror=getattr(failure, "winerror", None)):
                    self.info.side_effect = metadata
                    with self.assertRaisesRegex(PEER.IsolationError, "configuration_unverifiable"):
                        self.verify()
        self.open_handle.assert_not_called()

    def test_credential_and_rsa_metadata_cannot_be_directory_symlink_or_reparse(self):
        regular = SimpleNamespace(st_mode=stat.S_IFREG, st_file_attributes=0)
        for filename in (".credentials", ".credentials_migrated", ".credentials_rsaparams"):
            for mode, attributes in ((stat.S_IFDIR, 0), (stat.S_IFLNK, 0), (stat.S_IFREG, 0x400)):
                def metadata(path):
                    if path.endswith("\\" + filename):
                        return SimpleNamespace(st_mode=mode, st_file_attributes=attributes)
                    return regular

                with self.subTest(filename=filename, mode=mode, attributes=attributes):
                    self.info.side_effect = metadata
                    with self.assertRaisesRegex(PEER.IsolationError, "not_regular"):
                        self.verify()
        self.open_handle.assert_not_called()

    def test_every_existing_secret_requires_native_access_denied(self):
        for filename in (".credentials", ".credentials_migrated", ".credentials_rsaparams"):
            for code in (2, 3, 32, 0, None):
                self.last_error.side_effect = lambda name=filename, failure=code: (
                    failure if self.open_handle.call_args.args[0].endswith("\\" + name) else 5)
                with self.subTest(filename=filename, winerror=code):
                    with self.assertRaisesRegex(PEER.IsolationError, "did_not_prove_acl_denial"):
                        self.verify()
        self.read.assert_not_called()

    def test_ancestor_or_descendant_is_not_an_independent_instance(self):
        for peer in (r"D:\workers\one", CURRENT + r"\nested"):
            with self.subTest(peer=peer), self.assertRaisesRegex(PEER.IsolationError, "overlap"):
                self.verify(peer=peer)
        self.open_handle.assert_not_called()

    def test_similar_prefix_is_not_mistaken_for_an_ancestor(self):
        self.assertIsNone(self.verify(peer=r"D:\workers\one-extra\runner"))

    def test_undefined_non_d_relative_device_and_ambiguous_paths_fail_closed(self):
        for value in (None, "", " ", r"C:\workers\two", r"D:workers\two", r"\workers\two",
                      r"\\server\share\two", "D:\\", r"\\?\D:\workers\two", r"D:\workers\two.",
                      "D:\\workers\\two ", r"D:\workers\two:stream", r"D:\workers\NUL"):
            for parameter in ("current", "peer"):
                with self.subTest(value=value, parameter=parameter), self.assertRaises(PEER.IsolationError):
                    self.verify(**{parameter: value})
        self.open_handle.assert_not_called()

    def test_both_identities_are_required_and_peer_identity_must_differ(self):
        for runner, peer_runner in (("", "two"), ("one", None), ("one", "ONE")):
            with self.subTest(runner=runner, peer=peer_runner), self.assertRaises(PEER.IsolationError):
                self.verify(runner=runner, peer_runner=peer_runner)
        self.open_handle.assert_not_called()

    def test_current_metadata_must_match_actual_runner_name(self):
        with self.assertRaisesRegex(PEER.IsolationError, "identity_mismatch"):
            self.verify(runner="some-other-runner")
        self.open_handle.assert_not_called()

    def test_missing_invalid_or_oversized_current_metadata_fails(self):
        for contents in (b"{}", b"null", b"not-json", b"x" * (64 * 1024 + 1)):
            with self.subTest(contents=contents[:10]), patch("builtins.open", mock_open(read_data=contents)):
                with self.assertRaises(PEER.IsolationError):
                    self.verify()
        self.metadata.side_effect = FileNotFoundError("unknown current runner")
        with self.assertRaisesRegex(PEER.IsolationError, "metadata_unverifiable"):
            self.verify()
        self.open_handle.assert_not_called()

    def test_physical_aliases_of_current_root_are_rejected(self):
        for physical_peer in (r"d:\resolved\agent", r"D:\resolved\agent\child", r"D:\resolved"):
            self.resolve.side_effect = [r"D:\resolved\agent", physical_peer]
            with self.subTest(peer=physical_peer), self.assertRaisesRegex(PEER.IsolationError, "overlap"):
                self.verify()
        self.open_handle.assert_not_called()

    def test_short_path_or_junction_alias_resolving_to_current_fails(self):
        self.resolve.side_effect = [r"D:\workers\one\runner", r"D:\workers\one\runner"]
        with self.assertRaisesRegex(PEER.IsolationError, "overlap"):
            self.verify(peer=r"D:\WORKER~1\ONE\runner")
        self.open_handle.assert_not_called()

    def test_os_extended_local_path_is_canonicalized(self):
        self.resolve.side_effect = [r"\\?\D:\workers\one\runner", r"\\?\D:\workers\two\runner"]
        self.assertIsNone(self.verify())

    def test_physical_non_d_or_unverifiable_directory_is_not_isolation_proof(self):
        for physical in (r"C:\another-volume\runner", r"\\server\share\runner"):
            self.resolve.side_effect = [CURRENT, physical]
            with self.subTest(physical=physical), self.assertRaises(PEER.IsolationError):
                self.verify()
        for failure in (FileNotFoundError("missing"), windows_error(5)):
            self.resolve.side_effect = [CURRENT, failure]
            with self.subTest(error=type(failure)), self.assertRaisesRegex(PEER.IsolationError, "peer_root_unverifiable"):
                self.verify()
        self.open_handle.assert_not_called()

    def test_configured_roots_must_be_directories(self):
        self.directory.return_value.st_mode = stat.S_IFREG
        with self.assertRaisesRegex(PEER.IsolationError, "not_directory"):
            self.verify()
        self.open_handle.assert_not_called()

    def test_missing_peer_file_or_unreadable_file_metadata_fails_before_open(self):
        for failure in (FileNotFoundError("undefined peer"), windows_error(5)):
            self.info.side_effect = failure
            with self.subTest(error=type(failure)), self.assertRaisesRegex(PEER.IsolationError, "configuration_unverifiable"):
                self.verify()
        self.open_handle.assert_not_called()

    def test_both_peer_runner_and_env_metadata_must_exist(self):
        regular = SimpleNamespace(st_mode=stat.S_IFREG, st_file_attributes=0)
        for outcomes in ([FileNotFoundError("missing .runner")], [regular, FileNotFoundError("missing .env")]):
            self.info.side_effect = outcomes
            with self.subTest(outcomes=outcomes), self.assertRaisesRegex(PEER.IsolationError, "configuration_unverifiable"):
                self.verify()
        self.open_handle.assert_not_called()

    def test_directory_symlink_or_reparse_env_is_not_accepted(self):
        for mode, attributes in ((stat.S_IFDIR, 0), (stat.S_IFLNK, 0), (stat.S_IFREG, 0x400)):
            self.info.return_value = SimpleNamespace(st_mode=mode, st_file_attributes=attributes)
            with self.subTest(mode=mode, attributes=attributes), self.assertRaisesRegex(PEER.IsolationError, "not_regular"):
                self.verify()
        self.open_handle.assert_not_called()

    def test_only_windows_access_denied_proves_the_acl_result(self):
        for code in (2, 3, 32, 0, None):
            self.last_error.return_value = code
            with self.subTest(winerror=code), self.assertRaisesRegex(PEER.IsolationError, "did_not_prove_acl_denial"):
                self.verify()
        self.read.assert_not_called()

    def test_readable_peer_handle_is_closed_without_reading_and_fails(self):
        self.open_handle.side_effect = None
        self.open_handle.return_value = 123
        with self.assertRaisesRegex(PEER.IsolationError, "configuration_readable"):
            self.verify()
        self.close.assert_called_once_with(123)
        self.read.assert_not_called()

    def test_close_error_also_fails_with_fixed_diagnostic(self):
        self.open_handle.side_effect = None
        self.open_handle.return_value = 123
        self.close.return_value = 0
        with self.assertRaisesRegex(PEER.IsolationError, "handle_close_failed"):
            self.verify()

    def test_native_api_failure_or_null_handle_is_not_acl_evidence(self):
        self.kernel.side_effect = OSError("private host API path")
        with self.assertRaisesRegex(PEER.IsolationError, "windows_read_api_unavailable"):
            self.verify()
        self.kernel.side_effect = None
        self.open_handle.return_value = None
        with self.assertRaisesRegex(PEER.IsolationError, "did_not_prove_acl_denial"):
            self.verify()

    def test_non_windows_runtime_is_not_native_acceptance(self):
        with patch.object(PEER.sys, "platform", "linux"), self.assertRaisesRegex(PEER.IsolationError, "native_windows_required"):
            self.verify()
        self.resolve.assert_not_called()

    def test_cli_reports_failure_without_raw_errors_or_host_paths(self):
        self.last_error.return_value = 32
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            result = PEER.main(["--current-root", CURRENT, "--peer-root", OTHER,
                                "--runner-name", "one", "--peer-runner-name", "two"])
        self.assertEqual(result, 1)
        self.assertEqual(stdout.getvalue(), "")
        self.assertEqual(stderr.getvalue(), "Windows peer isolation failed: peer_read_did_not_prove_acl_denial\n")


if __name__ == "__main__":
    result = unittest.main(exit=False).result
    # The manual native gate must execute this contract rather than accepting
    # an empty selection, platform skips or expected failures as validation.
    raise SystemExit(0 if result.testsRun and result.wasSuccessful()
                     and not result.skipped and not result.expectedFailures else 1)

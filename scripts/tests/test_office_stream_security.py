"""Linux UNO endpoint checks, without LibreOffice, a model, or a network service."""
import importlib.util
import os
from pathlib import Path
import socket
import stat
import sys
import tempfile
import types
import unittest
from unittest.mock import Mock, patch


HELPER = (Path(__file__).resolve().parents[2]
          / "pinvou3-app/src-tauri/src/features/files/platform/office_stream.py")


@unittest.skipUnless(sys.platform.startswith("linux"), "Linux sealed-source UNO adapter")
class OfficeStreamEndpointTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location("office_stream_security", HELPER)
        self.helper = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.helper)
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.node = Path(self.directory.name) / "uno.socket"
        self.server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(self.server.close)
        self.server.bind(str(self.node))
        self.node.chmod(0o700)
        self.resolver = Mock()
        self.resolver.resolve.return_value = "context"
        self.process = Mock()
        self.process.poll.return_value = None

    def wait(self, **kwargs):
        with patch.object(self.helper, "pipe_node_path", return_value=self.node, create=True):
            return self.helper.wait_for_context(self.resolver, "pinvou_test", self.process, **kwargs)

    def test_shared_socket_is_rejected_before_any_uno_connection(self):
        self.node.chmod(0o777)
        with self.assertRaises(RuntimeError):
            self.wait()
        self.resolver.resolve.assert_not_called()

    def test_foreign_owner_metadata_is_rejected_before_connecting(self):
        # No privilege changes: the kernel UID is supplied at the lstat boundary.
        fields = list(self.node.lstat())
        fields[4] = os.geteuid() + 1
        with patch.object(Path, "lstat", return_value=os.stat_result(fields)):
            with self.assertRaises(RuntimeError):
                self.wait()
        self.resolver.resolve.assert_not_called()

    def test_regular_file_and_symlink_cannot_be_uno_endpoints(self):
        self.node.unlink()
        self.node.write_text("untrusted")
        with self.assertRaises(RuntimeError):
            self.wait()
        self.node.unlink()
        target = Path(self.directory.name) / "private.socket"
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
            server.bind(str(target))
            target.chmod(0o700)
            self.node.symlink_to(target)
            with self.assertRaises(RuntimeError):
                self.wait()
        self.resolver.resolve.assert_not_called()

    def test_private_owned_socket_can_resolve(self):
        self.assertEqual(self.wait(), "context")
        self.resolver.resolve.assert_called_once_with(
            "uno:pipe,name=pinvou_test;urp;StarOffice.ComponentContext")

    def test_endpoint_replacement_during_resolve_is_rejected(self):
        replacement = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(replacement.close)

        def replace(_):
            self.node.unlink()
            replacement.bind(str(self.node))
            self.node.chmod(0o700)
            return "untrusted context"

        self.resolver.resolve.side_effect = replace
        with self.assertRaises(RuntimeError):
            self.wait()

    def test_permissions_are_rechecked_after_resolve(self):
        def expose(_):
            self.node.chmod(0o770)
            return "context"

        self.resolver.resolve.side_effect = expose
        with self.assertRaises(RuntimeError):
            self.wait()

    def test_absent_node_waits_within_the_existing_startup_budget(self):
        self.node.unlink()
        ticks = iter([0, 21, 61])
        pause = Mock()
        with self.assertRaises(RuntimeError):
            self.wait(clock=lambda: next(ticks), pause=pause)
        self.resolver.resolve.assert_not_called()
        pause.assert_called_once_with(0.05)

    def test_private_node_can_become_ready_after_a_cold_start(self):
        self.resolver.resolve.side_effect = [RuntimeError("cold start"), "context"]
        self.assertEqual(self.wait(clock=lambda: 0, pause=lambda _: None), "context")
        self.assertEqual(self.resolver.resolve.call_count, 2)

    def test_exited_process_and_stat_errors_fail_without_connecting(self):
        self.process.poll.return_value = 1
        with patch.object(Path, "lstat", side_effect=FileNotFoundError):
            with self.assertRaises(RuntimeError):
                self.wait(clock=lambda: 0, pause=lambda _: self.fail("already exited"))
        with patch.object(Path, "lstat", side_effect=PermissionError):
            with self.assertRaises(PermissionError):
                self.wait()
        self.resolver.resolve.assert_not_called()

    def sealed_conversion_args(self):
        fcntl = self.helper.fcntl
        fd = os.memfd_create("uno-security-fixture", os.MFD_ALLOW_SEALING)
        self.addCleanup(os.close, fd)
        os.write(fd, b"{\\rtf1 Synthetic private document.}")
        fcntl.fcntl(fd, fcntl.F_ADD_SEALS,
                    fcntl.F_SEAL_WRITE | fcntl.F_SEAL_GROW
                    | fcntl.F_SEAL_SHRINK | fcntl.F_SEAL_SEAL)
        modules = {name: types.ModuleType(name) for name in (
            "uno", "com", "com.sun", "com.sun.star", "com.sun.star.document",
            "com.sun.star.document.MacroExecMode", "com.sun.star.document.UpdateDocMode")}
        modules["com.sun.star.document.MacroExecMode"].NEVER_EXECUTE = 4
        modules["com.sun.star.document.UpdateDocMode"].NO_UPDATE = 0
        argv = ["office_stream", f"/proc/{os.getpid()}/fd/{fd}",
                self.directory.name, "txt", "soffice", "1024"]
        return modules, argv

    def test_preexisting_nodes_stop_conversion_before_soffice_starts(self):
        # Exercise convert(), including real sealed input, rather than only a guard.
        modules, argv = self.sealed_conversion_args()
        for kind in ("socket", "file", "dangling symlink"):
            with self.subTest(kind=kind):
                if kind == "file":
                    self.node.unlink()
                    self.node.write_text("do not remove")
                elif kind == "dangling symlink":
                    self.node.unlink()
                    self.node.symlink_to(Path(self.directory.name) / "missing")
                identity = self.node.lstat()
                # convert owns its umask; restore the test process's mask afterwards.
                previous_mask = os.umask(0o077)
                try:
                    with patch.dict(sys.modules, modules), patch.object(sys, "argv", argv), \
                            patch.object(self.helper, "pipe_node_path", return_value=self.node), \
                            patch.object(self.helper.subprocess, "Popen") as launch:
                        with self.assertRaises(RuntimeError):
                            self.helper.convert()
                        launch.assert_not_called()
                finally:
                    os.umask(previous_mask)
                self.assertEqual(self.node.lstat(), identity)

    def test_racing_shared_node_stops_before_payload_and_reaps_child(self):
        modules, argv = self.sealed_conversion_args()
        local = Mock()
        local.ServiceManager.createInstanceWithContext.return_value = self.resolver
        modules["uno"].getComponentContext = lambda: local
        modules["uno"].ByteSequence = Mock()
        child = Mock()
        child.wait.return_value = 0
        racing_server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(racing_server.close)
        self.node.unlink()

        def launch(*args, **kwargs):
            racing_server.bind(str(self.node))
            self.node.chmod(0o777)
            return child

        previous_mask = os.umask(0o077)
        try:
            with patch.dict(sys.modules, modules), patch.object(sys, "argv", argv), \
                    patch.object(self.helper, "pipe_node_path", return_value=self.node), \
                    patch.object(self.helper.subprocess, "Popen", side_effect=launch):
                with self.assertRaises(RuntimeError):
                    self.helper.convert()
        finally:
            os.umask(previous_mask)
        self.resolver.resolve.assert_not_called()
        modules["uno"].ByteSequence.assert_not_called()
        child.terminate.assert_called_once()
        child.wait.assert_called_once_with(timeout=5)
        self.assertEqual(stat.S_IMODE(self.node.lstat().st_mode), 0o777)

    def test_untrusted_or_nonsticky_shared_parent_is_rejected(self):
        fields = list(Path(self.directory.name).lstat())
        for mode, uid in ((stat.S_IFDIR | 0o777, 0),
                          (stat.S_IFDIR | 0o1777, os.geteuid() + 1),
                          (stat.S_IFLNK | 0o777, 0)):
            with self.subTest(mode=mode, uid=uid):
                fields[0], fields[4] = mode, uid
                with patch.object(self.helper.os, "access", return_value=True), \
                        patch.object(Path, "lstat", return_value=os.stat_result(fields)):
                    with self.assertRaises(RuntimeError):
                        self.helper.pipe_node_path("pinvou_test")

    def test_osl_path_selection_uses_uid_and_var_tmp_fallback(self):
        private_directory = Path(self.directory.name).lstat()
        with patch.object(self.helper.os, "access", side_effect=[False, True]), \
                patch.object(Path, "lstat", return_value=private_directory):
            self.assertEqual(self.helper.pipe_node_path("pinvou_test"),
                             Path(f"/var/tmp/OSL_PIPE_{os.getuid()}_pinvou_test"))
        with patch.object(self.helper.os, "access", return_value=False):
            with self.assertRaises(RuntimeError):
                self.helper.pipe_node_path("pinvou_test")


if __name__ == "__main__":
    unittest.main()

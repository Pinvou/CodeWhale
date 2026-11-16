"""Check isolation failure policy and real Linux loopback/identity semantics."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

HELPER = Path(__file__).resolve().parents[1] / 'ci-browser-network.py'
SUPERVISOR = HELPER.with_name('ci-fixture-process.py')
spec = importlib.util.spec_from_file_location('browser_network', HELPER)
NETWORK = importlib.util.module_from_spec(spec)
spec.loader.exec_module(NETWORK)


class FailurePolicyTests(unittest.TestCase):
    def test_unsupported_platform_never_executes_the_fixture(self):
        with patch.object(NETWORK.sys, 'platform', 'win32'), patch.object(NETWORK.os, 'execv') as execute:
            self.assertEqual(NETWORK.main(['--', sys.executable, '-c', 'raise SystemExit(0)']), 78)
            execute.assert_not_called()

    @unittest.skipUnless(sys.platform == 'linux', 'Linux launch prerequisites')
    def test_missing_unshare_never_executes_in_host_network(self):
        with patch.object(NETWORK.os, 'getuid', return_value=1008), patch.object(NETWORK.shutil, 'which', return_value=None), patch.object(NETWORK.os, 'execv') as execute:
            self.assertEqual(NETWORK.main(['--', 'unexpected-host-command']), 78)
            execute.assert_not_called()


@unittest.skipUnless(sys.platform == 'linux' and os.environ.get('PINVOU_TEST_NETWORK_ISOLATION') == '1', 'real namespace checks run in the Linux browser shard preflight')
class LinuxNetworkTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if os.getuid() == 0 or not shutil.which('unshare'):
            raise RuntimeError('required browser network preflight needs a non-root user and unshare')

    def invoke(self, source, directory):
        receipt = Path(directory) / 'process.json'
        result = subprocess.run([sys.executable, str(SUPERVISOR), '--receipt', str(receipt), '--cleanup-seconds', '0.3', '--', sys.executable, str(HELPER), '--', sys.executable, '-c', source], capture_output=True, text=True, timeout=20)
        return result, json.loads(receipt.read_text()), receipt

    def test_parent_listener_is_invisible_but_fixture_and_its_children_share_loopback(self):
        parent_network = os.readlink('/proc/self/ns/net')
        parent_cgroup = hashlib.sha256(Path('/proc/self/cgroup').read_bytes()).hexdigest()
        with socket.socket() as listener, tempfile.TemporaryDirectory() as directory:
            listener.bind(('127.0.0.1', 0)); listener.listen()
            port = listener.getsockname()[1]
            source = f"""
import socket,os,pathlib,json,hashlib,subprocess,sys
try:
    socket.create_connection(('127.0.0.1',{port}),timeout=0.2)
except ConnectionRefusedError:
    pass
else:
    raise AssertionError('host listener leaked into fixture network')
server=socket.socket();server.bind(('127.0.0.1',{port}));server.listen()
child=subprocess.Popen([sys.executable,'-c',"import socket; s=socket.create_connection(('127.0.0.1',{port}));s.sendall(b'owned');s.close()"])
connection,_=server.accept();assert connection.recv(20)==b'owned';connection.close();server.close();assert child.wait()==0
assert os.getuid()=={os.getuid()} and os.getgid()=={os.getgid()}
assert os.readlink('/proc/self/ns/net')!={parent_network!r}
assert hashlib.sha256(pathlib.Path('/proc/self/cgroup').read_bytes()).hexdigest()=={parent_cgroup!r}
assert [name for _,name in socket.if_nameindex()]==['lo']
status=dict(l.split(':',1)for l in pathlib.Path('/proc/self/status').read_text().splitlines()if ':'in l)
assert all(int(status[k].strip(),16)==0 for k in ['CapEff','CapPrm','CapInh','CapAmb'])
print('private-loopback-confirmed')
"""
            result, receipt, path = self.invoke(source, directory)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('private-loopback-confirmed', result.stdout)
            self.assertEqual(receipt['state'], 'released'); self.assertFalse(receipt['cleanup_failed'])
            facts = json.loads((path.parent / 'process-resources/browser-network.txt').read_text())
            self.assertEqual(facts['mode'], 'loopback'); self.assertNotEqual(facts['network'], facts['parent_network'])

    def test_business_exit_code_survives_isolation_and_retirement(self):
        with tempfile.TemporaryDirectory() as directory:
            result, receipt, _ = self.invoke('raise SystemExit(7)', directory)
            self.assertEqual(result.returncode, 7, result.stderr)
            self.assertEqual(receipt['exit_code'], 7); self.assertFalse(receipt['cleanup_failed'])

    def test_detached_descendant_remains_owned_by_outer_supervisor(self):
        with tempfile.TemporaryDirectory() as directory:
            source = "import subprocess,sys; p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)'],start_new_session=True);print('owned-detached='+str(p.pid),flush=True)"
            result, receipt, _ = self.invoke(source, directory)
            self.assertEqual(result.returncode, 86, result.stderr)
            self.assertTrue(receipt['cleanup_failed']); self.assertEqual(receipt['state'], 'released')
            pid = int(result.stdout.split('owned-detached=')[1].splitlines()[0])
            self.assertFalse(Path(f'/proc/{pid}').exists())

    def test_cancellation_retires_the_isolated_command(self):
        with tempfile.TemporaryDirectory() as directory:
            receipt = Path(directory) / 'process.json'
            command = [sys.executable, str(SUPERVISOR), '--receipt', str(receipt), '--cleanup-seconds', '0.3', '--', sys.executable, str(HELPER), '--', sys.executable, '-c', "import os,time;print('ready='+str(os.getpid()),flush=True);time.sleep(30)"]
            child = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                for line in child.stdout:
                    if line.startswith('ready='):
                        pid = int(line.split('=')[1]);break
                else:
                    self.fail('isolated fixture never started: ' + child.stderr.read())
                child.send_signal(signal.SIGTERM)
                _, error = child.communicate(timeout=10)
                self.assertEqual(child.returncode, 143, error)
                facts = json.loads(receipt.read_text())
                self.assertEqual(facts['state'], 'released');self.assertFalse(facts['cleanup_failed'])
                self.assertFalse(Path(f'/proc/{pid}').exists())
            finally:
                if child.poll() is None:child.kill();child.wait()


if __name__ == '__main__':
    unittest.main()

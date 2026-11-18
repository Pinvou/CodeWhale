import importlib.util
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import shutil
import socket
import sys
import time
import tempfile
import threading
import unittest

SPEC = importlib.util.spec_from_file_location('ci_npm_audit', Path(__file__).resolve().parents[1] / 'ci-npm-audit.py')
AUDIT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(AUDIT)


def report(**counts):
    values = dict.fromkeys(AUDIT.SEVERITIES, 0)
    values.update(counts)
    values['total'] = sum(values.values())
    return json.dumps({'auditReportVersion': 2, 'metadata': {'vulnerabilities': values}})


class AuditTests(unittest.TestCase):
    def exercise(self, results, *, elapsed=0):
        calls, logs, sleeps = [], [], []
        clock = [0]

        def run(command, environment, timeout):
            calls.append((command, environment, timeout))
            clock[0] += elapsed
            return results[min(len(calls) - 1, len(results) - 1)]

        def sleep(seconds):
            sleeps.append(seconds)
            clock[0] += seconds

        environment = {'HTTPS_PROXY': 'http://localhost:1234', 'NO_PROXY': 'registry.npmjs.org', 'KEEP': 'unchanged'}
        status = AUDIT.audit('fixture', run=run, environment=environment, clock=lambda: clock[0], sleep=sleep, emit=logs.append)
        self.assertEqual(environment['NO_PROXY'], 'registry.npmjs.org', 'the parent environment must not change')
        return status, calls, logs, sleeps

    def test_rebuilds_a_transient_post_then_requires_a_valid_live_report(self):
        status, calls, logs, sleeps = self.exercise([(1, '{"error":{"code":"ETIMEDOUT"}}'), (0, report())])
        self.assertEqual(status, 0)
        self.assertEqual(len(calls), 2)
        self.assertEqual(sleeps, [1])
        self.assertIn('--registry=https://registry.npmjs.org', calls[0][0])
        self.assertIn('--audit-level=high', calls[0][0])
        self.assertIn('--json', calls[0][0])
        self.assertEqual(calls[0][1]['NO_PROXY'], 'localhost,127.0.0.1,::1')
        self.assertEqual(calls[0][1]['KEEP'], 'unchanged')
        self.assertTrue(any('ETIMEDOUT' in line for line in logs))

    def test_high_and_critical_findings_block_immediately_even_if_npm_exits_zero(self):
        for severity in ['high', 'critical']:
            with self.subTest(severity=severity):
                status, calls, _, sleeps = self.exercise([(0, report(**{severity: 1}))])
                self.assertEqual(status, 1)
                self.assertEqual(len(calls), 1)
                self.assertEqual(sleeps, [])

    def test_real_npm_http_error_shape_retries_only_explicit_transient_statuses(self):
        for code in [408, 429, 500, 502, 503, 504]:
            with self.subTest(code=code):
                payload = {'statusCode': code, 'error': {'summary': '', 'detail': ''}}
                status, calls, _, sleeps = self.exercise([(1, json.dumps(payload)), (0, report())])
                self.assertEqual((status, len(calls), sleeps), (0, 2, [1]))
        for code in [400, 401, 403, 404, 501, True, '503', None]:
            with self.subTest(code=code):
                payload = {'statusCode': code, 'error': {'summary': '', 'detail': 'private'}}
                status, calls, logs, sleeps = self.exercise([(1, json.dumps(payload))])
                self.assertEqual((status, len(calls), sleeps), (1, 1, []))
                self.assertNotIn('private', '\n'.join(logs))

    def test_low_findings_keep_the_existing_high_severity_threshold(self):
        self.assertEqual(self.exercise([(0, report(low=2, moderate=1))])[0], 0)

    def test_http_400_authentication_and_unknown_failures_do_not_retry(self):
        for code in ['E400', 'E401', 'E403', 'UNKNOWN']:
            with self.subTest(code=code):
                status, calls, logs, sleeps = self.exercise([(1, json.dumps({'error': {'code': code, 'detail': 'private-network-error'}}))])
                self.assertEqual(status, 1)
                self.assertEqual(len(calls), 1)
                self.assertEqual(sleeps, [])
                self.assertNotIn('private-network-error', '\n'.join(logs))

    def test_invalid_or_missing_audit_data_never_passes(self):
        for payload in ['{}', '[]', 'null', 'not-json', '{"metadata":{"vulnerabilities":{}}}',
                        '{"metadata":{"vulnerabilities":{"high":0,"critical":0}}}']:
            with self.subTest(payload=payload):
                status, calls, _, _ = self.exercise([(0, payload)])
                self.assertEqual(status, 1)
                self.assertEqual(len(calls), 1)

    def test_inconsistent_counts_and_error_with_counts_fail_closed(self):
        payload = json.loads(report())
        payload['metadata']['vulnerabilities']['total'] = 1
        self.assertEqual(self.exercise([(0, json.dumps(payload))])[0], 1)
        payload = json.loads(report())
        payload['error'] = {'code': 'ETIMEDOUT'}
        self.assertEqual(self.exercise([(0, json.dumps(payload))])[0], 1)
        payload = json.loads(report())
        del payload['auditReportVersion']
        self.assertEqual(self.exercise([(0, json.dumps(payload))])[0], 1)

    def test_nonzero_exit_is_not_converted_to_success_by_zero_counts(self):
        self.assertEqual(self.exercise([(1, report())])[0], 1)

    def test_persistent_transport_failure_is_bounded_and_blocking(self):
        status, calls, _, sleeps = self.exercise([(1, '{"error":{"code":"ECONNRESET"}}')])
        self.assertEqual(status, 1)
        self.assertEqual(len(calls), 3)
        self.assertEqual(sleeps, [1, 2])

    def test_elapsed_budget_stops_without_starting_another_attempt(self):
        status, calls, _, _ = self.exercise([(1, '{"error":{"code":"ETIMEDOUT"}}')], elapsed=60)
        self.assertEqual(status, 1)
        self.assertEqual(len(calls), 1)
        self.assertLessEqual(calls[0][2], 20)

    def test_real_npm_cli_recovers_from_registry_http_errors(self):
        node, npm = shutil.which('node'), shutil.which('npm')
        if not node or not npm:
            self.skipTest('real npm wire test requires Node/npm; the Linux CI entrypoints provide both')
        candidates = [Path(npm).resolve(), Path(node).parent / 'node_modules/npm/bin/npm-cli.js']
        cli = next((path for path in candidates if path.name == 'npm-cli.js' and path.is_file()), None)
        if cli is None:
            self.fail('cannot locate the installed npm CLI for the real producer test')
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            package = {'name': 'audit-wire-fixture', 'version': '1.0.0', 'dependencies': {'is-number': '7.0.0'}}
            (root / 'package.json').write_text(json.dumps(package))
            (root / 'package-lock.json').write_text(json.dumps({
                **package, 'lockfileVersion': 3, 'packages': {'': package,
                'node_modules/is-number': {'version': '7.0.0'}}}))
            (root / 'empty.npmrc').write_text('')
            for status in [429, 503, 'reset', 'timeout']:
                calls = []
                invocations = []
                release = threading.Event()
                class Registry(BaseHTTPRequestHandler):
                    def do_POST(self):
                        calls.append((len(invocations), self.path))
                        self.rfile.read(int(self.headers.get('Content-Length', 0)))
                        first_attempt = len(invocations) == 1
                        # npm 10 may fall back from bulk to quick within the
                        # same invocation. The entire first attempt is faulty;
                        # only a fresh CLI attempt receives a healthy bulk map.
                        if first_attempt and status == 'reset':
                            self.connection.shutdown(socket.SHUT_RDWR)
                            self.connection.close()
                            return
                        if first_attempt and status == 'timeout':
                            release.wait(5)
                            return
                        body = b'{"error":"temporary"}' if first_attempt else b'{}'
                        self.send_response(status if first_attempt else 200)
                        self.send_header('Content-Length', str(len(body)))
                        self.send_header('Content-Type', 'application/json')
                        self.end_headers()
                        self.wfile.write(body)
                    def log_message(self, *args):
                        pass
                registry = ThreadingHTTPServer(('127.0.0.1', 0), Registry)
                thread = threading.Thread(target=registry.serve_forever, daemon=True)
                thread.start()
                def run(command, environment, timeout):
                    invocations.append(command)
                    # Only the external registry/config are replaced. Real npm
                    # produces JSON and the actual wrapper decides recovery.
                    arguments = [f'--registry=http://127.0.0.1:{registry.server_port}'
                                 if arg.startswith('--registry=') else arg for arg in command[1:]]
                    arguments += ['--package-lock-only', f'--cache={root / "cache"}',
                                  f'--userconfig={root / "empty.npmrc"}', f'--globalconfig={root / "global.npmrc"}']
                    if status == 'timeout':
                        arguments += ['--fetch-timeout=200']
                    environment = {key: value for key, value in environment.items() if 'proxy' not in key.lower()}
                    result = subprocess.run([node, str(cli), *arguments], env=environment,
                                            capture_output=True, text=True, timeout=timeout)
                    return result.returncode, result.stdout
                try:
                    self.assertEqual(AUDIT.audit(root, run=run, emit=lambda line: None), 0, f'fault={status}')
                    self.assertEqual(len(invocations), 2, f'{status}: one fresh CLI recovery attempt')
                    self.assertEqual(calls[-1], (2, '/-/npm/v1/security/advisories/bulk'))
                    self.assertTrue(all(path in ('/-/npm/v1/security/advisories/bulk', '/-/npm/v1/security/audits/quick')
                                        for _, path in calls), 'audit fixture never installs or downloads packages')
                finally:
                    release.set()
                    registry.shutdown()
                    registry.server_close()
                    thread.join(timeout=3)

    @unittest.skipIf(os.name == 'nt', 'CI entrypoints run on Linux')
    def test_a_timed_out_attempt_retires_its_descendants(self):
        script = 'import subprocess,sys,time;subprocess.Popen([sys.executable,"-c","import time;time.sleep(60)"]);time.sleep(60)'
        started = time.monotonic()
        status, output = AUDIT.invoke([sys.executable, '-c', script], dict(os.environ), 0.3)
        self.assertEqual(status, 1)
        self.assertEqual(json.loads(output)['error']['code'], 'ETIMEDOUT')
        self.assertLess(time.monotonic() - started, 5, 'a descendant retaining pipes must not hang cleanup')


if __name__ == '__main__':
    unittest.main()

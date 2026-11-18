#!/usr/bin/env python3
"""Run a live high-severity audit with bounded transport recovery, fail closed."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

TRANSIENT = frozenset({
    'ETIMEDOUT', 'ECONNRESET', 'ECONNREFUSED', 'EAI_AGAIN', 'EPIPE',
    'ENETUNREACH', 'EHOSTUNREACH', 'E408', 'E429', 'E500', 'E502', 'E503', 'E504',
})
SEVERITIES = ('info', 'low', 'moderate', 'high', 'critical')


def invoke(command, environment, timeout):
    process = subprocess.Popen(command, env=environment, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, text=True,
                               start_new_session=os.name != 'nt')
    try:
        output, _ = process.communicate(timeout=timeout)
        return process.returncode, output
    except subprocess.TimeoutExpired:
        # Retire only this attempt's process group, including npm descendants.
        if os.name == 'nt':
            process.kill()
        else:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        process.communicate()
        return 1, json.dumps({'error': {'code': 'ETIMEDOUT'}})


def audit(prefix, *, run=invoke, clock=time.monotonic, sleep=time.sleep,
          environment=None, emit=print):
    environment = dict(os.environ if environment is None else environment)
    # Scope this to the audit subprocess. Package cache/install routing and
    # unrelated jobs retain their configuration. Hosted runners without an
    # explicit proxy still connect normally.
    if any(environment.get(k) for k in ('HTTPS_PROXY', 'https_proxy', 'HTTP_PROXY', 'http_proxy', 'ALL_PROXY', 'all_proxy')):
        for key in ('NO_PROXY', 'no_proxy', 'npm_config_noproxy', 'NPM_CONFIG_NOPROXY'):
            environment[key] = 'localhost,127.0.0.1,::1'
    command = ['npm', '--prefix', str(prefix), 'audit', '--json',
               '--registry=https://registry.npmjs.org', '--audit-level=high',
               '--fetch-retries=0', '--fetch-timeout=15000']
    started = clock()
    for attempt in range(1, 4):
        remaining = 60 - (clock() - started)
        if remaining <= 0:
            emit('NPM_AUDIT result=budget_exhausted')
            return 1
        status, output = run(command, environment, min(20, remaining))
        try:
            report = json.loads(output)
            if not isinstance(report, dict):
                raise ValueError('report must be an object')
        except (ValueError, TypeError):
            emit('NPM_AUDIT result=invalid_response')
            return 1
        counts = report.get('metadata', {}).get('vulnerabilities') if isinstance(report.get('metadata'), dict) else None
        if isinstance(counts, dict) and all(type(counts.get(k)) is int and counts[k] >= 0
                                            for k in (*SEVERITIES, 'total')):
            if report.get('auditReportVersion') != 2 or counts['total'] != sum(counts[k] for k in SEVERITIES) or report.get('error'):
                emit('NPM_AUDIT result=invalid_response')
                return 1
            emit(json.dumps({'phase': 'npm-audit', 'attempt': attempt,
                             'seconds': round(clock() - started, 3), 'vulnerabilities': counts}))
            if counts['high'] or counts['critical']:
                emit(json.dumps(report))
                return 1
            return 0 if status == 0 else 1
        error = report.get('error')
        code = error.get('code') if isinstance(error, dict) else None
        retry = isinstance(code, str) and code in TRANSIENT and attempt < 3
        # Do not forward raw network errors, headers, URLs or npm debug logs.
        safe_code = code if isinstance(code, str) and code in TRANSIENT else 'unclassified'
        emit(f'NPM_AUDIT result=transport_error attempt={attempt} code={safe_code} retry={str(retry).lower()}')
        if not retry:
            return 1
        delay = attempt
        if clock() - started + delay >= 60:
            return 1
        sleep(delay)
    return 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--prefix', required=True, type=Path)
    args = parser.parse_args()
    if not (args.prefix / 'package-lock.json').is_file():
        parser.error('a committed package lock is required')
    try:
        return audit(args.prefix)
    except OSError:
        print('NPM_AUDIT result=tool_unavailable')
        return 1


if __name__ == '__main__':
    sys.exit(main())

#!/usr/bin/env python3
"""Opt-in actual store-host recovery with explicit shell card fixtures.

No inference engine is installed. A script writes a partial file, is cancelled
before DB insertion, then is retried with the reviewed package and durable ID.
--checkpoint leaves a bounded private fixture for cmsd/UI integration.
"""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import struct
import subprocess
import tempfile
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pinvou', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--checkpoint', action='store_true')
    args = parser.parse_args()
    binary, evidence = args.pinvou.resolve(), args.evidence.resolve()
    evidence.mkdir(mode=0o700, parents=True, exist_ok=False)
    work = Path(tempfile.mkdtemp(prefix='pin-incomplete-'))
    profile, cmsd_socket = work/'profile', work/'cmsd.sock'
    store = profile/'model-store'
    cards, models, staging = store/'cards', work/'models', work/'staging'
    for directory in (store, cards, models, staging):
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    card = 'qa-incomplete-local'
    directory = cards/card
    directory.mkdir(mode=0o700)
    settings = {'models_dir': str(models), 'staging_dir': str(staging), 'min_free_space_gb': 0}
    (store/'config.json').write_text(json.dumps(settings))
    (store/'config.json').chmod(0o600)
    scripts = {
        'install_engine': '#!/bin/bash\nexit 0\n',
        'download_model': '#!/bin/bash\nset -euo pipefail\nmkdir -p "$LMM_MODELS_DIR/fixture" .runtime\nfile="$LMM_MODELS_DIR/fixture/partial.bin"\n[ -f "$file" ] || printf partial > "$file"\necho invocation >> .runtime/calls\necho LMM_PROGRESS_BYTES=7/11\nwhile [ ! -f .runtime/complete ]; do sleep .1; done\nprintf done >> "$file"\n',
        'deploy': '#!/bin/bash\nprintf \'PORT="19991"\\n\' > .deploy-env\n',
        'enable': '#!/bin/bash\nexit 0\n',
        'stop': '#!/bin/bash\nexit 0\n',
        'uninstall': '#!/bin/bash\nexit 0\n',
    }
    for name, text in scripts.items():
        path = directory/(name+'.sh')
        path.write_text(text)
        path.chmod(0o700)
    (directory/'common.sh').write_text('# Test fixture, not an inference runtime\nMODEL_ID="fixture-model"\n')
    (directory/'card.json').write_text(json.dumps({'id': card, 'name': 'Incomplete installation fixture',
        'engine_name': 'qa-incomplete-fixture', 'size_bytes': 11,
        'package_files': ['common.sh', 'card.json']}))
    env = {**os.environ, 'PINVOU3_HOME': str(profile), 'CMSD_SOCKET': str(cmsd_socket)}
    service_socket = store/'service.sock'
    events = []
    started = time.monotonic()
    host = None

    def record(name, data):
        events.append(name)
        with (evidence/'events.jsonl').open('a') as output:
            output.write(json.dumps({'case': name, 'data': data, 'at': time.time()})+'\n')
        print(name, flush=True)

    class Local(http.client.HTTPConnection):
        def connect(self):
            self.sock = socket.socket(socket.AF_UNIX)
            self.sock.settimeout(15)
            self.sock.connect(str(service_socket))
            peer_pid, peer_uid, _ = struct.unpack('3i', self.sock.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
            assert peer_pid == host.pid and peer_uid == os.geteuid(), 'unexpected store peer'

    def request(method, path, body=None, raw=None):
        connection = Local('localhost', timeout=15)
        try:
            connection.request(method, path, body=raw if raw is not None else None if body is None else json.dumps(body),
                               headers={'Content-Type': 'application/json'})
            response = connection.getresponse()
            data = response.read(16*1024*1024+1)
            assert len(data) <= 16*1024*1024
            return response.status, json.loads(data)
        finally:
            connection.close()

    def rpc(method, body=None):
        status, result = request('POST', '/v1/ui/'+method, body or {})
        assert status == 200, result
        return result['result']

    def start():
        nonlocal host
        with (work/'host.log').open('ab') as output:
            host = subprocess.Popen([str(binary), '--model-store-host'], env=env,
                                    stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
        deadline = time.monotonic()+20
        while time.monotonic() < deadline:
            assert host.poll() is None, 'store host exited; private fixture retained'
            try:
                value = rpc('health')
                assert 'model.install_retry.v1' in value['capabilities']
                return
            except (OSError, http.client.HTTPException):
                time.sleep(.1)
        raise TimeoutError('store host startup')

    def stop():
        if host is not None and host.poll() is None:
            host.send_signal(signal.SIGTERM)
            host.wait(timeout=10)

    def task(run_id):
        status, value = request('GET', '/v1/tasks/'+run_id)
        assert status == 200, value
        return value

    def wait(run_id, predicate, label, seconds=30):
        deadline = time.monotonic()+seconds
        while time.monotonic() < deadline:
            value = task(run_id)
            if predicate(value):
                return value
            time.sleep(.1)
        raise TimeoutError(label)

    try:
        start()
        status, result = request('POST', '/v1/install_local', {'card_id': card})
        assert status == 200 and result['kind'] == 'install_local'
        original = result['run_id']
        wait(original, lambda value: value['progress'] is not None, 'actual partial file')
        assert request('POST', '/v1/tasks/'+original+'/cancel', {})[0] == 200
        cancelled = wait(original, lambda value: value['status'] == 'cancelled', 'cancellation')
        partial = models/'fixture/partial.bin'
        assert partial.read_bytes() == b'partial'
        library = rpc('library')
        assert not library['models']
        assert any(item['runId'] == original for item in library['incompleteInstallations'])
        preview = rpc('incomplete_preview', {'run_id': original})
        record('real-script-cancelled-before-db-and-persistent-preview-available', {'run_id': original, 'kind': cancelled['kind']})

        if args.checkpoint:
            ready = {'pinvou': str(binary), 'host_pid': host.pid, 'profile': str(profile),
                'store_socket': str(service_socket), 'cmsd_socket': str(cmsd_socket), 'store_root': str(store),
                'cards_dir': str(cards), 'models_dir': str(models), 'card_id': card, 'original_run_id': original,
                'preview_sha256': preview['previewSha256'], 'package_sha256': preview['packageSha256'],
                'complete_flag': str(directory/'.runtime/complete'), 'release_file': str(evidence/'release')}
            path = evidence/'ready.json'
            path.write_text(json.dumps(ready, indent=2)+'\n')
            path.chmod(0o600)
            record('FIXTURE_CHECKPOINT', {'configuration': str(path)})
            deadline = time.monotonic()+600
            while not (evidence/'release').exists():
                if time.monotonic() > deadline:
                    raise TimeoutError('bounded external integration checkpoint')
                time.sleep(.2)
            (evidence/'fixture-report.json').write_text(json.dumps({'fixture_ready': True, 'released': True,
                'scope': 'Actual store host and partial/cancel fixture only; external integration results are separate',
                'private_work_retained': str(work)}, indent=2)+'\n')
            return

        body = {'original_run_id': original, 'expected_preview_sha256': preview['previewSha256'], 'run_id': 'cmsd-fixture-retry'}
        extra = {**body, 'confirmed': True}
        assert request('POST', '/v1/retry_install', extra)[0] == 400
        duplicate = json.dumps(body)[:-1]+',"original_run_id":'+json.dumps(original)+'}'
        assert request('POST', '/v1/retry_install', raw=duplicate)[0] == 400
        (directory/'.runtime/complete').write_text('complete')
        status, accepted = request('POST', '/v1/retry_install', body)
        assert status == 200 and accepted == {'run_id': body['run_id'], 'kind': 'install_local', 'card': card}, accepted
        assert request('POST', '/v1/retry_install', body) == (200, accepted)
        assert request('POST', '/v1/retry_install', {**body, 'expected_preview_sha256': '0'*64})[0] == 409
        assert request('POST', '/v1/retry_install', {**body, 'original_run_id': 'other-original'})[0] == 409
        wait(body['run_id'], lambda value: value['status'] == 'succeeded', 'verified retry')
        assert partial.read_bytes() == b'partialdone'
        library = rpc('library')
        assert next(item for item in library['models'] if item['card_id'] == card)['status'] == 'deployed'
        assert not library['incompleteInstallations']
        calls = (directory/'.runtime/calls').read_bytes()
        assert calls == b'invocation\ninvocation\n'
        record('reviewed-retry-preserves-partial-and-exact-idempotent-correlation', accepted)
        stop()
        start()
        assert request('POST', '/v1/retry_install', body) == (200, accepted)
        assert (directory/'.runtime/calls').read_bytes() == calls
        record('actual-host-restart-does-not-execute-again', {'status': task(body['run_id'])['status']})
        (evidence/'report.json').write_text(json.dumps({'passed': True, 'seconds': time.monotonic()-started,
            'cases': events, 'pinvou_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
            'scope': 'Actual production store host, Unix peer identity and explicit shell fixture; no cmsd parent admission, inference engine, model inference or native desktop input'}, indent=2)+'\n')
    finally:
        if host is not None and host.poll() is None:
            try:
                _, listing = request('GET', '/v1/tasks')
                for item in listing.get('tasks', []):
                    if item['status'] == 'running':
                        request('POST', '/v1/tasks/'+item['run_id']+'/cancel', {})
                        wait(item['run_id'], lambda value: value['status'] != 'running', 'fixture cleanup', seconds=10)
            finally:
                stop()
        if (evidence/'report.json').exists():
            shutil.rmtree(work)
        else:
            (evidence/'retained-work.txt').write_text(str(work)+'\n')


if __name__ == '__main__':
    main()

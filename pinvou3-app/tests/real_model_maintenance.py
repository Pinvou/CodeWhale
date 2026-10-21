#!/usr/bin/env python3
"""Opt-in, isolated Pinvou host -> real cmsd -> real CPU model acceptance.

Requires existing weights, CPU server and Docker image; no downloads or changes
to existing containers/services. Faults affect only processes created here.
The supplied CPU server must listen on 8080 and read /qa/model.
"""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import shlex
import shutil
import signal
import socket
import sqlite3
import subprocess
import tempfile
import time


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.host)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['pinvou-binary', 'cmsd-binary', 'model-dir', 'cpu-server', 'evidence']:
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--image', required=True)
    parser.add_argument('--model-port', type=int, default=18182)
    parser.add_argument('--gateway-port', type=int, default=18882)
    parser.add_argument('--ui-checkpoint', type=Path, help='pause a real interrupted reservation until this continuation file exists (120s bound)')
    args = parser.parse_args()
    image_id = subprocess.check_output(
        ['docker', 'image', 'inspect', args.image, '--format', '{{.Id}}'],
        text=True, timeout=15,
    ).strip()  # Inspect cannot pull; create below explicitly prohibits pulling.
    evidence = args.evidence.resolve()
    evidence.mkdir(mode=0o700)
    # UNIX socket paths must fit sockaddr_un. Keep the profile short and record
    # its location; never reuse another acceptance profile.
    root = Path(tempfile.mkdtemp(prefix='pin-maint-'))
    (evidence / 'profile-path.txt').write_text(str(root) + '\n')
    card_id = 'qa-maintenance-cpu'
    name = f'pinvou-maintenance-{os.getpid()}'
    profile = root / 'p'
    store = profile / 'model-store'
    card = store / 'cards' / card_id
    model = store / 'models' / 'maintenance-cpu'
    card.mkdir(parents=True)
    for port in [args.model_port, args.gateway_port]:
        with socket.socket() as probe:
            probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            probe.bind(('127.0.0.1', port))
    cmsd_socket = root / 'cmsd.sock'
    store_socket = store / 'service.sock'
    env = {**os.environ, 'PINVOU3_HOME': str(profile), 'CMSD_SOCKET': str(cmsd_socket)}
    config = root / 'cmsd.toml'
    config.write_text('\n'.join(f'{k} = {json.dumps(str(v))}' for k, v in {
        'socket_path': cmsd_socket, 'state_dir': root / 'state',
        'gateway_listen': f'127.0.0.1:{args.gateway_port}', 'systemd_mode': 'user',
        'cards_dir': store / 'cards', 'unit_dir': root / 'units',
        'engine_root': root / 'engines', 'models_dir': store / 'models',
        'staging_dir': root / 'staging', 'store_socket_path': store_socket,
    }.items()) + '\n')
    env['CMSD_CONFIG'] = str(config)
    frozen = {}
    for key in ['pinvou', 'cmsd']:
        source = getattr(args, key + '_binary').resolve()
        frozen[key] = evidence / (key + '-tested')
        shutil.copy2(source, frozen[key])
        frozen[key].chmod(0o700)
    source_weights = sorted(args.model_dir.resolve().glob('*.safetensors'))
    assert source_weights, 'real safetensors required'
    source_checksums = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in source_weights}
    common = (f'IMAGE={shlex.quote(args.image)}\nCONTAINER_NAME={shlex.quote(name)}\n'
              'MODEL_ID="SmolLM2-135M-Instruct-qa-cpu"\n'
              f'MODEL_DIR={shlex.quote(str(model))}\n'
              'LMM_BIG_FILES=(' + ' '.join(shlex.quote(f'{p.name}:{p.stat().st_size}') for p in source_weights) + ')\n'
              'LMM_SMALL_FILES=("config.json")\n')
    (card / 'common.sh').write_text(common)
    deploy_env = common + f'PORT={args.model_port}\n'
    (card / 'card.json').write_text(json.dumps({
        'name': 'QA Maintenance CPU', 'engine_name': 'sglang', 'engine_version': 'qa-maintenance',
        'source_repo_id': 'qa/maintenance-cpu', 'size_bytes': sum(p.stat().st_size for p in source_weights),
        'package_files': ['common.sh', 'card.json'],
    }))
    source = shlex.quote(str(args.model_dir.resolve()))
    target = shlex.quote(str(model))
    fixture = shlex.quote(str(root))
    scripts = {
        'install_engine': 'echo LMM_ENGINE_INSTALLED=1\necho LMM_NEED_ROOT=0\n',
        'download_model': f'mkdir -p {target}\ncp -a --reflink=auto {source}/. {target}/\n',
        'deploy': (f'if [ -f {fixture}/fail-deploy ]; then echo QA_DEPLOY_FAILURE; exit 19; fi\n'
                   f'printf %s {shlex.quote(deploy_env)} > {shlex.quote(str(card / ".deploy-env"))}\n'
                   f'echo deployed > {fixture}/deployment\n'),
        'enable': f'docker start {shlex.quote(name)}\n',
        'stop': f'docker stop -t 5 {shlex.quote(name)}\n',
        'uninstall': (f'echo started >> {fixture}/uninstall-count\n'
                      f'if [ -f {fixture}/pause ]; then echo MAINTENANCE_SCRIPT_READY; sleep 120; fi\n'
                      f'if docker inspect {shlex.quote(name)} >/dev/null 2>&1; then docker rm {shlex.quote(name)}; fi\n'
                      f'rm -f {fixture}/deployment\n'
                      f'rm -f {shlex.quote(str(card / ".deploy-env"))}\n'
                      f'if [ "${{LMM_PURGE_WEIGHTS:-0}}" = 1 ]; then rm -rf -- {target}; fi\n'),
    }
    for key, text in scripts.items():
        (card / (key + '.sh')).write_text('#!/bin/bash\nset -euo pipefail\n' + text)
    log = (evidence / 'processes.log').open('ab', buffering=0)
    processes = {'cmsd': None, 'pinvou': None}
    report = {'scope': 'same-host real Pinvou background host + cmsd + CPU inference; no physical multi-node claim',
              'image_id': image_id,
              'profile': str(root), 'checks': [], 'errors': [], 'binary_sha256': {
                  key: hashlib.sha256(value.read_bytes()).hexdigest() for key, value in frozen.items()}}
    started = time.monotonic()

    def api(path, body=None, host='cmsd', method=None):
        connection = UnixHTTP(str(cmsd_socket if host == 'cmsd' else store_socket), timeout=15)
        try:
            connection.request(method or ('GET' if body is None else 'POST'), path,
                               None if body is None else json.dumps(body), {'Content-Type': 'application/json'})
            response = connection.getresponse()
            return response.status, json.loads(response.read())
        finally:
            connection.close()

    def wait(check, timeout=45):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            value = check()
            if value:
                return value
            time.sleep(.15)
        raise AssertionError('timed out waiting for fixture state')

    def start(key):
        processes[key] = subprocess.Popen([str(frozen[key]), *(['--model-store-host'] if key == 'pinvou' else [])],
                                          env=env, stdout=log, stderr=log)
        def healthy():
            assert processes[key].poll() is None, f'{key} exited'
            try:
                status, value = api('/v1/status' if key == 'cmsd' else '/v1/ui/health',
                                    None if key == 'cmsd' else {}, key)
                if key == 'pinvou' and status == 200:
                    assert 'model.uninstall_reservation' in value['result']['capabilities']
                return status == 200
            except (OSError, http.client.HTTPException):
                return False
        wait(healthy)

    def kill(key):
        if processes[key] and processes[key].poll() is None:
            processes[key].kill()
            processes[key].wait(timeout=10)

    def begin(action, purge=False):
        status, value = api('/v1/ui/lifecycle', {'card_id': card_id, 'action': action, 'purge_weights': purge}, 'pinvou')
        assert status == 200, value
        return value['result']['run_id']

    def finish(run_id):
        def done():
            status, value = api('/v1/tasks/' + run_id, host='pinvou')
            assert status == 200, value
            return value if value['status'] != 'running' else None
        return wait(done, 90)

    def success(action, purge=False):
        result = finish(begin(action, purge))
        assert result['status'] == 'succeeded', result
        return result

    def record():
        return json.loads((store / 'maintenance' / (card_id + '.json')).read_text())

    def gate():
        status, state = api(f'/v1/models/{card_id}/maintenance')
        assert status == 200, state
        assert record()['owner'] not in json.dumps(state), 'private owner exposed by daemon'
        return state

    def count():
        return len((root / 'uninstall-count').read_text().splitlines())

    def wait_script(run_id):
        def ready():
            current = api('/v1/tasks/' + run_id, host='pinvou')[1]
            assert current['status'] == 'running', current
            return any('MAINTENANCE_SCRIPT_READY' in line for line in current['lines'])
        wait(ready)
        saved = record()
        assert (store / 'maintenance' / (card_id + '.json')).stat().st_mode & 0o777 == 0o600
        assert (store / 'maintenance').stat().st_mode & 0o777 == 0o700
        assert saved['owner'] not in json.dumps(api('/v1/tasks/' + run_id, host='pinvou')[1])
        status, library = api('/v1/ui/library', {}, 'pinvou')
        assert status == 200, library
        summaries = library['result']['maintenanceTransactions']
        assert len(summaries) == 1 and summaries[0]['workerActive'] is True
        assert saved['owner'] not in json.dumps(library), 'private owner exposed by library'

    created = False
    try:
        start('cmsd')
        start('pinvou')
        (root / 'fail-deploy').touch()
        status, value = api('/v1/install_local', {'card_id': card_id}, 'pinvou')
        assert status == 200, value
        installed = finish(value['run_id'])
        assert installed['status'] == 'failed' and '19' in installed['error'], installed
        observed = next(m for m in api('/v1/models')[1]['models'] if m['card'] == card_id)
        assert not observed['managed'] and not observed['can_adopt'] and observed['can_reserve_uninstall'], observed
        rejected_status,rejected=api(f'/v1/models/{card_id}/disable',{})
        assert rejected_status in (403,404,409) and 'error' in rejected and 'task_id' not in rejected,(rejected_status,rejected)
        assert not (profile / 'megacube/installations/ownership' / card_id).exists()
        (root / 'pause').touch()
        partial_run = begin('uninstall', False)
        wait_script(partial_run)
        assert record()['reservation_only'] is True and gate()['reservation_only'] is True
        api('/v1/tasks/' + partial_run + '/cancel', {}, 'pinvou')
        assert finish(partial_run)['status'] == 'cancelled'
        before = count()
        if args.ui_checkpoint:
            (evidence / 'ui-checkpoint.json').write_text(json.dumps({'profile':str(root),'card':card_id,'script_count':before}))
            wait(lambda: args.ui_checkpoint.exists(),120)
        success('recover_maintenance')
        assert count() == before and model.exists() and not gate()['maintenance']
        observed = next(m for m in api('/v1/models')[1]['models'] if m['card'] == card_id)
        assert not observed['managed'], observed
        (root / 'pause').unlink()
        success('uninstall',False)
        assert model.exists() and not gate()['maintenance']
        report['checks'].append('failed deploy leaves downloaded row; reservation cancel/recovery and cleanup never grant lifecycle authority or rerun scripts')
        (root / 'fail-deploy').unlink()
        status, value = api('/v1/install_local', {'card_id':card_id},'pinvou')
        assert status==200,value
        installed = finish(value['run_id'])
        assert installed['status']=='succeeded',installed
        observed = next(m for m in api('/v1/models')[1]['models'] if m['card']==card_id)
        assert not observed['managed'] and observed['can_adopt'],observed
        success('uninstall',False)
        assert model.exists() and not gate()['maintenance']
        report['checks'].append('never-enabled deployed card can uninstall with weights retained without first enabling')
        status,value=api('/v1/install_local',{'card_id':card_id},'pinvou')
        assert status==200,value
        installed=finish(value['run_id'])
        assert installed['status'] == 'succeeded', installed
        subprocess.run(['docker', 'create', '--pull=never', '--name', name, '--label', 'pinvou.acceptance=maintenance',
                        '--memory', '2g', '--cpus', '2', '--restart=no',
                        '-p', f'127.0.0.1:{args.model_port}:8080', '-v', f'{model}:/qa/model:ro',
                        '-v', f'{args.cpu_server.resolve()}:/qa/server.py:ro', '--entrypoint', 'python3',
                        args.image, '/qa/server.py'], check=True, capture_output=True, timeout=45)
        created = True
        success('enable')
        assert (profile / 'megacube/installations/ownership' / card_id).is_file()
        observed=next(m for m in api('/v1/models')[1]['models'] if m['card']==card_id)
        assert observed['managed'] and observed['recovery_owner']=='cmsd',observed
        connection = http.client.HTTPConnection('127.0.0.1', args.gateway_port, timeout=40)
        connection.request('POST', '/v1/chat/completions', json.dumps({
            'model': 'megacube-default', 'messages': [{'role': 'user', 'content': 'Say hello.'}],
            'max_tokens': 12, 'temperature': 0}), {'Content-Type': 'application/json'})
        response = connection.getresponse()
        value = json.loads(response.read())
        connection.close()
        assert response.status == 200 and value['choices'][0]['message']['content'], value
        report['sample_response'] = value['choices'][0]['message']['content']
        success('stop')
        report['checks'].append('local card install -> UI lifecycle enable -> real CPU inference -> UI stop')

        (root / 'pause').touch()
        run_id = begin('uninstall', True)
        wait_script(run_id)
        assert gate()['maintenance']
        for action in ['enable', 'install', 'disable', 'purge', 'set-default']:
            assert api(f'/v1/models/{card_id}/{action}', {})[0] == 409, action
        assert api('/v1/component-maintenance', {'action': 'begin', 'owner': 'unrelated-client'})[0] == 409
        assert api('/v1/ui/lifecycle', {'card_id': card_id, 'action': 'enable'}, 'pinvou')[0] == 409
        api('/v1/tasks/' + run_id + '/cancel', {}, 'pinvou')
        assert finish(run_id)['status'] == 'cancelled'
        assert gate()['maintenance'] and record()['phase'] == 'recovery_required'
        before = count()
        success('recover_maintenance')
        assert count() == before and not gate()['maintenance'] and model.exists()
        report['checks'].append('concurrent admissions rejected; cancel retains gate; direct recovery never reruns delete')

        run_id = begin('uninstall', True)
        wait_script(run_id)
        before = count()
        kill('pinvou')
        kill('cmsd')
        start('cmsd')
        assert gate()['maintenance'] and gate()['recovery_required']
        start('pinvou')
        original = finish(run_id)
        assert original['status'] == 'failed' and '未自动重跑卸载' in original['error'], original
        success('recover_maintenance')
        assert count() == before and not gate()['maintenance'] and model.exists()
        report['checks'].append('SIGKILL host and daemon -> persisted gate/receipt -> orphan cleanup -> explicit recovery')

        (root / 'pause').unlink()
        success('uninstall', False)
        assert not gate()['maintenance'] and model.exists()
        with sqlite3.connect(store / 'models.db') as db:
            row = db.execute('SELECT status,running FROM store_models WHERE card_id=?', (card_id,)).fetchone()
        assert row == ('downloaded', 0), row
        report['checks'].append('normal keep-weights uninstall commits downloaded/stopped state before releasing gate')
        success('uninstall', True)
        assert not gate()['maintenance'] and not model.exists() and not card.exists()
        with sqlite3.connect(store / 'models.db') as db:
            assert db.execute('SELECT count(*) FROM store_models WHERE card_id=?', (card_id,)).fetchone()[0] == 0
        assert record()['phase'] == 'completed'
        status, library = api('/v1/ui/library', {}, 'pinvou')
        assert status == 200 and not library['result']['maintenanceTransactions'], library
        report['checks'].append('normal purge removes owned weights/card/database row before releasing gate')
        assert source_checksums == {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in source_weights}
        report['checks'].append('original source weight checksums unchanged')
    except Exception as error:
        report['errors'].append(repr(error))
        raise
    finally:
        for key in processes:
            kill(key)
        # A failed assertion must not leave this fixture's destructive script
        # alive. Verify its private process identity before signalling its group.
        for identity_file in (store / 'maintenance' / 'executions').glob('*/process.json'):
            identity = json.loads(identity_file.read_text())
            if identity['boot_id'] != Path('/proc/sys/kernel/random/boot_id').read_text().strip():
                continue
            for process_path in Path('/proc').iterdir():
                if not process_path.name.isdecimal():
                    continue
                try:
                    fields = (process_path / 'stat').read_text().rsplit(')', 1)[1].split()
                    if int(fields[2]) == identity['pgid'] and int(fields[19]) >= identity['starttime'] and fields[0] not in ('Z', 'X'):
                        os.killpg(identity['pgid'], signal.SIGKILL)
                        break
                except (FileNotFoundError, ProcessLookupError):
                    pass
        if created:
            subprocess.run(['docker', 'rm', '-f', name], capture_output=True, timeout=30)
        report['elapsed_seconds'] = time.monotonic() - started
        (evidence / 'report.json').write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
        log.close()
        print(json.dumps(report, ensure_ascii=False))


if __name__ == '__main__':
    main()

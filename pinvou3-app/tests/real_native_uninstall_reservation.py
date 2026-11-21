#!/usr/bin/env python3
"""Real Pinvou/cmsd cleanup of an unenabled native standard-shaped fixture.

The engine and weights are tiny local fixtures: no native inference is claimed.
No unit is installed or enabled. Tests never grant permission to stop a service.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import socket
import sqlite3
import subprocess
import tempfile
import time

from real_model_maintenance import UnixHTTP


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['pinvou-binary', 'cmsd-binary', 'evidence']:
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--gateway-port', type=int, default=18985)
    parser.add_argument('--legacy-cmsd-binary',type=Path,help='also verify an old daemon is rejected before native purge scripts run')
    args = parser.parse_args()
    evidence = args.evidence.resolve()
    evidence.mkdir(mode=0o700)
    root = Path(tempfile.mkdtemp(prefix='pin-native-cleanup-'))
    (evidence / 'profile-path.txt').write_text(str(root) + '\n')
    profile = root / 'p'
    store = profile / 'model-store'
    card_id = f'qa-native-reservation-{os.getpid()}'
    card = store / 'cards' / card_id
    model = store / 'models' / 'fixture-native'
    card.mkdir(parents=True)
    model.mkdir(parents=True)
    engine = root / 'engines' / 'fixture'
    engine.mkdir(parents=True)
    engine_bytes = b'#!/bin/sh\n# fixture only; never a real inference engine\nexit 97\n'
    (engine / 'tachyon-serve').write_bytes(engine_bytes)
    (engine / 'tachyon-serve').chmod(0o700)
    artifact = 'fixture.tachyon'
    weights = b'local native cleanup fixture\n' * 1024
    (root / artifact).write_bytes(weights)
    common = (f'ENGINE_BIN_SHA256="{hashlib.sha256(engine_bytes).hexdigest()}"\n'
              f'MODEL_ID="Native-reservation-fixture"\nARTIFACT="{artifact}"\nTOTAL_BYTES="{len(weights)}"\n')
    (card / 'common.sh').write_text(common)
    (card / 'card.json').write_text(json.dumps({
        'name':'QA Native Reservation', 'engine_name':'tachyon', 'engine_version':'qa-fixture',
        'source_repo_id':'qa/fixture-native', 'size_bytes':len(weights), 'package_files':['card.json','common.sh'],
    }))
    deployment = '\n'.join(f'{key}="{value}"' for key, value in {
        'PORT':18986,'HOST':'127.0.0.1','MODEL_DIR':model,'ENGINE_DIR':engine,
        'ARTIFACT':artifact,'MODEL_ID':'Native-reservation-fixture','MAX_CONCURRENCY':1,
    }.items()) + '\n'
    scripts = {
        'install_engine':'echo LMM_ENGINE_INSTALLED=1\necho LMM_NEED_ROOT=0\n',
        'download_model':f'cp -- {shlex.quote(str(root / artifact))} {shlex.quote(str(model / artifact))}\n',
        'deploy':f'printf %s {shlex.quote(deployment)} > {shlex.quote(str(card / ".deploy-env"))}\n',
        'enable':f'touch {shlex.quote(str(root / "unexpected-enable"))}\nexit 97\n',
        'stop':f'touch {shlex.quote(str(root / "unexpected-stop"))}\nexit 98\n',
        'uninstall':(f'echo ran >> {shlex.quote(str(root / "uninstall-count"))}\n'
                     f'if [ "${{LMM_PURGE_WEIGHTS:-0}}" = 1 ]; then rm -f -- {shlex.quote(str(model / artifact))}; fi\n'
                     f'rm -f -- {shlex.quote(str(card / ".deploy-env"))}\n'),
    }
    for key, body in scripts.items():
        (card / (key + '.sh')).write_text('#!/bin/bash\nset -euo pipefail\n' + body)
    metadata = (card/'card.json').read_text()
    config = root / 'cmsd.toml'
    sock = root / 'cmsd.sock'
    config.write_text('\n'.join(f'{key}={json.dumps(str(value))}' for key, value in {
        'socket_path':sock,'state_dir':root/'state','gateway_listen':f'127.0.0.1:{args.gateway_port}',
        'systemd_mode':'user','cards_dir':store/'cards','unit_dir':root/'units','engine_root':root/'engines',
        'models_dir':store/'models','staging_dir':root/'staging','store_socket_path':store/'service.sock',
    }.items()) + '\n')
    with socket.socket() as probe:
        probe.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
        probe.bind(('127.0.0.1',args.gateway_port))
    env = {**os.environ,'PINVOU3_HOME':str(profile),'CMSD_SOCKET':str(sock),'CMSD_CONFIG':str(config)}
    frozen = {}
    for name in ['pinvou','cmsd']:
        frozen[name] = evidence / (name + '-tested')
        shutil.copy2(getattr(args,name+'_binary').resolve(),frozen[name])
        frozen[name].chmod(0o700)
    if args.legacy_cmsd_binary:
        frozen['legacy_cmsd']=evidence/'legacy-cmsd-tested'
        shutil.copy2(args.legacy_cmsd_binary.resolve(),frozen['legacy_cmsd'])
        frozen['legacy_cmsd'].chmod(0o700)
    log = (evidence / 'processes.log').open('ab',buffering=0)
    processes = []
    report = {'scope':'real host and daemon; native standard-shaped fixture, no native inference or service activation',
              'checks':[],'errors':[],'binary_sha256':{k:hashlib.sha256(p.read_bytes()).hexdigest() for k,p in frozen.items()}}
    started = time.monotonic()

    def api(path,body=None,host=False,allow_pending=False):
        connection=UnixHTTP(str(store/'service.sock' if host else sock),timeout=15)
        try:
            connection.request('GET' if body is None else 'POST',path,None if body is None else json.dumps(body))
            response=connection.getresponse();value=json.loads(response.read())
            if (allow_pending and response.status==503
                    and value.get('error',{}).get('code')=='store_selection_unavailable'):
                return None
            assert response.status==200,value
            return value
        finally:connection.close()

    def wait(check,timeout=45):
        deadline=time.monotonic()+timeout
        while time.monotonic()<deadline:
            assert all(p.poll() is None for p in processes),'owned process exited'
            value=check()
            if value:return value
            time.sleep(.1)
        raise AssertionError('native fixture deadline')

    def healthy(path,body=None,host=False):
        try:return api(path,body,host)
        except (OSError,ValueError):return False

    def observed_model():
        # Enrollment is durable before the supervisor publishes its first snapshot.
        value=api('/v1/models',allow_pending=True)
        return next((row for row in (value or {}).get('models',[]) if row['card']==card_id),None)

    def done(run):
        value=api('/v1/tasks/'+run,host=True)
        return value if value['status']!='running' else None

    def lifecycle(action,purge=False,expected='succeeded'):
        run=api('/v1/ui/lifecycle',{'card_id':card_id,'action':action,'purge_weights':purge},True)['result']['run_id']
        value=wait(lambda:done(run));assert value['status']==expected,value
        return value

    def install():
        run=api('/v1/install_local',{'card_id':card_id},True)['run_id']
        value=wait(lambda:done(run));assert value['status']=='succeeded',value

    try:
        processes.append(subprocess.Popen([str(frozen['cmsd'])],env=env,stdout=log,stderr=log))
        wait(lambda:healthy('/v1/status'))
        processes.append(subprocess.Popen([str(frozen['pinvou']),'--model-store-host'],env=env,stdout=log,stderr=log))
        wait(lambda:healthy('/v1/ui/health',{},True))
        install()
        row=wait(observed_model)
        assert not row['managed'] and row['can_adopt'] and row['can_reserve_uninstall'],row
        lifecycle('stop',expected='failed')
        assert not (root/'unexpected-stop').exists()
        shared=model/'unrelated-user-file.txt';shared.write_text('must survive metadata purge\n')
        lifecycle('uninstall',False)
        assert (model/artifact).read_bytes()==weights and shared.is_file()
        journal=json.loads((store/'maintenance'/f'{card_id}.json').read_text())
        assert journal['phase']=='completed' and journal['reservation_only']
        with sqlite3.connect(store/'models.db') as database:
            assert database.execute('SELECT status,running FROM store_models WHERE card_id=?',(card_id,)).fetchone()==('downloaded',0)
        report['checks'].append('never-enabled native fixture: ordinary stop denied; reserved uninstall keeps artifact and downloaded record')
        run=api('/v1/ui/deploy',{'card_id':card_id,'profile_id':None},True)['result']['run_id']
        deployed=wait(lambda:done(run));assert deployed['status']=='succeeded',deployed
        lifecycle('uninstall',True)
        assert not (model/artifact).exists() and shared.read_text()=='must survive metadata purge\n'
        assert not card.exists()
        assert api(f'/v1/models/{card_id}/maintenance')['maintenance'] is False
        assert (root/artifact).read_bytes()==weights and (engine/'tachyon-serve').read_bytes()==engine_bytes
        assert not (root/'unexpected-enable').exists() and not (root/'unexpected-stop').exists()
        report['checks'].append('scoped native purge leaves unrelated file, shared engine and source untouched; only declared artifact deleted by store script')
        if args.legacy_cmsd_binary:
            card.mkdir()
            (card/'common.sh').write_text(common)
            (card/'card.json').write_text(metadata)
            for key,body in scripts.items():
                (card/(key+'.sh')).write_text('#!/bin/bash\nset -euo pipefail\n'+body)
            install()
            before=(root/'uninstall-count').read_text()
            processes[0].terminate();processes[0].wait(timeout=10)
            processes[0]=subprocess.Popen([str(frozen['legacy_cmsd'])],env=env,stdout=log,stderr=log)
            wait(lambda:healthy('/v1/status'))
            rejected=lifecycle('uninstall',True,expected='failed')
            assert 'maintenance_unsupported' in rejected['error'],rejected
            assert (root/'uninstall-count').read_text()==before
            assert (model/artifact).read_bytes()==weights and shared.is_file()
            report['checks'].append('old real daemon missing reservation capability rejects native purge before any uninstall script or weight deletion')
    except Exception as error:
        report['errors'].append(repr(error));raise
    finally:
        for process in processes:
            if process.poll() is None:process.terminate()
        for process in processes:
            try:process.wait(timeout=10)
            except subprocess.TimeoutExpired:process.kill();process.wait()
        report['elapsed_seconds']=time.monotonic()-started
        (evidence/'report.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
        log.close();print(json.dumps(report,ensure_ascii=False))


if __name__=='__main__':main()

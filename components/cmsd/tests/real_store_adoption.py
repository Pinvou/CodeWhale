#!/usr/bin/env python3
"""Opt-in GB10 single-host acceptance: actual Pinvou SQLite inventory -> cmsd ->
Docker CPU model -> real generation. Uses an isolated profile/state/socket and
one private container; preserves all pre-existing daemons, containers and weights.
"""
import argparse, hashlib, http.client, json, os, signal, socket, sqlite3, subprocess, tempfile, time, urllib.request
from pathlib import Path

class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock=socket.socket(socket.AF_UNIX);self.sock.settimeout(self.timeout);self.sock.connect(self.host)

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ('binary','pinvou','weights','cpu-server','evidence'):parser.add_argument('--'+name,type=Path,required=True)
    args=parser.parse_args();out=args.evidence.resolve();out.mkdir(mode=0o700,parents=True,exist_ok=False)
    report={'scope':__doc__,'checks':[],'generations':[],'errors':[],'hashes':{k:hashlib.sha256(getattr(args,k).read_bytes()).hexdigest() for k in ['binary','pinvou']}}
    begin=time.monotonic();work=tempfile.TemporaryDirectory(prefix='cmsd-adopt-',dir='/run/user/'+str(os.geteuid()));root=Path(work.name);profile=root/'profile';store=profile/'model-store';cards=store/'cards';cards.mkdir(parents=True,mode=0o700)
    state=root/'state';state.mkdir(mode=0o700);sock=root/'cmsd.sock';nonce=root.name;container='qa-'+nonce;card='store-model--version1';logical='store-model';model='SmolLM2-StoreAdoption';process=None;logs=[];owned_containers=[container]
    def command(*argv):return subprocess.check_output([str(x) for x in argv],text=True,stderr=subprocess.STDOUT).strip()
    protected=command('systemctl','--user','show','cmsd.service','--property=MainPID','--property=ExecMainStartTimestampMonotonic')
    containers=command('docker','ps','-aq','--no-trunc').splitlines()
    def port():
        with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
    model_port=port();gateway_port=port()
    def api(method,path,body=None):
        conn=UnixHTTP(str(sock),timeout=15)
        try:
            conn.request(method,path,json.dumps(body) if body is not None else None,{'content-type':'application/json'});res=conn.getresponse();v=json.loads(res.read());assert res.status==200,(res.status,v);return v
        finally:conn.close()
    def wait(fn,timeout=90):
        deadline=time.monotonic()+timeout;last=None
        while time.monotonic()<deadline:
            try:
                result=fn()
                if result:return result
            except (OSError,http.client.HTTPException) as e:last=str(e)
            time.sleep(.1)
        raise TimeoutError(last or 'condition not met')
    def row():return next(x for x in api('GET','/v1/models')['models'] if x['card']==card)
    def infer(label,gateway=False):
        request=urllib.request.Request(f'http://127.0.0.1:{gateway_port if gateway else model_port}/v1/chat/completions',data=json.dumps({'model':model,'messages':[{'role':'user','content':'Say hello in one sentence.'}],'max_tokens':20,'temperature':0}).encode(),headers={'content-type':'application/json'})
        t=time.monotonic()
        with urllib.request.urlopen(request,timeout=60) as response:v=json.load(response)
        assert v['choices'][0]['message']['content'];report['generations'].append({'case':label,'text':v['choices'][0]['message']['content'],'seconds':round(time.monotonic()-t,3)})
    config=root/'cmsd.toml';settings={'state_dir':str(state),'socket_path':str(sock),'gateway_listen':f'127.0.0.1:{gateway_port}','systemd_mode':'user','native_unit_namespace':'state','cards_dir':str(cards),'models_dir':str(args.weights.resolve().parent),'engine_root':str(root/'engines'),'unit_dir':str(root/'units'),'store_socket_path':str(store/'service.sock'),'store_inventory_program':str(args.pinvou.resolve()),'store_inventory_profile':str(profile)}
    def write_config():config.write_text(''.join(k+' = '+json.dumps(v)+'\n' for k,v in settings.items()));config.chmod(0o600)
    def launch():
        nonlocal process
        log=(out/f'daemon-{len(logs)}.log').open('wb');logs.append(log);env={k:v for k,v in os.environ.items() if not k.startswith('CMSD_')};env['CMSD_CONFIG']=str(config)
        process=subprocess.Popen([str(args.binary.resolve())],env=env,stdout=log,stderr=log,start_new_session=True)
        wait(lambda: process.poll() is None and sock.exists() and api('GET','/v1/status')['service']=='cmsd',15)
    def stop():
        nonlocal process
        if process and process.poll() is None:os.killpg(process.pid,signal.SIGKILL);process.wait(timeout=8)
    def action(action):
        v=api('POST',f'/v1/models/{card}/{action}')
        if 'task_id' in v:
            t=wait(lambda:(t if t['status'] in ('succeeded','failed','cancelled') else None) if (t:=api('GET','/v1/tasks/'+v['task_id'])) else None)
            assert t['status']=='succeeded',t
        return v
    try:
        c=cards/card;c.mkdir(mode=0o700);weights=args.weights.resolve();files=sorted(weights.glob('*.safetensors'));assert files
        common=f'IMAGE="lmsysorg/sglang:v0.5.19"\nCONTAINER_NAME="{container}"\nMODEL_ID="{model}"\nCMSD_RESERVED_GIB="1.5"\nLMM_BIG_FILES=('+ ' '.join(json.dumps(p.name+':'+str(p.stat().st_size)) for p in files)+')\nLMM_SMALL_FILES=("config.json")\n'
        (c/'common.sh').write_text(common);(c/'common.sh').chmod(0o644)
        (c/'.deploy-env').write_text(f'IMAGE="lmsysorg/sglang:v0.5.19"\nCONTAINER_NAME="{container}"\nMODEL_DIR='+json.dumps(str(weights))+f'\nPORT={model_port}\n');(c/'.deploy-env').chmod(0o600)
        server=c/'server.py';server.write_text(args.cpu_server.read_text().replace("MODEL_ID = 'SmolLM2-135M-Instruct-qa-cpu'",f"MODEL_ID = '{model}'"))
        # Successful store receipt is required. These two negative rows do not qualify.
        db=sqlite3.connect(store/'models.db');db.execute('CREATE TABLE store_models(card_id TEXT,status TEXT,downloaded_at TEXT,card_source TEXT,runtime_card_id TEXT)')
        db.executemany('INSERT INTO store_models VALUES(?,?,?,?,?)',[(logical,'deployed','2026-10-20','local',card),('experiment','registered',None,'local',None),('incomplete','downloading',None,'store',None)]);db.commit();db.close();(store/'models.db').chmod(0o600)
        for name in ('experiment','incomplete','directory-only'):
            d=cards/name;d.mkdir();(d/'common.sh').write_text(common.replace(container,container+'-'+name));(d/'common.sh').chmod(0o644)
        command('docker','run','-d','--name',container,'--label','megacube.acceptance=store-adoption','--memory','2g','--cpus','2','--restart=always','-p',f'127.0.0.1:{model_port}:8080','-v',str(weights)+':/qa/model:ro','-v',str(server)+':/qa/server.py:ro','--entrypoint','python3','lmsysorg/sglang:v0.5.19','/qa/server.py')
        wait(lambda:json.load(urllib.request.urlopen(f'http://127.0.0.1:{model_port}/v1/models',timeout=1)).get('data'),90);infer('before-enrollment')
        runtime=command('docker','inspect','--format','{{.Id}} {{.State.Pid}} {{.State.StartedAt}}',container)
        write_config();launch();wait(lambda:row().get('managed') and row().get('healthy') is True)
        assert command('docker','inspect','--format','{{.Id}} {{.State.Pid}} {{.State.StartedAt}}',container)==runtime
        assert api('GET','/v1/models')['active_model'] is None
        assert api('GET','/v1/models')['store_bindings'][logical]==card
        assert row()['boot_enabled'] is True and row()['recovery']['desired_running'] is True
        assert all(not x['managed'] for x in api('GET','/v1/models')['models'] if x['card']!=card)
        report['checks'].append('Headless actual Pinvou read-only SQLite helper automatically enrolled a successful local store runtime version; experiment/incomplete/directory-only cards remained unowned; original PID/start/default/boot intent retained')
        infer('after-enrollment')
        infer('named-gateway-without-default',True)
        with urllib.request.urlopen(f'http://127.0.0.1:{gateway_port}/v1/models',timeout=5) as response:
            assert model in [x['id'] for x in json.load(response)['data']]
        assert api('GET','/v1/models')['active_model'] is None
        report['checks'].append('Named gateway inference and model discovery work with no selected default; requests do not create a default selection')
        action('disable-boot');assert row()['recovery']['desired_running'] is True
        for _ in range(10):action('enable')
        assert command('docker','inspect','--format','{{.Id}} {{.State.Pid}} {{.State.StartedAt}}',container)==runtime
        infer('gateway-after-idempotent-enable',True)
        # Switch from explicit installer config to the same private receipt used by bundled installs.
        stop();settings.pop('store_inventory_program');settings.pop('store_inventory_profile');write_config()
        reader=state/'store-reader.json';reader.write_text(json.dumps({'format':'pinvou-store-reader-v1','program':str(args.pinvou.resolve()),'profile':str(profile)}));reader.chmod(0o600)
        launch();wait(lambda:row().get('managed') and row().get('healthy') is True)
        assert row()['boot_enabled'] is False
        assert command('docker','inspect','--format','{{.Id}} {{.State.Pid}} {{.State.StartedAt}}',container)==runtime
        report['checks'].append('10 repeated actual enable requests retained runtime PID. Daemon SIGKILL/restart plus installer receipt retained ownership and disabled boot preference without a UI')
        reader.unlink()
        api('POST','/v1/store-enrollment',{'program':str(args.pinvou.resolve()),'profile':str(profile)})
        assert json.loads(reader.read_text())['profile']==str(profile)
        report['checks'].append('Actual same-account Unix API registration restores a standalone reader without changing the daemon cards/profile binding')
        action('disable');wait(lambda:row()['status']=='stopped');action('enable');wait(lambda:row()['status']=='running' and row().get('healthy') is True)
        infer('after-managed-stop-start',True)
        revision=api('GET','/v1/status')['lifecycle_revision'];policy=state/'recovery'/f'{card}.json';before=policy.stat().st_mtime_ns
        rss=[];deadline=time.monotonic()+65;requests=0
        while time.monotonic()<deadline:
            s=api('GET','/v1/status');assert s['lifecycle_revision']==revision
            assert row()['managed'] and row()['status']=='running';requests+=2
            rss.append(int(next(l.split()[1] for l in Path(f'/proc/{process.pid}/status').read_text().splitlines() if l.startswith('VmRSS:'))));time.sleep(.25)
        assert policy.stat().st_mtime_ns==before
        report['checks'].append('65-second live soak crossed periodic inventories: stable revision, policy mtime, ownership and running state')
        report['soak']={'seconds':65,'requests':requests,'rss_kib_min':min(rss),'rss_kib_max':max(rss)}
        infer('after-soak',True)
        # Exercise the reader/cmsd boundary with explicit successful deployment
        # receipts in both directions. Full card snapshot restore itself is
        # covered by Pinvou's ModelManager switch_version regression.
        first_card, first_port, first_model = card, model_port, model
        second_container = container+'-v2';owned_containers.append(second_container)
        second_card = logical+'--version2';second_port = port();second_model = model+'-v2'
        second_dir = cards/second_card;second_dir.mkdir(mode=0o700)
        (second_dir/'common.sh').write_text(common.replace(container,second_container).replace(model,second_model));(second_dir/'common.sh').chmod(0o644)
        (second_dir/'.deploy-env').write_text(f'IMAGE="lmsysorg/sglang:v0.5.19"\nCONTAINER_NAME="{second_container}"\nMODEL_DIR='+json.dumps(str(weights))+f'\nPORT={second_port}\n');(second_dir/'.deploy-env').chmod(0o600)
        second_server=second_dir/'server.py';second_server.write_text(args.cpu_server.read_text().replace("MODEL_ID = 'SmolLM2-135M-Instruct-qa-cpu'",f"MODEL_ID = '{second_model}'"))
        command('docker','run','-d','--name',second_container,'--label','megacube.acceptance=store-adoption','--memory','2g','--cpus','2','--restart=no','-p',f'127.0.0.1:{second_port}:8080','-v',str(weights)+':/qa/model:ro','-v',str(second_server)+':/qa/server.py:ro','--entrypoint','python3','lmsysorg/sglang:v0.5.19','/qa/server.py')
        wait(lambda:json.load(urllib.request.urlopen(f'http://127.0.0.1:{second_port}/v1/models',timeout=1)).get('data'))
        def select_receipt(runtime):
            with sqlite3.connect(store/'models.db') as db:
                db.execute('UPDATE store_models SET runtime_card_id=? WHERE card_id=?',(runtime,logical))
        card, model_port, model = second_card, second_port, second_model
        select_receipt(card)
        wait(lambda:api('GET','/v1/models')['store_bindings'].get(logical)==card and row().get('healthy') is True)
        action('enable');action('enable-boot')
        wait(lambda:command('docker','inspect','--format','{{.State.Running}}',container)=='false')
        assert json.loads((state/'recovery'/f'{first_card}.json').read_text())['phase']=='archived'
        infer('after-explicit-new-current',True)
        card, model_port, model = first_card, first_port, first_model
        select_receipt(card)
        wait(lambda:api('GET','/v1/models')['store_bindings'].get(logical)==card and row().get('boot_enabled') is True)
        assert row()['recovery']['desired_running'] is False
        action('enable');wait(lambda:row().get('healthy') is True)
        assert command('docker','inspect','--format','{{.State.Running}}',second_container)=='false'
        # Enable may fill an empty default under the existing v1 policy. It
        # must never leave the retired physical revision as the default.
        assert api('GET','/v1/models')['active_model'] in (None, card)
        infer('after-explicit-old-current-rollback',True)
        report['checks'].append('Actual SQLite current receipts selected v1 -> v2 -> v1: each explicit enable retired the other runtime, rollback reused the archived physical card, inherited logical boot intent without replaying old running intent, and gateway inference followed both revisions')
    except Exception as e:report['errors'].append(repr(e));raise
    finally:
        stop()
        for log in logs:log.close()
        for owned in owned_containers:subprocess.run(['docker','rm','-f',owned],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        if command('systemctl','--user','show','cmsd.service','--property=MainPID','--property=ExecMainStartTimestampMonotonic')!=protected:report['errors'].append('protected cmsd changed')
        if command('docker','ps','-aq','--no-trunc').splitlines()!=containers:report['errors'].append('protected container set changed')
        report['seconds']=round(time.monotonic()-begin,3);(out/'report.json').write_text(json.dumps(report,ensure_ascii=False,indent=2))
        work.cleanup()
    assert not report['errors'],report['errors'];print(json.dumps(report,ensure_ascii=False,indent=2))
if __name__=='__main__':main()

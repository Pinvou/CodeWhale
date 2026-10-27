#!/usr/bin/env python3
"""Opt-in real cmsd -> production store-host retry with a delayed Unix proxy.

The store card is explicitly a shell fixture. Independently, an isolated real
CPU model serves inference throughout; no claim that fixture installs a model.
Only uniquely named QA processes/container are stopped. Existing weights are RO.
"""
import argparse, concurrent.futures, hashlib, http.client, json, os
from pathlib import Path
import signal, socket, socketserver, subprocess, threading, time


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock=socket.socket(socket.AF_UNIX);self.sock.settimeout(self.timeout);self.sock.connect(self.host)


def request(sock,method,path,body=None,timeout=20):
    conn=UnixHTTP(str(sock),timeout=timeout)
    try:
        conn.request(method,path,None if body is None else json.dumps(body),{'Content-Type':'application/json'})
        response=conn.getresponse();data=response.read(16*1024*1024+1)
        assert len(data)<=16*1024*1024
        return response.status,json.loads(data)
    finally:conn.close()


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for key in ('binary','pinvou','fixture_driver','model_dir','cpu_server','card_source','evidence'):
        parser.add_argument('--'+key.replace('_','-'),type=Path,required=True)
    parser.add_argument('--image',required=True)
    parser.add_argument('--model-port',type=int,default=18186)
    parser.add_argument('--gateway-port',type=int,default=18896)
    parser.add_argument('--cancel-after-restart',action='store_true',help='persist cancellation, restart again, and explicitly retry the confirmed cancelled run')
    args=parser.parse_args();root=args.evidence.resolve();root.mkdir(mode=0o700)
    for port in (args.model_port,args.gateway_port):
        with socket.socket() as probe:probe.bind(('127.0.0.1',port))
    (root/'driver.py').write_bytes(Path(__file__).read_bytes())
    begun=time.monotonic();report={'scope':'Same-host production cmsd and store host, explicit incomplete-install shell fixture; independent real SmolLM2 CPU inference. No physical multi-node/native GUI claim.', 'checks':[], 'errors':[]}
    def command(*parts):return subprocess.check_output(parts,text=True,timeout=180).strip()
    protected_service=command('systemctl','--user','show','cmsd.service','--property=MainPID')
    protected_model=json.loads(command('docker','inspect','megacube-qa-smollm2-cpu'))[0]
    helper=daemon=server=None;ready=None;created=False;name=f'megacube-retry-{os.getpid()}'
    log=(root/'daemon.log').open('ab',buffering=0)
    events=[];event_lock=threading.Lock();counter=0
    try:
        fixture_log=(root/'fixture-driver.log').open('ab',buffering=0)
        helper=subprocess.Popen(['python3',str(args.fixture_driver.resolve()),'--pinvou',str(args.pinvou.resolve()),'--evidence',str(root/'fixture'),'--checkpoint'],stdout=fixture_log,stderr=fixture_log)
        deadline=time.monotonic()+45
        while not (root/'fixture/ready.json').exists():
            assert helper.poll() is None,'fixture helper exited'
            assert time.monotonic()<deadline,'fixture startup timeout';time.sleep(.1)
        ready=json.loads((root/'fixture/ready.json').read_text());work=Path(ready['profile']).parent
        downstream=Path(ready['store_socket']);proxy=work/'proxy.sock';sock=Path(ready['cmsd_socket'])
        class Handler(socketserver.StreamRequestHandler):
            def handle(self):
                nonlocal counter
                self.request.settimeout(35)
                first=self.rfile.readline(8193).decode();method,path,_=first.split()
                length=0;head=0
                while True:
                    line=self.rfile.readline(8193);head+=len(line);assert head<=65536
                    if line in (b'\r\n',b'\n'):break
                    key,value=line.decode().split(':',1)
                    if key.lower()=='content-length':length=int(value.strip())
                assert length<=65536;body=json.loads(self.rfile.read(length)) if length else None
                delayed=False
                if method=='POST' and path=='/v1/retry_install':
                    with event_lock:
                        counter+=1;delayed=counter==1
                        events.append({'kind':'retry-request','body':body,'delayed':delayed,'at':time.monotonic()-begun})
                    if delayed:time.sleep(6.5)
                status,value=request(downstream,method,path,body,timeout=30)
                with event_lock:events.append({'kind':'response','method':method,'path':path,'status':status,'at':time.monotonic()-begun})
                payload=json.dumps(value).encode()
                try:self.wfile.write(f'HTTP/1.1 {status} QA\r\nContent-Length: {len(payload)}\r\nConnection: close\r\n\r\n'.encode()+payload)
                except (BrokenPipeError,ConnectionResetError):pass # injected lost first reply
        class Server(socketserver.ThreadingUnixStreamServer):daemon_threads=True
        server=Server(str(proxy),Handler);threading.Thread(target=server.serve_forever,daemon=True).start()
        binary=root/'cmsd-tested';binary.write_bytes(args.binary.resolve().read_bytes());binary.chmod(0o700)
        report.update(binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),pinvou_sha256=hashlib.sha256(args.pinvou.read_bytes()).hexdigest())
        cards=Path(ready['cards_dir']);model_card=cards/'qa-retry-cpu';model_card.mkdir()
        (model_card/'card.json').write_bytes((args.card_source/'card.json').read_bytes())
        weights=sorted(args.model_dir.resolve().glob('*.safetensors'));assert weights
        sizes=' '.join('"'+p.name+':'+str(p.stat().st_size)+'"' for p in weights)
        (model_card/'common.sh').write_text(f'IMAGE="{args.image}"\nCONTAINER_NAME="{name}"\nMODEL_ID="SmolLM2-135M-Instruct-qa-cpu"\nLMM_BIG_FILES=({sizes})\nLMM_SMALL_FILES=("config.json")\n')
        (model_card/'.deploy-env').write_text(f'IMAGE="{args.image}"\nCONTAINER_NAME="{name}"\nMODEL_DIR="{args.model_dir.resolve()}"\nPORT="{args.model_port}"\n')
        config=root/'cmsd.toml'
        config.write_text('\n'.join(f'{k} = {json.dumps(str(v))}' for k,v in {
            'socket_path':sock,'state_dir':root/'state','gateway_listen':f'127.0.0.1:{args.gateway_port}',
            'systemd_mode':'user','cards_dir':cards,'unit_dir':root/'units','engine_root':root/'engines',
            'models_dir':ready['models_dir'],'staging_dir':work/'staging','store_socket_path':proxy}.items())+'\nstore_poll_ms = 100\n')
        def api(method,path,body=None):return request(sock,method,path,body)
        def start():
            nonlocal daemon
            env={k:v for k,v in os.environ.items() if not k.startswith('CMSD_')}
            env['CMSD_CONFIG']=str(config)
            daemon=subprocess.Popen([str(binary)],env=env,stdout=log,stderr=log)
            end=time.monotonic()+35
            while time.monotonic()<end:
                assert daemon.poll() is None,'cmsd exited'
                try:
                    if api('GET','/v1/status')[0]==200:return
                except (OSError,http.client.HTTPException):pass
                time.sleep(.1)
            raise TimeoutError('cmsd startup')
        def wait_task(task_id,want=None):
            end=time.monotonic()+45
            while time.monotonic()<end:
                code,value=api('GET','/v1/tasks/'+task_id);assert code==200,value
                if want and value['status']==want:return value
                if not want and value['status']!='running':return value
                time.sleep(.1)
            raise TimeoutError(('task',task_id,want))
        def infer():
            start_time=time.monotonic();conn=http.client.HTTPConnection('127.0.0.1',args.gateway_port,timeout=20)
            try:
                conn.request('POST','/v1/chat/completions',json.dumps({'model':'megacube-default','messages':[{'role':'user','content':'Say hello.'}],'temperature':0,'max_tokens':12}),{'Content-Type':'application/json'})
                response=conn.getresponse();value=json.loads(response.read());assert response.status==200 and value['choices'][0]['message']['content'],value
                return round(time.monotonic()-start_time,4)
            finally:conn.close()
        command('docker','create','--name',name,'--memory','2g','--cpus','2','--restart=no','-p',f'127.0.0.1:{args.model_port}:8080','-v',f'{args.model_dir.resolve()}:/qa/model:ro','-v',f'{args.cpu_server.resolve()}:/qa/server.py:ro','--entrypoint','python3',args.image,'/qa/server.py');created=True
        start();assert 'model.install_retry.v1' in api('GET','/v1/device')[1]['capabilities']
        code,value=api('POST','/v1/models/qa-retry-cpu/enable',{});assert code==200,value
        assert wait_task(value['task_id'])['status']=='succeeded'
        latencies=[infer()]
        card=ready['card_id'];body={'retry_store_run_id':ready['original_run_id'],'expected_preview_sha256':ready['preview_sha256']}
        assert api('POST',f'/v1/models/{card}/install/retry',{**body,'confirmed':True})[0]==400
        assert api('POST','/v1/models/wrong-card/install/retry',body)[0]==409
        # Exercise the production UI adapter, which must discover cmsd's new
        # capability and route its retry through cmsd, not directly to receiver.
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            future=pool.submit(request,downstream,'POST','/v1/ui/incomplete_retry',{'run_id':ready['original_run_id'],'expected_sha256':ready['preview_sha256']})
            time.sleep(.3)
            latencies.extend(infer() for _ in range(3))
            code,accepted=future.result(timeout=20)
        assert code==200,accepted;accepted=accepted['result'];task_id=accepted['cmsd_task_id'];run_id=accepted['run_id']
        assert run_id=='cmsd-'+task_id
        end=time.monotonic()+15
        while time.monotonic()<end:
            code,detail=request(downstream,'GET','/v1/tasks/'+run_id)
            if code==200 and detail['progress'] is not None:break
            time.sleep(.1)
        else:raise TimeoutError('same-correlation retry did not reach worker')
        assert api('POST',f'/v1/models/{card}/install/retry',body)[0]==409
        calls=Path(ready['cards_dir'])/card/'.runtime/calls'
        assert calls.read_bytes()==b'invocation\ninvocation\n'
        assert any(e['kind']=='response' and e['status']==404 and e['path']=='/v1/tasks/'+run_id for e in events)
        requests=[e['body'] for e in events if e['kind']=='retry-request']
        assert len(requests)>=2 and all(v==requests[0] for v in requests)
        assert set(requests[0])=={'original_run_id','expected_preview_sha256','run_id'}
        report['checks'].append('Production UI adapter -> cmsd admission -> store receiver: delayed first POST, temporary GET404 and identical replay execute only one retry')
        saved=api('GET','/v1/tasks/'+task_id)[1]
        assert saved['store_context']['kind']=='install_local' and saved['store_context']['original_run_id']==ready['original_run_id']
        revision=api('GET','/v1/status')[1]['lifecycle_revision']
        daemon.kill();daemon.wait(timeout=10);start()
        assert api('GET','/v1/status')[1]['lifecycle_revision']==revision
        assert api('GET','/v1/tasks/'+task_id)[1]['store_run_id']==run_id
        assert api('POST',f'/v1/models/{card}/install/retry',body)[0]==409
        latencies.append(infer())
        expected_calls=b'invocation\ninvocation\n'
        if args.cancel_after_restart:
            code,cancelling=api('POST','/v1/tasks/'+task_id+'/cancel',{});assert code==200,cancelling
            state=json.loads((root/'state/state.json').read_text())
            assert state['tasks'][task_id]['store_context']['cancel_requested'] is True
            daemon.kill();daemon.wait(timeout=10);start()
            cancelled=wait_task(task_id);assert cancelled['status']=='cancelled',cancelled
            assert (Path(ready['models_dir'])/'fixture/partial.bin').read_bytes()==b'partial'
            code,preview=request(downstream,'POST','/v1/ui/incomplete_preview',{'run_id':run_id});assert code==200,preview
            code,accepted=request(downstream,'POST','/v1/ui/incomplete_retry',{'run_id':run_id,'expected_sha256':preview['result']['previewSha256']});assert code==200,accepted
            next_id=accepted['result']['cmsd_task_id'];assert next_id!=task_id
            task_id=next_id;run_id=accepted['result']['run_id'];expected_calls+=b'invocation\n'
            end=time.monotonic()+15
            while time.monotonic()<end and calls.read_bytes()!=expected_calls:time.sleep(.1)
            assert calls.read_bytes()==expected_calls
            report['checks'].append('Cancellation is fsynced before forwarding, survives a second SIGKILL, retains partial bytes, and a new explicit reviewed retry resumes only after confirmed cancelled')
        Path(ready['complete_flag']).touch()
        final=wait_task(task_id);assert final['status']=='succeeded' and '尚未启用' in final['result'],final
        assert calls.read_bytes()==expected_calls
        assert (Path(ready['models_dir'])/'fixture/partial.bin').read_bytes()==b'partialdone'
        library=request(downstream,'POST','/v1/ui/library',{})[1]['result']
        assert not library['incompleteInstallations'] and next(v for v in library['models'] if v['card_id']==card)['status']=='deployed'
        report['checks'].append('SIGKILL/restart restores same cmsd task, store run, lifecycle revision, exclusion and correct deploy-without-enable result; partial file retained')
        for _ in range(10):latencies.append(infer())
        # Completed work remains queryable and does not execute again on restart.
        daemon.terminate();daemon.wait(timeout=10);start();assert api('GET','/v1/tasks/'+task_id)[1]['status']=='succeeded'
        assert calls.read_bytes()==expected_calls
        report['checks'].append('Reopening management after completed retry preserves the terminal result and never repeats scripts')
        report.update(passed=True,inference={'count':len(latencies),'failures':0,'latency_seconds':latencies},retry_task=task_id,store_run=run_id,final_task=final)
    except BaseException as error:
        report['errors'].append(repr(error));raise
    finally:
        if daemon is not None and daemon.poll() is None:daemon.terminate();daemon.wait(timeout=15)
        if created:command('docker','rm','-f',name)
        if server is not None:server.shutdown();server.server_close()
        if ready is not None:Path(ready['release_file']).touch()
        if helper is not None and helper.poll() is None:helper.wait(timeout=30)
        current=json.loads(command('docker','inspect','megacube-qa-smollm2-cpu'))[0]
        report['protected_unchanged']=(command('systemctl','--user','show','cmsd.service','--property=MainPID')==protected_service and current['State']['Pid']==protected_model['State']['Pid'] and current['HostConfig']['RestartPolicy']==protected_model['HostConfig']['RestartPolicy'])
        report['seconds']=round(time.monotonic()-begun,3)
        (root/'proxy-events.json').write_text(json.dumps(events,indent=2)+'\n')
        (root/'report.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
        log.close()


if __name__=='__main__':main()

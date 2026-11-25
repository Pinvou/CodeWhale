#!/usr/bin/env python3
"""Real accepted-parent / receiver-not-yet-created recovery acceptance.

Uses production Pinvou store host and cmsd with a bounded forwarding gate.
The card is an explicit partial-file shell fixture, not an inference engine.
No original service/model or physical multi-node operation is performed.
"""
import argparse,hashlib,importlib.util,json,os,signal,socketserver,subprocess,threading,time
from pathlib import Path
from real_install_retry import request


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for key in ('cmsd','pinvou','fixture_driver','evidence'):parser.add_argument('--'+key.replace('_','-'),type=Path,required=True)
    parser.add_argument('--gateway-port',type=int,default=18909)
    parser.add_argument('--mcp-harness',type=Path,help='Pinvou real stdio McpClient helper; no socket fixtures are instantiated')
    args=parser.parse_args();root=args.evidence.resolve();root.mkdir(mode=0o700)
    report={'scope':__doc__.strip(),'errors':[],'checks':[]};begun=time.monotonic()
    helper=daemon=replacement=server=mcp=None;ready=None;release=threading.Event();entered=threading.Event();requests=[];lock=threading.Lock()
    log=(root/'daemon.log').open('ab',buffering=0)
    original=subprocess.check_output(['systemctl','--user','show','cmsd.service','--property=MainPID'],text=True).strip()
    try:
        helper=subprocess.Popen(['python3',str(args.fixture_driver.resolve()),'--pinvou',str(args.pinvou.resolve()),'--evidence',str(root/'fixture'),'--checkpoint'],stdout=(root/'fixture.log').open('w'),stderr=subprocess.STDOUT)
        end=time.monotonic()+45
        while not (root/'fixture/ready.json').exists():
            assert helper.poll() is None and time.monotonic()<end;time.sleep(.05)
        ready=json.loads((root/'fixture/ready.json').read_text());work=Path(ready['profile']).parent;sock=Path(ready['cmsd_socket']);store=Path(ready['store_socket']);proxy=work/'pending.sock'
        class Handler(socketserver.StreamRequestHandler):
            def handle(self):
                self.request.settimeout(40);method,path,_=self.rfile.readline(8193).decode().split();length=0
                for _ in range(100):
                    line=self.rfile.readline(8193)
                    if line in (b'\r\n',b'\n'):break
                    key,value=line.decode().split(':',1)
                    if key.lower()=='content-length':length=int(value)
                assert 0<=length<=65536;body=json.loads(self.rfile.read(length)) if length else None
                if method=='POST' and path=='/v1/retry_install':
                    with lock:requests.append(body)
                    entered.set();assert release.wait(30),'bounded receiver forwarding gate expired'
                code,value=request(store,method,path,body);data=json.dumps(value).encode()
                try:self.wfile.write(f'HTTP/1.1 {code} QA\r\nContent-Length: {len(data)}\r\nConnection: close\r\n\r\n'.encode()+data)
                except (BrokenPipeError,ConnectionResetError):pass
        class Server(socketserver.ThreadingUnixStreamServer):daemon_threads=True
        server=Server(str(proxy),Handler);threading.Thread(target=server.serve_forever,daemon=True).start()
        binary=root/'cmsd-tested';binary.write_bytes(args.cmsd.resolve().read_bytes());binary.chmod(0o700)
        config=root/'cmsd.toml';config.write_text(''.join(k+' = '+json.dumps(str(v))+'\n' for k,v in {
            'socket_path':sock,'state_dir':root/'state','gateway_listen':f'127.0.0.1:{args.gateway_port}','systemd_mode':'user',
            'cards_dir':ready['cards_dir'],'unit_dir':root/'units','engine_root':root/'engines','models_dir':ready['models_dir'],
            'staging_dir':work/'staging','store_socket_path':proxy}.items())+'store_poll_ms = 100\n')
        def api(method,path,body=None):return request(sock,method,path,body)
        def ui(method,body):
            code,value=request(store,'POST','/v1/ui/'+method,body);assert code==200,value;return value['result']
        def start():
            nonlocal daemon
            env={k:v for k,v in os.environ.items() if not k.startswith('CMSD_')};env['CMSD_CONFIG']=str(config)
            daemon=subprocess.Popen([str(binary)],env=env,stdout=log,stderr=log);end=time.monotonic()+30
            while time.monotonic()<end:
                assert daemon.poll() is None
                try:
                    if api('GET','/v1/status')[0]==200:return
                except OSError:pass
                time.sleep(.05)
            raise TimeoutError('isolated cmsd startup')
        def wait_task(task_id,status):
            end=time.monotonic()+25
            while time.monotonic()<end:
                code,task=api('GET','/v1/tasks/'+task_id);assert code==200,task
                if task['status']==status:return task
                assert task['status']=='running',task;time.sleep(.05)
            raise TimeoutError((task_id,status))
        if args.mcp_harness:
            spec=importlib.util.spec_from_file_location('actual_pinvou_mcp_harness',args.mcp_harness.resolve());module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
            mcp=module.McpClient({**os.environ,'PINVOU3_HOME':ready['profile'],'CMSD_SOCKET':str(sock),'PYTHONDONTWRITEBYTECODE':'1'})
            mcp.call('initialize',{'protocolVersion':'2024-11-05','capabilities':{},'clientInfo':{'name':'real-pending-recovery','version':'1'}})
            mcp.proc.stdin.write(json.dumps({'jsonrpc':'2.0','method':'notifications/initialized'})+'\n');mcp.proc.stdin.flush()
            tools=mcp.call('tools/list')['result']['tools'];assert {'cube_preview_install_retry','cube_retry_install','cube_get_task','cube_cancel_task'}<={v['name'] for v in tools}
            source=Path(module.SERVER)
            report['mcp_server_sha256']=hashlib.sha256(source.read_bytes()).hexdigest()
            digest=hashlib.sha256()
            for path in sorted((source.parent/'megacube_tools').glob('*.py')):
                digest.update(path.name.encode());digest.update(hashlib.sha256(path.read_bytes()).digest())
            report['mcp_tools_source_sha256']=digest.hexdigest()
        start();before=time.monotonic()
        accepted=ui('incomplete_retry',{'run_id':ready['original_run_id'],'expected_sha256':ready['preview_sha256']})
        report['acceptance_seconds']=time.monotonic()-before;task_id=accepted['cmsd_task_id'];run_id=accepted['run_id'];assert run_id=='cmsd-'+task_id
        assert entered.wait(3) and request(store,'GET','/v1/tasks/'+run_id)[0]==404
        pending=ui('log',{'run_id':run_id,'from':0,'wait':0})
        assert pending['done'] is False and pending['backend_pending'] is True and pending['lines']==[] and pending['next']==0,pending
        if mcp:
            observed=mcp.tool('cube_get_task',{'task_id':'store:'+run_id,'wait_seconds':0})['data'];assert observed['status']=='running' and observed['backend_pending'] is True,observed
        active=ui('active',{'card_id':ready['card_id']});assert any(v['run_id']==run_id for v in active['runs']),active
        report['checks'].append('Durable parent returns quickly; actual receiver GET404 is backend_pending with empty log/next0/donefalse, and active reattaches the same accepted run')
        daemon.kill();daemon.wait(timeout=10)
        unknown=ui('log',{'run_id':run_id,'from':0,'wait':0});assert unknown['done'] is False and unknown['unconfirmed'] is True,unknown
        if mcp:assert mcp.tool('cube_get_task',{'task_id':'store:'+run_id,'wait_seconds':0})['data']['status']=='unknown'
        code,failed_cancel=request(store,'POST','/v1/ui/cancel',{'run_id':run_id});assert code>=400 and 'error' in failed_cancel,failed_cancel
        start()
        cancelled=mcp.tool('cube_cancel_task',{'task_id':'store:'+run_id})['data'] if mcp else ui('cancel',{'run_id':run_id})
        parent=api('GET','/v1/tasks/'+task_id)[1];assert parent['store_context']['cancel_requested'] is True
        assert request(store,'GET','/v1/tasks/'+run_id)[0]==404
        daemon.kill();daemon.wait(timeout=10);start();release.set()
        terminal=wait_task(task_id,'cancelled');assert terminal['store_run_id']==run_id
        log_value=ui('log',{'run_id':run_id,'from':0,'wait':0});assert log_value['done'] is True,log_value
        partial=Path(ready['models_dir'])/'fixture/partial.bin';assert partial.read_bytes()==b'partial'
        calls=Path(ready['cards_dir'])/ready['card_id']/'.runtime/calls';assert 1<=len(calls.read_text().splitlines())<=2
        same=[v for v in requests if v['run_id']==run_id];assert len(same)>=2 and all(v==same[0] for v in same)
        assert set(same[0])=={'original_run_id','expected_preview_sha256','run_id'}
        report['checks'].append('Actual daemon loss yields nonterminal unconfirmed; cancel fails honestly while unavailable, then forwards to durable parent before child exists; second SIGKILL restores cancellation and same-ID replay never duplicates scripts')
        preview=mcp.tool('cube_preview_install_retry',{'run_id':run_id})['data'] if mcp else ui('incomplete_preview',{'run_id':run_id})
        Path(ready['complete_flag']).touch()
        next_run=mcp.tool('cube_retry_install',{'run_id':run_id,'expected_preview_sha256':preview['previewSha256']})['data'] if mcp else ui('incomplete_retry',{'run_id':run_id,'expected_sha256':preview['previewSha256']})
        assert next_run['run_id']!=run_id
        final=wait_task(next_run['cmsd_task_id'],'succeeded');assert partial.read_bytes()==b'partialdone'
        calls_before=calls.read_bytes();os.kill(ready['host_pid'],signal.SIGTERM)
        end=time.monotonic()+10
        host_stat=Path('/proc/'+str(ready['host_pid'])+'/stat')
        # The checkpoint helper owns waitpid and may not reap until release.
        # A zombie has already closed its descriptors and host lock.
        while host_stat.exists() and host_stat.read_text().split(')')[-1].strip().split()[0]!='Z':
            assert time.monotonic()<end;time.sleep(.05)
        env={**os.environ,'PINVOU3_HOME':ready['profile'],'CMSD_SOCKET':str(sock)}
        replacement=subprocess.Popen([str(args.pinvou.resolve()),'--model-store-host'],env=env,stdout=(root/'store-restart.log').open('ab'),stderr=subprocess.STDOUT)
        end=time.monotonic()+20
        while time.monotonic()<end:
            try:
                if ui('health',{}):break
            except (OSError,AssertionError):pass
            time.sleep(.05)
        done=ui('log',{'run_id':next_run['run_id'],'from':0,'wait':0});assert done['done'] is True
        assert calls.read_bytes()==calls_before and not ui('active',{'card_id':ready['card_id']})['runs']
        if mcp:
            observed=mcp.tool('cube_get_task',{'task_id':'store:'+next_run['run_id'],'wait_seconds':0})['data'];assert observed['status']=='succeeded',observed
            report['checks'].append('Actual production stdio MCP observes pending/unknown, cancels parent before child exists, previews and submits the final retry through Rust/cmsd/store, then reads succeeded after store restart')
        report['checks'].append('Confirmed cancellation permits a fresh reviewed retry, deploys without enable, and actual store-host restart preserves final logs/empty active list without re-execution')
        report.update(passed=True,task_id=task_id,store_run_id=run_id,final_task=final,
            cmsd_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),pinvou_sha256=hashlib.sha256(args.pinvou.read_bytes()).hexdigest(),same_correlation_posts=len(same))
    except BaseException as error:report['errors'].append(repr(error));raise
    finally:
        release.set()
        if mcp is not None:mcp.close()
        if daemon is not None and daemon.poll() is None:daemon.terminate();daemon.wait(timeout=10)
        if replacement is not None and replacement.poll() is None:replacement.terminate();replacement.wait(timeout=10)
        if server is not None:server.shutdown();server.server_close()
        if ready is not None:Path(ready['release_file']).touch()
        if helper is not None and helper.poll() is None:helper.wait(timeout=30)
        report['protected_cmsd_unchanged']=original==subprocess.check_output(['systemctl','--user','show','cmsd.service','--property=MainPID'],text=True).strip()
        report['seconds']=time.monotonic()-begun;(root/'forwarded-retry-correlations.json').write_text(json.dumps(requests,indent=2)+'\n')
        (root/'report.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n');log.close()


if __name__=='__main__':main()

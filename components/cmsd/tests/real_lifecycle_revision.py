"""Opt-in actual old/new binary rollback and durable v1 admission acceptance.

Uses a new private state/socket/port, no model/service lifecycle changes. The
installation attempts deliberately point to an absent QA store, proving durable
admission/failure without executing scripts or claiming model installation.
"""
import argparse,hashlib,http.client,json,os,signal,socket,subprocess,time,uuid
from pathlib import Path


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock=socket.socket(socket.AF_UNIX);self.sock.settimeout(self.timeout);self.sock.connect(self.host)


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary',type=Path,required=True)
    parser.add_argument('--legacy-binary',type=Path,required=True)
    parser.add_argument('--evidence',type=Path,required=True)
    parser.add_argument('--port',type=int,default=18890)
    args=parser.parse_args();root=args.evidence.resolve();root.mkdir(mode=0o700,parents=True,exist_ok=False)
    for name in ('state','cards','units','engines'):(root/name).mkdir(mode=0o700)
    binaries={}
    for name,source in [('new',args.binary),('legacy',args.legacy_binary)]:
        destination=root/('cmsd-'+name);destination.write_bytes(source.resolve().read_bytes());destination.chmod(0o700);binaries[name]=destination
    sock=Path('/run/user')/str(os.geteuid())/('cmsd-epoch-qa-'+uuid.uuid4().hex[:10]+'.sock')
    config=root/'cmsd.toml';config.write_text(''.join(k+' = '+json.dumps(v)+'\n' for k,v in {
        'socket_path':str(sock),'state_dir':str(root/'state'),'gateway_listen':f'127.0.0.1:{args.port}',
        'systemd_mode':'user','cards_dir':str(root/'cards'),'unit_dir':str(root/'units'),
        'engine_root':str(root/'engines'),'store_socket_path':str(root/'absent-store.sock')}.items()));config.chmod(0o600)
    environment={k:v for k,v in os.environ.items() if not k.startswith('CMSD_')};environment['CMSD_CONFIG']=str(config)
    process=None;logs=[];started=time.monotonic()
    protected=subprocess.check_output(['systemctl','--user','show','cmsd.service','--property=MainPID']).decode().strip()
    report={'scope':'same-host real old/new cmsd processes; absent store negative admission, no install or inference claim',
        'binary_sha256':{n:hashlib.sha256(p.read_bytes()).hexdigest() for n,p in binaries.items()},'checks':[],'errors':[],'epochs':[]}
    def request(method,path,body=None):
        connection=UnixHTTP(str(sock),timeout=10)
        try:
            connection.request(method,path,None if body is None else json.dumps(body),{'Content-Type':'application/json'})
            response=connection.getresponse();return response.status,json.loads(response.read())
        finally:connection.close()
    def start(name,label):
        nonlocal process
        log=(root/(label+'.log')).open('wb');logs.append(log)
        process=subprocess.Popen([str(binaries[name])],env=environment,stdout=log,stderr=log,start_new_session=True)
        for _ in range(150):
            assert process.poll() is None,'test daemon exited; inspect private log'
            try:
                code,value=request('GET','/v1/status')
                if code==200:return value
            except (OSError,http.client.HTTPException):pass
            time.sleep(.1)
        raise TimeoutError('isolated daemon not ready')
    def stop(sig=signal.SIGTERM):
        if process and process.poll() is None:os.killpg(process.pid,sig);process.wait(timeout=15)
    def epoch():
        code,value=request('GET','/v1/status');assert code==200 and value['lifecycle_version_ready'],value
        report['epochs'].append(value['lifecycle_revision']);return value['lifecycle_revision']
    def failed_install():
        code,value=request('POST','/v1/models/qa-no-execution/install',{})
        assert code==409 and value['error']['code']=='store_offline',value
        saved=json.loads((root/'state/state.json').read_text())
        assert saved['tasks'] and all(t['status']=='failed' for t in saved['tasks'].values())
        return saved
    try:
        assert start('new','01-new')['lifecycle_revision']==0
        identity=request('GET','/v1/device')[1]['device_id']
        failed_install();assert epoch()==1
        stop(signal.SIGKILL);assert start('new','02-new-restart')['lifecycle_revision']==1
        report['checks'].append('v1 failed installation admission is durable before effects; SIGKILL restart preserves version')
        marker=root/'state/lifecycle-revision.json';saved_marker=marker.read_bytes()
        stop();start('legacy','03-actual-old-binary');failed_install();stop()
        assert 'lifecycle_revision' not in json.loads((root/'state/state.json').read_text())
        assert marker.read_bytes()==saved_marker
        assert start('new','04-upgrade-after-old-writer')['lifecycle_revision']==2
        assert request('GET','/v1/device')[1]['device_id']==identity
        failed_install();assert epoch()==3
        report['checks'].append('actual old binary drops unknown state field while retaining marker; upgraded daemon advances epoch and retains the local device ID')
        stop();original=marker.read_bytes();marker.write_bytes(b'isolated-corruption')
        assert start('new','05-marker-corrupt')['lifecycle_version_ready'] is False
        tasks=json.loads((root/'state/state.json').read_text())['tasks']
        for _ in range(12):
            code,value=request('POST','/v1/models/qa-no-execution/install',{})
            assert code==503 and value['error']['code']=='lifecycle_version_unavailable'
        assert json.loads((root/'state/state.json').read_text())['tasks']==tasks
        assert request('GET','/v1/device')[1]['device_id']==identity
        assert request('GET','/v1/models')[0]==200
        assert marker.read_bytes()==b'isolated-corruption'
        # A same-process manual file edit does not silently reopen writes.
        marker.write_bytes(original)
        assert request('POST','/v1/models/qa-no-execution/install',{})[0]==503
        stop();assert start('new','06-matching-backup')['lifecycle_version_ready']
        assert epoch()==3
        report['checks'].append('corrupt marker fences 12 repeated mutations, retains reads and original corrupt bytes; matching backup plus restart restores admission')
        stop();marker.unlink()
        assert start('new','07-marker-missing')['lifecycle_version_ready'] is False
        assert request('POST','/v1/models/qa-no-execution/install',{})[0]==503 and not marker.exists()
        stop();marker.write_bytes(original);marker.chmod(0o600)
        assert start('new','08-restored')['lifecycle_version_ready'];assert epoch()==3
        report['checks'].append('missing marker never resets version; retained matching state backup recovers')
    except Exception as error:report['errors'].append(repr(error));raise
    finally:
        stop()
        for log in logs:log.close()
        report['protected_cmsd_pid_unchanged']=subprocess.check_output(['systemctl','--user','show','cmsd.service','--property=MainPID']).decode().strip()==protected
        report['elapsed_seconds']=time.monotonic()-started
        (root/'report.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
        print(json.dumps(report,ensure_ascii=False))


if __name__=='__main__':main()

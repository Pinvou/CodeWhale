"""Opt-in actual CPU A/B switch -> production frontend handoff -> Rust session -> generation.

Uses a loopback-only authenticated acceptance bridge, two newly created labeled
CPU containers, borrowed read-only weights and an isolated cmsd/profile. Native
window interaction and physical multi-node acceptance are separate.
"""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import secrets
import select
import socket
import subprocess
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.host)


def api(sock, method, path, body=None):
    connection = UnixHTTP(str(sock), timeout=10)
    try:
        connection.request(method, path, None if body is None else json.dumps(body),
                           {'Content-Type': 'application/json'})
        response = connection.getresponse()
        value = json.loads(response.read())
        if response.status >= 400:
            raise RuntimeError('cmsd request rejected: ' + str(response.status))
        return value
    finally:
        connection.close()


class Stdio:
    def __init__(self, command, environment, log, ready=False):
        self.process = subprocess.Popen(command, env=environment, stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=log, start_new_session=True)
        self.sequence = 0
        self.buffer = b''
        if ready:
            assert self.read(40).get('ready') is True

    def read(self, timeout=120):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if b'\n' in self.buffer:
                line, self.buffer = self.buffer.split(b'\n', 1)
                try:
                    return json.loads(line)
                except ValueError:
                    continue
            readable, _, _ = select.select([self.process.stdout], [], [], deadline-time.monotonic())
            if not readable:
                break
            chunk = os.read(self.process.stdout.fileno(), 65536)
            if not chunk:
                raise RuntimeError('acceptance process exited')
            self.buffer += chunk
            if len(self.buffer) > 1024*1024:
                raise ValueError('oversized stdio reply')
        raise TimeoutError('stdio response deadline')

    def request(self, value):
        self.sequence += 1
        value['id'] = self.sequence
        self.process.stdin.write((json.dumps(value) + '\n').encode())
        self.process.stdin.flush()
        reply = self.read()
        assert reply.get('id') == self.sequence
        if 'error' in reply:
            raise RuntimeError(str(reply['error']))
        return reply['result']

    def close(self):
        if self.process.poll() is not None:
            return
        self.process.stdin.close()
        try:
            self.process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.process.terminate()
            self.process.wait(timeout=10)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cmsd-binary', type=Path, required=True)
    parser.add_argument('--pinvou-host', type=Path, required=True)
    parser.add_argument('--weights', type=Path, required=True)
    parser.add_argument('--cpu-server', type=Path, required=True)
    parser.add_argument('--image', required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--protect-socket', type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    root = args.evidence.resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    nonce = uuid.uuid4().hex[:10]
    started = time.monotonic()
    containers, logs = [], []
    daemon = host = mcp = server = None
    lock = threading.Lock()
    config = {}
    command = lambda *values: subprocess.check_output(values, text=True).strip()
    protected_daemon = command('systemctl', '--user', 'show', 'cmsd.service',
        '--property=MainPID,ExecMainStartTimestampMonotonic')
    original_containers = command('docker', 'ps', '--quiet').splitlines()
    protected = {item: command('docker', 'inspect', '--format',
        '{{.State.Pid}} {{.State.StartedAt}} {{.RestartCount}}', item) for item in original_containers}
    protected_native = {}
    if args.protect_socket:
        original = api(args.protect_socket, 'GET', '/v1/status')
        for model in original.get('models', []):
            pid = model.get('main_pid')
            if pid and model.get('status') == 'running':
                protected_native[pid] = Path('/proc', str(pid), 'stat').read_text().rsplit(')', 1)[1].split()[19]
    weights = args.weights.resolve()
    weight_files = sorted(weights.glob('*.safetensors'))
    assert weight_files and (weights/'config.json').is_file()
    hashes = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in weight_files}
    profile = root / ('mcp-switch-real-' + nonce)
    profile.mkdir(mode=0o700)
    for name in ['cards', 'state', 'units', 'engines', 'models', 'staging', 'credentials']:
        (root/name).mkdir(mode=0o700)
    sock = Path('/run/user')/str(os.getuid())/('cmsd-handoff-'+nonce+'.sock')
    def port():
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            return listener.getsockname()[1]
    gateway = '127.0.0.1:' + str(port())
    cards = {key: 'qa-handoff-'+nonce+'-'+key for key in ['a', 'b']}
    model_ids = {key: 'SmolLM2-135M-Handoff-'+key.upper() for key in cards}
    template = args.cpu_server.resolve().read_text()
    marker = "MODEL_ID = 'SmolLM2-135M-Instruct-qa-cpu'"
    assert marker in template
    try:
        for key, name in cards.items():
            card = root/'cards'/name
            card.mkdir(mode=0o700)
            container = 'megacube-handoff-'+nonce+'-'+key
            host_port = port()
            (card/'card.json').write_text(json.dumps({'name': model_ids[key], 'engine_name': 'sglang',
                'description': 'Actual CPU inference acceptance; truncation to 512 tokens, not context-quality validation'}))
            big = ' '.join(json.dumps(p.name+':'+str(p.stat().st_size)) for p in weight_files)
            (card/'common.sh').write_text('IMAGE='+json.dumps(args.image)+'\nCONTAINER_NAME='+json.dumps(container)
                +'\nMODEL_ID='+json.dumps(model_ids[key])+'\nLMM_BIG_FILES=('+big+')\nLMM_SMALL_FILES=("config.json")\n')
            (card/'.deploy-env').write_text('IMAGE='+json.dumps(args.image)+'\nCONTAINER_NAME='+json.dumps(container)
                +'\nMODEL_DIR='+json.dumps(str(weights))+'\nPORT='+json.dumps(str(host_port))+'\n')
            script = template.replace(marker, 'MODEL_ID = '+repr(model_ids[key]))
            script = script.replace("model = AutoModelForCausalLM", "tokenizer.truncation_side = 'left'  # declared 512-token fixture limit\nmodel = AutoModelForCausalLM", 1)
            script = script.replace("body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))",
                "body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))\n        print(json.dumps({'qa_inference': MODEL_ID, 'requested_model': body.get('model'), 'stream': bool(body.get('stream'))}), flush=True)")
            if key == 'b':
                script = script.replace('from transformers import',
                    'import time as _qa_time\n_qa_time.sleep(3)  # declared readiness-gap fixture\nfrom transformers import', 1)
            (card/'cpu_model_server.py').write_text(script)
            command('docker', 'create', '--name', container, '--label', 'megacube.acceptance='+nonce,
                '--memory', '2g', '--cpus', '2', '--restart=no', '-p', f'127.0.0.1:{host_port}:8080',
                '-v', str(weights)+':/qa/model:ro', '-v', str(card/'cpu_model_server.py')+':/qa/server.py:ro',
                '--entrypoint', 'python3', args.image, '/qa/server.py')
            containers.append(container)
        values = {'socket_path':str(sock), 'state_dir':str(root/'state'), 'cards_dir':str(root/'cards'),
            'unit_dir':str(root/'units'), 'engine_root':str(root/'engines'), 'models_dir':str(root/'models'),
            'staging_dir':str(root/'staging'), 'store_socket_path':str(sock)+'.store',
            'gateway_listen':gateway, 'systemd_mode':'user'}
        (root/'cmsd.toml').write_text(''.join(key+' = '+json.dumps(value)+'\n' for key,value in values.items()))
        environment = {k:v for k,v in os.environ.items() if not k.startswith('CMSD_')}
        environment['CMSD_CONFIG'] = str(root/'cmsd.toml')
        daemon_log = (root/'cmsd.log').open('wb'); logs.append(daemon_log)
        daemon = subprocess.Popen([str(args.cmsd_binary.resolve())], env=environment, stdout=daemon_log,
            stderr=daemon_log, start_new_session=True)
        deadline = time.monotonic()+20
        while True:
            try:
                if api(sock,'GET','/v1/status')['service']=='cmsd': break
            except (OSError,http.client.HTTPException): pass
            assert daemon.poll() is None and time.monotonic()<deadline
            time.sleep(.1)
        def action(card, verb):
            accepted = api(sock,'POST','/v1/models/'+card+'/'+verb,{})
            deadline = time.monotonic()+90
            while time.monotonic()<deadline:
                task = api(sock,'GET','/v1/tasks/'+accepted['task_id'])
                if task['status']!='running':
                    assert task['status']=='succeeded', task
                    return
                time.sleep(.2)
            raise TimeoutError('setup action')
        action(cards['a'],'enable'); action(cards['b'],'enable')
        action(cards['b'],'disable'); action(cards['a'],'set-default')
        environment.update(PINVOU3_HOME=str(profile), CODEWHALE_HOME=str(root/'credentials'),
            CODEWHALE_SECRET_BACKEND='file', CMSD_SOCKET=str(sock), MEGACUBE_CARDS_DIR=str(root/'cards'),
            PINVOU3_MEGACUBE_ACCEPTANCE='1', PYTHONDONTWRITEBYTECODE='1')
        environment.pop('PINVOU3_CMSD_UNIT', None)
        host_log = (root/'pinvou-host.log').open('wb'); logs.append(host_log)
        host = Stdio([str(args.pinvou_host.resolve())], environment, host_log, ready=True)
        source = Path(__file__).resolve().parent.parent/'src-tauri/resources/common/bundle/mcp-servers/megacube_admin_server.py'
        def start_mcp():
            nonlocal mcp
            log = (root/('mcp-'+str(time.time_ns())+'.log')).open('wb'); logs.append(log)
            mcp = Stdio(['python3','-B',str(source)],environment,log)
            mcp.request({'jsonrpc':'2.0','method':'initialize','params':{'protocolVersion':'2024-11-05',
                'capabilities':{},'clientInfo':{'name':'actual-handoff-acceptance','version':'1'}}})
        token = secrets.token_hex(24)
        config = {'cards':cards,'models':model_ids,'gateway':'http://'+gateway+'/v1','profile':str(profile)}
        class Handler(BaseHTTPRequestHandler):
            def log_message(self,*_): pass
            def do_POST(self):
                nonlocal mcp
                if self.headers.get('Authorization')!='Bearer '+token:
                    self.send_error(403);return
                size=int(self.headers.get('Content-Length','0'))
                if not 0<=size<=262144:self.send_error(413);return
                request=json.loads(self.rfile.read(size))
                try:
                    with lock:
                        if self.path=='/config':value=config
                        elif self.path=='/api':
                            path=request['path'];assert request['method']=='GET'
                            assert path in ['/v1/status','/v1/models','/v1/tasks'] or path.startswith('/v1/tasks/t-')
                            value=api(sock,'GET',path)
                        elif self.path=='/host':value=host.request(request)
                        elif self.path=='/mcp':
                            if mcp is None:start_mcp()
                            assert request['name'] in ['cube_switch_model','cube_get_task']
                            value=mcp.request({'jsonrpc':'2.0','method':'tools/call','params':request})
                        elif self.path=='/close-mcp':
                            if mcp:mcp.close();mcp=None
                            value={'closed':True}
                        else:raise ValueError('unknown acceptance route')
                    payload=json.dumps({'result':value}).encode()
                    self.send_response(200)
                except Exception as error:
                    payload=json.dumps({'error':str(error)}).encode();self.send_response(400)
                self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(payload)))
                self.end_headers();self.wfile.write(payload)
        server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
        threading.Thread(target=server.serve_forever,daemon=True).start()
        runner=Path(__file__).resolve().parent.parent/'scripts/megacube-switch-follow-real.mjs'
        child_env=os.environ.copy();child_env['MEGACUBE_HANDOFF_QA_TOKEN']=token
        subprocess.run(['node',str(runner),'http://127.0.0.1:'+str(server.server_port),str(root)],
            env=child_env,check=True,timeout=420)
        report=json.loads((root/'report.json').read_text())
        assert protected_daemon==command('systemctl','--user','show','cmsd.service',
            '--property=MainPID,ExecMainStartTimestampMonotonic')
        for item,before in protected.items():
            assert before==command('docker','inspect','--format',
                '{{.State.Pid}} {{.State.StartedAt}} {{.RestartCount}}',item)
        for pid, ticks in protected_native.items():
            assert Path('/proc', str(pid), 'stat').read_text().rsplit(')', 1)[1].split()[19] == ticks
        assert hashes=={p.name:hashlib.sha256(p.read_bytes()).hexdigest() for p in weight_files}
        report.update(protected_original_services_unchanged=True,protected_native_process_count=len(protected_native),
            borrowed_cpu_weight_hashes_unchanged=True,
            total_seconds=round(time.monotonic()-started,3),scope=__doc__,native_window_clicked=False)
        (root/'report.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
        print(json.dumps({'passed':True,'report':str(root/'report.json'),'seconds':report['total_seconds']}),flush=True)
    finally:
        if server:server.shutdown();server.server_close()
        if mcp:mcp.close()
        if host:host.close()
        if daemon and daemon.poll() is None:daemon.terminate();daemon.wait(timeout=10)
        for container in containers:
            with (root/(container+'.log')).open('wb') as log:
                subprocess.run(['docker','logs',container],stdout=log,stderr=log)
            subprocess.run(['docker','stop','-t','5',container],capture_output=True)
            subprocess.run(['docker','rm',container],capture_output=True)
        for log in logs:log.close()


if __name__=='__main__':main()

#!/usr/bin/env python3
"""Two-node customer launcher. Python standard library; run as the login user."""
import argparse
import base64
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import struct
import subprocess
import sys
import time
import urllib.request
import zlib

ROOT = Path(__file__).resolve().parent
STATE = ROOT / 'node.json'
DATA = ROOT / 'data'
IMAGE = 'ghcr.io/anemll/dspark-vllm-gx10:0.1.1@sha256:a83948492cf13df455170fb42885f5ef4db54fefe0feff0f841ecbff464ac9d8'
RUNTIME_IMAGE = 'megacube/deepseek-direct:0.1.1-a83948492cf1'
REVISION = '86f746b36186f0e567729a5c06a8c918caba82a9'
MODEL = 'deepseek-ai/DeepSeek-V4-Flash-Vision-Exp'
PROJECT = 'megacube-deepseek-direct'
MARKER = 'megacube-deepseek-direct-v1'


def run(args, capture=False, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, text=True,
                          stdout=subprocess.PIPE if capture else None, **kwargs).stdout


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def save(path, obj):
    tmp = path.with_suffix('.tmp')
    tmp.write_text(json.dumps(obj, indent=2) + '\n')
    tmp.chmod(0o600)
    tmp.replace(path)


def config():
    require(STATE.is_file(), '请先执行 init。')
    c = json.loads(STATE.read_text())
    require(c.get('owner') == MARKER, 'node.json 不是本工具的配置。')
    return c


def candidates():
    result = []
    for hca in sorted(Path('/sys/class/infiniband').glob('*')):
        for port in sorted((hca / 'ports').glob('*')):
            if not (port / 'state').read_text().strip().endswith('ACTIVE'):
                continue
            if (port / 'link_layer').read_text().strip() != 'Ethernet':
                continue
            for iface in sorted((hca / 'device/net').glob('*')):
                result.append((iface.name, hca.name, port.name))
    return list(dict.fromkeys(result))


def gid_index(rails, sysfs=Path('/sys/class/infiniband')):
    # ===== [LMM 本地补丁 2026-09-10] 空 GID 槽位要跳过，不能让它把整段炸掉 =====
    # ConnectX 的 GID 表固定 255 个槽位，绝大多数是空的。较新的 mlx5 驱动读空槽位
    # 返回 EINVAL（老驱动返回空串），于是原来这段没有保护的 read_text() 会在第一个
    # 空槽位上抛 OSError: [Errno 22]，preflight 整段崩掉——check / deploy / start
    # 全都过不去。真机实测：255 个槽位里 251 个报 EINVAL，而正确答案（index 3）就在
    # 剩下的几个里，本来是找得到的。
    #
    # 这是 LMM 侧为了先跑通流程打的**临时**补丁，SHA256SUMS 已同步重算。
    # 研究团队修好后请换回官方包，并删掉这个补丁。
    sets = []
    for rail in rails:
        p = sysfs / rail['hca'] / 'ports' / rail['port']
        valid = set()
        for gid in (p / 'gids').glob('*'):
            idx = gid.name
            try:
                gid_type = (p / 'gid_attrs/types' / idx).read_text().strip()
                gid_ndev = (p / 'gid_attrs/ndevs' / idx).read_text().strip()
                gid_value = gid.read_text().strip()
            except OSError:
                continue  # 空槽位
            if (gid_type == 'RoCE v2' and gid_ndev == rail['iface']
                    and ipaddress.ip_address(gid_value) ==
                    ipaddress.ip_address('::ffff:' + rail['ip'])):
                valid.add(int(idx))
        require(valid, f"{rail['iface']} 无对应 IPv4 的 RoCE v2 GID。")
        sets.append(valid)
    common = set.intersection(*sets)
    require(common, '所选双 rail 没有共同 GID index；请使用单 rail 重新初始化或由工程师处理。')
    return min(common)


def preflight(c, network=True):
    verify_bundle()
    require(platform.system() == 'Linux' and platform.machine() == 'aarch64',
            '仅支持 Linux aarch64 / GB10 MegaCube。')
    for command in ['docker', 'rsync', 'ssh', 'ip', 'ping']:
        require(shutil.which(command), f'缺少 {command}，请按 README 完成系统基线。')
    run(['docker', 'info'], capture=True)
    version = run(['docker', 'compose', 'version', '--short'], capture=True).strip().lstrip('v')
    require(tuple(map(int, version.split('.')[:2])) >= (2, 30), '需要 Docker Compose >= 2.30（gpus 支持）。')
    require(Path('/dev/infiniband').is_dir(), '缺少 /dev/infiniband，请检查 RDMA 驱动。')
    mem = int(re.search(r'MemTotal:\s+(\d+)', Path('/proc/meminfo').read_text())[1])
    require(mem >= 115_000_000, '需要 128GB 统一内存级别的 MegaCube。')
    active = candidates()
    for rail in c['rails']:
        require((rail['iface'], rail['hca'], rail['port']) in active, '直连端口已变化或未 ACTIVE，请检查线缆。')
        if network:
            peer = str(ipaddress.ip_address(rail['ip']) + (1 if c['role'] == 'head' else -1))
            route = json.loads(run(['ip', '-j', 'route', 'get', peer], capture=True))[0]
            require(route.get('dev') == rail['iface'] and route.get('prefsrc') == rail['ip'],
                    f'{peer} 未通过指定直连接口，请检查子网冲突。')
            run(['ping', '-c', '2', '-W', '3', '-I', rail['iface'], peer], capture=True)
    gid_index(c['rails'])
    require((ROOT / 'runtime/docker-compose.dspark.yml').is_file(), '缺少 runtime，请使用完整交付压缩包。')


def init(args):
    verify_bundle()
    require(os.geteuid() != 0, '请使用普通登录账号运行；仅网络配置通过 sudo 提权。')
    require(not STATE.exists(), '已经初始化；如需换口，请先按 README 清理网络配置后重新 init。')
    require(re.fullmatch(r'/[A-Za-z0-9_./-]+', str(ROOT)), '请解压到不含空格或中文的 Linux 路径。')
    require(shutil.which('nmcli'), '此版本需要 NetworkManager/nmcli；请使用规定的出厂系统基线。')
    available = candidates()
    print('ACTIVE RDMA 接口 (网卡, HCA, port):', available)
    names = args.ifaces.split(',') if args.ifaces else []
    if not names:
        names = input('输入直连网卡名；双 rail 用逗号分隔，两端顺序必须对应: ').strip().split(',')
    require(1 <= len(names) <= 2 and len(set(names)) == len(names), '请选择 1 或 2 个不同的直连接口。')
    require(len(args.subnets.split(',')) >= len(names), '--subnets 数量不足。')
    rails = []
    for i, name in enumerate(names):
        matches = [r for r in available if r[0] == name]
        require(len(matches) == 1, f'{name} 不是唯一 ACTIVE RDMA 接口。')
        addresses = json.loads(run(['ip', '-j', 'addr', 'show', 'dev', name], capture=True))[0]['addr_info']
        require(not any(a['family'] == 'inet' for a in addresses),
                f'{name} 已有 IPv4 配置，拒绝覆盖。请由工程师确认空闲专用接口。')
        network = ipaddress.ip_network(args.subnets.split(',')[i])
        require(network.version == 4 and network.prefixlen == 30, '直连子网必须为 IPv4 /30。')
        routes = json.loads(run(['ip', '-j', '-4', 'route', 'show', 'table', 'all'], capture=True))
        require(not any(r.get('dst', 'default') != 'default' and
                        network.overlaps(ipaddress.ip_network(r['dst'], strict=False)) for r in routes),
                f'{network} 与现有路由冲突，请通过 --subnets 更换子网。')
        rails.append(dict(iface=name, hca=matches[0][1], port=matches[0][2],
                          ip=str(network.network_address + (1 if args.role == 'head' else 2)),
                          previous=run(['nmcli', '-g', 'GENERAL.CON-UUID', 'device', 'show', name], capture=True).strip(),
                          connection=PROJECT + '-' + str(i)))
    require(len({r['ip'] for r in rails}) == len(rails), '两条 rail 不能使用同一子网。')
    c = dict(owner=MARKER, role=args.role, rails=rails, port=args.port,
             worker_user=args.worker_user, worker_dir=args.worker_dir, bind=args.bind)
    # Validate all inputs before modifying any interface.
    require(re.fullmatch(r'[a-z_][a-z0-9_-]*[$]?', c['worker_user']), '非法 worker 用户名。')
    require(re.fullmatch(r'/[A-Za-z0-9_./-]+', c['worker_dir']) and '..' not in c['worker_dir'].split('/'),
            'worker-dir 必须为不含空格的绝对路径。')
    require(1024 <= c['port'] <= 65535 and c['port'] != 25000, 'API 端口非法或与 master 端口冲突。')
    ipaddress.IPv4Address(c['bind'])
    existing = run(['nmcli', '-g', 'NAME', 'connection', 'show'], capture=True).splitlines()
    require(all(r['connection'] not in existing for r in rails), '发现同名网络连接，请先清理或恢复已有部署。')
    run(['sudo', '-v'])
    created = []
    try:
        for rail in rails:
            run(['sudo', 'nmcli', 'con', 'add', 'type', 'ethernet', 'con-name', rail['connection'],
                 'ifname', rail['iface'], 'ipv4.method', 'manual', 'ipv4.addresses', rail['ip'] + '/30',
                 'ipv4.never-default', 'yes', 'ipv6.method', 'disabled', '802-3-ethernet.mtu', '1500',
                 'connection.autoconnect', 'yes', 'connection.autoconnect-priority', '100'])
            created.append(rail['connection'])
            run(['sudo', 'nmcli', 'con', 'up', rail['connection']])
        save(STATE, c)
    except BaseException:
        for name in reversed(created):
            subprocess.run(['sudo', 'nmcli', 'con', 'delete', name], check=False)
            previous = next(r['previous'] for r in rails if r['connection'] == name)
            if previous and previous != '--':
                subprocess.run(['sudo', 'nmcli', 'con', 'up', 'uuid', previous], check=False)
        raise
    print('初始化完成。Worker 完成后，在 Head 执行 bash megacube.sh pair，然后 deploy。')


def ssh_base(c, batch=True):
    worker_ip = str(ipaddress.ip_address(c['rails'][0]['ip']) + 1)
    return ['ssh', '-o', 'ConnectTimeout=10', '-o', 'ServerAliveInterval=15',
            '-o', 'ServerAliveCountMax=3', '-o', 'StrictHostKeyChecking=yes' if batch else 'StrictHostKeyChecking=ask',
            '-o', 'BatchMode=yes' if batch else 'BatchMode=no',
            c['worker_user'] + '@' + worker_ip]


def remote(c, action, payload=None, capture=False):
    argv = ['python3', c['worker_dir'] + '/controller.py', '_node', action,
            base64.b64encode(json.dumps(payload or {}).encode()).decode()]
    return run(ssh_base(c) + [shlex.join(argv)], capture=capture)


def pair(c):
    require(shutil.which('ssh-copy-id'), '缺少 ssh-copy-id。')
    key = Path.home() / '.ssh/id_ed25519'
    if not key.exists():
        key.parent.mkdir(mode=0o700, exist_ok=True)
        run(['ssh-keygen', '-t', 'ed25519', '-f', key, '-N', ''])
    run(['ssh-copy-id', '-i', str(key) + '.pub', ssh_base(c, False)[-1]])
    pair_check(c)
    print('SSH 配对完成。')


def pair_check(c):
    worker = json.loads(remote(c, 'config', capture=True))
    require(worker['role'] == 'worker', '远端不是 Worker。')
    require(len(worker['rails']) == len(c['rails']), '两端 rail 数量不同。')
    for head, other in zip(c['rails'], worker['rails']):
        require(ipaddress.ip_address(other['ip']) == ipaddress.ip_address(head['ip']) + 1,
                '两端子网/接口顺序不对应。')
    require(worker['bundle'] == hashlib.sha256((ROOT / 'SHA256SUMS').read_bytes()).hexdigest(),
            '两端交付包版本不一致，请使用同一压缩包。')


def verify_bundle():
    for line in (ROOT / 'SHA256SUMS').read_text().splitlines():
        digest, name = line.split('  ', 1)
        path = ROOT / name
        require(path.resolve().is_relative_to(ROOT) and path.is_file(), '交付文件缺失: ' + name)
        require(hashlib.sha256(path.read_bytes()).hexdigest() == digest, '交付文件校验失败: ' + name)


def compose(args, capture=False):
    # Do not inherit shell tuning variables: generated per-node env is authoritative.
    env = {k: v for k, v in os.environ.items()
           if k in {'PATH', 'HOME', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_CONFIG', 'XDG_RUNTIME_DIR'}}
    return run(['docker', 'compose', '-p', PROJECT, '--env-file', ROOT / 'runtime/node.env',
                '-f', ROOT / 'runtime/docker-compose.dspark.yml'] + args, capture=capture, env=env)


def write_env(c):
    cache = DATA / 'cache'
    cache.mkdir(parents=True, exist_ok=True)
    (ROOT / 'runtime/vllm_patch_gb10').mkdir(exist_ok=True)
    (DATA / 'tmp').mkdir(exist_ok=True)
    rails = c['rails']
    master = rails[0]['ip'] if c['role'] == 'head' else str(ipaddress.ip_address(rails[0]['ip']) - 1)
    values = dict(DSPARK_VLLM_IMAGE=RUNTIME_IMAGE, HF_CACHE=str(cache), DSPARK_TMP_HOST=str(DATA / 'tmp'),
                  DSPARK_MODEL='/cache/huggingface/model', DSPARK_REVISION='',
                  DSPARK_ENCODING_FILE='/cache/huggingface/model/encoding/encoding_dsv4.py',
                  SERVED_MODEL_NAME='deepseek-v4-flash-vision-exp', HF_HUB_OFFLINE=1, TRANSFORMERS_OFFLINE=1,
                  NODE_RANK=0 if c['role'] == 'head' else 1, HEADLESS='' if c['role'] == 'head' else 1,
                  MASTER_ADDR=master, MASTER_PORT=25000, VLLM_HOST_IP=rails[0]['ip'],
                  VLLM_HOST=c['bind'], VLLM_PORT=c['port'],
                  NCCL_IB_HCA='=' + ','.join(r['hca'] + ':' + r['port'] for r in rails),
                  NCCL_SOCKET_IFNAME='=' + rails[0]['iface'], TP_SOCKET_IFNAME=rails[0]['iface'],
                  GLOO_SOCKET_IFNAME=rails[0]['iface'], NCCL_IB_GID_INDEX=gid_index(rails),
                  NCCL_CROSS_NIC=0, NCCL_NET='IB', NCCL_DEBUG='INFO',
                  MAX_MODEL_LEN=262144, MAX_NUM_SEQS=8, MAX_NUM_BATCHED_TOKENS=16384,
                  GPU_MEMORY_UTILIZATION='0.83', MTP_NUM_TOKENS=6, ENABLE_VLLM_GB10_PATCH=0,
                  DSPARK_RESTART_POLICY='no')
    path = ROOT / 'runtime/node.env'
    require(not any("'" in str(v) or '\n' in str(v) for v in values.values()), '部署路径含不支持字符。')
    path.write_text(''.join(f"{k}='{v}'\n" for k, v in values.items()))
    path.chmod(0o600)
    compose(['config', '--quiet'])


def gpu_check():
    run(['docker', 'run', '--rm', '--gpus', 'all', '--entrypoint', 'python3', RUNTIME_IMAGE, '-c',
         'import torch; assert torch.cuda.device_count()==1; '
         'assert torch.cuda.get_device_capability()==(12,1); print(torch.cuda.get_device_name())'])


def check_model(path):
    index = json.loads((path / 'model.safetensors.index.json').read_text())
    require(index.get('weight_map'), '模型权重索引为空。')
    required = set(index['weight_map'].values()) | {'config.json', 'encoding/encoding_dsv4.py'}
    require(any(path.glob('*token*')), '缺少 tokenizer 文件。')
    for name in required:
        file = path / name
        require(file.resolve().is_relative_to(path.resolve()) and file.is_file() and file.stat().st_size > 0,
                f'模型文件缺失或路径越界: {name}')


def model_hashes(path):
    check_model(path)
    hashes = {}
    for file in sorted(path.rglob('*')):
        if file.is_file() and '.cache' not in file.relative_to(path).parts:
            require(not file.is_symlink(), '模型交付目录不允许符号链接。')
            h = hashlib.sha256()
            with file.open('rb') as stream:
                for chunk in iter(lambda: stream.read(8 * 1024 * 1024), b''):
                    h.update(chunk)
            hashes[file.relative_to(path).as_posix()] = h.hexdigest()
    return hashes


def verify():
    expected = json.loads((DATA / 'model-manifest.json').read_text())
    require(expected['revision'] == REVISION and expected['model'] == MODEL, '模型清单版本不匹配。')
    require(model_hashes(DATA / 'cache/model') == expected['files'], '模型 SHA256 校验失败，请重新 deploy。')
    (DATA / 'verified').write_text(hashlib.sha256((DATA / 'model-manifest.json').read_bytes()).hexdigest())
    print('完整模型 SHA256 校验通过。')


def api(c, route, body=None, timeout=15):
    host = '127.0.0.1' if c['bind'] == '0.0.0.0' else c['bind']
    req = urllib.request.Request(f"http://{host}:{c['port']}{route}",
                                 data=None if body is None else json.dumps(body).encode(),
                                 headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=timeout) as response:
        return response.read()


def smoke(c):
    body = {'model': 'deepseek-v4-flash-vision-exp', 'messages': [{'role': 'user', 'content': '请用中文回答：1+1等于几？'}],
            'max_tokens': 128, 'temperature': 0, 'chat_template_kwargs': {'thinking': False}}
    result = json.loads(api(c, '/v1/chat/completions', body, timeout=180))
    require(result.get('choices') and result['choices'][0]['message'].get('content'), '文本冒烟没有有效内容。')
    print(result['choices'][0]['message']['content'])


def vision_smoke(c):
    def chunk(kind, content):
        return struct.pack('!I', len(content)) + kind + content + struct.pack('!I', zlib.crc32(kind + content))
    png = (b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('!2I5B', 64, 64, 8, 2, 0, 0, 0))
           + chunk(b'IDAT', zlib.compress((b'\0' + b'\xff\0\0' * 64) * 64)) + chunk(b'IEND', b''))
    body = {'model': 'deepseek-v4-flash-vision-exp', 'messages': [{'role': 'user', 'content': [
        {'type': 'text', 'text': '这张图片的主要颜色是什么？'},
        {'type': 'image_url', 'image_url': {'url': 'data:image/png;base64,' + base64.b64encode(png).decode()}}]}],
        'max_tokens': 128, 'temperature': 0, 'chat_template_kwargs': {'thinking': False}}
    result = json.loads(api(c, '/v1/chat/completions', body, timeout=180))
    content = result['choices'][0]['message'].get('content', '')
    require(content and ('红' in content or 'red' in content.lower()), '图像冒烟未识别红色: ' + content)
    print('原生图像冒烟通过: ' + content)


def node(action, payload):
    c = config()
    if action == 'config':
        verify_bundle()
        c['bundle'] = hashlib.sha256((ROOT / 'SHA256SUMS').read_bytes()).hexdigest()
        print(json.dumps(c)); return
    if action == 'check':
        preflight(c); return
    if action == 'prepare':
        (DATA / 'cache/model').mkdir(parents=True, exist_ok=True)
        present = sum(p.stat().st_size for p in (DATA / 'cache/model').rglob('*') if p.is_file())
        require(shutil.disk_usage(ROOT).free > max(0, 168_000_000_000 - present) + 60 * 1024**3,
                '空间不足：需容纳剩余权重及至少 60GiB 的镜像/缓存预算。Docker 数据盘也需保留至少 60GiB。')
        return
    if action == 'pull':
        run(['docker', 'pull', IMAGE])
        run(['docker', 'tag', IMAGE, RUNTIME_IMAGE])
        gpu_check(); return
    if action == 'gpu-check':
        gpu_check(); return
    if action == 'image-id':
        print(run(['docker', 'image', 'inspect', '--format', '{{.Id}}', RUNTIME_IMAGE], capture=True).strip()); return
    if action == 'manifest-id':
        print(hashlib.sha256((DATA / 'model-manifest.json').read_bytes()).hexdigest()); return
    if action == 'invalidate':
        (DATA / 'verified').unlink(missing_ok=True); return
    if action == 'purge-data':
        require(DATA.resolve() == ROOT / 'data' and not DATA.is_symlink(), '拒绝清理符号链接数据目录。')
        running = run(['docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + PROJECT], capture=True)
        require(not running.strip(), '请先完成双机 uninstall 再清理数据。')
        # Dedicated bind mount only; Docker owns some JIT cache files.
        if DATA.exists():
            run(['docker', 'run', '--rm', '-v', f'{DATA}:/payload', '--entrypoint', 'python3', RUNTIME_IMAGE,
                 '-c', 'import pathlib,shutil; p=pathlib.Path("/payload"); '
                 '[(f.unlink() if f.is_symlink() or f.is_file() else shutil.rmtree(f)) for f in p.iterdir()]'])
        return
    if action == 'verify':
        verify(); return
    if action == 'start':
        preflight(c)
        require((DATA / 'verified').is_file(), '模型尚未校验，请先 deploy/verify。')
        require((DATA / 'verified').read_text() == hashlib.sha256((DATA / 'model-manifest.json').read_bytes()).hexdigest(),
                '模型清单发生变化，请重新 verify。')
        check_model(DATA / 'cache/model')
        write_env(c)
        compose(['up', '-d', '--pull', 'never'])
        return
    if action in ('stop', 'uninstall'):
        if (ROOT / 'runtime/node.env').exists():
            compose(['stop' if action == 'stop' else 'down', '--timeout', '30'])
        return
    if action == 'status':
        compose(['ps', '--all']); return
    if action == 'logs':
        compose(['logs', '--no-color', '--tail', '150']); return
    if action == 'running':
        require(bool(compose(['ps', '--status', 'running', '-q'], capture=True).strip()), '该节点容器未运行。')
        return
    raise RuntimeError('未知节点命令: ' + action)


def both(c, action):
    errors = []
    # Still operate on Head if Worker is offline, but never report full success.
    for label, task in [('Worker', lambda: remote(c, action)), ('Head', lambda: node(action, {}))]:
        print('--- ' + label + ' ---', flush=True)
        try:
            task()
        except Exception as exc:
            errors.append(f'{label}: {exc}')
    require(not errors, '\n'.join(errors))


def deploy(c, args):
    pair_check(c)
    both(c, 'check')
    # Running ranks must be stopped explicitly before modifying model bytes.
    for local in [True, False]:
        text = (run(['docker', 'ps', '-q', '--filter', 'label=com.docker.compose.project=' + PROJECT], capture=True)
                if local else run(ssh_base(c) + [shlex.join(['docker', 'ps', '-q', '--filter',
                    'label=com.docker.compose.project=' + PROJECT])], capture=True))
        require(not text.strip(), '服务正在运行，请先 stop 再 deploy。')
    both(c, 'prepare')
    both(c, 'invalidate')
    DATA.mkdir(exist_ok=True)
    if args.offline:
        gpu_check()
    else:
        node('pull', {})
    target = ssh_base(c)[-1]
    transport = shlex.join(ssh_base(c)[:-1])
    # Transfer the exact image over the direct cable; Worker needs no Internet.
    print('通过直连线分发运行镜像。', flush=True)
    producer = subprocess.Popen(['docker', 'save', RUNTIME_IMAGE], stdout=subprocess.PIPE)
    try:
        consumer = subprocess.run(ssh_base(c) + ['docker load'], stdin=producer.stdout)
        producer.stdout.close()
        require(producer.wait() == 0 and consumer.returncode == 0, '镜像分发失败。')
    finally:
        if producer.poll() is None:
            producer.terminate(); producer.wait()
    remote(c, 'gpu-check')
    if not args.offline:
        cache = DATA / 'cache'
        cache.mkdir(exist_ok=True)
        run(['docker', 'run', '--rm', '--user', f'{os.getuid()}:{os.getgid()}',
             '-e', 'HOME=/tmp', '-e', 'HF_TOKEN', '-e', 'HF_HUB_DOWNLOAD_TIMEOUT=120',
             '-v', f'{cache}:/cache', '--entrypoint', 'python3', RUNTIME_IMAGE, '-c',
             'from huggingface_hub import snapshot_download; '
             f'snapshot_download({MODEL!r},revision={REVISION!r},local_dir="/cache/model",max_workers=4)'])
        if (DATA / 'model-manifest.json').exists():
            # A retry must not silently bless modified cached weights by replacing the old manifest.
            verify()
        print('生成完整模型校验清单，约 168GB 文件需要数分钟。', flush=True)
        save(DATA / 'model-manifest.json', dict(model=MODEL, revision=REVISION, files=model_hashes(cache / 'model')))
    else:
        verify()
    run(['rsync', '-a', '--partial', '--protect-args', '-e', transport,
         str(DATA / 'cache/model') + '/', target + ':' + c['worker_dir'] + '/data/cache/model/'])
    run(['rsync', '-a', '--protect-args', '-e', transport, str(DATA / 'model-manifest.json'),
         target + ':' + c['worker_dir'] + '/data/model-manifest.json'])
    both(c, 'verify')
    start(c, args.timeout)


def start(c, timeout):
    require(timeout > 0, 'timeout 必须大于 0。')
    pair_check(c)
    both(c, 'check')
    local_id = run(['docker', 'image', 'inspect', '--format', '{{.Id}}', RUNTIME_IMAGE], capture=True).strip()
    require(local_id == remote(c, 'image-id', capture=True).strip(), '两端镜像 ID 不一致，请重新 deploy。')
    require(hashlib.sha256((DATA / 'model-manifest.json').read_bytes()).hexdigest() ==
            remote(c, 'manifest-id', capture=True).strip(), '两端模型清单不一致，请重新 deploy。')
    try:
        remote(c, 'start')
        node('start', {})
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            node('running', {})
            remote(c, 'running')
            try:
                api(c, '/health', timeout=5)
                smoke(c)
                endpoint_host = '<Head业务IP>' if c['bind'] == '0.0.0.0' else c['bind']
                print(f"双机启动及文本冒烟通过，API: http://{endpoint_host}:{c['port']}/v1")
                return
            except (OSError, ValueError, RuntimeError):
                print('模型加载/预热中，15 秒后重试。', flush=True)
                time.sleep(15)
        raise RuntimeError('启动超时；请查看 logs，修复后重新 start。')
    except BaseException:
        try:
            both(c, 'logs')
        finally:
            both(c, 'stop')
        raise


def main():
    parser = argparse.ArgumentParser(description='MegaCube 双台直连 DeepSeek 管理入口')
    sub = parser.add_subparsers(dest='command', required=True)
    p = sub.add_parser('init')
    p.add_argument('--role', choices=['head', 'worker'], required=True)
    p.add_argument('--ifaces', default='')
    p.add_argument('--subnets', default='192.168.240.0/30,192.168.241.0/30')
    p.add_argument('--worker-user', default=os.environ.get('USER', 'test'))
    p.add_argument('--worker-dir', default=str(Path.home() / 'megacube-deepseek'))
    p.add_argument('--bind', default='127.0.0.1')
    p.add_argument('--port', type=int, default=8888)
    for name in ['pair', 'check', 'stop', 'status', 'logs', 'verify', 'smoke-test', 'network-reset']:
        sub.add_parser(name)
    p = sub.add_parser('uninstall')
    p.add_argument('--purge-data', action='store_true', help='同时永久删除两台的模型和编译缓存')
    for name in ['deploy', 'start']:
        p = sub.add_parser(name)
        p.add_argument('--timeout', type=int, default=3600)
        if name == 'deploy':
            p.add_argument('--offline', action='store_true')
    p = sub.add_parser('_node', help=argparse.SUPPRESS)
    p.add_argument('action'); p.add_argument('payload')
    args = parser.parse_args()
    if args.command == 'init':
        init(args); return
    c = config()
    if args.command == '_node':
        node(args.action, json.loads(base64.b64decode(args.payload))); return
    if args.command == 'network-reset':
        require(not run(['docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=' + PROJECT], capture=True).strip(),
                '请先从 Head 完成 uninstall。network-reset 应在本机控制台执行。')
        for rail in c['rails']:
            run(['sudo', 'nmcli', 'con', 'delete', rail['connection']])
            if rail.get('previous') and rail['previous'] != '--':
                run(['sudo', 'nmcli', 'con', 'up', 'uuid', rail['previous']])
        STATE.unlink()
        print('本机直连配置已移除，数据保留。'); return
    require(c['role'] == 'head', '集群管理命令请只在 Head 执行。')
    # Serialize lifecycle operations on the head, including stop and uninstall.
    import fcntl
    with (ROOT / '.operation.lock').open('w') as lock:
        if args.command not in ('status', 'logs'):
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.command == 'pair': pair(c)
        elif args.command == 'deploy': deploy(c, args)
        elif args.command == 'start': start(c, args.timeout)
        elif args.command == 'smoke-test':
            smoke(c); vision_smoke(c)
        else:
            both(c, args.command)
            if args.command == 'uninstall':
                if args.purge_data:
                    both(c, 'purge-data')
                print('双机服务已卸载。网络、SSH、运行镜像和交付工具保留。权重仅在 --purge-data 时删除。')


if __name__ == '__main__':
    try:
        main()
    except (Exception, KeyboardInterrupt) as exc:
        print(f'失败: {exc}', file=sys.stderr)
        sys.exit(1)

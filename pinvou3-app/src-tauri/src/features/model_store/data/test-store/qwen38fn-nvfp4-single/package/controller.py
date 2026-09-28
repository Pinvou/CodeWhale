#!/usr/bin/env python3
"""Single GB10 Qwen lifecycle manager. Does not mutate other containers or drivers."""
import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

from model_assets import load_source, quick_verify, require, verify, write_json

ROOT = Path(__file__).resolve().parent
STATE = ROOT / 'state'
CONFIG = ROOT / 'config.json'
IMAGE_REPOSITORY = 'vllm/vllm-openai'
# Registry-verified ARM64 manifest; see IMAGE-PROVENANCE.json and VALIDATION.md.
# This replaces the unavailable historical manifest; GPU acceptance is pending.
RECORDED_IDENTITY = 'sha256:a551e05307cd2e0092139d84db32af9c97e67d2eeeff072d21e429131d8c23f0'
IMAGE = IMAGE_REPOSITORY + '@' + RECORDED_IDENTITY
IMAGE_PLATFORM = 'linux/arm64'
NAME = 'megacube-qwen38fn-single'
OWNER = 'megacube.qwen38fn.single.v1'
VP = '/usr/local/lib/python3.12/dist-packages/vllm/'
PATCHES = {
    'ple_layer.py': 'models/qwen4_exp/nvidia/ple_layer.py',
    'ple_mmap.py': 'models/qwen4_exp/nvidia/ops/ple_mmap.py',
    'model_state.py': 'models/qwen4_exp/nvidia/model_state.py',
    'mtp_draft_vocab.py': 'models/qwen4_exp/nvidia/mtp.py',
    'upstream-overlays/ops_ple.py': 'models/qwen4_exp/nvidia/ops/ple.py',
    'upstream-overlays/ops_qsa.py': 'models/qwen4_exp/nvidia/ops/qsa.py',
    'upstream-overlays/qsa.py': 'models/qwen4_exp/nvidia/qsa.py',
    'upstream-overlays/platforms_interface.py': 'platforms/interface.py',
    'upstream-overlays/modelopt.py': 'model_executor/layers/quantization/modelopt.py',
}


def run(argv, capture=False):
    return subprocess.run([str(x) for x in argv], check=True, text=True,
                          stdout=subprocess.PIPE if capture else None).stdout


def bundle_check():
    for line in (ROOT / 'SHA256SUMS').read_text().splitlines():
        sha, name = line.split('  ', 1)
        file = ROOT / name
        require(file.resolve().is_relative_to(ROOT) and file.is_file(), '缺少交付文件: ' + name)
        require(hashlib.sha256(file.read_bytes()).hexdigest() == sha, '交付文件已变化: ' + name)


def configuration():
    if not CONFIG.exists():
        write_json(CONFIG, {'bind': '127.0.0.1', 'port': 8000,
                            'model_dir': str(ROOT / 'models/Qwen3.8-Flash-Next-NVFP4'),
                            'cache_dir': str(ROOT / 'cache'), 'revision': '', 'startup_timeout': 3600})
    c = json.loads(CONFIG.read_text(encoding='utf-8'))
    ipaddress.IPv4Address(c['bind'])
    require(type(c['port']) is int and 1024 <= c['port'] <= 65535, 'port 必须为1024~65535整数')
    require(type(c['startup_timeout']) is int and 60 <= c['startup_timeout'] <= 7200, 'startup_timeout应为60~7200秒')
    require(c['revision'] == '' or re.fullmatch('[0-9a-f]{40}', c['revision']), 'revision必须为空或40位commit SHA')
    paths = [Path(c[k]).expanduser() for k in ['model_dir', 'cache_dir']]
    for path in [ROOT, *paths]:
        require(path.is_absolute() and not any(x in str(path) for x in ',\n\r'), '路径必须为绝对路径且不含逗号/换行')
        require(not path.is_symlink(), '部署目录不能是符号链接')
    model, cache = [p.resolve() for p in paths]
    require(not (model == cache or model.is_relative_to(cache) or cache.is_relative_to(model)), '模型与缓存目录不得重叠')
    for p in [model, cache]:
        require(p != ROOT and not ROOT.is_relative_to(p) and p != STATE and not p.is_relative_to(STATE),
                '模型/缓存不能覆盖程序或state目录')
    c.update(model_dir=str(model), cache_dir=str(cache))
    return c


def docker_available():
    require(shutil.which('docker'), '缺少Docker；请安装出厂NVIDIA容器运行环境')
    run(['docker', 'info'], capture=True)


def inspect_container():
    # Query the daemon first: daemon/permission errors must not look like an absent container.
    ids = run(['docker', 'ps', '-aq', '--filter', 'name=^/' + NAME + '$'], capture=True).strip()
    if not ids:
        return None
    info = json.loads(run(['docker', 'inspect', NAME], capture=True))[0]
    labels = info['Config'].get('Labels') or {}
    require(labels.get('megacube.owner') == OWNER and labels.get('megacube.install') == str(ROOT),
            '同名容器不属于本安装目录，拒绝操作')
    return info


def ensure_stopped():
    info = inspect_container()
    require(not info or not info['State'].get('Running'), '服务正在运行，请先 stop')


def preflight(c):
    bundle_check()
    require(platform.system() == 'Linux' and platform.machine() == 'aarch64', '目标必须为Linux aarch64 GB10 MegaCube')
    require(os.geteuid() != 0, '请用可执行docker的普通登录账号运行，不要sudo整个脚本')
    docker_available()
    mem = Path('/proc/meminfo').read_text()
    total = int(re.search(r'MemTotal:\s+(\d+)', mem)[1])
    require(total >= 115_000_000, '该配方需要128GB级别统一内存')
    for key in ['model_dir', 'cache_dir']:
        Path(c[key]).mkdir(parents=True, exist_ok=True)
    STATE.mkdir(exist_ok=True)


def image_info(ref):
    return json.loads(run(['docker', 'image', 'inspect', ref], capture=True))[0]


def image_prepare():
    lock = STATE / 'image.json'
    previous = json.loads(lock.read_text(encoding='utf-8')) if lock.exists() else None
    # Reuse a locally verified digest without network access. Never pull a tag,
    # an old lock's arbitrary repository, or use a backend-specific Id as a ref.
    try:
        info = image_info(IMAGE)
    except subprocess.CalledProcessError:
        print('准备固定 ARM64 镜像:', IMAGE, flush=True)
        run(['docker', 'pull', '--platform', IMAGE_PLATFORM, IMAGE])
        info = image_info(IMAGE)
    verify_image(info)
    STATE.mkdir(parents=True, exist_ok=True)
    if previous is not None and not current_image_lock(previous):
        backup = STATE / ('image.previous-' + str(time.time_ns()) + '.json')
        write_json(backup, previous)
        print('旧镜像锁已备份:', backup, flush=True)
    write_json(lock, {'schema': 2, 'source': IMAGE, 'platform': IMAGE_PLATFORM,
                     'recorded_identity': RECORDED_IDENTITY,
                     'id': info['Id'], 'repo_digests': info.get('RepoDigests', [])})
    print('镜像身份校验通过:', IMAGE, flush=True)
    return IMAGE


def verify_image(info):
    require(info.get('Architecture') == 'arm64' and info.get('Os') == 'linux',
            '镜像平台不是 linux/arm64，停止部署')
    # A classic store's Id can be a config hash; a containerd store's Id can
    # be a manifest/index hash. Neither is used as the portable trust anchor.
    refs = info.get('RepoDigests') or []
    accepted = {IMAGE, 'docker.io/' + IMAGE, 'index.docker.io/' + IMAGE}
    require(any(ref in accepted for ref in refs),
            '镜像清单身份不一致；期望 ' + IMAGE +
            '；实际 Id=' + str(info.get('Id')) + '，RepoDigests=' + repr(refs) +
            '。请按固定摘要重新拉取；不要删除校验或改用浮动标签。')


def current_image_lock(record):
    return (isinstance(record, dict) and record.get('schema') == 2 and
            record.get('source') == IMAGE and record.get('platform') == IMAGE_PLATFORM and
            record.get('recorded_identity') == RECORDED_IDENTITY)


def locked_image():
    lock = STATE / 'image.json'
    require(lock.is_file(), '缺少镜像锁，请先运行 engine')
    record = json.loads(lock.read_text(encoding='utf-8'))
    require(current_image_lock(record), '镜像锁属于旧版或其他配方；请先 stop，再运行 engine 迁移')
    try:
        info = image_info(IMAGE)
    except subprocess.CalledProcessError as error:
        raise RuntimeError('固定镜像缺失，请运行 engine 恢复；不会自动换版') from error
    verify_image(info)
    return IMAGE


def engine(c):
    preflight(c)
    ensure_stopped()
    gpu_probe(image_prepare())
    print('引擎准备与GPU/补丁路径检查通过；尚未加载模型或验收推理。', flush=True)


def mount(source, destination, readonly=False):
    return ['--mount', f'type=bind,src={source},dst={destination}' + (',readonly' if readonly else '')]


def gpu_probe(image):
    code = ('import torch,pathlib; assert torch.cuda.device_count()==1; '
            'assert torch.cuda.get_device_capability()==(12,1); '
            f'paths={list(PATCHES.values())!r}; '
            f'assert all((pathlib.Path({VP!r})/p).is_file() for p in paths); '
            'print("GPU:",torch.cuda.get_device_name(),"; runtime patch targets present")')
    run(['docker', 'run', '--rm', '--gpus', 'all', '--entrypoint', 'python3', image, '-c', code])


def download(c):
    preflight(c)
    ensure_stopped()
    model = Path(c['model_dir'])
    used = sum(f.stat().st_size for f in model.rglob('*') if f.is_file())
    require(shutil.disk_usage(model).free >= max(0, 133_000_000_000 - used) + 20 * 1024**3,
            '模型盘空间不足；需容纳约133GB权重及至少20GiB余量')
    image = image_prepare()
    gpu_probe(image)
    args = ['docker', 'run', '--rm', '--user', f'{os.getuid()}:{os.getgid()}',
            '-e', 'HOME=/tmp', '-e', 'HF_TOKEN', '-e', 'HF_HUB_DOWNLOAD_TIMEOUT=120',
            '-e', 'HF_HUB_ETAG_TIMEOUT=30', '-e', 'HF_HUB_DISABLE_XET=1']
    args += mount(model, '/model') + mount(STATE, '/state') + mount(ROOT / 'model_assets.py', '/tools/model_assets.py', True)
    args += ['--entrypoint', 'python3', image, '/tools/model_assets.py', 'download', '--revision', c['revision']]
    run(args)
    print('下载和全量校验完成。')


def launch_args(c, image, fingerprint):
    args = ['docker', 'create', '--name', NAME, '--label', 'megacube.owner=' + OWNER,
            '--label', 'megacube.install=' + str(ROOT), '--label', 'megacube.recipe=' + fingerprint,
            '--restart', 'no', '--gpus', 'all', '--network', 'host', '--ipc', 'host',
            '--shm-size', '32g', '--ulimit', 'memlock=-1:-1',
            '--log-opt', 'max-size=20m', '--log-opt', 'max-file=5']
    args += mount(c['model_dir'], '/models/qwen38fn', True) + mount(c['cache_dir'], '/root/.cache')
    for src, dst in PATCHES.items():
        args += mount(ROOT / 'runtime/patch' / src, VP + dst, True)
    env = dict(HF_HUB_OFFLINE='1', TRANSFORMERS_OFFLINE='1', VLLM_ENGINE_READY_TIMEOUT_S='3600',
               PYTORCH_CUDA_ALLOC_CONF='expandable_segments:True', CUTE_DSL_ARCH='sm_121a',
               TORCH_CUDA_ARCH_LIST='12.1a', FLASHINFER_CUDA_ARCH_LIST='12.1a', FLASHINFER_DISABLE_VERSION_CHECK='1',
               VLLM_USE_DEEP_GEMM='0', VLLM_USE_V2_MODEL_RUNNER='1', QWEN4EXP_PLE_MMAP='1',
               QWEN4EXP_PLE_STAGED='1', QWEN4EXP_PLE_MMAP_THREADS='64', QWEN4EXP_DRAFT_VOCAB='65536')
    for k, v in env.items():
        args += ['-e', k + '=' + v]
    # Preserve the tested image's default entrypoint and positional model argument.
    args += [image, '/models/qwen38fn', '--served-model-name', 'qwen3.8-flash-next',
             '--host', c['bind'], '--port', str(c['port']), '--trust-remote-code',
             '--quantization', 'modelopt', '--tensor-parallel-size', '1', '--max-model-len', '262144',
             '--max-num-seqs', '1', '--max-num-batched-tokens', '2048', '--gpu-memory-utilization', '0.80',
             '--no-enable-flashinfer-autotune', '--enable-prefix-caching',
             '--reasoning-parser', 'qwen3', '--enable-auto-tool-choice', '--tool-call-parser', 'qwen3_xml',
             '--default-chat-template-kwargs', '{"enable_thinking":false}',
             '--speculative-config', '{"method":"mtp","num_speculative_tokens":3}']
    return args


def api(c, path, body=None, timeout=10):
    host = '127.0.0.1' if c['bind'] == '0.0.0.0' else c['bind']
    request = urllib.request.Request(f"http://{host}:{c['port']}{path}",
                                    data=None if body is None else json.dumps(body).encode(),
                                    headers={'Content-Type': 'application/json'})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(request, timeout=timeout) as response:
        return response.read()


def smoke(c, timeout=120):
    models = json.loads(api(c, '/v1/models', timeout=min(10, timeout)))
    require(any(r.get('id') == 'qwen3.8-flash-next' for r in models.get('data', [])), 'API模型名称不匹配')
    body = {'model': 'qwen3.8-flash-next', 'messages': [{'role': 'user', 'content': '请只回答：OK'}],
            'temperature': 0, 'max_tokens': 32, 'chat_template_kwargs': {'enable_thinking': False}}
    result = json.loads(api(c, '/v1/chat/completions', body, timeout=timeout))
    require(result.get('choices') and result['choices'][0].get('message', {}).get('content'), '生成请求无有效内容')
    print('生成冒烟通过:', result['choices'][0]['message']['content'])


def fingerprint(c, image):
    text = json.dumps({'config': c, 'image': image,
                       'bundle': hashlib.sha256((ROOT / 'SHA256SUMS').read_bytes()).hexdigest(),
                       'model': hashlib.sha256((STATE / 'model-source.json').read_bytes()).hexdigest()}, sort_keys=True)
    return hashlib.sha256(text.encode()).hexdigest()


def save_logs():
    log = ROOT / 'logs'
    log.mkdir(exist_ok=True)
    path = log / (time.strftime('%Y%m%d-%H%M%S') + '-service.log')
    with path.open('w', encoding='utf-8') as stream:
        result = subprocess.run(['docker', 'logs', '--tail', '2000', NAME], text=True,
                                stdout=stream, stderr=subprocess.STDOUT)
    require(result.returncode == 0, '日志归档失败；未继续删除容器')
    print('日志已保存:', path)


def stop():
    info = inspect_container()
    if not info:
        print('服务未安装，权重保留。'); return
    if info['State'].get('Running'):
        run(['docker', 'stop', '--time', '60', NAME])
    print('服务已停止，容器、权重和缓存保留。')


def uninstall():
    if not inspect_container():
        print('服务未安装，权重保留。'); return
    stop()
    save_logs()
    run(['docker', 'rm', NAME])
    print('服务容器已卸载；权重、缓存、配置、日志和镜像全部保留。')


def wait_ready(c):
    deadline = time.monotonic() + c['startup_timeout']
    while time.monotonic() < deadline:
        info = inspect_container()
        require(info and info['State'].get('Running'), '容器已退出，请查看logs')
        try:
            api(c, '/health', timeout=min(5, max(1, deadline - time.monotonic())))
            remaining = deadline - time.monotonic()
            require(remaining > 0, '启动超时')
            smoke(c, timeout=min(120, remaining))
            return
        except (OSError, ValueError, RuntimeError) as error:
            print('加载/预热中:', str(error)[:180], flush=True)
            time.sleep(min(15, max(0, deadline - time.monotonic())))
    raise RuntimeError('启动超时，首次加载通常需12~14分钟；请查日志和可用内存')


def start(c):
    preflight(c)
    quick_verify(Path(c['model_dir']), STATE)
    require(not c['revision'] or c['revision'] == load_source(STATE / 'model-source.json')['revision'],
            'config中的revision与已下载模型不一致')
    image = locked_image()
    signature = fingerprint(c, image)
    info = inspect_container()
    if info and info['State'].get('Running'):
        require(info['Config']['Labels'].get('megacube.recipe') == signature, '运行配置已变化，请先stop再start')
        wait_ready(c)
        print('现有服务已就绪。'); return
    available = int(re.search(r'MemAvailable:\s+(\d+)', Path('/proc/meminfo').read_text())[1])
    require(available >= 100 * 1024**2, '可用统一内存不足100GiB，请先停止其他重负载；不会自动停止其他服务')
    with socket.socket() as sock:
        sock.bind((c['bind'], c['port']))
    gpu_probe(image)
    if info:
        save_logs()
        run(['docker', 'rm', NAME])
    created = False
    try:
        run(launch_args(c, image, signature)); created = True
        run(['docker', 'start', NAME])
        wait_ready(c)
    except BaseException:
        if created:
            try:
                save_logs()
            finally:
                stop()
        raise
    host = '<本机业务IP>' if c['bind'] == '0.0.0.0' else c['bind']
    print(f"启动完成。API: http://{host}:{c['port']}/v1")


def status(c):
    info = inspect_container()
    if not info:
        print('未安装服务容器；模型目录:', c['model_dir']); return
    print('容器:', NAME, '| 状态:', info['State']['Status'], '| OOMKilled:', info['State'].get('OOMKilled'))
    if info['State'].get('Running'):
        try:
            api(c, '/health')
            print('HTTP健康检查通过；真实生成可执行 smoke-test。')
        except OSError:
            raise RuntimeError('容器运行中，但API尚未就绪或不可用')


def main():
    p = argparse.ArgumentParser(description='Qwen3.8-Flash-Next NVFP4 单台MegaCube部署管理')
    p.add_argument('command', choices=['init', 'check', 'engine', 'download', 'deploy', 'start', 'stop', 'status', 'logs', 'verify', 'smoke-test', 'uninstall'])
    a = p.parse_args()
    require(platform.system() == 'Linux', '脚本在MegaCube Linux上执行，Windows只用于打包/测试')
    # Lock independent of state/model directories. Read-only diagnostics remain available during startup.
    import fcntl
    with (ROOT / '.operation.lock').open('a') as lock:
        if a.command not in ('status', 'logs'):
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise RuntimeError('另一个部署/启停操作正在执行；如需中断启动，请在启动终端Ctrl+C')
        c = configuration()
        if a.command == 'init':
            print('配置文件:', CONFIG); return
        docker_available()
        if a.command == 'engine': engine(c)
        elif a.command == 'download': download(c)
        elif a.command == 'deploy': download(c); start(c)
        elif a.command == 'start': start(c)
        elif a.command == 'stop': stop()
        elif a.command == 'uninstall': uninstall()
        elif a.command == 'status': status(c)
        elif a.command == 'check': preflight(c); gpu_probe(locked_image()); print('系统与GPU检查通过。')
        elif a.command == 'verify':
            ensure_stopped(); verify(Path(c['model_dir']), STATE)
        elif a.command == 'smoke-test':
            info = inspect_container()
            require(info and info['State'].get('Running'), '本服务未运行')
            smoke(c)
        elif a.command == 'logs':
            require(inspect_container(), '服务容器不存在；历史日志在logs目录')
            run(['docker', 'logs', '--tail', '200', NAME])


if __name__ == '__main__':
    try:
        main()
    except (Exception, KeyboardInterrupt) as error:
        print('失败:', str(error), file=sys.stderr)
        sys.exit(1)

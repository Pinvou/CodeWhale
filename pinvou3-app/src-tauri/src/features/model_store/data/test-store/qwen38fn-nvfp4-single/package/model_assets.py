#!/usr/bin/env python3
"""Pinned Hub download and authoritative file verification, run inside runtime image."""
import argparse
import hashlib
import json
from pathlib import Path
import re

REPO = 'nvidia/Qwen3.8-Flash-Next-NVFP4'


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def write_json(path, data):
    tmp = path.with_suffix('.tmp')
    tmp.write_text(json.dumps(data, indent=2) + '\n', encoding='utf-8')
    tmp.replace(path)


def safe_file(root, name):
    require(isinstance(name, str) and name and '\\' not in name, '非法模型文件名')
    path = root / name
    require(not Path(name).is_absolute() and '..' not in Path(name).parts and
            path.resolve().is_relative_to(root.resolve()), '模型文件路径越界: ' + name)
    require(not any(p.is_symlink() for p in [path, *path.parents] if p != root.parent),
            '模型目录不允许符号链接: ' + name)
    return path


def load_source(path):
    s = json.loads(path.read_text(encoding='utf-8'))
    require(s.get('repo') == REPO and re.fullmatch('[0-9a-f]{40}', s.get('revision', '')), '模型来源锁文件非法')
    require(bool(s.get('files')), '模型文件清单为空')
    names = [row['name'] for row in s['files']]
    require(len(set(names)) == len(names), '模型清单含重复文件')
    for row in s['files']:
        require(isinstance(row['size'], int) and row['size'] >= 0, '文件尺寸非法')
        require(row['algorithm'] in ('sha256', 'git-sha1'), '不支持的校验算法')
        require(re.fullmatch('[0-9a-f]{' + ('64' if row['algorithm'] == 'sha256' else '40') + '}', row['digest']),
                '文件摘要非法')
    return s


def file_matches(root, row):
    path = safe_file(root, row['name'])
    if not path.is_file() or path.stat().st_size != row['size']:
        return False
    h = hashlib.sha256() if row['algorithm'] == 'sha256' else hashlib.sha1()
    if row['algorithm'] == 'git-sha1':
        h.update(f"blob {row['size']}\0".encode())
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(8 * 1024 * 1024), b''):
            h.update(block)
    return h.hexdigest() == row['digest']


def structure(root, source):
    known = {row['name'] for row in source['files']}
    require({'config.json', 'model.safetensors.index.json'} <= known, '缺少配置/权重索引')
    index = json.loads(safe_file(root, 'model.safetensors.index.json').read_text())
    require(bool(index.get('weight_map')), '权重索引为空')
    require(set(index['weight_map'].values()) <= known, '索引包含来源清单之外的权重分片')
    cfg = json.loads(safe_file(root, 'config.json').read_text())
    require('Qwen4ExpForConditionalGeneration' in cfg.get('architectures', []), '模型架构不符合归档单机配方')
    require(any('tokenizer' in name for name in known), '缺少 tokenizer 文件')
    actual = {p.relative_to(root).as_posix() for p in root.rglob('*.safetensors')}
    require(actual == {n for n in known if n.endswith('.safetensors')}, '存在缺失或额外的 safetensors 文件')


def verify(root, state, repair=None):
    marker = state / 'model-verified.json'
    marker.unlink(missing_ok=True)
    source = load_source(state / 'model-source.json')
    stats = {}
    for row in source['files']:
        print('校验 ' + row['name'], flush=True)
        valid = file_matches(root, row)
        if not valid and repair is not None:
            repair(row['name'])
            valid = file_matches(root, row)
        require(valid, '官方摘要/大小校验失败: ' + row['name'])
        st = (root / row['name']).stat()
        stats[row['name']] = [st.st_size, st.st_mtime_ns]
    structure(root, source)
    write_json(marker, {'source_sha256': hashlib.sha256((state / 'model-source.json').read_bytes()).hexdigest(),
                        'stats': stats})
    print('模型全量校验通过。', flush=True)


def quick_verify(root, state):
    source = load_source(state / 'model-source.json')
    marker = json.loads((state / 'model-verified.json').read_text())
    require(marker['source_sha256'] == hashlib.sha256((state / 'model-source.json').read_bytes()).hexdigest(),
            '模型来源变化，请执行 verify')
    require(set(marker['stats']) == {r['name'] for r in source['files']}, '校验记录不完整')
    for row in source['files']:
        st = safe_file(root, row['name']).stat()
        require(marker['stats'][row['name']] == [st.st_size, st.st_mtime_ns],
                '模型文件变化，请执行 verify: ' + row['name'])
    structure(root, source)


def download(root, state, revision):
    from huggingface_hub import HfApi, hf_hub_download, snapshot_download
    state.mkdir(parents=True, exist_ok=True)
    root.mkdir(parents=True, exist_ok=True)
    source_path = state / 'model-source.json'
    (state / 'model-verified.json').unlink(missing_ok=True)
    if source_path.exists():
        source = load_source(source_path)
        require(not revision or source['revision'] == revision, 'revision与已锁定版本冲突，请勿覆盖旧模型来源')
    else:
        info = HfApi().model_info(REPO, revision=revision or 'main', files_metadata=True)
        require(re.fullmatch('[0-9a-f]{40}', info.sha or ''), 'Hub未返回确定的commit SHA')
        rows = []
        for file in info.siblings:
            safe_file(root, file.rfilename)
            rows.append({'name': file.rfilename, 'size': file.size,
                         'algorithm': 'sha256' if file.lfs else 'git-sha1',
                         'digest': file.lfs.sha256 if file.lfs else file.blob_id})
        source = {'repo': REPO, 'revision': info.sha, 'files': rows}
        write_json(source_path, source)
        source = load_source(source_path)
    print('模型锁定revision: ' + source['revision'], flush=True)
    snapshot_download(REPO, revision=source['revision'], local_dir=str(root), max_workers=4,
                      allow_patterns=[r['name'] for r in source['files']])
    # Hub's local metadata alone does not detect edited cached bytes; repair mismatches once.
    def repair(name):
        print('重新下载校验不一致文件: ' + name, flush=True)
        hf_hub_download(REPO, name, revision=source['revision'], local_dir=str(root), force_download=True)
    verify(root, state, repair=repair)


if __name__ == '__main__':
    p = argparse.ArgumentParser()
    p.add_argument('action', choices=['download', 'verify'])
    p.add_argument('--revision', default='')
    p.add_argument('--model', type=Path, default=Path('/model'))
    p.add_argument('--state', type=Path, default=Path('/state'))
    a = p.parse_args()
    if a.action == 'download': download(a.model, a.state, a.revision)
    else: verify(a.model, a.state)

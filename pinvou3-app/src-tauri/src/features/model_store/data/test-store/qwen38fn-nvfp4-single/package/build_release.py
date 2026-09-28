#!/usr/bin/env python3
"""Explicit allowlist packaging: no config/state/model/cache/log files."""
import ast
import hashlib
from pathlib import Path
import tarfile

ROOT = Path(__file__).resolve().parent
VERSION = '1.0.0-rc3'


def main():
    names = ['megacube.sh', 'controller.py', 'model_assets.py', 'test_package.py',
             'build_release.py', 'README.md', 'SOURCE.md', 'VALIDATION.md',
             'IMAGE-PROVENANCE.json', 'UPGRADE.md']
    files = [ROOT / name for name in names]
    files += [p for p in (ROOT / 'runtime').rglob('*') if p.is_file() and
              p.suffix != '.pyc' and '__pycache__' not in p.parts]
    for file in files:
        text = file.read_text(encoding='utf-8').replace('\r\n', '\n')
        file.write_bytes(text.encode('utf-8'))
        if file.suffix == '.py': ast.parse(text, filename=str(file))
    import controller
    for name in controller.PATCHES:
        assert (ROOT / 'runtime/patch' / name).is_file(), name
    manifest = ROOT / 'SHA256SUMS'
    manifest.write_bytes(''.join(hashlib.sha256(f.read_bytes()).hexdigest() + '  ' +
                         f.relative_to(ROOT).as_posix() + '\n' for f in sorted(files)).encode())
    files.append(manifest)
    out = ROOT.parent / 'release/single' / VERSION
    out.mkdir(parents=True, exist_ok=True)
    archive = out / f'MegaCube-Qwen3.8-Flash-Next-NVFP4-single-{VERSION}.tar.gz'
    with tarfile.open(archive, 'w:gz') as tar:
        for file in sorted(files):
            info = tar.gettarinfo(str(file), arcname='megacube-qwen38fn-single/' + file.relative_to(ROOT).as_posix())
            info.uid = info.gid = 0; info.uname = info.gname = ''
            info.mode = 0o755 if file.suffix == '.sh' else 0o644
            with file.open('rb') as stream: tar.addfile(info, stream)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_suffix('.gz.sha256').write_bytes((digest + '  ' + archive.name + '\n').encode())
    with tarfile.open(archive) as tar:
        assert len(tar.getmembers()) == len(files)
        for file in files:
            name = 'megacube-qwen38fn-single/' + file.relative_to(ROOT).as_posix()
            assert tar.getmember(name).isfile()
            assert tar.extractfile(name).read() == file.read_bytes()
    controller.bundle_check()
    print(f'打包及校验通过：{len(files)}个文件\n{archive}\nSHA256: {digest}')


if __name__ == '__main__': main()

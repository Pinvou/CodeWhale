#!/usr/bin/env python3
"""Package only an explicit allowlist; never include customer state or weights."""
import ast
import hashlib
from pathlib import Path
import re
import tarfile

ROOT = Path(__file__).resolve().parent
VERSION = '1.0.0-rc1'


def main():
    files = [ROOT / name for name in ['megacube.sh', 'controller.py', 'README.md',
             'test_controller.py', 'build_release.py', 'VALIDATION.md']]
    files += [ROOT / 'runtime/docker-compose.dspark.yml', ROOT / 'runtime/LICENSE.upstream']
    files += [p for p in (ROOT / 'runtime/patches').rglob('*') if p.is_file() and
              '__pycache__' not in p.parts and p.suffix != '.pyc']
    for path in files:
        text = path.read_text(encoding='utf-8')
        path.write_bytes(text.replace('\r\n', '\n').encode('utf-8'))
        if path.suffix == '.py':
            ast.parse(text, filename=str(path))
    compose = (ROOT / 'runtime/docker-compose.dspark.yml').read_text(encoding='utf-8')
    for relative in re.findall(r'\./patches/([^}]+)', compose):
        assert (ROOT / 'runtime/patches' / relative).is_file(), relative
    manifest = ROOT / 'SHA256SUMS'
    manifest.write_bytes(''.join(hashlib.sha256(p.read_bytes()).hexdigest() + '  ' +
                        p.relative_to(ROOT).as_posix() + '\n' for p in sorted(files)).encode())
    files.append(manifest)
    release = ROOT.parent / 'release/direct' / VERSION
    release.mkdir(parents=True, exist_ok=True)
    archive = release / f'megacube-deepseek-direct-{VERSION}.tar.gz'
    with tarfile.open(archive, 'w:gz') as tar:
        for path in sorted(files):
            info = tar.gettarinfo(str(path), arcname='megacube-deepseek/' + path.relative_to(ROOT).as_posix())
            info.uid = info.gid = 0
            info.uname = info.gname = ''
            info.mode = 0o755 if path.suffix == '.sh' else 0o644
            with path.open('rb') as stream:
                tar.addfile(info, stream)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_suffix(archive.suffix + '.sha256').write_bytes((digest + '  ' + archive.name + '\n').encode())
    with tarfile.open(archive) as tar:
        members = tar.getmembers()
        assert len(members) == len(files)
        assert all(m.isfile() and m.name.startswith('megacube-deepseek/') and '..' not in Path(m.name).parts for m in members)
        for path in files:
            name = 'megacube-deepseek/' + path.relative_to(ROOT).as_posix()
            assert tar.extractfile(name).read() == path.read_bytes()
    print(f'Built and verified {len(files)} files: {archive}\nSHA256: {digest}')


if __name__ == '__main__':
    main()

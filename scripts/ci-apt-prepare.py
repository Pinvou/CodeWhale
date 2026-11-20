#!/usr/bin/env python3
"""Refresh the pinned LLVM signing certificate only when that source is used."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

KEY_SHA256 = '8b2a587ffd672c4687e7581dad4b2f6c1bb2ad6b480cd9771ba2ff48e0b8c75d'
PRIMARY = '6084F3CF814B57C1CF12EFD515CF4D18AF4F7421'
SIGNING_SUBKEY = '56E1477F57BAB5ECF37818045B3D8846AF9463CE'


def targets(root):
    root = Path(root)
    sources = [root / 'etc/apt/sources.list']
    sources += list((root / 'etc/apt/sources.list.d').glob('*.list'))
    sources += list((root / 'etc/apt/sources.list.d').glob('*.sources'))
    result = set()
    for source in sources:
        if not source.is_file():
            continue
        text = '\n'.join(line.split('#', 1)[0] for line in source.read_text().splitlines())
        blocks = text.split('\n\n') if source.suffix == '.sources' else text.splitlines()
        for block in blocks:
            if not re.search(r'https?://apt\.llvm\.org(?:/|\s|$)', block):
                continue
            if re.search(r'(?im)^Enabled:\s*no\s*$', block):
                continue
            match = re.search(r'(?im)(?:signed-by=|^Signed-By:\s*)([^\]\s]+)', block)
            if not match:
                result.add(root / 'etc/apt/trusted.gpg.d/pinvou-llvm.gpg')
                continue
            value = match[1]
            if (not value.startswith(('/etc/apt/', '/usr/share/keyrings/'))
                    or '..' in Path(value).parts or not re.fullmatch(r'[A-Za-z0-9_./-]+\.(gpg|asc)', value)
                    or (source.suffix == '.sources' and
                        re.search(r'(?im)^Signed-By:\s*(.+)$', block)[1].strip() != value)):
                raise ValueError('unsupported LLVM signing key scope; refusing a trust-policy change')
            result.add(root / value.lstrip('/'))
    return sorted(result)


def prepare(root=Path('/'), *, key=None):
    paths = targets(root)
    if not paths:
        print(json.dumps({'phase': 'apt-signing', 'llvm_source': False}))
        return []
    key = key or Path(__file__).parent / 'vendor/llvm-snapshot.asc'
    data = key.read_bytes()
    if hashlib.sha256(data).hexdigest() != KEY_SHA256:
        raise ValueError('LLVM public certificate SHA-256 mismatch')
    changed = []
    with tempfile.TemporaryDirectory(prefix='ci-apt-key-') as folder:
        home = Path(folder)

        def gpg(args, contents=None):
            return subprocess.run(['gpg', '--batch', '--homedir', str(home), *args],
                                  input=contents, capture_output=True, check=True, timeout=15).stdout

        keys = gpg(['--with-colons', '--show-keys'], data).decode().splitlines()
        fingerprints = [line.split(':')[9] for line in keys if line.startswith('fpr:')]
        if fingerprints != [PRIMARY, SIGNING_SUBKEY]:
            raise ValueError('unexpected LLVM public certificate identity')
        for path in paths:
            if path.is_symlink():
                raise ValueError('refusing symbolic-link keyring output')
            old = path.read_bytes() if path.exists() else b''
            # Export existing identities plus the refreshed LLVM certificate.
            # Each destination is handled separately to avoid widening trust
            # by copying keys between unrelated Signed-By scopes.
            separate = home / ('scope-' + hashlib.sha256(str(path).encode()).hexdigest()[:12])
            separate.mkdir(mode=0o700)
            def scoped(args, content=None):
                return subprocess.run(['gpg', '--batch', '--homedir', str(separate), *args],
                                      input=content, capture_output=True, check=True, timeout=15).stdout
            if old:
                scoped(['--import'], old)
            scoped(['--import'], data)
            content = scoped((['--armor'] if path.suffix == '.asc' else []) + ['--export'])
            if old == content:
                continue
            path.parent.mkdir(parents=True, exist_ok=True)
            fd, name = tempfile.mkstemp(prefix='.llvm-key-', dir=path.parent)
            try:
                with os.fdopen(fd, 'wb') as output:
                    output.write(content)
                    output.flush()
                    os.fsync(output.fileno())
                os.chmod(name, 0o644)
                os.replace(name, path)
            finally:
                Path(name).unlink(missing_ok=True)
            changed.append(path.name)
    print(json.dumps({'phase': 'apt-signing', 'llvm_source': True, 'updated_keyrings': changed,
                      'primary_fingerprint': PRIMARY, 'signing_subkey': SIGNING_SUBKEY}))
    return changed


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true', required=True)
    parser.parse_args()
    prepare()

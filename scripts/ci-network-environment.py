#!/usr/bin/env python3
"""Retire only the known CI rsproxy override; preserve isolated Rust homes."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import tempfile


def retire_mirror(path):
    path = Path(path)
    if not path.exists():
        return False
    if path.is_symlink() or not path.is_file():
        raise ValueError('Cargo configuration must be a regular file')
    text = path.read_text(encoding='utf-8')
    source = r'(?m)^\[source\.crates-io\]\s*\nreplace-with\s*=\s*"rsproxy-ci"\s*\n'
    mirror = r'(?m)^\[source\.rsproxy-ci\]\s*\nregistry\s*=\s*"sparse\+https://rsproxy\.cn/index/"\s*\n'
    if not re.search(source, text) or not re.search(mirror, text):
        return False
    # Refuse extra properties in either stanza rather than silently discarding
    # an operator's custom registry policy. Retain unrelated build/profile keys.
    for section in ('source.crates-io', 'source.rsproxy-ci'):
        block = re.search(r'(?ms)^\[' + re.escape(section) + r'\]\n(.*?)(?=^\[|\Z)', text)
        lines = [line for line in block[1].splitlines() if line.strip() and not line.lstrip().startswith('#')]
        if len(lines) != 1:
            raise ValueError('Custom Cargo source policy needs explicit review')
    updated = re.sub(mirror, '', re.sub(source, '', text))
    backup = path.with_name(path.name + '.before-network-common-' + hashlib.sha256(text.encode()).hexdigest()[:12])
    if not backup.exists():
        with backup.open('x', encoding='utf-8') as out:
            out.write(text)
        os.chmod(backup, path.stat().st_mode & 0o777)
    fd, name = tempfile.mkstemp(dir=path.parent, prefix='.network-config-')
    try:
        with os.fdopen(fd, 'w', encoding='utf-8', newline='\n') as out:
            out.write(updated)
        os.chmod(name, path.stat().st_mode & 0o777)
        os.replace(name, path)
    finally:
        Path(name).unlink(missing_ok=True)
    return True


def prepare(cargo, cache, environment):
    changed = [name for name in ('config', 'config.toml') if retire_mirror(Path(cargo) / name)]
    values = {'RUSTUP_DIST_SERVER': 'https://static.rust-lang.org',
              'RUSTUP_UPDATE_ROOT': 'https://static.rust-lang.org/rustup',
              'PINVOU_CI_DOWNLOAD_CACHE': str(cache)}
    if any('\n' in value or '\r' in value for value in values.values()):
        raise ValueError('Invalid network environment value')
    with Path(environment).open('a', encoding='utf-8') as out:
        for key, value in values.items():
            out.write(f'{key}={value}\n')
    print(json.dumps({'phase': 'network-environment', 'retired_mirror_configs': changed,
                      'rust_source': 'official', 'isolated_toolchains_preserved': True}))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cargo-home', required=True, type=Path)
    parser.add_argument('--cache', required=True, type=Path)
    args = parser.parse_args()
    prepare(args.cargo_home, args.cache, os.environ['GITHUB_ENV'])

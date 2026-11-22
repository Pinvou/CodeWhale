#!/usr/bin/env python3
"""Select CI Rust sources and verified downloads; preserve isolated Rust homes."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import tempfile
import sys

MIRROR_CONFIG = ('[source.crates-io]\nreplace-with = "rsproxy-ci"\n\n'
                 '[source.rsproxy-ci]\nregistry = "sparse+https://rsproxy.cn/index/"\n')


def cargo_source(cargo, mirror):
    paths = [Path(cargo) / name for name in ('config', 'config.toml')]
    if mirror != 'rsproxy':
        return [path.name for path in paths if retire_mirror(path)]
    if any(p.is_symlink() for p in paths):
        raise ValueError('Cargo configuration must be a regular file')
    existing = [p for p in paths if p.exists()]
    if len(existing) > 1:
        raise ValueError('Ambiguous Cargo configuration needs explicit review')
    path = existing[0] if existing else paths[1]
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise ValueError('Cargo configuration must be a regular file')
    text = path.read_text(encoding='utf-8') if path.exists() else ''
    retired = retired_mirror_text(text)
    # Preserve custom registries rather than silently replacing operator policy.
    if re.search(r'(?m)^\s*\[\s*source(?:\.|\s*\])', text if retired is None else retired):
        raise ValueError('Custom Cargo source policy needs explicit review')
    if retired is not None:
        return []  # The exact owned mirror is already active; no rewrite window.
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(dir=path.parent, prefix='.cargo-source-')
    try:
        with os.fdopen(fd, 'w', encoding='utf-8', newline='\n') as out:
            out.write(text.rstrip() + '\n\n' + MIRROR_CONFIG)
        os.chmod(temporary, path.stat().st_mode & 0o777 if path.exists() else 0o600)
        os.replace(temporary, path)
    finally:
        Path(temporary).unlink(missing_ok=True)
    return []


def retired_mirror_text(text):
    source = r'(?m)^\[source\.crates-io\]\s*\nreplace-with\s*=\s*"rsproxy-ci"\s*\n'
    mirror = r'(?m)^\[source\.rsproxy-ci\]\s*\nregistry\s*=\s*"sparse\+https://rsproxy\.cn/index/"\s*\n'
    if not re.search(source, text) or not re.search(mirror, text):
        return None
    # Refuse extra properties in either stanza rather than silently discarding
    # an operator's custom registry policy. Retain unrelated build/profile keys.
    for section in ('source.crates-io', 'source.rsproxy-ci'):
        block = re.search(r'(?ms)^\[' + re.escape(section) + r'\]\n(.*?)(?=^\[|\Z)', text)
        lines = [line for line in block[1].splitlines() if line.strip() and not line.lstrip().startswith('#')]
        if len(lines) != 1:
            raise ValueError('Custom Cargo source policy needs explicit review')
    return re.sub(mirror, '', re.sub(source, '', text))


def retire_mirror(path):
    path = Path(path)
    if not path.exists():
        return False
    if path.is_symlink() or not path.is_file():
        raise ValueError('Cargo configuration must be a regular file')
    text = path.read_text(encoding='utf-8')
    updated = retired_mirror_text(text)
    if updated is None:
        return False
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


def prepare(cargo, cache, environment, *, dist_cache=False):
    default = 'rsproxy' if sys.platform == 'linux' and os.environ.get('RUNNER_ENVIRONMENT') == 'self-hosted' else 'official'
    mirror = os.environ.get('PINVOU_CI_RUST_MIRROR', default)
    if mirror not in ('official', 'rsproxy'):
        raise ValueError('Unsupported CI Rust mirror')
    changed = cargo_source(cargo, mirror)
    values = {'RUSTUP_DIST_SERVER': 'https://static.rust-lang.org',
              'RUSTUP_UPDATE_ROOT': 'https://static.rust-lang.org/rustup',
              'PINVOU_CI_DOWNLOAD_CACHE': str(cache),
              'PINVOU_CI_RUST_MIRROR': mirror,
              'PINVOU_CI_CARGO_SOURCE': mirror}
    if dist_cache and os.environ.get('GITHUB_ACTIONS') == 'true':
        spec = importlib.util.spec_from_file_location('ci_rust_dist', Path(__file__).with_name('ci-rust-dist.py'))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        url, state, _child = module.start(cache, os.environ.get('RUNNER_TEMP'), mirror=mirror)
        values.update(RUSTUP_DIST_SERVER=url, PINVOU_CI_RUST_DIST_STATE=str(state),
                      PINVOU_CI_RUST_DIST_ORIGIN=module.ORIGIN)
        for name in ('NO_PROXY', 'no_proxy'):
            entries = [entry.strip() for entry in os.environ.get(name, '').split(',') if entry.strip()]
            values[name] = ','.join(dict.fromkeys([*entries, 'localhost', '127.0.0.1', '::1',
                                                  *(['rsproxy.cn', '.rsproxy.cn'] if mirror == 'rsproxy' else [])]))
    if any('\n' in value or '\r' in value for value in values.values()):
        raise ValueError('Invalid network environment value')
    with Path(environment).open('a', encoding='utf-8') as out:
        for key, value in values.items():
            out.write(f'{key}={value}\n')
    print(json.dumps({'phase': 'network-environment', 'retired_mirror_configs': changed,
                      'cargo_source': mirror,
                      'rust_source': ('official-checksum-mirror-cache' if mirror == 'rsproxy' else 'verified-official-cache')
                      if 'PINVOU_CI_RUST_DIST_STATE' in values else 'official',
                      'isolated_toolchains_preserved': True}))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cargo-home', required=True, type=Path)
    parser.add_argument('--cache', required=True, type=Path)
    parser.add_argument('--dist-cache', action='store_true')
    args = parser.parse_args()
    prepare(args.cargo_home, args.cache, os.environ['GITHUB_ENV'], dist_cache=args.dist_cache)

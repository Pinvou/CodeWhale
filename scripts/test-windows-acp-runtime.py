#!/usr/bin/env python3
"""Run the Windows ACP smoke from fixed Git sources in a supervised private app.

The caller must use ci-fixture-process.py. Its private directory and Windows Job
remain the lifetime boundary, including on failure/cancellation. Persistent
build/runtime caches in the checkout are never copied, changed or reclaimed.
"""
import argparse
import io
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
SOURCE_PATHS = (
    'pinvou3-app/package.json',
    'pinvou3-app/scripts/tauri',
    'pinvou3-app/scripts/codex-bridge-runtime',
    'pinvou3-app/src-tauri/tauri.conf.json',
    'pinvou3-app/src-tauri/config',
    'pinvou3-app/src-tauri/resources/common/config',
    'pinvou3-app/src-tauri/resources/platforms/windows/chrome-devtools-mcp',
    'pinvou3-app/tests/codex_acp_windows_runtime_smoke.js',
)


def supervised_root(environment):
    root = Path(environment.get('PINVOU_CI_FIXTURE_ROOT', ''))
    scope = environment.get('PINVOU_CI_PROCESS_SCOPE', '')
    if (not root.is_absolute() or root.is_symlink() or not root.is_dir()
            or not re.fullmatch('[0-9a-f]{32}', scope)
            or (root/'.pinvou-fixture-scope').read_text(encoding='ascii') != scope):
        raise RuntimeError('ACP smoke requires its owning ci-fixture-process scope')
    return root


def materialize(raw, destination):
    """Copy regular tracked inputs only, never links or archive-selected paths."""
    destination = destination.resolve()
    with tarfile.open(fileobj=io.BytesIO(raw), mode='r:') as archive:
        seen = set()
        for member in archive:
            name = PurePosixPath(member.name)
            if (name.is_absolute() or not name.parts or name.parts[0] != 'pinvou3-app'
                    or any(part in ('.', '..') or ':' in part for part in name.parts)
                    or '\\' in member.name or member.name in seen
                    or not (member.isdir() or member.isfile())):
                raise ValueError('unsupported ACP source archive entry')
            seen.add(member.name)
            target = destination.joinpath(*name.parts).resolve()
            if not target.is_relative_to(destination):
                raise ValueError('ACP source escaped its private directory')
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with archive.extractfile(member) as source, target.open('xb') as output:
                    shutil.copyfileobj(source, output)


def run_smoke(*, source_ref='HEAD', environment=None, platform=None):
    environment = dict(os.environ if environment is None else environment)
    if (sys.platform if platform is None else platform) != 'win32':
        raise RuntimeError('ACP runtime smoke requires native Windows')
    scope = supervised_root(environment)
    if source_ref != 'HEAD' and not re.fullmatch('[0-9a-f]{40}', source_ref):
        raise ValueError('ACP source must be HEAD or a fixed Git object')
    # Resolve to an immutable tree before creating the archive. --source-ref is
    # useful for pre-commit verification of a fixed staged tree; CI uses HEAD.
    tree = subprocess.check_output(['git', 'rev-parse', '--verify', source_ref+'^{tree}'],
                                   cwd=ROOT, encoding='ascii').strip()
    if not re.fullmatch('[0-9a-f]{40}', tree):
        raise ValueError('invalid ACP source tree')
    raw = subprocess.check_output(['git', 'archive', '--format=tar', tree, *SOURCE_PATHS], cwd=ROOT)
    destination = Path(tempfile.mkdtemp(prefix='acp-', dir=scope))
    materialize(raw, destination)
    npm = shutil.which('npm.cmd', path=environment.get('PATH'))
    if not npm:
        raise RuntimeError('Windows ACP smoke requires the configured npm executable')
    print(f'Windows ACP smoke: immutable tree {tree} in an owned private app; checkout caches untouched', flush=True)
    # No fixture directory deletion here: the supervisor observes all owned
    # descendants exiting before removing its whole private directory.
    result = subprocess.run([npm, '--prefix', str(destination/'pinvou3-app'), 'run',
                             'test:codex-acp-windows-runtime'], cwd=destination,
                            env=environment, check=False, timeout=600)
    return result.returncode


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-ref', default='HEAD')
    args = parser.parse_args()
    return run_smoke(source_ref=args.source_ref)


if __name__ == '__main__':
    raise SystemExit(main())

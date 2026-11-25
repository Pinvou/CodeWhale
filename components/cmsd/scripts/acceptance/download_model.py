#!/usr/bin/env python3
"""Opt-in acceptance download; verifies manifest sizes and declared LFS SHA-256.

Place source.json next to this script with repo, an operator-selected fixed
revision, and files: [{"rfilename": "...", "size": 123, "lfs": {"sha256": "..."}}].
The lfs field is optional; without it only the declared file size is verified.
LMM_MODELS_DIR selects the parent of qa-smollm2. Existing files and failed .part
files are retained; an existing mismatch is rejected without downloading over it.
"""
import hashlib
import json
import os
import subprocess
import time
from pathlib import Path

def verify_file(path, item):
    if path.stat().st_size != item['size']:
        raise RuntimeError('download size mismatch: '+item['rfilename'])
    if item.get('lfs'):
        hasher = hashlib.sha256()
        with path.open('rb') as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                hasher.update(chunk)
        if hasher.hexdigest() != item['lfs']['sha256']:
            raise RuntimeError('download digest mismatch: '+item['rfilename'])


card = Path(__file__).resolve().parent
manifest = json.loads((card/'source.json').read_text())
target = Path(os.environ['LMM_MODELS_DIR'])/'qa-smollm2'
target.mkdir(parents=True, exist_ok=True)
total = sum(item['size'] for item in manifest['files'])
done = 0
for item in manifest['files']:
    path = target/item['rfilename']
    if path.exists():
        verify_file(path, item)
    else:
        partial = path.with_suffix(path.suffix+'.part')
        url = 'https://hf-mirror.com/'+manifest['repo']+'/resolve/'+manifest['revision']+'/'+item['rfilename']
        worker = subprocess.Popen(['curl', '--fail', '--location', '--retry', '2', '--connect-timeout', '15',
                                   '--max-time', '600', '--silent', '--show-error', '--continue-at', '-',
                                   '--limit-rate', '4M', '--output', str(partial), url])
        while worker.poll() is None:
            size = partial.stat().st_size if partial.exists() else 0
            print(f'LMM_PROGRESS_BYTES={done+size}/{total}', flush=True)
            time.sleep(.5)
        if worker.returncode:
            raise SystemExit(worker.returncode)
        verify_file(partial, item)
        partial.rename(path)
    done += path.stat().st_size
    print(f'LMM_PROGRESS_BYTES={done}/{total}', flush=True)
print('Manifest sizes and declared LFS SHA-256 verified; weights retained for repeat acceptance.', flush=True)

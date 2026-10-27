#!/usr/bin/env python3
"""Package an operator-built cmsd binary. Does not sign, download or install it."""
import argparse
import hashlib
import json
import shutil
import subprocess
from pathlib import Path


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary',type=Path,required=True)
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    binary=args.binary.resolve(strict=True)
    info=json.loads(subprocess.check_output([str(binary),'--component-info'],timeout=5))
    if (info.get('component')!='cmsd' or info.get('update_contract')!=1
            or info.get('state_format')!='cmsd-json-v1' or info.get('api')!='v1'):
        raise ValueError('binary does not implement the component contract')
    args.output.mkdir(parents=True,exist_ok=False)
    target=args.output/'cmsd'
    shutil.copyfile(binary,target);target.chmod(0o700)
    with target.open('rb') as stream:
        hasher = hashlib.sha256()
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            hasher.update(chunk)
        digest = hasher.hexdigest()
    manifest={'format':'megacube-cmsd-component-v1','component':'cmsd','version':info['version'],
        'architecture':info['architecture'],'os':info['os'],'api':info['api'],
        'state_format':info['state_format'],'sha256':digest,
        'size':target.stat().st_size}
    (args.output/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
    print(json.dumps({'package':str(args.output),'version':info['version'],'sha256':manifest['sha256'],
        'build_revision':info.get('build_revision'),'publisher_verified':False}))


if __name__=='__main__':main()

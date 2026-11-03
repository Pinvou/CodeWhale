"""Initialize and verify job-owned test-resource receipts, without cleanup by guessing."""
import argparse
import json
import importlib.util
import os
import shutil
from pathlib import Path
import tempfile

KINDS = {'database', 'process', 'vite', 'browser', 'http', 'tcp', 'postgres-services', 'deployment-containers', 'docker'}


def inspect(directories):
    count, failures = 0, []
    for root in directories:
        if root.is_symlink() or not root.is_dir():
            failures.append({'kind': 'directory', 'reason': 'receipt directory unavailable'})
            continue
        for directory, children, files in os.walk(root, followlinks=False):
            for name in children:
                if (Path(directory) / name).is_symlink():
                    failures.append({'kind': 'directory', 'reason': 'symbolic receipt directory'})
            for name in files:
                if not name.endswith('.json'):
                    continue
                path = Path(directory) / name
                count += 1
                try:
                    if path.is_symlink() or path.stat().st_size > 65536:
                        raise ValueError('invalid receipt boundary')
                    value = json.loads(path.read_text(encoding='utf-8'))
                    if value.get('schema') != 1 or value.get('kind') not in KINDS:
                        raise ValueError('unknown receipt schema')
                    if value.get('state') != 'released' or value.get('cleanup_failed', False):
                        failures.append({'kind': value['kind'], 'id': value.get('id'), 'state': value.get('state'), 'reason': 'retirement not clean'})
                except (OSError, ValueError, AttributeError):
                    failures.append({'kind': 'receipt', 'reason': 'unreadable or malformed receipt'})
    return {'schema': 1, 'receipts': count, 'failures': failures, 'clean': not failures}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--init', action='store_true')
    parser.add_argument('--recover', action='store_true')
    parser.add_argument('--docker', action='store_true', help='register Docker creations on this job PATH only')
    parser.add_argument('--directory', type=Path, action='append', default=[])
    args = parser.parse_args()
    if args.init:
        parent = Path(os.environ['RUNNER_TEMP']).resolve(strict=True)
        root = Path(tempfile.mkdtemp(prefix='pinvou-resource-ledger-', dir=parent))
        # Keep a diagnostic artifact even if dependency/lint setup fails before
        # the first fixture starts. Zero JSON receipts do not prove tests ran.
        (root / 'ledger.txt').write_text('Independent job resource ledger initialized. JSON receipts appear when resources are registered; an empty set is not proof that tests executed.\n', encoding='utf-8')
        with Path(os.environ['GITHUB_ENV']).open('a', encoding='utf-8') as output:
            output.write('PINVOU_CI_RESOURCE_LEDGER=' + str(root) + '\n')
            if args.docker:
                if os.name != 'posix':
                    raise RuntimeError('job-local Docker leases require a POSIX runner')
                real = shutil.which('docker')
                if not real:
                    raise RuntimeError('Docker executable unavailable')
                output.write('PINVOU_CI_REAL_DOCKER=' + str(Path(real).absolute()) + '\n')
                binary = root / 'bin'
                binary.mkdir()
                launcher = binary / 'docker'
                launcher.write_text('#!/usr/bin/env python3\nimport runpy\nrunpy.run_path('
                    + repr(str(Path(__file__).with_name('ci-fixture-docker.py').resolve()))
                    + ', run_name="__main__")\n', encoding='utf-8')
                launcher.chmod(0o755)
                with Path(os.environ['GITHUB_PATH']).open('a', encoding='utf-8') as search:
                    search.write(str(binary) + '\n')
        print('Initialized an independent job test-resource ledger')
        return
    directories = args.directory or [Path(os.environ['PINVOU_CI_RESOURCE_LEDGER'])]
    if args.recover:
        if os.environ.get('GITHUB_ACTIONS') != 'true' or directories[0] != Path(os.environ['PINVOU_CI_RESOURCE_LEDGER']):
            raise RuntimeError('recovery is restricted to this job ledger')
        spec = importlib.util.spec_from_file_location('fixture_recover', Path(__file__).with_name('ci-fixture-recover.py'))
        recovery = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(recovery)
        for path in directories[0].glob('*.json'):
            if path.is_symlink() or path.stat().st_size > 65536:
                raise RuntimeError('invalid process receipt boundary')
            value = json.loads(path.read_text(encoding='utf-8'))
            if value.get('kind') == 'process' and value.get('state') != 'released':
                try:
                    recovered = recovery.recover(value)
                    path.with_suffix('.before-recovery.txt').write_text(json.dumps(value), encoding='utf-8')
                    temporary = path.with_suffix('.recovered.tmp')
                    temporary.write_text(json.dumps(recovered), encoding='utf-8')
                    os.replace(temporary, path)
                except (OSError, ValueError, RuntimeError):
                    print('::error::An unconfirmed test scope could not be reclaimed; original receipt retained')
        docker_receipts = list(directories[0].glob('docker-*.json'))
        if docker_receipts:
            spec = importlib.util.spec_from_file_location('fixture_docker', Path(__file__).with_name('ci-fixture-docker.py'))
            docker = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(docker)
            try:
                docker.recover(directories[0])
            except (OSError, ValueError, RuntimeError):
                print('::error::Owned container retirement was not confirmed; creation receipts retained')
    report = inspect(directories)
    print(json.dumps(report, sort_keys=True))
    if not report['clean']:
        raise SystemExit(1)


if __name__ == '__main__':
    main()

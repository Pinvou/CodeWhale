#!/usr/bin/env python3
"""Opt-in real component configuration migration, rollback and worker-crash recovery.

Uses only new private profiles, ephemeral sockets/ports and unguessable test-owned
user units. The upgrade package is a private build of a source snapshot whose
only code metadata change is its package version. A narrow PATH wrapper
kills only the verified test-owned component worker at the config commit boundary.
No existing unit or model is controlled. This is not GPU or model inference QA.
"""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import tempfile
import time
import uuid


def digest(path):
    result = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(4*1024*1024), b''):
            result.update(block)
    return result.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pinvou', type=Path, required=True)
    parser.add_argument('--package', type=Path, required=True)
    parser.add_argument('--upgrade-package', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--source-head')
    parser.add_argument('--live', action='store_true')
    args = parser.parse_args()
    if not args.live:
        parser.error('--live is required to control unique test-owned user units and interrupt a test worker')
    evidence = args.evidence.resolve()
    evidence.mkdir(mode=0o700, parents=True, exist_ok=False)
    assert args.pinvou.is_file() and args.package.is_dir()
    real_systemctl = shutil.which('systemctl')
    assert real_systemctl
    work = Path(tempfile.mkdtemp(prefix='pin-r04-config-'))
    frozen = work/'pinvou'
    subprocess.run(['cp', '--reflink=auto', str(args.pinvou.resolve()), str(frozen)], check=True)
    frozen.chmod(0o700)
    old_package, new_package = work/'old-package', work/'new-package'
    shutil.copytree(args.package.resolve(), old_package)
    shutil.copytree(args.upgrade_package.resolve(), new_package)
    manifest = json.loads((old_package/'manifest.json').read_text())
    updated = json.loads((new_package/'manifest.json').read_text())
    old_version, new_version = manifest['version'], updated['version']
    assert tuple(map(int, new_version.split('.'))) > tuple(map(int, old_version.split('.')))
    new_info = json.loads(subprocess.check_output([str(new_package/'cmsd'), '--component-info'], text=True, timeout=10))
    old_info = json.loads(subprocess.check_output([str(old_package/'cmsd'), '--component-info'], text=True, timeout=10))
    expected_info = {**old_info, 'version': new_version, 'build_revision': new_info['build_revision']}
    assert new_info == expected_info, ('upgrade fixture changed unexpected capabilities', old_info, new_info)
    assert digest(new_package/'cmsd') == updated['sha256']
    assert digest(old_package/'cmsd') == manifest['sha256']
    started, events, owned = time.monotonic(), [], []
    component_by_unit = {}
    removed_templates = []

    def systemd(*values, check=True):
        return subprocess.run([real_systemctl, '--user', *values], capture_output=True,
                              text=True, timeout=40, check=check)

    def properties(unit):
        result = systemd('show', unit, '--property=LoadState,ActiveState,MainPID,FragmentPath')
        return dict(line.split('=', 1) for line in result.stdout.splitlines() if '=' in line)

    def record(case, value):
        events.append(case)
        with (evidence/'events.jsonl').open('a') as output:
            output.write(json.dumps({'case': case, 'value': value, 'at': time.time()})+'\n')
        print(case, flush=True)

    def quiesce_worker(unit):
        component = component_by_unit[unit]
        operation_path = component / 'operation.json'
        if not operation_path.exists():
            return
        operation = json.loads(operation_path.read_text())
        operation_id = operation.get('id', '')
        assert operation_id.startswith('component-') and all(
            letter.isascii() and (letter.isalnum() or letter == '-') for letter in operation_id
        ), 'refuse to infer a worker without its exact private operation ID'
        assert hasattr(os, 'pidfd_open') and hasattr(signal, 'pidfd_send_signal'), (
            'safe timeout cleanup requires Linux pidfd support')
        # A failed CLI or deadline can leave its detached worker alive. Match
        # only our frozen executable and exact private job; pidfd pins identity
        # across the signal, including PID reuse between observation and open.
        for entry in Path('/proc').iterdir():
            if not entry.name.isdigit():
                continue
            descriptor = None
            try:
                if entry.stat().st_uid != os.geteuid():
                    continue
                descriptor = os.pidfd_open(int(entry.name), 0)
                if (entry / 'exe').resolve(strict=True) != frozen:
                    continue
                argv = [part for part in (entry / 'cmdline').read_bytes().split(b'\0') if part]
                if argv[1:] != [b'--cmsd-component-worker', operation_id.encode()]:
                    continue
                signal.pidfd_send_signal(descriptor, signal.SIGKILL)
                deadline = time.monotonic() + 3
                while time.monotonic() < deadline:
                    try:
                        state = (entry / 'stat').read_text().rsplit(') ', 1)[1].split()[0]
                    except FileNotFoundError:
                        break
                    if state in ('Z', 'X'):
                        break
                    time.sleep(.02)
                else:
                    raise TimeoutError('test-owned worker did not exit')
            except (FileNotFoundError, ProcessLookupError, PermissionError):
                continue
            finally:
                if descriptor is not None:
                    os.close(descriptor)

    def cleanup(unit, unit_file):
        quiesce_worker(unit)
        facts = properties(unit)
        if facts.get('MainPID') != '0':
            executable = Path('/proc', facts['MainPID'], 'exe').resolve(strict=True)
            assert executable.is_relative_to(work), 'refuse to stop a process outside this test'
            assert facts.get('FragmentPath') == str(unit_file)
        if unit_file.exists():
            assert not unit_file.is_symlink()
            text = unit_file.read_text()
            assert text.startswith('[Unit]\nDescription=Pinvou managed Cube Manage Software device service\n')
            assert str(work) in text, 'refuse to remove another registration'
        systemd('stop', unit, check=False)
        systemd('disable', unit, check=False)
        systemd('--runtime', 'disable', unit, check=False)
        systemd('reset-failed', unit, check=False)
        if unit_file.exists():
            unit_file.unlink()
        # cmsd installs a template isolated by this private state directory.
        # No model is installed, so no instance may remain before removing it.
        component = component_by_unit[unit]
        for template in unit_file.parent.glob('megacube-model-*@.service'):
            if not re.fullmatch(r'megacube-model-[0-9a-f]{16}@\.service', template.name):
                continue
            if template.is_symlink():
                continue
            text = template.read_text()
            if str(component/'state')+'/' not in text:
                continue
            assert template.stat().st_uid == os.geteuid()
            assert text.startswith('[Unit]\nDescription=MegaCube model engine (card %i)\n')
            instances = systemd('list-units', template.name.replace('@.service', '@*.service'),
                               '--all', '--no-legend', '--plain').stdout
            assert not instances.strip(), 'private fixture unexpectedly left a native model instance'
            template.unlink()
            removed_templates.append(template.name)
        systemd('daemon-reload')
        assert properties(unit).get('MainPID') == '0' and not unit_file.exists()

    try:
        record('version-only-private-source-build-fixture', {'old': old_version, 'new': new_version,
               'build_revision': new_info['build_revision'], 'old_sha256': digest(old_package/'cmsd'),
               'new_sha256': digest(new_package/'cmsd')})
        for interrupted in (False, True):
            case = 'crash' if interrupted else 'normal'
            root = work/case
            profile, sock = root/'profile', root/'cmsd.sock'
            profile.mkdir(parents=True, mode=0o700)
            unit = 'pinvou-cmsd-r04mig-'+uuid.uuid4().hex[:12]+'.service'
            unit_file = Path.home()/'.config/systemd/user'/unit
            assert not unit_file.exists() and not unit_file.with_name(unit+'.d').exists()
            facts = properties(unit)
            assert facts.get('LoadState') == 'not-found' and facts.get('MainPID') == '0', facts
            owned.append((unit, unit_file))
            with socket.socket() as probe:
                probe.bind(('127.0.0.1', 0))
                port = probe.getsockname()[1]
            env = {**os.environ, 'PINVOU3_HOME': str(profile), 'CMSD_SOCKET': str(sock),
                   'PINVOU3_CMSD_UNIT': unit, 'PINVOU3_CMSD_GATEWAY': f'127.0.0.1:{port}',
                   'XDG_CONFIG_HOME': str(Path.home()/'.config')}
            component = profile/'megacube/cmsd-component'
            component_by_unit[unit] = component
            service = component/'service.json'
            marker, armed = root/'worker-interrupted', root/'interrupt-armed'
            if interrupted:
                wrapper_dir = root/'bin'
                wrapper_dir.mkdir(mode=0o700)
                wrapper = wrapper_dir/'systemctl'
                wrapper.write_text('#!/usr/bin/python3\nimport os,pathlib,signal,sys\n'
                    +f'real={real_systemctl!r}\narmed=pathlib.Path({str(armed)!r})\nmarker=pathlib.Path({str(marker)!r})\ncomponent=pathlib.Path({str(component)!r})\nprogram={str(frozen)!r}\nprofile={str(profile)!r}\n'
                    +"if armed.exists() and not marker.exists() and (component/'config-migration.json').exists() and 'require_store_selection = \\\"v1\\\"' in (component/'cmsd.toml').read_text():\n"
                    +"    parent=os.getppid()\n    argv=pathlib.Path('/proc',str(parent),'cmdline').read_bytes().split(b'\\0')\n    actual=pathlib.Path('/proc',str(parent),'exe').resolve(strict=True)\n    assert str(actual)==program and b'--cmsd-component-worker' in argv and os.environ.get('PINVOU3_HOME')==profile\n    marker.write_text(str(parent))\n    armed.unlink()\n    os.kill(parent,signal.SIGKILL)\nos.execv(real,[real,*sys.argv[1:]])\n")
                wrapper.chmod(0o700)
                env['PATH'] = str(wrapper_dir)+os.pathsep+os.environ['PATH']

            def cli(*values):
                result = subprocess.run([str(frozen), '--cmsd-component-cli', *values], env=env,
                                        capture_output=True, text=True, timeout=45)
                with (evidence/(case+'-cli.jsonl')).open('a') as output:
                    output.write(json.dumps({'args': values, 'code': result.returncode,
                                             'stdout': result.stdout, 'stderr': result.stderr})+'\n')
                assert result.returncode == 0, result.stderr
                return json.loads(result.stdout)

            def action(name, package=None, expected='succeeded'):
                values = ['action', name]
                if package:
                    values += [str(package), json.loads((package/'manifest.json').read_text())['sha256']]
                accepted = cli(*values)
                deadline = time.monotonic()+75
                while time.monotonic() < deadline:
                    status = cli('status')
                    operation = status.get('operation') or {}
                    if operation.get('id') == accepted['operationId'] and operation['status'] not in ('queued', 'running'):
                        assert operation['status'] == expected, operation
                        return status
                    time.sleep(.15)
                raise TimeoutError((case, name, accepted))

            def save(path, value):
                temporary = path.with_name(path.name+'.fixture-new')
                temporary.write_text(json.dumps(value))
                temporary.chmod(0o600)
                temporary.replace(path)

            def daemon_status():
                with socket.socket(socket.AF_UNIX) as connection:
                    connection.settimeout(8)
                    connection.connect(str(sock))
                    connection.sendall(b'GET /v1/status HTTP/1.1\r\nHost: cmsd\r\nConnection: close\r\nContent-Length: 0\r\n\r\n')
                    data = b''
                    while block := connection.recv(65536):
                        data += block
                head, body = data.split(b'\r\n\r\n', 1)
                assert head.startswith(b'HTTP/1.1 200 '), head
                return json.loads(body)

            action('install', old_package)
            assert daemon_status()['version'] == old_version
            original = json.loads(service.read_text())
            identity = component/'state/device-identity.json'
            identity_hash = digest(identity)
            action('stop')
            old = json.loads(service.read_text())
            config = Path(old['config'])
            before_text = config.read_text()
            legacy_text = before_text.replace('require_store_selection = "v1"\n', '')
            assert before_text != legacy_text
            config.write_text(legacy_text)
            config.chmod(0o600)
            # Explicitly provision a consistent private historical fixture, not
            # an untrusted config edit. The public API must still verify it.
            old['registration']['config_sha256'] = digest(config)
            save(service, old)
            assert cli('status')['scope'] == 'user'
            if interrupted:
                armed.write_text('interrupt only our worker after config apply')
                stopped = action('update', new_package, expected='interrupted')
                assert marker.is_file() and not (component/'config-migration.json').exists()
                repaired = json.loads(service.read_text())
                backup = json.loads((component/'backup.json').read_text())
                assert repaired['binary'] == old['binary'] and stopped['scope'] == 'user'
                assert repaired['registration']['config_sha256'] == digest(config)
                assert backup['registration']['config_sha256'] == digest(config)
                assert repaired['registration']['unit_sha256'] == old['registration']['unit_sha256']
                assert repaired['registration']['binary_sha256'] == old['registration']['binary_sha256']
                record(case+'-config-commit-worker-exit-recovered', {'operation': stopped['operation']['status'],
                       'reader': str(component/'state/store-reader.json'), 'unit_digest_preserved': True,
                       'binary_digest_preserved': True})
                action('start')
            updated_status = action('update', new_package)
            installed = json.loads(service.read_text())
            backup = json.loads((component/'backup.json').read_text())
            assert installed['version'] == new_version and installed['binary'] != old['binary']
            assert installed['previous']['binary'] == old['binary'] and backup['binary'] == old['binary']
            assert 'require_store_selection = "v1"' in config.read_text()
            for receipt in (installed, installed['previous'], backup):
                assert receipt['registration']['config_sha256'] == digest(config)
            assert not (component/'config-migration.json').exists()
            if updated_status['activeState'] != 'active':
                action('start')
            assert daemon_status()['version'] == new_version
            assert Path('/proc', properties(unit)['MainPID'], 'exe').resolve() == Path(installed['binary'])
            record(case+'-update-starts-new-real-daemon', {'version': new_version, 'binary_sha256': digest(Path(installed['binary'])),
                   'backup_config_digest_updated': True})
            rollback = action('rollback')
            restored = json.loads(service.read_text())
            assert rollback['installed'] and rollback['activeState'] == 'active'
            assert restored['binary'] == original['binary'] and restored['version'] == old_version
            assert restored['registration']['config_sha256'] == digest(config)
            assert Path('/proc', properties(unit)['MainPID'], 'exe').resolve() == Path(original['binary'])
            assert daemon_status()['version'] == old_version
            assert digest(identity) == identity_hash
            assert not (component/'state/component-maintenance.json').exists()
            assert not (component/'config-migration.json').exists()
            record(case+'-manual-rollback-starts-original-real-daemon', {'version': old_version,
                   'new_selection_policy_retained': True, 'device_identity_preserved': True})
            cleanup(unit, unit_file)
            owned.remove((unit, unit_file))
            record(case+'-unique-unit-cleaned', {'unit': unit, 'MainPID': properties(unit)['MainPID']})
        report = {'passed': True, 'seconds': time.monotonic()-started, 'cases': events,
                  'pinvou_sha256': digest(frozen), 'source_head': args.source_head,
                  'package_sha256': digest(old_package/'cmsd'), 'upgrade_fixture_sha256': digest(new_package/'cmsd'),
                  'private_namespace_templates_removed': removed_templates, 'no_test_native_unit_instances': True,
                  'scope': 'Real Pinvou component worker and real cmsd under unique user units. Private package-version source build fixture, config migration, worker interruption, successful upgrade and manual rollback. No model inference/GPU or existing service control.'}
        (evidence/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    except Exception as error:
        (evidence/'failure.json').write_text(json.dumps({'error': repr(error), 'cases_completed': events}, indent=2)+'\n')
        raise
    finally:
        errors = []
        for unit, unit_file in owned:
            try:
                cleanup(unit, unit_file)
            except Exception as error:
                errors.append({'unit': unit, 'error': repr(error)})
        if errors:
            (evidence/'cleanup-errors.json').write_text(json.dumps(errors, indent=2)+'\n')
            raise RuntimeError('Could not safely clean the private fixture; inspect cleanup-errors.json')
        shutil.rmtree(work)


if __name__ == '__main__':
    main()

#!/usr/bin/env python3
"""Opt-in real component update/uninstall with bounded registration reload faults.

Creates unique test-owned user units, private profiles and ephemeral gateways.
Only this test process receives a PATH wrapper. No model is installed or run.
"""
import argparse
import hashlib
import json
import os
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
        for block in iter(lambda: stream.read(4 * 1024 * 1024), b''):
            result.update(block)
    return result.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pinvou', type=Path, required=True)
    parser.add_argument('--old-package', type=Path, required=True)
    parser.add_argument('--new-package', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--live', action='store_true')
    args = parser.parse_args()
    if not args.live:
        parser.error('--live is required for creation/control of unique test-owned user units')
    evidence = args.evidence.resolve()
    evidence.mkdir(mode=0o700, parents=True, exist_ok=False)
    executable = args.pinvou.resolve()
    old_package, new_package = args.old_package.resolve(), args.new_package.resolve()
    assert executable.is_file() and old_package.is_dir() and new_package.is_dir()
    old_manifest = json.loads((old_package / 'manifest.json').read_text())
    new_manifest = json.loads((new_package / 'manifest.json').read_text())
    assert old_manifest['sha256'] != new_manifest['sha256']
    assert old_manifest['version'] != new_manifest['version']
    assert digest(old_package / 'cmsd') == old_manifest['sha256']
    assert digest(new_package / 'cmsd') == new_manifest['sha256']
    real_systemctl = shutil.which('systemctl')
    assert real_systemctl
    work = Path(tempfile.mkdtemp(prefix='pin-r04-register-'))
    frozen = work / 'pinvou'
    subprocess.run(['cp', '--reflink=auto', str(executable), str(frozen)], check=True)
    frozen.chmod(0o700)
    all_units, cases = [], []
    component_by_unit, template_hash_by_unit = {}, {}
    removed_templates = []
    started = time.monotonic()

    def systemd(*values, check=True):
        return subprocess.run([real_systemctl, '--user', *values], capture_output=True,
                              text=True, timeout=40, check=check)

    def properties(unit):
        output = systemd('show', unit, '--property=LoadState,ActiveState,MainPID,FragmentPath')
        return dict(line.split('=', 1) for line in output.stdout.splitlines() if '=' in line)

    def event(name, value):
        item = {'case': name, 'value': value, 'at': time.time()}
        cases.append(item)
        with (evidence / 'events.jsonl').open('a') as output:
            output.write(json.dumps(item) + '\n')
        print(name, flush=True)

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

    def template_path(unit, unit_file):
        state = component_by_unit[unit] / 'state'
        suffix = hashlib.sha256(os.fsencode(state / 'env')).hexdigest()[:16]
        return unit_file.parent / ('megacube-model-' + suffix + '@.service')

    def remove_private_template(unit, unit_file):
        state = component_by_unit[unit] / 'state'
        template = template_path(unit, unit_file)
        prefix = template.name.removesuffix('@.service')
        marker = state / 'native-unit-namespace-v1'
        backup = template.with_suffix('.service.cmsd-before-profile-v1')
        if template.exists() or template.is_symlink() or backup.exists() or backup.is_symlink():
            assert marker.is_file() and not marker.is_symlink(), 'template lacks its exact private namespace receipt'
        if marker.exists() or marker.is_symlink():
            assert not marker.is_symlink() and marker.is_file() and marker.stat().st_uid == os.geteuid()
            assert marker.read_text() == 'cmsd-native-unit-namespace-v1\n' + prefix + '\n'
        # No model is installed in this fixture. Loaded instances and persisted
        # instance registrations must both be absent before deleting a template.
        instances = systemd('list-units', prefix + '@*.service', '--all',
                           '--no-legend', '--plain').stdout
        assert not instances.strip(), 'test namespace unexpectedly has a native model instance'
        unexpected = [path for path in unit_file.parent.glob(prefix + '@*.service*')
                      if path not in (template, backup)]
        assert not unexpected, 'test namespace unexpectedly has an instance registration'
        for owned in (template, backup):
            if not owned.exists() and not owned.is_symlink():
                continue
            metadata = owned.lstat()
            assert not owned.is_symlink() and owned.is_file() and metadata.st_uid == os.geteuid()
            assert metadata.st_size <= 65536 and metadata.st_mode & 0o022 == 0
            text = owned.read_text()
            assert text.startswith('[Unit]\nDescription=MegaCube model engine (card %i)\n')
            assert ('EnvironmentFile=' + str(state / 'env' / '%i.env') + '\n') in text
            if owned == template and unit in template_hash_by_unit:
                assert digest(owned) == template_hash_by_unit[unit], 'private template changed after installation'
            owned.unlink()
            removed_templates.append(owned.name)

    def cleanup(unit, unit_file):
        quiesce_worker(unit)
        facts = properties(unit)
        if facts.get('MainPID') != '0':
            process = Path('/proc', facts['MainPID'], 'exe').resolve(strict=True)
            assert process.is_relative_to(work), 'refuse to stop a foreign process'
            assert facts.get('FragmentPath') == str(unit_file), 'refuse to control a foreign unit'
        if unit_file.exists():
            assert not unit_file.is_symlink()
            text = unit_file.read_text()
            assert text.startswith('[Unit]\nDescription=Pinvou managed Cube Manage Software device service\n')
            assert str(work) in text, 'refuse to remove a foreign registration'
        systemd('stop', unit, check=False)
        systemd('disable', unit, check=False)
        systemd('--runtime', 'disable', unit, check=False)
        systemd('reset-failed', unit, check=False)
        if unit_file.exists():
            unit_file.unlink()
        remove_private_template(unit, unit_file)
        systemd('daemon-reload')
        assert properties(unit)['MainPID'] == '0' and not unit_file.exists()

    try:
        for mode in ('once', 'persistent'):
            root = work / mode
            profile, sock = root / 'profile', root / 'cmsd.sock'
            (profile / 'model-store').mkdir(mode=0o700, parents=True)
            unit = 'pinvou-cmsd-r04register-' + uuid.uuid4().hex[:12] + '.service'
            unit_file = Path.home() / '.config/systemd/user' / unit
            assert not unit_file.exists() and not unit_file.with_name(unit + '.d').exists()
            initial = properties(unit)
            assert initial['LoadState'] == 'not-found' and initial['MainPID'] == '0', initial
            component_by_unit[unit] = profile / 'megacube/cmsd-component'
            template = template_path(unit, unit_file)
            assert not template.exists() and not template.is_symlink()
            backup_template = template.with_suffix('.service.cmsd-before-profile-v1')
            assert not backup_template.exists() and not backup_template.is_symlink()
            all_units.append((unit, unit_file))
            with socket.socket() as probe:
                probe.bind(('127.0.0.1', 0))
                gateway = probe.getsockname()[1]
            component = profile / 'megacube/cmsd-component'
            marker = root / 'inject.json'
            wrapper_log = root / 'wrapper-events.jsonl'
            wrapper_dir = root / 'bin'
            wrapper_dir.mkdir(mode=0o700)
            wrapper = wrapper_dir / 'systemctl'
            wrapper.write_text('#!/usr/bin/python3\nimport json,os,pathlib,sys,time\n'
                + f'real={real_systemctl!r}\nmarker=pathlib.Path({str(marker)!r})\nunit=pathlib.Path({str(unit_file)!r})\nlog=pathlib.Path({str(wrapper_log)!r})\n'
                + "if sys.argv[1:]==['--user','daemon-reload'] and marker.exists():\n"
                + "    fault=json.loads(marker.read_text())\n"
                + "    if fault['seen'] or (fault.get('remove') and not unit.exists()) or (unit.is_file() and fault['candidate'] in unit.read_text()):\n"
                + "        fault['seen']=True\n        marker.write_text(json.dumps(fault))\n"
                + "        with log.open('a') as out: out.write(json.dumps({'at':time.time(),'fault':'registration-reload','args':sys.argv[1:],'unit_sha256':__import__('hashlib').sha256(unit.read_bytes()).hexdigest() if unit.exists() else None})+'\\n')\n"
                + "        if fault['mode']=='once': marker.unlink()\n"
                + "        print('R04 injected registration daemon-reload failure',file=sys.stderr)\n        sys.exit(1)\n"
                + "os.execv(real,[real,*sys.argv[1:]])\n")
            wrapper.chmod(0o700)
            env = {**os.environ, 'PINVOU3_HOME': str(profile), 'CMSD_SOCKET': str(sock),
                   'PINVOU3_CMSD_UNIT': unit, 'PINVOU3_CMSD_GATEWAY': f'127.0.0.1:{gateway}',
                   'XDG_CONFIG_HOME': str(Path.home() / '.config'),
                   'PATH': str(wrapper_dir) + os.pathsep + os.environ['PATH']}

            def cli(*values):
                output = subprocess.run([str(frozen), '--cmsd-component-cli', *values], env=env,
                                        capture_output=True, text=True, timeout=45)
                with (evidence / (mode + '-cli.jsonl')).open('a') as log:
                    log.write(json.dumps({'args': values, 'code': output.returncode,
                                          'stdout': output.stdout, 'stderr': output.stderr}) + '\n')
                assert output.returncode == 0, output.stderr
                return json.loads(output.stdout)

            def action(name, artifact=None, expected='succeeded'):
                values = ['action', name]
                if artifact:
                    values += [str(artifact), json.loads((artifact / 'manifest.json').read_text())['sha256']]
                accepted = cli(*values)
                deadline = time.monotonic() + 90
                while time.monotonic() < deadline:
                    status = cli('status')
                    operation = status.get('operation') or {}
                    if operation.get('id') == accepted['operationId'] and operation['status'] not in ('queued', 'running'):
                        assert operation['status'] == expected, operation
                        return status
                    time.sleep(.15)
                raise TimeoutError((mode, name, accepted))

            def daemon_status():
                with socket.socket(socket.AF_UNIX) as connection:
                    connection.settimeout(5)
                    connection.connect(str(sock))
                    connection.sendall(b'GET /v1/status HTTP/1.1\r\nHost: cmsd\r\nConnection: close\r\nContent-Length: 0\r\n\r\n')
                    chunks = []
                    while block := connection.recv(65536):
                        chunks.append(block)
                header, body = b''.join(chunks).split(b'\r\n\r\n', 1)
                assert header.startswith(b'HTTP/1.1 200 '), header
                return json.loads(body)

            installed = action('install', old_package)
            assert installed['installed'] and installed['activeState'] == 'active'
            template_hash_by_unit[unit] = digest(template)
            original = json.loads((component / 'service.json').read_text())
            original_unit = unit_file.read_bytes()
            original_config = Path(original['config']).read_bytes()
            before_pid = properties(unit)['MainPID']
            assert before_pid != '0'
            assert Path('/proc', before_pid, 'exe').resolve() == Path(original['binary'])
            assert daemon_status()['version'] == old_manifest['version']
            event(mode + '-old-real-component-running', {'pid': before_pid, 'version': old_manifest['version']})
            marker.write_text(json.dumps({'mode': mode, 'seen': False,
                                          'candidate': str(component / 'releases' / new_manifest['sha256'] / 'cmsd')}))
            marker.chmod(0o600)
            failed = action('update', new_package, expected='failed')
            error = failed['operation']['error']
            assert 'R04 injected registration daemon-reload failure' in error, error
            assert wrapper_log.is_file()
            faults = [json.loads(line) for line in wrapper_log.read_text().splitlines()]
            assert faults and faults[0]['unit_sha256'] != hashlib.sha256(original_unit).hexdigest()
            if mode == 'once':
                assert failed['operation']['recovery']['status'] == 'succeeded', failed
                assert 'rollback=succeeded' in error, error
                assert not (component / 'registration-transition.json').exists()
                event(mode + '-registration-failure-auto-compensated', {'error': error, 'injections': len(faults)})
            else:
                assert (component / 'registration-transition.json').is_file()
                journal = json.loads((component / 'registration-transition.json').read_text())
                assert journal['before']['binary'] == original['binary']
                assert journal['after']['binary'] != original['binary']
                assert properties(unit)['MainPID'] == '0'
                assert unit_file.read_bytes() == original_unit
                event(mode + '-reload-failure-retains-exact-journal', {'error': error, 'injections': len(faults)})
                marker.unlink()
                recovered = cli('status')  # OS-only reader completes exact old registration.
                assert not (component / 'registration-transition.json').exists()
                assert recovered['installed'] and recovered['activeState'] == 'inactive', recovered
                assert recovered['canStart'], recovered
                restored = action('start')
                assert restored['installed'] and restored['activeState'] == 'active'
                event(mode + '-advertised-start-completes-after-manager-recovery', restored['operation'])
            final = json.loads((component / 'service.json').read_text())
            facts, daemon = properties(unit), daemon_status()
            assert facts['MainPID'] != '0'
            assert Path('/proc', facts['MainPID'], 'exe').resolve() == Path(original['binary'])
            assert final['binary'] == original['binary'] and final['version'] == old_manifest['version']
            assert final['installed'] and not final['pending_install'] and final['maintenance_owner'] is None
            assert unit_file.read_bytes() == original_unit
            assert Path(final['config']).read_bytes() == original_config
            assert final['registration']['unit_sha256'] == hashlib.sha256(original_unit).hexdigest()
            assert final['registration']['config_sha256'] == hashlib.sha256(original_config).hexdigest()
            assert final['registration']['binary_sha256'] == digest(Path(final['binary']))
            assert daemon['version'] == old_manifest['version'] and daemon['component_maintenance'] is False
            assert daemon['store_adoption']['phase'] == 'ready'
            assert not (Path(final['state']) / 'component-maintenance.json').exists()
            event(mode + '-old-real-pid-version-socket-receipt-verified', {'old_pid': before_pid,
                  'restored_pid': facts['MainPID'], 'version': daemon['version'], 'receipt_exact': True})

            # The same transaction also protects owned-unit removal. Fail only
            # after unlink, then require the exact original registration to be
            # restored before the UI-advertised recovery action can proceed.
            marker.write_text(json.dumps({'mode': mode, 'seen': False, 'remove': True,
                                          'candidate': str(component / 'releases' / new_manifest['sha256'] / 'cmsd')}))
            marker.chmod(0o600)
            failed = action('uninstall', expected='failed')
            error = failed['operation']['error']
            assert 'R04 injected registration daemon-reload failure' in error, error
            assert properties(unit)['MainPID'] == '0'
            assert unit_file.read_bytes() == original_unit
            receipt = json.loads((component / 'service.json').read_text())
            assert receipt['installed'] and receipt['binary'] == original['binary']
            assert receipt['registration'] == original['registration']
            assert Path(receipt['config']).read_bytes() == original_config
            if mode == 'once':
                assert 'registration_restore=succeeded' in error, error
                assert not (component / 'registration-transition.json').exists()
                recovered = failed
                event(mode + '-uninstall-failure-restores-owned-unit', {'error': error})
            else:
                journal = json.loads((component / 'registration-transition.json').read_text())
                assert journal['before']['installed'] and not journal['after']['installed']
                assert journal['old_text'].encode() == original_unit and journal['new_text'] is None
                assert {**journal['after'], 'installed': True} == journal['before']
                event(mode + '-uninstall-failure-retains-removal-journal', {'error': error})
                marker.unlink()
                recovered = cli('status')
                assert not (component / 'registration-transition.json').exists()
            assert recovered['installed'] and recovered['activeState'] == 'inactive', recovered
            assert recovered['canStart'], recovered
            restarted = action('start')
            assert restarted['installed'] and restarted['activeState'] == 'active'
            assert daemon_status()['component_maintenance'] is False

            removed = action('uninstall')
            assert not removed['installed'] and removed['canRestore'], removed
            assert not unit_file.exists() and properties(unit)['MainPID'] == '0'
            assert not (component / 'registration-transition.json').exists()
            restored = action('restore')
            assert restored['installed'] and restored['activeState'] == 'active', restored
            assert unit_file.read_bytes() == original_unit
            assert Path('/proc', properties(unit)['MainPID'], 'exe').resolve() == Path(original['binary'])
            assert daemon_status()['version'] == old_manifest['version']
            assert daemon_status()['component_maintenance'] is False
            event(mode + '-successful-uninstall-and-advertised-restore', restored['operation'])
            cleanup(unit, unit_file)
            all_units.remove((unit, unit_file))
            event(mode + '-unique-unit-cleaned', {'unit': unit, 'MainPID': properties(unit)['MainPID']})
        report = {'passed': True, 'seconds': time.monotonic() - started, 'cases': cases,
                  'pinvou_sha256': digest(frozen), 'old_package_sha256': old_manifest['sha256'],
                  'new_package_sha256': new_manifest['sha256'],
                  'private_namespace_templates_removed': removed_templates,
                  'no_test_native_unit_instances': True,
                  'scope': 'Real component worker, unique user units and real cmsd. Running old installation -> reload fault on new registration -> exact old runtime recovery; owned-unit removal -> reload fault -> exact registration recovery -> advertised Start; successful uninstall -> advertised Restore. No model install or inference; no existing unit controlled.'}
        (evidence / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
    except Exception as error:
        (evidence / 'failure.json').write_text(json.dumps({'error': repr(error), 'cases': cases}, indent=2) + '\n')
        raise
    finally:
        cleanup_errors = []
        for unit, unit_file in all_units:
            try:
                cleanup(unit, unit_file)
            except Exception as error:
                cleanup_errors.append({'unit': unit, 'error': repr(error)})
        if cleanup_errors:
            (evidence / 'cleanup-errors.json').write_text(json.dumps(cleanup_errors, indent=2) + '\n')
            (evidence / 'retained-work.txt').write_text(str(work) + '\n')
            raise RuntimeError('Safe cleanup failed; see cleanup-errors.json')
        shutil.rmtree(work)


if __name__ == '__main__':
    main()

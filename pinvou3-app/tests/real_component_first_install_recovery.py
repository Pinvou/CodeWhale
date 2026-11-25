#!/usr/bin/env python3
"""Actual first component install faults followed by complete Restore.

Only test-owned profiles, unique user units and ephemeral gateway ports are used.
A narrow PATH wrapper fails one daemon-reload; a directory at the private reader
receipt injects the second I/O failure. No model is installed or enabled.
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
    h = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(4*1024*1024), b''):
            h.update(block)
    return h.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pinvou', type=Path, required=True)
    parser.add_argument('--package', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--live', action='store_true', help='Explicitly allow creation/control of unique test-owned user units')
    args = parser.parse_args()
    if not args.live:
        parser.error('This opt-in test needs --live and a working systemd user manager; it never controls an existing unit.')
    evidence = args.evidence.resolve()
    evidence.mkdir(mode=0o700, parents=True, exist_ok=False)
    assert args.pinvou.is_file() and args.package.is_dir(), 'Provide a built Pinvou binary and a verified package directory'
    real_systemctl = shutil.which('systemctl')
    assert real_systemctl, 'systemctl is required'
    work = Path(tempfile.mkdtemp(prefix='pin-r02-first-'))
    frozen = work/'pinvou'
    package = work/'package'
    try:
        subprocess.run(['cp', '--reflink=auto', str(args.pinvou.resolve()), str(frozen)], check=True)
        frozen.chmod(0o700)
        shutil.copytree(args.package.resolve(), package)
    except Exception:
        shutil.rmtree(work)
        raise
    cases, all_units = [], []
    component_by_unit, template_hash_by_unit = {}, {}
    removed_templates = []
    started = time.monotonic()

    def systemd(*values, check=True):
        return subprocess.run([real_systemctl, '--user', *values], capture_output=True,
                              text=True, timeout=40, check=check)

    def properties(unit):
        run = systemd('show', unit, '--property=LoadState,ActiveState,MainPID,FragmentPath')
        return dict(line.split('=', 1) for line in run.stdout.splitlines() if '=' in line)

    def record(name, value):
        item = {'case': name, 'value': value, 'at': time.time()}
        cases.append(item)
        with (evidence/'events.jsonl').open('a') as output:
            output.write(json.dumps(item)+'\n')
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
            executable = Path('/proc', facts['MainPID'], 'exe').resolve(strict=True)
            assert executable.is_relative_to(work), 'refuse to stop a process outside this test directory'
            assert facts.get('FragmentPath') == str(unit_file), 'refuse to control another registration'
        if unit_file.exists():
            assert not unit_file.is_symlink()
            text = unit_file.read_text()
            assert text.startswith('[Unit]\nDescription=Pinvou managed Cube Manage Software device service\n')
            assert str(work) in text, 'refuse to remove another unit file'
        systemd('stop', unit, check=False)
        systemd('disable', unit, check=False)
        systemd('--runtime', 'disable', unit, check=False)
        systemd('reset-failed', unit, check=False)
        if unit_file.exists():
            unit_file.unlink()
        remove_private_template(unit, unit_file)
        systemd('daemon-reload')
        assert properties(unit).get('MainPID') == '0'
        assert not unit_file.exists()

    try:
        for fault in ('daemon-reload', 'store-reader'):
            root = work/fault
            profile, sock = root/'profile', root/'cmsd.sock'
            (profile/'model-store').mkdir(mode=0o700, parents=True)
            token = uuid.uuid4().hex[:12]
            unit = f'pinvou-cmsd-r02first-{token}.service'
            unit_file = Path.home()/'.config/systemd/user'/unit
            assert not unit_file.exists() and not unit_file.with_name(unit+'.d').exists()
            initial_unit = properties(unit)
            assert initial_unit.get('LoadState') == 'not-found' and initial_unit.get('MainPID') == '0', initial_unit
            component_by_unit[unit] = profile/'megacube/cmsd-component'
            template = template_path(unit, unit_file)
            assert not template.exists() and not template.is_symlink()
            backup_template = template.with_suffix('.service.cmsd-before-profile-v1')
            assert not backup_template.exists() and not backup_template.is_symlink()
            # Register only a positively absent, unguessable test name for cleanup.
            all_units.append((unit, unit_file))
            with socket.socket() as probe:
                probe.bind(('127.0.0.1', 0))
                gateway = probe.getsockname()[1]
            env = {**os.environ, 'PINVOU3_HOME': str(profile), 'CMSD_SOCKET': str(sock),
                   'PINVOU3_CMSD_UNIT': unit, 'PINVOU3_CMSD_GATEWAY': f'127.0.0.1:{gateway}',
                   'XDG_CONFIG_HOME': str(Path.home()/'.config')}
            component = profile/'megacube/cmsd-component'
            reader = component/'state/store-reader.json'
            wrapper_log = root/'wrapper-events.jsonl'
            if fault == 'daemon-reload':
                wrapper_dir = root/'bin'
                wrapper_dir.mkdir(mode=0o700)
                marker = root/'injected'
                wrapper = wrapper_dir/'systemctl'
                wrapper.write_text('#!/usr/bin/python3\nimport json,os,pathlib,sys\n'
                    +f'real={real_systemctl!r}\nmarker=pathlib.Path({str(marker)!r})\nunit=pathlib.Path({str(unit_file)!r})\nlog=pathlib.Path({str(wrapper_log)!r})\n'
                    +"if sys.argv[1:]==['--user','daemon-reload'] and unit.exists() and not marker.exists():\n"
                    +"    marker.write_text('injected once')\n    log.write_text(json.dumps({'fault':'daemon-reload','args':sys.argv[1:]})+'\\n')\n    print('R02 injected first daemon-reload failure',file=sys.stderr)\n    sys.exit(1)\nos.execv(real,[real,*sys.argv[1:]])\n")
                wrapper.chmod(0o700)
                env['PATH'] = str(wrapper_dir)+os.pathsep+os.environ['PATH']
            else:
                reader.mkdir(mode=0o700, parents=True)

            def cli(*values):
                run = subprocess.run([str(frozen), '--cmsd-component-cli', *values], env=env,
                                     capture_output=True, text=True, timeout=45)
                with (evidence/(fault+'-cli.jsonl')).open('a') as output:
                    output.write(json.dumps({'args': values, 'code': run.returncode,
                                             'stdout': run.stdout, 'stderr': run.stderr})+'\n')
                assert run.returncode == 0, run.stderr
                return json.loads(run.stdout)

            def action(name, artifact=None, expected='succeeded'):
                values = ['action', name]
                if artifact:
                    values += [str(artifact), json.loads((artifact/'manifest.json').read_text())['sha256']]
                accepted = cli(*values)
                deadline = time.monotonic()+70
                while time.monotonic() < deadline:
                    status = cli('status')
                    task = status.get('operation') or {}
                    if task.get('id') == accepted['operationId'] and task['status'] not in ('queued', 'running'):
                        assert task['status'] == expected, task
                        return status
                    time.sleep(.15)
                raise TimeoutError((fault, name, accepted))

            failed = action('install', package, expected='failed')
            saved = json.loads((component/'service.json').read_text())
            assert saved['binary'] and saved['version']
            assert failed['scope'] == 'user' and not failed['canInstall']
            assert properties(unit)['MainPID'] == '0'
            if fault == 'daemon-reload':
                # A failed registration is restored to its exact absent state.
                assert saved['pending_install'] and not saved['installed'] and failed['canRestore']
                assert not failed['canUpdate'] and not failed['canStart'] and not failed['installed']
                assert not unit_file.exists()
                assert 'R02 injected first daemon-reload failure' in failed['operation']['error']
                assert marker.read_text() == 'injected once'
            else:
                # Registration committed before the reader I/O failure. Keep
                # that accurate installed/stopped receipt for explicit repair.
                assert saved['installed'] and not saved['pending_install'] and failed['installed']
                assert failed['canStart'] and not failed['canRestore']
                assert unit_file.is_file() and reader.is_dir()
                reader.rmdir()
            expected_binary = Path(saved['binary'])
            expected_hash = digest(expected_binary)
            record(fault+'-failure-remains-restorable', {'error': failed['operation']['error'],
                'pending_install': saved['pending_install'], 'installed': saved['installed'], 'canRestore': failed['canRestore'], 'binary_sha256': expected_hash})
            recovery_action = 'restore' if saved['pending_install'] else 'start'
            restored = action(recovery_action)
            final = json.loads((component/'service.json').read_text())
            facts = properties(unit)
            assert restored['installed'] and restored['activeState'] == 'active'
            template_hash_by_unit[unit] = digest(template)
            assert final['installed'] and not final['pending_install'] and final['maintenance_owner'] is None
            assert Path('/proc', facts['MainPID'], 'exe').resolve() == expected_binary
            assert digest(expected_binary) == expected_hash
            assert reader.is_file() and not (component/'state/component-maintenance.json').exists()
            reader_receipt = json.loads(reader.read_text())
            assert reader_receipt['program'] == str(frozen) and reader_receipt['profile'] == str(profile)
            with socket.socket(socket.AF_UNIX) as connection:
                connection.settimeout(5)
                connection.connect(str(sock))
                connection.sendall(b'GET /v1/status HTTP/1.1\r\nHost: cmsd\r\nConnection: close\r\nContent-Length: 0\r\n\r\n')
                raw = b''
                while block := connection.recv(65536):
                    raw += block
                daemon = json.loads(raw.split(b'\r\n\r\n', 1)[1])
            assert daemon['service'] == 'cmsd' and daemon['component_maintenance'] is False, daemon
            assert daemon['store_adoption']['phase'] == 'ready', daemon['store_adoption']
            with socket.socket(socket.AF_UNIX) as connection:
                connection.settimeout(5)
                connection.connect(str(sock))
                connection.sendall(b'GET /v1/models HTTP/1.1\r\nHost: cmsd\r\nConnection: close\r\nContent-Length: 0\r\n\r\n')
                raw = b''
                while block := connection.recv(65536):
                    raw += block
                head, body = raw.split(b'\r\n\r\n', 1)
                assert head.startswith(b'HTTP/1.1 200 '), head
                inventory = json.loads(body)
            assert inventory['models'] == [] and inventory['selection_policy'] == 'latest-installed-v1'
            assert inventory['store_reader'] == {'program': str(frozen), 'profile': str(profile)}
            record(fault+'-restore-completes-real-daemon-start', {'pid': facts['MainPID'],
                'operation': restored['operation']['status'], 'recovery_action': recovery_action, 'pending_install': final['pending_install'],
                'daemon_version': daemon['version'], 'component_maintenance': daemon['component_maintenance'],
                'store_adoption': daemon['store_adoption']['phase'], 'reader_roundtrip_verified': True})
            cleanup(unit, unit_file)
            all_units.remove((unit, unit_file))
            record(fault+'-unique-unit-cleaned', {'unit': unit, 'MainPID': properties(unit)['MainPID']})
        report = {'passed': True, 'seconds': time.monotonic()-started, 'cases': cases,
                  'pinvou_sha256': digest(frozen), 'package_sha256': digest(package/'cmsd'),
                  'private_namespace_templates_removed': removed_templates,
                  'no_test_native_unit_instances': True,
                  'scope': 'Real component worker, private profile, unique user units and real cmsd using the advertised Restore/Start action after first-install I/O faults. No model install/inference; no existing unit controlled.'}
        (evidence/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    except Exception as error:
        diagnostics = {'error': repr(error), 'cases_completed': [item['case'] for item in cases]}
        (evidence/'failure.json').write_text(json.dumps(diagnostics, indent=2)+'\n')
        raise
    finally:
        cleanup_errors = []
        for unit, unit_file in all_units:
            try:
                cleanup(unit, unit_file)
            except Exception as error:
                cleanup_errors.append({'unit': unit, 'error': str(error)})
        # Preserve diagnostics, not a live fixture. All generated profiles,
        # sockets and frozen binaries are removed even after an assertion fails.
        if cleanup_errors:
            (evidence/'cleanup-errors.json').write_text(json.dumps(cleanup_errors, indent=2)+'\n')
            (evidence/'retained-work.txt').write_text(str(work)+'\n')
        else:
            shutil.rmtree(work)
        if cleanup_errors:
            raise RuntimeError('Could not safely finish cleanup; see cleanup-errors.json')


if __name__ == '__main__':
    main()

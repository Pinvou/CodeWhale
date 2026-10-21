#!/usr/bin/env python3
"""Opt-in real user-manager test; creates one unique unit, never installs models.

Requires a built Pinvou executable, a verified offline cmsd package and a C compiler.
No downloads, no root, no changes to an existing service or package.
"""
import argparse
import fcntl
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pinvou', type=Path, required=True)
    parser.add_argument('--package', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    args = parser.parse_args()
    pinvou, package = args.pinvou.resolve(), args.package.resolve()
    evidence = args.evidence.resolve()
    evidence.mkdir(parents=True, exist_ok=False, mode=0o700)
    started = time.monotonic()
    token = uuid.uuid4().hex[:12]
    unit = f'pinvou-cmsd-binding-{token}.service'
    unit_file = Path.home()/'.config/systemd/user'/unit
    assert not unit_file.exists()
    assert not (unit_file.parent/(unit+'.d')).exists()
    work = Path(tempfile.mkdtemp(prefix='pin-cbind-'))
    sock = work/'cmsd.sock'
    probe = socket.socket()
    probe.bind(('127.0.0.1', 0))
    gateway_port = probe.getsockname()[1]
    probe.close()
    env_base = {**os.environ, 'CMSD_SOCKET': str(sock), 'PINVOU3_CMSD_UNIT': unit,
                'PINVOU3_CMSD_GATEWAY': f'127.0.0.1:{gateway_port}',
                'XDG_CONFIG_HOME': str(Path.home()/'.config')}
    profiles = {key: {**env_base, 'PINVOU3_HOME': str(work/key)} for key in ('a', 'b')}
    profiles['c'] = {**env_base, 'PINVOU3_HOME': str(work/'c'),
                     'XDG_CONFIG_HOME': str(work/'other-config')}
    checks = []
    component = work/'a/megacube/cmsd-component'
    state = component/'state'
    namespace = 'megacube-model-' + hashlib.sha256(os.fsencode(state/'env')).hexdigest()[:16]
    template = unit_file.parent/(namespace+'@.service')
    template_backup = template.with_suffix('.service.cmsd-before-profile-v1')
    for owned in (template, template_backup):
        assert not owned.exists() and not owned.is_symlink(), 'private template namespace already exists'
    template_sha256 = None
    removed_templates = []

    def systemd(*values):
        return subprocess.check_output(['systemctl', '--user', *values], text=True, timeout=40).strip()

    def pid(name=unit):
        return systemd('show', name, '--property=MainPID', '--value')

    original_pid = pid('cmsd.service')
    original_hash = hashlib.sha256((package/'cmsd').read_bytes()).hexdigest()

    def record(name, value):
        checks.append(name)
        with (evidence/'events.jsonl').open('a') as output:
            output.write(json.dumps({'case': name, 'at': time.time(), 'value': value})+'\n')
        print(name, flush=True)

    def cli(profile, *values, failure=False):
        result = subprocess.run([str(pinvou), '--cmsd-component-cli', *values], env=profiles[profile],
                                capture_output=True, text=True, timeout=45)
        if failure:
            assert result.returncode, result.stdout
            return result.stderr
        assert result.returncode == 0, result.stderr
        return json.loads(result.stdout)

    def action(profile, name, artifact=None, expected='succeeded'):
        values = ['action', name]
        if artifact:
            manifest = json.loads((artifact/'manifest.json').read_text())
            values += [str(artifact), manifest['sha256']]
        receipt = cli(profile, *values)
        deadline = time.monotonic()+55
        while time.monotonic() < deadline:
            status = cli(profile, 'status')
            operation = status.get('operation') or {}
            if operation.get('id') == receipt['operationId'] and operation['status'] not in ('queued', 'running'):
                record(f'{profile}-{name}-{expected}', operation)
                assert operation['status'] == expected, operation
                return status
            time.sleep(.15)
        raise TimeoutError((profile, name))

    def quiesce_workers():
        for profile in (work/'a', work/'b', work/'c'):
            quiesce_worker(profile)

    def quiesce_worker(profile):
        component = profile/'megacube/cmsd-component'
        operation_path = component/'operation.json'
        if not operation_path.exists():
            return
        operation = json.loads(operation_path.read_text())
        operation_id = operation.get('id', '')
        assert operation_id.startswith('component-') and all(
            letter.isascii() and (letter.isalnum() or letter == '-') for letter in operation_id)
        assert hasattr(os, 'pidfd_open') and hasattr(signal, 'pidfd_send_signal')
        for entry in Path('/proc').iterdir():
            if not entry.name.isdigit():
                continue
            descriptor = None
            try:
                if entry.stat().st_uid != os.geteuid():
                    continue
                descriptor = os.pidfd_open(int(entry.name), 0)
                if (entry/'exe').resolve(strict=True) != pinvou:
                    continue
                argv = [part for part in (entry/'cmdline').read_bytes().split(b'\0') if part]
                if argv[1:] != [b'--cmsd-component-worker', operation_id.encode()]:
                    continue
                environment = dict(item.split(b'=', 1) for item in (entry/'environ').read_bytes().split(b'\0') if b'=' in item)
                if environment.get(b'PINVOU3_HOME') != os.fsencode(profile):
                    continue
                signal.pidfd_send_signal(descriptor, signal.SIGKILL)
                deadline = time.monotonic()+3
                while time.monotonic() < deadline:
                    try:
                        process_state = (entry/'stat').read_text().rsplit(') ', 1)[1].split()[0]
                    except FileNotFoundError:
                        break
                    if process_state in ('Z', 'X'):
                        break
                    time.sleep(.02)
                else:
                    raise TimeoutError('test-owned worker did not exit')
            except (FileNotFoundError, ProcessLookupError, PermissionError):
                continue
            finally:
                if descriptor is not None:
                    os.close(descriptor)

    def remove_private_template():
        marker = state/'native-unit-namespace-v1'
        if any(owned.exists() or owned.is_symlink() for owned in (template, template_backup)):
            assert marker.is_file() and not marker.is_symlink(), 'private namespace receipt is missing'
        if marker.exists() or marker.is_symlink():
            assert marker.is_file() and not marker.is_symlink() and marker.stat().st_uid == os.geteuid()
            assert marker.read_text() == 'cmsd-native-unit-namespace-v1\n'+namespace+'\n'
        assert not systemd('list-units', namespace+'@*.service', '--all', '--no-legend', '--plain'), (
            'private namespace unexpectedly has a native model instance')
        unexpected = [path for path in unit_file.parent.glob(namespace+'@*.service*')
                      if path not in (template, template_backup)]
        assert not unexpected, 'private namespace unexpectedly has an instance registration'
        for owned in (template, template_backup):
            if not owned.exists() and not owned.is_symlink():
                continue
            metadata = owned.lstat()
            assert owned.is_file() and not owned.is_symlink() and metadata.st_uid == os.geteuid()
            assert metadata.st_size <= 65536 and metadata.st_mode & 0o022 == 0
            content = owned.read_text()
            assert content.startswith('[Unit]\nDescription=MegaCube model engine (card %i)\n')
            assert 'EnvironmentFile='+str(state/'env'/'%i.env')+'\n' in content
            if owned == template and template_sha256:
                assert hashlib.sha256(owned.read_bytes()).hexdigest() == template_sha256
            owned.unlink()
            removed_templates.append(owned.name)

    try:
        action('a', 'install', package)
        template_sha256 = hashlib.sha256(template.read_bytes()).hexdigest()
        status = cli('a', 'status')
        assert status['installed'] and status['ownedUnit']
        baseline_pid = pid()
        baseline_unit = unit_file.read_bytes()
        service_file = work/'a/megacube/cmsd-component/service.json'
        service = json.loads(service_file.read_text())
        config = Path(service['config'])
        baseline_config = config.read_bytes()
        assert service['registration']['binary_sha256'] == original_hash
        assert b'CMSD_CONTROL_LISTEN' in baseline_unit
        # The previous exact owned-unit template lacked UnsetEnvironment. A
        # legacy receipt must still be recognized without replacing that unit.
        legacy = {key: value for key, value in service.items() if key not in ('registration', 'bootstrap_import')}
        service_file.write_text(json.dumps(legacy))
        old_template = b'\n'.join(line for line in baseline_unit.split(b'\n') if not line.startswith(b'UnsetEnvironment='))
        unit_file.write_bytes(old_template)
        systemd('daemon-reload')
        assert cli('a', 'status')['installed'] and pid() == baseline_pid
        unit_file.write_bytes(baseline_unit)
        systemd('daemon-reload')
        record('previous-exact-owned-unit-template-is-migratable', {'pid_unchanged': True})

        # Real canonical flock interoperates with the platform Python installer.
        lock_path = Path('/run/user')/str(os.geteuid())/'megacube-component-locks/cmsd-user-manager.lock'
        with lock_path.open('r+') as held:
            fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
            for profile in profiles:
                assert 'component_busy' in cli(profile, 'action', 'rescue_stop', failure=True)
            assert cli('a', 'status')['installed']
        assert pid() == baseline_pid
        record('canonical-python-flock-two-profiles', {'pid_unchanged': True})

        # A second profile observes but gains no uninstall authority.
        observed = cli('b', 'status')
        assert observed['installed'] and not observed['ownedUnit'] and not observed['canUninstall']
        # Legacy receipts can re-establish OS identity even while the API is
        # unresponsive. Pause only this unique test unit, then always resume it.
        for profile in ('a', 'b'):
            receipt_path = work/profile/'megacube/cmsd-component/service.json'
            legacy = json.loads(receipt_path.read_text())
            legacy.pop('registration', None)
            receipt_path.write_text(json.dumps(legacy))
        systemd('kill', '--kill-whom=main', '--signal=STOP', unit)
        held_connections = []
        try:
            full = False
            for _ in range(8192):
                connection = socket.socket(socket.AF_UNIX)
                connection.setblocking(False)
                try:
                    connection.connect(str(sock))
                except BlockingIOError:
                    connection.close()
                    full = True
                    break
                held_connections.append(connection)
            record('paused-api-queue-fixture', {'connections': len(held_connections), 'queue_full': full,
                    'process_state': next(line for line in Path('/proc', baseline_pid, 'status').read_text().splitlines() if line.startswith('State:'))})
            assert full, 'the paused test daemon listen queue did not fill within the 8192-connection test budget'
            assert cli('a', 'status')['canStop']
            assert cli('b', 'status')['canStop']
        finally:
            for connection in held_connections:
                connection.close()
            systemd('kill', '--kill-whom=main', '--signal=CONT', unit)
        action('a', 'enable_boot')
        action('b', 'enable_boot')
        assert pid() == baseline_pid
        record('legacy-receipts-and-paused-api-retain-os-identity', {'pid_unchanged': True, 'full_listen_queue': True})

        # A checksum-valid second artifact uses the same real executable with a
        # harmless ELF trailing byte; it exercises replacement without a download.
        alternate = work/'package-b'
        shutil.copytree(package, alternate)
        with (alternate/'cmsd').open('ab') as output:
            output.write(b'\0')
        manifest = json.loads((alternate/'manifest.json').read_text())
        manifest.update(sha256=hashlib.sha256((alternate/'cmsd').read_bytes()).hexdigest(),
                        size=(alternate/'cmsd').stat().st_size)
        (alternate/'manifest.json').write_text(json.dumps(manifest))
        # The same live service is visible from a profile whose user-unit
        # directory differs. It has no restorable unit, so reject before stop.
        assert not cli('c', 'status')['canUpdate']
        rejected = action('c', 'update', alternate, expected='failed')
        assert 'external_installation' in rejected['operation']['error']
        assert pid() == baseline_pid
        record('unrestorable-external-update-rejected-before-stop', {'pid_unchanged': True})
        action('a', 'update', alternate)
        changed_pid = pid()
        drifted = action('b', 'rescue_stop', expected='failed')
        assert 'service_configuration_changed' in drifted['operation']['error']
        assert pid() == changed_pid and unit_file.read_bytes() != baseline_unit
        action('a', 'rollback')
        assert unit_file.read_bytes() == baseline_unit
        record('stale-profile-cannot-stop-replacement', {'foreign_pid_unchanged': True})

        # A real ELF failure fixture reports the real artifact's introspection
        # contract but fails when started. Verify the existing compensation path
        # remains usable after adding current-registration checks.
        for mode, body in [('failed-start', 'return 42;'), ('unhealthy-process', 'for(;;)pause();')]:
            broken = work/('package-'+mode)
            broken.mkdir()
            source = work/(mode+'.c')
            source.write_text('#include <string.h>\n#include <unistd.h>\n'
                              'int main(int argc,char **argv){ if(argc==2 && '
                              '!strcmp(argv[1],"--component-info")){ char *args[]={'
                              +json.dumps(str(package/'cmsd'))+',"--component-info",0}; '
                              'execv(args[0],args); } '+body+' }\n')
            subprocess.run(['cc', str(source), '-o', str(broken/'cmsd')], check=True, timeout=30)
            (broken/'cmsd').chmod(0o700)
            failed_manifest = json.loads((package/'manifest.json').read_text())
            failed_manifest.update(sha256=hashlib.sha256((broken/'cmsd').read_bytes()).hexdigest(),
                                   size=(broken/'cmsd').stat().st_size)
            (broken/'manifest.json').write_text(json.dumps(failed_manifest))
            compensated = action('a', 'update', broken, expected='failed')
            assert 'rollback=succeeded' in compensated['operation']['error']
            assert compensated['activeState'] == 'active' and unit_file.read_bytes() == baseline_unit
            record(mode+'-compensation-retains-owned-registration', {'original_component_restored': True})

        # Registration snapshots survive stopped services and reject even benign
        # config drift before acquiring daemon maintenance or stopping anything.
        before = pid()
        config.write_bytes(baseline_config+b'\n# separate administrator edit\n')
        drifted = action('a', 'stop', expected='failed')
        assert 'service_configuration_changed' in drifted['operation']['error']
        assert pid() == before
        config.write_bytes(baseline_config)
        record('config-drift-does-not-stop-daemon', {'pid_unchanged': True})

        # B has a stale snapshot after A's update; discard only this test-owned
        # receipt to rediscover the restored original before explicit B update.
        (work/'b/megacube/cmsd-component/service.json').unlink()
        assert cli('b', 'status')['installed']
        action('b', 'update', alternate)
        foreign_pid = pid()
        override = unit_file.parent/(unit+'.d/90-pinvou-cmsd.conf')
        override_bytes = override.read_bytes()
        refused = action('a', 'rollback', expected='failed')
        assert 'service_' in refused['operation']['error']
        assert pid() == foreign_pid and override.read_bytes() == override_bytes
        action('b', 'rollback')
        assert not override.exists() and unit_file.read_bytes() == baseline_unit
        # An empty drop-in directory left by our own removal carries no override.
        override.parent.rmdir()
        record('stale-owner-rollback-cannot-overwrite-other-profile', {'pid_unchanged': True})

        action('a', 'uninstall')
        assert not unit_file.exists() and config.read_bytes() == baseline_config
        unit_file.write_text('[Service]\nExecStart=/usr/bin/sleep infinity\n')
        systemd('daemon-reload')
        systemd('start', unit)
        foreign_pid = pid()
        replacement = unit_file.read_bytes()
        action('a', 'restore', expected='failed')
        assert pid() == foreign_pid and unit_file.read_bytes() == replacement
        systemd('stop', unit)
        unit_file.unlink()
        systemd('daemon-reload')
        action('a', 'restore')
        action('a', 'stop')
        action('a', 'start')
        record('restore-refuses-foreign-unit-and-works-after-removal', {'passed': True})
        assert pid('cmsd.service') == original_pid
        assert hashlib.sha256((package/'cmsd').read_bytes()).hexdigest() == original_hash
        report = {'passed': False, 'functional_checks_passed': True, 'cleanup_completed': False, 'cases': checks, 'seconds': time.monotonic()-started,
                  'pinvou_sha256': hashlib.sha256(pinvou.read_bytes()).hexdigest(),
                  'package_sha256': original_hash, 'original_cmsd_pid_unchanged': True,
                  'scope': 'real isolated systemd user service and real cmsd; no model inference or native desktop input'}
        (evidence/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    finally:
        try:
            quiesce_workers()
            subprocess.run(['systemctl', '--user', 'stop', unit], capture_output=True, timeout=40)
            subprocess.run(['systemctl', '--user', 'disable', unit], capture_output=True, timeout=40)
            subprocess.run(['systemctl', '--user', '--runtime', 'disable', unit], capture_output=True, timeout=40)
            subprocess.run(['systemctl', '--user', 'reset-failed', unit], capture_output=True, timeout=40)
            assert pid() == '0', 'test-owned component must exit before template cleanup'
            if unit_file.exists():
                unit_file.unlink()
            override_dir = unit_file.parent/(unit+'.d')
            if override_dir.exists():
                shutil.rmtree(override_dir)
            remove_private_template()
            subprocess.run(['systemctl', '--user', 'daemon-reload'], check=True, timeout=40)
            # Publish success only after the private registrations and work are gone.
            if (evidence/'report.json').exists():
                report = json.loads((evidence/'report.json').read_text())
                shutil.rmtree(work)
                report.update(passed=report.get('functional_checks_passed') is True,
                              cleanup_completed=True, private_namespace_templates_removed=removed_templates,
                              no_test_native_unit_instances=True)
                (evidence/'report.json').write_text(json.dumps(report, indent=2)+'\n')
            else:
                (evidence/'retained-work.txt').write_text(str(work)+'\n')
        except Exception as error:
            (evidence/'retained-work.txt').write_text(str(work)+'\n')
            try:
                report = json.loads((evidence/'report.json').read_text())
            except (OSError, ValueError):
                report = {}
            report.update(passed=False, cleanup_completed=False, cleanup_error=repr(error),
                          private_work_retained=str(work), private_namespace_templates_removed=removed_templates)
            (evidence/'report.json').write_text(json.dumps(report, indent=2)+'\n')
            raise



if __name__ == '__main__':
    main()

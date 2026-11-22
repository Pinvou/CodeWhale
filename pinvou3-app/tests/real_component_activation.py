#!/usr/bin/env python3
"""Opt-in real rollback/restore failure recovery using a test-owned user unit.

Fault injection provisions a checksum-valid ELF failure fixture in this private
test cache and constructs a historical receipt pointing to it. No production
service, model or credential is modified. Requires a C compiler and systemd user.
"""
import argparse
import copy
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
    parser.add_argument('--pinvou', required=True, type=Path)
    parser.add_argument('--package', required=True, type=Path)
    parser.add_argument('--evidence', required=True, type=Path)
    parser.add_argument('--ui-checkpoint', action='store_true')
    args = parser.parse_args()
    binary, package = args.pinvou.resolve(), args.package.resolve()
    real_systemctl = shutil.which('systemctl')
    assert real_systemctl
    evidence = args.evidence.resolve()
    evidence.mkdir(parents=True, mode=0o700, exist_ok=False)
    work = Path(tempfile.mkdtemp(prefix='pin-activate-'))
    frozen = work/'pinvou'
    subprocess.run(['cp', '--reflink=auto', str(binary), str(frozen)], check=True)
    frozen.chmod(0o700)
    profile, sock = work/'profile', work/'cmsd.sock'
    unit = 'pinvou-cmsd-activate-'+uuid.uuid4().hex[:12]+'.service'
    unit_file = Path.home()/'.config/systemd/user'/unit
    assert not unit_file.exists() and not unit_file.with_name(unit+'.d').exists()
    probe = socket.socket()
    probe.bind(('127.0.0.1', 0))
    gateway = probe.getsockname()[1]
    probe.close()
    env = {**os.environ, 'PINVOU3_HOME': str(profile), 'CMSD_SOCKET': str(sock),
           'PINVOU3_CMSD_UNIT': unit, 'PINVOU3_CMSD_GATEWAY': f'127.0.0.1:{gateway}',
           'XDG_CONFIG_HOME': str(Path.home()/'.config')}
    component = profile/'megacube/cmsd-component'
    marker = work/'compensation-fault.json'
    wrapper_log = evidence/'compensation-reload-events.jsonl'
    wrapper_dir = work/'bin'
    wrapper_dir.mkdir(mode=0o700)
    wrapper = wrapper_dir/'systemctl'
    wrapper.write_text('#!/usr/bin/python3\nimport json,os,pathlib,sys,time\n'
        + f'real={real_systemctl!r}\nmarker=pathlib.Path({str(marker)!r})\nunit=pathlib.Path({str(unit_file)!r})\ncomponent=pathlib.Path({str(component)!r})\nlog=pathlib.Path({str(wrapper_log)!r})\n'
        + "if marker.exists():\n"
        + "    fault=json.loads(marker.read_text())\n"
        + "    if sys.argv[1:]==['--user','start',unit.name] and not fault['start_failed']:\n"
        + "        fault['start_failed']=True\n        marker.write_text(json.dumps(fault))\n"
        + "        print('private injected Restore start failure',file=sys.stderr)\n        sys.exit(1)\n"
        + "    journal=component/'registration-transition.json'\n"
        + "    if sys.argv[1:]==['--user','daemon-reload'] and journal.is_file():\n"
        + "        intent=json.loads(journal.read_text())\n"
        + "        assert intent['before']['unit']==unit.name and intent['after']['unit']==unit.name\n"
        + "        removal=intent['before']['installed'] and not intent['after']['installed'] and intent['new_text'] is None\n"
        + "        if fault['mode']!='normal' and (removal or fault['seen']):\n"
        + "            fault['seen']=True\n            marker.write_text(json.dumps(fault))\n"
        + "            with log.open('a') as out: out.write(json.dumps({'at':time.time(),'mode':fault['mode'],'unit_present':unit.exists(),'journal_present':True,'before_installed':intent['before']['installed'],'after_installed':intent['after']['installed']})+'\\n')\n"
        + "            if fault['mode']=='once': marker.unlink()\n"
        + "            print('private injected compensation removal reload failure',file=sys.stderr)\n            sys.exit(1)\n"
        + "os.execv(real,[real,*sys.argv[1:]])\n")
    wrapper.chmod(0o700)
    env['PATH'] = str(wrapper_dir)+os.pathsep+os.environ['PATH']
    service_file = component/'service.json'
    state = component/'state'
    namespace = 'megacube-model-' + hashlib.sha256(os.fsencode(state/'env')).hexdigest()[:16]
    template = unit_file.parent/(namespace+'@.service')
    template_backup = template.with_suffix('.service.cmsd-before-profile-v1')
    for owned in (template, template_backup):
        assert not owned.exists() and not owned.is_symlink(), 'private template namespace already exists'
    template_sha256 = None
    removed_templates = []
    started = time.monotonic()
    events = []

    def systemd(*values):
        return subprocess.check_output([real_systemctl, '--user', *values], text=True, timeout=40).strip()

    def pid(name=unit):
        return systemd('show', name, '--property=MainPID', '--value')

    original_pid = pid('cmsd.service')
    package_hash = hashlib.sha256((package/'cmsd').read_bytes()).hexdigest()

    def record(name, value):
        events.append(name)
        with (evidence/'events.jsonl').open('a') as output:
            output.write(json.dumps({'case': name, 'value': value, 'at': time.time()})+'\n')
        print(name, flush=True)

    def cli(*values):
        result = subprocess.run([str(frozen), '--cmsd-component-cli', *values], env=env,
                                capture_output=True, text=True, timeout=45)
        assert result.returncode == 0, result.stderr
        return json.loads(result.stdout)

    def action(name, artifact=None, expected='succeeded'):
        values = ['action', name]
        if artifact:
            values += [str(artifact), json.loads((artifact/'manifest.json').read_text())['sha256']]
        receipt = cli(*values)
        deadline = time.monotonic()+60
        while time.monotonic() < deadline:
            status = cli('status')
            operation = status.get('operation') or {}
            if operation.get('id') == receipt['operationId'] and operation['status'] not in ('queued', 'running'):
                record(name+'-'+expected, operation)
                record(name+'-service-state', {'active_state': status.get('activeState'), 'pid': pid()})
                assert operation['status'] == expected, operation
                return status
            time.sleep(.15)
        raise TimeoutError(name)

    def write_record(path, value):
        path.write_text(json.dumps(value))
        path.chmod(0o600)

    def quiesce_worker():
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
                if (entry/'exe').resolve(strict=True) != frozen:
                    continue
                argv = [part for part in (entry/'cmdline').read_bytes().split(b'\0') if part]
                if argv[1:] != [b'--cmsd-component-worker', operation_id.encode()]:
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
        action('install', package)
        template_sha256 = hashlib.sha256(template.read_bytes()).hexdigest()
        good = json.loads(service_file.read_text())
        config = Path(good['config'])
        config_bytes = config.read_bytes()
        good_unit = unit_file.read_bytes()
        authority = {path: hashlib.sha256(path.read_bytes()).hexdigest()
                     for path in Path(good['state']).rglob('*') if path.is_file()
                     and ('identity' in str(path.relative_to(good['state'])) or 'remote-v2' in path.parts)}

        # This intentionally invalid *runtime* is still a valid inspected ELF
        # package. The original real cmsd is restored and verified after failure.
        source = work/'failure.c'
        source.write_text('#include <string.h>\n#include <unistd.h>\n'
                          'int main(int argc,char **argv){if(argc==2&&!strcmp(argv[1],"--component-info"))'
                          '{char *args[]={'+json.dumps(str(package/'cmsd'))+',"--component-info",0};execv(args[0],args);}return 42;}\n')
        bad = work/'failure-cmsd'
        subprocess.run(['cc', str(source), '-o', str(bad)], check=True, timeout=30)
        bad.chmod(0o700)
        digest = hashlib.sha256(bad.read_bytes()).hexdigest()
        release = component/'releases'/digest
        release.mkdir(mode=0o700)
        cached = release/'cmsd'
        shutil.copyfile(bad, cached)
        cached.chmod(0o700)
        manifest = json.loads((package/'manifest.json').read_text())
        manifest.update(sha256=digest, size=cached.stat().st_size)
        write_record(release/'manifest.json', manifest)
        bad_history = copy.deepcopy(good)
        bad_history.update(binary=str(cached), previous=None, maintenance_owner=None)
        bad_history['registration']['binary_sha256'] = digest
        bad_history['registration']['unit_sha256'] = hashlib.sha256(good_unit.replace(str(good['binary']).encode(), str(cached).encode())).hexdigest()
        write_record(component/'backup.json', bad_history)
        failed = action('rollback', expected='failed')
        assert failed['operation']['recovery'] == {'status': 'succeeded', 'previousState': 'running', 'error': None}
        assert failed['activeState'] == 'active' and Path('/proc', pid(), 'exe').resolve() == Path(good['binary'])
        assert unit_file.read_bytes() == good_unit and config.read_bytes() == config_bytes
        record('failed-rollback-restores-real-running-component', {'passed': True})

        action('stop')
        write_record(component/'backup.json', bad_history)
        failed = action('rollback', expected='failed')
        assert failed['operation']['recovery']['status'] == 'succeeded'
        assert failed['operation']['recovery']['previousState'] == 'stopped'
        assert failed['activeState'] == 'inactive' and pid() == '0'
        action('start')
        record('failed-rollback-preserves-previous-stopped-state', {'passed': True})

        action('uninstall')
        retained = json.loads(service_file.read_text())
        retained.update(binary=str(cached), previous=None)
        retained['registration'] = copy.deepcopy(bad_history['registration'])
        write_record(service_file, retained)
        failed = action('restore', expected='failed')
        assert failed['operation']['recovery'] == {'status': 'succeeded', 'previousState': 'uninstalled', 'error': None}
        assert not failed['installed'] and failed['canUpdate'] and not unit_file.exists() and pid() == '0'
        assert config.read_bytes() == config_bytes
        record('failed-restore-returns-to-unregistered-with-data', {'passed': True})

        if args.ui_checkpoint:
            config = {'pinvou': str(binary), 'profile': str(profile), 'socket': str(sock), 'unit': unit,
                      'package': str(package), 'sha256': package_hash}
            path = evidence/'ui.json'
            path.write_text(json.dumps(config, indent=2))
            path.chmod(0o600)
            record('UI_CHECKPOINT', {'configuration': str(path)})
            deadline = time.monotonic()+600
            while not (evidence/'ui-complete').exists():
                if time.monotonic() > deadline:
                    raise TimeoutError('bounded UI checkpoint')
                time.sleep(.2)
            restored = cli('status')
            assert restored['operation']['status'] == 'succeeded'
        else:
            restored = action('update', package)
        assert restored['installed'] and restored['activeState'] == 'active'
        assert Path('/proc', pid(), 'exe').resolve() == Path(good['binary'])
        for path, old_hash in authority.items():
            assert hashlib.sha256(path.read_bytes()).hexdigest() == old_hash
        assert pid('cmsd.service') == original_pid
        assert hashlib.sha256((package/'cmsd').read_bytes()).hexdigest() == package_hash
        record('trusted-package-repairs-retained-installation', {'passed': True})

        # Real good software, private injected start failure: compensation must
        # return to absence or leave a proven unit/journal, never bare absence
        # with an installed receipt. A later ordinary Restore must still work.
        for mode in ('normal', 'once', 'persistent'):
            action('uninstall')
            before = json.loads(service_file.read_text())
            assert not before['installed'] and not unit_file.exists()
            marker.write_text(json.dumps({'mode': mode, 'seen': False, 'start_failed': False}))
            marker.chmod(0o600)
            failed = action('restore', expected='failed')
            recovery = failed['operation']['recovery']
            current = json.loads(service_file.read_text())
            journal = component/'registration-transition.json'
            assert recovery['previousState'] == 'uninstalled'
            assert pid() == '0'
            if mode == 'normal':
                assert recovery['status'] == 'succeeded' and not current['installed']
                assert not unit_file.exists() and not journal.exists()
            else:
                assert recovery['status'] == 'failed' and current['installed'] and unit_file.is_file()
                if mode == 'persistent':
                    intent = json.loads(journal.read_text())
                    assert intent['before']['installed'] and not intent['after']['installed']
                    assert {**intent['after'], 'installed': True} == intent['before']
                else:
                    assert not journal.exists()
            marker.unlink(missing_ok=True)
            observation = cli('status')
            assert observation['scope'] == 'user' and not journal.exists()
            restored = action('restore')
            assert restored['installed'] and restored['activeState'] == 'active'
            assert Path('/proc', pid(), 'exe').resolve() == Path(good['binary'])
            assert config.read_bytes() == config_bytes and unit_file.read_bytes() == good_unit
            assert not (state/'component-maintenance.json').exists()
            record('restore-'+mode+'-removal-recovery-and-subsequent-restore', {'passed': True})
        assert pid('cmsd.service') == original_pid
        assert hashlib.sha256((package/'cmsd').read_bytes()).hexdigest() == package_hash
        (evidence/'report.json').write_text(json.dumps({'passed': True, 'seconds': time.monotonic()-started,
             'duration_includes_ui_checkpoint': args.ui_checkpoint,
             'cases': events, 'pinvou_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
             'package_sha256': package_hash, 'original_cmsd_pid_unchanged': True,
             'scope': 'real systemd and real cmsd recovery; explicit test-owned historical failure-fixture receipt, no model inference or native input'}, indent=2)+'\n')
    finally:
        marker.unlink(missing_ok=True)
        quiesce_worker()
        subprocess.run([real_systemctl, '--user', 'stop', unit], capture_output=True, timeout=40)
        subprocess.run([real_systemctl, '--user', 'disable', unit], capture_output=True, timeout=40)
        subprocess.run([real_systemctl, '--user', '--runtime', 'disable', unit], capture_output=True, timeout=40)
        subprocess.run([real_systemctl, '--user', 'reset-failed', unit], capture_output=True, timeout=40)
        if unit_file.exists():
            assert unit_file.is_file() and not unit_file.is_symlink()
            content = unit_file.read_text()
            assert content.startswith('[Unit]\nDescription=Pinvou managed Cube Manage Software device service\n')
            assert str(component) in content, 'refuse to remove a foreign registration'
            unit_file.unlink()
        try:
            remove_private_template()
            subprocess.run([real_systemctl, '--user', 'daemon-reload'], check=True, timeout=40)
            assert pid() == '0' and not unit_file.exists()
        except Exception as error:
            (evidence/'cleanup-errors.json').write_text(json.dumps({'error': repr(error)})+'\n')
            (evidence/'retained-work.txt').write_text(str(work)+'\n')
            raise
        if (evidence/'report.json').exists():
            report = json.loads((evidence/'report.json').read_text())
            report.update(private_namespace_templates_removed=removed_templates,
                          no_test_native_unit_instances=True)
            (evidence/'report.json').write_text(json.dumps(report, indent=2)+'\n')
            shutil.rmtree(work)
        else:
            (evidence/'retained-work.txt').write_text(str(work)+'\n')


if __name__ == '__main__':
    main()

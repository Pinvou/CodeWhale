#!/usr/bin/env python3
"""Actual isolated config failures and offline diagnostics; no model inference.

--baseline records the old daemon's ignored errors/hang, without asserting fixed
behavior. All candidate paths are overridden into a NEW private directory, so
even a broken binary cannot open the user's default cmsd state or endpoint.
"""
import argparse
import concurrent.futures
import hashlib
import json
import os
import signal
import socket
import subprocess
import tempfile
import time
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--baseline', action='store_true')
    args = parser.parse_args()
    assert os.geteuid() != 0, 'unreadable-file test requires unprivileged user'
    root = args.evidence.resolve(); root.mkdir(mode=0o700, parents=True, exist_ok=False)
    binary = root / 'cmsd-tested'; binary.write_bytes(args.binary.resolve().read_bytes()); binary.chmod(0o700)
    report = {'scope': __doc__.strip(), 'baseline': args.baseline, 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'cases': {}, 'errors': []}
    started = time.monotonic()
    protected = subprocess.check_output(['systemctl', '--user', 'show', 'cmsd.service', '--property=MainPID'], text=True).strip()
    runtime = tempfile.TemporaryDirectory(prefix='cmsd-cfg-', dir='/run/user/' + str(os.geteuid()))
    short = Path(runtime.name)
    canary = 'secret_config_canary_do_not_log'
    base_env = {k: v for k, v in os.environ.items() if not k.startswith('CMSD_')}
    try:
        for case in ('missing', 'unreadable', 'directory', 'fifo', 'oversized', 'bad-utf8', 'bad-toml', 'duplicate', 'unknown-key', 'numeric-type', 'bad-mode', 'bad-env', 'relative-path'):
            directory = root / case; directory.mkdir(mode=0o700)
            config = directory / 'config.toml'; config.write_text('systemd_mode = "user"\n')
            env = {**base_env, 'CMSD_CONFIG': str(config), 'CMSD_STATE_DIR': str(directory / 'state'),
                   'CMSD_SOCKET': str(short / (case + '.sock')), 'CMSD_GATEWAY_LISTEN': '127.0.0.1:0',
                   'CMSD_CARDS_DIR': str(directory / 'cards'), 'CMSD_UNIT_DIR': str(directory / 'units'),
                   'CMSD_ENGINE_ROOT': str(directory / 'engines'), 'CMSD_MODELS_DIR': str(directory / 'models'),
                   'CMSD_STAGING_DIR': str(directory / 'staging'), 'CMSD_STORE_SOCKET': str(short / 'absent-store.sock')}
            if case == 'missing': config.unlink()
            elif case == 'unreadable': config.chmod(0)
            elif case == 'directory': config.unlink(); config.mkdir(mode=0o700)
            elif case == 'fifo': config.unlink(); os.mkfifo(config, mode=0o600)
            elif case == 'oversized': config.write_text('#' + 'x' * 65536)
            elif case == 'bad-utf8': config.write_bytes(b'\xff')
            elif case == 'bad-toml': config.write_text('hf_endpoint = "' + canary + '\n')
            elif case == 'duplicate': config.write_text('pool_end = 19000\npool_end = 19001\n')
            elif case == 'unknown-key': config.write_text('state_dr = "/unintended/default"\n')
            elif case == 'numeric-type': config.write_text('store_poll_ms = "100"\n')
            elif case == 'bad-mode': config.write_text('systemd_mode = "users"\n')
            elif case == 'bad-env': env['CMSD_STORE_POLL_MS'] = 'not-a-number'
            elif case == 'relative-path': env['CMSD_SOCKET'] = 'relative.sock'
            output = directory / 'daemon.log'
            begin = time.monotonic()
            with output.open('wb') as log:
                process = subprocess.Popen([str(binary)], env=env, cwd=directory, stdout=log, stderr=log, start_new_session=True)
                try:
                    try: process.wait(timeout=.4 if args.baseline else 1.5)
                    except subprocess.TimeoutExpired: pass
                    running = process.poll() is None
                    state_created = (directory / 'state').exists()
                    report['cases'][case] = {'still_running': running, 'return_code': process.returncode, 'seconds': round(time.monotonic() - begin, 4), 'state_directory_created': state_created}
                    if not args.baseline:
                        assert process.returncode == 2 and not state_created, report['cases'][case]
                        assert not (short / (case + '.sock')).exists() and not (directory / 'relative.sock').exists()
                finally:
                    if process.poll() is None: os.killpg(process.pid, signal.SIGKILL); process.wait(timeout=5)
            assert canary not in output.read_text(errors='replace'), 'configuration value leaked in diagnostics'

        if not args.baseline:
            directory = root / 'valid'; directory.mkdir(mode=0o700)
            state = directory / 'unused-state'; sock = short / 'unused.sock'
            config = directory / 'config.toml'
            settings = {'socket_path': str(sock), 'state_dir': str(state), 'gateway_listen': '127.0.0.1:0', 'systemd_mode': 'user',
                        'store_socket_path': str(short / 'absent-store.sock'), 'cards_dir': str(directory / 'cards#literal'),
                        'unit_dir': str(directory / 'units'), 'engine_root': str(directory / 'engines')}
            config.write_text(''.join(k + ' = ' + json.dumps(v) + ' # inline comment\n' for k, v in settings.items()))
            config.chmod(0o600)
            alias = directory / 'alias.toml'; alias.symlink_to(config)
            env = {**base_env, 'CMSD_CONFIG': str(alias)}
            def inspect(_):
                result = subprocess.run([str(binary), '--check-config'], env=env, capture_output=True, timeout=2, check=True)
                value = json.loads(result.stdout)
                assert value['configuration_valid'] is True and value['read_only'] is True and value['runtime_access_verified'] is False
                assert value['cards_dir'] == settings['cards_dir'] and value['socket_path'] == str(sock)
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as executor: list(executor.map(inspect, range(32)))
            assert not state.exists() and not sock.exists() and not (directory / 'units').exists() and not (directory / 'cards#literal').exists()
            config.write_text('invalid ' + canary)
            for argument in ('--version', '--component-info'):
                result = subprocess.run([str(binary), argument], env=env, capture_output=True, timeout=2)
                assert result.returncode == 0 and canary.encode() not in result.stderr
            report['offline_diagnostics'] = {'concurrent_checks': 32, 'state_or_runtime_side_effects': False, 'literal_hash_and_inline_comments_preserved': True,
                                             'normal_file_config_symlink_supported': True, 'artifact_inspection_ignores_bad_config': True}
        assert protected == subprocess.check_output(['systemctl', '--user', 'show', 'cmsd.service', '--property=MainPID'], text=True).strip()
        report['protected_original_cmsd_unchanged'] = True
    except Exception as error:
        report['errors'].append(repr(error)); raise
    finally:
        runtime.cleanup()
        report['seconds'] = round(time.monotonic() - started, 3)
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
        print(json.dumps(report))


if __name__ == '__main__': main()

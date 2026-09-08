"""Opt-in real pgBackRest encryption/full/incremental/PITR test on new resources only."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import tempfile
import time

DATABASES = ('account', 'billing', 'token', 'keycloak', 'newapi')
KEYS = ('ACCOUNT_DB_PASSWORD', 'BILLING_DB_PASSWORD', 'TOKEN_DB_PASSWORD',
        'BILLING_MIGRATION_DB_PASSWORD', 'TOKEN_MIGRATION_DB_PASSWORD',
        'TOKEN_POLICY_DB_PASSWORD', 'NEWAPI_DB_PASSWORD', 'KEYCLOAK_DB_PASSWORD')


def require(condition, marker):
    if not condition:
        raise RuntimeError(marker)


def run(argv, *, data=None, env=None, check=True, timeout=60):
    # Third-party failure diagnostics can contain undecodable bytes. Never print
    # them; preserve fixed ASCII error markers and secret-canary checks instead.
    result = subprocess.run(argv, input=data, text=True, encoding='utf-8', errors='replace',
                            capture_output=True, env=env, timeout=timeout)
    require(not check or result.returncode == 0, 'BACKUP_FIXTURE_COMMAND_FAILED')
    return result


def private(path, value):
    with path.open('x', encoding='utf-8') as output:
        output.write(value)
    path.chmod(0o600)
    os.chown(path, 999, 999)


def info_backup(raw, kind):
    items = json.loads(raw)
    require(len(items) == 1 and items[0]['name'] == 'platform' and items[0]['status']['code'] == 0,
            'BACKUP_INFORMATION_NOT_READY')
    require(items[0]['cipher'] == 'aes-256-cbc', 'BACKUP_NOT_ENCRYPTED')
    matches = [b for b in items[0]['backup'] if b['type'] == kind and not b.get('error', False)]
    require(bool(matches), 'EXPECTED_BACKUP_TYPE_MISSING')
    return matches[-1]


def require_cipher_failure(exit_code, message):
    # pgBackRest 2.59.1 error.yaml assigns CryptoError exactly 95. Command
    # diagnostics use a numeric prefix, unlike info's nested named errors.
    require(type(exit_code) is int and exit_code == 95, 'WRONG_CIPHER_NOT_REJECTED')
    require(re.search(r'ERROR:\s*\[095\]', message) and ('unable to decrypt' in message or 'unable to flush' in message),
            'WRONG_CIPHER_FAILURE_NOT_CRYPTOGRAPHIC')
    require(not any(error in message for error in ('FileMissingError', 'FileOpenError', 'PermissionError')),
            'WRONG_CIPHER_FAILURE_NOT_CRYPTOGRAPHIC')


def verify_user(metadata):
    require(metadata['Config']['User'] == '999:999' and metadata['HostConfig']['Privileged'] is False
            and not metadata['HostConfig'].get('CapAdd'), 'CONTAINER_USER_OR_PRIVILEGES')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', action='store_true', required=True)
    parser.add_argument('--image', required=True, help='reviewed local sha256 image ID')
    args = parser.parse_args()
    require(os.name == 'posix' and os.geteuid() == 0, 'LINUX_ROOT_FIXTURE_ONLY')
    require(re.fullmatch(r'sha256:[a-f0-9]{64}', args.image), 'IMMUTABLE_IMAGE_REQUIRED')
    repository = Path(__file__).resolve().parents[1]
    source = repository / 'account-platform/deploy'
    spec = importlib.util.spec_from_file_location('boundary', repository / 'scripts/account-platform-keycloak-production-fixture.py')
    boundary = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(boundary)
    candidate = json.loads(run(['docker', 'image', 'inspect', args.image]).stdout)[0]
    require(candidate['Id'] == args.image and candidate['Config']['User'] == '999:999', 'WRONG_IMAGE_OR_UID')
    name = 'pinvou-pgbackup-fixture-' + secrets.token_hex(6)
    root = Path(tempfile.mkdtemp(prefix=name + '-', dir='/tmp')).resolve()
    root.chmod(0o755)
    containers = []
    network = None
    uncertain = False
    all_secrets = []
    try:
        for directory in ('data', 'tls', 'admin', 'bootstrap', 'backup', 'repo', 'restore', 'restore-incr', 'wrong', 'wrong-data'):
            target = root / directory
            target.mkdir(mode=0o700)
            os.chown(target, 999, 999)
        admin, cipher = secrets.token_hex(32), secrets.token_hex(32)
        all_secrets.extend((admin, cipher))
        private(root / 'admin/password', admin)
        for key in KEYS:
            value = secrets.token_hex(32)
            all_secrets.append(value)
            private(root / 'bootstrap' / key, value)
        template = (source / 'production/pgbackrest.conf.template').read_text()
        require(template.count('@REPOSITORY_CIPHER_PASS@') == 1, 'CIPHER_TEMPLATE_INVALID')
        private(root / 'backup/pgbackrest.conf', template.replace('@REPOSITORY_CIPHER_PASS@', cipher))
        private(root / 'backup/pgpass', ''.join(host + ':5432:postgres:account_platform_admin:' + admin + '\n'
                                               for host in ('/var/run/postgresql', 'localhost')))
        wrong_cipher = secrets.token_hex(32)
        all_secrets.append(wrong_cipher)
        private(root / 'wrong/pgbackrest.conf', template.replace('@REPOSITORY_CIPHER_PASS@', wrong_cipher))
        run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2',
             '-subj', '/CN=Temporary backup fixture CA', '-addext', 'basicConstraints=critical,CA:TRUE',
             '-addext', 'keyUsage=critical,keyCertSign,cRLSign', '-keyout', str(root / 'ca.key'), '-out', str(root / 'ca.crt')])
        (root / 'ca.key').chmod(0o600)
        run(['openssl', 'req', '-new', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=postgres',
             '-keyout', str(root / 'tls/server.key'), '-out', str(root / 'server.csr')])
        (root / 'ext').write_text('subjectAltName=DNS:postgres\nextendedKeyUsage=serverAuth\n')
        run(['openssl', 'x509', '-req', '-in', str(root / 'server.csr'), '-CA', str(root / 'ca.crt'),
             '-CAkey', str(root / 'ca.key'), '-CAcreateserial', '-days', '2', '-extfile', str(root / 'ext'),
             '-out', str(root / 'tls/server.crt')])
        for target in (root / 'tls').iterdir():
            target.chmod(0o600)
            os.chown(target, 999, 999)
        env = {**os.environ, 'POSTGRES_IMAGE': args.image,
               'POSTGRES_BACKUP_REPOSITORY': str(root / 'repo'), 'POSTGRES_BACKUP_SECRETS': str(root / 'backup'),
               **{'POSTGRES_' + key + '_DIRECTORY': str(root / value) for key, value in
                  (('DATA', 'data'), ('TLS', 'tls'), ('ADMIN', 'admin'), ('BOOTSTRAP', 'bootstrap'))}}
        compose = ['docker', 'compose', '--project-name', name, '-f', str(source / 'production/compose.dependencies.yaml'),
                   '-f', str(source / 'production/compose.postgres-backup.yaml')]
        expected = {
            '/var/lib/postgresql/data': (root / 'data', False),
            '/run/pinvou/postgres-tls': (root / 'tls', True),
            '/run/pinvou/postgres-admin': (root / 'admin', True),
            '/run/pinvou/postgres-bootstrap': (root / 'bootstrap', True),
            '/etc/pinvou/postgres/postgres.conf': (source / 'production/postgres-backup.conf', True),
            '/etc/pinvou/postgres/postgres-base.conf': (source / 'production/postgres.conf', True),
            '/etc/pinvou/postgres/pg_hba.conf': (source / 'production/pg_hba.conf', True),
            '/docker-entrypoint-initdb.d/10-pinvou.sh': (source / 'production/postgres-init.sh', True),
            **{'/opt/pinvou/production/' + f: (source / 'production' / f, True) for f in
               ('postgres-common.sh', 'postgres-entrypoint.sh', 'postgres-health.sh', 'postgres-backup.sh')},
            **{'/opt/pinvou/' + f: (source / 'compose' / f, True) for f in ('init-databases.sh', 'validate-database-secrets.sh')},
            '/var/lib/pgbackrest': (root / 'repo', False),
            '/run/pinvou/postgres-backup': (root / 'backup', True),
        }
        temporary = {
            '/tmp': 'rw,noexec,nosuid,nodev,size=16m,uid=999,gid=999,mode=1700',
            '/var/run/postgresql': 'rw,noexec,nosuid,nodev,size=8m,uid=999,gid=999,mode=0700',
        }
        uncertain = True
        run([*compose, 'create', 'postgres'], env=env)
        primary = run([*compose, 'ps', '--all', '-q', 'postgres'], env=env).stdout.strip()
        require(re.fullmatch(r'[a-f0-9]{64}', primary), 'PRIMARY_ID_UNKNOWN')
        containers.append(primary)
        metadata = json.loads(run(['docker', 'inspect', primary]).stdout)[0]
        network = metadata['NetworkSettings']['Networks'][name + '_data']['NetworkID']
        if not network:
            network = json.loads(run(['docker', 'network', 'inspect', name + '_data']).stdout)[0]['Id']
        require(re.fullmatch(r'[a-f0-9]{64}', network), 'NETWORK_ID_UNKNOWN')
        uncertain = False
        boundary.verify_limits(metadata, 1024 ** 3, 192)
        verify_user(metadata)
        boundary.verify_mounts(metadata, [(str(src), dst, ro) for dst, (src, ro) in expected.items()], temporary)
        require(metadata['Image'] == args.image and set(metadata['NetworkSettings']['Networks']) == {name + '_data'}, 'PRIMARY_BOUNDARY')
        require(json.loads(run(['docker', 'network', 'inspect', network]).stdout)[0]['Internal'], 'FIXTURE_NETWORK_NOT_INTERNAL')

        def wait_ready(cid):
            for _ in range(180):
                state = json.loads(run(['docker', 'inspect', cid]).stdout)[0]['State']
                require(state['Running'], 'BACKUP_POSTGRES_EXITED')
                if state.get('Health', {}).get('Status') == 'healthy':
                    return
                time.sleep(0.5)
            raise RuntimeError('BACKUP_POSTGRES_NOT_READY')

        def sql(cid, query, db='postgres'):
            return run(['docker', 'exec', '-i', '-u', '999:999', '-e',
                        'PGPASSFILE=/run/pinvou/postgres-backup/pgpass', cid, 'psql', '-X', '-At', '-w',
                        '-h', '/var/run/postgresql', '-U', 'account_platform_admin', '-d', db,
                        '--set=ON_ERROR_STOP=1'], data=query + '\n').stdout.strip()

        def backup(command, check=True):
            result = run(['docker', 'exec', '-u', '999:999', primary, '/bin/sh',
                          '/opt/pinvou/production/postgres-backup.sh', command], check=check, timeout=180)
            require(not any(secret in result.stdout + result.stderr for secret in all_secrets), 'BACKUP_SECRET_IN_OUTPUT')
            return result

        run(['docker', 'start', primary])
        wait_ready(primary)
        require(sql(primary, 'SHOW archive_mode;') == 'on' and sql(primary, 'SHOW archive_timeout;') == '30s', 'ARCHIVE_CONFIGURATION')
        # The fixture PGPASSFILE allows its five named databases; no production secret is used.
        pgpass = root / 'backup/pgpass'
        with pgpass.open('a') as output:
            for db in DATABASES:
                for host in ('/var/run/postgresql', 'localhost'):
                    output.write(host + ':5432:account_platform_' + db + ':account_platform_admin:' + admin + '\n')
        for db in DATABASES:
            sql(primary, 'CREATE TABLE backup_fixture (id integer PRIMARY KEY); INSERT INTO backup_fixture VALUES (1);', 'account_platform_' + db)
        source_id = sql(primary, 'SELECT system_identifier FROM pg_control_system();')
        roles = sql(primary, "SELECT rolname FROM pg_roles WHERE rolname NOT LIKE 'pg_%' ORDER BY rolname;")
        backup('create')
        backup('check')
        backup('full')
        full = info_backup(backup('info').stdout, 'full')
        print('PG_BACKUP_ENCRYPTED_FULL_PASS', flush=True)
        for db in DATABASES:
            sql(primary, 'INSERT INTO backup_fixture VALUES (2);', 'account_platform_' + db)
        sql(primary, "SELECT pg_create_restore_point('pinvou_backup_fixture_target');")
        for db in DATABASES:
            sql(primary, 'INSERT INTO backup_fixture VALUES (3);', 'account_platform_' + db)
        backup('check')
        archive = json.loads(backup('stats').stdout)
        require(archive['system_identifier'] == source_id and archive['last_archived_wal'], 'REAL_WAL_NOT_ARCHIVED')
        backup('incr')
        incremental = info_backup(backup('info').stdout, 'incr')
        require(incremental['prior'] == full['label'], 'INCREMENTAL_NOT_LINKED_TO_FULL')
        print('PG_BACKUP_INCREMENTAL_AND_REAL_WAL_PASS', flush=True)

        # Use short-lived, explicitly owned restore commands; repository always read-only.
        common = ['--network', 'none', '--user', '999:999', '--read-only', '--cap-drop', 'ALL',
                  '--security-opt', 'no-new-privileges', '--memory', '1g', '--cpus', '1', '--pids-limit', '192',
                  '--ulimit', 'core=0:0', '--label', 'pinvou.backup-fixture=' + name]
        for dst, options in temporary.items():
            common.extend(['--tmpfs', dst + ':' + options])
        restore_bindings = {**expected, '/var/lib/postgresql/data': (root / 'restore', False),
                            '/var/lib/pgbackrest': (root / 'repo', True)}

        def create_helper(helper_name, bindings, entrypoint, extra):
            nonlocal uncertain
            argv = ['docker', 'create', '--name', helper_name, *common, '-e', 'PGDATA=/var/lib/postgresql/data']
            for dst, (src, readonly) in bindings.items():
                argv.extend(['--mount', f'type=bind,src={src},dst={dst}' + (',readonly' if readonly else '')])
            argv.extend(['--entrypoint', entrypoint, args.image, *extra])
            uncertain = True
            cid = run(argv).stdout.strip()
            require(re.fullmatch(r'[a-f0-9]{64}', cid), 'HELPER_ID_UNKNOWN')
            containers.append(cid)
            uncertain = False
            meta = json.loads(run(['docker', 'inspect', cid]).stdout)[0]
            boundary.verify_limits(meta, 1024 ** 3, 192)
            verify_user(meta)
            boundary.verify_mounts(meta, [(str(src), dst, ro) for dst, (src, ro) in bindings.items()], temporary)
            require(meta['Image'] == args.image and meta['HostConfig']['NetworkMode'] == 'none', 'HELPER_BOUNDARY')
            return cid

        command = ['--config=/run/pinvou/postgres-backup/pgbackrest.conf', '--stanza=platform']
        wrong = create_helper(name + '-wrong', {**restore_bindings, '/run/pinvou/postgres-backup': (root / 'wrong', True),
                                              '/var/lib/postgresql/data': (root / 'wrong-data', False)},
                              '/usr/bin/pgbackrest', [*command, '--set=' + full['label'], '--type=immediate', '--archive-mode=off', 'restore'])
        wrong_result = run(['docker', 'start', '-a', wrong], check=False)
        wrong_state = json.loads(run(['docker', 'inspect', wrong]).stdout)[0]['State']
        # Test actual restore refusal, not info's third-party error-message JSON serialization.
        combined = wrong_result.stdout + wrong_result.stderr
        error_types = [name for name in ('CryptoError', 'FormatError', 'ChecksumError', 'FileMissingError',
                                         'FileOpenError', 'PathMissingError', 'OptionInvalidError') if name in combined]
        print('PG_BACKUP_NEGATIVE_EXIT_' + str(wrong_state['ExitCode']), flush=True)
        print('PG_BACKUP_NEGATIVE_TYPES_' + ('_'.join(error_types) or 'UNCLASSIFIED'), flush=True)
        require(not wrong_state['Running'] and not wrong_state['OOMKilled'], 'WRONG_CIPHER_RESULT_UNCERTAIN')
        require(not any((root / 'wrong-data').iterdir()), 'WRONG_CIPHER_WROTE_RESTORE_DATA')
        require(not any(secret in combined for secret in all_secrets), 'WRONG_CIPHER_SECRET_OUTPUT')
        restore_helper = create_helper(name + '-restore', restore_bindings, '/usr/bin/pgbackrest',
            [*command, '--set=' + full['label'], '--type=name', '--target=pinvou_backup_fixture_target',
             '--target-action=promote', '--archive-mode=off', 'restore'])
        started = time.monotonic()
        run(['docker', 'start', '-a', restore_helper], timeout=180)
        require(json.loads(run(['docker', 'inspect', restore_helper]).stdout)[0]['State']['ExitCode'] == 0, 'RESTORE_FAILED')
        restored = create_helper(name + '-recovered', restore_bindings, '/bin/sh', ['/opt/pinvou/production/postgres-entrypoint.sh'])
        run(['docker', 'start', restored])
        # docker create did not inherit Compose healthcheck; readiness is actual authenticated SQL.
        for _ in range(120):
            result = run(['docker', 'exec', '-u', '999:999', restored, '/bin/sh', '/opt/pinvou/production/postgres-health.sh'], check=False)
            if result.returncode == 0:
                break
            state = json.loads(run(['docker', 'inspect', restored]).stdout)[0]['State']
            require(state['Running'], 'RESTORED_POSTGRES_EXITED')
            time.sleep(0.5)
        else:
            raise RuntimeError('RESTORED_POSTGRES_NOT_READY')
        require(sql(restored, 'SHOW archive_mode;') == 'off', 'RESTORED_ARCHIVING_NOT_DISABLED')
        require(sql(restored, 'SELECT system_identifier FROM pg_control_system();') == source_id, 'RESTORED_CLUSTER_MISMATCH')
        require(sql(restored, "SELECT rolname FROM pg_roles WHERE rolname NOT LIKE 'pg_%' ORDER BY rolname;") == roles, 'RESTORED_ROLES_MISMATCH')
        for db in DATABASES:
            require(sql(restored, 'SELECT string_agg(id::text,\',\' ORDER BY id) FROM backup_fixture;', 'account_platform_' + db) == '1,2', 'PITR_TARGET_INCORRECT')
            require(sql(primary, 'SELECT string_agg(id::text,\',\' ORDER BY id) FROM backup_fixture;', 'account_platform_' + db) == '1,2,3', 'SOURCE_CHANGED_BY_RESTORE')
        require(time.monotonic() - started < 3600, 'FIXTURE_RESTORE_TIME_EXCEEDED')
        print('PG_BACKUP_FULL_PITR_TARGET_PASS', flush=True)
        incremental_bindings = {**restore_bindings, '/var/lib/postgresql/data': (root / 'restore-incr', False)}
        incremental_helper = create_helper(name + '-restore-incr', incremental_bindings, '/usr/bin/pgbackrest',
            [*command, '--set=' + incremental['label'], '--type=immediate', '--target-action=promote', '--archive-mode=off', 'restore'])
        run(['docker', 'start', '-a', incremental_helper], timeout=180)
        require(json.loads(run(['docker', 'inspect', incremental_helper]).stdout)[0]['State']['ExitCode'] == 0, 'INCREMENTAL_RESTORE_FAILED')
        recovered_incr = create_helper(name + '-recovered-incr', incremental_bindings, '/bin/sh', ['/opt/pinvou/production/postgres-entrypoint.sh'])
        run(['docker', 'start', recovered_incr])
        for _ in range(120):
            result = run(['docker', 'exec', '-u', '999:999', recovered_incr, '/bin/sh', '/opt/pinvou/production/postgres-health.sh'], check=False)
            if result.returncode == 0:
                break
            require(json.loads(run(['docker', 'inspect', recovered_incr]).stdout)[0]['State']['Running'], 'INCREMENTAL_POSTGRES_EXITED')
            time.sleep(0.5)
        else:
            raise RuntimeError('INCREMENTAL_POSTGRES_NOT_READY')
        require(sql(recovered_incr, 'SHOW archive_mode;') == 'off', 'INCREMENTAL_ARCHIVING_NOT_DISABLED')
        require(sql(recovered_incr, 'SELECT system_identifier FROM pg_control_system();') == source_id, 'INCREMENTAL_CLUSTER_MISMATCH')
        require(sql(recovered_incr, "SELECT rolname FROM pg_roles WHERE rolname NOT LIKE 'pg_%' ORDER BY rolname;") == roles, 'INCREMENTAL_ROLES_MISMATCH')
        for db in DATABASES:
            query = 'SELECT string_agg(id::text,\',\' ORDER BY id) FROM backup_fixture;'
            require(sql(recovered_incr, query, 'account_platform_' + db) == '1,2,3', 'INCREMENTAL_RESTORED_FACTS_INCORRECT')
            require(sql(primary, query, 'account_platform_' + db) == '1,2,3', 'INCREMENTAL_SOURCE_CHANGED')
        print('PG_BACKUP_INCREMENTAL_CHAIN_RESTORE_PASS', flush=True)
        # Restore can wrap unreadable encrypted backup.info as BackupSetInvalid
        # (75). Confirm that refusal against successful same-set restores, then
        # test the actual decryption primitive on the fixed existing info file.
        require(wrong_state['ExitCode'] in (75, 95, 29), 'WRONG_CIPHER_RESTORE_ERROR_UNEXPECTED')
        require((root / 'wrong/pgbackrest.conf').read_text().replace(wrong_cipher, cipher) ==
                (root / 'backup/pgbackrest.conf').read_text(), 'WRONG_CIPHER_CONTROL_DIFFERS')
        decrypt_helper = create_helper(name + '-wrong-decrypt',
            {**restore_bindings, '/run/pinvou/postgres-backup': (root / 'wrong', True),
             '/var/lib/postgresql/data': (root / 'wrong-data', True)},
            '/usr/bin/pgbackrest', [*command, 'repo-get', 'backup/platform/backup.info'])
        decrypt_result = run(['docker', 'start', '-a', decrypt_helper], check=False)
        decrypt_state = json.loads(run(['docker', 'inspect', decrypt_helper]).stdout)[0]['State']
        require(not decrypt_state['Running'] and not decrypt_state['OOMKilled'], 'WRONG_CIPHER_DECRYPT_UNCERTAIN')
        require_cipher_failure(decrypt_state['ExitCode'], decrypt_result.stderr + decrypt_result.stdout)
        require(not any(secret in decrypt_result.stdout + decrypt_result.stderr for secret in all_secrets), 'DECRYPT_SECRET_OUTPUT')
        print('PG_BACKUP_WRONG_CIPHER_REJECTED', flush=True)
        require(backup('restore', check=False).returncode != 0, 'ARBITRARY_OPERATION_ACCEPTED')
        for cid in containers:
            logs = run(['docker', 'logs', cid], check=False)
            require(not any(secret in logs.stdout + logs.stderr for secret in all_secrets), 'SECRET_IN_CONTAINER_LOGS')
        print('PG_BACKUP_FIVE_DATABASES_ROLES_PITR_SOURCE_PRESERVED_PASS', flush=True)
        print('PG_BACKUP_PRODUCTION_FIXTURE_PASS', flush=True)
    finally:
        require(not uncertain, 'CREATION_UNCERTAIN_PRESERVE_FIXTURE')
        for cid in reversed(containers):
            metadata = json.loads(run(['docker', 'inspect', cid]).stdout)[0]
            labels = metadata['Config'].get('Labels') or {}
            require(labels.get('com.docker.compose.project') == name or labels.get('pinvou.backup-fixture') == name, 'CLEANUP_OWNERSHIP_MISMATCH')
            run(['docker', 'rm', '-f', cid])
        if network:
            metadata = json.loads(run(['docker', 'network', 'inspect', network]).stdout)[0]
            require(metadata['Labels'].get('com.docker.compose.project') == name and not metadata['Containers'], 'NETWORK_CLEANUP_BOUNDARY')
            run(['docker', 'network', 'rm', network])
        require(root.parent == Path('/tmp') and root.name.startswith(name + '-') and not root.is_symlink(), 'FIXTURE_PATH_BOUNDARY')
        shutil.rmtree(root)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # Never emit raw subprocess output, config, secrets, paths or SQL data.
        marker = str(error)
        print(marker if re.fullmatch(r'[A-Z][A-Z0-9_]{1,95}', marker) else 'PG_BACKUP_FIXTURE_FAILED', flush=True)
        if isinstance(error, KeyError) and error.args[0] in ('repo', 'cipher', 'status', 'message', 'code'):
            print('PG_BACKUP_FIXTURE_MISSING_FIELD_' + error.args[0].upper(), flush=True)
        elif isinstance(error, json.JSONDecodeError):
            print('PG_BACKUP_FIXTURE_INVALID_JSON', flush=True)
        else:
            frames = error.__traceback__
            while frames and frames.tb_next:
                frames = frames.tb_next
            if frames:
                print('PG_BACKUP_FIXTURE_FAILURE_LINE_' + str(frames.tb_lineno), flush=True)
            if type(error).__name__ in ('KeyError', 'TypeError', 'ValueError', 'AttributeError', 'IndexError', 'PermissionError', 'FileNotFoundError'):
                print('PG_BACKUP_FIXTURE_EXCEPTION_' + type(error).__name__.upper(), flush=True)
        raise SystemExit(1)

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';
import { checkProductionPostgres } from '../../../scripts/account-platform-postgres-compose-check.mjs';

const root = path.resolve(import.meta.dirname, '../..');
const read = name => readFileSync(path.join(root, name), 'utf8');
const config = YAML.parse(read('deploy/production/compose.dependencies.yaml'));

function rejectsArgs(value) {
  const candidate = structuredClone(config);
  if (value === undefined) delete candidate.services.postgres.environment.POSTGRES_INITDB_ARGS;
  else candidate.services.postgres.environment.POSTGRES_INITDB_ARGS = value;
  assert.throws(() => checkProductionPostgres(candidate), /production_postgres_initdb_args_invalid/);
}

test('production PostgreSQL deployment checker accepts only explicit UTF8 with C locale', () => {
  assert.deepEqual(checkProductionPostgres(config), {
    decision: 'POSTGRES_CONFIG_VALID', encoding: 'UTF8', locale: 'C', environment_status: 'ENV_PENDING',
  });
  for (const value of [
    undefined,
    '--locale=C --auth-local=scram-sha-256 --auth-host=scram-sha-256',
    '--encoding=UTF8 --encoding=UTF8 --locale=C --auth-local=scram-sha-256 --auth-host=scram-sha-256',
    '--encoding=SQL_ASCII --locale=C --auth-local=scram-sha-256 --auth-host=scram-sha-256',
    '--encoding=UTF8 --auth-local=scram-sha-256 --auth-host=scram-sha-256',
    '--encoding=UTF8 --locale=C.UTF-8 --auth-local=scram-sha-256 --auth-host=scram-sha-256',
    '--encoding=UTF8 --locale=C --locale=C --auth-local=scram-sha-256 --auth-host=scram-sha-256',
  ]) rejectsArgs(value);
});

test('production PostgreSQL is standalone, nonroot, private and file-credential only', () => {
  assert.deepEqual(Object.keys(config.services), ['postgres']);
  const pg = config.services.postgres;
  assert.equal(pg.user, '999:999');
  assert.equal(pg.read_only, true);
  assert.deepEqual(pg.cap_drop, ['ALL']);
  assert.equal(pg.ports, undefined);
  assert.equal(pg.pull_policy, 'never');
  assert.equal(config.networks.data.internal, true);
  assert.equal(pg.environment.POSTGRES_PASSWORD, undefined);
  assert.equal(pg.environment.POSTGRES_PASSWORD_FILE, '/run/pinvou/postgres-admin/password');
  assert.equal(pg.environment.POSTGRES_INITDB_ARGS, '--encoding=UTF8 --locale=C --auth-local=scram-sha-256 --auth-host=scram-sha-256');
  for (const volume of pg.volumes) {
    assert.equal(volume.type, 'bind');
    assert.equal(volume.bind.create_host_path, false);
    assert.equal(volume.read_only ?? false, volume.target !== '/var/lib/postgresql/data');
    assert.ok(!/kms|custody|ca-private|identities.age/.test(volume.source));
  }
  assert.match(pg.healthcheck.test.join(' '), /postgres-health.sh/);
});

test('HBA allows only exact database and role combinations over TLS SCRAM', () => {
  const hba = read('deploy/production/pg_hba.conf').split(/\r?\n/).filter(line => line && !line.startsWith('#'));
  assert.equal(hba[0], 'local all account_platform_admin scram-sha-256');
  assert.equal(hba[1], 'local all all reject');
  assert.equal(hba[2], 'hostnossl all all all reject');
  assert.equal(hba.at(-1), 'host all all all reject');
  assert.equal(hba.filter(line => line.startsWith('hostssl ')).length, 5);
  for (const line of hba.slice(3, -1)) {
    const [, db, roles, address, method] = line.split(/\s+/);
    assert.match(db, /^account_platform_(account|billing|token|newapi|keycloak)$/);
    assert.ok(!roles.includes('admin') && !roles.includes('+') && roles !== 'all');
    assert.equal(address, 'all');
    assert.equal(method, 'scram-sha-256');
  }
  const conf = read('deploy/production/postgres.conf');
  assert.match(conf, /ssl = on/);
  assert.match(conf, /ssl_min_protocol_version = 'TLSv1.2'/);
  assert.match(conf, /log_min_error_statement = 'panic'/);
});

test('production bootstrap fails closed on incomplete data and keeps password argv empty', () => {
  const entry = read('deploy/production/postgres-entrypoint.sh');
  assert.match(entry, /PG_VERSION[\s\S]*db_ready/);
  assert.match(entry, /find[\s\S]*mindepth 1/);
  const common = read('deploy/production/postgres-common.sh');
  assert.match(common, /999:999:600/);
  assert.match(common, /Database system identifier/);
  assert.match(common, /pinvou-production-postgres-v2-utf8 \$DB_CONTROL_ID/);
  assert.match(common, /count\(\*\)=8[\s\S]*pg_encoding_to_char\(encoding\)='UTF8'[\s\S]*datcollate='C'[\s\S]*datctype='C'/);
  const init = read('deploy/production/postgres-init.sh');
  assert.match(init, /ENABLE_PAYMENT_SIMULATOR_DB=false/);
  const sharedInitIndex = init.indexOf('. /opt/pinvou/init-databases.sh');
  const encodingGateIndex = init.indexOf('db_utf8 "$POSTGRES_USER"');
  const completionMarkerIndex = init.indexOf('set -C; printf');
  assert.ok(sharedInitIndex >= 0);
  assert.ok(encodingGateIndex >= 0);
  assert.ok(completionMarkerIndex >= 0);
  assert.ok(sharedInitIndex < completionMarkerIndex);
  assert.ok(encodingGateIndex < completionMarkerIndex);
  assert.match(init, /sync "\$PGDATA\/\.pinvou-bootstrap-complete"/);
  assert.match(read('deploy/production/postgres-health.sh'), /db_ready[\s\S]*SELECT 1[\s\S]*db_utf8 account_platform_admin/);
  const shared = read('deploy/compose/init-databases.sh');
  assert.deepEqual([...shared.matchAll(/\\getenv\s+\w+\s+(\w+)/g)].map(match => match[1]).sort(), [
    'ACCOUNT_DB_PASSWORD', 'BILLING_DB_PASSWORD', 'TOKEN_DB_PASSWORD',
    'ACCOUNT_MIGRATION_DB_PASSWORD', 'BILLING_MIGRATION_DB_PASSWORD',
    'ACCOUNT_APPROVAL_WRITER_DB_PASSWORD', 'ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD',
    'TOKEN_MIGRATION_DB_PASSWORD', 'TOKEN_POLICY_DB_PASSWORD',
    'NEWAPI_DB_PASSWORD', 'KEYCLOAK_DB_PASSWORD', 'PAYMENT_SIM_DB_PASSWORD',
  ].sort());
  assert.doesNotMatch(shared, /--set=\w*password=/);
});

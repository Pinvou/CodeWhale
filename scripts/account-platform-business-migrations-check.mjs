#!/usr/bin/env node
// No execution or secret reads. Inventory authenticity is verified by the site executor.
import { readFileSync } from 'node:fs';
import { isDeepStrictEqual as equal } from 'node:util';
import { pathToFileURL } from 'node:url';
import path from 'node:path';
import { parseReleaseJSON } from './account-platform-model-token-release-plan.mjs';
const roles = { account: ['account_migrator', 'account_service'], billing: ['billing_migrator', 'billing_service'], token: ['token_migrator', 'token_gateway'] };
const keys = (a, b) => equal(Object.keys(a || {}).sort(), Object.keys(b || {}).sort());
function check(ok, code) { if (!ok) throw new Error(`business_migrations_${code}`); }
function directory(value) {
  check(typeof value === 'string' && value.startsWith('/') && value !== '/' && !value.endsWith('/')
    && path.posix.normalize(value) === value && !/[\\\x00-\x20\x7f]/.test(value), 'path_invalid');
  return value;
}
const overlap = (a, b) => a === b || a.startsWith(`${b}/`) || b.startsWith(`${a}/`);
export function checkBusinessMigrations(config, inventory, now = Date.now()) {
  check(config?.name === 'pinvou-production-business-migrations' && keys(config.services, { 'account-migrate': 1, 'billing-migrate': 1, 'token-migrate': 1 })
    && keys(config.networks, { data: 1 }) && !config.volumes && !config.secrets && !config.configs, 'inventory_invalid');
  check(/^sha256:[a-f0-9]{64}$/.test(inventory?.image_id), 'image_invalid');
  const backup = inventory?.backup;
  check(backup && /^\d{8}-\d{6}F_\d{8}-\d{6}I$/.test(backup.id) && /^[a-f0-9]{64}$/.test(backup.manifest_sha256), 'backup_invalid');
  const completed = Date.parse(backup.completed_at);
  check(Number.isFinite(completed) && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/.test(backup.completed_at)
    && completed >= now - 86400000 && completed <= now + 300000, 'backup_stale');
  const network = config.networks.data;
  check(network.external === true && network.name === inventory.network_name && /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/.test(network.name)
    && Object.keys(network).every(k => ['name', 'external', 'ipam'].includes(k))
    && (!Object.hasOwn(network, 'ipam') || equal(network.ipam, {})), 'network_invalid');
  const sources = [], authorities = [];
  const fixed = { pull_policy: 'never', profiles: ['business-migrations'], restart: 'no', entrypoint: ['/service'], user: '65532:65532', read_only: true,
    cap_drop: ['ALL'], security_opt: ['no-new-privileges:true'], pids_limit: 96, healthcheck: { disable: true },
    tmpfs: ['/tmp:rw,noexec,nosuid,nodev,size=16m,uid=65532,gid=65532,mode=0700'],
    logging: { driver: 'local', options: { 'max-size': '10m', 'max-file': '3' } } };
  for (const [domain, [login, role]] of Object.entries(roles)) {
    const service = config.services[`${domain}-migrate`];
    check(keys(service, { ...fixed, image: 1, command: 1, cpus: 1, mem_limit: 1, ulimits: 1, networks: 1, environment: 1, volumes: 1 }), 'service_scope');
    for (const [key, value] of Object.entries(fixed)) check(equal(service[key], value), 'runtime_boundary');
    check(service.image === inventory.image_id && [0.5, '0.5'].includes(service.cpus) && [268435456, '268435456'].includes(service.mem_limit), 'image_or_limits');
    check(keys(service.ulimits, { core: 1 }) && service.ulimits.core && !Array.isArray(service.ulimits.core)
      && typeof service.ulimits.core === 'object' && Object.entries(service.ulimits.core).every(([k, v]) => ['soft', 'hard'].includes(k) && v === 0), 'core_limit');
    check(equal(service.command, ['--dir', `/migrations/${domain}`, '--direction', 'up'])
      && keys(service.networks, { data: 1 }) && (service.networks.data == null || equal(service.networks.data, {})), 'execution_scope');
    check(equal(service.environment, {
      APP_ENV: 'production', PINVOU_DEPLOYMENT_ENVIRONMENT: 'production-new',
      DATABASE_URL: `postgresql://${login}@postgres:5432/account_platform_${domain}?sslmode=verify-full&sslrootcert=/var/run/secrets/pinvou/trust/ca.crt`,
      MIGRATION_ROLE: role, PGPASSFILE: '/var/run/secrets/pinvou/database/pgpass',
      MIGRATION_BACKUP_EVIDENCE_ID: backup.id, MIGRATION_BACKUP_COMPLETED_AT: backup.completed_at, MIGRATION_BACKUP_SHA256: backup.manifest_sha256,
    }), 'environment_invalid');
    check(Array.isArray(service.volumes) && service.volumes.length === 2, 'mount_count');
    for (const target of ['database', 'trust']) {
      const found = service.volumes.filter(v => v.target === `/var/run/secrets/pinvou/${target}`);
      check(found.length === 1, 'mount_missing');
      const volume = found[0];
      check(keys(volume, { type: 1, source: 1, target: 1, read_only: 1, bind: 1 }) && volume.type === 'bind' && volume.read_only === true
        && equal(volume.bind, { create_host_path: false }), 'mount_unsafe');
      const source = directory(volume.source);
      (target === 'database' ? sources : authorities).push(source);
    }
  }
  check(new Set(authorities).size === 1 && sources.every(s => !overlap(s, authorities[0]))
    && sources.every((s, i) => sources.slice(i + 1).every(other => !overlap(s, other))), 'shared_secret');
  return { decision: 'MIGRATION_CONFIG_VALID', environment_status: 'ENV_PENDING', migrations_executed: false };
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const args = process.argv.slice(2);
    check(args.length === 4 && args[0] === '--compose-render' && args[2] === '--inventory', 'usage');
    process.stdout.write(JSON.stringify(checkBusinessMigrations(parseReleaseJSON(readFileSync(args[1], 'utf8')), parseReleaseJSON(readFileSync(args[3], 'utf8')))) + '\n');
  } catch (error) {
    process.stderr.write((/^business_migrations_[a-z_]+$/.test(error?.message) ? error.message : 'business_migrations_input_rejected') + '\n');
    process.exitCode = 1;
  }
}

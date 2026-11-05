import { isMainModule } from './account-platform-cli-entry.mjs';
import { approvedRender, bytesDigest, gateCLI, mountedSource, physicalSourcePath, readSourceBytes, readSourceFile, requireGate as check, stableJSON } from './account-platform-production-render.mjs';

const roles = Object.freeze({ account: ['account_migrator', 'account_service'],
  billing: ['billing_migrator', 'billing_service'], token: ['token_migrator', 'token_gateway'] });
const same = (a, b) => stableJSON(a) === stableJSON(b);
export function verifiedDatabaseURL(text, expected) {
  check(typeof text === 'string' && text.length <= 8192 && !/[\s\u0000-\u001f]/.test(text), 'database_url');
  let url;
  try { url = new URL(text); } catch { check(false, 'database_url'); }
  check(url.protocol === 'postgresql:' && !url.hash && !url.host.includes(',')
    && url.hostname === expected.host && (url.port || '5432') === String(expected.port || 5432)
    && url.pathname === `/${expected.name}` && decodeURIComponent(url.username) === expected.login, 'database_identity');
  check([...url.searchParams.keys()].every(k => ['sslmode', 'sslrootcert', 'connect_timeout'].includes(k))
    && new Set(url.searchParams.keys()).size === [...url.searchParams.keys()].length
    && url.searchParams.get('sslmode') === 'verify-full', 'database_tls');
  const ca = url.searchParams.get('sslrootcert');
  check(ca === 'system' || (typeof ca === 'string' && ca.startsWith('/') && !ca.includes('..')), 'database_ca');
  if (url.searchParams.has('connect_timeout')) check(/^[1-9][0-9]?$/.test(url.searchParams.get('connect_timeout')), 'database_url');
  return { url, ca };
}

function migrationFor(workload, domain) {
  return workload.kind === 'compose' ? workload.id === `${domain}-migrate`
    : workload.resource.kind === 'Job' && workload.resource.spec?.template?.metadata?.labels?.['app.kubernetes.io/component'] === `migrate-${domain}`;
}
function strictMount(workload, path) {
  return mountedSource(workload, path);
}
export function pgpassRows(text) {
  check(typeof text === 'string' && Buffer.byteLength(text) <= 65536 && !text.includes('\0'), 'pgpass');
  return text.split(/\r?\n/).filter(line => line && !line.startsWith('#')).map(line => {
    const cells = []; let cell = '';
    for (let index = 0; index < line.length; index++) {
      const char = line[index];
      if (char === '\\') { check(index + 1 < line.length, 'pgpass'); cell += line[++index]; }
      else if (char === ':') { cells.push(cell); cell = ''; } else cell += char;
    }
    cells.push(cell);
    check(cells.length === 5 && cells.every(v => v && !/[\u0000-\u001f]/.test(v))
      && cells.slice(0, 4).every(v => v !== '*'), 'pgpass');
    return cells;
  });
}
export function checkRuntimeDatabases(all, inventory, render) {
  const identities = {
    'account-service': ['account', 'account_service'], 'account-event-relay': ['account', 'account_service'],
    'billing-service': ['billing', 'billing_service'], 'billing-event-relay': ['billing', 'billing_service'],
    'token-gateway': ['token', 'token_gateway'], 'token-credential-portal': ['token', 'token_gateway'],
  };
  for (const workload of all) {
    const name = workload.kind === 'compose' ? workload.id : workload.service.name;
    const identity = identities[name];
    if (!identity) continue;
    // Only the current personal Compose Token facade is a non-SQL process.
    // A missing DSN must never make an actual SQL consumer disappear.
    if (name === 'token-gateway' && render?.kind === 'compose' && render.recipe.profile === 'personal') continue;
    check(typeof workload.environment.DATABASE_URL === 'string'
      && workload.environment.DATABASE_URL.trim().length > 0, 'runtime_database_missing');
    const [domain, login] = identity, db = inventory.databases?.[domain];
    check(db, 'runtime_database_inventory');
    const { url, ca } = verifiedDatabaseURL(workload.environment.DATABASE_URL, { ...db, login });
    if (workload.kind === 'helm') {
      check(ca === 'system' && url.password && workload.refs?.DATABASE_URL, 'runtime_database_secret');
    } else {
      if (ca !== 'system') readSourceFile(mountedSource(workload, ca));
      const pgpass = workload.environment.PGPASSFILE;
      if (pgpass) {
        const rows = pgpassRows(readSourceFile(mountedSource(workload, pgpass)));
        check(rows.length === 1 && same(rows[0].slice(0, 4), [db.host, String(db.port || 5432), db.name, login]), 'runtime_pgpass_identity');
      } else check(url.password, 'runtime_database_secret');
    }
  }
}
export function checkBusinessMigrations(render, inventory, now = Date.now()) {
  const all = approvedRender(render, inventory);
  const domains = render.kind === 'compose' && render.recipe.profile === 'personal' ? ['account', 'billing'] : Object.keys(roles);
  check(same(Object.keys(inventory.databases || {}).sort(), domains.slice().sort()), 'database_inventory');
  const backup = inventory.backup;
  check(backup && typeof backup.id === 'string' && /^[^\s\u0000-\u001f]{8,128}$/.test(backup.id)
    && /^[a-f0-9]{64}$/.test(backup.sha256 || '') && typeof backup.manifestFile === 'string'
    && bytesDigest(readSourceBytes(backup.manifestFile)) === backup.sha256, 'backup_manifest');
  const completed = Date.parse(backup.completedAt);
  check(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:Z|[+-]\d\d:\d\d)$/.test(backup.completedAt || '')
    && Number.isFinite(completed) && completed >= now - 86400000 && completed <= now + 300000, 'backup_stale');
  const selected = [], databases = new Set(), credentials = [];
  for (const domain of domains) {
    const found = all.filter(w => migrationFor(w, domain));
    check(found.length === 1, 'migration_inventory');
    const workload = found[0], env = workload.environment, [login, role] = roles[domain];
    selected.push(workload.id);
    const command = workload.kind === 'compose' ? workload.service.command : workload.service.args;
    check(same(command, ['--dir', `/migrations/${domain}`]) && !workload.service.entrypoint
      && !workload.service.command?.includes('/bin/sh'), 'migration_command');
    check(env.PINVOU_DEPLOYMENT_ENVIRONMENT === 'production-new' && env.MIGRATION_ROLE === role, 'migration_role');
    check(env.MIGRATION_BACKUP_EVIDENCE_ID === backup.id && env.MIGRATION_BACKUP_SHA256 === backup.sha256
      && env.MIGRATION_BACKUP_COMPLETED_AT === backup.completedAt, 'backup_binding');
    const db = inventory.databases[domain];
    check(db && /^[a-zA-Z0-9_.-]+$/.test(db.host || '') && /^[a-zA-Z0-9_]+$/.test(db.name || ''), 'database_inventory');
    const { url, ca } = verifiedDatabaseURL(env.DATABASE_URL, { ...db, login });
    const identity = `${url.host}${url.pathname}`;
    check(!databases.has(identity), 'database_alias'); databases.add(identity);
    if (workload.kind === 'compose') {
      check(workload.service.restart === 'no' && workload.service.healthcheck?.disable === true
        && !workload.service.profiles?.includes('business-migrations'), 'migration_startup');
      if (ca !== 'system') readSourceFile(strictMount(workload, ca));
      if (!url.password) {
        check(typeof env.PGPASSFILE === 'string', 'pgpass_missing');
        const source = strictMount(workload, env.PGPASSFILE);
        const rows = pgpassRows(readSourceFile(source));
        check(rows.length === 1 && same(rows[0].slice(0, 4), [db.host, String(db.port || 5432), db.name, login]), 'pgpass_identity');
        credentials.push({ source, login, workload: workload.id });
      }
      for (const consumer of all.filter(w => w.id === `${domain}-service` || (domain === 'token' && w.id === 'token-gateway')
        || (domain === 'account' && w.id === 'account-event-relay'))) {
        check(consumer.service.depends_on?.[`${domain}-migrate`]?.condition === 'service_completed_successfully', 'migration_dependency');
      }
    } else {
      const annotations = workload.resource.metadata?.annotations;
      check(annotations?.['helm.sh/hook'] === 'pre-install,pre-upgrade'
        && annotations['helm.sh/hook-weight'] === { account: '-30', billing: '-20', token: '-10' }[domain]
        && annotations['helm.sh/hook-delete-policy'] === 'before-hook-creation,hook-succeeded'
        && workload.pod.restartPolicy === 'Never', 'migration_hook');
      // The current scratch migrate image contains system CA roots. A specific
      // Secret key holding a verified-TLS DSN is valid without a pgpass volume.
      check(ca === 'system' && url.password && workload.refs?.DATABASE_URL, 'helm_database_secret');
      const ref = workload.refs.DATABASE_URL;
      check(ref.key === `${domain}-migration-database-url`, 'helm_database_secret');
      check(['MIGRATION_BACKUP_EVIDENCE_ID', 'MIGRATION_BACKUP_COMPLETED_AT', 'MIGRATION_BACKUP_SHA256']
        .every(key => workload.refs?.[key]), 'backup_binding');
      const limits = workload.service.resources?.limits;
      const cpu = typeof limits?.cpu === 'string' && limits.cpu.endsWith('m') ? Number(limits.cpu.slice(0, -1)) / 1000 : Number(limits?.cpu);
      const memory = /^\d+Mi$/.test(limits?.memory || '') ? Number(limits.memory.slice(0, -2)) * 1024 * 1024 : Number(limits?.memory);
      check(cpu > 0 && cpu <= 2 && memory > 0 && memory <= 1024 * 1024 * 1024, 'migration_resources');
    }
  }
  // Actual secret contents, not directory spelling, decide credential exposure.
  // A runtime mount containing its migrator's pgpass is always rejected.
  for (const { source, workload } of credentials) for (const other of all) {
    if (other.id === workload) continue;
    const file = physicalSourcePath(source);
    check(!other.mounts.some(v => v.type === 'bind' && (file === physicalSourcePath(v.source)
      || file.startsWith(`${physicalSourcePath(v.source)}/`))), 'migration_credential_exposure');
  }
  const unexpected = all.filter(w => (w.kind === 'compose' ? /-migrate$/.test(w.id)
    : w.resource.kind === 'Job' && /^migrate-/.test(w.resource.spec?.template?.metadata?.labels?.['app.kubernetes.io/component'] || ''))
    && !selected.includes(w.id));
  check(unexpected.length === 0, 'migration_inventory');
  return { decision: 'MIGRATION_CONFIG_VALID', environment_status: 'ENV_PENDING', migrations_executed: false,
    deployment_status: 'NOT_DEPLOYED', domains_checked: domains.length };
}
if (isMainModule(import.meta.url)) gateCLI(checkBusinessMigrations);

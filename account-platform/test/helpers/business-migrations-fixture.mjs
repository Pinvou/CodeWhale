import { readFileSync } from 'node:fs';
import YAML from 'yaml';

// Public configuration only; no pgpass files, database or runtime containers.
export function businessMigrationsFixture() {
  const variables = {
    MIGRATE_IMAGE: 'sha256:' + '1'.repeat(64),
    MIGRATION_BACKUP_EVIDENCE_ID: '20260909-010000F_20260909-020000I',
    MIGRATION_BACKUP_COMPLETED_AT: '2026-09-09T02:00:00Z',
    MIGRATION_BACKUP_SHA256: '2'.repeat(64),
    DATA_NETWORK: 'fixture-private-data', INFRA_CA_DIRECTORY: '/fixture/migrations/ca',
    ACCOUNT_MIGRATE_DATABASE_DIRECTORY: '/fixture/migrations/account',
    BILLING_MIGRATE_DATABASE_DIRECTORY: '/fixture/migrations/billing',
    TOKEN_MIGRATE_DATABASE_DIRECTORY: '/fixture/migrations/token',
  };
  const source = readFileSync(new URL('../../deploy/production/compose.business-migrations.yaml', import.meta.url), 'utf8');
  const replace = value => typeof value === 'string' ? value.replace(/\$\{([A-Z0-9_]+):\?[^}]+\}/g, (_, key) => variables[key])
    : Array.isArray(value) ? value.map(replace) : value && typeof value === 'object' ? Object.fromEntries(Object.entries(value).map(([key, item]) => [key, replace(item)])) : value;
  const input = replace(YAML.parse(source, { merge: true }));
  for (const key of Object.keys(input)) if (key.startsWith('x-')) delete input[key];
  const config = structuredClone(input);
  for (const service of Object.values(config.services)) {
    service.mem_limit = '268435456'; service.ulimits = { core: {} };
    service.networks = { data: {} };
  }
  config.networks.data.ipam = {};
  return { source, variables, input, config, now: Date.parse('2026-09-09T02:01:00Z'), inventory: {
    image_id: variables.MIGRATE_IMAGE, network_name: variables.DATA_NETWORK,
    backup: { id: variables.MIGRATION_BACKUP_EVIDENCE_ID, completed_at: variables.MIGRATION_BACKUP_COMPLETED_AT, manifest_sha256: variables.MIGRATION_BACKUP_SHA256 },
  } };
}

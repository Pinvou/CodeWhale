import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

const commands = [
  ['BILLING_PRICE_DRAFT_CREATE', 'admin.POST("/pricing/drafts"'],
  ['BILLING_PRICE_DRAFT_UPDATE', 'admin.PUT("/pricing/:price_version_id/draft"'],
  ['BILLING_PRICE_PUBLISH', 'admin.POST("/pricing/:price_version_id/publish"'],
  ['BILLING_PRICE_TRANSITION', 'admin.POST("/pricing/:price_version_id/transition"'],
  ['BILLING_METERING_UNIT_CREATE', 'admin.POST("/metering-units"'],
  ['BILLING_METERING_UNIT_DEACTIVATE', 'admin.POST("/metering-units/:unit_definition_id/deactivate"'],
];

test('pricing and metering writes have exact audit-only command policies', async () => {
  const [registry, service, migrationUp, migrationDown] = await Promise.all([
    read('internal/platform/approvalpolicy/registry.go'),
    read('internal/billing/service.go'),
    read('migrations/account/000041_pricing_metering_command_policies.up.sql'),
    read('migrations/account/000041_pricing_metering_command_policies.down.sql'),
  ]);

  for (const [command, route] of commands) {
    assert.match(registry, new RegExp(`"${command}"\\s*:\\s*"platform\\.pricing\\.manage"`));
    const line = service.split(/\r?\n/).find(candidate => candidate.includes(route));
    assert.ok(line, `missing route ${route}`);
    assert.match(line, new RegExp(`authorizer\\.Require\\("platform\\.pricing\\.manage"\\).*approvalpolicy\\.Require\\("${command}", approvalpolicy\\.AuditOnly\\)`));
    assert.match(migrationUp, new RegExp(`'${command}'\\s*,\\s*1\\s*,\\s*'AUDIT_ONLY'\\s*,\\s*'platform\\.pricing\\.manage'`));
    assert.match(migrationDown, new RegExp(`'${command}'`));
  }

  assert.match(migrationUp, /'GOVERNANCE_OR_NORMAL_STATE_MACHINE'\s*,\s*0\s*,\s*false\s*,\s*1/);
  assert.match(migrationUp, /'\["target_id","request_fingerprint"\]'\s*,\s*INTERVAL '24 hours'\s*,\s*INTERVAL '5 minutes'\s*,\s*'requesting-domain-service'/);
  assert.doesNotMatch(migrationUp, /UPDATE approval_command_policies/);
  assert.match(migrationDown, /DROP TRIGGER approval_command_policies_immutable/);
  assert.match(migrationDown, /CREATE TRIGGER approval_command_policies_immutable/);
  assert.doesNotMatch(migrationDown, /BILLING_PRICE_VERSION_CREATE/);
  assert.doesNotMatch(registry, /BILLING_PRICE_VERSION_CREATE/);
});

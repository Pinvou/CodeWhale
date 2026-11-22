import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';

const source = path => readFileSync(new URL(`../../${path}`, import.meta.url), 'utf8');
const digest = path => createHash('sha256').update(source(path).replace(/\r\n?/g, '\n')).digest('hex');

test('waiver entry points retain exact human policy and distinct domain workload scopes', () => {
  const billing = source('internal/billing/service.go');
  const token = source('internal/token/management_routes.go');
  assert.match(billing, /POST\("\/usage-holds\/:hold_id\/release", s\.authorizer\.Require\("platform\.billing\.holds\.release"\), approvalpolicy\.Require\("BILLING_USAGE_HOLD_SAFE_RELEASE", approvalpolicy\.AuditOnly\)/);
  assert.match(billing, /POST\("\/internal\/v1\/holds\/waiver", serviceauth\.Require\("internal\.billing\.hold\.waive"\)/);
  assert.match(token, /POST\("\/usage-waivers", serviceauth\.Require\("internal\.token\.usage\.waive"\)/);
  const tokenRules = JSON.parse(source('deploy/production/workload-token-rules.json.template'));
  const billingRules = JSON.parse(source('deploy/production/workload-billing-rules.json.template'));
  assert.deepEqual(tokenRules.filter(row => row.permissions.includes('internal.token.usage.waive')).map(row => row.service), ['billing-service']);
  assert.deepEqual(billingRules.filter(row => row.permissions.includes('internal.billing.hold.waive')).map(row => row.service), ['token-gateway']);
  const contract = JSON.parse(source('contracts/openapi/usage-waivers.json'));
  assert.equal(contract.routes.length, 3);
  assert.equal(contract.routes[0].mode, 'AUDIT_ONLY');
});

test('forward waiver identities are pinned while original026 remains unchanged', () => {
  const billing = source('internal/billingschema/usage_waivers.go');
  assert.ok(billing.includes(digest('migrations/billing/900024_usage_waiver_commands.up.sql')));
  assert.ok(billing.includes(digest('migrations/billing/900024_usage_waiver_commands.down.sql')));
  const token = source('internal/tokenschema/stage.go');
  assert.match(token, /900005:\s*\{name: "usage_waiver_intents"/);
  assert.ok(token.includes(digest('migrations/token/900005_usage_waiver_intents.up.sql')));
  assert.ok(token.includes(digest('migrations/token/900005_usage_waiver_intents.down.sql')));
  const history = source('internal/billingschema/usage_waiver_history.go');
  assert.ok(history.includes(digest('migrations/billing/000026_hold_operations_evidence.up.sql')));
  assert.ok(history.includes(digest('internal/billingschema/retained/000026_hold_operations_evidence_oldfix.up.sql')));
});

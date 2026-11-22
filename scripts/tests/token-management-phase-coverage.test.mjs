import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { checkCoverage, loadInputs, commandSpecs } from '../check-token-management-authorization-coverage.mjs';

// These are boundary tests of the actual command parser. Host, SQL and OpenAPI
// verification still uses the assembled repository, not these synthetic texts.
const commands = [
  'TOKEN_PROTOCOL_DRAFT_CREATE', 'TOKEN_PROTOCOL_PUBLISH',
  'TOKEN_FORMULA_DRAFT_CREATE', 'TOKEN_FORMULA_PUBLISH',
  'TOKEN_MODEL_DRAFT_SAVE', 'TOKEN_MODEL_PUBLISH', 'TOKEN_MODEL_LIFECYCLE_CHANGE',
  'TOKEN_MODEL_CATALOG_SYNC', 'TOKEN_MODEL_CATALOG_ROLLBACK',
  'TOKEN_ACCOUNT_CREATE', 'TOKEN_ACCOUNT_STATUS_CHANGE',
  'TOKEN_ROUTE_UPSERT', 'TOKEN_ROUTE_DELETE',
  'TOKEN_CREDENTIAL_ENROLL', 'TOKEN_CREDENTIAL_ROTATE', 'TOKEN_CREDENTIAL_REVOKE',
  'TOKEN_CREDENTIAL_VALIDATE', 'TOKEN_ROUTE_PROVISIONING_REPAIR',
  'TOKEN_USAGE_RECONCILIATION_RETRY',
];
function sources(names) {
  return {
    specs: `var tokenCommandSpecs = map[string]tokenCommandSpec{\n${names.map(name => `  "${name}": {[]string{"platform.models.manage"}, true, false, nil},`).join('\n')}\n}`,
    handlers: names.map(name => `"${name}": {"CONTROLLED", "COMMAND", (*Service).controlled}`).join('\n'),
  };
}

test('the current phase keeps all predecessor commands and adds usage retry', () => {
  const { specs, handlers } = sources(commands);
  assert.deepEqual(commandSpecs(specs, handlers).map(item => item.command).sort(), [...commands].sort());
});

test('missing retry cannot masquerade as a complete personal execution phase', () => {
  const { specs, handlers } = sources(commands.filter(name => name !== 'TOKEN_USAGE_RECONCILIATION_RETRY'));
  assert.throws(() => commandSpecs(specs, handlers), /complete management and usage retry/);
});

test('a missing predecessor command is rejected even with nineteen entries', () => {
  const { specs, handlers } = sources(commands.map(name => name === 'TOKEN_MODEL_CATALOG_ROLLBACK' ? 'TOKEN_FUTURE_COMMAND' : name));
  assert.throws(() => commandSpecs(specs, handlers), /complete management and usage retry/);
});

test('duplicate commands cannot conceal a missing management capability', () => {
  const { specs, handlers } = sources(commands.map(name => name === 'TOKEN_ROUTE_DELETE' ? 'TOKEN_ROUTE_UPSERT' : name));
  assert.throws(() => commandSpecs(specs, handlers), /complete management and usage retry/);
});

test('retry requires an actual registered handler, not just a policy declaration', () => {
  const { specs } = sources(commands);
  const { handlers } = sources(commands.filter(name => name !== 'TOKEN_USAGE_RECONCILIATION_RETRY'));
  assert.throws(() => commandSpecs(specs, handlers), /missing actual handler/);
});

test('the real assembled Go dispatch table contains exactly the current phase', () => {
  const specs = fs.readFileSync(new URL('../../account-platform/internal/token/command_governance.go', import.meta.url), 'utf8');
  const handlers = fs.readFileSync(new URL('../../account-platform/internal/token/governance_handlers.go', import.meta.url), 'utf8');
  assert.deepEqual(commandSpecs(specs, handlers).map(item => item.command).sort(), [...commands].sort());
});


test('installed organization lifecycle contracts keep exact workload authority and real DTOs', () => {
  const inputs = loadInputs();
  checkCoverage(inputs);
  for (const [suffix, permission, command, response] of [
    ['', 'internal.token.organization.lifecycle', 'TOKEN_ORGANIZATION_LIFECYCLE_APPLY', 'OrganizationLifecycleAssessment'],
    ['/units/{unit_id}', 'internal.token.organization-unit.lifecycle', 'TOKEN_ORGANIZATION_UNIT_LIFECYCLE_APPLY', 'OrganizationUnitDeletionAssessment'],
  ]) {
    const op = inputs.management.paths[`/internal/v1/organizations/{organization_id}${suffix}/lifecycle`].put;
    assert.deepEqual(op['x-authority'], { kind: 'workload', all: [permission] });
    assert.deepEqual(op['x-command-policy'], { command, mode: 'AuditOnly' });
    assert.equal(op.responses['200'].content['application/json'].schema.$ref, `#/components/schemas/${response}`);
    assert.ok(inputs.management.components.schemas[response].properties.ready);
    assert.ok(inputs.management.components.schemas[response].properties.blockers);
  }
});

test('lifecycle contracts cannot hide a missing serialized response field', () => {
  const inputs = loadInputs();
  delete inputs.management.components.schemas.OrganizationLifecycleAssessment.properties.account_version;
  assert.throws(() => checkCoverage(inputs), /selected DTO field coverage drift/);
});

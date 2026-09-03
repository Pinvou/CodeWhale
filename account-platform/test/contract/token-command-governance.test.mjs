import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import YAML from 'yaml';

const read = (path) => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

const commands = [
  'TOKEN_PROTOCOL_DRAFT_CREATE', 'TOKEN_PROTOCOL_PUBLISH', 'TOKEN_FORMULA_DRAFT_CREATE', 'TOKEN_FORMULA_PUBLISH',
  'TOKEN_MODEL_DRAFT_SAVE', 'TOKEN_MODEL_PUBLISH', 'TOKEN_MODEL_LIFECYCLE_CHANGE', 'TOKEN_MODEL_CATALOG_SYNC', 'TOKEN_MODEL_CATALOG_ROLLBACK',
  'TOKEN_ACCOUNT_CREATE', 'TOKEN_ACCOUNT_STATUS_CHANGE', 'TOKEN_ROUTE_UPSERT', 'TOKEN_ROUTE_DELETE', 'TOKEN_CREDENTIAL_ENROLL',
  'TOKEN_CREDENTIAL_ROTATE', 'TOKEN_CREDENTIAL_REVOKE', 'TOKEN_CREDENTIAL_VALIDATE', 'TOKEN_ROUTE_PROVISIONING_REPAIR', 'TOKEN_USAGE_RECONCILIATION_RETRY',
];

test('APT-123 registers nineteen policies without creating or activating a governance record', async () => {
  const [accountUp, tokenUp, registry, activation] = await Promise.all([
    read('migrations/account/000045_token_command_governance.up.sql'),
    read('migrations/token/000024_token_command_results.up.sql'),
    read('internal/platform/approvalpolicy/registry.go'),
    read('internal/token/command_governance_activation.go'),
  ]);
  for (const command of commands) {
    assert.match(`${accountUp}\n${registry}`, new RegExp(`['\"]${command}['\"]`), command);
  }
  assert.match(accountUp, /platform\.models\.lifecycle\.request/);
  assert.match(accountUp, /platform\.models\.catalog\.rollback\.approve/);
  assert.match(accountUp, /platform\.tokens\.credentials\.revoke\.approve/);
  assert.match(tokenUp, /UNIQUE\(actor_user_id,command_type,idempotency_key_digest\)/);
  assert.match(tokenUp, /token_command_results_approval_idx/);
  assert.doesNotMatch(`${accountUp}\n${tokenUp}`, /CREATE TABLE\s+.*activation|INSERT INTO\s+.*activation|SAFE_ROLLBACK/i);
  assert.match(activation, /commandGovernanceActivation == nil/);
  assert.match(activation, /DIRECT_APPROVAL_COMMAND_FORBIDDEN/);
});

test('APT-123 OpenAPI closes command envelopes and keeps executor routes internal', async () => {
  const [admin, token, helm, accountHTTP, billingGate, governance] = await Promise.all([
    read('contracts/openapi/admin.yaml').then(YAML.parse),
    read('contracts/openapi/token-gateway.yaml').then(YAML.parse),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
    read('internal/account/approval_execution_http.go'),
    read('internal/billing/approval_execution.go'),
    read('internal/token/command_governance.go'),
  ]);
  assert.equal(admin.components.schemas.TokenCommandEnvelope.additionalProperties, false);
  assert.deepEqual(admin.components.schemas.TokenCommandEnvelope.properties.command_type.enum, commands);
  for (const path of ['/internal/v1/commands/model-lifecycle/execute', '/internal/v1/commands/catalog-rollback/execute', '/internal/v1/commands/credential-revoke/execute']) {
    assert.deepEqual(token.paths[path].post.security, [{ serviceToken: [] }]);
    assert.match(token.paths[path].post.description || token.paths['/internal/v1/commands/model-lifecycle/execute'].post.description, /APT-138|Outcome unknown/);
  }
  assert.match(helm, /token-command-executor/);
  assert.match(helm, /internal\.approval-execution\.begin/);
  assert.match(helm, /internal\.token\.credential-revoke\.execute/);
  assert.doesNotMatch(helm, /"service" "token-command-executor" "permissions" \(list[^\n]*"platform\./);
  assert.ok(token.components.schemas.ApprovedTokenCommandExecution.required.includes('execution_idempotency_key'));
  assert.match(governance, /ExecuteApprovedTokenCommand\(context\.Context, string, string, TokenCommandEnvelope\)/);
  assert.match(accountHTTP, /serviceauth\.FromContext/);
  assert.doesNotMatch(accountHTTP, /ExecutorIdentity\s+string\s+`json:"executor_identity"`/);
  assert.doesNotMatch(billingGate, /"executor_identity"\s*:/);
});

#!/usr/bin/env node
// APT-137 planning validation only: never mutates deployment or attests environment readiness.
import { readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';

const require = createRequire(new URL('../account-platform/package.json', import.meta.url));
const Ajv = require('ajv');
const addFormats = require('ajv-formats');
const ajv = new Ajv({ strict: true, allErrors: false, coerceTypes: false, useDefaults: false });
addFormats(ajv);
const schemas = new URL('../account-platform/contracts/deployment/', import.meta.url);
const planSchema = JSON.parse(await readFile(new URL('model-token-release-plan.schema.json', schemas), 'utf8'));
const evidenceSchema = JSON.parse(await readFile(new URL('model-token-release-evidence.schema.json', schemas), 'utf8'));
const validPlan = ajv.compile(planSchema);
const validEvidence = ajv.compile(evidenceSchema);
export const modelTokenGateNames = Object.freeze(Object.keys(planSchema.properties.gates.properties));

function reject(code) { throw new Error(`model_token_release_${code}`); }

// JSON.parse alone accepts duplicate keys. Tokenize only after grammar validation;
// decoded keys (including escaped spellings) must be unique in every object.
export function parseReleaseJSON(text) {
  if (typeof text !== 'string' || Buffer.byteLength(text) > 1024 * 1024) reject('json_invalid');
  let value;
  try { value = JSON.parse(text); } catch { reject('json_invalid'); }
  const tokens = text.match(/"(?:\\[\s\S]|[^"\\])*"|[{}\[\]:,]|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|null/g) || [];
  const stack = [];
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (token === '{') stack.push(new Set());
    else if (token === '[') stack.push(null);
    else if (token === '}' || token === ']') stack.pop();
    else if (token.startsWith('"') && tokens[index + 1] === ':') {
      const key = JSON.parse(token);
      const keys = stack.at(-1);
      if (keys.has(key)) reject('json_duplicate_key');
      keys.add(key);
    }
  }
  return value;
}

export function closedModelTokenGates() {
  return Object.fromEntries(modelTokenGateNames.map(name => [name, false]));
}

// The CLI projects SQL NULL digests as empty strings in its initial PRE snapshot.
// This is the single transport normalization; missing/failed reads are never PRE.
export function activationFromGovernanceSnapshot(snapshot) {
  if (!snapshot || !['PRE_ACTIVATION', 'ACTIVE', 'SAFE_ROLLBACK'].includes(snapshot.state)
    || !Number.isSafeInteger(snapshot.epoch) || snapshot.epoch < 0) reject('activation_snapshot_invalid');
  const initial = snapshot.state === 'PRE_ACTIVATION';
  if (initial && snapshot.epoch !== 0) reject('activation_snapshot_invalid');
  const normalize = value => initial && (value === '' || value === null) ? null : value;
  const result = { state: snapshot.state, epoch: snapshot.epoch,
    installed_policy_set_digest: normalize(snapshot.policy_set_digest),
    installed_handler_set_digest: normalize(snapshot.handler_set_digest) };
  if (initial ? result.installed_policy_set_digest !== null || result.installed_handler_set_digest !== null
    : snapshot.epoch < 1 || typeof result.installed_policy_set_digest !== 'string'
      || typeof result.installed_handler_set_digest !== 'string'
      || !/^[a-f0-9]{64}$/.test(result.installed_policy_set_digest)
      || !/^[a-f0-9]{64}$/.test(result.installed_handler_set_digest)) reject('activation_snapshot_invalid');
  return result;
}

export function validateModelTokenReleasePlan(plan) {
  if (!validPlan(plan)) reject('plan_schema_invalid');
  const allowedEnvironments = plan.environment === 'PRODUCTION' ? ['production', 'production-new'] : ['staging'];
  if (!allowedEnvironments.includes(plan.app_env)) reject('environment_mismatch');
  const activation = plan.activation;
  if (activation.state === 'NOT_INSTALLED') {
    if (activation.epoch !== null || activation.installed_policy_set_digest !== null || activation.installed_handler_set_digest !== null) reject('activation_invalid');
  } else if (activation.state === 'PRE_ACTIVATION') {
    if (activation.epoch !== 0 || activation.installed_policy_set_digest !== null || activation.installed_handler_set_digest !== null) reject('activation_invalid');
  } else {
    if (!Number.isSafeInteger(activation.epoch) || activation.epoch < 1
      || activation.installed_policy_set_digest === null || activation.installed_handler_set_digest === null) reject('activation_invalid');
    if (activation.installed_policy_set_digest !== plan.candidate.policy_set_digest
      || activation.installed_handler_set_digest !== plan.candidate.handler_set_digest) reject('digest_transition_unsupported');
  }
  if (plan.gates.PORTAL_ADMIN_MODEL_TOKEN_ACTIONS_ENABLED && activation.state !== 'ACTIVE') reject('write_requires_active');
  const origin = plan.environment === 'PRODUCTION' ? 'https://credentials.pinvou.com' : 'https://credentials-uat.pinvou.com';
  if (plan.credential_origin !== null && plan.credential_origin !== origin) reject('credential_origin_invalid');
  if (plan.gates.TOKEN_CREDENTIAL_ISOLATION_ENABLED && plan.credential_origin !== origin) reject('credential_origin_required');
  const roles = plan.runtime_roles;
  const writers = [roles.gateway, roles.credential_portal];
  const protectedRoles = ['token_gateway', 'token_schema_owner', 'token_migrator', 'token_cap_policy_deployer'];
  if ([...writers, ...roles.retired_roles, plan.operations.token_migrator, plan.operations.account_policy_role]
    .some(role => !/^[a-z_][a-z0-9_]{0,62}$/.test(role))) reject('role_identifier_invalid');
  if (new Set(writers).size !== 2 || writers.some(role => roles.retired_roles.includes(role)
    || protectedRoles.includes(role) || role === plan.operations.token_migrator || role === plan.operations.account_policy_role)) reject('runtime_role_invalid');
  if ((plan.legacy_links.ga_at === null) !== (plan.legacy_links.ga_version === null)) reject('legacy_ga_invalid');
  const expectedStages = ['INTERNAL', 'LIMITED', 'FULL'];
  for (const [index, stage] of plan.stages.entries()) {
    if (stage.stage !== expectedStages[index] || !Number.isSafeInteger(stage.observation_seconds)
      || !Number.isSafeInteger(stage.minimum_samples)
      || (index > 0 && stage.traffic_percent <= plan.stages[index - 1].traffic_percent)
      || (index === 2 && stage.traffic_percent !== 100)) reject('stage_invalid');
    for (const [metric, threshold] of Object.entries(stage.thresholds)) {
      if (!Number.isFinite(threshold.advance_max) || !Number.isFinite(threshold.pause_above)
        || threshold.advance_max > threshold.pause_above) reject('threshold_invalid');
      if (['error_rate', 'capacity_utilization'].includes(metric) && threshold.pause_above > 1) reject('threshold_invalid');
      if (metric === 'unauthorized_effects' && threshold.pause_above !== 0) reject('threshold_invalid');
      if (metric === 'capacity_utilization' && (threshold.advance_max > 0.70 || threshold.pause_above > 0.85)) reject('capacity_threshold_invalid');
    }
  }
  // This validates a proposed configuration, not the truth of role/state/evidence declarations.
  return Object.freeze({ decision: 'PLAN_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' });
}

export function validateModelTokenEvidenceEnvelope(evidence, plan, now = Date.now()) {
  validateModelTokenReleasePlan(plan);
  if (!validEvidence(evidence)) reject('evidence_schema_invalid');
  if (!Number.isFinite(now) || evidence.environment !== plan.environment
    || Object.keys(plan.candidate).some(key => evidence.candidate[key] !== plan.candidate[key])) reject('evidence_binding_invalid');
  const start = Date.parse(evidence.started_at); const end = Date.parse(evidence.ended_at);
  if (!Number.isFinite(start) || !Number.isFinite(end) || start >= end || end > now) reject('evidence_window_invalid');
  if (evidence.executor_reference === evidence.reviewer_reference) reject('independent_review_required');
  // Report bytes, execution provenance, freshness and complete coverage are checked by the
  // later release evidence gate. A syntactically valid PASS envelope is never a release GO.
  return Object.freeze({ decision: 'ENVELOPE_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' });
}

async function main(args) {
  if (args.length !== 2 || args[0] !== '--plan') reject('usage');
  let raw;
  try { raw = await readFile(args[1], 'utf8'); } catch { reject('plan_unreadable'); }
  const result = validateModelTokenReleasePlan(parseReleaseJSON(raw));
  process.stdout.write(`${JSON.stringify(result)}\n`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch(error => {
    const code = /^model_token_release_[a-z_]+$/.test(error?.message) ? error.message : 'model_token_release_invalid';
    process.stderr.write(`${code}\n`);
    process.exitCode = 1;
  });
}

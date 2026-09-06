#!/usr/bin/env node
// Explicit one-operation adapter for existing deployment CLIs. No chaining/retry.
import { readFile, readdir } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { spawn } from 'node:child_process';
import { isAbsolute, basename, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { parseReleaseJSON } from './account-platform-model-token-release-plan.mjs';

const hash = value => createHash('sha256').update(value).digest('hex');
const fail = code => { throw new Error(`MODEL_TOKEN_OPERATION_${code}`); };
const check = (value, code) => { if (!value) fail(code); };
const reference = value => typeof value === 'string' && /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(value);
const digest = value => typeof value === 'string' && /^[a-f0-9]{64}$/.test(value);
const uuid = value => typeof value === 'string' && /^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/.test(value) && value !== '00000000-0000-0000-0000-000000000000';
function fields(object, names) {
  check(object && typeof object === 'object' && !Array.isArray(object)
    && Object.keys(object).length === names.length && names.every(key => Object.hasOwn(object, key)), 'FIELDS_INVALID');
}

export function validateOperation(request) {
  fields(request, ['schema_version', 'operation', 'approval_reference', 'executable', 'executable_sha256', 'expected_database', 'expected_account_database', 'options']);
  check(request.schema_version === 1 && reference(request.approval_reference)
    && typeof request.executable === 'string' && isAbsolute(request.executable) && digest(request.executable_sha256)
    && typeof request.expected_database === 'string' && /^[a-z][a-z0-9_]{0,62}$/.test(request.expected_database), 'INPUT_INVALID');
  const options = request.options;
  const result = { args: [], environment: [], write: false };
  switch (request.operation) {
    case 'migrate-up': {
      fields(options, ['domain', 'directory', 'manifest_sha256', 'backup_reference', 'backup_completed_at', 'backup_sha256']);
      check(['account', 'billing', 'token'].includes(options.domain) && typeof options.directory === 'string' && isAbsolute(options.directory)
        && basename(options.directory) === options.domain && digest(options.manifest_sha256)
        && reference(options.backup_reference) && digest(options.backup_sha256)
        && typeof options.backup_completed_at === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/.test(options.backup_completed_at)
        && Number.isFinite(Date.parse(options.backup_completed_at))
        && new Date(options.backup_completed_at).toISOString() === options.backup_completed_at.replace('Z', '.000Z'), 'MIGRATION_INPUT_INVALID');
      result.args = ['--dir', options.directory, '--direction', 'up'];
      result.environment = [`${options.domain.toUpperCase()}_MIGRATOR_DATABASE_URL`]; result.write = true;
      break;
    }
    case 'cap-readiness': case 'cap-quiescence':
      fields(options, []); result.args = ['--mode', request.operation.slice(4)]; result.environment = ['TOKEN_POLICY_DATABASE_URL']; break;
    case 'cap-bootstrap':
      fields(options, ['max_per_owner', 'expected_version', 'idempotency_key', 'reason']);
      check(Number.isSafeInteger(options.max_per_owner) && options.max_per_owner >= 1 && options.max_per_owner <= 1000
        && Number.isSafeInteger(options.expected_version) && options.expected_version >= 0
        && typeof options.idempotency_key === 'string' && /^[A-Za-z0-9._:-]{16,128}$/.test(options.idempotency_key)
        && typeof options.reason === 'string' && options.reason.trim() === options.reason && options.reason.length > 0 && options.reason.length <= 500, 'CAP_INPUT_INVALID');
      result.args = ['--mode', 'bootstrap', '--max-per-owner', String(options.max_per_owner), '--expected-version', String(options.expected_version), '--idempotency-key', options.idempotency_key, '--reason', options.reason];
      result.environment = ['TOKEN_POLICY_DATABASE_URL']; result.write = true; break;
    case 'governance-check':
      fields(options, []); result.environment = ['TOKEN_MIGRATOR_DATABASE_URL', 'ACCOUNT_POLICY_DATABASE_URL']; break;
    case 'governance-result':
      fields(options, ['request_id']); check(uuid(options.request_id), 'REQUEST_ID_INVALID');
      result.args = ['--request-id', options.request_id]; result.environment = ['TOKEN_MIGRATOR_DATABASE_URL']; break;
    case 'governance-active-check': case 'governance-activate': case 'governance-safe-check': case 'governance-safe': {
      fields(options, ['expected_state', 'expected_epoch', 'request_id', 'operator', 'reason', 'runtime_roles']);
      const safe = request.operation.startsWith('governance-safe');
      check(Number.isSafeInteger(options.expected_epoch) && options.expected_epoch >= 0 && uuid(options.request_id) && reference(options.operator)
        && typeof options.reason === 'string' && options.reason.trim() === options.reason && options.reason.length > 0 && options.reason.length <= 500
        && (safe ? options.expected_state === 'ACTIVE' && options.expected_epoch > 0 : options.expected_state === 'PRE_ACTIVATION' && options.expected_epoch === 0 || options.expected_state === 'SAFE_ROLLBACK' && options.expected_epoch > 0)
        && Array.isArray(options.runtime_roles) && options.runtime_roles.length === 2
        && options.runtime_roles.every((role, i, roles) => typeof role === 'string' && /^[a-z][a-z0-9_]{0,62}$/.test(role) && role !== 'token_gateway' && !role.startsWith('pg_') && !['token_schema_owner', 'token_migrator', 'token_cap_policy_deployer', 'token_policy_operator'].includes(role) && (i === 0 || roles[i-1] < role)), 'GOVERNANCE_INPUT_INVALID');
      result.args = ['--to', safe ? 'SAFE_ROLLBACK' : 'ACTIVE', '--expected-state', options.expected_state, '--expected-epoch', String(options.expected_epoch), '--request-id', options.request_id, '--operator', options.operator, '--reason', options.reason, '--runtime-roles', options.runtime_roles.join(',')];
      result.write = ['governance-activate', 'governance-safe'].includes(request.operation);
      if (result.write) result.args.push('--apply');
      result.environment = safe ? ['TOKEN_MIGRATOR_DATABASE_URL'] : ['TOKEN_MIGRATOR_DATABASE_URL', 'ACCOUNT_POLICY_DATABASE_URL'];
      break;
    }
    default: fail('OPERATION_UNSUPPORTED');
  }
  check(result.environment.includes('ACCOUNT_POLICY_DATABASE_URL')
    ? typeof request.expected_account_database === 'string' && /^[a-z][a-z0-9_]{0,62}$/.test(request.expected_account_database)
    : request.expected_account_database === null, 'ACCOUNT_DATABASE_BINDING_INVALID');
  return result;
}

export async function migrationDirectoryDigest(directory) {
  const names = (await readdir(directory)).filter(name => /^\d+_[a-z0-9_]+\.(up|down)\.sql$/.test(name)).sort();
  check(names.length > 0, 'MIGRATION_PACKAGE_EMPTY');
  const entries = [];
  for (const name of names) entries.push([name, hash(await readFile(join(directory, name)))]);
  return hash(JSON.stringify(entries));
}

function childEnvironment(request, operation, environment) {
  const result = {};
  for (const key of ['SystemRoot', 'SYSTEMROOT', 'WINDIR', 'TEMP', 'TMP', 'SSL_CERT_FILE', 'SSL_CERT_DIR']) if (environment[key]) result[key] = environment[key];
  for (const name of operation.environment) {
    check(typeof environment[name] === 'string' && environment[name].length > 0, 'DATABASE_ENVIRONMENT_REQUIRED');
    let parsed; try { parsed = new URL(environment[name]); } catch { fail('DATABASE_ENVIRONMENT_INVALID'); }
    check(['postgres:', 'postgresql:'].includes(parsed.protocol) && parsed.hostname && parsed.pathname.length > 1, 'DATABASE_ENVIRONMENT_INVALID');
    // pgx applies URL query parameters AFTER the pathname. Reject rebinding,
    // aliases and startup options rather than trusting only the visible path.
    const seen = new Set();
    for (const entry of parsed.search.slice(1).split('&').filter(Boolean)) {
      const rawKey = entry.split('=', 1)[0];
      let key; try { key = decodeURIComponent(rawKey); } catch { fail('DATABASE_QUERY_INVALID'); }
      check(key === rawKey && ['sslmode', 'sslrootcert', 'sslcert', 'sslkey', 'connect_timeout'].includes(key) && !seen.has(key), 'DATABASE_QUERY_INVALID');
      seen.add(key);
      const value = parsed.searchParams.get(key);
      check(typeof value === 'string' && value.length > 0 && !/[\r\n\0]/.test(value), 'DATABASE_QUERY_INVALID');
      if (key === 'sslmode') check(['disable', 'require', 'verify-full'].includes(value), 'DATABASE_QUERY_INVALID');
      if (key === 'connect_timeout') check(/^[1-9]\d*$/.test(value) && Number(value) <= 30, 'DATABASE_QUERY_INVALID');
    }
    check(!parsed.hash, 'DATABASE_QUERY_INVALID');
    check(decodeURIComponent(parsed.pathname.slice(1)) === (name === 'ACCOUNT_POLICY_DATABASE_URL' ? request.expected_account_database : request.expected_database), 'DATABASE_BINDING_MISMATCH');
    result[name] = environment[name];
  }
  if (request.operation === 'migrate-up') {
    const key = operation.environment[0]; result.DATABASE_URL = result[key]; delete result[key];
    if (request.options.domain === 'token') result.MIGRATION_ROLE = 'token_gateway';
    if (request.options.domain === 'billing') result.MIGRATION_ROLE = 'billing_service';
    // Enforce the existing production backup validator even for a UAT rehearsal.
    result.PINVOU_DEPLOYMENT_ENVIRONMENT = 'production-new';
    result.MIGRATION_BACKUP_EVIDENCE_ID = request.options.backup_reference;
    result.MIGRATION_BACKUP_COMPLETED_AT = request.options.backup_completed_at;
    result.MIGRATION_BACKUP_SHA256 = request.options.backup_sha256;
  }
  return result;
}

async function runChild(executable, args, environment) {
  return new Promise(resolve => {
    const child = spawn(executable, args, { env: environment, shell: false, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
    let bytes = 0; let exhausted = false; const output = createHash('sha256');
    const timeout = setTimeout(() => { exhausted = true; child.kill(); }, 125000);
    const collect = data => { bytes += data.length; if (bytes > 1024 * 1024) { exhausted = true; child.kill(); } };
    child.stdout.on('data', data => { collect(data); if (!exhausted) output.update(data); });
    child.stderr.on('data', collect); // Deliberately never forwarded or included in evidence.
    child.once('error', () => { clearTimeout(timeout); resolve({ ok: false }); });
    child.once('close', code => { clearTimeout(timeout); resolve({ ok: code === 0 && !exhausted, output_sha256: output.digest('hex') }); });
  });
}

export async function executeOperation(request, { execute = false, environment = process.env, runner = runChild } = {}) {
  const operation = validateOperation(request);
  check(hash(await readFile(request.executable)) === request.executable_sha256, 'CONTROL_ARTIFACT_MISMATCH');
  if (request.operation === 'migrate-up') check(await migrationDirectoryDigest(request.options.directory) === request.options.manifest_sha256, 'MIGRATION_PACKAGE_MISMATCH');
  const evidence = { operation: request.operation, approval_reference: request.approval_reference, executable_sha256: request.executable_sha256, request_sha256: hash(JSON.stringify(request)), decision: 'PLAN_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' };
  if (!execute) return evidence; // Never invoke default-writing migrate/bootstrap.
  const childEnv = childEnvironment(request, operation, environment);
  const result = await runner(request.executable, operation.args, childEnv);
  check(result.ok === true, operation.write ? 'OUTCOME_UNCONFIRMED_QUERY_ORIGINAL' : 'CHECK_FAILED');
  check(digest(result.output_sha256), 'OUTPUT_EVIDENCE_INVALID');
  return { ...evidence, decision: 'OPERATION_COMPLETED', output_sha256: result.output_sha256 };
}

async function main(args) {
  check(args.length === 2 || args.length === 3 && args[2] === '--execute', 'USAGE');
  check(args[0] === '--request', 'USAGE');
  const request = parseReleaseJSON(await readFile(args[1], 'utf8'));
  const result = await executeOperation(request, { execute: args.length === 3 });
  process.stdout.write(`${JSON.stringify(result)}\n`);
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main(process.argv.slice(2)).catch(error => {
  process.stderr.write(`${/^MODEL_TOKEN_OPERATION_[A-Z_]+$/.test(error?.message) ? error.message : 'MODEL_TOKEN_OPERATION_INVALID'}\n`); process.exitCode = 1;
});

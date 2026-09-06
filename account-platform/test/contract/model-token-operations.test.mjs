import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { executeOperation, validateOperation, migrationDirectoryDigest } from '../../../scripts/account-platform-model-token-operations.mjs';

const sha = value => createHash('sha256').update(value).digest('hex');
const artifact = process.execPath;
const artifactHash = sha(await readFile(artifact));
const request = (operation = 'governance-check', options = {}) => ({ schema_version: 1, operation, approval_reference: 'approval-test', executable: artifact, executable_sha256: artifactHash, expected_database: 'token_test', expected_account_database: ['governance-check', 'governance-active-check', 'governance-activate'].includes(operation) ? 'account_test' : null, options });
const transition = { expected_state: 'PRE_ACTIVATION', expected_epoch: 0, request_id: '11111111-1111-4111-8111-111111111111', operator: 'operator-test', reason: 'isolated operation test', runtime_roles: ['credential_release', 'gateway_release'] };
const environment = { TOKEN_MIGRATOR_DATABASE_URL: 'postgresql://token_migrator:fixture-only@db.example.invalid/token_test', ACCOUNT_POLICY_DATABASE_URL: 'postgresql://policy_reader:fixture-only@db.example.invalid/account_test', TOKEN_POLICY_DATABASE_URL: 'postgresql://policy_operator:fixture-only@db.example.invalid/token_test', UNRELATED_SECRET: 'must-not-pass-to-child' };

test('operation planner never launches default-writing CLIs or implicitly chains operations', async () => {
  let calls = 0;
  const runner = async () => { calls++; return { ok: true, output_sha256: 'a'.repeat(64) }; };
  for (const item of [request(), request('cap-bootstrap', { max_per_owner: 10, expected_version: 0, idempotency_key: 'approved-fixture-key', reason: 'approved isolated test' }), request('governance-activate', transition)]) {
    assert.equal((await executeOperation(item, { runner })).decision, 'PLAN_VALID');
  }
  assert.equal(calls, 0);
  const completed = await executeOperation(request('governance-activate', transition), { execute: true, environment, runner: async (_exe, args, env) => {
    calls++; assert.deepEqual(Object.keys(env).sort(), ['ACCOUNT_POLICY_DATABASE_URL', 'TOKEN_MIGRATOR_DATABASE_URL']);
    assert.ok(args.includes('--apply')); assert.equal(args[args.indexOf('--to') + 1], 'ACTIVE');
    return { ok: true, output_sha256: 'a'.repeat(64) };
  } });
  assert.equal(calls, 1); assert.equal(completed.environment_status, 'ENV_PENDING'); assert.equal(completed.deployment_status, 'NOT_DEPLOYED');
});

test('safe and original-result query do not depend on Account and cannot replay apply', async () => {
  for (const operation of ['governance-safe', 'governance-safe-check', 'governance-result']) {
    const options = operation === 'governance-result' ? { request_id: transition.request_id } : { ...transition, expected_state: 'ACTIVE', expected_epoch: 1 };
    await executeOperation(request(operation, options), { execute: true, environment: { TOKEN_MIGRATOR_DATABASE_URL: environment.TOKEN_MIGRATOR_DATABASE_URL }, runner: async (_exe, args, env) => {
      assert.deepEqual(Object.keys(env), ['TOKEN_MIGRATOR_DATABASE_URL']);
      assert.equal(args.includes('--apply'), operation === 'governance-safe');
      assert.equal(args[args.indexOf('--request-id') + 1], transition.request_id);
      return { ok: true, output_sha256: 'a'.repeat(64) };
    } });
  }
});

test('operation inputs are closed and preserve exact state, role and cap constraints', () => {
  for (const input of [{ ...request(), apply: true }, request('unknown'), request('cap-readiness', { mode: 'bootstrap' }), request('governance-activate', { ...transition, expected_state: 'ACTIVE', expected_epoch: 1 }), request('governance-activate', { ...transition, runtime_roles: ['gateway_release', 'credential_release'] }), request('governance-activate', { ...transition, runtime_roles: ['token_migrator', 'token_schema_owner'] }), request('cap-bootstrap', { max_per_owner: 0, expected_version: -1, idempotency_key: 'short', reason: '' })]) {
    assert.throws(() => validateOperation(input), /MODEL_TOKEN_OPERATION_/);
  }
  assert.deepEqual(validateOperation(request('cap-readiness')).args, ['--mode', 'readiness']);
  assert.deepEqual(validateOperation(request('cap-quiescence')).args, ['--mode', 'quiescence']);
  assert.equal(validateOperation(request('governance-active-check', transition)).args.includes('--apply'), false);
});

test('artifact/database mismatch and failed child produce safe errors without retry or secret output', async () => {
  let calls = 0;
  const runner = async () => { calls++; return { ok: false, stderr: 'private connection password must-not-escape', stdout: 'private raw response' }; };
  await assert.rejects(executeOperation({ ...request(), executable_sha256: 'f'.repeat(64) }, { execute: true, environment, runner }), /CONTROL_ARTIFACT_MISMATCH/);
  await assert.rejects(executeOperation(request(), { execute: true, environment: { ...environment, TOKEN_MIGRATOR_DATABASE_URL: environment.TOKEN_MIGRATOR_DATABASE_URL.replace('token_test', 'other_database') }, runner }), /DATABASE_BINDING_MISMATCH/);
  assert.equal(calls, 0);
  await assert.rejects(executeOperation(request(), { execute: true, environment: { ...environment, ACCOUNT_POLICY_DATABASE_URL: environment.ACCOUNT_POLICY_DATABASE_URL.replace('account_test', 'wrong_authority') }, runner }), /DATABASE_BINDING_MISMATCH/);
  assert.equal(calls, 0);
  await assert.rejects(executeOperation(request('governance-activate', transition), { execute: true, environment, runner }), error => error.message === 'MODEL_TOKEN_OPERATION_OUTCOME_UNCONFIRMED_QUERY_ORIGINAL');
  assert.equal(calls, 1);
  const source = await readFile(new URL('../../../scripts/account-platform-model-token-operations.mjs', import.meta.url), 'utf8');
  assert.match(source, /shell: false, windowsHide: true/);
  assert.doesNotMatch(source, /stderr\.pipe|process\.stderr\.write\(data/);
});

test('migration execution pins the complete package and always selects explicit forward mode and backup evidence', async () => {
  const directory = fileURLToPath(new URL('../../migrations/token', import.meta.url));
  const manifest = await migrationDirectoryDigest(directory);
  const input = request('migrate-up', { domain: 'token', directory, manifest_sha256: manifest, backup_reference: 'backup-test', backup_completed_at: '2026-09-06T00:00:00Z', backup_sha256: 'a'.repeat(64) });
  let calls = 0;
  const runner = async (_exe, args, env) => {
    calls++; assert.deepEqual(args, ['--dir', directory, '--direction', 'up']);
    assert.equal(env.DATABASE_URL, environment.TOKEN_MIGRATOR_DATABASE_URL); assert.equal(env.MIGRATION_ROLE, 'token_gateway');
    assert.equal(env.PINVOU_DEPLOYMENT_ENVIRONMENT, 'production-new'); assert.equal(env.MIGRATION_BACKUP_EVIDENCE_ID, 'backup-test');
    assert.equal(env.TOKEN_MIGRATOR_DATABASE_URL, undefined);
    return { ok: true, output_sha256: 'a'.repeat(64) };
  };
  await executeOperation(input, { environment, runner }); assert.equal(calls, 0);
  await executeOperation(input, { execute: true, environment, runner }); assert.equal(calls, 1);
  await assert.rejects(executeOperation({ ...input, options: { ...input.options, manifest_sha256: 'f'.repeat(64) } }, { execute: true, environment, runner }), /MIGRATION_PACKAGE_MISMATCH/);
  for (const change of [{ domain: 'billing' }, { direction: 'down' }, { backup_completed_at: '2026-02-31T00:00:00Z' }]) {
    assert.throws(() => validateOperation({ ...input, options: { ...input.options, ...change } }), /MODEL_TOKEN_OPERATION_/);
  }
  assert.equal(calls, 1);
});

test('both PostgreSQL URLs reject query rebinding, startup options, duplicate and alias parameters before any child', async () => {
  let calls = 0;
  const runner = async () => { calls++; return { ok: true, output_sha256: 'a'.repeat(64) }; };
  for (const key of ['TOKEN_MIGRATOR_DATABASE_URL', 'ACCOUNT_POLICY_DATABASE_URL']) {
    for (const query of ['dbname=other_database', 'database=other_database', 'host=other.example.invalid', 'user=other_role', 'service=other', 'options=-c%20role%3Dother', 'search_path=other', 'sslmode=require&sslmode=disable', 'SSLmode=require', 'ssl%6dode=require', 'sslmode=allow', 'connect_timeout=0']) {
      await assert.rejects(executeOperation(request(), { execute: true, environment: { ...environment, [key]: `${environment[key]}?${query}` }, runner }), /DATABASE_QUERY_INVALID/);
    }
  }
  assert.equal(calls, 0);
  await executeOperation(request(), { execute: true, environment: { ...environment, TOKEN_MIGRATOR_DATABASE_URL: `${environment.TOKEN_MIGRATOR_DATABASE_URL}?sslmode=verify-full&connect_timeout=5`, ACCOUNT_POLICY_DATABASE_URL: `${environment.ACCOUNT_POLICY_DATABASE_URL}?sslmode=require` }, runner });
  assert.equal(calls, 1);
});

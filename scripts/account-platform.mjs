#!/usr/bin/env node

import { randomBytes } from 'node:crypto';
import { existsSync } from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { readTestWorkloadCredential } from '../account-platform/test/integration/workload-credential.mjs';
import { developmentNATSConfiguration, newDevelopmentNATSProject, removeDevelopmentNATSProject, retainDevelopmentNATSEvidence } from './account-platform-dev-nats.mjs';
import { guardExistingDevelopmentNATSStorage } from './account-platform-dev-nats-storage.mjs';
import { initializeFreshDevelopmentNATS, developmentTestPaymentEnvironment } from './account-platform-dev-nats-runner.mjs';
import { composeTestConfiguration } from '../account-platform/test/integration/compose-test-configuration.mjs';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const platformRoot = path.join(repositoryRoot, 'account-platform');
const composeFile = path.join(platformRoot, 'deploy', 'compose', 'compose.yaml');
const composeNATSFile = path.join(platformRoot, 'deploy', 'compose', 'compose.dev-nats.yaml');
const composeEnv = path.join(platformRoot, 'deploy', 'compose', '.env');

function executable(name) {
  if (name === 'go' && process.env.GO) return process.env.GO;
  if (process.platform !== 'win32') return name;
  if (name === 'go') return 'go.exe';
  return name;
}

function runNpm(args, options = {}) {
  if (process.platform === 'win32') {
    const npmCli = path.join(path.dirname(process.execPath), 'node_modules', 'npm', 'bin', 'npm-cli.js');
    if (!existsSync(npmCli)) throw new Error(`npm CLI was not found beside Node.js: ${npmCli}`);
    run(process.execPath, [npmCli, ...args], options);
    return;
  }
  run('npm', args, options);
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, {
    cwd: options.cwd || repositoryRoot,
    env: { ...process.env, ...options.env },
    stdio: 'inherit',
    shell: false,
  });
  if (result.error) throw result.error;
  if (result.status !== 0) {
    const error = new Error(`${command} exited with status ${result.status ?? 1}`);
    error.exitCode = result.status ?? 1;
    throw error;
  }
}

function commandAvailable(command, args = ['--version']) {
  const result = spawnSync(command, args, { cwd: repositoryRoot, env: process.env, stdio: 'ignore', shell: false });
  return !result.error && result.status === 0;
}

function generatedComposeEnvironment() {
  // Fail before starting Docker if the operator has not supplied a real
  // workload credential. Random strings cannot authenticate JWT services.
  const workloadCredential = readTestWorkloadCredential();
  const operatorConfiguration = composeTestConfiguration();
  const secret = (bytes = 32) => randomBytes(bytes).toString('base64url');
  return {
    ...operatorConfiguration,
    ACCOUNT_PLATFORM_POSTGRES_PASSWORD: secret(),
    ACCOUNT_DB_PASSWORD: secret(),
    BILLING_DB_PASSWORD: secret(),
    TOKEN_DB_PASSWORD: secret(),
    ACCOUNT_MIGRATION_DB_PASSWORD: secret(),
    ACCOUNT_APPROVAL_WRITER_DB_PASSWORD: secret(),
    ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD: secret(),
    ACCOUNT_APPROVAL_RELAY_DB_PASSWORD: secret(),
    BILLING_MIGRATION_DB_PASSWORD: secret(),
    TOKEN_MIGRATION_DB_PASSWORD: secret(),
    TOKEN_POLICY_DB_PASSWORD: secret(),
    PAYMENT_SIM_DB_PASSWORD: secret(),
    NEWAPI_DB_PASSWORD: secret(),
    KEYCLOAK_DB_PASSWORD: secret(),
    KEYCLOAK_ADMIN_USERNAME: `pinvou-test-${randomBytes(8).toString('hex')}`,
    KEYCLOAK_ADMIN_PASSWORD: secret(),
    PAYMENT_SIM_HMAC_UNIONPAY: secret(),
    PAYMENT_SIM_HMAC_ALIPAY: secret(),
    PAYMENT_SIM_HMAC_WECHAT: secret(),
    INTERNAL_SERVICE_TOKEN: workloadCredential,
    PAYMENT_SIM_PAY_TOKEN_SECRET: secret(48),
    TOKEN_LOCAL_KEK_BASE64: randomBytes(32).toString('base64'),
    AUDIT_EXPORT_MASTER_KEY_BASE64: randomBytes(32).toString('base64'),
    MOCK_NEWAPI_KEY: secret(48),
    NEWAPI_SESSION_SECRET: secret(48),
    NEWAPI_CRYPTO_SECRET: secret(48),
  };
}

function ensureNodeModules() {
  if (!existsSync(path.join(platformRoot, 'node_modules'))) {
    throw new Error('account-platform dependencies are missing; run npm --prefix account-platform ci');
  }
}

function verify() {
  ensureNodeModules();
  run(process.execPath, [path.join(repositoryRoot, 'scripts', 'check-recharge-product-document-baseline.mjs')]);
  run(process.execPath, [path.join(repositoryRoot, 'scripts', 'check-account-platform-tasks.mjs')]);
  run(process.execPath, [path.join(repositoryRoot, 'scripts', 'check-account-platform-authorization-coverage.mjs')]);
  run(process.execPath, [path.join(repositoryRoot, 'scripts', 'account-platform-migration-manifest.mjs')]);
  run(process.execPath, [path.join(repositoryRoot, 'scripts', 'account-platform-recovery-gate.mjs')]);
  run(process.execPath, [path.join(repositoryRoot, 'scripts', 'account-platform-ha-gate.mjs')]);
  run(process.execPath, [path.join(repositoryRoot, 'scripts', 'account-platform-performance-gate.mjs')]);
  run(executable('go'), ['test', '-tags=nomsgpack', './...'], { cwd: platformRoot });
  run(executable('go'), ['vet', '-tags=nomsgpack', './...'], { cwd: platformRoot });
  runNpm(['test'], { cwd: platformRoot });
  run(process.execPath, [path.join(repositoryRoot, 'scripts', 'check-account-platform-i18n.mjs')]);
  run(process.execPath, [path.join(repositoryRoot, 'scripts', 'check-account-platform-observability.mjs')]);
  run(process.env.PYTHON || 'python', ['scripts/architecture-guard.py']);
  run('git', ['diff', '--check']);
}

function capture(command, args, env) {
  const result = spawnSync(command, args, { cwd: repositoryRoot, env: { ...process.env, ...env }, encoding: 'utf8', shell: false, maxBuffer: 1024 * 1024 });
  if (result.error || result.status !== 0) throw new Error('DEV_NATS_OPERATOR_COMMAND_FAILED');
  return result.stdout;
}

function compose(action) {
  if (!existsSync(composeEnv)) {
    throw new Error(`Compose environment is missing: copy ${composeEnv}.example to ${composeEnv} and replace every placeholder`);
  }
  const environment = developmentNATSConfiguration(process.env, { cleanup: action === 'down' });
  const args = ['compose', '--env-file', composeEnv, '-f', composeFile, '-f', composeNATSFile, '--profile', 'real-newapi'];
  if (action === 'up') guardExistingDevelopmentNATSStorage({ args, environment, dockerHost: process.env.DOCKER_HOST, capture: (command, commandArgs) => capture(command, commandArgs, environment) });
  run('docker', [...args, action, ...(action === 'up' ? ['--build', '-d', '--wait'] : [])], { env: environment });
}

function composeNATSOperation(operationArgs) {
  const [operation, flag, expectedVersion, ...extra] = operationArgs;
  let command;
  if (operation === 'prepare' && flag === undefined) command = [];
  else if (operation === 'create-only' && flag === undefined) command = ['dev-event-resources', '-create-only'];
  else if (['status', 'quiesce', 'fence', 'initialize-consumers'].includes(operation) && flag === '--expected-version' && /^[1-9][0-9]{0,18}$/.test(expectedVersion || '') && BigInt(expectedVersion) <= 9223372036854775807n && extra.length === 0) command = ['dev-approval-resources', '-action', operation, '-expected-version', expectedVersion];
  else throw new Error('usage: dev nats <prepare|create-only> | dev nats <status|quiesce|fence|initialize-consumers> --expected-version N');
  if (!existsSync(composeEnv)) throw new Error('Compose environment is missing');
  const environment = developmentNATSConfiguration(process.env, { initializing: true });
  const args = ['compose', '--env-file', composeEnv, '-f', composeFile, '-f', composeNATSFile, '--profile', 'real-newapi'];
  guardExistingDevelopmentNATSStorage({ args, environment, dockerHost: process.env.DOCKER_HOST, capture: (command, commandArgs) => capture(command, commandArgs, environment) });
  if (operation === 'prepare') {
    run('docker', [...args, 'run', '--rm', '--no-deps', 'deployment-credentials-check'], { env: environment });
    run('docker', [...args, 'up', '--build', '-d', '--wait', 'postgres', 'nats'], { env: environment });
    run('docker', [...args, 'run', '--rm', '--build', '--no-deps', 'account-migrate'], { env: environment });
  } else run('docker', [...args, 'run', '--rm', '--build', '--no-deps', ...command], { env: environment });
}

function composeTest(testFiles) {
  const paymentEnvironment = developmentTestPaymentEnvironment(testFiles, process.env);
  const environment = { ...generatedComposeEnvironment(), ...developmentNATSConfiguration(process.env, { fresh: true }), ...paymentEnvironment };
  if (process.env.DOCKER_HOST) throw new Error('DEV_NATS_TEST_DOCKER_HOST_OVERRIDE_FORBIDDEN');
  if (!commandAvailable('docker', ['--version'])) throw new Error('Docker is required for integration tests');
  const owned = newDevelopmentNATSProject(environment);
  const args = ['compose', '--env-file', `${composeEnv}.example`, '-f', composeFile, '-f', composeNATSFile, '--profile', 'real-newapi'];
  let failure;
  let clean = true;
  try {
    initializeFreshDevelopmentNATS({
      args, owned,
      run: (command, commandArgs) => run(command, commandArgs, { env: owned.environment }),
      capture: (command, commandArgs) => capture(command, commandArgs, owned.environment),
    });
    run('docker', [...args, 'up', '--build', '-d', '--wait'], { env: owned.environment });
    for (const testFile of testFiles) {
      run(process.execPath, [path.join(platformRoot, 'test', 'integration', testFile)], {
        env: {
          ...owned.environment,
          PINVOU_TEST_COMPOSE_FILE: composeFile,
          PINVOU_TEST_COMPOSE_NATS_FILE: composeNATSFile,
          PINVOU_TEST_COMPOSE_ENV_FILE: `${composeEnv}.example`,
        },
      });
    }
  } catch (error) {
    failure = error;
  } finally {
    if (owned.mayCleanCompose) {
      try {
        run('docker', [...args, 'down', '--volumes', '--remove-orphans'], { env: owned.environment });
      } catch (cleanupError) {
        clean = false;
        if (!failure) failure = cleanupError;
        else console.error(`[account-platform] cleanup failed: ${cleanupError.message}`);
      }
    }
    // Retain owned files when Docker cleanup failed; a live container may
    // still be using them. External supplied material is never removed.
    try { console.log(`[account-platform] public initialization evidence: ${retainDevelopmentNATSEvidence(owned, failure)}`); }
    catch { if (!failure) failure = new Error('DEV_NATS_EVIDENCE_RETENTION_FAILED'); clean = false; }
    if (clean) removeDevelopmentNATSProject(owned);
  }
  if (failure) throw failure;
}

function integration() {
  composeTest(['compose-smoke.mjs']);
}

function e2e() {
  composeTest(['compose-smoke.mjs', 'compose-acceptance.mjs']);
}

function fault() {
  composeTest(['compose-smoke.mjs', 'compose-fault.mjs']);
}

function security() {
  ensureNodeModules();
  run(executable('go'), ['run', 'golang.org/x/vuln/cmd/govulncheck@v1.6.0', './...'], { cwd: platformRoot });
  runNpm(['audit', '--registry=https://registry.npmjs.org', '--audit-level=high'], { cwd: platformRoot });
}

const [scope, action] = process.argv.slice(2);
try {
  if (scope === 'verify' && action === undefined) verify();
  else if (scope === 'production' && ['check-business', 'check-migrations'].includes(action)) {
    ensureNodeModules();
    run(process.execPath, [path.join(repositoryRoot, 'scripts', action === 'check-business'
      ? 'account-platform-business-compose-check.mjs' : 'account-platform-business-migrations-check.mjs'), ...process.argv.slice(4)]);
  }
  else if (scope === 'approval-backfill') run(executable('go'), ['run', '-tags=nomsgpack', './cmd/approval-authorization-backfill', ...process.argv.slice(3)], { cwd: platformRoot });
  else if (scope === 'dev' && action === 'nats') composeNATSOperation(process.argv.slice(4));
  else if (scope === 'dev' && action === 'up') compose('up');
  else if (scope === 'dev' && action === 'down') compose('down');
  else if (scope === 'test' && action === 'unit') run(executable('go'), ['test', '-tags=nomsgpack', './...'], { cwd: platformRoot });
  else if (scope === 'test' && action === 'contract') {
    ensureNodeModules();
    runNpm(['test'], { cwd: platformRoot });
  } else if (scope === 'test' && action === 'integration') integration();
  else if (scope === 'test' && action === 'e2e') e2e();
  else if (scope === 'test' && action === 'fault') fault();
  else if (scope === 'test' && action === 'security') security();
  else {
    throw new Error('usage: account-platform.mjs verify | production <check-business|check-migrations> --recipe FILE --inventory FILE --renderer BINARY | approval-backfill [--batch=100 --deadline=5m] | dev <up|down> | dev nats <prepare|create-only|status|quiesce|fence|initialize-consumers> [--expected-version N] | test <unit|contract|integration|e2e|fault|security>');
  }
} catch (error) {
  console.error(`[account-platform] ${error.message}`);
  process.exitCode = error.exitCode || 1;
}

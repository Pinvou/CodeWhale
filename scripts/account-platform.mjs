#!/usr/bin/env node

import { randomBytes } from 'node:crypto';
import { existsSync } from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const platformRoot = path.join(repositoryRoot, 'account-platform');
const composeFile = path.join(platformRoot, 'deploy', 'compose', 'compose.yaml');
const composeEnv = path.join(platformRoot, 'deploy', 'compose', '.env');

function executable(name) {
  if (process.platform !== 'win32') return name;
  if (name === 'go') {
    const local = process.env.LOCALAPPDATA
      ? path.join(process.env.LOCALAPPDATA, 'PinvouCodexTools', 'go1.26.5', 'go', 'bin', 'go.exe')
      : '';
    return local && existsSync(local) ? local : 'go.exe';
  }
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
  const secret = (bytes = 32) => randomBytes(bytes).toString('base64url');
  return {
    COMPOSE_PROJECT_NAME: `pinvou-account-platform-test-${process.pid}`,
    ACCOUNT_PLATFORM_POSTGRES_PASSWORD: secret(),
    ACCOUNT_DB_PASSWORD: secret(),
    BILLING_DB_PASSWORD: secret(),
    TOKEN_DB_PASSWORD: secret(),
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
    INTERNAL_SERVICE_TOKEN: secret(48),
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
  run('python', ['scripts/architecture-guard.py']);
  run('git', ['diff', '--check']);
}

function compose(action) {
  if (!existsSync(composeEnv)) {
    throw new Error(`Compose environment is missing: copy ${composeEnv}.example to ${composeEnv} and replace every placeholder`);
  }
  run('docker', ['compose', '--env-file', composeEnv, '-f', composeFile, ...(action === 'up' ? ['--profile', 'real-newapi'] : []), action, ...(action === 'up' ? ['--build', '-d', '--wait'] : [])]);
}

function composeTest(testFiles) {
  if (!commandAvailable('docker', ['version'])) throw new Error('Docker is required for integration tests');
  const environment = generatedComposeEnvironment();
  const args = ['compose', '--env-file', `${composeEnv}.example`, '-f', composeFile];
  let failure;
  try {
    run('docker', [...args, 'up', '--build', '-d', '--wait'], { env: environment });
    for (const testFile of testFiles) {
      run(process.execPath, [path.join(platformRoot, 'test', 'integration', testFile)], {
        env: {
          ...environment,
          PINVOU_TEST_COMPOSE_FILE: composeFile,
          PINVOU_TEST_COMPOSE_ENV_FILE: `${composeEnv}.example`,
        },
      });
    }
  } catch (error) {
    failure = error;
  } finally {
    try {
      run('docker', [...args, 'down', '--volumes', '--remove-orphans'], { env: environment });
    } catch (cleanupError) {
      if (!failure) throw cleanupError;
      console.error(`[account-platform] cleanup failed: ${cleanupError.message}`);
    }
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
    throw new Error('usage: account-platform.mjs verify | dev <up|down> | test <unit|contract|integration|e2e|fault|security>');
  }
} catch (error) {
  console.error(`[account-platform] ${error.message}`);
  process.exitCode = error.exitCode || 1;
}

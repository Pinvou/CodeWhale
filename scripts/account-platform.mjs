#!/usr/bin/env node

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

function ensureNodeModules() {
  if (!existsSync(path.join(platformRoot, 'node_modules'))) {
    throw new Error('account-platform dependencies are missing; run npm --prefix account-platform ci');
  }
}

function verify() {
  ensureNodeModules();
  run(executable('go'), ['test', './...'], { cwd: platformRoot });
  run(executable('go'), ['vet', './...'], { cwd: platformRoot });
  runNpm(['test'], { cwd: platformRoot });
  run('python', ['scripts/architecture-guard.py']);
  run('git', ['diff', '--check']);
}

function compose(action) {
  if (!existsSync(composeEnv)) {
    throw new Error(`Compose environment is missing: copy ${composeEnv}.example to ${composeEnv} and replace every placeholder`);
  }
  run('docker', ['compose', '--env-file', composeEnv, '-f', composeFile, action, ...(action === 'up' ? ['--build', '-d', '--wait'] : [])]);
}

function integration() {
  if (!commandAvailable('docker', ['version'])) throw new Error('Docker is required for integration tests');
  const environment = { COMPOSE_PROJECT_NAME: 'pinvou-account-platform-test' };
  const args = ['compose', '--env-file', `${composeEnv}.example`, '-f', composeFile];
  let failure;
  try {
    run('docker', [...args, 'up', '--build', '-d', '--wait'], { env: environment });
    run(process.execPath, [path.join(platformRoot, 'test', 'integration', 'compose-smoke.mjs')]);
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
  else if (scope === 'test' && action === 'unit') run(executable('go'), ['test', './...'], { cwd: platformRoot });
  else if (scope === 'test' && action === 'contract') {
    ensureNodeModules();
    runNpm(['test'], { cwd: platformRoot });
  } else if (scope === 'test' && action === 'integration') integration();
  else if (scope === 'test' && action === 'security') security();
  else {
    throw new Error('usage: account-platform.mjs verify | dev <up|down> | test <unit|contract|integration|security>');
  }
} catch (error) {
  console.error(`[account-platform] ${error.message}`);
  process.exitCode = error.exitCode || 1;
}

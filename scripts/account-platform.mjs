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
      ? path.join(process.env.LOCALAPPDATA, 'PinvouCodexTools', 'go1.26.6', 'go', 'bin', 'go.exe')
      : '';
    return local && existsSync(local) ? local : 'go.exe';
  }
  return name;
}

function runNpm(args, options = {}) {
  options = { ...options, env: { ...options.env, PINVOU_ACCOUNT_GO_EXECUTABLE: executable('go') } };
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
  if (result.status !== 0) process.exit(result.status ?? 1);
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

const [scope, action] = process.argv.slice(2);
try {
  if (scope === 'verify' && action === undefined) verify();
  else if (scope === 'dev' && action === 'up') compose('up');
  else if (scope === 'dev' && action === 'down') compose('down');
  else if (scope === 'test' && action === 'unit') run(executable('go'), ['test', './...'], { cwd: platformRoot });
  else if (scope === 'test' && action === 'contract') {
    ensureNodeModules();
    runNpm(['test'], { cwd: platformRoot });
  } else {
    throw new Error('usage: account-platform.mjs verify | dev <up|down> | test <unit|contract>');
  }
} catch (error) {
  console.error(`[account-platform] ${error.message}`);
  process.exit(1);
}

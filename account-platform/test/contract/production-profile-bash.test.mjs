import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { dirname } from 'node:path';
import test from 'node:test';
import { findTestBash } from '../fixtures/bash-binary.mjs';

const success = { status: 0, stdout: 'pinvou-controlled-bash' };

test('explicit Bash configuration is probed and takes precedence over discovery', () => {
  const calls = [];
  assert.equal(findTestBash({
    platform: 'win32', env: { PINVOU_TEST_BASH_BINARY: 'D:/Git/bin/bash.exe' },
    exists() { assert.fail('explicit configuration must not discover another shell'); },
    run(binary) { calls.push(binary); return success; },
  }), 'D:/Git/bin/bash.exe');
  assert.deepEqual(calls, ['D:/Git/bin/bash.exe']);
});

test('invalid or empty explicit Bash configuration fails without PATH fallback', () => {
  for (const binary of ['D:/absent-fixture/bash.exe', '']) {
    const calls = [];
    assert.throws(() => findTestBash({
      platform: 'win32', env: { PINVOU_TEST_BASH_BINARY: binary },
      exists() { assert.fail('invalid explicit configuration must not discover another shell'); },
      run(value) { calls.push(value); return { error: new Error('controlled ENOENT'), status: null }; },
    }), /PINVOU_TEST_BASH_BINARY must name a runnable Bash/);
    assert.deepEqual(calls, binary ? [binary] : []);
  }
  assert.throws(() => findTestBash({
    env: { ...process.env, PINVOU_TEST_BASH_BINARY: `${process.execPath}.absent-bash-fixture` },
  }), /PINVOU_TEST_BASH_BINARY must name a runnable Bash/);
});

test('Windows missing C-drive Git candidates uses real PATH Bash for all four interpolation outcomes', () => {
  const installed = findTestBash();
  const env = { ...process.env };
  delete env.PINVOU_TEST_BASH_BINARY;
  // Only the known, already runnable shell directory is added; no installation
  // or machine-wide search. Model absent C paths without changing the machine.
  const separator = process.platform === 'win32' ? ';' : ':';
  env.PATH = `${dirname(installed)}${separator}${env.PATH || ''}`;
  const calls = [];
  const bash = findTestBash({
    platform: 'win32', env, exists: () => false,
    run(binary, args, options) { calls.push(binary); return spawnSync(binary, args, options); },
  });
  assert.equal(bash, 'bash');
  assert.deepEqual(calls, ['bash']);
  for (const value of [undefined, '', 'personal', 'models']) {
    const modeEnv = { ...env };
    delete modeEnv.ACCOUNT_PLATFORM_PROFILE;
    if (value !== undefined) modeEnv.ACCOUNT_PLATFORM_PROFILE = value;
    const result = spawnSync(bash, ['-c', 'printf \'%s\' "${ACCOUNT_PLATFORM_PROFILE:?set personal or models matching selected composition}"'], {
      env: modeEnv, encoding: 'utf8', timeout: 15_000,
    });
    assert.equal(result.error, undefined);
    if (!value) {
      assert.notEqual(result.status, 0);
      assert.equal(result.stdout, '');
      assert.match(result.stderr, /set personal or models matching selected composition/);
    } else {
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, value);
    }
  }
});

test('Windows rejects failing WSL launchers and programs that are not Bash', () => {
  for (const failure of [
    { status: 1, stdout: '', stderr: 'controlled WSL startup failure' },
    { status: null, error: new Error('controlled startup failure') },
    { status: 0, stdout: '' },
  ]) {
    assert.throws(() => findTestBash({ platform: 'win32', env: {}, exists: () => false, run: () => failure }), /Bash is required/);
  }
});

test('Windows probes known Git candidates before trying PATH and rejects an unstartable candidate', () => {
  const calls = [];
  assert.equal(findTestBash({
    platform: 'win32', env: {}, exists: () => true,
    run(binary) { calls.push(binary); return binary === 'bash' ? success : { status: 1, stdout: '' }; },
  }), 'bash');
  assert.deepEqual(calls, ['C:/Program Files/Git/bin/bash.exe', 'C:/Program Files/Git/usr/bin/bash.exe', 'bash']);
});

test('Linux retains the existing bin Bash path', () => {
  const calls = [];
  assert.equal(findTestBash({ platform: 'linux', env: {}, run(binary) { calls.push(binary); return success; } }), '/bin/bash');
  assert.deepEqual(calls, ['/bin/bash']);
});

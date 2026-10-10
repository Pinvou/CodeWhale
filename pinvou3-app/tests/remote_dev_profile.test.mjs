import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

const source = readFileSync(new URL('../run-dev.sh', import.meta.url), 'utf8');
const bash = process.platform === 'win32' ? 'C:/Program Files/Git/bin/bash.exe' : 'bash';

const begin = source.indexOf('# ── 完整 WebUI v2 relay');
const end = source.indexOf('# ── 编译器自身线程栈注入', begin);
assert.ok(begin >= 0 && end > begin, 'the real Relay setup block must exist');
const block = source.slice(begin, end);
const variables = [
  'PINVOU_REMOTE_ENDPOINT_PROFILE', 'PINVOU_REMOTE_PUBLIC_URL', 'PINVOU_REMOTE_RELAY_WS_URL',
  'PINVOU_REMOTE_RUNTIME_RELAY_WS_URL', 'PINVOU_REMOTE_RELAY_SIGNING_KEY_ID',
  'PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64',
];

test('development setup exports only the profile and never imposes address or pin defaults', () => {
  const exports = source.split(/\r?\n/).filter((line) => /^export PINVOU_REMOTE_/.test(line));
  assert.equal(exports.length, 1, 'only the profile may be defaulted by the development entry');
  assert.match(exports[0], /^export PINVOU_REMOTE_ENDPOINT_PROFILE=/);
});

const explicitOverrides = {
  PINVOU_REMOTE_PUBLIC_URL: 'https://override.invalid/remote',
  PINVOU_REMOTE_RELAY_WS_URL: 'wss://override.invalid/remote/ws',
  PINVOU_REMOTE_RUNTIME_RELAY_WS_URL: 'wss://runtime.invalid/remote/ws',
  PINVOU_REMOTE_RELAY_SIGNING_KEY_ID: 'fixture-key',
  PINVOU_REMOTE_RELAY_SIGNING_PUBLIC_KEY_B64: 'fixture-not-a-real-key',
};
const cases = [
  ['unset profile', {}, 'test'],
  ['explicit production profile', { PINVOU_REMOTE_ENDPOINT_PROFILE: 'production' }, 'production'],
  ['empty profile', { PINVOU_REMOTE_ENDPOINT_PROFILE: '' }, 'test'],
  ['other explicit profile', { PINVOU_REMOTE_ENDPOINT_PROFILE: 'custom-profile' }, 'custom-profile'],
  ['empty address and pin overrides', Object.fromEntries(variables.slice(1).map(key => [key, ''])), 'test'],
  ['production with explicit overrides', { PINVOU_REMOTE_ENDPOINT_PROFILE: 'production', ...explicitOverrides }, 'production'],
  ['default profile with explicit overrides', explicitOverrides, 'test'],
];
for (const [name, overrides, profile] of cases) {
  test(`development setup preserves ${name} without launching the app`, () => {
    const env = { ...process.env };
    for (const key of variables) delete env[key];
    Object.assign(env, overrides);
    // Execute the actual setup, not a reimplementation, and stop before the
    // compiler wrapper or app launch. Node serializes unset and empty distinctly.
    const report = `process.stdout.write(JSON.stringify(Object.fromEntries(${JSON.stringify(variables)}.map(k=>[k,process.env[k]??null]))))`;
    const result = spawnSync(bash, ['-c', `${block}\nexec node -e '${report}'`], { env, encoding: 'utf8' });
    assert.equal(result.status, 0, result.error?.message || result.stderr);
    const observed = JSON.parse(result.stdout);
    assert.equal(observed.PINVOU_REMOTE_ENDPOINT_PROFILE, profile);
    for (const key of variables.slice(1)) assert.equal(observed[key], overrides[key] ?? null, key);
  });
}

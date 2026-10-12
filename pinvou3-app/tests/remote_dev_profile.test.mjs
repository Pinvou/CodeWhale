import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

const source = readFileSync(new URL('../run-dev.sh', import.meta.url), 'utf8');
const bash = process.platform === 'win32' ? 'C:/Program Files/Git/bin/bash.exe' : 'bash';

test('development endpoint defaults select the matching pin profile without launching the app', () => {
  const exports = source.split(/\r?\n/).filter((line) => /^export PINVOU_REMOTE_(?:ENDPOINT_PROFILE|PUBLIC_URL|RELAY_WS_URL)=/.test(line));
  assert.equal(exports.length, 3, 'profile and both endpoint defaults must be exported');
  const env = { ...process.env };
  for (const key of ['PINVOU_REMOTE_ENDPOINT_PROFILE', 'PINVOU_REMOTE_PUBLIC_URL', 'PINVOU_REMOTE_RELAY_WS_URL']) delete env[key];
  const command = `${exports.join('\n')}\nprintf '%s\\n' "$PINVOU_REMOTE_ENDPOINT_PROFILE" "$PINVOU_REMOTE_PUBLIC_URL" "$PINVOU_REMOTE_RELAY_WS_URL"`;
  const defaults = spawnSync(bash, ['-c', command], { env, encoding: 'utf8' });
  assert.equal(defaults.status, 0, defaults.error?.message || defaults.stderr);
  assert.deepEqual(defaults.stdout.trim().split(/\r?\n/), [
    'test', 'https://pinvou.com/pinvou3/remote-test', 'wss://pinvou.com/pinvou3/remote-test/ws',
  ]);
  const overrides = spawnSync(bash, ['-c', command], {
    env: { ...env, PINVOU_REMOTE_ENDPOINT_PROFILE: 'production',
      PINVOU_REMOTE_PUBLIC_URL: 'https://override.invalid/remote',
      PINVOU_REMOTE_RELAY_WS_URL: 'wss://override.invalid/remote/ws' },
    encoding: 'utf8',
  });
  assert.equal(overrides.status, 0, overrides.error?.message || overrides.stderr);
  assert.deepEqual(overrides.stdout.trim().split(/\r?\n/), [
    'production', 'https://override.invalid/remote', 'wss://override.invalid/remote/ws',
  ]);
});

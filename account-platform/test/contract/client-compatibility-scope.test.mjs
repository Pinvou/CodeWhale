import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

test('client compatibility is consumed by readiness only', async () => {
  const source = await readFile(new URL('../../cmd/token-gateway/personal_execution.go', import.meta.url), 'utf8');
  assert.match(source, /clientcompat\.FromEnv\(\)/);
  assert.match(source, /Compatibility:\s*compatibility\.Manifest/);
  assert.doesNotMatch(source, /compatibility\.(?:Wrap|WrapExcept)\(/);
});

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../../..');

test('unified runner exposes deterministic integration and security gates', async () => {
  const runner = await readFile(path.join(root, 'scripts/account-platform.mjs'), 'utf8');
  assert.match(runner, /test.*integration/);
  assert.match(runner, /generatedComposeEnvironment/);
  assert.match(runner, /COMPOSE_PROJECT_NAME: `pinvou-account-platform-test-\$\{process\.pid\}`/);
  assert.match(runner, /test.*e2e/);
  assert.match(runner, /test.*fault/);
  assert.match(runner, /down', '--volumes', '--remove-orphans'/);
  assert.match(runner, /govulncheck@v1\.8\.0/);
  assert.match(runner, /audit-level=high/);
  assert.match(runner, /Docker is required for integration tests/);
});

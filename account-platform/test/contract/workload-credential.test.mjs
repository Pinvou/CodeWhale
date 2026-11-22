import assert from 'node:assert/strict';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { spawnSync } from 'node:child_process';
import { readTestWorkloadCredential } from '../integration/workload-credential.mjs';

test('test workload input fails closed without a bounded absolute regular credential file', async () => {
  assert.throws(() => readTestWorkloadCredential({}), /FILE_REQUIRED/);
  assert.throws(() => readTestWorkloadCredential({ PINVOU_TEST_WORKLOAD_CREDENTIAL_FILE: 'relative.jwt' }), /FILE_REQUIRED/);
  const directory = await mkdtemp(path.join(tmpdir(), 'pinvou-workload-input-'));
  const file = path.join(directory, 'workload.jwt');
  const input = { PINVOU_TEST_WORKLOAD_CREDENTIAL_FILE: file };
  try {
    assert.throws(() => readTestWorkloadCredential(input), /FILE_INVALID/);
    await writeFile(file, 'not-a-jwt');
    assert.throws(() => readTestWorkloadCredential(input), /FILE_INVALID/);
    await writeFile(file, 'x'.repeat(65537));
    assert.throws(() => readTestWorkloadCredential(input), /FILE_INVALID/);
    await mkdir(path.join(directory, 'not-a-file'));
    assert.throws(() => readTestWorkloadCredential({ PINVOU_TEST_WORKLOAD_CREDENTIAL_FILE: path.join(directory, 'not-a-file') }), /FILE_INVALID/);
    // Controlled syntax-only inputs, not signed credentials or service proof.
    await writeFile(file, 'e30.e30.c2ln\n');
    assert.equal(readTestWorkloadCredential(input), 'e30.e30.c2ln');
    await writeFile(file, 'e30.e30.cm90YXRlZA\n');
    assert.equal(readTestWorkloadCredential(input), 'e30.e30.cm90YXRlZA');
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test('E2E and fault entrypoints refuse missing credentials before starting Docker', () => {
  const environment = { ...process.env };
  delete environment.PINVOU_TEST_WORKLOAD_CREDENTIAL_FILE;
  for (const action of ['e2e', 'fault', 'integration']) {
    const result = spawnSync(process.execPath, [path.resolve(import.meta.dirname, '../../../scripts/account-platform.mjs'), 'test', action], {
      env: environment, encoding: 'utf8', shell: false,
    });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /TEST_WORKLOAD_CREDENTIAL_FILE_REQUIRED/);
    assert.equal(result.stdout, '');
  }
});

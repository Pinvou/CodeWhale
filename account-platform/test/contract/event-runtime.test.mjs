import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';

const root = path.resolve(import.meta.dirname, '../..');

test('registration, profile, and payment schemas accept the actual production event envelope', async () => {
  const result = spawnSync(process.env.PINVOU_ACCOUNT_GO_EXECUTABLE || (process.platform === 'win32' ? 'go.exe' : 'go'),
    ['run', '-mod=readonly', './test/contract/event-fixtures'], { cwd: root, encoding: 'utf8', timeout: 120000 });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  const fixtures = JSON.parse(result.stdout);
  assert.equal(Object.keys(fixtures).length, 7);
  const ajv = new Ajv2020({ strict: true, allErrors: true });
  addFormats(ajv);
  for (const [name, event] of Object.entries(fixtures)) {
    const schema = JSON.parse(await readFile(path.join(root, 'contracts/events', `${name}.schema.json`), 'utf8'));
    const validate = ajv.compile(schema);
    assert.ok(validate(event), `${name}: ${JSON.stringify(validate.errors)}`);
    assert.equal(event.trace_id, 'fixture-trace');
    assert.equal(validate({ ...event, trace_id: undefined }), false);
    assert.equal(validate({ ...event, password: 'forbidden-fixture' }), false);
    assert.equal(validate({ ...event, payload: { ...event.payload, access_token: 'forbidden-fixture' } }), false);
  }
});

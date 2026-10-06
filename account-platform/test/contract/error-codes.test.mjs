import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv from 'ajv/dist/2020.js';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');

async function productionSources(directory) {
  const entries = await readdir(directory, { withFileTypes: true });
  return (await Promise.all(entries.map(async entry => {
    const filename = path.join(directory, entry.name);
    if (entry.isDirectory()) return productionSources(filename);
    return entry.name.endsWith('.go') && !entry.name.endsWith('_test.go') ? [filename] : [];
  }))).flat();
}

test('common error contract accepts declared production literals and rejects unknown codes', async () => {
  const common = YAML.parse(await readFile(path.join(root, 'contracts/openapi/common.yaml'), 'utf8'));
  const validate = new Ajv({ strict: true }).compile(common.components.schemas.Error);
  const codes = new Set();
  for (const filename of await productionSources(path.join(root, 'internal'))) {
    const source = await readFile(filename, 'utf8');
    const patterns = [
      /api\.WriteError\([^\n]*?,\s*"([A-Z][A-Z0-9_]+)"/g,
      /(?:status, code, message\s*=|http\.Status\w+,)\s*(?:http\.Status\w+,\s*)?"([A-Z][A-Z0-9_]+)"/g,
    ];
    for (const pattern of patterns) {
      for (const match of source.matchAll(pattern)) {
        codes.add(match[1]);
        assert.ok(validate({ code: match[1], message: 'contract probe', request_id: 'probe' }),
          `${path.relative(root, filename)} ${match[1]}: ${JSON.stringify(validate.errors)}`);
      }
    }
  }
  assert.ok(codes.size > 0, 'the production inventory must match real literals');
  assert.equal(validate({ code: 'UNKNOWN_CONTRACT_CODE', message: 'probe', request_id: 'probe' }), false);
  // This is a schema/literal guard, not a claim that each error branch ran.
});

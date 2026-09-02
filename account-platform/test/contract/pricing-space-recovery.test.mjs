import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');

test('billing repairs missing pricing-space ETags before serving lifecycle reads', async () => {
  const migration = await readFile(
    path.join(root, 'migrations/billing/000051_pricing_space_versions_repair.up.sql'),
    'utf8',
  );

  assert.match(migration, /INSERT INTO pricing_space_versions/);
  assert.match(migration, /FROM pricing_versions/);
  assert.match(migration, /ON CONFLICT \(scope_type,scope_key,protocol_code,formula_version_id,credit_unit\)/);
  assert.match(migration, /GREATEST\(pricing_space_versions\.etag,EXCLUDED\.etag\)/);
  assert.match(migration, /pricing space version invariant failed/);
});

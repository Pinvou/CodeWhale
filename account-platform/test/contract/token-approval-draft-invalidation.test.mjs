import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = async name => (await readFile(path.join(root, 'migrations/account', name), 'utf8')).replace(/\r\n?/g, '\n');

test('Token draft invalidation migration only adds the reviewed transition and down restores the original guard', async () => {
  const originalMigration = await read('000018_approval_commands.up.sql');
  const original = originalMigration.match(/CREATE FUNCTION protect_submitted_approval_request\(\)[\s\S]*?\n\$\$;/)?.[0];
  assert.ok(original, 'the published immutable guard must remain present');
  const baseline = original.replace('CREATE FUNCTION', 'CREATE OR REPLACE FUNCTION').trim();
  const up = await read('000047_token_approval_draft_invalidation.up.sql');
  const down = await read('000047_token_approval_draft_invalidation.down.sql');
  const restrictedEdge = "\t\tOR (OLD.status='DRAFT' AND NEW.status='INVALIDATED'\n" +
    "\t\t    AND OLD.command_type IN ('TOKEN_MODEL_LIFECYCLE_CHANGE','TOKEN_MODEL_CATALOG_ROLLBACK','TOKEN_CREDENTIAL_REVOKE')\n" +
    "\t\t    AND NEW.invalidation_code IS NOT DISTINCT FROM 'AUTHORITATIVE_SNAPSHOT_CHANGED')\n";
  assert.equal(up.split(restrictedEdge).length, 2, 'exactly one command/reason-scoped state edge');
  assert.equal(up.replace(restrictedEdge, '').trim(), baseline, 'all previous transitions and immutable fields stay identical');
  assert.equal(down.trim(), baseline, 'rollback restores the published guard without deleting history');
  for (const sql of [up, down]) assert.doesNotMatch(sql, /\b(?:DELETE\s+FROM|TRUNCATE|DROP\s+(?:TABLE|TRIGGER))\b/i);
});

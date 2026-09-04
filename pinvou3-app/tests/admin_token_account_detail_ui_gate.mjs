import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import path from 'node:path';

const smoke = path.join(import.meta.dirname, 'admin_token_account_detail_ui_smoke.mjs');
for (let run = 1; run <= 5; run += 1) {
  const result = spawnSync(process.execPath, [smoke], { cwd: path.resolve(import.meta.dirname, '..'), encoding: 'utf8', env: process.env });
  process.stdout.write(result.stdout || '');
  process.stderr.write(result.stderr || '');
  assert.equal(result.status, 0, `Admin Token Account credential isolation smoke failed on consecutive run ${run}`);
}
console.log('Admin Token Account credential isolation smoke passed 5 consecutive runs');

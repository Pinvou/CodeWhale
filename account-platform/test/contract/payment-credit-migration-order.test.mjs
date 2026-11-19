import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import YAML from 'yaml';
import { checkMigrationSuccessOrder } from '../../../scripts/account-platform-business-migrations-check.mjs';
import { workloads } from '../../../scripts/account-platform-production-render.mjs';

for (const file of ['compose/compose.yaml', 'production/compose.personal.yaml']) {
  test(`${file} retains a mandatory successful Billing prerequisite`, () => {
    const config = YAML.parse(readFileSync(new URL(`../../deploy/${file}`, import.meta.url), 'utf8'), { merge: true });
    const render = { kind: 'compose', config };
    checkMigrationSuccessOrder(workloads(render));
    for (const mutate of [
      c => { delete c.services['account-migrate'].depends_on['billing-migrate']; },
      c => { c.services['account-migrate'].depends_on['billing-migrate'].condition = 'service_started'; },
      c => { c.services['account-migrate'].depends_on['billing-migrate'].required = false; },
      c => { c.services['billing-migrate'].depends_on = { ...c.services['billing-migrate'].depends_on, 'account-migrate': { condition: 'service_completed_successfully' } }; },
    ]) {
      const changed = structuredClone(config); mutate(changed);
      assert.throws(() => checkMigrationSuccessOrder(workloads({ kind: 'compose', config: changed })), /production_render_migration_order/);
    }
  });
}

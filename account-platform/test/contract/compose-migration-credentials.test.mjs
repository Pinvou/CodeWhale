import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import YAML from 'yaml';

function assertSimulatorMigrationGate(config) {
  assert.deepEqual(config.services['payment-simulator-migrate'].depends_on, {
    'deployment-credentials-check': { condition: 'service_completed_successfully' },
    postgres: { condition: 'service_healthy' },
  });
}

test('simulator migration validates deployment credentials before using retained database volumes', () => {
  const source = readFileSync(new URL('../../deploy/compose/compose.yaml', import.meta.url), 'utf8');
  const config = YAML.parse(source);
  assertSimulatorMigrationGate(config);
  for (const change of [
    c => { delete c.services['payment-simulator-migrate'].depends_on['deployment-credentials-check']; },
    c => { c.services['payment-simulator-migrate'].depends_on['deployment-credentials-check'].condition = 'service_started'; },
    c => { c.services['payment-simulator-migrate'].depends_on.postgres.condition = 'service_started'; },
  ]) {
    const mutated = structuredClone(config);
    change(mutated);
    assert.throws(() => assertSimulatorMigrationGate(mutated));
  }
});

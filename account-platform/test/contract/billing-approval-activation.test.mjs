import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');

function validator(contract) {
  const document = JSON.parse(JSON.stringify({
    $schema: 'https://json-schema.org/draft/2020-12/schema',
    $ref: '#/$defs/BillingApprovalActivationSnapshot',
    $defs: contract.components.schemas,
  }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
}

const commands = [
  ['CHARGEBACK_FINANCIAL_REPAIR', 1, 'INACTIVE'],
  ['ENTITLEMENT_COMPONENT_ADJUSTMENT', 1, 'INACTIVE'],
  ['LEDGER_REPAIR', 1, 'INACTIVE'],
  ['PAYMENT_FINANCIAL_REPAIR', 1, 'INACTIVE'],
  ['PAYMENT_PRODUCT_RESUME', 1, 'ACTIVE'],
  ['PAYMENT_PROVIDER_RESUME', 2, 'ACTIVE'],
  ['RECEIVABLE_WRITE_OFF', 1, 'INACTIVE'],
  ['RECONCILIATION_FINANCIAL_REPAIR', 1, 'INACTIVE'],
  ['REFUND_EXCEPTION_REPAIR', 1, 'INACTIVE'],
  ['USAGE_SETTLEMENT_REPAIR', 1, 'INACTIVE'],
  ['WALLET_ADJUSTMENT', 1, 'ACTIVE'],
];

function snapshot() {
  return {
    schema_version: 1,
    activations: commands.map(([command_type, policy_version, state]) => ({
      command_type,
      policy_version,
      snapshot_schema_version: 1,
      state,
      version: 1,
      updated_at: '2026-09-14T12:00:00Z',
    })),
  };
}

test('Billing activation OpenAPI accepts only the fixed complete version-bound projection', async () => {
  const contract = YAML.parse(await readFile(path.join(root, 'contracts/openapi/billing.yaml'), 'utf8'));
  const operation = contract.paths['/internal/v1/approval-command-activations']?.get;
  assert.ok(operation);
  assert.equal(operation.responses['200'].headers['Cache-Control'].schema.const, 'no-store');
  for (const status of ['200', '401', '403', '503']) assert.ok(operation.responses[status]);

  const validate = validator(contract);
  const valid = snapshot();
  assert.equal(validate(valid), true, JSON.stringify(validate.errors));
  const allCompatibleInactive = snapshot();
  for (const index of [4, 5, 10]) allCompatibleInactive.activations[index].state = 'INACTIVE';
  assert.equal(validate(allCompatibleInactive), true, JSON.stringify(validate.errors));

  // The entitlement correction command may append exactly one policy v2 /
  // snapshot v2 row while retaining the immutable v1 history; the projection
  // then reports the higher version, which is the only projection of this
  // command that may be activated.
  const versionedCorrection = snapshot();
  versionedCorrection.activations[1].policy_version = 2;
  versionedCorrection.activations[1].snapshot_schema_version = 2;
  assert.equal(validate(versionedCorrection), true, JSON.stringify(validate.errors));
  const activatedCorrection = snapshot();
  activatedCorrection.activations[1].policy_version = 2;
  activatedCorrection.activations[1].snapshot_schema_version = 2;
  activatedCorrection.activations[1].state = 'ACTIVE';
  assert.equal(validate(activatedCorrection), true, JSON.stringify(validate.errors));

  // The ledger repair command owns exactly one v1/s1 projection, and that row is
  // the only projection of the command that may be activated.
  const activatedLedgerRepair = snapshot();
  activatedLedgerRepair.activations[2].state = 'ACTIVE';
  assert.equal(validate(activatedLedgerRepair), true, JSON.stringify(validate.errors));

  for (const [name, mutate] of [
    ['partial', value => value.activations.pop()],
    ['extra', value => value.activations.push({ ...value.activations[10] })],
    ['duplicate', value => { value.activations[1] = { ...value.activations[0] }; }],
    ['wrong-order', value => { [value.activations[0], value.activations[1]] = [value.activations[1], value.activations[0]]; }],
    ['wrong-policy-version', value => { value.activations[5].policy_version = 1; }],
    ['wrong-snapshot-schema', value => { value.activations[10].snapshot_schema_version = 2; }],
    ['policy-only-active', value => { value.activations[0].state = 'ACTIVE'; }],
    ['mixed-ledger-repair-policy', value => { value.activations[2].policy_version = 2; }],
    ['mixed-ledger-repair-schema', value => { value.activations[2].snapshot_schema_version = 2; }],
    ['mixed-correction-version', value => { value.activations[1].policy_version = 2; }],
    ['unknown-correction-version', value => { value.activations[1].policy_version = 3; value.activations[1].snapshot_schema_version = 3; }],
    ['active-correction-v1-history', value => { value.activations[1].state = 'ACTIVE'; }],
    ['unknown-command', value => { value.activations[0].command_type = 'UNKNOWN_COMMAND'; }],
    ['extra-field', value => { value.activations[0].private_fact = 'must-not-escape'; }],
  ]) {
    const invalid = snapshot();
    mutate(invalid);
    assert.equal(validate(invalid), false, `${name} unexpectedly matched the closed snapshot schema`);
  }
});

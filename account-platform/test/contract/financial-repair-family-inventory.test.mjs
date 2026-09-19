import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = (relative) => readFile(path.join(root, relative), 'utf8');

// The seven financial repair commands are delivered across five separately
// maintained layers: the service switch, the consumer durable, the activation
// runbook, the authorization catalog event entry and the recovery
// classification registry. Each one has already drifted at least once in this
// family, so this contract pins them together: a rename or a version bump in
// any single layer fails here instead of at activation time in production.
const family = {
  LEDGER_REPAIR: {
    switch: 'BILLING_LEDGER_REPAIR_CONSUMER_ENABLED',
    durable: 'billing-ledger-repair-executor-p1-v1',
    runbook: 'deploy/runbooks/ledger-repair-activation.md',
  },
  RECEIVABLE_WRITE_OFF: {
    switch: 'BILLING_RECEIVABLE_WRITE_OFF_CONSUMER_ENABLED',
    durable: 'billing-receivable-write-off-executor-p1-v1',
    runbook: 'deploy/runbooks/receivable-write-off-activation.md',
  },
  PAYMENT_FINANCIAL_REPAIR: {
    switch: 'BILLING_PAYMENT_FINANCIAL_REPAIR_CONSUMER_ENABLED',
    durable: 'billing-payment-financial-repair-executor-p1-v1',
    runbook: 'deploy/runbooks/payment-financial-repair-activation.md',
  },
  REFUND_EXCEPTION_REPAIR: {
    switch: 'BILLING_REFUND_EXCEPTION_REPAIR_CONSUMER_ENABLED',
    durable: 'billing-refund-exception-repair-executor-p1-v1',
    runbook: 'deploy/runbooks/refund-exception-repair-activation.md',
  },
  CHARGEBACK_FINANCIAL_REPAIR: {
    switch: 'BILLING_CHARGEBACK_FINANCIAL_REPAIR_CONSUMER_ENABLED',
    durable: 'billing-chargeback-financial-repair-executor-p1-v1',
    runbook: 'deploy/runbooks/chargeback-financial-repair-activation.md',
  },
  RECONCILIATION_FINANCIAL_REPAIR: {
    switch: 'BILLING_RECONCILIATION_FINANCIAL_REPAIR_CONSUMER_ENABLED',
    durable: 'billing-reconciliation-financial-repair-executor-p1-v1',
    runbook: 'deploy/runbooks/reconciliation-financial-repair-activation.md',
  },
  USAGE_SETTLEMENT_REPAIR: {
    switch: 'BILLING_USAGE_SETTLEMENT_REPAIR_CONSUMER_ENABLED',
    durable: 'billing-usage-settlement-repair-executor-p1-v1',
    runbook: 'deploy/runbooks/usage-settlement-repair-activation.md',
  },
};

test('every financial repair command keeps one switch, durable, runbook and event entry', async () => {
  const main = await read('cmd/billing-service/main.go');
  const registry = await read('internal/platform/approvalconsumer/registry.go');
  const catalog = JSON.parse(await read('deploy/authorization-entrypoints.json'));
  const readiness = await read('docs/financial-repair-family-release-readiness.md');
  for (const [command, entry] of Object.entries(family)) {
    assert.ok(main.includes(entry.switch), `${command} must read ${entry.switch}`);
    assert.ok(registry.includes(entry.durable), `${command} must register ${entry.durable}`);
    const runbook = await read(entry.runbook);
    assert.ok(runbook.includes(entry.switch), `${entry.runbook} must name its switch`);
    assert.ok(runbook.includes(entry.durable), `${entry.runbook} must name its durable`);
    // The activation statement is the one production write; a wrong version
    // silently updates zero rows instead of activating the command.
    assert.ok(runbook.includes(`WHERE command_type = '${command}' AND policy_version = 1 AND snapshot_schema_version = 1`),
      `${entry.runbook} must activate exactly the v1/s1 row of ${command}`);
    const events = (catalog.events || []).filter((item) => item.durable === entry.durable);
    assert.equal(events.length, 1, `${entry.durable} must have exactly one catalog event entry`);
    assert.equal(events[0].subject, 'account.approval-command.authorized.v1');
    assert.equal(events[0].permission, 'internal.event.consume.account-approval-command-authorized');
    assert.ok(readiness.includes(entry.durable) && readiness.includes(entry.switch),
      `the family readiness inventory must list ${command}`);
  }
});

test('the recovery classification and activation projection cover the same command set', async () => {
  const recovery = await read('internal/platform/approvalconsumer/recovery.go');
  const activation = await read('internal/billing/approval_activation.go');
  // Every Billing command the recovery worker may ever inspect must have a
  // classification branch at its deployed policy version.
  for (const [command, policyVersion] of Object.entries({
    WALLET_ADJUSTMENT: 1, PAYMENT_PROVIDER_RESUME: 2, PAYMENT_PRODUCT_RESUME: 1,
    ENTITLEMENT_COMPONENT_ADJUSTMENT: 2, ...Object.fromEntries(Object.keys(family).map((command) => [command, 1])),
  })) {
    assert.ok(recovery.includes(`command == "${command}" && policyVersion == ${policyVersion}`),
      `${command} v${policyVersion} must stay classified`);
  }
  for (const command of Object.keys({
    WALLET_ADJUSTMENT: 1, PAYMENT_PROVIDER_RESUME: 1, PAYMENT_PRODUCT_RESUME: 1,
    ENTITLEMENT_COMPONENT_ADJUSTMENT: 1, ...Object.fromEntries(Object.keys(family).map((command) => [command, 1])),
  })) {
    assert.ok(activation.includes(`"${command}"`), `${command} must stay in the activation projection`);
  }
  // The entitlement command owns an appended (2,2) projection while its v1
  // history stays INACTIVE-only; the repair commands stay v1/s1.
  assert.match(activation, /commandType == entitlementComponentAdjustmentCommand && item\.PolicyVersion == 2 && item\.SnapshotSchemaVersion == 2/);
  assert.match(activation, /commandType == ledgerRepairCommand && item\.PolicyVersion == 1 && item\.SnapshotSchemaVersion == 1/);
  assert.match(activation, /commandType == usageSettlementRepairCommand && item\.PolicyVersion == 1 && item\.SnapshotSchemaVersion == 1/);
  const order = activation.slice(activation.indexOf('billingApprovalActivationOrder = []string{'));
  const listed = [...order.slice(0, order.indexOf('}')).matchAll(/"([A-Z_]+)"/g)].map((match) => match[1]).sort();
  assert.deepEqual(listed, ['CHARGEBACK_FINANCIAL_REPAIR', 'ENTITLEMENT_COMPONENT_ADJUSTMENT', 'LEDGER_REPAIR',
    'PAYMENT_FINANCIAL_REPAIR', 'PAYMENT_PRODUCT_RESUME', 'PAYMENT_PROVIDER_RESUME', 'RECEIVABLE_WRITE_OFF',
    'RECONCILIATION_FINANCIAL_REPAIR', 'REFUND_EXCEPTION_REPAIR', 'USAGE_SETTLEMENT_REPAIR', 'WALLET_ADJUSTMENT'],
  'the activation projection order must stay the closed 11-command set');
});

test('the family readiness inventory never claims an activated command', async () => {
  const readiness = await read('docs/financial-repair-family-release-readiness.md');
  assert.match(readiness, /NOT_ACTIVATED/);
  assert.match(readiness, /INACTIVE/);
  // No migration in this family may ship an ACTIVE activation row.
  const { readdirSync } = await import('node:fs');
  for (const [directory, versions] of Object.entries({
    billing: ['000061', '000063', '000064', '000070', '000072', '000074', '000075', '000076'],
    account: ['000071', '000072', '000073', '000074', '000075', '000076', '000077'],
  })) {
    const files = readdirSync(path.join(root, 'migrations', directory));
    for (const version of versions) {
      const matches = files.filter((name) => name.startsWith(version) && name.endsWith('.up.sql'));
      assert.ok(matches.length > 0, `${directory} migration ${version} must exist`);
      for (const name of matches) {
        const sql = await read(`migrations/${directory}/${name}`);
        assert.doesNotMatch(sql, /state\s*=\s*'ACTIVE'/, `${name} must not activate a command`);
      }
    }
  }
});

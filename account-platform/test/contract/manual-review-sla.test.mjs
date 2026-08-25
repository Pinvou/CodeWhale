import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = path => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('all domains persist the same non-resetting manual review SLA projection', async () => {
  const [sla, accountMigration, billingMigration, tokenMigration] = await Promise.all([
    read('internal/platform/manualreview/sla.go'),
    read('migrations/account/000031_manual_review_sla.up.sql'),
    Promise.all([
      read('migrations/billing/000028_late_payment_review_lifecycle.up.sql'),
      read('migrations/billing/000034_manual_review_sla.up.sql'),
    ]).then(parts => parts.join('\n')),
    read('migrations/token/000020_manual_review_sla.up.sql'),
  ]);
  for (const duration of ['15 * time.Minute', '30 * time.Minute', '2 * time.Hour', '4 * time.Hour', '24 * time.Hour']) assert.match(sla, new RegExp(duration.replaceAll('*', '\\*')));
  for (const migration of [accountMigration, billingMigration, tokenMigration]) {
    for (const field of ['severity', 'owner_id', 'claimed_at', 'first_response_at', 'mitigation_due_at', 'resolution_due_at', 'escalation_level', 'assignment_revision']) assert.match(migration, new RegExp(field));
    assert.match(migration, /manual_review_assignments/);
    assert.match(migration, /immutable/);
  }
});

test('registration, closure, payment, usage, route and high-risk command repairs feed the queue', async () => {
  const [account, billing, token] = await Promise.all([
    read('internal/account/manual_reviews.go'),
    read('internal/billing/late_payment_reviews.go'),
    read('internal/token/manual_reviews.go'),
  ]);
  for (const source of ['REGISTRATION_REPAIR', 'ACCOUNT_CLOSURE_REPAIR', 'PROFILE_CHANGE_REPAIR', 'HIGH_RISK_COMMAND_REPAIR']) assert.match(account, new RegExp(source));
  for (const source of ['manual_review_cases', 'payment_provider_controls', 'payment_product_controls', 'STOP_LOSS_EVALUATED']) assert.match(billing, new RegExp(source));
  for (const source of ['USAGE_RECONCILIATION_REVIEW', 'ROUTE_PROVISIONING_REPAIR', "health_status='UNHEALTHY'", 'STOP_LOSS_EVALUATED']) assert.match(token, new RegExp(source));
});

test('production requires an ordered on-call chain and all workers receive it', async () => {
  const [sla, accountMain, billingMain, tokenMain, helm] = await Promise.all([
    read('internal/platform/manualreview/sla.go'),
    read('cmd/account-service/main.go'),
    read('cmd/billing-service/main.go'),
    read('cmd/token-gateway/main.go'),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
  ]);
  assert.match(sla, /MANUAL_REVIEW_ON_CALL_USER_IDS is required in production/);
  for (const main of [accountMain, billingMain, tokenMain]) {
    assert.match(main, /ParseOnCallSchedule/);
    assert.match(main, /NewManualReviewSLAWorker/);
  }
  assert.match(helm, /MANUAL_REVIEW_ON_CALL_USER_IDS/);
});

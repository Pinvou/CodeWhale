import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = path => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('late-payment review lifecycle freezes policy evidence and blocks silent archive', async () => {
  const [migration, reviews, reconciliation] = await Promise.all([
    read('migrations/billing/000028_late_payment_review_lifecycle.up.sql'),
    read('internal/billing/late_payment_reviews.go'),
    read('internal/billing/reconciliation.go'),
  ]);
  for (const field of ['provider_payable_until', 'late_review_boundary_at', 'final_archive_eligible_at', 'last_successful_query_at', 'last_authoritative_unpaid_at', 'archive_status']) assert.match(migration, new RegExp(field));
  assert.match(migration, /UNIQUE\(domain,case_type,resource_type,resource_id\)/);
  assert.match(migration, /late_payment_review_evidence/);
  assert.match(reviews, /ARCHIVED_CONFIRMED_UNPAID/);
  assert.match(reviews, /last_successful_query_at>=final_archive_eligible_at/);
  assert.match(reviews, /last_authoritative_unpaid_at>=final_archive_eligible_at/);
  assert.match(reviews, /USER_PAYMENT_CLAIM/);
  assert.match(reviews, /PROVIDER_BLIND/);
  assert.match(reconciliation, /EscalateLatePaymentReview/);
  assert.match(reconciliation, /SyncReviewedOrder/);
});

test('provider admission and reconciliation degradation are explicit capabilities', async () => {
  const [provider, payments, openapi] = await Promise.all([
    read('internal/platform/payment/provider.go'),
    read('internal/billing/payments.go'),
    read('contracts/openapi/billing.yaml'),
  ]);
  assert.match(provider, /FormalCollectionEligible/);
  assert.match(provider, /OrderHistoryRetentionSeconds/);
  assert.match(provider, /AuthoritativeUnpaidTerminal/);
  assert.match(payments, /QUERY_DEGRADED/);
  assert.match(payments, /payment_provider_controls/);
  for (const field of ['bill_file_import_supported', 'reconciliation_mode', 'formal_collection_admitted']) assert.match(openapi, new RegExp(field));
});

test('operators can append evidence but cannot submit a paid fact', async () => {
  const [service, admin, gateway] = await Promise.all([
    read('internal/billing/service.go'),
    read('contracts/openapi/admin.yaml'),
    read('contracts/openapi/token-gateway.yaml'),
  ]);
  for (const action of ['claim', 'query', 'schedule', 'archive', 'provider-blind']) assert.match(service, new RegExp(`late-payment-reviews/:case_id/${action}`));
  assert.match(admin, /immutable evidence and operator timeline/);
  assert.match(gateway, /payment-claims/);
  assert.doesNotMatch(admin, /mark-paid|paid_status|manual_paid/);
});

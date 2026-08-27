import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = path => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('a single overdue payment case never pauses the provider', async () => {
  const source = await read('internal/billing/late_payment_reviews.go');
  const start = source.indexOf('func (r *PostgresRepository) evaluateManualReviewStopLoss');
  const end = source.indexOf('func (r *PostgresRepository) EvaluateOverduePaymentStopLosses');
  const evaluator = source.slice(start, end);
  assert.ok(start >= 0 && end > start);
  assert.doesNotMatch(evaluator, /INSERT INTO payment_provider_controls/);
  assert.match(evaluator, /provider_paused": false/);
  assert.match(evaluator, /PAYMENT_FACT_CONFLICT/);
  assert.match(evaluator, /ILLEGAL_TRANSITION/);
});

test('payment stop-loss evaluation is independent from notification throttling', async () => {
  const source = await read('internal/billing/late_payment_reviews.go');
  const start = source.indexOf('func (r *PostgresRepository) EvaluateOverduePaymentStopLosses');
  const end = source.indexOf('func (r *PostgresRepository) EscalateArchiveBlindCases');
  const evaluator = source.slice(start, end);
  assert.match(evaluator, /mitigation_due_at<=now\(\)/);
  assert.match(evaluator, /stop_loss_evaluated_at IS NULL/);
  assert.doesNotMatch(evaluator, /last_escalated_at/);
  assert.match(source, /EvaluateOverduePaymentStopLosses\(ctx, 100\)[\s\S]*EscalateOverdueManualReviews/);
});

test('provider health observations are durable, partitioned, deduplicated, and business rejections are excluded', async () => {
  const [migration, health, resilience] = await Promise.all([
    read('migrations/billing/000047_payment_tiered_stop_loss.up.sql'),
    read('internal/billing/provider_health.go'),
    read('internal/billing/provider_resilience.go'),
  ]);
  for (const fact of ['payment_provider_attempts', 'payment_provider_health_observations', 'payment_observation_gaps', 'payment_stop_loss_evaluations', 'payment_stop_loss_evaluation_members', 'payment_legacy_stop_loss_reviews']) assert.match(migration, new RegExp(fact));
  assert.match(migration, /provider_attempt_id TEXT NOT NULL UNIQUE/);
  assert.match(migration, /status='INTENT' AND created_at<=now\(\)-interval '1 minute'/);
  assert.match(migration, /LEGACY_UNCLASSIFIED/);
  assert.match(health, /capability IN \('ORDER_CREATE','ORDER_QUERY','PAYMENT_STATUS_QUERY'\)/);
  assert.match(health, /outcome<>'BUSINESS_REJECTION'/);
  assert.match(health, /len\(lastIDs\) == 5[\s\S]*len\(orders\) >= 3/);
  assert.match(health, /len\(ids\) >= 20[\s\S]*failures\*2 >= len\(ids\)[\s\S]*len\(orders\) >= 5/);
  assert.match(health, /COUNT\(DISTINCT evaluation\.payment_product_id\)>=2[\s\S]*COUNT\(DISTINCT payment\.id\)>=3[\s\S]*COUNT\(DISTINCT payment\.requester_id\)>=2/);
  assert.match(resilience, /beginProviderAttempt[\s\S]*call\(\)[\s\S]*finishProviderAttempt/);
  assert.match(resilience, /BUSINESS_REJECTION/);
});

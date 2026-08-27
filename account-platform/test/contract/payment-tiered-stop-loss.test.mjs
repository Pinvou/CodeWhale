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

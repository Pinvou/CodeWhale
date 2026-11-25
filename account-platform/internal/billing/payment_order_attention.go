package billing

// All fragments below refer to the UPDATE target alias "payment". Attention is
// derived from current owner facts: preserving an old REVIEW_REQUIRED cannot
// distinguish an outstanding review from a principal gap that has recovered.
// This projection neither authorizes nor repeats a financial operation.
const paymentOrderChargebackReviewSQL = `EXISTS(
 SELECT 1 FROM public.chargeback_cases c JOIN public.chargeback_cash_principals principal ON principal.chargeback_case_id=c.id
 WHERE c.payment_order_id=payment.id AND c.status IN ('REVIEW_REQUIRED','CONFIRMED')
 AND (c.status='REVIEW_REQUIRED' OR (principal.fact->>'manual_review_required')::boolean))`

const paymentOrderAttentionSQL = `CASE
 WHEN EXISTS(SELECT 1 FROM public.receivables r WHERE r.payment_order_id=payment.id AND r.outstanding_microcredits>0) THEN 'RECEIVABLE_OPEN'
 WHEN EXISTS(SELECT 1 FROM public.payment_transactions t WHERE t.payment_order_id=payment.id AND (t.status='REVIEW_REQUIRED' OR t.fulfillment_status='REVIEW_REQUIRED'))
  OR EXISTS(SELECT 1 FROM public.refunds r WHERE r.payment_order_id=payment.id AND r.status='MANUAL_REVIEW')
  OR EXISTS(SELECT 1 FROM public.forced_refund_cases f WHERE f.payment_order_id=payment.id AND f.status='REVIEW_REQUIRED')
  OR EXISTS(SELECT 1 FROM public.payment_anomaly_cases a WHERE a.payment_order_id=payment.id AND a.status='OPEN')
  OR ` + paymentOrderChargebackReviewSQL + ` THEN 'REVIEW_REQUIRED'
 WHEN EXISTS(SELECT 1 FROM public.refunds r WHERE r.payment_order_id=payment.id AND r.status='REQUESTED')
  OR (payment.status IN ('CREATED','PENDING') AND payment.close_status IN ('CLOSE_REQUESTED','LOCALLY_CLOSED_EXTERNAL_PENDING')) THEN 'PROCESSING'
 ELSE 'NORMAL' END`

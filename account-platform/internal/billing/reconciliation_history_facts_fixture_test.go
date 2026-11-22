package billing

import (
	"context"
	"strings"
	"time"
)

// The retained 031/032/033 reconciliation tests use the actual pre-900044
// facts query. The current pipeline still persists observations, audits proofs
// and verifies replay in the same transactions.
func listPrePrincipalReconciliationFacts(ctx context.Context, querier reconciliationQuerier, channel string, start, end time.Time, providerID, providerVersion string, externalRows ...[]ProviderBillEntry) ([]ReconciliationInternalFact, error) {
	var supplied []ProviderBillEntry
	if len(externalRows) > 0 {
		supplied = externalRows[0]
	}
	references := reconciliationReversalReferences(supplied)
	rows, err := querier.Query(ctx, `
SELECT 'PAYMENT',transaction.provider_transaction_id,payment.merchant_order_no,payment.id,transaction.amount_fen,transaction.currency,
       jsonb_build_object('payment_order',to_jsonb(payment),'payment_transaction',to_jsonb(transaction)),NULL,
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference IN ('payment-transaction:'||transaction.id::text,'repair-payment-transaction:'||transaction.id::text)),'[]'::jsonb)
FROM payment_transactions transaction JOIN payment_orders payment ON payment.id=transaction.payment_order_id
WHERE transaction.channel=$1 AND transaction.provider_paid_at>=$2 AND transaction.provider_paid_at<$3 AND transaction.status IN ('PAID','LATE_PAID') AND transaction.provider_id=$4 AND transaction.provider_version=$5
UNION ALL
SELECT 'REFUND',btrim(refund.channel_refund_id,$7::text),payment.merchant_order_no,payment.id,refund.amount_fen,payment.currency,
       jsonb_build_object('payment_order_id',payment.id,'payment_transaction_id',refund.payment_transaction_id),to_jsonb(refund),
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference IN ('refund:'||refund.id::text,'refund-repair:'||refund.id::text)),'[]'::jsonb)
FROM refunds refund JOIN payment_orders payment ON payment.id=refund.payment_order_id
WHERE payment.channel=$1 AND ((refund.updated_at>=$2 AND refund.updated_at<$3) OR (btrim(refund.channel_refund_id,$7::text),payment.merchant_order_no) IN (SELECT reference,merchant FROM jsonb_to_recordset($6::jsonb) AS supplied(reference text,merchant text,kind text) WHERE kind='REFUND')) AND refund.status='SUCCEEDED' AND refund.channel_refund_id IS NOT NULL AND payment.provider_id=$4 AND payment.provider_version=$5
UNION ALL
SELECT 'REFUND',btrim(forced.provider_refund_id,$7::text),payment.merchant_order_no,payment.id,forced.amount_fen,payment.currency,
       jsonb_build_object('payment_order_id',payment.id,'payment_transaction_id',forced.payment_transaction_id),to_jsonb(forced),
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference='forced-refund:'||forced.id::text),'[]'::jsonb)
FROM forced_refund_cases forced JOIN payment_orders payment ON payment.id=forced.payment_order_id
WHERE payment.channel=$1 AND ((forced.resolved_at>=$2 AND forced.resolved_at<$3) OR (btrim(forced.provider_refund_id,$7::text),payment.merchant_order_no) IN (SELECT reference,merchant FROM jsonb_to_recordset($6::jsonb) AS supplied(reference text,merchant text,kind text) WHERE kind='REFUND')) AND forced.resolution_code='CONFIRM_FORCED_REFUND' AND payment.provider_id=$4 AND payment.provider_version=$5
UNION ALL
SELECT 'CHARGEBACK',btrim(chargeback.provider_chargeback_id,$7::text),payment.merchant_order_no,payment.id,transaction.amount_fen,transaction.currency,
       jsonb_build_object('payment_order_id',payment.id,'payment_transaction_id',chargeback.payment_transaction_id),to_jsonb(chargeback),
       COALESCE((SELECT jsonb_agg(to_jsonb(entry) ORDER BY entry.created_at,entry.id) FROM journal_entries entry WHERE entry.business_reference IN ('payment-transaction:'||chargeback.payment_transaction_id::text||':chargeback:'||chargeback.provider_chargeback_id,'chargeback-repair:'||chargeback.id::text)),'[]'::jsonb)
FROM chargeback_cases chargeback JOIN payment_orders payment ON payment.id=chargeback.payment_order_id JOIN payment_transactions transaction ON transaction.id=chargeback.payment_transaction_id
WHERE payment.channel=$1 AND ((chargeback.resolved_at>=$2 AND chargeback.resolved_at<$3) OR (btrim(chargeback.provider_chargeback_id,$7::text),payment.merchant_order_no) IN (SELECT reference,merchant FROM jsonb_to_recordset($6::jsonb) AS supplied(reference text,merchant text,kind text) WHERE kind='CHARGEBACK')) AND chargeback.status='CONFIRMED' AND transaction.provider_id=$4 AND transaction.provider_version=$5
UNION ALL
SELECT 'PAYMENT','ORPHAN_PAYMENT_ORDER:'||payment.id::text,payment.merchant_order_no,payment.id,payment.amount_fen,payment.currency,
       jsonb_build_object('payment_order',to_jsonb(payment),'payment_date_verification_required',payment.paid_at IS NULL),NULL,'[]'::jsonb
FROM payment_orders payment
WHERE payment.channel=$1 AND payment.status='PAID' AND payment.fulfillment_status='CREDITED'
  AND (payment.paid_at IS NULL OR (payment.paid_at>=$2 AND payment.paid_at<$3))
  AND payment.provider_id=$4 AND payment.provider_version=$5
  AND NOT EXISTS(SELECT 1 FROM payment_transactions transaction WHERE transaction.payment_order_id=payment.id)
ORDER BY 1,2`, channel, start, end, providerID, providerVersion, references, reconciliationReferenceTrimCharacters)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := []ReconciliationInternalFact{}
	for rows.Next() {
		var fact ReconciliationInternalFact
		if err := rows.Scan(&fact.EntryType, &fact.ProviderReferenceID, &fact.MerchantOrderNo, &fact.PaymentOrderID, &fact.AmountFen, &fact.Currency, &fact.InternalSnapshot, &fact.RefundSnapshot, &fact.LedgerSnapshot); err != nil {
			return nil, err
		}
		// Match the established provider bill/proof identity without rewriting source evidence.
		fact.ProviderReferenceID = strings.TrimSpace(fact.ProviderReferenceID)
		fact.LedgerPresent = len(fact.LedgerSnapshot) > 0 && string(fact.LedgerSnapshot) != "[]"
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return classifyReversalEventDates(ctx, querier, facts, channel, start, end, providerID, providerVersion, supplied)
}

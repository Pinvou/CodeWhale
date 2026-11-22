package billing

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// A disproved review is history, not a financial effect. Only an exact persisted
// DISMISS receipt and absent accounting/repair facts permit a new refund plan.
// Return the complete proof tuples so approval freezes their identities too.
func refundExceptionDisprovedHistory(ctx context.Context, source refundExceptionRepairReader, packageID uuid.UUID) (json.RawMessage, error) {
	repaired, err := entitlementCorrectionHasChargebackRepairHistory(ctx, source, packageID)
	if err != nil {
		return nil, err
	}
	if repaired {
		return nil, ErrApprovalSnapshotMismatch
	}
	var blocked bool
	var snapshot json.RawMessage
	err = source.QueryRow(ctx, `WITH cases AS (
 SELECT 'CHARGEBACK' kind,c.id,c.payment_order_id,c.payment_transaction_id,c.payment_evidence_id,c.wallet_id,c.manual_review_case_id,
 c.status='DISMISSED' AND c.resolution_code='DISMISS' AND c.version>1 AND c.resolved_at IS NOT NULL dismissed,
 c.receivable_id IS NULL AND c.revoked_base_microcredits=0 AND c.revoked_bonus_microcredits=0 no_recorded_effect,
 ARRAY['payment-transaction:'||c.payment_transaction_id::text||':chargeback:'||c.provider_chargeback_id,'chargeback-repair:'||c.id::text] refs,to_jsonb(c) facts
 FROM chargeback_cases c WHERE c.entitlement_package_id=$1
 UNION ALL
 SELECT 'FORCED_REFUND',c.id,c.payment_order_id,c.payment_transaction_id,c.payment_evidence_id,c.wallet_id,c.manual_review_case_id,
 c.status='RESOLVED' AND c.resolution_code='DISMISS' AND c.version>1 AND c.resolved_at IS NOT NULL,
 c.receivable_id IS NULL AND c.revoked_base_microcredits=0 AND c.revoked_bonus_microcredits=0,
 ARRAY['forced-refund:'||c.id::text],to_jsonb(c)
 FROM forced_refund_cases c WHERE c.entitlement_package_id=$1
), proofs AS (
 SELECT c.*,
 a.id IS NOT NULL AND a.anomaly_type=c.kind AND a.status='RESOLVED' AND a.resolution_code='DISMISS'
 AND a.financial_action_status='NONE' AND a.resolved_at IS NOT NULL
 AND a.payment_order_id=c.payment_order_id AND a.payment_transaction_id=c.payment_transaction_id
 AND a.payment_evidence_id=c.payment_evidence_id
 AND ((c.kind='CHARGEBACK' AND a.chargeback_case_id=c.id AND a.forced_refund_case_id IS NULL)
   OR (c.kind='FORCED_REFUND' AND a.forced_refund_case_id=c.id AND a.chargeback_case_id IS NULL))
 AND EXISTS(SELECT 1 FROM payment_anomaly_commands cmd WHERE cmd.payment_anomaly_case_id=a.id AND cmd.action='DISMISS'
   AND cmd.after_snapshot->>'case_id'=a.id::text AND cmd.after_snapshot->>'status'='RESOLVED'
   AND cmd.after_snapshot->>'resolution_code'='DISMISS' AND cmd.after_snapshot->>'financial_action_status'='NONE'
   AND cmd.after_snapshot->>'payment_order_id'=c.payment_order_id::text
   AND cmd.after_snapshot->>'payment_transaction_id'=c.payment_transaction_id::text
   AND cmd.after_snapshot->>'payment_evidence_id'=c.payment_evidence_id::text
   AND ((c.kind='CHARGEBACK' AND cmd.after_snapshot->>'chargeback_case_id'=c.id::text)
     OR (c.kind='FORCED_REFUND' AND cmd.after_snapshot->>'forced_refund_case_id'=c.id::text))) receipt,
 to_jsonb(a) anomaly,
 (SELECT COALESCE(jsonb_agg(to_jsonb(cmd) ORDER BY cmd.id),'[]'::jsonb) FROM payment_anomaly_commands cmd WHERE cmd.payment_anomaly_case_id=a.id AND cmd.action='DISMISS') commands
 FROM cases c LEFT JOIN payment_anomaly_cases a ON a.id=c.manual_review_case_id
)
SELECT EXISTS(SELECT 1 FROM proofs p WHERE NOT COALESCE(p.dismissed AND p.receipt AND p.no_recorded_effect,false)
 OR NOT EXISTS(SELECT 1 FROM entitlement_packages package WHERE package.id=$1 AND package.wallet_id=p.wallet_id
   AND package.payment_order_id=p.payment_order_id AND package.source_payment_transaction_id=p.payment_transaction_id)
 OR EXISTS(SELECT 1 FROM journal_entries j WHERE j.business_reference=ANY(p.refs))
 OR EXISTS(SELECT 1 FROM journal_entries j JOIN ledger_postings posting ON posting.journal_entry_id=j.id
   JOIN entitlement_components component ON component.credit_lot_id=posting.credit_lot_id
   WHERE component.package_id=$1 AND j.entry_type IN ('CHARGEBACK','FORCED_REFUND'))
 OR EXISTS(SELECT 1 FROM receivables r WHERE r.source_reference=ANY(p.refs)
   OR r.entitlement_package_id=$1 OR r.payment_transaction_id=p.payment_transaction_id)),
 COALESCE((SELECT jsonb_agg(jsonb_build_object('case',p.facts,'anomaly',p.anomaly,'commands',p.commands) ORDER BY p.kind,p.id) FROM proofs p),'[]'::jsonb)`, packageID).Scan(&blocked, &snapshot)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, ErrApprovalSnapshotMismatch
	}
	return snapshot, nil
}

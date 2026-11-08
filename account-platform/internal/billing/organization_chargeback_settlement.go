package billing

// A terminal label alone cannot retire a financial review. Both organization
// deletion paths use the same retained source/receipt proof; all other wallet,
// entitlement, hold and outstanding-receivable blockers remain independent.
// A financial commit precedes the modern final review receipt: neither that
// intermediate state nor a terminal label can stand in for the receipt. Legacy
// pre-review rows are admitted only without an attached modern anomaly.
// The query aliases c (chargeback) and w (its wallet) are supplied by the caller.
const organizationChargebackSettledSQL = `EXISTS(SELECT 1 FROM entitlement_packages p JOIN payment_transactions t ON t.id=p.source_payment_transaction_id
   JOIN payment_orders o ON o.id=t.payment_order_id JOIN payment_evidence e ON e.id=c.payment_evidence_id
   WHERE p.id=c.entitlement_package_id AND p.wallet_id=w.id AND p.payment_order_id=c.payment_order_id
     AND t.id=c.payment_transaction_id AND o.id=c.payment_order_id AND o.wallet_id=w.id
     AND e.payment_order_id=o.id AND e.payment_transaction_id=t.id AND e.evidence_type='CHARGEBACK')
 AND (
 (c.status='CONFIRMED' AND (
   (c.version=1 AND c.manual_review_case_id IS NULL AND c.resolution_code IS NULL
     AND NOT EXISTS(SELECT 1 FROM payment_anomaly_cases a WHERE a.chargeback_case_id=c.id)
     AND EXISTS(SELECT 1 FROM journal_entries j WHERE j.entry_type='CHARGEBACK'
       AND j.business_reference='payment-transaction:'||c.payment_transaction_id::text||':chargeback:'||c.provider_chargeback_id))
   OR (c.version>1 AND c.resolved_at IS NOT NULL AND EXISTS(
     SELECT 1 FROM payment_anomaly_cases a JOIN manual_review_cases manual ON manual.id=a.id
     JOIN payment_anomaly_commands command ON command.payment_anomaly_case_id=a.id
     WHERE a.id=c.manual_review_case_id AND a.anomaly_type='CHARGEBACK' AND a.chargeback_case_id=c.id
       AND a.forced_refund_case_id IS NULL AND a.status='RESOLVED' AND a.resolved_at IS NOT NULL
       AND a.financial_action_status='APPLIED' AND manual.resolved_at IS NOT NULL
       AND manual.owner_id=command.actor_id AND command.reason=a.resolution_reason
       AND a.payment_order_id=c.payment_order_id AND a.payment_transaction_id=c.payment_transaction_id
       AND a.payment_evidence_id=c.payment_evidence_id
       AND command.after_snapshot->>'case_id'=a.id::text AND command.after_snapshot->>'status'='RESOLVED'
       AND command.after_snapshot->>'resolution_code'=a.resolution_code
       AND command.after_snapshot->>'owner_id'=command.actor_id::text
       AND command.after_snapshot->>'payment_order_id'=c.payment_order_id::text
       AND command.after_snapshot->>'payment_transaction_id'=c.payment_transaction_id::text
       AND command.after_snapshot->>'payment_evidence_id'=c.payment_evidence_id::text
       AND command.after_snapshot->>'chargeback_case_id'=c.id::text
       AND (
         (c.resolution_code='CONFIRM_CHARGEBACK' AND a.resolution_code='CONFIRM_CHARGEBACK'
           AND manual.status='RESOLVED' AND command.action='CONFIRM_CHARGEBACK'
           AND command.after_snapshot->>'financial_action_status'='APPLIED'
           AND command.after_snapshot->>'version'=a.version::text
           AND EXISTS(SELECT 1 FROM journal_entries j WHERE j.entry_type='CHARGEBACK'
             AND j.business_reference='payment-transaction:'||c.payment_transaction_id::text||':chargeback:'||c.provider_chargeback_id))
         OR (c.resolution_code='CONFIRM_CHARGEBACK_SHORTFALL' AND a.resolution_code='ESCALATE_FINANCIAL_REPAIR'
           AND manual.status='LONG_TERM_PLAN' AND command.action='ESCALATE_FINANCIAL_REPAIR'
           AND command.after_snapshot->>'financial_action_status'='PENDING'
           AND EXISTS(SELECT 1 FROM chargeback_financial_repair_cases repair
             JOIN journal_entries j ON j.id=repair.journal_entry_id
             WHERE repair.chargeback_case_id=c.id AND repair.anomaly_case_id=a.id
               AND repair.wallet_id=w.id AND repair.package_id=c.entitlement_package_id
               AND repair.payment_evidence_id=c.payment_evidence_id AND repair.receivable_id=c.receivable_id
               AND repair.state='EXECUTED' AND j.entry_type='CHARGEBACK'
               AND j.business_reference='chargeback-repair:'||c.id::text))
       )
   ))
 ))
 OR (c.status='DISMISSED' AND c.resolution_code='DISMISS' AND c.version>1 AND c.resolved_at IS NOT NULL
   AND c.receivable_id IS NULL AND c.revoked_base_microcredits=0 AND c.revoked_bonus_microcredits=0
   AND EXISTS(SELECT 1 FROM payment_anomaly_cases a JOIN payment_anomaly_commands command
     ON command.payment_anomaly_case_id=a.id AND command.action='DISMISS'
     WHERE a.id=c.manual_review_case_id AND a.anomaly_type='CHARGEBACK' AND a.chargeback_case_id=c.id
       AND a.forced_refund_case_id IS NULL AND a.status='RESOLVED' AND a.resolution_code='DISMISS'
       AND a.financial_action_status='NONE' AND a.resolved_at IS NOT NULL
       AND a.payment_order_id=c.payment_order_id AND a.payment_transaction_id=c.payment_transaction_id
       AND a.payment_evidence_id=c.payment_evidence_id
       AND command.after_snapshot->>'case_id'=a.id::text AND command.after_snapshot->>'status'='RESOLVED'
       AND command.after_snapshot->>'resolution_code'='DISMISS' AND command.after_snapshot->>'financial_action_status'='NONE'
       AND command.after_snapshot->>'payment_order_id'=c.payment_order_id::text
       AND command.after_snapshot->>'payment_transaction_id'=c.payment_transaction_id::text
       AND command.after_snapshot->>'payment_evidence_id'=c.payment_evidence_id::text
       AND command.after_snapshot->>'chargeback_case_id'=c.id::text)
   AND NOT EXISTS(SELECT 1 FROM journal_entries j WHERE j.business_reference IN
     ('payment-transaction:'||c.payment_transaction_id::text||':chargeback:'||c.provider_chargeback_id,'chargeback-repair:'||c.id::text))
   AND NOT EXISTS(SELECT 1 FROM journal_entries j JOIN ledger_postings posting ON posting.journal_entry_id=j.id
     JOIN entitlement_components component ON component.credit_lot_id=posting.credit_lot_id
     WHERE component.package_id=c.entitlement_package_id AND j.entry_type='CHARGEBACK')
   AND NOT EXISTS(SELECT 1 FROM receivables debt WHERE debt.entitlement_package_id=c.entitlement_package_id
     OR debt.payment_transaction_id=c.payment_transaction_id)
   AND NOT EXISTS(SELECT 1 FROM chargeback_financial_repair_cases repair WHERE repair.chargeback_case_id=c.id))
 )`

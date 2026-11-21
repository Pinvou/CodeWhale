-- Supplier-confirmed cash is independent of both original gross and points.
-- No inferred/backfilled amounts and no financial effects are installed here.
ALTER TABLE chargeback_cases ADD CONSTRAINT chargeback_case_transaction_identity UNIQUE(id,payment_transaction_id);
CREATE INDEX chargeback_cases_active_order_idx ON chargeback_cases(payment_order_id,status,id)
WHERE status IN ('REVIEW_REQUIRED','CONFIRMED');
CREATE TABLE chargeback_principal_evidence (
 id UUID PRIMARY KEY,
 chargeback_case_id UUID NOT NULL REFERENCES chargeback_cases(id),
 payment_transaction_id UUID NOT NULL REFERENCES payment_transactions(id),
 provider_id TEXT NOT NULL,
 provider_version TEXT NOT NULL,
 provider_chargeback_id TEXT NOT NULL,
 provider_transaction_id TEXT NOT NULL,
 amount_fen BIGINT NOT NULL CHECK (amount_fen > 0),
 currency TEXT NOT NULL CHECK (char_length(currency) BETWEEN 1 AND 16),
 supplier_evidence_reference TEXT NOT NULL CHECK (octet_length(supplier_evidence_reference) BETWEEN 1 AND 256),
 payload_digest TEXT NOT NULL CHECK (payload_digest ~ '^[0-9a-f]{64}$'),
 payload_snapshot JSONB NOT NULL CHECK (jsonb_typeof(payload_snapshot)='object'),
 observation_digest TEXT NOT NULL CHECK (observation_digest ~ '^[0-9a-f]{64}$'),
 recorded_by_service TEXT NOT NULL,
 recorded_by_subject TEXT NOT NULL,
 received_at TIMESTAMPTZ NOT NULL,
 FOREIGN KEY(chargeback_case_id,payment_transaction_id) REFERENCES chargeback_cases(id,payment_transaction_id),
 UNIQUE(chargeback_case_id,observation_digest)
);
CREATE INDEX chargeback_principal_evidence_transaction_idx ON chargeback_principal_evidence(payment_transaction_id,chargeback_case_id);
-- Match the bounded per-case UUID keyset page without changing evidence.
CREATE INDEX chargeback_principal_evidence_case_page_idx ON public.chargeback_principal_evidence(chargeback_case_id,id);
CREATE FUNCTION reject_chargeback_principal_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'chargeback principal evidence is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER chargeback_principal_immutable BEFORE UPDATE OR DELETE ON chargeback_principal_evidence
FOR EACH ROW EXECUTE FUNCTION reject_chargeback_principal_mutation();
CREATE TRIGGER chargeback_principal_no_truncate BEFORE TRUNCATE ON chargeback_principal_evidence
FOR EACH STATEMENT EXECUTE FUNCTION reject_chargeback_principal_mutation();

-- Keep all conflicting observations. Never use newest/oldest receipt as cash
-- authority. The transaction-wide check also fences multiple chargeback IDs
-- whose combined claimed principal exceeds the original payment.
CREATE VIEW chargeback_cash_principals AS
SELECT c.id AS chargeback_case_id,
 jsonb_build_object(
  'status',CASE WHEN own.observations=0 THEN 'MISSING'
                WHEN own.amounts<>1 OR all_claims.invalid OR all_claims.total>t.amount_fen::numeric THEN 'CONFLICT'
                ELSE 'CONFIRMED' END,
  'amount_fen',CASE WHEN own.observations>0 AND own.amounts=1 AND NOT all_claims.invalid AND all_claims.total<=t.amount_fen::numeric THEN own.amount::text ELSE NULL END,
  'currency',t.currency,
  'manual_review_required',own.observations=0 OR own.amounts<>1 OR own.amount<>t.amount_fen OR all_claims.invalid OR all_claims.total>t.amount_fen::numeric,
  'observation_count',own.observations,
  'evidence_digest',all_claims.digest
 ) AS fact
FROM chargeback_cases c
JOIN payment_transactions t ON t.id=c.payment_transaction_id
CROSS JOIN LATERAL (
 SELECT count(*) observations,count(DISTINCT e.amount_fen) amounts,min(e.amount_fen) amount
 FROM chargeback_principal_evidence e WHERE e.chargeback_case_id=c.id
) own
CROSS JOIN LATERAL (
 SELECT COALESCE(sum(claim.amount),0) total,
        COALESCE(bool_or(claim.invalid),false) invalid,
        encode(sha256(convert_to(COALESCE(string_agg(claim.digest,',' ORDER BY claim.case_id),''),'UTF8')),'hex') digest
 FROM (
  SELECT e.chargeback_case_id case_id,max(e.amount_fen)::numeric amount,
   count(DISTINCT e.amount_fen)<>1 OR bool_or(
    e.provider_id<>t.provider_id OR e.provider_version<>t.provider_version OR
    e.provider_transaction_id<>t.provider_transaction_id OR e.currency<>t.currency OR
    e.provider_chargeback_id<>peer.provider_chargeback_id OR
    peer.payment_transaction_id<>t.id OR e.amount_fen>t.amount_fen) invalid,
   encode(sha256(convert_to(string_agg(e.observation_digest,',' ORDER BY e.observation_digest),'UTF8')),'hex') digest
  FROM chargeback_principal_evidence e JOIN chargeback_cases peer ON peer.id=e.chargeback_case_id
  WHERE e.payment_transaction_id=t.id
  GROUP BY e.chargeback_case_id
 ) claim
) all_claims;

-- Old cached loss totals were original gross, not independently evidenced cash.
-- Mark them explicitly unknown while leaving all historical financial facts.
UPDATE payment_orders p SET chargeback_summary=p.chargeback_summary||jsonb_build_object(
 'lost_amount_fen',NULL,'verified_lost_amount_fen','0','cash_principal_status','UNVERIFIED',
 'unverified_count',(SELECT count(*) FROM chargeback_cases c WHERE c.payment_order_id=p.id AND c.status IN ('CONFIRMED','REVIEW_REQUIRED'))),
 summary_version=p.summary_version+1,
 attention_summary=CASE WHEN EXISTS(SELECT 1 FROM receivables r WHERE r.payment_order_id=p.id AND r.outstanding_microcredits>0)
  THEN 'RECEIVABLE_OPEN' ELSE 'REVIEW_REQUIRED' END
WHERE EXISTS(SELECT 1 FROM chargeback_cases c WHERE c.payment_order_id=p.id AND c.status IN ('CONFIRMED','REVIEW_REQUIRED'));

ALTER TABLE public.reconciliation_cases DROP CONSTRAINT reconciliation_cases_case_type_check;
ALTER TABLE public.reconciliation_cases ADD CONSTRAINT reconciliation_cases_case_type_check CHECK(case_type IN (
 'PROVIDER_PAID_INTERNAL_MISSING','INTERNAL_PAID_PROVIDER_MISSING','AMOUNT_MISMATCH','DUPLICATE_TRANSACTION',
 'PROVIDER_REFUND_INTERNAL_MISSING','INTERNAL_REFUND_PROVIDER_MISSING','REFUND_AMOUNT_MISMATCH',
 'PROVIDER_CHARGEBACK_INTERNAL_MISSING','INTERNAL_CHARGEBACK_PROVIDER_MISSING','UNSUPPORTED_PROVIDER_ENTRY',
 'PAYMENT_LEDGER_MISSING','REFUND_LEDGER_MISSING','CHARGEBACK_LEDGER_MISSING',
 'PAYMENT_LEDGER_INVALID','REFUND_LEDGER_INVALID','CHARGEBACK_LEDGER_INVALID','PAYMENT_PACKAGE_INVALID',
 'REFUND_DATE_UNVERIFIED','CHARGEBACK_DATE_UNVERIFIED','LEDGER_REPAIR_PROOF_UNAVAILABLE',
 'PAYMENT_TRANSACTION_MISSING','CHARGEBACK_PRINCIPAL_MISSING','CHARGEBACK_PRINCIPAL_CONFLICT'));

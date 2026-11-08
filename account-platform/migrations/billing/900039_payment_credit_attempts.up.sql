CREATE TABLE payment_credit_attempts (
    id UUID PRIMARY KEY,
    payer_user_id UUID NOT NULL,
    payment_order_id UUID NOT NULL REFERENCES payment_orders(id),
    payment_transaction_id UUID REFERENCES payment_transactions(id),
    transaction_digest TEXT NOT NULL CHECK (transaction_digest ~ '^[0-9a-f]{64}$'),
    facts_digest TEXT NOT NULL CHECK (facts_digest ~ '^[0-9a-f]{64}$'),
    fingerprint TEXT NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    state TEXT NOT NULL CHECK (state IN ('PREPARED','APPLIED','ABORTED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    resolved_at TIMESTAMPTZ,
		account_acknowledged_at TIMESTAMPTZ,
		retained_until TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()+interval '3 years',
		legal_hold BOOLEAN NOT NULL DEFAULT false,
		hold_review_at TIMESTAMPTZ,
		CHECK (NOT legal_hold OR hold_review_at IS NOT NULL),
		CHECK (retained_until>=GREATEST(created_at,COALESCE(resolved_at,created_at),COALESCE(account_acknowledged_at,created_at))+interval '3 years'),
		CHECK (state<>'PREPARED' OR account_acknowledged_at IS NULL),
    CHECK ((state='PREPARED')=(resolved_at IS NULL)),
    CHECK (state<>'APPLIED' OR payment_transaction_id IS NOT NULL)
);
CREATE INDEX payment_credit_attempt_order_idx ON payment_credit_attempts(payment_order_id,id);
CREATE INDEX payment_credit_attempt_transaction_idx ON payment_credit_attempts(payment_transaction_id) WHERE state IN ('PREPARED','APPLIED');
CREATE INDEX payment_credit_attempt_retention_idx ON payment_credit_attempts(retained_until,id) WHERE state<>'PREPARED' AND account_acknowledged_at IS NOT NULL AND legal_hold=false;
REVOKE ALL ON payment_credit_attempts FROM PUBLIC;
GRANT SELECT,INSERT,UPDATE,DELETE ON payment_credit_attempts TO billing_service;
COMMENT ON TABLE payment_credit_attempts IS 'Financial evidence and effect tombstones; unresolved attempts never expire. Apply financial retention and legal holds, never ordinary event cleanup.';

CREATE FUNCTION guard_payment_credit_attempt_facts() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    IF NEW.id<>OLD.id OR NEW.payer_user_id<>OLD.payer_user_id OR NEW.payment_order_id<>OLD.payment_order_id
       OR NEW.payment_transaction_id IS DISTINCT FROM OLD.payment_transaction_id
       OR NEW.transaction_digest<>OLD.transaction_digest OR NEW.facts_digest<>OLD.facts_digest
       OR NEW.fingerprint<>OLD.fingerprint OR NEW.created_at<>OLD.created_at
			 OR NEW.retained_until<>OLD.retained_until
			 OR (OLD.account_acknowledged_at IS NOT NULL AND NEW.account_acknowledged_at IS DISTINCT FROM OLD.account_acknowledged_at)
       OR (OLD.state<>'PREPARED' AND (NEW.state<>OLD.state OR NEW.resolved_at IS DISTINCT FROM OLD.resolved_at)) THEN
        RAISE EXCEPTION 'payment credit attempt facts and terminal outcome are immutable' USING ERRCODE='55000';
    END IF;
    IF NEW.state='ABORTED' AND NEW.payment_transaction_id IS NOT NULL AND
       (EXISTS(SELECT 1 FROM public.entitlement_packages WHERE source_payment_transaction_id=NEW.payment_transaction_id)
        OR EXISTS(SELECT 1 FROM public.journal_entries WHERE business_reference IN ('payment-transaction:'||NEW.payment_transaction_id::text,'repair-payment-transaction:'||NEW.payment_transaction_id::text))) THEN
        RAISE EXCEPTION 'payment credit with financial effects cannot be aborted' USING ERRCODE='55000';
    END IF;
		NEW.retained_until:=GREATEST(OLD.retained_until,COALESCE(NEW.resolved_at,OLD.created_at)+interval '3 years',COALESCE(NEW.account_acknowledged_at,OLD.created_at)+interval '3 years');
    RETURN NEW;
END $$;
CREATE TRIGGER payment_credit_attempt_facts BEFORE UPDATE ON payment_credit_attempts
FOR EACH ROW EXECUTE FUNCTION guard_payment_credit_attempt_facts();

CREATE FUNCTION guard_payment_credit_attempt_deletion() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
		IF TG_OP='TRUNCATE' THEN
				RAISE EXCEPTION 'payment credit attempt truncate is forbidden' USING ERRCODE='55000';
		END IF;
		IF OLD.state='PREPARED' OR OLD.account_acknowledged_at IS NULL OR OLD.legal_hold OR OLD.retained_until>clock_timestamp() THEN
				RAISE EXCEPTION 'payment credit attempt retention or financial hold is active' USING ERRCODE='55000';
		END IF;
		RETURN OLD;
END $$;
CREATE TRIGGER payment_credit_attempt_retention BEFORE DELETE ON payment_credit_attempts FOR EACH ROW EXECUTE FUNCTION guard_payment_credit_attempt_deletion();
CREATE TRIGGER payment_credit_attempt_no_truncate BEFORE TRUNCATE ON payment_credit_attempts FOR EACH STATEMENT EXECUTE FUNCTION guard_payment_credit_attempt_deletion();

-- Older Billing writers do not create a prepared admission and therefore
-- cannot silently bypass the interlock during mixed-version operation.
CREATE FUNCTION require_payment_credit_admission() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE target UUID;
BEGIN
    IF NEW.entry_type='RECHARGE' AND NEW.business_reference ~ '^payment-transaction:[0-9a-f-]{36}$' THEN
        target:=substring(NEW.business_reference FROM 21)::uuid;
    ELSIF NEW.entry_type='REPAIR_CREDIT' AND NEW.business_reference ~ '^repair-payment-transaction:[0-9a-f-]{36}$' THEN
        target:=substring(NEW.business_reference FROM 28)::uuid;
    ELSE
        RETURN NEW;
    END IF;
    IF NOT EXISTS(SELECT 1 FROM public.payment_credit_attempts attempt
        JOIN public.payment_transactions transaction ON transaction.id=attempt.payment_transaction_id
        JOIN public.payment_orders payment ON payment.id=attempt.payment_order_id AND payment.id=transaction.payment_order_id
        WHERE attempt.payment_transaction_id=target AND attempt.state='PREPARED'
          AND payment.payer_user_id=attempt.payer_user_id) THEN
        RAISE EXCEPTION 'PAYMENT_CREDIT_ADMISSION_REQUIRED' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER payment_credit_admission_guard BEFORE INSERT ON journal_entries
FOR EACH ROW EXECUTE FUNCTION require_payment_credit_admission();

-- Content-free clearance evidence is committed atomically with each purge.
CREATE TABLE payment_credit_retention_batches (
    id UUID PRIMARY KEY,
    policy_version INTEGER NOT NULL CHECK (policy_version=1),
    purged_record_count INTEGER NOT NULL CHECK (purged_record_count BETWEEN 0 AND 100),
    purged_batch_count INTEGER NOT NULL CHECK (purged_batch_count BETWEEN 0 AND 100),
    range_start TIMESTAMPTZ NOT NULL,
    range_end TIMESTAMPTZ NOT NULL,
    result TEXT NOT NULL CHECK (result='PURGED'),
    completed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    retained_until TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()+interval '3 years',
    legal_hold BOOLEAN NOT NULL DEFAULT false,
    hold_review_at TIMESTAMPTZ,
    CHECK (purged_record_count+purged_batch_count>0),
    CHECK (range_start<=range_end AND range_end<=completed_at),
    CHECK (retained_until>=completed_at+interval '3 years'),
    CHECK (NOT legal_hold OR hold_review_at IS NOT NULL)
);
CREATE INDEX payment_credit_retention_batches_expiry_idx ON payment_credit_retention_batches(retained_until,id) WHERE legal_hold=false;
REVOKE ALL ON payment_credit_retention_batches FROM PUBLIC;
GRANT SELECT,INSERT,UPDATE,DELETE ON payment_credit_retention_batches TO billing_service;
COMMENT ON TABLE payment_credit_retention_batches IS 'Funds admission clearance evidence: policy, time range, counts and result only; no subject, order, provider or deleted content.';
CREATE FUNCTION guard_payment_credit_retention_batches() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    IF TG_OP='TRUNCATE' THEN
        RAISE EXCEPTION 'clearance proof truncate is forbidden' USING ERRCODE='55000';
    ELSIF TG_OP='DELETE' THEN
        IF OLD.legal_hold OR OLD.retained_until>clock_timestamp() THEN
            RAISE EXCEPTION 'clearance proof retention or hold is active' USING ERRCODE='55000';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.id<>OLD.id OR NEW.policy_version<>OLD.policy_version
       OR NEW.purged_record_count<>OLD.purged_record_count OR NEW.purged_batch_count<>OLD.purged_batch_count
       OR NEW.range_start<>OLD.range_start OR NEW.range_end<>OLD.range_end OR NEW.result<>OLD.result
       OR NEW.completed_at<>OLD.completed_at OR NEW.retained_until<>OLD.retained_until THEN
        RAISE EXCEPTION 'clearance proof facts are immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER payment_credit_clearance_facts BEFORE UPDATE OR DELETE ON payment_credit_retention_batches FOR EACH ROW EXECUTE FUNCTION guard_payment_credit_retention_batches();
CREATE TRIGGER payment_credit_clearance_no_truncate BEFORE TRUNCATE ON payment_credit_retention_batches FOR EACH STATEMENT EXECUTE FUNCTION guard_payment_credit_retention_batches();

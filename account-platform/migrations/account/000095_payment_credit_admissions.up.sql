DO $$ BEGIN
    IF session_user <> 'account_migrator' OR current_user <> 'account_service' THEN
        RAISE EXCEPTION '000095 requires account_migrator with MIGRATION_ROLE account_service' USING ERRCODE='42501';
    END IF;
END $$;

CREATE TABLE account_payment_credit_admissions (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id),
    payment_order_id UUID NOT NULL,
    transaction_digest TEXT NOT NULL CHECK (transaction_digest ~ '^[0-9a-f]{64}$'),
    facts_digest TEXT NOT NULL CHECK (facts_digest ~ '^[0-9a-f]{64}$'),
    fingerprint TEXT NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    state TEXT NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING','APPLIED','ABORTED')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    resolved_at TIMESTAMPTZ,
		billing_acknowledged_at TIMESTAMPTZ,
		retained_until TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()+interval '3 years',
		legal_hold BOOLEAN NOT NULL DEFAULT false,
		hold_review_at TIMESTAMPTZ,
		CHECK (NOT legal_hold OR hold_review_at IS NOT NULL),
		CHECK (retained_until>=GREATEST(created_at,COALESCE(resolved_at,created_at),COALESCE(billing_acknowledged_at,created_at))+interval '3 years'),
		CHECK (state<>'PENDING' OR billing_acknowledged_at IS NULL),
    CHECK ((state='PENDING')=(resolved_at IS NULL))
);
CREATE INDEX account_payment_credit_pending_user_idx ON account_payment_credit_admissions(user_id,id) WHERE state='PENDING';
CREATE INDEX account_payment_credit_recovery_idx ON account_payment_credit_admissions(next_attempt_at,id) WHERE billing_acknowledged_at IS NULL;
CREATE INDEX account_payment_credit_retention_idx ON account_payment_credit_admissions(retained_until,id) WHERE state<>'PENDING' AND billing_acknowledged_at IS NOT NULL AND legal_hold=false;
COMMENT ON TABLE account_payment_credit_admissions IS 'Financial admission evidence; unresolved outcomes never expire. Apply financial retention and legal holds, never ordinary event cleanup.';

CREATE FUNCTION guard_payment_credit_admission_facts() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    IF NEW.id<>OLD.id OR NEW.user_id<>OLD.user_id OR NEW.payment_order_id<>OLD.payment_order_id
       OR NEW.transaction_digest<>OLD.transaction_digest OR NEW.facts_digest<>OLD.facts_digest
       OR NEW.fingerprint<>OLD.fingerprint OR NEW.created_at<>OLD.created_at
			 OR NEW.retained_until<>OLD.retained_until
			 OR (OLD.billing_acknowledged_at IS NOT NULL AND NEW.billing_acknowledged_at IS DISTINCT FROM OLD.billing_acknowledged_at)
       OR (OLD.state<>'PENDING' AND (NEW.state<>OLD.state OR NEW.resolved_at IS DISTINCT FROM OLD.resolved_at)) THEN
        RAISE EXCEPTION 'payment credit admission facts and resolved outcome are immutable' USING ERRCODE='55000';
    END IF;
		-- Retain the final financial audit outcome/ack for its own full term.
		-- Callers cannot shorten or arbitrarily extend this timestamp.
		NEW.retained_until:=GREATEST(OLD.retained_until,COALESCE(NEW.resolved_at,OLD.created_at)+interval '3 years',COALESCE(NEW.billing_acknowledged_at,OLD.created_at)+interval '3 years');
    RETURN NEW;
END $$;
CREATE TRIGGER payment_credit_admission_facts BEFORE UPDATE ON account_payment_credit_admissions
FOR EACH ROW EXECUTE FUNCTION guard_payment_credit_admission_facts();

CREATE FUNCTION guard_payment_credit_admission_deletion() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
		IF TG_OP='TRUNCATE' THEN
				RAISE EXCEPTION 'payment credit admission truncate is forbidden' USING ERRCODE='55000';
		END IF;
		IF OLD.state='PENDING' OR OLD.billing_acknowledged_at IS NULL OR OLD.legal_hold OR OLD.retained_until>clock_timestamp() THEN
				RAISE EXCEPTION 'payment credit admission retention or financial hold is active' USING ERRCODE='55000';
		END IF;
		RETURN OLD;
END $$;
CREATE TRIGGER payment_credit_admission_retention BEFORE DELETE ON account_payment_credit_admissions FOR EACH ROW EXECUTE FUNCTION guard_payment_credit_admission_deletion();
CREATE TRIGGER payment_credit_admission_no_truncate BEFORE TRUNCATE ON account_payment_credit_admissions FOR EACH STATEMENT EXECUTE FUNCTION guard_payment_credit_admission_deletion();

-- The existing worker locks closure then user. Also fence older application
-- instances and direct runtime transitions at the actual database boundary.
CREATE FUNCTION guard_identity_cleanup_payment_credits() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    IF NEW.stage='IDENTITY_CLEANUP' AND OLD.stage<>NEW.stage THEN
        PERFORM id FROM public.users WHERE id=NEW.user_id FOR UPDATE;
        IF EXISTS(SELECT 1 FROM public.account_payment_credit_admissions WHERE user_id=NEW.user_id AND state='PENDING') THEN
            RAISE EXCEPTION 'PAYMENT_CREDIT_OUTCOME_PENDING' USING ERRCODE='55000';
        END IF;
    END IF;
    RETURN NEW;
END $$;
-- Install the closure-table trigger in 900015, after the optional historical
-- closure lineage. This ordinary migration must also upgrade databases that
-- have never installed account_closure_requests.

CREATE FUNCTION guard_subject_cleanup_payment_credits() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
		IF ((NEW.status='DELETED' AND OLD.status<>'DELETED') OR
				(NEW.anonymized_at IS NOT NULL AND OLD.anonymized_at IS NULL)) AND
				EXISTS(SELECT 1 FROM public.account_payment_credit_admissions WHERE user_id=NEW.id AND state='PENDING') THEN
				RAISE EXCEPTION 'PAYMENT_CREDIT_OUTCOME_PENDING' USING ERRCODE='55000';
		END IF;
		RETURN NEW;
END $$;
-- The anonymized_at column is also introduced by the closure lineage. Its
-- subject cleanup trigger is installed together with the stage fence in 900015.

-- Content-free clearance evidence is committed atomically with each purge.
CREATE TABLE account_payment_credit_retention_batches (
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
CREATE INDEX account_payment_credit_retention_batches_expiry_idx ON account_payment_credit_retention_batches(retained_until,id) WHERE legal_hold=false;
REVOKE ALL ON account_payment_credit_retention_batches FROM PUBLIC;
GRANT SELECT,INSERT,UPDATE,DELETE ON account_payment_credit_retention_batches TO account_service;
COMMENT ON TABLE account_payment_credit_retention_batches IS 'Funds admission clearance evidence: policy, time range, counts and result only; no subject, order, provider or deleted content.';
CREATE FUNCTION guard_account_payment_credit_retention_batches() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
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
CREATE TRIGGER payment_credit_clearance_facts BEFORE UPDATE OR DELETE ON account_payment_credit_retention_batches FOR EACH ROW EXECUTE FUNCTION guard_account_payment_credit_retention_batches();
CREATE TRIGGER payment_credit_clearance_no_truncate BEFORE TRUNCATE ON account_payment_credit_retention_batches FOR EACH STATEMENT EXECUTE FUNCTION guard_account_payment_credit_retention_batches();

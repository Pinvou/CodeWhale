CREATE TABLE usage_hold_expiry_recovery (
 hold_id UUID PRIMARY KEY REFERENCES usage_holds(id),
 state TEXT NOT NULL CHECK(state IN ('MANUAL_REVIEW','COMPLETED')),
 identity JSONB NOT NULL,
 proof JSONB,
 next_attempt_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE usage_hold_expiry_evidence (
 id UUID PRIMARY KEY,
 hold_id UUID NOT NULL REFERENCES usage_holds(id),
 decision TEXT NOT NULL CHECK(decision IN ('DISPATCH_UNPROVEN','CONFIRMED')),
 proof JSONB,
 occurred_at TIMESTAMPTZ NOT NULL
);
CREATE TRIGGER usage_hold_expiry_evidence_immutable BEFORE UPDATE OR DELETE ON usage_hold_expiry_evidence FOR EACH ROW EXECUTE FUNCTION reject_usage_hold_evidence_mutation();
CREATE INDEX usage_holds_expired_active_idx ON usage_holds(expires_at,id) WHERE status IN ('ACTIVE','REVIEW');
CREATE INDEX usage_hold_expiry_recovery_pending_idx ON usage_hold_expiry_recovery(next_attempt_at,hold_id) WHERE state<>'COMPLETED';
CREATE FUNCTION public.guard_hold_expiry_identity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.identity IS DISTINCT FROM OLD.identity OR NEW.hold_id IS DISTINCT FROM OLD.hold_id THEN
  RAISE EXCEPTION 'hold recovery identity is immutable' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER usage_hold_expiry_identity_immutable BEFORE UPDATE ON usage_hold_expiry_recovery FOR EACH ROW EXECUTE FUNCTION public.guard_hold_expiry_identity();
CREATE TRIGGER usage_hold_expiry_recovery_no_delete BEFORE DELETE ON usage_hold_expiry_recovery FOR EACH ROW EXECUTE FUNCTION reject_usage_hold_evidence_mutation();
CREATE FUNCTION public.guard_expired_hold_renewal() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF OLD.expires_at<=clock_timestamp() AND NEW.expires_at>OLD.expires_at THEN
  RAISE EXCEPTION 'expired hold cannot be renewed during recovery' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER usage_hold_expiry_renewal_fence BEFORE UPDATE OF expires_at ON usage_holds FOR EACH ROW EXECUTE FUNCTION public.guard_expired_hold_renewal();

DO $$ BEGIN
    IF session_user <> 'account_migrator' OR current_user <> 'account_service' THEN
        RAISE EXCEPTION '900016 requires account_migrator with MIGRATION_ROLE account_service' USING ERRCODE='42501';
    END IF;
END $$;

-- This additive closure lineage follows 000027/000028/900005 even when those
-- migrations are installed after the ordinary Account predecessor delivery.
-- The admission table and function remain in 000095; never conditionally omit
-- this final database fence from a full installation.
CREATE TRIGGER account_closure_payment_credit_guard BEFORE UPDATE OF stage ON account_closure_requests
FOR EACH ROW EXECUTE FUNCTION guard_identity_cleanup_payment_credits();

-- users.anonymized_at is introduced by the same closure lineage.
CREATE TRIGGER subject_cleanup_payment_credit_guard BEFORE UPDATE OF status,anonymized_at ON users
FOR EACH ROW EXECUTE FUNCTION guard_subject_cleanup_payment_credits();

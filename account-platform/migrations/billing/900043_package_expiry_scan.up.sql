-- Match the cross-wallet expiry worker's complete ordering, without changing
-- REVIEW exclusions, wallet-first locking or any posted financial facts.
DO $$ BEGIN
 IF session_user <> 'billing_migrator' OR current_user <> 'billing_service'
    OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
  RAISE EXCEPTION 'package expiry index requires isolated deployment identities' USING ERRCODE='42501';
 END IF;
END $$;
CREATE INDEX entitlement_packages_expiry_scan_idx
 ON public.entitlement_packages (valid_until,credited_at,id)
 INCLUDE (wallet_id) WHERE status='ACTIVE';

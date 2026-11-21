-- Current accepted-query authority is independent from historical timestamps.
-- Existing rows remain unknown; only a real new query can establish authority.
DO $$ BEGIN
 IF session_user <> 'billing_migrator' OR current_user <> 'billing_service'
    OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
  RAISE EXCEPTION 'late payment query admission requires isolated deployment identities' USING ERRCODE='42501';
 END IF;
END $$;
ALTER TABLE public.payment_orders
 ADD COLUMN archive_query_generation UUID,
 ADD COLUMN archive_query_authoritative_unpaid BOOLEAN,
 ADD CONSTRAINT payment_orders_archive_query_generation_check
 CHECK (archive_query_authoritative_unpaid IS NULL OR archive_query_generation IS NOT NULL);

-- Preserve the original per-transaction repair facts; this lookup is deliberately non-unique.
DO $$ BEGIN
 IF session_user <> 'billing_migrator' OR current_user <> 'billing_service'
    OR pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
  RAISE EXCEPTION 'payment repair lookup requires isolated deployment identities' USING ERRCODE='42501';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=900016) THEN
  RAISE EXCEPTION 'payment repair lookup requires the installed execution capability' USING ERRCODE='55000';
 END IF;
END $$;
CREATE INDEX payment_order_repairs_order_lookup_idx ON public.payment_order_repairs(payment_order_id);

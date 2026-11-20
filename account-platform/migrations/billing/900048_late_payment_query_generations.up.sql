-- Append immutable command-to-observation identity without changing published
-- command rows, receipts, historical timestamps or their 900030 trigger.
DO $$ BEGIN
 IF session_user <> 'billing_migrator' OR current_user <> 'billing_service'
    OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
  RAISE EXCEPTION 'late payment query generations require isolated deployment identities' USING ERRCODE='42501';
 END IF;
 IF (SELECT count(*) FROM pg_catalog.pg_attribute
     WHERE attrelid=pg_catalog.to_regclass('public.payment_orders') AND NOT attisdropped
       AND ((attname='archive_query_generation' AND atttypid='uuid'::regtype)
         OR (attname='archive_query_authoritative_unpaid' AND atttypid='bool'::regtype))) <> 2 THEN
  RAISE EXCEPTION 'query command generations require installed query admission facts' USING ERRCODE='23514';
 END IF;
END $$;
SET ROLE billing_schema_owner;
CREATE TABLE public.late_payment_query_generations (
    idempotency_key TEXT PRIMARY KEY REFERENCES public.late_payment_query_commands(idempotency_key),
    generation UUID NOT NULL UNIQUE CHECK(generation <> '00000000-0000-0000-0000-000000000000'::uuid)
);
CREATE FUNCTION public.guard_late_payment_query_generation() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 RAISE EXCEPTION 'query command generation bindings are immutable';
END $$;
CREATE TRIGGER late_payment_query_generation_immutable BEFORE UPDATE OR DELETE ON public.late_payment_query_generations
 FOR EACH ROW EXECUTE FUNCTION public.guard_late_payment_query_generation();
CREATE TRIGGER late_payment_query_generation_no_truncate BEFORE TRUNCATE ON public.late_payment_query_generations
 FOR EACH STATEMENT EXECUTE FUNCTION public.guard_late_payment_query_generation();
-- Old admitted rows remain untouched. Any new writer must commit its binding
-- and current unknown fact atomically before it can receive a dispatch token.
CREATE FUNCTION public.require_late_payment_query_generation() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM public.late_payment_query_generations binding
               JOIN public.payment_orders payment ON payment.id=NEW.payment_order_id
               WHERE binding.idempotency_key=NEW.idempotency_key
                 AND binding.generation=payment.archive_query_generation
                 AND payment.archive_query_authoritative_unpaid IS NULL) THEN
  RAISE EXCEPTION 'new query command requires its current unknown generation' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER late_payment_query_generation_required AFTER INSERT ON public.late_payment_query_commands
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.require_late_payment_query_generation();
REVOKE ALL ON public.late_payment_query_generations FROM PUBLIC,billing_service;
GRANT SELECT,INSERT ON public.late_payment_query_generations TO billing_service;
REVOKE ALL ON FUNCTION public.guard_late_payment_query_generation() FROM PUBLIC;
REVOKE ALL ON FUNCTION public.require_late_payment_query_generation() FROM PUBLIC;
SET ROLE billing_service;

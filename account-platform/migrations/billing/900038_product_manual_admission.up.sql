-- Manual reviews are append-only facts; they do not change approval governance.
CREATE TABLE public.product_manual_admission_submissions (
 verification_id uuid PRIMARY KEY REFERENCES public.product_admission_verifications(verification_id),
 catalog_item_id uuid NOT NULL REFERENCES public.recharge_product_catalog_items(catalog_item_id),
 material_id uuid NOT NULL UNIQUE REFERENCES public.product_admission_materials(material_id),
 submission_sha text NOT NULL CHECK(submission_sha ~ '^[0-9a-f]{64}$'),
 snapshot jsonb NOT NULL CHECK(jsonb_typeof(snapshot)='object'),
 requested_by uuid NOT NULL, requested_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL CHECK(expires_at>requested_at)
);
CREATE TABLE public.product_manual_admission_decisions (
 verification_id uuid NOT NULL REFERENCES public.product_manual_admission_submissions(verification_id),
 reviewer_id uuid NOT NULL,
 decision text NOT NULL CHECK(decision IN ('APPROVE','REJECT')),
 reason text NOT NULL CHECK(length(btrim(reason)) BETWEEN 1 AND 2000),
 decided_at timestamptz NOT NULL,
 PRIMARY KEY(verification_id,reviewer_id)
);
REVOKE ALL ON public.product_manual_admission_submissions,public.product_manual_admission_decisions FROM PUBLIC;
GRANT SELECT,INSERT ON public.product_manual_admission_submissions,public.product_manual_admission_decisions TO billing_service;

-- This trigger is only reachable after the material guard proved expiry,
-- absence of a hold and successful ciphertext deletion. It has no caller API.
CREATE FUNCTION public.purge_product_manual_admission_facts() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE target uuid;
BEGIN
 IF TG_TABLE_SCHEMA<>'public' OR TG_TABLE_NAME<>'product_admission_materials' OR TG_OP<>'UPDATE' THEN RAISE EXCEPTION 'invalid retention invocation'; END IF;
 IF OLD.purged_at IS NOT NULL OR NEW.purged_at IS NULL THEN RETURN NEW; END IF;
 IF NEW.legal_hold OR NEW.purge_after>clock_timestamp() OR EXISTS(SELECT 1 FROM public.product_admission_material_ciphertexts WHERE material_id=NEW.material_id) THEN RAISE EXCEPTION 'original retention not satisfied'; END IF;
 target:=NEW.verification_id;
 UPDATE public.product_manual_admission_submissions SET snapshot='{}'::jsonb WHERE verification_id=target;
 UPDATE public.product_manual_admission_decisions SET reason='REMOVED_AFTER_RETENTION' WHERE verification_id=target;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION public.purge_product_manual_admission_facts() FROM PUBLIC;
CREATE TRIGGER product_manual_admission_retention AFTER UPDATE OF purged_at ON public.product_admission_materials FOR EACH ROW EXECUTE FUNCTION public.purge_product_manual_admission_facts();

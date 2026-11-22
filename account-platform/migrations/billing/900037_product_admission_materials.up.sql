-- Product-admission originals only; envelope binding metadata is immutable.
CREATE TABLE public.product_admission_materials (
 material_id uuid PRIMARY KEY,
 catalog_item_id uuid NOT NULL REFERENCES public.recharge_product_catalog_items(catalog_item_id),
 catalog_version bigint NOT NULL CHECK(catalog_version>0), uploaded_by uuid NOT NULL,
 media_type text NOT NULL CHECK(media_type IN ('image/png','image/jpeg','application/pdf')),
 size bigint NOT NULL CHECK(size BETWEEN 1 AND 10485760), sha256 text NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 intended_expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 terminal_at timestamptz, purge_after timestamptz NOT NULL,
 purged_at timestamptz, version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 legal_hold boolean NOT NULL DEFAULT false, hold_review_at timestamptz,
 verification_id uuid UNIQUE,
 CHECK(intended_expires_at>created_at), CHECK(NOT legal_hold OR hold_review_at IS NOT NULL)
);
CREATE INDEX product_admission_material_purge ON public.product_admission_materials(purge_after) WHERE purged_at IS NULL AND NOT legal_hold;
CREATE TABLE public.product_admission_material_ciphertexts (
 material_id uuid PRIMARY KEY REFERENCES public.product_admission_materials(material_id),
 key_id text NOT NULL CHECK(length(key_id) BETWEEN 1 AND 128),
 wrapped_dek bytea NOT NULL CHECK(octet_length(wrapped_dek) BETWEEN 1 AND 65536),
 nonce bytea NOT NULL CHECK(octet_length(nonce)=12),
 ciphertext bytea NOT NULL CHECK(octet_length(ciphertext) BETWEEN 17 AND 10485776)
);
CREATE TABLE public.product_admission_material_commands (
 actor uuid NOT NULL, operation_key text NOT NULL CHECK(length(operation_key) BETWEEN 16 AND 128),
 request_hash text NOT NULL, catalog_item_id uuid NOT NULL,
 response jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(actor,operation_key)
);
CREATE TABLE public.product_admission_material_audits (
 audit_id uuid PRIMARY KEY, material_id uuid NOT NULL REFERENCES public.product_admission_materials(material_id),
 actor uuid NOT NULL, action text NOT NULL CHECK(action IN ('UPLOAD','READ','HOLD','RELEASE_HOLD','PURGE')),
 reason text NOT NULL, review_at timestamptz, occurred_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION public.guard_product_admission_material() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  NEW.purge_after := (NEW.intended_expires_at AT TIME ZONE 'UTC' + interval '3 years') AT TIME ZONE 'UTC';
  IF NEW.terminal_at IS NOT NULL OR NEW.purged_at IS NOT NULL OR NEW.verification_id IS NOT NULL OR NEW.legal_hold OR NEW.version<>1 THEN RAISE EXCEPTION 'invalid initial material state'; END IF;
  RETURN NEW;
 END IF;
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'material metadata cannot be deleted'; END IF;
 IF ROW(NEW.material_id,NEW.catalog_item_id,NEW.catalog_version,NEW.uploaded_by,NEW.media_type,NEW.size,NEW.sha256,NEW.intended_expires_at,NEW.created_at)
 IS DISTINCT FROM ROW(OLD.material_id,OLD.catalog_item_id,OLD.catalog_version,OLD.uploaded_by,OLD.media_type,OLD.size,OLD.sha256,OLD.intended_expires_at,OLD.created_at)
 OR (OLD.verification_id IS NOT NULL AND NEW.verification_id IS DISTINCT FROM OLD.verification_id)
 OR (OLD.terminal_at IS NOT NULL AND NEW.terminal_at IS DISTINCT FROM OLD.terminal_at)
 OR (OLD.purged_at IS NOT NULL AND NEW.purged_at IS DISTINCT FROM OLD.purged_at) THEN RAISE EXCEPTION 'material facts are immutable'; END IF;
 IF NEW.terminal_at IS DISTINCT FROM OLD.terminal_at AND ((NEW.terminal_at<statement_timestamp()-interval '1 minute' AND NEW.terminal_at<>NEW.intended_expires_at) OR NEW.terminal_at>clock_timestamp()) THEN RAISE EXCEPTION 'invalid terminal anchor'; END IF;
 IF NEW.purged_at IS DISTINCT FROM OLD.purged_at AND (NEW.legal_hold OR OLD.purge_after>clock_timestamp() OR EXISTS(SELECT 1 FROM public.product_admission_material_ciphertexts WHERE material_id=NEW.material_id)) THEN RAISE EXCEPTION 'original has not been safely purged'; END IF;
 NEW.purge_after := (LEAST(NEW.terminal_at,NEW.intended_expires_at) AT TIME ZONE 'UTC' + interval '3 years') AT TIME ZONE 'UTC';
 RETURN NEW;
END $$;
CREATE TRIGGER product_admission_material_guard BEFORE INSERT OR UPDATE OR DELETE ON public.product_admission_materials FOR EACH ROW EXECUTE FUNCTION public.guard_product_admission_material();
CREATE FUNCTION public.guard_product_admission_ciphertext() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  IF NOT EXISTS(SELECT 1 FROM public.product_admission_materials WHERE material_id=NEW.material_id AND purged_at IS NULL AND octet_length(NEW.ciphertext)=size+16) THEN RAISE EXCEPTION 'original owner is invalid'; END IF;
  RETURN NEW;
 END IF;
 IF TG_OP='UPDATE' OR NOT EXISTS(SELECT 1 FROM public.product_admission_materials WHERE material_id=OLD.material_id AND NOT legal_hold AND purged_at IS NULL AND purge_after<=clock_timestamp() FOR UPDATE) THEN RAISE EXCEPTION 'original cannot be changed or prematurely purged'; END IF;
 RETURN OLD;
END $$;
CREATE TRIGGER product_admission_ciphertext_guard BEFORE INSERT OR UPDATE OR DELETE ON public.product_admission_material_ciphertexts FOR EACH ROW EXECUTE FUNCTION public.guard_product_admission_ciphertext();

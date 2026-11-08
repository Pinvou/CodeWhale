SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000056 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000056 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000056 schema owner must remain separate from runtime';
    END IF;
    IF (SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
        WHERE oid='public.entitlement_components'::regclass) <> 'billing_service' THEN
        RAISE EXCEPTION '000056 legacy entitlement table owner must be billing_service';
    END IF;
END $$;

ALTER TABLE public.entitlement_components
    ADD COLUMN reclassified_out_microcredits BIGINT NOT NULL DEFAULT 0
    CHECK (reclassified_out_microcredits >= 0);
ALTER TABLE public.entitlement_components
    ADD CONSTRAINT entitlement_components_correction_inactive
    CHECK (reclassified_out_microcredits = 0);

GRANT REFERENCES ON public.entitlement_packages,public.wallets,public.payment_orders,
    public.payment_transactions,public.entitlement_components,public.credit_lots,
    public.journal_entries TO billing_schema_owner;

SET ROLE billing_schema_owner;

-- The old conservation, (package_id,component_type) uniqueness, and explicit
-- zero-only gate remain in force. No correction can be written by this stage.
CREATE TABLE public.entitlement_component_corrections (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL UNIQUE,
    authorization_event_id UUID NOT NULL UNIQUE,
    approval_epoch BIGINT NOT NULL CHECK (approval_epoch > 0),
    execution_key_digest TEXT NOT NULL UNIQUE CHECK (execution_key_digest ~ '^[0-9a-f]{64}$'),
    package_id UUID NOT NULL UNIQUE REFERENCES public.entitlement_packages(id),
    wallet_id UUID NOT NULL REFERENCES public.wallets(id),
    payment_order_id UUID NOT NULL REFERENCES public.payment_orders(id),
    payment_transaction_id UUID NOT NULL REFERENCES public.payment_transactions(id),
    source_component_id UUID NOT NULL REFERENCES public.entitlement_components(id),
    source_lot_id UUID NOT NULL REFERENCES public.credit_lots(id),
    target_component_id UUID NOT NULL UNIQUE REFERENCES public.entitlement_components(id),
    target_lot_id UUID NOT NULL UNIQUE REFERENCES public.credit_lots(id),
    source_type TEXT NOT NULL CHECK (source_type IN ('BASE','BONUS')),
    target_type TEXT NOT NULL CHECK (target_type IN ('BASE','BONUS') AND target_type <> source_type),
    amount_microcredits BIGINT NOT NULL CHECK (amount_microcredits > 0),
    reason_code TEXT NOT NULL CHECK (char_length(reason_code) BETWEEN 1 AND 80),
    source_evidence_digest TEXT NOT NULL CHECK (source_evidence_digest ~ '^[0-9a-f]{64}$'),
    facts_digest TEXT NOT NULL CHECK (facts_digest ~ '^[0-9a-f]{64}$'),
    journal_entry_id UUID NOT NULL UNIQUE REFERENCES public.journal_entries(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CHECK (source_component_id <> target_component_id),
    CHECK (source_lot_id <> target_lot_id)
);
CREATE FUNCTION public.reject_entitlement_component_correction_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'entitlement component correction is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER entitlement_component_corrections_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.entitlement_component_corrections
FOR EACH STATEMENT EXECUTE FUNCTION public.reject_entitlement_component_correction_mutation();
REVOKE ALL ON public.entitlement_component_corrections FROM PUBLIC;
GRANT SELECT ON public.entitlement_component_corrections TO billing_service;

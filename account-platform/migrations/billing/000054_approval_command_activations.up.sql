DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000054 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='billing_service')
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='billing_schema_owner' AND NOT rolcanlogin)
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='billing_migrator' AND rolcanlogin AND NOT rolinherit) THEN
        RAISE EXCEPTION '000054 deployment roles must be provisioned before migration';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000054 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF session_user='billing_migrator' AND NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION 'billing_migrator must be a member of billing_schema_owner';
    END IF;
    IF pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION 'billing runtime must not be a member of billing_schema_owner';
    END IF;
END $$;

SET ROLE billing_schema_owner;
CREATE TABLE public.billing_approval_command_activations (
    command_type TEXT NOT NULL CHECK(command_type ~ '^[A-Z][A-Z0-9_]{0,119}$'),
    policy_version BIGINT NOT NULL CHECK(policy_version>0),
    snapshot_schema_version INTEGER NOT NULL CHECK(snapshot_schema_version>0),
    state TEXT NOT NULL CHECK(state IN ('ACTIVE','INACTIVE')),
    version BIGINT NOT NULL CHECK(version>0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(command_type,policy_version)
);

CREATE TABLE public.billing_approval_command_activation_history (
    command_type TEXT NOT NULL CHECK(command_type ~ '^[A-Z][A-Z0-9_]{0,119}$'),
    policy_version BIGINT NOT NULL CHECK(policy_version>0),
    snapshot_schema_version INTEGER NOT NULL CHECK(snapshot_schema_version>0),
    state TEXT NOT NULL CHECK(state IN ('ACTIVE','INACTIVE')),
    version BIGINT NOT NULL CHECK(version>0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(command_type,policy_version,version)
);

CREATE FUNCTION public.enforce_billing_approval_activation_update() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'billing approval activation current projection cannot be deleted' USING ERRCODE='55000';
    END IF;
    IF NEW.command_type<>OLD.command_type
       OR NEW.policy_version<>OLD.policy_version
       OR NEW.snapshot_schema_version<>OLD.snapshot_schema_version THEN
        RAISE EXCEPTION 'billing approval activation identity is immutable' USING ERRCODE='55000';
    END IF;
    IF NEW.state=OLD.state THEN
        IF NEW.version<>OLD.version OR NEW.updated_at<>OLD.updated_at THEN
            RAISE EXCEPTION 'unchanged billing approval activation cannot advance' USING ERRCODE='55000';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.version<>OLD.version+1 THEN
        RAISE EXCEPTION 'billing approval activation version must advance exactly once' USING ERRCODE='55000';
    END IF;
    NEW.updated_at := transaction_timestamp();
    RETURN NEW;
END $$;

CREATE FUNCTION public.append_billing_approval_activation_history() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    INSERT INTO public.billing_approval_command_activation_history
        (command_type,policy_version,snapshot_schema_version,state,version,updated_at)
    VALUES
        (NEW.command_type,NEW.policy_version,NEW.snapshot_schema_version,NEW.state,NEW.version,NEW.updated_at);
    RETURN NEW;
END $$;

CREATE FUNCTION public.enforce_billing_approval_activation_history_immutable() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'billing approval activation history is immutable' USING ERRCODE='55000';
END $$;

CREATE TRIGGER billing_approval_activation_update_guard
BEFORE UPDATE OR DELETE ON public.billing_approval_command_activations
FOR EACH ROW EXECUTE FUNCTION public.enforce_billing_approval_activation_update();
CREATE TRIGGER billing_approval_activation_truncate_guard
BEFORE TRUNCATE ON public.billing_approval_command_activations
FOR EACH STATEMENT EXECUTE FUNCTION public.enforce_billing_approval_activation_history_immutable();
CREATE TRIGGER billing_approval_activation_history_insert
AFTER INSERT ON public.billing_approval_command_activations
FOR EACH ROW EXECUTE FUNCTION public.append_billing_approval_activation_history();
CREATE TRIGGER billing_approval_activation_history_update
AFTER UPDATE ON public.billing_approval_command_activations
FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state)
EXECUTE FUNCTION public.append_billing_approval_activation_history();
CREATE TRIGGER billing_approval_activation_history_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.billing_approval_command_activation_history
FOR EACH STATEMENT EXECUTE FUNCTION public.enforce_billing_approval_activation_history_immutable();

INSERT INTO public.billing_approval_command_activations
    (command_type,policy_version,snapshot_schema_version,state,version,updated_at)
VALUES
    ('CHARGEBACK_FINANCIAL_REPAIR',1,1,'INACTIVE',1,transaction_timestamp()),
    ('ENTITLEMENT_COMPONENT_ADJUSTMENT',1,1,'INACTIVE',1,transaction_timestamp()),
    ('LEDGER_REPAIR',1,1,'INACTIVE',1,transaction_timestamp()),
    ('PAYMENT_FINANCIAL_REPAIR',1,1,'INACTIVE',1,transaction_timestamp()),
    ('PAYMENT_PRODUCT_RESUME',1,1,'ACTIVE',1,transaction_timestamp()),
    ('PAYMENT_PROVIDER_RESUME',2,1,'ACTIVE',1,transaction_timestamp()),
    ('RECEIVABLE_WRITE_OFF',1,1,'INACTIVE',1,transaction_timestamp()),
    ('RECONCILIATION_FINANCIAL_REPAIR',1,1,'INACTIVE',1,transaction_timestamp()),
    ('REFUND_EXCEPTION_REPAIR',1,1,'INACTIVE',1,transaction_timestamp()),
    ('USAGE_SETTLEMENT_REPAIR',1,1,'INACTIVE',1,transaction_timestamp()),
    ('WALLET_ADJUSTMENT',1,1,'ACTIVE',1,transaction_timestamp());

REVOKE ALL ON public.billing_approval_command_activations FROM PUBLIC;
REVOKE ALL ON public.billing_approval_command_activation_history FROM PUBLIC;
GRANT SELECT ON public.billing_approval_command_activations TO billing_service;
SET ROLE billing_service;

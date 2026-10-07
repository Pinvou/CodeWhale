DO $$
DECLARE
    existing_schema_owner TEXT;
BEGIN
    IF session_user<>'account_migrator'
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolsuper) THEN
        RAISE EXCEPTION '000050 requires the dedicated account_migrator identity';
    END IF;
    IF current_user<>'account_service'
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=current_user AND rolsuper) THEN
        RAISE EXCEPTION '000050 requires MIGRATION_ROLE account_service';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='account_approval_control_owner' AND NOT rolcanlogin AND NOT rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='account_migrator' AND rolcanlogin AND NOT rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='account_service' AND rolcanlogin
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT pg_catalog.pg_has_role('account_migrator','account_service','MEMBER')
       OR NOT pg_catalog.pg_has_role('account_migrator','account_approval_control_owner','MEMBER')
       OR pg_catalog.pg_has_role('account_service','account_migrator','MEMBER')
       OR pg_catalog.pg_has_role('account_service','account_approval_control_owner','MEMBER') THEN
        RAISE EXCEPTION '000050 requires isolated account migration and control owner roles';
    END IF;
    IF NOT pg_catalog.has_database_privilege('account_migrator',current_database(),'CREATE') THEN
        RAISE EXCEPTION '000050 requires temporary database CREATE for account_migrator';
    END IF;

    SELECT pg_catalog.pg_get_userbyid(nspowner)
      INTO existing_schema_owner
      FROM pg_catalog.pg_namespace
     WHERE nspname='account_approval_control';
    IF existing_schema_owner IS NOT NULL THEN
        RAISE EXCEPTION 'account_approval_control schema must not pre-exist migration 000050';
    END IF;
END $$;

RESET ROLE;
CREATE SCHEMA account_approval_control AUTHORIZATION account_approval_control_owner;
SET ROLE account_service;
DO $$
BEGIN
    EXECUTE pg_catalog.format('REVOKE CREATE ON DATABASE %I FROM account_migrator',current_database());
END $$;
SET ROLE account_approval_control_owner;

CREATE TABLE account_approval_control.admin_approval_control (
    control_id TEXT PRIMARY KEY,
    schema_version SMALLINT NOT NULL,
    mode TEXT NOT NULL,
    approval_wire_version SMALLINT NOT NULL,
    proof_required BOOLEAN NOT NULL,
    authorization_relay_state TEXT NOT NULL,
    approval_epoch BIGINT NOT NULL,
    authorization_relay_epoch BIGINT NOT NULL,
    version BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT admin_approval_control_singleton CHECK(control_id='ADMIN_APPROVAL'),
    CONSTRAINT admin_approval_control_schema CHECK(schema_version=1),
    CONSTRAINT admin_approval_control_mode CHECK(mode IN ('LEGACY','QUIESCING','PROOF_REQUIRED','SAFE_ROLLBACK')),
    CONSTRAINT admin_approval_control_wire CHECK(approval_wire_version IN (1,2)),
    CONSTRAINT admin_approval_control_relay CHECK(authorization_relay_state IN ('RUNNING','FENCED')),
    CONSTRAINT admin_approval_control_positive_versions CHECK(approval_epoch>0 AND authorization_relay_epoch>0 AND version>0),
    CONSTRAINT admin_approval_control_mode_tuple CHECK(
        (mode IN ('LEGACY','QUIESCING') AND approval_wire_version=1 AND proof_required=false)
        OR (mode='PROOF_REQUIRED' AND approval_wire_version=2 AND proof_required=true)
        OR (mode='SAFE_ROLLBACK' AND approval_wire_version=1 AND proof_required=true)
    )
);

INSERT INTO account_approval_control.admin_approval_control(
    control_id,schema_version,mode,approval_wire_version,proof_required,
    authorization_relay_state,approval_epoch,authorization_relay_epoch,version
) VALUES ('ADMIN_APPROVAL',1,'LEGACY',1,false,'RUNNING',1,1,1);

CREATE FUNCTION account_approval_control.enforce_admin_approval_control_update() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE
    mode_changed BOOLEAN := NEW.mode IS DISTINCT FROM OLD.mode;
    relay_changed BOOLEAN := NEW.authorization_relay_state IS DISTINCT FROM OLD.authorization_relay_state;
BEGIN
    IF NEW.control_id IS DISTINCT FROM OLD.control_id
       OR NEW.schema_version IS DISTINCT FROM OLD.schema_version THEN
        RAISE EXCEPTION 'ADMIN_APPROVAL identity and schema are immutable' USING ERRCODE='55000';
    END IF;
    IF mode_changed AND relay_changed THEN
        RAISE EXCEPTION 'ADMIN_APPROVAL mode and Relay changes require separate transactions' USING ERRCODE='23514';
    END IF;
    IF NOT mode_changed AND NOT relay_changed THEN
        RAISE EXCEPTION 'ADMIN_APPROVAL no-op and direct version changes are forbidden' USING ERRCODE='23514';
    END IF;

    IF mode_changed THEN
        IF NEW.authorization_relay_state IS DISTINCT FROM OLD.authorization_relay_state
           OR NEW.authorization_relay_epoch IS DISTINCT FROM OLD.authorization_relay_epoch
           OR NEW.approval_epoch IS DISTINCT FROM OLD.approval_epoch+1
           OR NEW.version IS DISTINCT FROM OLD.version+1 THEN
            RAISE EXCEPTION 'invalid ADMIN_APPROVAL mode version transition' USING ERRCODE='23514';
        END IF;
        IF NOT (
            (OLD.mode='LEGACY' AND NEW.mode='QUIESCING' AND OLD.authorization_relay_state='RUNNING')
            OR (OLD.mode='QUIESCING' AND NEW.mode='LEGACY' AND OLD.authorization_relay_state='RUNNING')
            OR (OLD.mode='QUIESCING' AND NEW.mode='PROOF_REQUIRED' AND OLD.authorization_relay_state='FENCED')
            OR (OLD.mode='PROOF_REQUIRED' AND NEW.mode='SAFE_ROLLBACK')
            OR (OLD.mode='SAFE_ROLLBACK' AND NEW.mode='PROOF_REQUIRED' AND OLD.authorization_relay_state='FENCED')
        ) THEN
            RAISE EXCEPTION 'invalid ADMIN_APPROVAL mode edge' USING ERRCODE='23514';
        END IF;
    ELSE
        IF NEW.mode IS DISTINCT FROM OLD.mode
           OR NEW.approval_wire_version IS DISTINCT FROM OLD.approval_wire_version
           OR NEW.proof_required IS DISTINCT FROM OLD.proof_required
           OR NEW.approval_epoch IS DISTINCT FROM OLD.approval_epoch
           OR NEW.authorization_relay_epoch IS DISTINCT FROM OLD.authorization_relay_epoch+1
           OR NEW.version IS DISTINCT FROM OLD.version+1 THEN
            RAISE EXCEPTION 'invalid ADMIN_APPROVAL Relay version transition' USING ERRCODE='23514';
        END IF;
        IF OLD.mode='SAFE_ROLLBACK' AND OLD.authorization_relay_state='FENCED'
           AND NEW.authorization_relay_state='RUNNING' THEN
            RAISE EXCEPTION 'SAFE_ROLLBACK Relay cannot resume before PROOF_REQUIRED' USING ERRCODE='23514';
        END IF;
    END IF;

    NEW.updated_at := transaction_timestamp();
    RETURN NEW;
END $$;

CREATE FUNCTION account_approval_control.reject_admin_approval_control_destructive() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'ADMIN_APPROVAL control cannot be deleted or truncated' USING ERRCODE='55000';
END $$;

CREATE TRIGGER admin_approval_control_update_guard
BEFORE UPDATE ON account_approval_control.admin_approval_control
FOR EACH ROW EXECUTE FUNCTION account_approval_control.enforce_admin_approval_control_update();
CREATE TRIGGER admin_approval_control_delete_guard
BEFORE DELETE ON account_approval_control.admin_approval_control
FOR EACH ROW EXECUTE FUNCTION account_approval_control.reject_admin_approval_control_destructive();
CREATE TRIGGER admin_approval_control_truncate_guard
BEFORE TRUNCATE ON account_approval_control.admin_approval_control
FOR EACH STATEMENT EXECUTE FUNCTION account_approval_control.reject_admin_approval_control_destructive();

-- SELECT FOR SHARE requires UPDATE privilege when issued directly. Runtime is
-- granted only this fixed, schema-qualified shared-lock operation.
CREATE FUNCTION account_approval_control.lock_admin_approval_control_shared()
RETURNS SETOF account_approval_control.admin_approval_control
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
    SELECT *
      FROM account_approval_control.admin_approval_control
     WHERE control_id='ADMIN_APPROVAL'
       FOR SHARE
$$;

REVOKE ALL ON SCHEMA account_approval_control FROM PUBLIC,account_service;
GRANT USAGE ON SCHEMA account_approval_control TO account_service;
REVOKE ALL ON account_approval_control.admin_approval_control FROM PUBLIC,account_service;
GRANT SELECT ON account_approval_control.admin_approval_control TO account_service;
REVOKE ALL ON FUNCTION account_approval_control.lock_admin_approval_control_shared() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION account_approval_control.lock_admin_approval_control_shared() TO account_service;
REVOKE ALL ON FUNCTION account_approval_control.enforce_admin_approval_control_update(),account_approval_control.reject_admin_approval_control_destructive() FROM PUBLIC,account_service;

SET ROLE account_service;

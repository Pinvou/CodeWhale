DO $$
DECLARE
    authorization_owner_oid OID;
    migrator_oid OID;
    service_oid OID;
    writer_oid OID;
    backfill_oid OID;
BEGIN
    IF session_user <> 'account_migrator'
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolsuper) THEN
        RAISE EXCEPTION '000051 requires the dedicated account_migrator identity';
    END IF;
    IF current_user <> 'account_service'
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=current_user AND rolsuper) THEN
        RAISE EXCEPTION '000051 requires MIGRATION_ROLE account_service';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='account_approval_authorization_owner' AND NOT rolcanlogin AND NOT rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='account_approval_writer' AND rolcanlogin AND NOT rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname='account_approval_backfill' AND rolcanlogin AND NOT rolinherit
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
    ) THEN
        RAISE EXCEPTION '000051 requires isolated low-privilege authorization roles';
    END IF;
    SELECT oid INTO authorization_owner_oid FROM pg_catalog.pg_roles WHERE rolname='account_approval_authorization_owner';
    SELECT oid INTO migrator_oid FROM pg_catalog.pg_roles WHERE rolname='account_migrator';
    SELECT oid INTO service_oid FROM pg_catalog.pg_roles WHERE rolname='account_service';
    SELECT oid INTO writer_oid FROM pg_catalog.pg_roles WHERE rolname='account_approval_writer';
    SELECT oid INTO backfill_oid FROM pg_catalog.pg_roles WHERE rolname='account_approval_backfill';
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_auth_members
        WHERE roleid=authorization_owner_oid AND member=migrator_oid
          AND admin_option AND set_option AND NOT inherit_option
    ) OR EXISTS (
        SELECT 1 FROM pg_catalog.pg_auth_members am
         WHERE (am.roleid IN (authorization_owner_oid,writer_oid,backfill_oid)
                OR am.member IN (authorization_owner_oid,writer_oid,backfill_oid))
           AND NOT (am.roleid=authorization_owner_oid AND am.member=migrator_oid
                    AND am.admin_option AND am.set_option AND NOT am.inherit_option)
    ) THEN
        RAISE EXCEPTION 'authorization role membership is unsafe';
    END IF;
    IF pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.outbox_events'::regclass)) <> 'account_service' THEN
        RAISE EXCEPTION 'outbox_events must still be owned by account_service before migration 000051';
    END IF;
    IF NOT pg_catalog.has_table_privilege('account_service','public.outbox_events','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER')
       OR EXISTS (
           SELECT 1
             FROM pg_catalog.pg_class c,
                  LATERAL pg_catalog.aclexplode(COALESCE(c.relacl,pg_catalog.acldefault('r',c.relowner))) acl
            WHERE c.oid='public.outbox_events'::regclass AND acl.grantee<>service_oid
              AND acl.privilege_type IN ('SELECT','INSERT','UPDATE','DELETE','TRUNCATE','TRIGGER')
       )
       OR EXISTS (
           SELECT 1
             FROM pg_catalog.pg_attribute a,
                  LATERAL pg_catalog.aclexplode(a.attacl) acl
            WHERE a.attrelid='public.outbox_events'::regclass AND a.attnum>0 AND NOT a.attisdropped
              AND acl.grantee<>service_oid AND acl.privilege_type IN ('SELECT','INSERT','UPDATE','REFERENCES')
       )
       OR pg_catalog.has_any_column_privilege('account_approval_writer','public.outbox_events','SELECT,INSERT,UPDATE,REFERENCES')
       OR pg_catalog.has_any_column_privilege('account_approval_backfill','public.outbox_events','SELECT,INSERT,UPDATE,REFERENCES') THEN
        RAISE EXCEPTION 'outbox_events pre-migration ACL is invalid';
    END IF;
    IF pg_catalog.pg_get_userbyid((SELECT nspowner FROM pg_catalog.pg_namespace WHERE nspname='account_approval_control')) <> 'account_approval_control_owner' THEN
        RAISE EXCEPTION 'ADMIN_APPROVAL control owner is invalid';
    END IF;
    IF pg_catalog.to_regnamespace('account_approval_authorization') IS NOT NULL THEN
        RAISE EXCEPTION 'account_approval_authorization schema must not pre-exist migration 000051';
    END IF;
END $$;

-- 000050 revokes this privilege at its transaction boundary. Re-grant and
-- revoke inside this transaction so a fresh 50 -> 51 run remains executable.
SET ROLE account_service;
DO $$ BEGIN
    EXECUTE pg_catalog.format('GRANT CREATE ON DATABASE %I TO account_migrator',current_database());
END $$;
RESET ROLE;
CREATE SCHEMA account_approval_authorization AUTHORIZATION account_approval_authorization_owner;
SET ROLE account_service;
DO $$ BEGIN
    EXECUTE pg_catalog.format('REVOKE CREATE ON DATABASE %I FROM account_migrator',current_database());
END $$;
RESET ROLE;

-- PostgreSQL requires the old table owner to SET ROLE to the new owner and
-- the new owner to have CREATE on the containing schema. Both grants are
-- transactional and removed immediately after the transfer.
GRANT account_approval_authorization_owner TO account_service WITH INHERIT FALSE, SET TRUE;
SET ROLE account_service;
GRANT USAGE,CREATE ON SCHEMA public TO account_approval_authorization_owner;
ALTER TABLE public.outbox_events ADD COLUMN approval_control_epoch BIGINT CHECK(approval_control_epoch IS NULL OR approval_control_epoch>0);
ALTER TABLE public.outbox_events OWNER TO account_approval_authorization_owner;
RESET ROLE;

SET ROLE account_approval_authorization_owner;
CREATE INDEX outbox_events_authorization_backfill_idx ON public.outbox_events(occurred_at,event_id)
WHERE event_type='account.approval-command.authorized.v1';
CREATE UNIQUE INDEX outbox_events_authorization_version_uidx ON public.outbox_events(aggregate_id,aggregate_version)
WHERE event_type='account.approval-command.authorized.v1';
RESET ROLE;

SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;
GRANT USAGE ON SCHEMA public TO account_approval_authorization_owner;
RESET ROLE;
REVOKE account_approval_authorization_owner FROM account_service;

SET ROLE account_approval_control_owner;
GRANT USAGE ON SCHEMA account_approval_control TO account_approval_authorization_owner;
GRANT EXECUTE ON FUNCTION account_approval_control.lock_admin_approval_control_shared() TO account_approval_authorization_owner;
CREATE FUNCTION account_approval_control.lock_admin_approval_control_exclusive_for_authorization()
RETURNS SETOF account_approval_control.admin_approval_control
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
    SELECT * FROM account_approval_control.admin_approval_control
     WHERE control_id='ADMIN_APPROVAL' FOR UPDATE
$$;
REVOKE ALL ON FUNCTION account_approval_control.lock_admin_approval_control_exclusive_for_authorization() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION account_approval_control.lock_admin_approval_control_exclusive_for_authorization()
    TO account_approval_authorization_owner,account_migrator;
RESET ROLE;
SET ROLE account_approval_authorization_owner;

CREATE TABLE account_approval_authorization.approval_authorization_event_bytes (
    schema_version SMALLINT NOT NULL CHECK(schema_version=1),
    event_id UUID PRIMARY KEY REFERENCES public.outbox_events(event_id) ON DELETE RESTRICT,
    serialization_version TEXT NOT NULL CHECK(serialization_version IN ('LEGACY_TYPED_V1','APPROVAL_RFC8785_DECIMAL_V1')),
    provenance TEXT NOT NULL CHECK(provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY','RECONSTRUCTED_AFTER_PUBLISH')),
    delivery_bytes BYTEA,
    event_digest TEXT,
    reconstructed_semantic_digest TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT approval_authorization_sidecar_oneof CHECK (
        (provenance='FROZEN_AT_APPROVAL' AND delivery_bytes IS NOT NULL
         AND octet_length(delivery_bytes) BETWEEN 1 AND 65536
         AND event_digest IS NOT NULL AND event_digest ~ '^[0-9a-f]{64}$'
         AND reconstructed_semantic_digest IS NULL)
        OR
        (provenance='RECONSTRUCTED_FOR_DELIVERY' AND serialization_version='LEGACY_TYPED_V1'
         AND delivery_bytes IS NOT NULL AND octet_length(delivery_bytes) BETWEEN 1 AND 65536
         AND event_digest IS NOT NULL AND event_digest ~ '^[0-9a-f]{64}$'
         AND reconstructed_semantic_digest IS NULL)
        OR
        (provenance='RECONSTRUCTED_AFTER_PUBLISH' AND serialization_version='LEGACY_TYPED_V1'
         AND delivery_bytes IS NULL AND event_digest IS NULL
         AND reconstructed_semantic_digest IS NOT NULL
         AND reconstructed_semantic_digest ~ '^[0-9a-f]{64}$')
    ),
    CONSTRAINT approval_authorization_delivery_digest CHECK (
        delivery_bytes IS NULL OR (event_digest IS NOT NULL
            AND event_digest=pg_catalog.encode(pg_catalog.sha256(delivery_bytes),'hex'))
    )
);

-- Rows in this table are transaction-local protocol markers: prepare inserts,
-- finalize consumes, and a deferred trigger forbids committing any remainder.
CREATE TABLE account_approval_authorization.authorization_event_pending (
    event_id UUID PRIMARY KEY REFERENCES public.outbox_events(event_id) ON DELETE RESTRICT,
    aggregate_id UUID NOT NULL,
    aggregate_version BIGINT NOT NULL CHECK(aggregate_version>0),
    approval_control_epoch BIGINT NOT NULL CHECK(approval_control_epoch>0),
    serialization_version TEXT NOT NULL CHECK(serialization_version IN ('LEGACY_TYPED_V1','APPROVAL_RFC8785_DECIMAL_V1'))
);

CREATE FUNCTION account_approval_authorization.reject_sidecar_mutation() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'approval authorization sidecar is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER approval_authorization_sidecar_update_guard BEFORE UPDATE ON account_approval_authorization.approval_authorization_event_bytes
FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.reject_sidecar_mutation();
CREATE TRIGGER approval_authorization_sidecar_delete_guard BEFORE DELETE ON account_approval_authorization.approval_authorization_event_bytes
FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.reject_sidecar_mutation();
CREATE TRIGGER approval_authorization_sidecar_truncate_guard BEFORE TRUNCATE ON account_approval_authorization.approval_authorization_event_bytes
FOR EACH STATEMENT EXECUTE FUNCTION account_approval_authorization.reject_sidecar_mutation();

CREATE FUNCTION account_approval_authorization.reject_unfinalized_pending() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM account_approval_authorization.authorization_event_pending WHERE event_id=NEW.event_id) THEN
        RAISE EXCEPTION 'authorization prepare must be finalized in the same transaction' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER approval_authorization_pending_commit_guard
AFTER INSERT ON account_approval_authorization.authorization_event_pending
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
EXECUTE FUNCTION account_approval_authorization.reject_unfinalized_pending();

CREATE FUNCTION account_approval_authorization.guard_outbox_insert() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    control_row account_approval_control.admin_approval_control%ROWTYPE;
    request_status TEXT;
    request_version BIGINT;
BEGIN
    IF NEW.event_type <> 'account.approval-command.authorized.v1' THEN
        IF NEW.approval_control_epoch IS NOT NULL THEN
            RAISE EXCEPTION 'non-authorization outbox event cannot bind approval epoch' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
    SELECT status,version INTO request_status,request_version FROM public.approval_requests WHERE id=NEW.aggregate_id;
    IF request_status IS DISTINCT FROM 'APPROVED' OR request_version IS DISTINCT FROM NEW.aggregate_version THEN
        RAISE EXCEPTION 'authorization event does not bind the approved request version' USING ERRCODE='23514';
    END IF;
    IF NEW.approval_control_epoch IS NULL THEN
        IF session_user <> 'account_service'
           OR control_row.schema_version<>1 OR control_row.mode<>'LEGACY'
           OR control_row.approval_wire_version<>1 OR control_row.proof_required
           OR control_row.authorization_relay_state<>'RUNNING' OR control_row.approval_epoch<>1
           OR control_row.version<=0 OR control_row.authorization_relay_epoch<=0 THEN
            RAISE EXCEPTION 'legacy authorization append is not compatible with current control' USING ERRCODE='55000';
        END IF;
        NEW.approval_control_epoch := 1;
        RETURN NEW;
    END IF;
    IF session_user <> 'account_approval_writer'
       OR NEW.approval_control_epoch<>control_row.approval_epoch
       OR control_row.authorization_relay_state<>'RUNNING'
       OR NOT ((control_row.mode='LEGACY' AND control_row.approval_wire_version=1 AND NOT control_row.proof_required)
               OR (control_row.mode='PROOF_REQUIRED' AND control_row.approval_wire_version=2 AND control_row.proof_required)) THEN
        RAISE EXCEPTION 'explicit authorization epoch is not permitted' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;

CREATE FUNCTION account_approval_authorization.guard_outbox_update() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF OLD.event_type='account.approval-command.authorized.v1' OR NEW.event_type='account.approval-command.authorized.v1' THEN
        IF NEW.event_id IS DISTINCT FROM OLD.event_id OR NEW.event_type IS DISTINCT FROM OLD.event_type
           OR NEW.aggregate_id IS DISTINCT FROM OLD.aggregate_id OR NEW.aggregate_version IS DISTINCT FROM OLD.aggregate_version
           OR NEW.payload IS DISTINCT FROM OLD.payload OR NEW.trace_id IS DISTINCT FROM OLD.trace_id
           OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at THEN
            RAISE EXCEPTION 'authorization outbox identity and payload are immutable' USING ERRCODE='55000';
        END IF;
        IF NEW.approval_control_epoch IS DISTINCT FROM OLD.approval_control_epoch
           AND NOT (session_user='account_approval_backfill' AND OLD.approval_control_epoch IS NULL AND NEW.approval_control_epoch=1) THEN
            RAISE EXCEPTION 'authorization outbox epoch is immutable' USING ERRCODE='55000';
        END IF;
    ELSIF NEW.approval_control_epoch IS NOT NULL THEN
        RAISE EXCEPTION 'non-authorization outbox event cannot bind approval epoch' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;

CREATE FUNCTION account_approval_authorization.guard_outbox_delete() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF OLD.event_type='account.approval-command.authorized.v1' THEN
        RAISE EXCEPTION 'authorization outbox event cannot be deleted' USING ERRCODE='55000';
    END IF;
    RETURN OLD;
END $$;
CREATE FUNCTION account_approval_authorization.guard_outbox_truncate() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.outbox_events WHERE event_type='account.approval-command.authorized.v1') THEN
        RAISE EXCEPTION 'outbox with authorization events cannot be truncated' USING ERRCODE='55000';
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER approval_authorization_outbox_insert_guard BEFORE INSERT ON public.outbox_events
FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.guard_outbox_insert();
CREATE TRIGGER approval_authorization_outbox_update_guard BEFORE UPDATE ON public.outbox_events
FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.guard_outbox_update();
CREATE TRIGGER approval_authorization_outbox_delete_guard BEFORE DELETE ON public.outbox_events
FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.guard_outbox_delete();
CREATE TRIGGER approval_authorization_outbox_truncate_guard BEFORE TRUNCATE ON public.outbox_events
FOR EACH STATEMENT EXECUTE FUNCTION account_approval_authorization.guard_outbox_truncate();

-- The writer has one two-step API inside its caller-owned transaction. Prepare
-- returns the persisted JSONB/time facts; finalize accepts Go-produced bytes.
-- The deferred trigger makes the pair atomic at commit.
CREATE FUNCTION account_approval_authorization.prepare_authorization_event(
    p_event_id UUID,p_aggregate_id UUID,p_aggregate_version BIGINT,p_payload JSONB,
    p_trace_id TEXT,p_approval_control_epoch BIGINT,p_serialization_version TEXT
) RETURNS TABLE(
    event_id UUID,event_type TEXT,aggregate_id UUID,aggregate_version BIGINT,
    payload JSONB,trace_id TEXT,occurred_at TIMESTAMPTZ,approval_control_epoch BIGINT
) LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    control_row account_approval_control.admin_approval_control%ROWTYPE;
    request_row RECORD;
BEGIN
    IF session_user<>'account_approval_writer' THEN
        RAISE EXCEPTION 'authorization writer identity is invalid' USING ERRCODE='42501';
    END IF;
    SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
    SELECT status,version,command_type,policy_version,target_resource_type,target_resource_id
      INTO STRICT request_row FROM public.approval_requests WHERE id=p_aggregate_id FOR UPDATE;
    IF request_row.status<>'APPROVED' OR request_row.version<>p_aggregate_version
       OR p_approval_control_epoch<>control_row.approval_epoch THEN
        RAISE EXCEPTION 'authorization append is not bound to locked facts' USING ERRCODE='23514';
    END IF;
    IF (p_serialization_version='LEGACY_TYPED_V1' AND NOT (
            control_row.schema_version=1 AND control_row.mode='LEGACY'
            AND control_row.approval_wire_version=1 AND NOT control_row.proof_required
            AND control_row.authorization_relay_state='RUNNING' AND control_row.approval_epoch=1))
       OR (p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1' AND NOT (
            control_row.schema_version=1 AND control_row.mode='PROOF_REQUIRED'
            AND control_row.approval_wire_version=2 AND control_row.proof_required
            AND control_row.authorization_relay_state='RUNNING'))
       OR p_serialization_version NOT IN ('LEGACY_TYPED_V1','APPROVAL_RFC8785_DECIMAL_V1') THEN
        RAISE EXCEPTION 'authorization serialization is incompatible with control' USING ERRCODE='55000';
    END IF;
    IF p_event_id IS NULL OR p_payload IS NULL OR p_trace_id IS NULL OR pg_catalog.btrim(p_trace_id)='' THEN
        RAISE EXCEPTION 'authorization append input is invalid' USING ERRCODE='22023';
    END IF;
    IF EXISTS (
        SELECT 1 FROM public.outbox_events existing_event
         WHERE existing_event.event_type='account.approval-command.authorized.v1'
           AND existing_event.aggregate_id=p_aggregate_id AND existing_event.aggregate_version=p_aggregate_version
    ) THEN
        RAISE EXCEPTION 'authorization event already exists for approved request version' USING ERRCODE='23505';
    END IF;
    IF p_payload->>'approval_request_id' IS DISTINCT FROM p_aggregate_id::TEXT
       OR p_payload->>'command_type' IS DISTINCT FROM request_row.command_type
       OR (p_payload->>'policy_version')::BIGINT IS DISTINCT FROM request_row.policy_version
       OR p_payload->>'target_resource_type' IS DISTINCT FROM request_row.target_resource_type
       OR p_payload->>'target_resource_id' IS DISTINCT FROM request_row.target_resource_id
       OR (p_serialization_version='LEGACY_TYPED_V1' AND p_payload ? 'approval_control_epoch')
       OR (p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
           AND p_payload->>'approval_control_epoch' IS DISTINCT FROM p_approval_control_epoch::TEXT) THEN
        RAISE EXCEPTION 'authorization payload does not match persisted facts' USING ERRCODE='23514';
    END IF;
    RETURN QUERY
    INSERT INTO public.outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,approval_control_epoch)
    VALUES (p_event_id,'account.approval-command.authorized.v1',p_aggregate_id,p_aggregate_version,p_payload,p_trace_id,transaction_timestamp(),p_approval_control_epoch)
    RETURNING outbox_events.event_id,outbox_events.event_type,outbox_events.aggregate_id,
              outbox_events.aggregate_version,outbox_events.payload,outbox_events.trace_id,
              outbox_events.occurred_at,outbox_events.approval_control_epoch;
    INSERT INTO account_approval_authorization.authorization_event_pending(
        event_id,aggregate_id,aggregate_version,approval_control_epoch,serialization_version
    ) VALUES (p_event_id,p_aggregate_id,p_aggregate_version,p_approval_control_epoch,p_serialization_version);
END $$;

CREATE FUNCTION account_approval_authorization.finalize_authorization_event(
    p_event_id UUID,p_serialization_version TEXT,p_delivery_bytes BYTEA
) RETURNS VOID LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    outbox_row public.outbox_events%ROWTYPE;
    pending_row account_approval_authorization.authorization_event_pending%ROWTYPE;
    envelope JSONB;
BEGIN
    IF session_user<>'account_approval_writer' THEN
        RAISE EXCEPTION 'authorization writer identity is invalid' USING ERRCODE='42501';
    END IF;
    SELECT * INTO STRICT pending_row FROM account_approval_authorization.authorization_event_pending
      WHERE event_id=p_event_id FOR UPDATE;
    IF pending_row.serialization_version<>p_serialization_version THEN
        RAISE EXCEPTION 'authorization finalize serialization changed after prepare' USING ERRCODE='23514';
    END IF;
    SELECT * INTO STRICT outbox_row FROM public.outbox_events
      WHERE event_id=p_event_id AND event_type='account.approval-command.authorized.v1' FOR SHARE;
    IF pending_row.aggregate_id<>outbox_row.aggregate_id
       OR pending_row.aggregate_version<>outbox_row.aggregate_version
       OR pending_row.approval_control_epoch<>outbox_row.approval_control_epoch THEN
        RAISE EXCEPTION 'authorization pending marker does not match outbox' USING ERRCODE='23514';
    END IF;
    IF p_serialization_version NOT IN ('LEGACY_TYPED_V1','APPROVAL_RFC8785_DECIMAL_V1')
       OR p_delivery_bytes IS NULL OR octet_length(p_delivery_bytes) NOT BETWEEN 1 AND 65536 THEN
        RAISE EXCEPTION 'authorization finalize input is invalid' USING ERRCODE='22023';
    END IF;
    BEGIN
        envelope := pg_catalog.convert_from(p_delivery_bytes,'UTF8')::JSONB;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'authorization bytes are not valid JSON' USING ERRCODE='22023';
    END;
    IF envelope->>'event_id' IS DISTINCT FROM outbox_row.event_id::TEXT
       OR envelope->>'event_type' IS DISTINCT FROM outbox_row.event_type
       OR envelope->>'aggregate_id' IS DISTINCT FROM outbox_row.aggregate_id::TEXT
       OR (envelope->>'aggregate_version')::BIGINT IS DISTINCT FROM outbox_row.aggregate_version
       OR envelope->>'trace_id' IS DISTINCT FROM outbox_row.trace_id
       OR (envelope->>'occurred_at')::TIMESTAMPTZ IS DISTINCT FROM outbox_row.occurred_at
       OR envelope->'payload' IS DISTINCT FROM outbox_row.payload
       OR (p_serialization_version='LEGACY_TYPED_V1' AND envelope ? 'serialization_version')
       OR (p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1' AND (
            envelope->>'serialization_version' IS DISTINCT FROM 'APPROVAL_RFC8785_DECIMAL_V1'
            OR envelope->'payload'->>'approval_control_epoch' IS DISTINCT FROM outbox_row.approval_control_epoch::TEXT)) THEN
        RAISE EXCEPTION 'authorization bytes do not match persisted event' USING ERRCODE='23514';
    END IF;
    INSERT INTO account_approval_authorization.approval_authorization_event_bytes(
        schema_version,event_id,serialization_version,provenance,delivery_bytes,event_digest
    ) VALUES (1,p_event_id,p_serialization_version,'FROZEN_AT_APPROVAL',p_delivery_bytes,
        pg_catalog.encode(pg_catalog.sha256(p_delivery_bytes),'hex'));
    DELETE FROM account_approval_authorization.authorization_event_pending WHERE event_id=p_event_id;
END $$;

CREATE FUNCTION account_approval_authorization.backfill_authorization_sidecar(
    p_event_id UUID,p_delivery_bytes BYTEA,p_reconstructed_semantic_digest TEXT
) RETURNS VOID LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    control_row account_approval_control.admin_approval_control%ROWTYPE;
    outbox_row public.outbox_events%ROWTYPE;
    existing account_approval_authorization.approval_authorization_event_bytes%ROWTYPE;
    provenance_value TEXT;
    digest_value TEXT;
    envelope JSONB;
BEGIN
    IF session_user<>'account_approval_backfill' THEN
        RAISE EXCEPTION 'authorization backfill identity is invalid' USING ERRCODE='42501';
    END IF;
    SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
    IF control_row.schema_version<>1 OR control_row.mode<>'LEGACY' OR control_row.approval_wire_version<>1
       OR control_row.proof_required OR control_row.authorization_relay_state<>'RUNNING' OR control_row.approval_epoch<>1 THEN
        RAISE EXCEPTION 'authorization backfill requires LEGACY epoch 1' USING ERRCODE='55000';
    END IF;
    SELECT * INTO STRICT outbox_row FROM public.outbox_events
      WHERE event_id=p_event_id AND event_type='account.approval-command.authorized.v1' FOR UPDATE;
    IF p_delivery_bytes IS NULL OR octet_length(p_delivery_bytes) NOT BETWEEN 1 AND 65536 THEN
        RAISE EXCEPTION 'backfill requires reconstructed legacy bytes' USING ERRCODE='22023';
    END IF;
    BEGIN
        envelope := pg_catalog.convert_from(p_delivery_bytes,'UTF8')::JSONB;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'backfill bytes are not valid JSON' USING ERRCODE='22023';
    END;
    IF envelope ? 'serialization_version'
       OR envelope->>'event_id' IS DISTINCT FROM outbox_row.event_id::TEXT
       OR envelope->>'event_type' IS DISTINCT FROM outbox_row.event_type
       OR envelope->>'aggregate_id' IS DISTINCT FROM outbox_row.aggregate_id::TEXT
       OR (envelope->>'aggregate_version')::BIGINT IS DISTINCT FROM outbox_row.aggregate_version
       OR envelope->>'trace_id' IS DISTINCT FROM outbox_row.trace_id
       OR (envelope->>'occurred_at')::TIMESTAMPTZ IS DISTINCT FROM outbox_row.occurred_at
       OR envelope->'payload' IS DISTINCT FROM outbox_row.payload THEN
        RAISE EXCEPTION 'backfill bytes do not match persisted event' USING ERRCODE='23514';
    END IF;
    provenance_value := CASE WHEN outbox_row.published_at IS NULL THEN 'RECONSTRUCTED_FOR_DELIVERY' ELSE 'RECONSTRUCTED_AFTER_PUBLISH' END;
    digest_value := pg_catalog.encode(pg_catalog.sha256(p_delivery_bytes),'hex');
    IF (provenance_value='RECONSTRUCTED_FOR_DELIVERY' AND p_reconstructed_semantic_digest IS NOT NULL)
       OR (provenance_value='RECONSTRUCTED_AFTER_PUBLISH' AND p_reconstructed_semantic_digest IS DISTINCT FROM digest_value) THEN
        RAISE EXCEPTION 'backfill evidence does not match publication state' USING ERRCODE='23514';
    END IF;
    SELECT * INTO existing FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=p_event_id;
    IF FOUND THEN
        IF outbox_row.approval_control_epoch IS DISTINCT FROM 1 OR existing.schema_version<>1
           OR existing.serialization_version<>'LEGACY_TYPED_V1'
           OR existing.provenance NOT IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY','RECONSTRUCTED_AFTER_PUBLISH')
           OR (existing.provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY') AND (
                existing.delivery_bytes IS DISTINCT FROM p_delivery_bytes OR existing.event_digest IS DISTINCT FROM digest_value
                OR existing.reconstructed_semantic_digest IS NOT NULL))
           OR (existing.provenance='RECONSTRUCTED_AFTER_PUBLISH' AND (
                outbox_row.published_at IS NULL OR existing.delivery_bytes IS NOT NULL OR existing.event_digest IS NOT NULL
                OR existing.reconstructed_semantic_digest IS DISTINCT FROM digest_value)) THEN
            RAISE EXCEPTION 'authorization sidecar conflicts with existing immutable facts' USING ERRCODE='55000';
        END IF;
        RETURN;
    END IF;
    IF outbox_row.approval_control_epoch IS NULL THEN
        UPDATE public.outbox_events SET approval_control_epoch=1 WHERE event_id=p_event_id;
    ELSIF outbox_row.approval_control_epoch<>1 THEN
        RAISE EXCEPTION 'authorization backfill epoch is not compatible' USING ERRCODE='55000';
    END IF;
    INSERT INTO account_approval_authorization.approval_authorization_event_bytes(
        schema_version,event_id,serialization_version,provenance,delivery_bytes,event_digest,reconstructed_semantic_digest
    ) VALUES (
        1,p_event_id,'LEGACY_TYPED_V1',provenance_value,
        CASE WHEN provenance_value='RECONSTRUCTED_FOR_DELIVERY' THEN p_delivery_bytes ELSE NULL END,
        CASE WHEN provenance_value='RECONSTRUCTED_FOR_DELIVERY' THEN digest_value ELSE NULL END,
        p_reconstructed_semantic_digest
    );
END $$;

CREATE FUNCTION account_approval_authorization.authorization_commit_outcome(
    p_event_id UUID,p_request_id UUID,p_request_version BIGINT,p_event_digest TEXT,
    p_serialization_version TEXT,p_approval_epoch BIGINT,p_decision_id UUID,p_state_event_id UUID
) RETURNS BOOLEAN LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF session_user<>'account_approval_writer' THEN
        RAISE EXCEPTION 'authorization writer identity is invalid' USING ERRCODE='42501';
    END IF;
    RETURN EXISTS (
        SELECT 1
          FROM public.approval_requests r
          JOIN public.outbox_events o ON o.aggregate_id=r.id AND o.aggregate_version=p_request_version
          JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
          JOIN public.approval_decisions d ON d.approval_request_id=r.id
          JOIN public.approval_state_events e ON e.approval_request_id=r.id
         WHERE r.id=p_request_id AND r.version>=p_request_version
           AND o.event_id=p_event_id AND o.event_type='account.approval-command.authorized.v1'
           AND o.approval_control_epoch=p_approval_epoch
           AND s.provenance='FROZEN_AT_APPROVAL' AND s.event_digest=p_event_digest
           AND s.serialization_version=p_serialization_version
           AND d.id=p_decision_id AND d.decision='APPROVED'
           AND e.id=p_state_event_id AND e.to_status='APPROVED' AND e.reason_code='INDEPENDENT_DECISION'
           AND NOT EXISTS (SELECT 1 FROM account_approval_authorization.authorization_event_pending p WHERE p.event_id=o.event_id)
    );
END $$;

CREATE FUNCTION account_approval_authorization.authorization_backfill_outcome(
    p_event_id UUID,p_provenance TEXT,p_digest TEXT
) RETURNS BOOLEAN LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF session_user<>'account_approval_backfill' THEN
        RAISE EXCEPTION 'authorization backfill identity is invalid' USING ERRCODE='42501';
    END IF;
    IF p_event_id IS NULL OR p_provenance IS NULL
       OR p_provenance NOT IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY','RECONSTRUCTED_AFTER_PUBLISH')
       OR p_digest IS NULL OR p_digest !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'authorization backfill outcome binding is invalid' USING ERRCODE='22023';
    END IF;
    RETURN EXISTS (
        SELECT 1
          FROM public.outbox_events o
          JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
         WHERE o.event_id=p_event_id AND o.event_type='account.approval-command.authorized.v1'
           AND o.approval_control_epoch=1 AND s.schema_version=1
           AND s.serialization_version='LEGACY_TYPED_V1' AND s.provenance=p_provenance
           AND ((p_provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY')
                 AND s.delivery_bytes IS NOT NULL AND s.event_digest=p_digest
                 AND pg_catalog.encode(pg_catalog.sha256(s.delivery_bytes),'hex')=p_digest
                 AND s.reconstructed_semantic_digest IS NULL)
                OR (p_provenance='RECONSTRUCTED_AFTER_PUBLISH'
                    AND o.published_at IS NOT NULL AND s.delivery_bytes IS NULL AND s.event_digest IS NULL
                    AND s.reconstructed_semantic_digest=p_digest))
           AND NOT EXISTS (SELECT 1 FROM account_approval_authorization.authorization_event_pending p WHERE p.event_id=o.event_id)
    );
END $$;

CREATE FUNCTION account_approval_authorization.approval_terminal_commit_outcome(
    p_request_id UUID,p_request_version BIGINT,p_request_status TEXT,
    p_state_event_id UUID,p_reason_code TEXT
) RETURNS BOOLEAN LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF session_user<>'account_approval_writer' THEN
        RAISE EXCEPTION 'authorization writer identity is invalid' USING ERRCODE='42501';
    END IF;
    IF p_request_id IS NULL OR p_request_version IS NULL OR p_request_version<1
       OR p_request_status IS NULL OR p_state_event_id IS NULL OR p_reason_code IS NULL THEN
        RAISE EXCEPTION 'approval terminal outcome binding is invalid' USING ERRCODE='22023';
    END IF;
    RETURN EXISTS (
        SELECT 1
          FROM public.approval_requests r
          JOIN public.approval_state_events e ON e.approval_request_id=r.id
         WHERE r.id=p_request_id
           AND e.id=p_state_event_id AND e.to_status=p_request_status AND e.reason_code=p_reason_code
    );
END $$;

CREATE FUNCTION account_approval_authorization.require_authorization_sidecar_at_commit() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE current_mode TEXT;
BEGIN
    IF NEW.event_type<>'account.approval-command.authorized.v1' THEN RETURN NULL; END IF;
    SELECT mode INTO STRICT current_mode FROM account_approval_control.lock_admin_approval_control_shared();
    IF session_user='account_approval_writer' OR current_mode<>'LEGACY' THEN
        IF NOT EXISTS (SELECT 1 FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=NEW.event_id) THEN
            RAISE EXCEPTION 'authorization event requires an immutable sidecar' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER approval_authorization_sidecar_presence
AFTER INSERT OR UPDATE ON public.outbox_events DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.require_authorization_sidecar_at_commit();

CREATE FUNCTION account_approval_authorization.require_approved_request_authorization_at_commit() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF OLD.status IS DISTINCT FROM 'APPROVED' AND NEW.status='APPROVED' THEN
        IF session_user NOT IN ('account_service','account_approval_writer') THEN
            RAISE EXCEPTION 'approval transition identity is invalid' USING ERRCODE='42501';
        END IF;
        IF NOT EXISTS (
            SELECT 1 FROM public.outbox_events o
             WHERE o.event_type='account.approval-command.authorized.v1'
               AND o.aggregate_id=NEW.id AND o.aggregate_version=NEW.version
               AND o.payload->>'approval_request_id'=NEW.id::TEXT
        ) THEN
            RAISE EXCEPTION 'approved request requires its authorization facts' USING ERRCODE='23514';
        END IF;
        IF session_user='account_approval_writer' AND (
            EXISTS (SELECT 1 FROM account_approval_authorization.authorization_event_pending p
                     JOIN public.outbox_events o ON o.event_id=p.event_id
                    WHERE o.aggregate_id=NEW.id AND o.aggregate_version=NEW.version)
            OR NOT EXISTS (
                SELECT 1 FROM public.outbox_events o
                JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
                 WHERE o.aggregate_id=NEW.id AND o.aggregate_version=NEW.version
                   AND s.provenance='FROZEN_AT_APPROVAL'
            )
        ) THEN
            RAISE EXCEPTION 'writer approval requires a finalized frozen sidecar' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NULL;
END $$;

CREATE FUNCTION account_approval_authorization.authorization_delivery_matches(
    p_event_id UUID,p_event_type TEXT,p_aggregate_id UUID,p_aggregate_version BIGINT,
    p_payload JSONB,p_trace_id TEXT,p_occurred_at TIMESTAMPTZ,p_approval_control_epoch BIGINT,
    p_serialization_version TEXT,p_delivery_bytes BYTEA
) RETURNS BOOLEAN LANGUAGE plpgsql STABLE SET search_path=pg_catalog AS $$
DECLARE envelope JSONB;
BEGIN
    IF p_delivery_bytes IS NULL OR octet_length(p_delivery_bytes) NOT BETWEEN 1 AND 65536 THEN RETURN false; END IF;
    envelope := pg_catalog.convert_from(p_delivery_bytes,'UTF8')::JSONB;
    RETURN COALESCE(envelope->>'event_id'=p_event_id::TEXT
       AND envelope->>'event_type'=p_event_type
       AND envelope->>'aggregate_id'=p_aggregate_id::TEXT
       AND (envelope->>'aggregate_version')::BIGINT=p_aggregate_version
       AND envelope->>'trace_id'=p_trace_id
       AND (envelope->>'occurred_at')::TIMESTAMPTZ=p_occurred_at
       AND envelope->'payload'=p_payload
       AND ((p_serialization_version='LEGACY_TYPED_V1' AND NOT envelope ? 'serialization_version')
            OR (p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
                AND envelope->>'serialization_version'='APPROVAL_RFC8785_DECIMAL_V1'
                AND envelope->'payload'->>'approval_control_epoch'=p_approval_control_epoch::TEXT)),false);
EXCEPTION WHEN OTHERS THEN
    RETURN false;
END $$;

CREATE FUNCTION account_approval_authorization.authorization_sidecar_summary()
RETURNS TABLE(authorization_count BIGINT,missing_epoch_count BIGINT,missing_sidecar_count BIGINT,invalid_sidecar_count BIGINT)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
    SELECT count(*)::BIGINT,
           count(*) FILTER (WHERE o.approval_control_epoch IS NULL OR o.approval_control_epoch<=0)::BIGINT,
           count(*) FILTER (WHERE s.event_id IS NULL)::BIGINT,
           count(*) FILTER (WHERE s.event_id IS NOT NULL AND (
               s.schema_version<>1 OR
               (s.provenance<>'FROZEN_AT_APPROVAL' AND (
                    o.approval_control_epoch<>1 OR s.serialization_version<>'LEGACY_TYPED_V1')) OR
               (s.provenance='FROZEN_AT_APPROVAL' AND
                    ((s.serialization_version='LEGACY_TYPED_V1' AND o.approval_control_epoch<>1)
                     OR (s.serialization_version='APPROVAL_RFC8785_DECIMAL_V1' AND o.approval_control_epoch<=0))) OR
               (s.provenance='RECONSTRUCTED_AFTER_PUBLISH' AND o.published_at IS NULL) OR
               (s.provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY') AND
                (s.delivery_bytes IS NULL OR s.event_digest IS NULL
                 OR s.event_digest<>pg_catalog.encode(pg_catalog.sha256(s.delivery_bytes),'hex')
                 OR NOT account_approval_authorization.authorization_delivery_matches(
                    o.event_id,o.event_type,o.aggregate_id,o.aggregate_version,o.payload,o.trace_id,o.occurred_at,
                    o.approval_control_epoch,s.serialization_version,s.delivery_bytes))) OR
               (s.provenance='RECONSTRUCTED_AFTER_PUBLISH' AND s.reconstructed_semantic_digest IS NULL)
           ))::BIGINT
      FROM public.outbox_events o
      LEFT JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
     WHERE o.event_type='account.approval-command.authorized.v1'
$$;

CREATE FUNCTION account_approval_authorization.authorization_backfill_candidates(
    p_after_occurred_at TIMESTAMPTZ,p_after_event_id UUID,
    p_high_water_occurred_at TIMESTAMPTZ,p_high_water_event_id UUID,p_limit INTEGER
) RETURNS TABLE(
    event_id UUID,event_type TEXT,aggregate_id UUID,aggregate_version BIGINT,
    payload JSONB,trace_id TEXT,occurred_at TIMESTAMPTZ,published_at TIMESTAMPTZ,
    approval_control_epoch BIGINT,sidecar_serialization_version TEXT,sidecar_provenance TEXT,
    sidecar_delivery_bytes BYTEA,sidecar_event_digest TEXT,sidecar_reconstructed_semantic_digest TEXT
) LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE control_row account_approval_control.admin_approval_control%ROWTYPE;
BEGIN
    IF session_user<>'account_approval_backfill' THEN
        RAISE EXCEPTION 'authorization backfill identity is invalid' USING ERRCODE='42501';
    END IF;
    IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 200
       OR p_high_water_occurred_at IS NULL OR p_high_water_event_id IS NULL
       OR ((p_after_occurred_at IS NULL)<>(p_after_event_id IS NULL)) THEN
        RAISE EXCEPTION 'authorization backfill cursor is invalid' USING ERRCODE='22023';
    END IF;
    SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
    IF control_row.mode<>'LEGACY' OR control_row.approval_wire_version<>1 OR control_row.proof_required
       OR control_row.authorization_relay_state<>'RUNNING' OR control_row.approval_epoch<>1 THEN
        RAISE EXCEPTION 'authorization backfill requires LEGACY epoch 1' USING ERRCODE='55000';
    END IF;
    RETURN QUERY SELECT o.event_id,o.event_type,o.aggregate_id,o.aggregate_version,o.payload,o.trace_id,
        o.occurred_at,o.published_at,o.approval_control_epoch,s.serialization_version,s.provenance,
        s.delivery_bytes,s.event_digest,s.reconstructed_semantic_digest
      FROM public.outbox_events o
      LEFT JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
     WHERE o.event_type='account.approval-command.authorized.v1'
       AND (p_after_occurred_at IS NULL OR (o.occurred_at,o.event_id)>(p_after_occurred_at,p_after_event_id))
       AND (o.occurred_at,o.event_id)<=(p_high_water_occurred_at,p_high_water_event_id)
     ORDER BY o.occurred_at,o.event_id LIMIT p_limit;
END $$;

CREATE FUNCTION account_approval_authorization.authorization_backfill_readiness()
RETURNS TABLE(
    authorization_count BIGINT,missing_epoch_count BIGINT,missing_sidecar_count BIGINT,invalid_sidecar_count BIGINT,
    high_water_occurred_at TIMESTAMPTZ,high_water_event_id UUID
)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF session_user<>'account_approval_backfill' THEN
        RAISE EXCEPTION 'authorization backfill identity is invalid' USING ERRCODE='42501';
    END IF;
    RETURN QUERY
    WITH summary AS (SELECT * FROM account_approval_authorization.authorization_sidecar_summary()),
         high_water AS (
             SELECT o.occurred_at,o.event_id FROM public.outbox_events o
              WHERE o.event_type='account.approval-command.authorized.v1'
              ORDER BY o.occurred_at DESC,o.event_id DESC LIMIT 1
         )
    SELECT s.authorization_count,s.missing_epoch_count,s.missing_sidecar_count,s.invalid_sidecar_count,
           h.occurred_at,h.event_id FROM summary s LEFT JOIN high_water h ON true;
END $$;

CREATE FUNCTION account_approval_authorization.validate_authorization_outbox_for_quiescing(p_expected_control_version BIGINT)
RETURNS TABLE(authorization_count BIGINT,missing_epoch_count BIGINT,missing_sidecar_count BIGINT,invalid_sidecar_count BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE control_row account_approval_control.admin_approval_control%ROWTYPE;
BEGIN
    IF session_user<>'account_migrator' THEN
        RAISE EXCEPTION 'authorization final validator identity is invalid' USING ERRCODE='42501';
    END IF;
    IF pg_catalog.current_setting('transaction_isolation')<>'serializable' THEN
        RAISE EXCEPTION 'authorization final validation requires SERIALIZABLE' USING ERRCODE='25001';
    END IF;
    IF p_expected_control_version IS NULL OR p_expected_control_version<1 THEN
        RAISE EXCEPTION 'authorization final validation version is invalid' USING ERRCODE='22023';
    END IF;
    SELECT * INTO STRICT control_row
      FROM account_approval_control.lock_admin_approval_control_exclusive_for_authorization();
    IF control_row.mode<>'LEGACY' OR control_row.authorization_relay_state<>'RUNNING'
       OR control_row.version<>p_expected_control_version THEN
        RAISE EXCEPTION 'authorization final validation control changed' USING ERRCODE='40001';
    END IF;
    RETURN QUERY SELECT * FROM account_approval_authorization.authorization_sidecar_summary();
END $$;

CREATE FUNCTION account_approval_authorization.authorization_validation_candidates(
    p_after_occurred_at TIMESTAMPTZ,p_after_event_id UUID,p_limit INTEGER
) RETURNS TABLE(
    event_id UUID,event_type TEXT,aggregate_id UUID,aggregate_version BIGINT,
    payload JSONB,trace_id TEXT,occurred_at TIMESTAMPTZ,published_at TIMESTAMPTZ,
    approval_control_epoch BIGINT,sidecar_serialization_version TEXT,sidecar_provenance TEXT,
    sidecar_delivery_bytes BYTEA,sidecar_event_digest TEXT,sidecar_reconstructed_semantic_digest TEXT
) LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE control_row account_approval_control.admin_approval_control%ROWTYPE;
BEGIN
    IF session_user<>'account_migrator' THEN
        RAISE EXCEPTION 'authorization final validator identity is invalid' USING ERRCODE='42501';
    END IF;
    IF pg_catalog.current_setting('transaction_isolation')<>'serializable' THEN
        RAISE EXCEPTION 'authorization final validation requires SERIALIZABLE' USING ERRCODE='25001';
    END IF;
    IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 200
       OR ((p_after_occurred_at IS NULL)<>(p_after_event_id IS NULL)) THEN
        RAISE EXCEPTION 'authorization validation cursor is invalid' USING ERRCODE='22023';
    END IF;
    SELECT * INTO STRICT control_row
      FROM account_approval_control.lock_admin_approval_control_exclusive_for_authorization();
    IF control_row.mode<>'LEGACY' OR control_row.authorization_relay_state<>'RUNNING' THEN
        RAISE EXCEPTION 'authorization final validation control changed' USING ERRCODE='40001';
    END IF;
    RETURN QUERY SELECT o.event_id,o.event_type,o.aggregate_id,o.aggregate_version,o.payload,o.trace_id,
        o.occurred_at,o.published_at,o.approval_control_epoch,s.serialization_version,s.provenance,
        s.delivery_bytes,s.event_digest,s.reconstructed_semantic_digest
      FROM public.outbox_events o
      LEFT JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
     WHERE o.event_type='account.approval-command.authorized.v1'
       AND (p_after_occurred_at IS NULL OR (o.occurred_at,o.event_id)>(p_after_occurred_at,p_after_event_id))
     ORDER BY o.occurred_at,o.event_id LIMIT p_limit;
END $$;

CREATE FUNCTION account_approval_authorization.validate_control_transition_sidecars() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE summary RECORD;
BEGIN
    IF OLD.mode='LEGACY' AND NEW.mode='QUIESCING' THEN
        SELECT * INTO summary FROM account_approval_authorization.authorization_sidecar_summary();
        IF summary.missing_epoch_count<>0 OR summary.missing_sidecar_count<>0 OR summary.invalid_sidecar_count<>0 THEN
            RAISE EXCEPTION 'authorization outbox is not ready for QUIESCING' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NULL;
END $$;

REVOKE ALL ON SCHEMA account_approval_authorization FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill;
GRANT USAGE ON SCHEMA account_approval_authorization TO account_approval_writer,account_approval_backfill,account_migrator;
REVOKE ALL ON account_approval_authorization.approval_authorization_event_bytes FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill;
REVOKE ALL ON account_approval_authorization.authorization_event_pending FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA account_approval_authorization FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill;
GRANT EXECUTE ON FUNCTION account_approval_authorization.prepare_authorization_event(UUID,UUID,BIGINT,JSONB,TEXT,BIGINT,TEXT) TO account_approval_writer;
GRANT EXECUTE ON FUNCTION account_approval_authorization.finalize_authorization_event(UUID,TEXT,BYTEA) TO account_approval_writer;
GRANT EXECUTE ON FUNCTION account_approval_authorization.authorization_commit_outcome(UUID,UUID,BIGINT,TEXT,TEXT,BIGINT,UUID,UUID) TO account_approval_writer;
GRANT EXECUTE ON FUNCTION account_approval_authorization.approval_terminal_commit_outcome(UUID,BIGINT,TEXT,UUID,TEXT) TO account_approval_writer;
GRANT EXECUTE ON FUNCTION account_approval_authorization.backfill_authorization_sidecar(UUID,BYTEA,TEXT) TO account_approval_backfill;
GRANT EXECUTE ON FUNCTION account_approval_authorization.authorization_backfill_outcome(UUID,TEXT,TEXT) TO account_approval_backfill;
GRANT EXECUTE ON FUNCTION account_approval_authorization.authorization_backfill_candidates(TIMESTAMPTZ,UUID,TIMESTAMPTZ,UUID,INTEGER) TO account_approval_backfill;
GRANT EXECUTE ON FUNCTION account_approval_authorization.authorization_backfill_readiness() TO account_approval_backfill;
GRANT EXECUTE ON FUNCTION account_approval_authorization.validate_authorization_outbox_for_quiescing(BIGINT) TO account_migrator;
GRANT EXECUTE ON FUNCTION account_approval_authorization.authorization_validation_candidates(TIMESTAMPTZ,UUID,INTEGER) TO account_migrator;
GRANT USAGE ON SCHEMA account_approval_authorization TO account_approval_control_owner;
GRANT EXECUTE ON FUNCTION account_approval_authorization.validate_control_transition_sidecars() TO account_approval_control_owner;
GRANT USAGE ON SCHEMA account_approval_authorization TO account_service;
GRANT EXECUTE ON FUNCTION account_approval_authorization.require_approved_request_authorization_at_commit() TO account_service;

RESET ROLE;
GRANT account_approval_authorization_owner TO account_service WITH INHERIT FALSE, SET TRUE;
SET ROLE account_service;
GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
GRANT SELECT(id,status,version,command_type,policy_version,target_resource_type,target_resource_id),UPDATE(version)
    ON public.approval_requests TO account_approval_authorization_owner;
GRANT SELECT(id,approval_request_id,decision) ON public.approval_decisions TO account_approval_authorization_owner;
GRANT SELECT(id,approval_request_id,to_status,reason_code) ON public.approval_state_events TO account_approval_authorization_owner;
GRANT SELECT ON public.users,public.role_assignments,public.role_permissions,public.authorization_versions
    TO account_approval_authorization_owner;
GRANT UPDATE(id) ON public.users,public.role_assignments TO account_approval_authorization_owner;
GRANT UPDATE(version) ON public.authorization_versions TO account_approval_authorization_owner;

CREATE FUNCTION public.lock_approval_writer_authorization(p_user_id UUID,p_permission TEXT)
RETURNS TABLE(binding_id UUID,authorization_version BIGINT,valid_until TIMESTAMPTZ)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF session_user<>'account_approval_writer' THEN
        RAISE EXCEPTION 'approval writer authorization identity is invalid' USING ERRCODE='42501';
    END IF;
    SELECT ra.id,ra.expires_at INTO binding_id,valid_until
      FROM public.users u
      JOIN public.role_assignments ra ON ra.user_id=u.id AND ra.organization_id IS NULL
      JOIN public.role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=p_permission
     WHERE u.id=p_user_id AND u.status='ACTIVE' AND ra.revoked_at IS NULL
       AND ra.starts_at<=pg_catalog.now() AND (ra.expires_at IS NULL OR ra.expires_at>pg_catalog.now())
     ORDER BY ra.expires_at NULLS LAST,ra.id LIMIT 1 FOR SHARE OF u,ra;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    SELECT av.version INTO authorization_version
      FROM public.authorization_versions av
     WHERE av.user_id=p_user_id AND av.scope='PLATFORM'
       AND av.scope_id='00000000-0000-0000-0000-000000000000'::UUID FOR SHARE;
    IF NOT FOUND THEN
        authorization_version := 1;
    END IF;
    RETURN NEXT;
END $$;
ALTER FUNCTION public.lock_approval_writer_authorization(UUID,TEXT) OWNER TO account_approval_authorization_owner;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;

RESET ROLE;
REVOKE account_approval_authorization_owner FROM account_service;
SET ROLE account_approval_authorization_owner;
REVOKE ALL ON FUNCTION public.lock_approval_writer_authorization(UUID,TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.lock_approval_writer_authorization(UUID,TEXT) TO account_approval_writer;

RESET ROLE;
SET ROLE account_service;
GRANT SELECT ON public.identities,public.users,public.role_assignments,public.role_permissions,
    public.authorization_versions,public.approval_command_policies,public.approval_requests,
    public.approval_request_snapshots TO account_approval_writer;
GRANT UPDATE(status,execution_deadline,decided_at,invalidation_code,invalidation_details,version,updated_at)
    ON public.approval_requests TO account_approval_writer;
GRANT INSERT ON public.approval_decisions,public.approval_state_events TO account_approval_writer;
CREATE CONSTRAINT TRIGGER approval_authorization_request_transition_guard
AFTER UPDATE OF status,version ON public.approval_requests
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
EXECUTE FUNCTION account_approval_authorization.require_approved_request_authorization_at_commit();

RESET ROLE;
SET ROLE account_approval_authorization_owner;
REVOKE EXECUTE ON FUNCTION account_approval_authorization.require_approved_request_authorization_at_commit() FROM account_service;
REVOKE USAGE ON SCHEMA account_approval_authorization FROM account_service;
GRANT SELECT ON public.outbox_events TO account_service;
GRANT INSERT(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) ON public.outbox_events TO account_service;
GRANT UPDATE(attempts,next_attempt_at,locked_by,locked_until,published_at,dead_lettered_at,last_error) ON public.outbox_events TO account_service;

RESET ROLE;
SET ROLE account_approval_control_owner;
GRANT USAGE ON SCHEMA account_approval_control TO account_approval_writer;
GRANT EXECUTE ON FUNCTION account_approval_control.lock_admin_approval_control_shared() TO account_approval_writer;
CREATE CONSTRAINT TRIGGER approval_authorization_control_transition_guard
AFTER UPDATE ON account_approval_control.admin_approval_control
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
EXECUTE FUNCTION account_approval_authorization.validate_control_transition_sidecars();

RESET ROLE;
SET ROLE account_service;
DO $$
DECLARE
    authorization_owner_oid OID := (SELECT oid FROM pg_catalog.pg_roles WHERE rolname='account_approval_authorization_owner');
    migrator_oid OID := (SELECT oid FROM pg_catalog.pg_roles WHERE rolname='account_migrator');
    writer_oid OID := (SELECT oid FROM pg_catalog.pg_roles WHERE rolname='account_approval_writer');
    backfill_oid OID := (SELECT oid FROM pg_catalog.pg_roles WHERE rolname='account_approval_backfill');
BEGIN
    IF pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.outbox_events'::regclass))<>'account_approval_authorization_owner'
       OR pg_catalog.has_database_privilege('account_migrator',current_database(),'CREATE')
       OR pg_catalog.has_schema_privilege('account_approval_authorization_owner','public','CREATE')
       OR pg_catalog.pg_has_role('account_service','account_approval_authorization_owner','MEMBER')
       OR NOT pg_catalog.has_column_privilege('account_service','public.outbox_events','event_id','INSERT')
       OR pg_catalog.has_column_privilege('account_service','public.outbox_events','approval_control_epoch','INSERT')
       OR pg_catalog.has_column_privilege('account_service','public.outbox_events','approval_control_epoch','UPDATE')
       OR pg_catalog.has_table_privilege('account_service','public.outbox_events','DELETE,TRUNCATE,TRIGGER')
       OR EXISTS (
            SELECT 1 FROM pg_catalog.pg_auth_members am
             WHERE (am.roleid IN (authorization_owner_oid,writer_oid,backfill_oid)
                    OR am.member IN (authorization_owner_oid,writer_oid,backfill_oid))
               AND NOT (am.roleid=authorization_owner_oid AND am.member=migrator_oid
                        AND am.admin_option AND am.set_option AND NOT am.inherit_option)
       ) THEN
        RAISE EXCEPTION '000051 final owner or ACL verification failed';
    END IF;
END $$;

-- Expand-only migration. The existing usage_requests shape remains unchanged so
-- older Gateway pods can keep writing throughout a rolling deployment. A short
-- lock timeout prevents trigger installation from queueing writes behind a
-- long-running transaction; a failed migration is safe to retry atomically.
DO $$
BEGIN
    IF session_user<>'token_migrator' AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolsuper) THEN
        RAISE EXCEPTION '000034 requires the dedicated token_migrator identity';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='token_schema_owner' AND NOT rolcanlogin)
       OR pg_catalog.pg_has_role('token_gateway','token_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('token_gateway',(SELECT datdba FROM pg_catalog.pg_database WHERE datname=current_database()),'MEMBER')
       OR (SELECT pg_catalog.pg_get_userbyid(nspowner) FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.to_regnamespace('public')) IS DISTINCT FROM 'token_schema_owner' THEN
        RAISE EXCEPTION '000034 requires an isolated NOLOGIN schema owner';
    END IF;
END $$;
SET LOCAL lock_timeout = '2s';
-- usage_requests predates the isolated schema-owner rollout and remains owned
-- by token_gateway. The NOLOGIN owner needs a durable read grant for the
-- SECURITY DEFINER backfill and a migration-only REFERENCES grant for the two
-- sidecar foreign keys.
SET ROLE token_gateway;
GRANT SELECT,REFERENCES ON public.usage_requests TO token_schema_owner;
SET ROLE token_schema_owner;

CREATE TABLE public.usage_history_projection_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
    last_usage_request_id UUID,
    projected_rows BIGINT NOT NULL DEFAULT 0 CHECK(projected_rows >= 0),
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO public.usage_history_projection_state(singleton) VALUES(TRUE);

CREATE TABLE public.usage_history_members (
    usage_request_id UUID PRIMARY KEY REFERENCES public.usage_requests(id) ON DELETE CASCADE,
    user_id UUID NOT NULL,
    organization_scope_id UUID NOT NULL,
    organization_id UUID,
    usage_group_key TEXT NOT NULL,
    logical_turn_id TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    CHECK (organization_scope_id = COALESCE(organization_id,'00000000-0000-0000-0000-000000000000'::uuid)),
    CHECK (organization_id IS NULL OR organization_id <> '00000000-0000-0000-0000-000000000000'::uuid),
    CHECK ((logical_turn_id IS NULL AND usage_group_key='request:'||usage_request_id::text)
        OR (logical_turn_id IS NOT NULL AND usage_group_key='turn:'||logical_turn_id))
);

-- Both indexes are built on a new, initially empty sidecar. No index build or
-- table rewrite touches the three-year usage_requests history.
CREATE INDEX usage_history_members_group_idx
    ON public.usage_history_members(user_id,organization_scope_id,usage_group_key,started_at DESC,usage_request_id DESC);

CREATE TABLE public.usage_history_groups (
    user_id UUID NOT NULL,
    organization_scope_id UUID NOT NULL,
    organization_id UUID,
    usage_group_key TEXT NOT NULL,
    logical_turn_id TEXT,
    head_started_at TIMESTAMPTZ NOT NULL,
    head_request_id UUID NOT NULL REFERENCES public.usage_requests(id) ON DELETE RESTRICT,
    PRIMARY KEY(user_id,organization_scope_id,usage_group_key),
    CHECK (organization_scope_id = COALESCE(organization_id,'00000000-0000-0000-0000-000000000000'::uuid)),
    CHECK (organization_id IS NULL OR organization_id <> '00000000-0000-0000-0000-000000000000'::uuid),
    CHECK ((logical_turn_id IS NULL AND usage_group_key='request:'||head_request_id::text)
        OR (logical_turn_id IS NOT NULL AND usage_group_key='turn:'||logical_turn_id))
);

CREATE INDEX usage_history_groups_page_idx
    ON public.usage_history_groups(user_id,organization_scope_id,head_started_at DESC,head_request_id DESC)
    INCLUDE(usage_group_key,logical_turn_id,organization_id);

CREATE OR REPLACE FUNCTION public.project_usage_history_request(
    projected_id UUID,
    projected_user_id UUID,
    projected_organization_id UUID,
    projected_request_snapshot JSONB,
    projected_started_at TIMESTAMPTZ
) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    projected_logical_turn_id TEXT := NULLIF(projected_request_snapshot->>'logical_turn_id','');
    projected_group_key TEXT;
    projected_scope_id UUID := COALESCE(projected_organization_id,'00000000-0000-0000-0000-000000000000'::uuid);
BEGIN
    projected_group_key := CASE
        WHEN projected_logical_turn_id IS NULL THEN 'request:'||projected_id::text
        ELSE 'turn:'||projected_logical_turn_id
    END;

    INSERT INTO public.usage_history_members(
        usage_request_id,user_id,organization_scope_id,organization_id,
        usage_group_key,logical_turn_id,started_at
    ) VALUES(
        projected_id,projected_user_id,projected_scope_id,projected_organization_id,
        projected_group_key,projected_logical_turn_id,projected_started_at
    ) ON CONFLICT(usage_request_id) DO NOTHING;

    INSERT INTO public.usage_history_groups AS existing(
        user_id,organization_scope_id,organization_id,usage_group_key,
        logical_turn_id,head_started_at,head_request_id
    ) VALUES(
        projected_user_id,projected_scope_id,projected_organization_id,projected_group_key,
        projected_logical_turn_id,projected_started_at,projected_id
    )
    ON CONFLICT(user_id,organization_scope_id,usage_group_key) DO UPDATE
    SET head_started_at=EXCLUDED.head_started_at,
        head_request_id=EXCLUDED.head_request_id
    WHERE (EXCLUDED.head_started_at,EXCLUDED.head_request_id) >
          (existing.head_started_at,existing.head_request_id);
END;
$$;

CREATE OR REPLACE FUNCTION public.maintain_usage_history_projection() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    PERFORM public.project_usage_history_request(
        NEW.id,NEW.user_id,NEW.organization_id,NEW.request_snapshot,NEW.started_at
    );
    RETURN NULL;
END;
$$;

REVOKE ALL ON FUNCTION public.project_usage_history_request(UUID,UUID,UUID,JSONB,TIMESTAMPTZ),public.maintain_usage_history_projection() FROM PUBLIC,token_gateway;
-- Only the table owner may install the compatibility trigger. EXECUTE is
-- granted just long enough to create it; trigger execution subsequently uses
-- the isolated function owner and does not require runtime EXECUTE.
GRANT EXECUTE ON FUNCTION public.maintain_usage_history_projection() TO token_gateway;
SET ROLE token_gateway;
CREATE TRIGGER usage_history_projection_after_insert
AFTER INSERT ON public.usage_requests
FOR EACH ROW EXECUTE FUNCTION public.maintain_usage_history_projection();
REVOKE REFERENCES ON public.usage_requests FROM token_schema_owner;
SET ROLE token_schema_owner;
REVOKE EXECUTE ON FUNCTION public.maintain_usage_history_projection() FROM token_gateway;

-- Each call owns only one bounded batch and therefore one short transaction.
-- Multiple rolling pods serialize on the singleton row with SKIP LOCKED.
CREATE OR REPLACE FUNCTION public.backfill_usage_history_projection(batch_size INTEGER)
RETURNS TABLE(processed INTEGER, complete BOOLEAN)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    cursor_id UUID;
    item RECORD;
BEGIN
    IF batch_size < 1 OR batch_size > 5000 THEN
        RAISE EXCEPTION 'usage history projection batch size must be between 1 and 5000';
    END IF;

    SELECT state.last_usage_request_id
    INTO cursor_id
    FROM public.usage_history_projection_state state
    WHERE state.singleton
    FOR UPDATE SKIP LOCKED;

    IF NOT FOUND THEN
        RETURN QUERY SELECT 0, FALSE;
        RETURN;
    END IF;

    processed := 0;
    FOR item IN
        SELECT request.id,request.user_id,request.organization_id,request.request_snapshot,request.started_at
        FROM public.usage_requests request
        WHERE request.id > COALESCE(cursor_id,'00000000-0000-0000-0000-000000000000'::uuid)
        ORDER BY request.id
        LIMIT batch_size
    LOOP
        PERFORM public.project_usage_history_request(
            item.id,item.user_id,item.organization_id,item.request_snapshot,item.started_at
        );
        cursor_id := item.id;
        processed := processed + 1;
    END LOOP;

    complete := processed < batch_size;
    UPDATE public.usage_history_projection_state
    SET last_usage_request_id=cursor_id,
        projected_rows=projected_rows+processed,
        completed_at=CASE WHEN complete THEN COALESCE(completed_at,now()) ELSE NULL END,
        updated_at=now()
    WHERE singleton;

    RETURN NEXT;
END;
$$;

-- Runtime can only read the projection and invoke the bounded worker. All
-- sidecar writes stay behind owner-controlled SECURITY DEFINER functions so a
-- compromised Gateway cannot forge usage history or widen its DML authority.
REVOKE ALL ON public.usage_history_projection_state,public.usage_history_members,public.usage_history_groups FROM PUBLIC,token_gateway;
GRANT SELECT ON public.usage_history_projection_state,public.usage_history_members,public.usage_history_groups TO token_gateway;
REVOKE ALL ON FUNCTION public.project_usage_history_request(UUID,UUID,UUID,JSONB,TIMESTAMPTZ),public.maintain_usage_history_projection(),public.backfill_usage_history_projection(INTEGER) FROM PUBLIC,token_gateway;
GRANT EXECUTE ON FUNCTION public.backfill_usage_history_projection(INTEGER) TO token_gateway;
SET ROLE token_gateway;

package tokenschema

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
)

// Exact frozen 034 bodies are approved; names alone never authorize definer code.
// Invoker bridging exists only inside the historical hardening transaction.
var historyDefiners = []struct{ signature, body string }{
	{`public.project_usage_history_request(uuid,uuid,uuid,jsonb,timestamp with time zone)`, `
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
`},
	{`public.maintain_usage_history_projection()`, `
BEGIN
    PERFORM public.project_usage_history_request(
        NEW.id,NEW.user_id,NEW.organization_id,NEW.request_snapshot,NEW.started_at
    );
    RETURN NULL;
END;
`},
	{`public.backfill_usage_history_projection(integer)`, `
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
`},
}

func bridgeHistoryDefiners(ctx context.Context, tx pgx.Tx, restore bool) error {
	var presentCount int
	for _, expected := range historyDefiners {
		var present bool
		if err := tx.QueryRow(ctx, `SELECT to_regprocedure($1) IS NOT NULL`, expected.signature).Scan(&present); err != nil {
			return err
		}
		if present {
			presentCount++
		}
	}
	if presentCount != 0 && presentCount != len(historyDefiners) {
		return fmt.Errorf("incomplete Token history definer identity")
	}
	for _, expected := range historyDefiners {
		var present bool
		if err := tx.QueryRow(ctx, `SELECT to_regprocedure($1) IS NOT NULL`, expected.signature).Scan(&present); err != nil {
			return err
		}
		if !present {
			continue
		}
		var body, owner, language string
		var config []string
		if err := tx.QueryRow(ctx, `SELECT p.prosrc,pg_get_userbyid(p.proowner),l.lanname,p.proconfig FROM pg_proc p JOIN pg_language l ON l.oid=p.prolang WHERE p.oid=to_regprocedure($1)`, expected.signature).Scan(&body, &owner, &language, &config); err != nil {
			return err
		}
		if body != expected.body || owner != "token_schema_owner" || language != "plpgsql" || len(config) != 1 || config[0] != "search_path=pg_catalog" {
			return fmt.Errorf("unreviewed Token history definer identity")
		}
		mode := "INVOKER"
		if restore {
			mode = "DEFINER"
		}
		if _, err := tx.Exec(ctx, `SET ROLE token_schema_owner; ALTER FUNCTION `+expected.signature+` SECURITY `+mode+`; REVOKE ALL ON FUNCTION `+expected.signature+` FROM PUBLIC,token_gateway; SET ROLE token_gateway;`); err != nil {
			return err
		}
	}
	if restore {
		var present bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('public.usage_history_groups') IS NOT NULL`).Scan(&present); err != nil {
			return err
		}
		if present {
			_, err := tx.Exec(ctx, `SET ROLE token_schema_owner; REVOKE ALL ON public.usage_history_projection_state,public.usage_history_members,public.usage_history_groups FROM PUBLIC,token_gateway; GRANT SELECT ON public.usage_history_projection_state,public.usage_history_members,public.usage_history_groups TO token_gateway; GRANT EXECUTE ON FUNCTION public.backfill_usage_history_projection(integer) TO token_gateway; REVOKE UPDATE,DELETE ON public.client_readiness_attestations FROM token_gateway; SET ROLE token_gateway;`)
			return err
		}
	}
	return nil
}

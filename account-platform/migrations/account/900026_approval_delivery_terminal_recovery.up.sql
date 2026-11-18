-- Repair delivery metadata only. RESULT_CONFIRMED is the frozen authority;
-- this function neither proves broker ACK nor executes any domain command.
DO $$ BEGIN
  IF session_user <> 'account_migrator' OR current_user <> 'account_service' THEN
    RAISE EXCEPTION 'terminal recovery requires isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=58) THEN
    RAISE EXCEPTION 'terminal recovery requires consumer delivery binding' USING ERRCODE='55000';
  END IF;
END $$;

GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
RESET ROLE;
SET ROLE account_approval_authorization_owner;

CREATE INDEX approval_delivery_result_terminal_recovery_idx
  ON account_approval_authorization.approval_delivery_states(original_event_id,durable)
  WHERE state='RESULT_CONFIRMED';

CREATE FUNCTION public.recover_approval_authorization_terminals(p_limit INTEGER)
RETURNS INTEGER LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE recovered INTEGER;
BEGIN
  IF session_user <> 'account_service' THEN
    RAISE EXCEPTION 'terminal recovery forbidden' USING ERRCODE='42501';
  END IF;
  IF p_limit IS NULL OR p_limit < 1 OR p_limit > 100 THEN
    RAISE EXCEPTION 'terminal recovery limit invalid' USING ERRCODE='22023';
  END IF;
  WITH candidates AS MATERIALIZED (
    SELECT d.original_event_id,d.durable,d.version
      FROM account_approval_authorization.approval_delivery_states d
     WHERE d.state='RESULT_CONFIRMED'
       AND account_approval_authorization.authorization_consumer_mapping_allows(
         d.durable,d.executor_identity,d.command_type,d.policy_version,d.executor_identity,d.serialization_version)
     ORDER BY d.original_event_id,d.durable
     LIMIT p_limit FOR UPDATE SKIP LOCKED
  ), repaired AS (
    UPDATE account_approval_authorization.approval_delivery_states d
       SET state='TERMINAL_CONFIRMED',version=d.version+1,
           updated_at=GREATEST(d.updated_at,clock_timestamp())
      FROM candidates c
     WHERE d.original_event_id=c.original_event_id AND d.durable=c.durable
       AND d.version=c.version AND d.state='RESULT_CONFIRMED'
    RETURNING d.original_event_id
  ) SELECT count(*)::INTEGER INTO recovered FROM repaired;
  RETURN recovered;
END $$;
REVOKE ALL ON FUNCTION public.recover_approval_authorization_terminals(INTEGER) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.recover_approval_authorization_terminals(INTEGER) TO account_service;
RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;

DO $$
DECLARE function_oid OID := to_regprocedure('public.recover_approval_authorization_terminals(integer)');
BEGIN
  IF function_oid IS NULL OR (SELECT pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner'
      OR NOT p.prosecdef OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
      OR NOT pg_catalog.has_function_privilege('account_service',p.oid,'EXECUTE')
      OR pg_catalog.has_function_privilege('public',p.oid,'EXECUTE')
      FROM pg_catalog.pg_proc p WHERE p.oid=function_oid)
    OR pg_catalog.has_schema_privilege('account_approval_authorization_owner','public','CREATE')
    OR EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
       WHERE n.nspname='account_approval_authorization' AND c.relname='approval_delivery_states'
         AND (pg_catalog.has_table_privilege('account_service',c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER')
           OR pg_catalog.has_any_column_privilege('account_service',c.oid,'SELECT,INSERT,UPDATE,REFERENCES')))
    OR EXISTS(SELECT 1 FROM pg_catalog.pg_roles r
       WHERE r.rolname IN ('account_approval_writer','account_approval_backfill','account_approval_relay','account_approval_recovery')
         AND pg_catalog.has_function_privilege(r.oid,function_oid,'EXECUTE')) THEN
    RAISE EXCEPTION 'terminal recovery final authority invalid' USING ERRCODE='55000';
  END IF;
END $$;
